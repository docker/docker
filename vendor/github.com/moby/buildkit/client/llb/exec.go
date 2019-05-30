package llb

import (
	_ "crypto/sha256"
	"fmt"
	"net"
	"sort"

	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/system"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type Meta struct {
	Args       []string
	Env        EnvList
	Cwd        string
	User       string
	ProxyEnv   *ProxyEnv
	ExtraHosts []HostIP
	Network    pb.NetMode
	Security   pb.SecurityMode
}

func NewExecOp(root Output, meta Meta, readOnly bool, c Constraints) *ExecOp {
	e := &ExecOp{meta: meta, constraints: c}
	rootMount := &mount{
		target:   pb.RootMount,
		source:   root,
		readonly: readOnly,
	}
	e.mounts = append(e.mounts, rootMount)
	if readOnly {
		e.root = root
	} else {
		o := &output{vertex: e, getIndex: e.getMountIndexFn(rootMount)}
		if p := c.Platform; p != nil {
			o.platform = p
		}
		e.root = o
	}
	rootMount.output = e.root
	return e
}

type mount struct {
	target       string
	readonly     bool
	source       Output
	output       Output
	selector     string
	cacheID      string
	tmpfs        bool
	cacheSharing CacheMountSharingMode
	noOutput     bool
}

type ExecOp struct {
	MarshalCache
	root        Output
	mounts      []*mount
	meta        Meta
	constraints Constraints
	isValidated bool
	secrets     []SecretInfo
	ssh         []SSHInfo
}

func (e *ExecOp) AddMount(target string, source Output, opt ...MountOption) Output {
	m := &mount{
		target: target,
		source: source,
	}
	for _, o := range opt {
		o(m)
	}
	e.mounts = append(e.mounts, m)
	if m.readonly {
		m.output = source
	} else if m.tmpfs {
		m.output = &output{vertex: e, err: errors.Errorf("tmpfs mount for %s can't be used as a parent", target)}
	} else if m.noOutput {
		m.output = &output{vertex: e, err: errors.Errorf("mount marked no-output and %s can't be used as a parent", target)}
	} else {
		o := &output{vertex: e, getIndex: e.getMountIndexFn(m)}
		if p := e.constraints.Platform; p != nil {
			o.platform = p
		}
		m.output = o
	}
	e.Store(nil, nil, nil)
	e.isValidated = false
	return m.output
}

func (e *ExecOp) GetMount(target string) Output {
	for _, m := range e.mounts {
		if m.target == target {
			return m.output
		}
	}
	return nil
}

func (e *ExecOp) Validate() error {
	if e.isValidated {
		return nil
	}
	if len(e.meta.Args) == 0 {
		return errors.Errorf("arguments are required")
	}
	if e.meta.Cwd == "" {
		return errors.Errorf("working directory is required")
	}
	for _, m := range e.mounts {
		if m.source != nil {
			if err := m.source.Vertex().Validate(); err != nil {
				return err
			}
		}
	}
	e.isValidated = true
	return nil
}

func (e *ExecOp) Marshal(c *Constraints) (digest.Digest, []byte, *pb.OpMetadata, error) {
	if e.Cached(c) {
		return e.Load()
	}
	if err := e.Validate(); err != nil {
		return "", nil, nil, err
	}
	// make sure mounts are sorted
	sort.Slice(e.mounts, func(i, j int) bool {
		return e.mounts[i].target < e.mounts[j].target
	})

	if len(e.ssh) > 0 {
		for i, s := range e.ssh {
			if s.Target == "" {
				e.ssh[i].Target = fmt.Sprintf("/run/buildkit/ssh_agent.%d", i)
			}
		}
		if _, ok := e.meta.Env.Get("SSH_AUTH_SOCK"); !ok {
			e.meta.Env = e.meta.Env.AddOrReplace("SSH_AUTH_SOCK", e.ssh[0].Target)
		}
	}
	if c.Caps != nil {
		if err := c.Caps.Supports(pb.CapExecMetaSetsDefaultPath); err != nil {
			e.meta.Env = e.meta.Env.SetDefault("PATH", system.DefaultPathEnv)
		} else {
			addCap(&e.constraints, pb.CapExecMetaSetsDefaultPath)
		}
	}

	meta := &pb.Meta{
		Args: e.meta.Args,
		Env:  e.meta.Env.ToArray(),
		Cwd:  e.meta.Cwd,
		User: e.meta.User,
	}
	if len(e.meta.ExtraHosts) > 0 {
		hosts := make([]*pb.HostIP, len(e.meta.ExtraHosts))
		for i, h := range e.meta.ExtraHosts {
			hosts[i] = &pb.HostIP{Host: h.Host, IP: h.IP.String()}
		}
		meta.ExtraHosts = hosts
	}

	peo := &pb.ExecOp{
		Meta:     meta,
		Network:  e.meta.Network,
		Security: e.meta.Security,
	}
	if e.meta.Network != NetModeSandbox {
		addCap(&e.constraints, pb.CapExecMetaNetwork)
	}

	if e.meta.Security != SecurityModeSandbox {
		addCap(&e.constraints, pb.CapExecMetaSecurity)
	}

	if p := e.meta.ProxyEnv; p != nil {
		peo.Meta.ProxyEnv = &pb.ProxyEnv{
			HttpProxy:  p.HttpProxy,
			HttpsProxy: p.HttpsProxy,
			FtpProxy:   p.FtpProxy,
			NoProxy:    p.NoProxy,
		}
		addCap(&e.constraints, pb.CapExecMetaProxy)
	}

	addCap(&e.constraints, pb.CapExecMetaBase)

	for _, m := range e.mounts {
		if m.selector != "" {
			addCap(&e.constraints, pb.CapExecMountSelector)
		}
		if m.cacheID != "" {
			addCap(&e.constraints, pb.CapExecMountCache)
			addCap(&e.constraints, pb.CapExecMountCacheSharing)
		} else if m.tmpfs {
			addCap(&e.constraints, pb.CapExecMountTmpfs)
		} else if m.source != nil {
			addCap(&e.constraints, pb.CapExecMountBind)
		}
	}

	if len(e.secrets) > 0 {
		addCap(&e.constraints, pb.CapExecMountSecret)
	}

	if len(e.ssh) > 0 {
		addCap(&e.constraints, pb.CapExecMountSSH)
	}

	pop, md := MarshalConstraints(c, &e.constraints)
	pop.Op = &pb.Op_Exec{
		Exec: peo,
	}

	outIndex := 0
	for _, m := range e.mounts {
		inputIndex := pb.InputIndex(len(pop.Inputs))
		if m.source != nil {
			if m.tmpfs {
				return "", nil, nil, errors.Errorf("tmpfs mounts must use scratch")
			}
			inp, err := m.source.ToInput(c)
			if err != nil {
				return "", nil, nil, err
			}

			newInput := true

			for i, inp2 := range pop.Inputs {
				if *inp == *inp2 {
					inputIndex = pb.InputIndex(i)
					newInput = false
					break
				}
			}

			if newInput {
				pop.Inputs = append(pop.Inputs, inp)
			}
		} else {
			inputIndex = pb.Empty
		}

		outputIndex := pb.OutputIndex(-1)
		if !m.noOutput && !m.readonly && m.cacheID == "" && !m.tmpfs {
			outputIndex = pb.OutputIndex(outIndex)
			outIndex++
		}

		pm := &pb.Mount{
			Input:    inputIndex,
			Dest:     m.target,
			Readonly: m.readonly,
			Output:   outputIndex,
			Selector: m.selector,
		}
		if m.cacheID != "" {
			pm.MountType = pb.MountType_CACHE
			pm.CacheOpt = &pb.CacheOpt{
				ID: m.cacheID,
			}
			switch m.cacheSharing {
			case CacheMountShared:
				pm.CacheOpt.Sharing = pb.CacheSharingOpt_SHARED
			case CacheMountPrivate:
				pm.CacheOpt.Sharing = pb.CacheSharingOpt_PRIVATE
			case CacheMountLocked:
				pm.CacheOpt.Sharing = pb.CacheSharingOpt_LOCKED
			}
		}
		if m.tmpfs {
			pm.MountType = pb.MountType_TMPFS
		}
		peo.Mounts = append(peo.Mounts, pm)
	}

	for _, s := range e.secrets {
		pm := &pb.Mount{
			Dest:      s.Target,
			MountType: pb.MountType_SECRET,
			SecretOpt: &pb.SecretOpt{
				ID:       s.ID,
				Uid:      uint32(s.UID),
				Gid:      uint32(s.GID),
				Optional: s.Optional,
				Mode:     uint32(s.Mode),
			},
		}
		peo.Mounts = append(peo.Mounts, pm)
	}

	for _, s := range e.ssh {
		pm := &pb.Mount{
			Dest:      s.Target,
			MountType: pb.MountType_SSH,
			SSHOpt: &pb.SSHOpt{
				ID:       s.ID,
				Uid:      uint32(s.UID),
				Gid:      uint32(s.GID),
				Mode:     uint32(s.Mode),
				Optional: s.Optional,
			},
		}
		peo.Mounts = append(peo.Mounts, pm)
	}

	dt, err := pop.Marshal()
	if err != nil {
		return "", nil, nil, err
	}
	e.Store(dt, md, c)
	return e.Load()
}

func (e *ExecOp) Output() Output {
	return e.root
}

func (e *ExecOp) Inputs() (inputs []Output) {
	mm := map[Output]struct{}{}
	for _, m := range e.mounts {
		if m.source != nil {
			mm[m.source] = struct{}{}
		}
	}
	for o := range mm {
		inputs = append(inputs, o)
	}
	return
}

func (e *ExecOp) getMountIndexFn(m *mount) func() (pb.OutputIndex, error) {
	return func() (pb.OutputIndex, error) {
		// make sure mounts are sorted
		sort.Slice(e.mounts, func(i, j int) bool {
			return e.mounts[i].target < e.mounts[j].target
		})

		i := 0
		for _, m2 := range e.mounts {
			if m2.noOutput || m2.readonly || m2.cacheID != "" {
				continue
			}
			if m == m2 {
				return pb.OutputIndex(i), nil
			}
			i++
		}
		return pb.OutputIndex(0), errors.Errorf("invalid mount: %s", m.target)
	}
}

type ExecState struct {
	State
	exec *ExecOp
}

func (e ExecState) AddMount(target string, source State, opt ...MountOption) State {
	return source.WithOutput(e.exec.AddMount(target, source.Output(), opt...))
}

func (e ExecState) GetMount(target string) State {
	return NewState(e.exec.GetMount(target))
}

func (e ExecState) Root() State {
	return e.State
}

type MountOption func(*mount)

func Readonly(m *mount) {
	m.readonly = true
}

func SourcePath(src string) MountOption {
	return func(m *mount) {
		m.selector = src
	}
}

func ForceNoOutput(m *mount) {
	m.noOutput = true
}

func AsPersistentCacheDir(id string, sharing CacheMountSharingMode) MountOption {
	return func(m *mount) {
		m.cacheID = id
		m.cacheSharing = sharing
	}
}

func Tmpfs() MountOption {
	return func(m *mount) {
		m.tmpfs = true
	}
}

type RunOption interface {
	SetRunOption(es *ExecInfo)
}

type runOptionFunc func(*ExecInfo)

func (fn runOptionFunc) SetRunOption(ei *ExecInfo) {
	fn(ei)
}

func Network(n pb.NetMode) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = network(n)(ei.State)
	})
}

func Security(s pb.SecurityMode) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = security(s)(ei.State)
	})
}

func Shlex(str string) RunOption {
	return Shlexf(str)
}
func Shlexf(str string, v ...interface{}) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = shlexf(str, v...)(ei.State)
	})
}

func Args(a []string) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = args(a...)(ei.State)
	})
}

func AddEnv(key, value string) RunOption {
	return AddEnvf(key, value)
}

func AddEnvf(key, value string, v ...interface{}) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = ei.State.AddEnvf(key, value, v...)
	})
}

func User(str string) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = ei.State.User(str)
	})
}

func Dir(str string) RunOption {
	return Dirf(str)
}
func Dirf(str string, v ...interface{}) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = ei.State.Dirf(str, v...)
	})
}

func AddExtraHost(host string, ip net.IP) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = ei.State.AddExtraHost(host, ip)
	})
}

func Reset(s State) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = ei.State.Reset(s)
	})
}

func With(so ...StateOption) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.State = ei.State.With(so...)
	})
}

func AddMount(dest string, mountState State, opts ...MountOption) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.Mounts = append(ei.Mounts, MountInfo{dest, mountState.Output(), opts})
	})
}

func AddSSHSocket(opts ...SSHOption) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		s := &SSHInfo{
			Mode: 0600,
		}
		for _, opt := range opts {
			opt.SetSSHOption(s)
		}
		ei.SSH = append(ei.SSH, *s)
	})
}

type SSHOption interface {
	SetSSHOption(*SSHInfo)
}

type sshOptionFunc func(*SSHInfo)

func (fn sshOptionFunc) SetSSHOption(si *SSHInfo) {
	fn(si)
}

func SSHID(id string) SSHOption {
	return sshOptionFunc(func(si *SSHInfo) {
		si.ID = id
	})
}

func SSHSocketTarget(target string) SSHOption {
	return sshOptionFunc(func(si *SSHInfo) {
		si.Target = target
	})
}

func SSHSocketOpt(target string, uid, gid, mode int) SSHOption {
	return sshOptionFunc(func(si *SSHInfo) {
		si.Target = target
		si.UID = uid
		si.GID = gid
		si.Mode = mode
	})
}

var SSHOptional = sshOptionFunc(func(si *SSHInfo) {
	si.Optional = true
})

type SSHInfo struct {
	ID       string
	Target   string
	Mode     int
	UID      int
	GID      int
	Optional bool
}

func AddSecret(dest string, opts ...SecretOption) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		s := &SecretInfo{ID: dest, Target: dest, Mode: 0400}
		for _, opt := range opts {
			opt.SetSecretOption(s)
		}
		ei.Secrets = append(ei.Secrets, *s)
	})
}

type SecretOption interface {
	SetSecretOption(*SecretInfo)
}

type secretOptionFunc func(*SecretInfo)

func (fn secretOptionFunc) SetSecretOption(si *SecretInfo) {
	fn(si)
}

type SecretInfo struct {
	ID       string
	Target   string
	Mode     int
	UID      int
	GID      int
	Optional bool
}

var SecretOptional = secretOptionFunc(func(si *SecretInfo) {
	si.Optional = true
})

func SecretID(id string) SecretOption {
	return secretOptionFunc(func(si *SecretInfo) {
		si.ID = id
	})
}

func SecretFileOpt(uid, gid, mode int) SecretOption {
	return secretOptionFunc(func(si *SecretInfo) {
		si.UID = uid
		si.GID = gid
		si.Mode = mode
	})
}

func ReadonlyRootFS() RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.ReadonlyRootFS = true
	})
}

func WithProxy(ps ProxyEnv) RunOption {
	return runOptionFunc(func(ei *ExecInfo) {
		ei.ProxyEnv = &ps
	})
}

type ExecInfo struct {
	constraintsWrapper
	State          State
	Mounts         []MountInfo
	ReadonlyRootFS bool
	ProxyEnv       *ProxyEnv
	Secrets        []SecretInfo
	SSH            []SSHInfo
}

type MountInfo struct {
	Target string
	Source Output
	Opts   []MountOption
}

type ProxyEnv struct {
	HttpProxy  string
	HttpsProxy string
	FtpProxy   string
	NoProxy    string
}

type CacheMountSharingMode int

const (
	CacheMountShared CacheMountSharingMode = iota
	CacheMountPrivate
	CacheMountLocked
)

const (
	NetModeSandbox = pb.NetMode_UNSET
	NetModeHost    = pb.NetMode_HOST
	NetModeNone    = pb.NetMode_NONE
)

const (
	SecurityModeInsecure = pb.SecurityMode_INSECURE
	SecurityModeSandbox  = pb.SecurityMode_SANDBOX
)
