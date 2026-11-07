package buildkit

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/platforms"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/control"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/entitlements"
	"github.com/moby/buildkit/util/tracing"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/builder"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/images"
	"github.com/moby/moby/v2/daemon/internal/builder-next/exporter"
	"github.com/moby/moby/v2/daemon/internal/builder-next/exporter/mobyexporter"
	"github.com/moby/moby/v2/daemon/internal/builder-next/exporter/overrides"
	"github.com/moby/moby/v2/daemon/internal/ocihook"
	"github.com/moby/moby/v2/daemon/internal/streamformatter"
	"github.com/moby/moby/v2/daemon/internal/timestamp"
	"github.com/moby/moby/v2/daemon/libnetwork"
	"github.com/moby/moby/v2/daemon/pkg/opts"
	"github.com/moby/moby/v2/daemon/server/buildbackend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/sys/user"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"resenje.org/singleflight"
	"tags.cncf.io/container-device-interface/pkg/cdi"
)

type errMultipleFilterValues struct{}

func (errMultipleFilterValues) Error() string { return "filters expect only one value" }

func (errMultipleFilterValues) InvalidParameter() {}

type errConflictFilter struct {
	a, b string
}

func (e errConflictFilter) Error() string {
	return fmt.Sprintf("conflicting filters: %q and %q", e.a, e.b)
}

func (errConflictFilter) InvalidParameter() {}

type errInvalidFilterValue struct {
	error
}

func (errInvalidFilterValue) InvalidParameter() {}

var cacheFields = map[string]bool{
	"id":          true,
	"parent":      true,
	"type":        true,
	"description": true,
	"inuse":       true,
	"shared":      true,
	"private":     true,
	// fields from buildkit that are not exposed
	"mutable":   false,
	"immutable": false,
}

// Opt is option struct required for creating the builder
type Opt struct {
	SessionManager      *session.Manager
	Root                string
	EngineID            string
	Dist                images.DistributionServices
	ImageTagger         mobyexporter.ImageTagger
	NetworkController   *libnetwork.Controller
	OCIHooks            *ocihook.Server
	DefaultCgroupParent string
	RegistryHosts       docker.RegistryHosts
	BuilderConfig       config.BuilderConfig
	Rootless            bool
	IdentityMapping     user.IdentityMapping
	DNSConfig           config.DNSConfig
	ApparmorProfile     string
	UseSnapshotter      bool
	Snapshotter         string
	ContainerdAddress   string
	// ContainerdDialer, when set (embedded mode), connects to the in-process
	// containerd over an in-memory pipe so BuildKit avoids socket syscalls.
	// ContainerdAddress is still the gRPC target.
	ContainerdDialer    func(ctx context.Context, address string) (net.Conn, error)
	ContainerdNamespace string
	HyperVIsolation     bool
	Callbacks           exporter.BuildkitCallbacks
	CDICache            *cdi.Cache
}

// Builder can build using BuildKit backend
type Builder struct {
	controller     *control.Controller
	dnsconfig      config.DNSConfig
	reqBodyHandler *reqBodyHandler
	diskUsage      singleflight.Group[buildbackend.DiskUsageOptions, *buildbackend.DiskUsage]

	mu             sync.Mutex
	jobs           map[string]*buildJob
	useSnapshotter bool
}

// New creates a new builder
func New(ctx context.Context, opt Opt) (*Builder, error) {
	reqHandler := newReqBodyHandler(tracing.DefaultTransport)

	c, err := newController(ctx, reqHandler, opt)
	if err != nil {
		return nil, err
	}
	b := &Builder{
		controller:     c,
		dnsconfig:      opt.DNSConfig,
		reqBodyHandler: reqHandler,
		jobs:           map[string]*buildJob{},
		useSnapshotter: opt.UseSnapshotter,
	}
	return b, nil
}

func (b *Builder) Close() error {
	return b.controller.Close()
}

// RegisterGRPC registers controller to the grpc server.
func (b *Builder) RegisterGRPC(s *grpc.Server) {
	b.controller.Register(s)
}

// Cancel cancels a build using ID
func (b *Builder) Cancel(ctx context.Context, id string) error {
	b.mu.Lock()
	if j, ok := b.jobs[id]; ok && j.cancel != nil {
		j.cancel()
	}
	b.mu.Unlock()
	return nil
}

// DiskUsage returns a report about space used by build cache
func (b *Builder) DiskUsage(ctx context.Context, options buildbackend.DiskUsageOptions) (*buildbackend.DiskUsage, error) {
	res, _, err := b.diskUsage.Do(ctx, options, func(ctx context.Context) (*buildbackend.DiskUsage, error) {
		duResp, err := b.controller.DiskUsage(ctx, &controlapi.DiskUsageRequest{})
		if err != nil {
			return nil, fmt.Errorf("error getting build cache usage: %w", err)
		}

		var usage buildbackend.DiskUsage
		for _, r := range duResp.Record {
			usage.TotalCount++
			usage.TotalSize += r.Size
			if r.InUse {
				usage.ActiveCount++
			}
			if !r.InUse && !r.Shared {
				usage.Reclaimable += r.Size
			}
			if !options.Verbose {
				continue
			}
			usage.Items = append(usage.Items, build.CacheRecord{
				ID:          r.ID,
				Parents:     r.Parents,
				Type:        r.RecordType,
				Description: r.Description,
				InUse:       r.InUse,
				Shared:      r.Shared,
				Size:        r.Size,
				CreatedAt: func() time.Time {
					if r.CreatedAt != nil {
						return r.CreatedAt.AsTime()
					}
					return time.Time{}
				}(),
				LastUsedAt: func() *time.Time {
					if r.LastUsedAt == nil {
						return nil
					}
					t := r.LastUsedAt.AsTime()
					return &t
				}(),
				UsageCount: int(r.UsageCount),
			})
		}
		return &usage, nil
	})
	return res, err
}

// Prune clears all reclaimable build cache.
func (b *Builder) Prune(ctx context.Context, opts buildbackend.CachePruneOptions) (int64, []string, error) {
	ch := make(chan *controlapi.UsageRecord)

	eg, ctx := errgroup.WithContext(ctx)

	validFilters := make(map[string]bool, 1+len(cacheFields))
	validFilters["unused-for"] = true
	validFilters["until"] = true
	validFilters["label"] = true  // TODO(tiborvass): handle label
	validFilters["label!"] = true // TODO(tiborvass): handle label!
	maps.Copy(validFilters, cacheFields)
	if err := opts.Filters.Validate(validFilters); err != nil {
		return 0, nil, err
	}

	pi, err := toBuildkitPruneInfo(opts)
	if err != nil {
		return 0, nil, err
	}

	eg.Go(func() error {
		defer close(ch)
		return b.controller.Prune(&controlapi.PruneRequest{
			All:           pi.All,
			KeepDuration:  int64(pi.KeepDuration),
			ReservedSpace: pi.ReservedSpace,
			MaxUsedSpace:  pi.MaxUsedSpace,
			MinFreeSpace:  pi.MinFreeSpace,
			Filter:        pi.Filter,
		}, &pruneProxy{
			streamProxy: streamProxy{ctx: ctx},
			ch:          ch,
		})
	})

	var size int64
	var cacheIDs []string
	eg.Go(func() error {
		for r := range ch {
			size += r.Size
			cacheIDs = append(cacheIDs, r.ID)
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		return 0, nil, err
	}

	return size, cacheIDs, nil
}

// Build executes a build request
func (b *Builder) Build(ctx context.Context, opt buildbackend.BuildConfig) (*builder.Result, error) {
	if len(opt.Options.Outputs) > 1 {
		return nil, errors.Errorf("multiple outputs not supported")
	}

	rc := opt.Source
	if buildID := opt.Options.BuildID; buildID != "" {
		b.mu.Lock()

		upload := false
		if strings.HasPrefix(buildID, "upload-request:") {
			upload = true
			buildID = strings.TrimPrefix(buildID, "upload-request:")
		}

		if _, ok := b.jobs[buildID]; !ok {
			b.jobs[buildID] = newBuildJob()
		}
		j := b.jobs[buildID]
		var cancel func()
		ctx, cancel = context.WithCancel(ctx)
		j.cancel = cancel
		b.mu.Unlock()

		if upload {
			ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			err := j.SetUpload(ctx2, rc)
			return nil, err
		}

		if remoteContext := opt.Options.RemoteContext; remoteContext == "upload-request" {
			ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var err error
			rc, err = j.WaitUpload(ctx2)
			if err != nil {
				return nil, err
			}
			opt.Options.RemoteContext = ""
		}

		defer func() {
			b.mu.Lock()
			delete(b.jobs, buildID)
			b.mu.Unlock()
		}()
	}

	var out builder.Result

	frontendAttrs := map[string]string{}

	if opt.Options.Target != "" {
		frontendAttrs["target"] = opt.Options.Target
	}

	if opt.Options.Dockerfile != "" && opt.Options.Dockerfile != "." {
		frontendAttrs["filename"] = opt.Options.Dockerfile
	}

	if opt.Options.RemoteContext != "" {
		if opt.Options.RemoteContext != "client-session" {
			frontendAttrs["context"] = opt.Options.RemoteContext
		}
	} else {
		url, cancel := b.reqBodyHandler.newRequest(rc)
		defer cancel()
		frontendAttrs["context"] = url
	}

	cacheFrom := append([]string{}, opt.Options.CacheFrom...)

	frontendAttrs["cache-from"] = strings.Join(cacheFrom, ",")

	for k, v := range opt.Options.BuildArgs {
		if v == nil {
			continue
		}
		frontendAttrs["build-arg:"+k] = *v
	}

	for k, v := range opt.Options.Labels {
		frontendAttrs["label:"+k] = v
	}

	if opt.Options.NoCache {
		frontendAttrs["no-cache"] = ""
	}

	if opt.Options.PullParent {
		frontendAttrs["image-resolve-mode"] = "pull"
	} else {
		frontendAttrs["image-resolve-mode"] = "default"
	}

	if opt.Options.Platform != "" {
		// same as in newBuilder in builder/dockerfile.builder.go
		// TODO: remove once opt.Options.Platform is of type specs.Platform
		_, err := platforms.Parse(opt.Options.Platform)
		if err != nil {
			return nil, errdefs.InvalidParameter(err)
		}
		frontendAttrs["platform"] = opt.Options.Platform
	}

	switch opt.Options.NetworkMode {
	case network.NetworkHost, network.NetworkNone:
		frontendAttrs["force-network-mode"] = opt.Options.NetworkMode
	case "", network.NetworkDefault:
	default:
		return nil, errors.Errorf("network mode %q not supported by buildkit", opt.Options.NetworkMode)
	}

	extraHosts, err := toBuildkitExtraHosts(opt.Options.ExtraHosts, b.dnsconfig.HostGatewayIPs)
	if err != nil {
		return nil, err
	}
	frontendAttrs["add-hosts"] = extraHosts

	if opt.Options.ShmSize > 0 {
		frontendAttrs["shm-size"] = strconv.FormatInt(opt.Options.ShmSize, 10)
	}

	ulimits, err := toBuildkitUlimits(opt.Options.Ulimits)
	if err != nil {
		return nil, err
	} else if ulimits != "" {
		frontendAttrs["ulimit"] = ulimits
	}

	exporterName := ""
	exporterAttrs := map[string]string{}
	if len(opt.Options.Outputs) == 0 {
		exporterName = exporter.Moby
	} else {
		// cacheonly is a special type for triggering skipping all exporters
		if opt.Options.Outputs[0].Type != "cacheonly" {
			exporterName = opt.Options.Outputs[0].Type
			exporterAttrs = opt.Options.Outputs[0].Attrs
		}
	}

	if (exporterName == client.ExporterImage || exporterName == exporter.Moby) && len(opt.Options.Tags) > 0 {
		nameAttr, err := overrides.SanitizeRepoAndTags(opt.Options.Tags)
		if err != nil {
			return nil, err
		}
		if exporterAttrs == nil {
			exporterAttrs = make(map[string]string)
		}
		exporterAttrs["name"] = strings.Join(nameAttr, ",")
	}

	cache := &controlapi.CacheOptions{}
	if inlineCache := opt.Options.BuildArgs["BUILDKIT_INLINE_CACHE"]; inlineCache != nil {
		if b, err := strconv.ParseBool(*inlineCache); err == nil && b {
			cache.Exports = append(cache.Exports, &controlapi.CacheOptionsEntry{
				Type: "inline",
			})
		}
	}

	id := identity.NewID()
	req := &controlapi.SolveRequest{
		Ref: id,
		Exporters: []*controlapi.Exporter{
			{Type: exporterName, Attrs: exporterAttrs},
		},
		Frontend:      "dockerfile.v0",
		FrontendAttrs: frontendAttrs,
		Session:       opt.Options.SessionID,
		Cache:         cache,
	}

	if opt.Options.NetworkMode == network.NetworkHost {
		req.Entitlements = append(req.Entitlements, string(entitlements.EntitlementNetworkHost))
	}

	aux := streamformatter.AuxFormatter{Writer: opt.ProgressWriter.Output}

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		resp, err := b.controller.Solve(ctx, req)
		if err != nil {
			return err
		}
		if exporterName != exporter.Moby && exporterName != client.ExporterImage {
			return nil
		}
		imgID, ok := resp.ExporterResponse["containerimage.digest"]
		if !ok {
			return errors.Errorf("missing image id")
		}
		out.ImageID = imgID
		return aux.Emit("moby.image.id", build.Result{ID: imgID})
	})

	ch := make(chan *controlapi.StatusResponse)

	eg.Go(func() error {
		defer close(ch)
		// streamProxy.ctx is not set to ctx because when request is cancelled,
		// only the build request has to be cancelled, not the status request.
		stream := &statusProxy{streamProxy: streamProxy{ctx: context.TODO()}, ch: ch}
		return b.controller.Status(&controlapi.StatusRequest{Ref: id}, stream)
	})

	eg.Go(func() error {
		for sr := range ch {
			dt, err := proto.Marshal(sr)
			if err != nil {
				return err
			}
			if err := aux.Emit("moby.buildkit.trace", dt); err != nil {
				return err
			}
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return &out, nil
}

type streamProxy struct {
	ctx context.Context
}

func (sp *streamProxy) SetHeader(_ grpcmetadata.MD) error {
	return nil
}

func (sp *streamProxy) SendHeader(_ grpcmetadata.MD) error {
	return nil
}

func (sp *streamProxy) SetTrailer(_ grpcmetadata.MD) {
}

func (sp *streamProxy) Context() context.Context {
	return sp.ctx
}

func (sp *streamProxy) RecvMsg(m any) error {
	return io.EOF
}

type statusProxy struct {
	streamProxy
	ch chan *controlapi.StatusResponse
}

func (sp *statusProxy) Send(resp *controlapi.StatusResponse) error {
	return sp.SendMsg(resp)
}

func (sp *statusProxy) SendMsg(m any) error {
	if sr, ok := m.(*controlapi.StatusResponse); ok {
		sp.ch <- sr
	}
	return nil
}

type pruneProxy struct {
	streamProxy
	ch chan *controlapi.UsageRecord
}

func (sp *pruneProxy) Send(resp *controlapi.UsageRecord) error {
	return sp.SendMsg(resp)
}

func (sp *pruneProxy) SendMsg(m any) error {
	if sr, ok := m.(*controlapi.UsageRecord); ok {
		sp.ch <- sr
	}
	return nil
}

type wrapRC struct {
	io.ReadCloser
	once   sync.Once
	err    error
	waitCh chan struct{}
}

func (w *wrapRC) Read(b []byte) (int, error) {
	n, err := w.ReadCloser.Read(b)
	if err != nil {
		switch err {
		case io.EOF:
			w.close(nil)
		default:
			w.close(err)
		}
	}
	return n, err
}

func (w *wrapRC) Close() error {
	err := w.ReadCloser.Close()
	w.close(err)
	return err
}

func (w *wrapRC) close(err error) {
	w.once.Do(func() {
		w.err = err
		close(w.waitCh)
	})
}

func (w *wrapRC) wait() error {
	<-w.waitCh
	return w.err
}

type buildJob struct {
	cancel func()
	waitCh chan func(io.ReadCloser) error
}

func newBuildJob() *buildJob {
	return &buildJob{waitCh: make(chan func(io.ReadCloser) error)}
}

func (j *buildJob) WaitUpload(ctx context.Context) (io.ReadCloser, error) {
	done := make(chan struct{})

	var upload io.ReadCloser
	fn := func(rc io.ReadCloser) error {
		w := &wrapRC{ReadCloser: rc, waitCh: make(chan struct{})}
		upload = w
		close(done)
		return w.wait()
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case j.waitCh <- fn:
		<-done
		return upload, nil
	}
}

func (j *buildJob) SetUpload(ctx context.Context, rc io.ReadCloser) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case fn := <-j.waitCh:
		return fn(rc)
	}
}

// toBuildkitExtraHosts converts hosts from docker key:value format to buildkit's csv format
func toBuildkitExtraHosts(inp []string, hostGatewayIPs []netip.Addr) (string, error) {
	if len(inp) == 0 {
		return "", nil
	}
	hosts := make([]string, 0, len(inp))
	for _, h := range inp {
		host, ip, ok := strings.Cut(h, ":")
		if !ok || host == "" || ip == "" {
			return "", errors.Errorf("invalid host %s", h)
		}
		// If the IP Address is a "host-gateway", replace this value with the
		// IP address(es) stored in the daemon level HostGatewayIPs config variable.
		if ip == opts.HostGatewayName {
			if len(hostGatewayIPs) == 0 {
				return "", errors.New("unable to derive the IP value for host-gateway")
			}
			for _, gip := range hostGatewayIPs {
				hosts = append(hosts, host+"="+gip.String())
			}
		} else {
			hosts = append(hosts, host+"="+ip)
		}
	}
	return strings.Join(hosts, ","), nil
}

// toBuildkitUlimits converts ulimits from docker type=soft:hard format to buildkit's csv format
func toBuildkitUlimits(inp []*container.Ulimit) (string, error) {
	if len(inp) == 0 {
		return "", nil
	}
	ulimits := make([]string, 0, len(inp))
	for _, ulimit := range inp {
		ulimits = append(ulimits, ulimit.String())
	}
	return strings.Join(ulimits, ","), nil
}

func toBuildkitPruneInfo(opts buildbackend.CachePruneOptions) (client.PruneInfo, error) {
	var until time.Duration
	untilValues := opts.Filters.Get("until")          // canonical
	unusedForValues := opts.Filters.Get("unused-for") // deprecated synonym for "until" filter

	if len(untilValues) > 0 && len(unusedForValues) > 0 {
		return client.PruneInfo{}, errConflictFilter{"until", "unused-for"}
	}
	filterKey := "until"
	if len(unusedForValues) > 0 {
		filterKey = "unused-for"
	}
	untilValues = append(untilValues, unusedForValues...)

	switch len(untilValues) {
	case 0:
		// nothing to do
	case 1:
		parsed, err := timestamp.Parse(untilValues[0], time.Now())
		if err != nil {
			return client.PruneInfo{}, errInvalidFilterValue{
				errors.Wrapf(err, "%q filter expects a duration (e.g., '24h') or a timestamp", filterKey),
			}
		}
		until = time.Since(parsed)
	default:
		return client.PruneInfo{}, errMultipleFilterValues{}
	}

	bkFilter := make([]string, 0, opts.Filters.Len())
	for cacheField := range cacheFields {
		if opts.Filters.Contains(cacheField) {
			values := opts.Filters.Get(cacheField)
			switch len(values) {
			case 0:
				bkFilter = append(bkFilter, cacheField)
			case 1:
				if cacheField == "id" {
					bkFilter = append(bkFilter, cacheField+"~="+values[0])
				} else {
					bkFilter = append(bkFilter, cacheField+"=="+values[0])
				}
			default:
				return client.PruneInfo{}, errMultipleFilterValues{}
			}
		}
	}

	return client.PruneInfo{
		All:           opts.All,
		KeepDuration:  until,
		ReservedSpace: opts.ReservedSpace,
		MaxUsedSpace:  opts.MaxUsedSpace,
		MinFreeSpace:  opts.MinFreeSpace,
		Filter:        []string{strings.Join(bkFilter, ",")},
	}, nil
}
