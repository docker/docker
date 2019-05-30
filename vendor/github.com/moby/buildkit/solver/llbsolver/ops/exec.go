package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/locker"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets"
	"github.com/moby/buildkit/session/sshforward"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/progress/logs"
	utilsystem "github.com/moby/buildkit/util/system"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runc/libcontainer/system"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const execCacheType = "buildkit.exec.v0"

type execOp struct {
	op        *pb.ExecOp
	cm        cache.Manager
	sm        *session.Manager
	md        *metadata.Store
	exec      executor.Executor
	w         worker.Worker
	platform  *pb.Platform
	numInputs int

	cacheMounts map[string]*cacheRefShare
}

func NewExecOp(v solver.Vertex, op *pb.Op_Exec, platform *pb.Platform, cm cache.Manager, sm *session.Manager, md *metadata.Store, exec executor.Executor, w worker.Worker) (solver.Op, error) {
	return &execOp{
		op:          op.Exec,
		cm:          cm,
		sm:          sm,
		md:          md,
		exec:        exec,
		numInputs:   len(v.Inputs()),
		w:           w,
		platform:    platform,
		cacheMounts: map[string]*cacheRefShare{},
	}, nil
}

func cloneExecOp(old *pb.ExecOp) pb.ExecOp {
	n := *old
	meta := *n.Meta
	meta.ExtraHosts = nil
	for i := range n.Meta.ExtraHosts {
		h := *n.Meta.ExtraHosts[i]
		meta.ExtraHosts = append(meta.ExtraHosts, &h)
	}
	n.Meta = &meta
	n.Mounts = nil
	for i := range n.Mounts {
		m := *n.Mounts[i]
		n.Mounts = append(n.Mounts, &m)
	}
	return n
}

func (e *execOp) CacheMap(ctx context.Context, index int) (*solver.CacheMap, bool, error) {
	op := cloneExecOp(e.op)
	for i := range op.Meta.ExtraHosts {
		h := op.Meta.ExtraHosts[i]
		h.IP = ""
		op.Meta.ExtraHosts[i] = h
	}
	for i := range op.Mounts {
		op.Mounts[i].Selector = ""
	}
	op.Meta.ProxyEnv = nil

	p := platforms.DefaultSpec()
	if e.platform != nil {
		p = specs.Platform{
			OS:           e.platform.OS,
			Architecture: e.platform.Architecture,
			Variant:      e.platform.Variant,
		}
	}

	dt, err := json.Marshal(struct {
		Type    string
		Exec    *pb.ExecOp
		OS      string
		Arch    string
		Variant string `json:",omitempty"`
	}{
		Type:    execCacheType,
		Exec:    &op,
		OS:      p.OS,
		Arch:    p.Architecture,
		Variant: p.Variant,
	})
	if err != nil {
		return nil, false, err
	}

	cm := &solver.CacheMap{
		Digest: digest.FromBytes(dt),
		Deps: make([]struct {
			Selector          digest.Digest
			ComputeDigestFunc solver.ResultBasedCacheFunc
		}, e.numInputs),
	}

	deps, err := e.getMountDeps()
	if err != nil {
		return nil, false, err
	}

	for i, dep := range deps {
		if len(dep.Selectors) != 0 {
			dgsts := make([][]byte, 0, len(dep.Selectors))
			for _, p := range dep.Selectors {
				dgsts = append(dgsts, []byte(p))
			}
			cm.Deps[i].Selector = digest.FromBytes(bytes.Join(dgsts, []byte{0}))
		}
		if !dep.NoContentBasedHash {
			cm.Deps[i].ComputeDigestFunc = llbsolver.NewContentHashFunc(toSelectors(dedupePaths(dep.Selectors)))
		}
	}

	return cm, true, nil
}

func dedupePaths(inp []string) []string {
	old := make(map[string]struct{}, len(inp))
	for _, p := range inp {
		old[p] = struct{}{}
	}
	paths := make([]string, 0, len(old))
	for p1 := range old {
		var skip bool
		for p2 := range old {
			if p1 != p2 && strings.HasPrefix(p1, p2+"/") {
				skip = true
				break
			}
		}
		if !skip {
			paths = append(paths, p1)
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		return paths[i] < paths[j]
	})
	return paths
}

func toSelectors(p []string) []llbsolver.Selector {
	sel := make([]llbsolver.Selector, 0, len(p))
	for _, p := range p {
		sel = append(sel, llbsolver.Selector{Path: p, FollowLinks: true})
	}
	return sel
}

type dep struct {
	Selectors          []string
	NoContentBasedHash bool
}

func (e *execOp) getMountDeps() ([]dep, error) {
	deps := make([]dep, e.numInputs)
	for _, m := range e.op.Mounts {
		if m.Input == pb.Empty {
			continue
		}
		if int(m.Input) >= len(deps) {
			return nil, errors.Errorf("invalid mountinput %v", m)
		}

		sel := m.Selector
		if sel != "" {
			sel = path.Join("/", sel)
			deps[m.Input].Selectors = append(deps[m.Input].Selectors, sel)
		}

		if (!m.Readonly || m.Dest == pb.RootMount) && m.Output != -1 { // exclude read-only rootfs && read-write mounts
			deps[m.Input].NoContentBasedHash = true
		}
	}
	return deps, nil
}

func (e *execOp) getRefCacheDir(ctx context.Context, ref cache.ImmutableRef, id string, m *pb.Mount, sharing pb.CacheSharingOpt) (mref cache.MutableRef, err error) {

	key := "cache-dir:" + id
	if ref != nil {
		key += ":" + ref.ID()
	}

	if ref, ok := e.cacheMounts[key]; ok {
		return ref.clone(), nil
	}
	defer func() {
		if err == nil {
			share := &cacheRefShare{MutableRef: mref, refs: map[*cacheRef]struct{}{}}
			e.cacheMounts[key] = share
			mref = share.clone()
		}
	}()

	switch sharing {
	case pb.CacheSharingOpt_SHARED:
		return sharedCacheRefs.get(key, func() (cache.MutableRef, error) {
			return e.getRefCacheDirNoCache(ctx, key, ref, id, m, false)
		})
	case pb.CacheSharingOpt_PRIVATE:
		return e.getRefCacheDirNoCache(ctx, key, ref, id, m, false)
	case pb.CacheSharingOpt_LOCKED:
		return e.getRefCacheDirNoCache(ctx, key, ref, id, m, true)
	default:
		return nil, errors.Errorf("invalid cache sharing option: %s", sharing.String())
	}

}

func (e *execOp) getRefCacheDirNoCache(ctx context.Context, key string, ref cache.ImmutableRef, id string, m *pb.Mount, block bool) (cache.MutableRef, error) {
	makeMutable := func(cache.ImmutableRef) (cache.MutableRef, error) {
		desc := fmt.Sprintf("cached mount %s from exec %s", m.Dest, strings.Join(e.op.Meta.Args, " "))
		return e.cm.New(ctx, ref, cache.WithRecordType(client.UsageRecordTypeCacheMount), cache.WithDescription(desc), cache.CachePolicyRetain)
	}

	cacheRefsLocker.Lock(key)
	defer cacheRefsLocker.Unlock(key)
	for {
		sis, err := e.md.Search(key)
		if err != nil {
			return nil, err
		}
		locked := false
		for _, si := range sis {
			if mRef, err := e.cm.GetMutable(ctx, si.ID()); err == nil {
				logrus.Debugf("reusing ref for cache dir: %s", mRef.ID())
				return mRef, nil
			} else if errors.Cause(err) == cache.ErrLocked {
				locked = true
			}
		}
		if block && locked {
			cacheRefsLocker.Unlock(key)
			select {
			case <-ctx.Done():
				cacheRefsLocker.Lock(key)
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
				cacheRefsLocker.Lock(key)
			}
		} else {
			break
		}
	}
	mRef, err := makeMutable(ref)
	if err != nil {
		return nil, err
	}

	si, _ := e.md.Get(mRef.ID())
	v, err := metadata.NewValue(key)
	if err != nil {
		mRef.Release(context.TODO())
		return nil, err
	}
	v.Index = key
	if err := si.Update(func(b *bolt.Bucket) error {
		return si.SetValue(b, key, v)
	}); err != nil {
		mRef.Release(context.TODO())
		return nil, err
	}
	return mRef, nil
}

func (e *execOp) getSSHMountable(ctx context.Context, m *pb.Mount) (cache.Mountable, error) {
	sessionID := session.FromContext(ctx)
	if sessionID == "" {
		return nil, errors.New("could not access local files without session")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	caller, err := e.sm.Get(timeoutCtx, sessionID)
	if err != nil {
		return nil, err
	}

	if err := sshforward.CheckSSHID(ctx, caller, m.SSHOpt.ID); err != nil {
		if m.SSHOpt.Optional {
			return nil, nil
		}
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
			return nil, errors.Errorf("no SSH key %q forwarded from the client", m.SSHOpt.ID)
		}
		return nil, err
	}

	return &sshMount{mount: m, caller: caller, idmap: e.cm.IdentityMapping()}, nil
}

type sshMount struct {
	mount  *pb.Mount
	caller session.Caller
	idmap  *idtools.IdentityMapping
}

func (sm *sshMount) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	return &sshMountInstance{sm: sm, idmap: sm.idmap}, nil
}

type sshMountInstance struct {
	sm      *sshMount
	cleanup func() error
	idmap   *idtools.IdentityMapping
}

func (sm *sshMountInstance) Mount() ([]mount.Mount, error) {
	ctx, cancel := context.WithCancel(context.TODO())

	uid := int(sm.sm.mount.SSHOpt.Uid)
	gid := int(sm.sm.mount.SSHOpt.Gid)

	if sm.idmap != nil {
		identity, err := sm.idmap.ToHost(idtools.Identity{
			UID: uid,
			GID: gid,
		})
		if err != nil {
			return nil, err
		}
		uid = identity.UID
		gid = identity.GID
	}

	sock, cleanup, err := sshforward.MountSSHSocket(ctx, sm.sm.caller, sshforward.SocketOpt{
		ID:   sm.sm.mount.SSHOpt.ID,
		UID:  uid,
		GID:  gid,
		Mode: int(sm.sm.mount.SSHOpt.Mode & 0777),
	})
	if err != nil {
		cancel()
		return nil, err
	}
	sm.cleanup = func() error {
		var err error
		if cleanup != nil {
			err = cleanup()
		}
		cancel()
		return err
	}

	return []mount.Mount{{
		Type:    "bind",
		Source:  sock,
		Options: []string{"rbind"},
	}}, nil
}

func (sm *sshMountInstance) Release() error {
	if sm.cleanup != nil {
		if err := sm.cleanup(); err != nil {
			return err
		}
	}
	return nil
}

func (sm *sshMountInstance) IdentityMapping() *idtools.IdentityMapping {
	return sm.idmap
}

func (e *execOp) getSecretMountable(ctx context.Context, m *pb.Mount) (cache.Mountable, error) {
	if m.SecretOpt == nil {
		return nil, errors.Errorf("invalid sercet mount options")
	}
	sopt := *m.SecretOpt

	id := sopt.ID
	if id == "" {
		return nil, errors.Errorf("secret ID missing from mount options")
	}

	sessionID := session.FromContext(ctx)
	if sessionID == "" {
		return nil, errors.New("could not access local files without session")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	caller, err := e.sm.Get(timeoutCtx, sessionID)
	if err != nil {
		return nil, err
	}

	dt, err := secrets.GetSecret(ctx, caller, id)
	if err != nil {
		if errors.Cause(err) == secrets.ErrNotFound && m.SecretOpt.Optional {
			return nil, nil
		}
		return nil, err
	}

	return &secretMount{mount: m, data: dt, idmap: e.cm.IdentityMapping()}, nil
}

type secretMount struct {
	mount *pb.Mount
	data  []byte
	idmap *idtools.IdentityMapping
}

func (sm *secretMount) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	return &secretMountInstance{sm: sm, idmap: sm.idmap}, nil
}

type secretMountInstance struct {
	sm    *secretMount
	root  string
	idmap *idtools.IdentityMapping
}

func (sm *secretMountInstance) Mount() ([]mount.Mount, error) {
	dir, err := ioutil.TempDir("", "buildkit-secrets")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp dir")
	}

	if err := os.Chmod(dir, 0711); err != nil {
		return nil, err
	}

	tmpMount := mount.Mount{
		Type:    "tmpfs",
		Source:  "tmpfs",
		Options: []string{"nodev", "nosuid", "noexec", fmt.Sprintf("uid=%d,gid=%d", os.Geteuid(), os.Getegid())},
	}

	if system.RunningInUserNS() {
		tmpMount.Options = nil
	}

	if err := mount.All([]mount.Mount{tmpMount}, dir); err != nil {
		return nil, errors.Wrap(err, "unable to setup secret mount")
	}
	sm.root = dir

	randID := identity.NewID()
	fp := filepath.Join(dir, randID)
	if err := ioutil.WriteFile(fp, sm.sm.data, 0600); err != nil {
		sm.Release()
		return nil, err
	}

	uid := int(sm.sm.mount.SecretOpt.Uid)
	gid := int(sm.sm.mount.SecretOpt.Gid)

	if sm.idmap != nil {
		identity, err := sm.idmap.ToHost(idtools.Identity{
			UID: uid,
			GID: gid,
		})
		if err != nil {
			return nil, err
		}
		uid = identity.UID
		gid = identity.GID
	}

	if err := os.Chown(fp, uid, gid); err != nil {
		return nil, err
	}

	if err := os.Chmod(fp, os.FileMode(sm.sm.mount.SecretOpt.Mode&0777)); err != nil {
		return nil, err
	}

	return []mount.Mount{{
		Type:    "bind",
		Source:  fp,
		Options: []string{"ro", "rbind"},
	}}, nil
}

func (sm *secretMountInstance) Release() error {
	if sm.root != "" {
		if err := mount.Unmount(sm.root, 0); err != nil {
			return err
		}
		return os.RemoveAll(sm.root)
	}
	return nil
}

func (sm *secretMountInstance) IdentityMapping() *idtools.IdentityMapping {
	return sm.idmap
}

func addDefaultEnvvar(env []string, k, v string) []string {
	for _, e := range env {
		if strings.HasPrefix(e, k+"=") {
			return env
		}
	}
	return append(env, k+"="+v)
}

func (e *execOp) Exec(ctx context.Context, inputs []solver.Result) ([]solver.Result, error) {
	var mounts []executor.Mount
	var root cache.Mountable
	var readonlyRootFS bool

	var outputs []cache.Ref

	defer func() {
		for _, o := range outputs {
			if o != nil {
				go o.Release(context.TODO())
			}
		}
	}()

	// loop over all mounts, fill in mounts, root and outputs
	for _, m := range e.op.Mounts {
		var mountable cache.Mountable
		var ref cache.ImmutableRef

		if m.Dest == pb.RootMount && m.MountType != pb.MountType_BIND {
			return nil, errors.Errorf("invalid mount type %s for %s", m.MountType.String(), m.Dest)
		}

		// if mount is based on input validate and load it
		if m.Input != pb.Empty {
			if int(m.Input) > len(inputs) {
				return nil, errors.Errorf("missing input %d", m.Input)
			}
			inp := inputs[int(m.Input)]
			workerRef, ok := inp.Sys().(*worker.WorkerRef)
			if !ok {
				return nil, errors.Errorf("invalid reference for exec %T", inp.Sys())
			}
			ref = workerRef.ImmutableRef
			mountable = ref
		}

		makeMutable := func(cache.ImmutableRef) (cache.MutableRef, error) {
			desc := fmt.Sprintf("mount %s from exec %s", m.Dest, strings.Join(e.op.Meta.Args, " "))
			return e.cm.New(ctx, ref, cache.WithDescription(desc))
		}

		switch m.MountType {
		case pb.MountType_BIND:
			// if mount creates an output
			if m.Output != pb.SkipOutput {
				// it it is readonly and not root then output is the input
				if m.Readonly && ref != nil && m.Dest != pb.RootMount {
					outputs = append(outputs, ref.Clone())
				} else {
					// otherwise output and mount is the mutable child
					active, err := makeMutable(ref)
					if err != nil {
						return nil, err
					}
					outputs = append(outputs, active)
					mountable = active
				}
			} else if ref == nil {
				// this case is empty readonly scratch without output that is not really useful for anything but don't error
				active, err := makeMutable(ref)
				if err != nil {
					return nil, err
				}
				defer active.Release(context.TODO())
				mountable = active
			}

		case pb.MountType_CACHE:
			if m.CacheOpt == nil {
				return nil, errors.Errorf("missing cache mount options")
			}
			mRef, err := e.getRefCacheDir(ctx, ref, m.CacheOpt.ID, m, m.CacheOpt.Sharing)
			if err != nil {
				return nil, err
			}
			mountable = mRef
			defer func() {
				go mRef.Release(context.TODO())
			}()
			if m.Output != pb.SkipOutput && ref != nil {
				outputs = append(outputs, ref.Clone())
			}

		case pb.MountType_TMPFS:
			mountable = newTmpfs(e.cm.IdentityMapping())

		case pb.MountType_SECRET:
			secretMount, err := e.getSecretMountable(ctx, m)
			if err != nil {
				return nil, err
			}
			if secretMount == nil {
				continue
			}
			mountable = secretMount

		case pb.MountType_SSH:
			sshMount, err := e.getSSHMountable(ctx, m)
			if err != nil {
				return nil, err
			}
			if sshMount == nil {
				continue
			}
			mountable = sshMount

		default:
			return nil, errors.Errorf("mount type %s not implemented", m.MountType)
		}

		// validate that there is a mount
		if mountable == nil {
			return nil, errors.Errorf("mount %s has no input", m.Dest)
		}

		// if dest is root we need mutable ref even if there is no output
		if m.Dest == pb.RootMount {
			root = mountable
			readonlyRootFS = m.Readonly
			if m.Output == pb.SkipOutput && readonlyRootFS {
				active, err := makeMutable(ref)
				if err != nil {
					return nil, err
				}
				defer func() {
					go active.Release(context.TODO())
				}()
				root = active
			}
		} else {
			mounts = append(mounts, executor.Mount{Src: mountable, Dest: m.Dest, Readonly: m.Readonly, Selector: m.Selector})
		}
	}

	// sort mounts so parents are mounted first
	sort.Slice(mounts, func(i, j int) bool {
		return mounts[i].Dest < mounts[j].Dest
	})

	extraHosts, err := parseExtraHosts(e.op.Meta.ExtraHosts)
	if err != nil {
		return nil, err
	}

	meta := executor.Meta{
		Args:           e.op.Meta.Args,
		Env:            e.op.Meta.Env,
		Cwd:            e.op.Meta.Cwd,
		User:           e.op.Meta.User,
		ReadonlyRootFS: readonlyRootFS,
		ExtraHosts:     extraHosts,
		NetMode:        e.op.Network,
		SecurityMode:   e.op.Security,
	}

	if e.op.Meta.ProxyEnv != nil {
		meta.Env = append(meta.Env, proxyEnvList(e.op.Meta.ProxyEnv)...)
	}
	meta.Env = addDefaultEnvvar(meta.Env, "PATH", utilsystem.DefaultPathEnv)

	stdout, stderr := logs.NewLogStreams(ctx, os.Getenv("BUILDKIT_DEBUG_EXEC_OUTPUT") == "1")
	defer stdout.Close()
	defer stderr.Close()

	if err := e.exec.Exec(ctx, meta, root, mounts, nil, stdout, stderr); err != nil {
		return nil, errors.Wrapf(err, "executor failed running %v", meta.Args)
	}

	refs := []solver.Result{}
	for i, out := range outputs {
		if mutable, ok := out.(cache.MutableRef); ok {
			ref, err := mutable.Commit(ctx)
			if err != nil {
				return nil, errors.Wrapf(err, "error committing %s", mutable.ID())
			}
			refs = append(refs, worker.NewWorkerRefResult(ref, e.w))
		} else {
			refs = append(refs, worker.NewWorkerRefResult(out.(cache.ImmutableRef), e.w))
		}
		outputs[i] = nil
	}
	return refs, nil
}

func proxyEnvList(p *pb.ProxyEnv) []string {
	out := []string{}
	if v := p.HttpProxy; v != "" {
		out = append(out, "HTTP_PROXY="+v, "http_proxy="+v)
	}
	if v := p.HttpsProxy; v != "" {
		out = append(out, "HTTPS_PROXY="+v, "https_proxy="+v)
	}
	if v := p.FtpProxy; v != "" {
		out = append(out, "FTP_PROXY="+v, "ftp_proxy="+v)
	}
	if v := p.NoProxy; v != "" {
		out = append(out, "NO_PROXY="+v, "no_proxy="+v)
	}
	return out
}

func newTmpfs(idmap *idtools.IdentityMapping) cache.Mountable {
	return &tmpfs{idmap: idmap}
}

type tmpfs struct {
	idmap *idtools.IdentityMapping
}

func (f *tmpfs) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	return &tmpfsMount{readonly: readonly, idmap: f.idmap}, nil
}

type tmpfsMount struct {
	readonly bool
	idmap    *idtools.IdentityMapping
}

func (m *tmpfsMount) Mount() ([]mount.Mount, error) {
	opt := []string{"nosuid"}
	if m.readonly {
		opt = append(opt, "ro")
	}
	return []mount.Mount{{
		Type:    "tmpfs",
		Source:  "tmpfs",
		Options: opt,
	}}, nil
}
func (m *tmpfsMount) Release() error {
	return nil
}

func (m *tmpfsMount) IdentityMapping() *idtools.IdentityMapping {
	return m.idmap
}

var cacheRefsLocker = locker.New()
var sharedCacheRefs = &cacheRefs{}

type cacheRefs struct {
	mu     sync.Mutex
	shares map[string]*cacheRefShare
}

func (r *cacheRefs) get(key string, fn func() (cache.MutableRef, error)) (cache.MutableRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.shares == nil {
		r.shares = map[string]*cacheRefShare{}
	}

	share, ok := r.shares[key]
	if ok {
		return share.clone(), nil
	}

	mref, err := fn()
	if err != nil {
		return nil, err
	}

	share = &cacheRefShare{MutableRef: mref, main: r, key: key, refs: map[*cacheRef]struct{}{}}
	r.shares[key] = share

	return share.clone(), nil
}

type cacheRefShare struct {
	cache.MutableRef
	mu   sync.Mutex
	refs map[*cacheRef]struct{}
	main *cacheRefs
	key  string
}

func (r *cacheRefShare) clone() cache.MutableRef {
	cacheRef := &cacheRef{cacheRefShare: r}
	r.mu.Lock()
	r.refs[cacheRef] = struct{}{}
	r.mu.Unlock()
	return cacheRef
}

func (r *cacheRefShare) release(ctx context.Context) error {
	if r.main != nil {
		r.main.mu.Lock()
		defer r.main.mu.Unlock()
		delete(r.main.shares, r.key)
	}
	return r.MutableRef.Release(ctx)
}

type cacheRef struct {
	*cacheRefShare
}

func (r *cacheRef) Release(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.refs, r)
	if len(r.refs) == 0 {
		return r.release(ctx)
	}
	return nil
}

func parseExtraHosts(ips []*pb.HostIP) ([]executor.HostIP, error) {
	out := make([]executor.HostIP, len(ips))
	for i, hip := range ips {
		ip := net.ParseIP(hip.IP)
		if ip == nil {
			return nil, errors.Errorf("failed to parse IP %s", hip.IP)
		}
		out[i] = executor.HostIP{
			IP:   ip,
			Host: hip.Host,
		}
	}
	return out, nil
}
