package llb

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

type StateOption func(State) State

type Output interface {
	ToInput(context.Context, *Constraints) (*pb.Input, error)
	Vertex(context.Context, *Constraints) Vertex
}

type Vertex interface {
	Validate(context.Context, *Constraints) error
	Marshal(context.Context, *Constraints) (digest.Digest, []byte, *pb.OpMetadata, []*SourceLocation, error)
	Output() Output
	Inputs() []Output
}

func NewConstraints(co ...ConstraintsOpt) *Constraints {
	defaultPlatform := platforms.Normalize(platforms.DefaultSpec())
	c := &Constraints{
		Platform:      &defaultPlatform,
		LocalUniqueID: identity.NewID(),
	}
	for _, o := range co {
		o.SetConstraintsOption(c)
	}
	return c
}

func NewState(o Output) State {
	s := State{
		out: o,
	}.Dir("/")
	s = s.ensurePlatform()
	return s
}

// State represents all operations that must be done to produce a given output.
// States are immutable, and all operations return a new state linked to the previous one.
// State is the core type of the LLB API and is used to build a graph of operations.
// The graph is then marshaled into a definition that can be executed by a backend (such as buildkitd).
//
// Operations performed on a State are executed lazily after the entire state graph is marshalled and sent to the backend.
type State struct {
	out   Output
	prev  *State
	key   any
	value func(context.Context, *Constraints) (any, error)
	opts  []ConstraintsOpt
	async *asyncState
}

func (s State) ensurePlatform() State {
	if o, ok := s.out.(interface {
		Platform() *ocispecs.Platform
	}); ok {
		if p := o.Platform(); p != nil {
			s = platform(*p)(s)
		}
	}
	return s
}

func (s State) WithValue(k, v any) State {
	return s.withValue(k, func(context.Context, *Constraints) (any, error) {
		return v, nil
	})
}

func (s State) withValue(k any, v func(context.Context, *Constraints) (any, error)) State {
	return State{
		out:   s.Output(),
		prev:  &s, // doesn't need to be original pointer
		key:   k,
		value: v,
	}
}

func (s State) Value(ctx context.Context, k any, co ...ConstraintsOpt) (any, error) {
	c := &Constraints{}
	for _, f := range co {
		f.SetConstraintsOption(c)
	}
	return s.getValue(k)(ctx, c)
}

func (s State) getValue(k any) func(context.Context, *Constraints) (any, error) {
	if s.key == k {
		return s.value
	}
	if s.async != nil {
		return func(ctx context.Context, c *Constraints) (any, error) {
			target, err := s.async.Do(ctx, c)
			if err != nil {
				return nil, err
			}
			return target.getValue(k)(ctx, c)
		}
	}
	if s.prev == nil {
		return nilValue
	}
	return s.prev.getValue(k)
}

func (s State) Async(f func(context.Context, State, *Constraints) (State, error)) State {
	as := &asyncState{
		f:    f,
		prev: s,
	}
	as.g.CacheError = true
	s2 := State{
		async: as,
	}
	return s2
}

func (s State) SetMarshalDefaults(co ...ConstraintsOpt) State {
	s.opts = co
	return s
}

// Marshal marshals the state and all its parents into a [Definition].
func (s State) Marshal(ctx context.Context, co ...ConstraintsOpt) (*Definition, error) {
	c := NewConstraints(append(s.opts, co...)...)
	def := &Definition{
		Metadata:    make(map[digest.Digest]OpMetadata, 0),
		Constraints: c,
	}

	if s.Output() == nil || s.Output().Vertex(ctx, c) == nil {
		return def, nil
	}
	smc := newSourceMapCollector()

	def, err := marshal(ctx, s.Output().Vertex(ctx, c), def, smc, map[digest.Digest]struct{}{}, map[Vertex]struct{}{}, c)
	if err != nil {
		return def, err
	}
	inp, err := s.Output().ToInput(ctx, c)
	if err != nil {
		return def, err
	}
	proto := &pb.Op{Inputs: []*pb.Input{inp}}
	dt, err := proto.MarshalVT()
	if err != nil {
		return def, err
	}
	def.Def = append(def.Def, dt)

	dgst := digest.FromBytes(dt)
	md := def.Metadata[dgst]
	md.Caps = map[apicaps.CapID]bool{
		pb.CapConstraints: true,
		pb.CapPlatform:    true,
	}

	for _, m := range def.Metadata {
		if m.IgnoreCache {
			md.Caps[pb.CapMetaIgnoreCache] = true
		}
		if m.Description != nil {
			md.Caps[pb.CapMetaDescription] = true
		}
		if m.ExportCache != nil {
			md.Caps[pb.CapMetaExportCache] = true
		}
	}

	def.Metadata[dgst] = md
	sm, err := smc.Marshal(ctx, co...)
	if err != nil {
		return nil, err
	}
	def.Source = sm

	return def, nil
}

func marshal(ctx context.Context, v Vertex, def *Definition, s *sourceMapCollector, cache map[digest.Digest]struct{}, vertexCache map[Vertex]struct{}, c *Constraints) (*Definition, error) {
	if _, ok := vertexCache[v]; ok {
		return def, nil
	}
	for _, inp := range v.Inputs() {
		var err error
		def, err = marshal(ctx, inp.Vertex(ctx, c), def, s, cache, vertexCache, c)
		if err != nil {
			return def, err
		}
	}

	dgst, dt, opMeta, sls, err := v.Marshal(ctx, c)
	if err != nil {
		return def, err
	}
	vertexCache[v] = struct{}{}
	if opMeta != nil {
		def.Metadata[dgst] = mergeMetadata(def.Metadata[dgst], NewOpMetadata(opMeta))
	}
	s.Add(dgst, sls)
	if _, ok := cache[dgst]; ok {
		return def, nil
	}
	def.Def = append(def.Def, dt)
	cache[dgst] = struct{}{}
	return def, nil
}

// Validate validates the state.
// This validation, unlike most other operations on [State], is not lazily performed.
func (s State) Validate(ctx context.Context, c *Constraints) error {
	return s.Output().Vertex(ctx, c).Validate(ctx, c)
}

// Output returns the output of the state.
func (s State) Output() Output {
	if s.async != nil {
		return s.async.Output()
	}
	return s.out
}

// WithOutput creates a new state with the output set to the given output.
func (s State) WithOutput(o Output) State {
	prev := s
	s = State{
		out:  o,
		prev: &prev,
	}
	s = s.ensurePlatform()
	return s
}

// WithImageConfig adds the environment variables, working directory, and platform specified in the image config to the state.
func (s State) WithImageConfig(c []byte) (State, error) {
	var img ocispecs.Image
	if err := json.Unmarshal(c, &img); err != nil {
		return State{}, err
	}
	for _, env := range img.Config.Env {
		parts := strings.SplitN(env, "=", 2)
		if len(parts[0]) > 0 {
			var v string
			if len(parts) > 1 {
				v = parts[1]
			}
			s = s.AddEnv(parts[0], v)
		}
	}
	s = s.Dir(img.Config.WorkingDir)
	if img.Architecture != "" && img.OS != "" {
		plat := ocispecs.Platform{
			OS:           img.OS,
			Architecture: img.Architecture,
			Variant:      img.Variant,
			OSVersion:    img.OSVersion,
		}
		if img.OSFeatures != nil {
			plat.OSFeatures = slices.Clone(img.OSFeatures)
		}
		s = s.Platform(plat)
	}
	return s, nil
}

// Run performs the command specified by the arguments within the context of the current [State].
// The command is executed as a container with the [State]'s filesystem as the root filesystem.
// As such any command you run must be present in the [State]'s filesystem.
// Constraints such as [State.Ulimit], [State.ParentCgroup], [State.Network], etc. are applied to the container.
//
// Run is useful when none of the LLB ops are sufficient for the operation that you want to perform.
func (s State) Run(ro ...RunOption) ExecState {
	ei := &ExecInfo{State: s}
	for _, o := range ro {
		o.SetRunOption(ei)
	}
	exec := NewExecOp(ei.State, ei.ProxyEnv, ei.ReadonlyRootFS, ei.Constraints)
	for _, m := range ei.Mounts {
		exec.AddMount(m.Target, m.Source, m.Opts...)
	}
	exec.secrets = ei.Secrets
	exec.ssh = ei.SSH
	exec.cdiDevices = ei.CDIDevices

	return ExecState{
		State: s.WithOutput(exec.Output()),
		exec:  exec,
	}
}

// File performs a file operation on the current state.
// See [FileAction] for details on the operations that can be performed.
func (s State) File(a *FileAction, opts ...ConstraintsOpt) State {
	var c Constraints
	for _, o := range opts {
		o.SetConstraintsOption(&c)
	}

	return s.WithOutput(NewFileOp(s, a, c).Output())
}

// AddEnv returns a new [State] with the provided environment variable set.
// See [AddEnv]
func (s State) AddEnv(key, value string) State {
	return AddEnv(key, value)(s)
}

// AddEnvf is the same as [State.AddEnv] but with a format string.
func (s State) AddEnvf(key, value string, v ...any) State {
	return AddEnvf(key, value, v...)(s)
}

// DelEnv returns a new [State] with the provided environment variable removed.
// See [DelEnv]
func (s State) DelEnv(key string) State {
	return DelEnv(key)(s)
}

// Dir returns a new [State] with the provided working directory set.
// See [Dir]
func (s State) Dir(str string) State {
	return Dir(str)(s)
}

// Dirf is the same as [State.Dir] but with a format string.
func (s State) Dirf(str string, v ...any) State {
	return Dirf(str, v...)(s)
}

// GetEnv returns the value of the environment variable with the provided key.
func (s State) GetEnv(ctx context.Context, key string, co ...ConstraintsOpt) (string, bool, error) {
	c := &Constraints{}
	for _, f := range co {
		f.SetConstraintsOption(c)
	}
	env, err := getEnv(s)(ctx, c)
	if err != nil {
		return "", false, err
	}
	v, ok := env.Get(key)
	return v, ok, nil
}

// Env returns the current environment variables for the state.
func (s State) Env(ctx context.Context, co ...ConstraintsOpt) (*EnvList, error) {
	c := &Constraints{}
	for _, f := range co {
		f.SetConstraintsOption(c)
	}
	return getEnv(s)(ctx, c)
}

// GetDir returns the current working directory for the state.
func (s State) GetDir(ctx context.Context, co ...ConstraintsOpt) (string, error) {
	c := &Constraints{}
	for _, f := range co {
		f.SetConstraintsOption(c)
	}
	return getDir(s)(ctx, c)
}

func (s State) GetArgs(ctx context.Context, co ...ConstraintsOpt) ([]string, error) {
	c := &Constraints{}
	for _, f := range co {
		f.SetConstraintsOption(c)
	}
	return getArgs(s)(ctx, c)
}

// Reset is used to return a new [State] with all of the current state and the
// provided [State] as the parent. In effect you can think of this as creating
// a new state with all the output from the current state but reparented to the
// provided state.  See [Reset] for more details.
func (s State) Reset(s2 State) State {
	return Reset(s2)(s)
}

func (s State) Requires(id string, deps ...State) State {
	if len(deps) == 0 {
		return s
	}
	inputs := make([]PassthroughInput, 0, len(deps)+1)
	inputs = append(inputs, PassthroughInput{State: s, Output: true})
	for _, dep := range deps {
		inputs = append(inputs, PassthroughInput{State: dep})
	}
	return s.WithOutput(NewPassthroughOp(id, inputs).Output())
}

// User sets the user for this state.
// See [User] for more details.
func (s State) User(v string) State {
	return User(v)(s)
}

// Hostname sets the hostname for this state.
// See [Hostname] for more details.
func (s State) Hostname(v string) State {
	return Hostname(v)(s)
}

// GetHostname returns the hostname set on the state.
// See [Hostname] for more details.
func (s State) GetHostname(ctx context.Context, co ...ConstraintsOpt) (string, error) {
	c := &Constraints{}
	for _, f := range co {
		f.SetConstraintsOption(c)
	}
	return getHostname(s)(ctx, c)
}

// Platform sets the platform for the state. Platforms are used to determine
// image variants to pull and run as well as the platform metadata to set on the
// image config.
func (s State) Platform(p ocispecs.Platform) State {
	return platform(p)(s)
}

// GetPlatform returns the platform for the state.
func (s State) GetPlatform(ctx context.Context, co ...ConstraintsOpt) (*ocispecs.Platform, error) {
	c := &Constraints{}
	for _, f := range co {
		f.SetConstraintsOption(c)
	}
	return getPlatform(s)(ctx, c)
}

// Network sets the network mode for the state.
// Network modes are used by [State.Run] to determine the network mode used when running the container.
// Network modes are not applied to image configs.
func (s State) Network(n pb.NetMode) State {
	return Network(n)(s)
}

// GetNetwork returns the network mode for the state.
func (s State) GetNetwork(ctx context.Context, co ...ConstraintsOpt) (pb.NetMode, error) {
	c := &Constraints{}
	for _, f := range co {
		f.SetConstraintsOption(c)
	}
	return getNetwork(s)(ctx, c)
}

// Security sets the security mode for the state.
// Security modes are used by [State.Run] to the privileges that processes in the container will run with.
// Security modes are not applied to image configs.
func (s State) Security(n pb.SecurityMode) State {
	return Security(n)(s)
}

// GetSecurity returns the security mode for the state.
func (s State) GetSecurity(ctx context.Context, co ...ConstraintsOpt) (pb.SecurityMode, error) {
	c := &Constraints{}
	for _, f := range co {
		f.SetConstraintsOption(c)
	}
	return getSecurity(s)(ctx, c)
}

// With applies [StateOption]s to the [State].
// Each applied [StateOption] creates a new [State] object with the previous as its parent.
func (s State) With(so ...StateOption) State {
	for _, o := range so {
		s = o(s)
	}
	return s
}

// AddExtraHost adds a host name to IP mapping to any containers created from this state.
func (s State) AddExtraHost(host string, ip net.IP) State {
	return extraHost(host, ip)(s)
}

// AddUlimit sets the hard/soft for the given ulimit.
// The ulimit is applied to containers created from this state.
// Ulimits are Linux specific and only applies to containers created from this state such as via `[State.Run]`
// Ulimits do not apply to image configs.
func (s State) AddUlimit(name UlimitName, soft int64, hard int64) State {
	return ulimit(name, soft, hard)(s)
}

// WithCgroupParent sets the parent cgroup for any containers created from this state.
// This is useful when you want to apply resource constraints to a group of containers.
// Cgroups are Linux specific and only applies to containers created from this state such as via `[State.Run]`
// Cgroups do not apply to image configs.
func (s State) WithCgroupParent(cp string) State {
	return cgroupParent(cp)(s)
}

func (s State) isFileOpCopyInput() {}

type output struct {
	vertex   Vertex
	getIndex func() (pb.OutputIndex, error)
	err      error
	platform *ocispecs.Platform
}

func (o *output) ToInput(ctx context.Context, c *Constraints) (*pb.Input, error) {
	if o.err != nil {
		return nil, o.err
	}
	var index pb.OutputIndex
	if o.getIndex != nil {
		var err error
		index, err = o.getIndex()
		if err != nil {
			return nil, err
		}
	}
	dgst, _, _, _, err := o.vertex.Marshal(ctx, c)
	if err != nil {
		return nil, err
	}
	return &pb.Input{Digest: string(dgst), Index: int64(index)}, nil
}

func (o *output) Vertex(context.Context, *Constraints) Vertex {
	return o.vertex
}

func (o *output) Platform() *ocispecs.Platform {
	return o.platform
}

type ConstraintsOpt interface {
	SetConstraintsOption(*Constraints)
	RunOption
	LocalOption
	HTTPOption
	ImageBlobOption
	ImageOption
	GitOption
	OCILayoutOption
}

type constraintsOptFunc func(m *Constraints)

func (fn constraintsOptFunc) SetConstraintsOption(m *Constraints) {
	fn(m)
}

func (fn constraintsOptFunc) SetRunOption(ei *ExecInfo) {
	ei.applyConstraints(fn)
}

func (fn constraintsOptFunc) SetLocalOption(li *LocalInfo) {
	li.applyConstraints(fn)
}

func (fn constraintsOptFunc) SetOCILayoutOption(oi *OCILayoutInfo) {
	oi.applyConstraints(fn)
}

func (fn constraintsOptFunc) SetHTTPOption(hi *HTTPInfo) {
	hi.applyConstraints(fn)
}

func (fn constraintsOptFunc) SetImageBlobOption(ii *ImageBlobInfo) {
	ii.applyConstraints(fn)
}

func (fn constraintsOptFunc) SetImageOption(ii *ImageInfo) {
	ii.applyConstraints(fn)
}

func (fn constraintsOptFunc) SetGitOption(gi *GitInfo) {
	gi.applyConstraints(fn)
}

func mergeMetadata(m1, m2 OpMetadata) OpMetadata {
	if m2.IgnoreCache {
		m1.IgnoreCache = true
	}
	if len(m2.Description) > 0 {
		if m1.Description == nil {
			m1.Description = make(map[string]string)
		}
		maps.Copy(m1.Description, m2.Description)
	}
	if m2.ExportCache != nil {
		m1.ExportCache = m2.ExportCache
	}

	for k := range m2.Caps {
		if m1.Caps == nil {
			m1.Caps = make(map[apicaps.CapID]bool, len(m2.Caps))
		}
		m1.Caps[k] = true
	}

	if m2.ProgressGroup != nil {
		m1.ProgressGroup = m2.ProgressGroup
	}

	if m2.LinuxResources != nil {
		m1.LinuxResources = m2.LinuxResources
	}

	return m1
}

var IgnoreCache = constraintsOptFunc(func(c *Constraints) {
	c.Metadata.IgnoreCache = true
})

func WithDescription(m map[string]string) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		if c.Metadata.Description == nil {
			c.Metadata.Description = map[string]string{}
		}
		maps.Copy(c.Metadata.Description, m)
	})
}

func WithCustomName(name string) ConstraintsOpt {
	return WithDescription(map[string]string{
		"llb.customname": name,
	})
}

func WithCustomNamef(name string, a ...any) ConstraintsOpt {
	return WithCustomName(fmt.Sprintf(name, a...))
}

// WithExportCache forces results for this vertex to be exported with the cache
func WithExportCache() ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		c.Metadata.ExportCache = &pb.ExportCache{Value: true}
	})
}

// WithoutExportCache sets results for this vertex to be not exported with
// the cache
func WithoutExportCache() ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		// ExportCache with value false means to disable exporting
		c.Metadata.ExportCache = &pb.ExportCache{Value: false}
	})
}

// WithoutDefaultExportCache resets the cache export for the vertex to use
// the default defined by the build configuration.
func WithoutDefaultExportCache() ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		// nil means no vertex based config has been set
		c.Metadata.ExportCache = nil
	})
}

// WithCaps exposes supported LLB caps to the marshaler
func WithCaps(caps apicaps.CapSet) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		c.Caps = &caps
	})
}

type constraintsWrapper struct {
	Constraints
}

func (cw *constraintsWrapper) applyConstraints(f func(c *Constraints)) {
	f(&cw.Constraints)
}

type Constraints struct {
	Platform          *ocispecs.Platform
	WorkerConstraints []string
	Metadata          OpMetadata
	LocalUniqueID     string
	Caps              *apicaps.CapSet
	SourceLocations   []*SourceLocation
}

// OpMetadata has a more friendly interface for pb.OpMetadata.
type OpMetadata struct {
	IgnoreCache    bool                   `json:"ignore_cache,omitempty"`
	Description    map[string]string      `json:"description,omitempty"`
	ExportCache    *pb.ExportCache        `json:"export_cache,omitempty"`
	Caps           map[apicaps.CapID]bool `json:"caps,omitempty"`
	ProgressGroup  *pb.ProgressGroup      `json:"progress_group,omitempty"`
	LinuxResources *pb.LinuxResources     `json:"linux_resources,omitempty"`
}

func NewOpMetadata(mpb *pb.OpMetadata) OpMetadata {
	var m OpMetadata
	m.FromPB(mpb)
	return m
}

func (m OpMetadata) ToPB() *pb.OpMetadata {
	caps := make(map[string]bool, len(m.Caps))
	for k, v := range m.Caps {
		caps[string(k)] = v
	}
	return &pb.OpMetadata{
		IgnoreCache:    m.IgnoreCache,
		Description:    m.Description,
		ExportCache:    m.ExportCache,
		Caps:           caps,
		ProgressGroup:  m.ProgressGroup,
		LinuxResources: m.LinuxResources,
	}
}

func (m *OpMetadata) FromPB(mpb *pb.OpMetadata) {
	if mpb == nil {
		return
	}

	m.IgnoreCache = mpb.IgnoreCache
	m.Description = mpb.Description
	m.ExportCache = mpb.ExportCache
	if len(mpb.Caps) > 0 {
		m.Caps = make(map[apicaps.CapID]bool, len(mpb.Caps))
		for k, v := range mpb.Caps {
			m.Caps[apicaps.CapID(k)] = v
		}
	} else {
		m.Caps = nil
	}
	m.ProgressGroup = mpb.ProgressGroup
	m.LinuxResources = mpb.LinuxResources
}

func Platform(p ocispecs.Platform) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		c.Platform = &p
	})
}

func LocalUniqueID(v string) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		c.LocalUniqueID = v
	})
}

func ProgressGroup(id, name string, weak bool) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		c.Metadata.ProgressGroup = &pb.ProgressGroup{Id: id, Name: name, Weak: weak}
	})
}

// WithLinuxResources sets all CPU/memory resource limits at once.
// Resource limits are applied via OpMetadata and do not affect the cache key.
func WithLinuxResources(res LinuxResources) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		c.Metadata.LinuxResources = &pb.LinuxResources{
			Memory:     res.Memory,
			MemorySwap: res.MemorySwap,
			CpuShares:  res.CPUShares,
			CpuPeriod:  res.CPUPeriod,
			CpuQuota:   res.CPUQuota,
			CpusetCpus: res.CpusetCpus,
			CpusetMems: res.CpusetMems,
		}
	})
}

func ensureLinuxResources(c *Constraints) *pb.LinuxResources {
	if c.Metadata.LinuxResources == nil {
		c.Metadata.LinuxResources = &pb.LinuxResources{}
	}
	return c.Metadata.LinuxResources
}

func MemoryLimit(limit int64) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		ensureLinuxResources(c).Memory = limit
	})
}

func MemorySwapLimit(limit int64) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		ensureLinuxResources(c).MemorySwap = limit
	})
}

func CPUShares(shares uint64) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		ensureLinuxResources(c).CpuShares = shares
	})
}

func CPUPeriod(period uint64) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		ensureLinuxResources(c).CpuPeriod = period
	})
}

func CPUQuota(quota int64) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		ensureLinuxResources(c).CpuQuota = quota
	})
}

func CpusetCpus(cpus string) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		ensureLinuxResources(c).CpusetCpus = cpus
	})
}

func CpusetMems(mems string) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		ensureLinuxResources(c).CpusetMems = mems
	})
}

var (
	LinuxAmd64   = Platform(ocispecs.Platform{OS: "linux", Architecture: "amd64"})
	LinuxArmhf   = Platform(ocispecs.Platform{OS: "linux", Architecture: "arm", Variant: "v7"})
	LinuxArm     = LinuxArmhf
	LinuxArmel   = Platform(ocispecs.Platform{OS: "linux", Architecture: "arm", Variant: "v6"})
	LinuxArm64   = Platform(ocispecs.Platform{OS: "linux", Architecture: "arm64"})
	LinuxS390x   = Platform(ocispecs.Platform{OS: "linux", Architecture: "s390x"})
	LinuxPpc64   = Platform(ocispecs.Platform{OS: "linux", Architecture: "ppc64"})
	LinuxPpc64le = Platform(ocispecs.Platform{OS: "linux", Architecture: "ppc64le"})
	LinuxRiscv64 = Platform(ocispecs.Platform{OS: "linux", Architecture: "riscv64"})
	Darwin       = Platform(ocispecs.Platform{OS: "darwin", Architecture: "amd64"})
	Windows      = Platform(ocispecs.Platform{OS: "windows", Architecture: "amd64"})
)

func Require(filters ...string) ConstraintsOpt {
	return constraintsOptFunc(func(c *Constraints) {
		c.WorkerConstraints = append(c.WorkerConstraints, filters...)
	})
}

func nilValue(context.Context, *Constraints) (any, error) {
	return nil, nil
}
