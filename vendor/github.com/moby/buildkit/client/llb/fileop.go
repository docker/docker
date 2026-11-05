package llb

import (
	"context"
	_ "crypto/sha256" // for opencontainers/go-digest
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/moby/buildkit/solver/pb"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

// Examples:
// local := llb.Local(...)
// llb.Image().Dir("/abc").File(Mkdir("./foo").Mkfile("/abc/foo/bar", []byte("data")))
// llb.Image().File(Mkdir("/foo").Mkfile("/foo/bar", []byte("data")))
// llb.Image().File(Copy(local, "/foo", "/bar")).File(Copy(local, "/foo2", "/bar2"))
//
// a := Mkdir("./foo")  // *FileAction /ced/foo
// b := Mkdir("./bar") // /abc/bar
// c := b.Copy(a.WithState(llb.Scratch().Dir("/ced")), "./foo", "./baz") // /abc/baz
// llb.Image().Dir("/abc").File(c)
//
// In future this can be extended to multiple outputs with:
// a := Mkdir("./foo")
// b, id := a.GetSelector()
// c := b.Mkdir("./bar")
// filestate = state.File(c)
// filestate.GetOutput(id).Exec()

func NewFileOp(s State, action *FileAction, c Constraints) *FileOp {
	action = action.bind(s)

	f := &FileOp{
		action:      action,
		constraints: c,
	}

	f.output = &output{vertex: f, getIndex: func() (pb.OutputIndex, error) {
		return pb.OutputIndex(0), nil
	}}

	return f
}

// CopyInput is either llb.State or *FileActionWithState
// It is used by [Copy] to specify the source of the copy operation.
type CopyInput interface {
	isFileOpCopyInput()
}

type subAction interface {
	toProtoAction(context.Context, string, pb.InputIndex) (pb.IsFileAction, error)
}

type capAdder interface {
	addCaps(*FileOp)
}

// FileAction is used to specify a file operation on a [State].
// It can be used to create a directory, create a file, or remove a file, etc.
// This is used by [State.File]
// Typically a FileAction is created by calling one of the helper functions such as [Mkdir], [Copy], [Rm], [Mkfile]
type FileAction struct {
	state  *State
	prev   *FileAction
	action subAction
	err    error
}

func (fa *FileAction) Mkdir(p string, m os.FileMode, opt ...MkdirOption) *FileAction {
	a := Mkdir(p, m, opt...)
	a.prev = fa
	return a
}

func (fa *FileAction) Mkfile(p string, m os.FileMode, dt []byte, opt ...MkfileOption) *FileAction {
	a := Mkfile(p, m, dt, opt...)
	a.prev = fa
	return a
}

// Symlink creates a symlink at `newpath` that points to `oldpath`
func (fa *FileAction) Symlink(oldpath, newpath string, opt ...SymlinkOption) *FileAction {
	a := Symlink(oldpath, newpath, opt...)
	a.prev = fa
	return a
}

func (fa *FileAction) Rm(p string, opt ...RmOption) *FileAction {
	a := Rm(p, opt...)
	a.prev = fa
	return a
}

func (fa *FileAction) Copy(input CopyInput, src, dest string, opt ...CopyOption) *FileAction {
	a := Copy(input, src, dest, opt...)
	a.prev = fa
	return a
}

func (fa *FileAction) allOutputs(seen map[Output]struct{}, outputs []Output) []Output {
	if fa == nil {
		return outputs
	}

	if fa.state != nil {
		out := fa.state.Output()
		if out != nil {
			if _, ok := seen[out]; !ok {
				outputs = append(outputs, out)
				seen[out] = struct{}{}
			}
		}
	}

	if a, ok := fa.action.(*fileActionCopy); ok {
		if a.state != nil {
			out := a.state.Output()
			if out != nil {
				if _, ok := seen[out]; !ok {
					outputs = append(outputs, out)
					seen[out] = struct{}{}
				}
			}
		} else if a.fas != nil {
			outputs = a.fas.allOutputs(seen, outputs)
		}
	}
	return fa.prev.allOutputs(seen, outputs)
}

func (fa *FileAction) bind(s State) *FileAction {
	if fa == nil {
		return nil
	}
	fa2 := *fa
	fa2.prev = fa.prev.bind(s)
	fa2.state = &s
	return &fa2
}

func (fa *FileAction) WithState(s State) CopyInput {
	return &fileActionWithState{FileAction: fa.bind(s)}
}

type fileActionWithState struct {
	*FileAction
}

func (fas *fileActionWithState) isFileOpCopyInput() {}

// Mkdir creates a FileAction which creates a directory at the given path.
// Example:
//
//	llb.Scratch().File(llb.Mkdir("/foo", 0755))
func Mkdir(p string, m os.FileMode, opt ...MkdirOption) *FileAction {
	var mi MkdirInfo
	for _, o := range opt {
		o.SetMkdirOption(&mi)
	}

	return &FileAction{
		action: &fileActionMkdir{
			file: p,
			mode: m,
			info: mi,
		},
	}
}

type fileActionMkdir struct {
	file string
	mode os.FileMode
	info MkdirInfo
}

func (a *fileActionMkdir) toProtoAction(ctx context.Context, parent string, base pb.InputIndex) (pb.IsFileAction, error) {
	return &pb.FileAction_Mkdir{
		Mkdir: &pb.FileActionMkDir{
			Path:        normalizePath(parent, a.file, false),
			Mode:        int32(a.mode & 0777),
			MakeParents: a.info.MakeParents,
			Owner:       a.info.ChownOpt.marshal(base),
			Timestamp:   marshalTime(a.info.CreatedTime),
		},
	}, nil
}

type MkdirOption interface {
	SetMkdirOption(*MkdirInfo)
}

type ChownOption interface {
	MkdirOption
	MkfileOption
	CopyOption
	SymlinkOption
}

type mkdirOptionFunc func(*MkdirInfo)

func (fn mkdirOptionFunc) SetMkdirOption(mi *MkdirInfo) {
	fn(mi)
}

var _ MkdirOption = &MkdirInfo{}

// WithParents is an option for Mkdir which creates parent directories if they do not exist.
func WithParents(b bool) MkdirOption {
	return mkdirOptionFunc(func(mi *MkdirInfo) {
		mi.MakeParents = b
	})
}

type MkdirInfo struct {
	MakeParents bool
	ChownOpt    *ChownOpt
	CreatedTime *time.Time
}

func (mi *MkdirInfo) SetMkdirOption(mi2 *MkdirInfo) {
	*mi2 = *mi
}

func WithUser(name string) ChownOption {
	opt := ChownOpt{}

	parts := strings.SplitN(name, ":", 2)
	for i, v := range parts {
		switch i {
		case 0:
			uid, err := parseUID(v)
			if err != nil {
				opt.User = &UserOpt{Name: v}
			} else {
				opt.User = &UserOpt{UID: uid}
			}
		case 1:
			// An omitted group uses the same default as a user without a colon.
			if v == "" {
				continue
			}
			gid, err := parseUID(v)
			if err != nil {
				opt.Group = &UserOpt{Name: v}
			} else {
				opt.Group = &UserOpt{UID: gid}
			}
		}
	}

	return opt
}

func parseUID(str string) (int, error) {
	if str == "root" {
		return 0, nil
	}
	// uid_t/gid_t span 0..2^32-1, matching the daemon-side parser in
	// executor/oci. ParseInt here would reject the upper half of that range and
	// silently turn a numeric ID into a name lookup.
	uid, err := strconv.ParseUint(str, 10, 32)
	if err != nil {
		return 0, err
	}
	return int(uid), nil
}

func WithUIDGID(uid, gid int) ChownOption {
	return ChownOpt{
		User:  &UserOpt{UID: uid},
		Group: &UserOpt{UID: gid},
	}
}

type ChmodOpt struct {
	Mode    os.FileMode
	ModeStr string
}

func (co ChmodOpt) SetCopyOption(mi *CopyInfo) {
	mi.Mode = &co
}

type ChownOpt struct {
	User  *UserOpt
	Group *UserOpt
}

func (co ChownOpt) SetMkdirOption(mi *MkdirInfo) {
	mi.ChownOpt = &co
}

func (co ChownOpt) SetMkfileOption(mi *MkfileInfo) {
	mi.ChownOpt = &co
}

func (co ChownOpt) SetCopyOption(mi *CopyInfo) {
	mi.ChownOpt = &co
}

func (co ChownOpt) SetSymlinkOption(si *SymlinkInfo) {
	si.ChownOpt = &co
}

func (co *ChownOpt) marshal(base pb.InputIndex) *pb.ChownOpt {
	if co == nil {
		return nil
	}
	return &pb.ChownOpt{
		User:  co.User.marshal(base),
		Group: co.Group.marshal(base),
	}
}

type UserOpt struct {
	UID  int
	Name string
}

func (up *UserOpt) marshal(base pb.InputIndex) *pb.UserOpt {
	if up == nil {
		return nil
	}
	if up.Name != "" {
		return &pb.UserOpt{User: &pb.UserOpt_ByName{ByName: &pb.NamedUserOpt{
			Name: up.Name, Input: int64(base),
		}}}
	}
	return &pb.UserOpt{User: &pb.UserOpt_ByID{ByID: uint32(up.UID)}}
}

// Mkfile creates a FileAction which creates a file at the given path with the provided contents.
// Example:
//
//	llb.Scratch().File(llb.Mkfile("/foo", 0644, []byte("hello world!")))
func Mkfile(p string, m os.FileMode, dt []byte, opts ...MkfileOption) *FileAction {
	var mi MkfileInfo
	for _, o := range opts {
		o.SetMkfileOption(&mi)
	}

	return &FileAction{
		action: &fileActionMkfile{
			file: p,
			mode: m,
			dt:   dt,
			info: mi,
		},
	}
}

// SymlinkInfo is the modifiable options used to create symlinks
type SymlinkInfo struct {
	ChownOpt    *ChownOpt
	CreatedTime *time.Time
}

func (si *SymlinkInfo) SetSymlinkOption(si2 *SymlinkInfo) {
	*si2 = *si
}

type SymlinkOption interface {
	SetSymlinkOption(*SymlinkInfo)
}

// Symlink creates a symlink at `newpath` that points to `oldpath`
func Symlink(oldpath, newpath string, opts ...SymlinkOption) *FileAction {
	var si SymlinkInfo
	for _, o := range opts {
		o.SetSymlinkOption(&si)
	}

	return &FileAction{
		action: &fileActionSymlink{
			oldpath: oldpath,
			newpath: newpath,
			info:    si,
		},
	}
}

type fileActionSymlink struct {
	oldpath string
	newpath string
	info    SymlinkInfo
}

func (s *fileActionSymlink) addCaps(f *FileOp) {
	addCap(&f.constraints, pb.CapFileSymlinkCreate)
}

func (s *fileActionSymlink) toProtoAction(_ context.Context, _ string, base pb.InputIndex) (pb.IsFileAction, error) {
	return &pb.FileAction_Symlink{
		Symlink: &pb.FileActionSymlink{
			Oldpath:   s.oldpath,
			Newpath:   s.newpath,
			Owner:     s.info.ChownOpt.marshal(base),
			Timestamp: marshalTime(s.info.CreatedTime),
		},
	}, nil
}

type MkfileOption interface {
	SetMkfileOption(*MkfileInfo)
}

type MkfileInfo struct {
	ChownOpt    *ChownOpt
	CreatedTime *time.Time
}

func (mi *MkfileInfo) SetMkfileOption(mi2 *MkfileInfo) {
	*mi2 = *mi
}

var _ MkfileOption = &MkfileInfo{}

type fileActionMkfile struct {
	file string
	mode os.FileMode
	dt   []byte
	info MkfileInfo
}

func (a *fileActionMkfile) toProtoAction(ctx context.Context, parent string, base pb.InputIndex) (pb.IsFileAction, error) {
	return &pb.FileAction_Mkfile{
		Mkfile: &pb.FileActionMkFile{
			Path:      normalizePath(parent, a.file, false),
			Mode:      int32(a.mode & 0777),
			Data:      a.dt,
			Owner:     a.info.ChownOpt.marshal(base),
			Timestamp: marshalTime(a.info.CreatedTime),
		},
	}, nil
}

// Rm creates a FileAction which removes a file or directory at the given path.
// Example:
//
//	llb.Scratch().File(Mkfile("/foo", 0644, []byte("not around for long..."))).File(llb.Rm("/foo"))
func Rm(p string, opts ...RmOption) *FileAction {
	var mi RmInfo
	for _, o := range opts {
		o.SetRmOption(&mi)
	}

	return &FileAction{
		action: &fileActionRm{
			file: p,
			info: mi,
		},
	}
}

type RmOption interface {
	SetRmOption(*RmInfo)
}

type rmOptionFunc func(*RmInfo)

func (fn rmOptionFunc) SetRmOption(mi *RmInfo) {
	fn(mi)
}

type RmInfo struct {
	AllowNotFound bool
	AllowWildcard bool
}

func (mi *RmInfo) SetRmOption(mi2 *RmInfo) {
	*mi2 = *mi
}

var _ RmOption = &RmInfo{}

func WithAllowNotFound(b bool) RmOption {
	return rmOptionFunc(func(mi *RmInfo) {
		mi.AllowNotFound = b
	})
}

func WithAllowWildcard(b bool) RmOption {
	return rmOptionFunc(func(mi *RmInfo) {
		mi.AllowWildcard = b
	})
}

type excludeOnCopyAction struct {
	patterns []string
}

func (e *excludeOnCopyAction) SetCopyOption(i *CopyInfo) {
	i.ExcludePatterns = append(i.ExcludePatterns, e.patterns...)
}

func WithExcludePatterns(patterns []string) CopyOption {
	return &excludeOnCopyAction{patterns}
}

type fileActionRm struct {
	file string
	info RmInfo
}

func (a *fileActionRm) toProtoAction(ctx context.Context, parent string, base pb.InputIndex) (pb.IsFileAction, error) {
	return &pb.FileAction_Rm{
		Rm: &pb.FileActionRm{
			Path:          normalizePath(parent, a.file, false),
			AllowNotFound: a.info.AllowNotFound,
			AllowWildcard: a.info.AllowWildcard,
		},
	}, nil
}

// Copy produces a FileAction which copies a file or directory from the source to the destination.
// The "input" parameter is the contents to copy from.
// "src" is the path to copy from within the "input".
// "dest" is the path to copy to within the destination (the state being operated on).
// See [CopyInput] for the valid types of input.
//
// Example:
//
//	st := llb.Local(".")
//	llb.Scratch().File(llb.Copy(st, "/foo", "/bar"))
//
// The example copies the local (client) directory "./foo" to a new empty directory at /bar.
//
// Note: Copying directories can have different behavior based on if the destination exists or not.
// When the destination already exists, the contents of the source directory is copied underneath the destination, including the directory itself.
// You may need to supply a copy option to copy the dir contents only.
// You may also need to pass in a [CopyOption] which creates parent directories if they do not exist.
//
// See [CopyOption] for more details on what options are available.
func Copy(input CopyInput, src, dest string, opts ...CopyOption) *FileAction {
	var state *State
	var fas *fileActionWithState
	var err error
	if st, ok := input.(State); ok {
		state = &st
	} else if v, ok := input.(*fileActionWithState); ok {
		fas = v
	} else {
		err = errors.Errorf("invalid input type %T for copy", input)
	}

	var mi CopyInfo
	for _, o := range opts {
		o.SetCopyOption(&mi)
	}
	return &FileAction{
		action: &fileActionCopy{
			state: state,
			fas:   fas,
			src:   src,
			dest:  dest,
			info:  mi,
		},
		err: err,
	}
}

type CopyOption interface {
	SetCopyOption(*CopyInfo)
}

type CopyInfo struct {
	Mode                           *ChmodOpt
	FollowSymlinks                 bool
	CopyDirContentsOnly            bool
	IncludePatterns                []string
	ExcludePatterns                []string
	RequiredPaths                  []string
	AttemptUnpack                  bool
	CreateDestPath                 bool
	AllowWildcard                  bool
	AllowEmptyWildcard             bool
	ChownOpt                       *ChownOpt
	CreatedTime                    *time.Time
	AlwaysReplaceExistingDestPaths bool
}

func (mi *CopyInfo) SetCopyOption(mi2 *CopyInfo) {
	*mi2 = *mi
}

var _ CopyOption = &CopyInfo{}

type fileActionCopy struct {
	state *State
	fas   *fileActionWithState
	src   string
	dest  string
	info  CopyInfo
}

func (a *fileActionCopy) toProtoAction(ctx context.Context, parent string, base pb.InputIndex) (pb.IsFileAction, error) {
	src, err := a.sourcePath(ctx)
	if err != nil {
		return nil, err
	}
	c := &pb.FileActionCopy{
		Src:                              src,
		Dest:                             normalizePath(parent, a.dest, true),
		Owner:                            a.info.ChownOpt.marshal(base),
		IncludePatterns:                  a.info.IncludePatterns,
		ExcludePatterns:                  a.info.ExcludePatterns,
		RequiredPaths:                    a.info.RequiredPaths,
		AllowWildcard:                    a.info.AllowWildcard,
		AllowEmptyWildcard:               a.info.AllowEmptyWildcard,
		FollowSymlink:                    a.info.FollowSymlinks,
		DirCopyContents:                  a.info.CopyDirContentsOnly,
		AttemptUnpackDockerCompatibility: a.info.AttemptUnpack,
		CreateDestPath:                   a.info.CreateDestPath,
		Timestamp:                        marshalTime(a.info.CreatedTime),
		AlwaysReplaceExistingDestPaths:   a.info.AlwaysReplaceExistingDestPaths,
	}
	if a.info.Mode != nil {
		if a.info.Mode.ModeStr != "" {
			c.ModeStr = a.info.Mode.ModeStr
		} else {
			c.Mode = int32(a.info.Mode.Mode)
		}
	} else {
		c.Mode = -1
	}
	return &pb.FileAction_Copy{
		Copy: c,
	}, nil
}

func (a *fileActionCopy) sourcePath(ctx context.Context) (string, error) {
	p := filepath.ToSlash(path.Clean(a.src))
	dir := "/"
	var err error
	if !path.IsAbs(p) {
		if a.state != nil {
			dir, err = a.state.GetDir(ctx)
		} else if a.fas != nil {
			dir, err = a.fas.state.GetDir(ctx)
		}
		if err != nil {
			return "", err
		}
	}
	return path.Join(dir, p), nil
}

func (a *fileActionCopy) addCaps(f *FileOp) {
	if len(a.info.IncludePatterns) != 0 || len(a.info.ExcludePatterns) != 0 {
		addCap(&f.constraints, pb.CapFileCopyIncludeExcludePatterns)
	}
	if len(a.info.RequiredPaths) != 0 {
		addCap(&f.constraints, pb.CapFileCopyRequiredPaths)
	}
	if a.info.AlwaysReplaceExistingDestPaths {
		addCap(&f.constraints, pb.CapFileCopyAlwaysReplaceExistingDestPaths)
	}
	if a.info.Mode.ModeStr != "" {
		addCap(&f.constraints, pb.CapFileCopyModeStringFormat)
	}
}

type CreatedTime time.Time

func WithCreatedTime(t time.Time) CreatedTime {
	return CreatedTime(t)
}

func (c CreatedTime) SetMkdirOption(mi *MkdirInfo) {
	mi.CreatedTime = (*time.Time)(&c)
}

func (c CreatedTime) SetMkfileOption(mi *MkfileInfo) {
	mi.CreatedTime = (*time.Time)(&c)
}

func (c CreatedTime) SetSymlinkOption(si *SymlinkInfo) {
	si.CreatedTime = (*time.Time)(&c)
}

func (c CreatedTime) SetCopyOption(mi *CopyInfo) {
	mi.CreatedTime = (*time.Time)(&c)
}

func marshalTime(t *time.Time) int64 {
	if t == nil {
		return -1
	}
	return t.UnixNano()
}

type FileOp struct {
	MarshalCache
	action *FileAction
	output Output

	constraints Constraints
	isValidated bool
}

func (f *FileOp) Validate(context.Context, *Constraints) error {
	if f.isValidated {
		return nil
	}
	if f.action == nil {
		return errors.New("action is required")
	}
	f.isValidated = true
	return nil
}

type marshalState struct {
	ctx     context.Context
	visited map[*FileAction]*fileActionState
	inputs  []*pb.Input
	actions []*fileActionState
}

func newMarshalState(ctx context.Context) *marshalState {
	return &marshalState{
		visited: map[*FileAction]*fileActionState{},
		ctx:     ctx,
	}
}

type fileActionState struct {
	base           pb.InputIndex
	input          pb.InputIndex
	inputRelative  *int
	input2         pb.InputIndex
	input2Relative *int
	target         int
	action         subAction
	fa             *FileAction
}

func (ms *marshalState) addInput(c *Constraints, o Output) (pb.InputIndex, error) {
	inp, err := o.ToInput(ms.ctx, c)
	if err != nil {
		return 0, err
	}
	for i, inp2 := range ms.inputs {
		if inp.EqualVT(inp2) {
			return pb.InputIndex(i), nil
		}
	}
	i := pb.InputIndex(len(ms.inputs))
	ms.inputs = append(ms.inputs, inp)
	return i, nil
}

func (ms *marshalState) add(fa *FileAction, c *Constraints) (*fileActionState, error) {
	if st, ok := ms.visited[fa]; ok {
		return st, nil
	}

	if fa.err != nil {
		return nil, fa.err
	}

	var prevState *fileActionState
	if parent := fa.prev; parent != nil {
		var err error
		prevState, err = ms.add(parent, c)
		if err != nil {
			return nil, err
		}
	}

	st := &fileActionState{
		action: fa.action,
		input:  -1,
		input2: -1,
		base:   -1,
		fa:     fa,
	}

	if source := fa.state.Output(); source != nil {
		inp, err := ms.addInput(c, source)
		if err != nil {
			return nil, err
		}
		st.base = inp
	}

	if fa.prev == nil {
		st.input = st.base
	} else {
		st.inputRelative = &prevState.target
	}

	if a, ok := fa.action.(*fileActionCopy); ok {
		if a.state != nil {
			if out := a.state.Output(); out != nil {
				inp, err := ms.addInput(c, out)
				if err != nil {
					return nil, err
				}
				st.input2 = inp
			}
		} else if a.fas != nil {
			src, err := ms.add(a.fas.FileAction, c)
			if err != nil {
				return nil, err
			}
			st.input2Relative = &src.target
		} else {
			return nil, errors.New("invalid empty source for copy")
		}
	}

	st.target = len(ms.actions)

	ms.visited[fa] = st
	ms.actions = append(ms.actions, st)

	return st, nil
}

func (f *FileOp) Marshal(ctx context.Context, c *Constraints) (digest.Digest, []byte, *pb.OpMetadata, []*SourceLocation, error) {
	cache := f.Acquire()
	defer cache.Release()

	if dgst, dt, md, srcs, err := cache.Load(c); err == nil {
		return dgst, dt, md, srcs, nil
	}

	if err := f.Validate(ctx, c); err != nil {
		return "", nil, nil, nil, err
	}

	addCap(&f.constraints, pb.CapFileBase)

	pfo := &pb.FileOp{}

	if f.constraints.Platform == nil {
		p, err := getPlatform(*f.action.state)(ctx, c)
		if err != nil {
			return "", nil, nil, nil, err
		}
		f.constraints.Platform = p
	}

	state := newMarshalState(ctx)
	for _, st := range state.actions {
		if adder, isCapAdder := st.action.(capAdder); isCapAdder {
			adder.addCaps(f)
		}
	}

	pop, md := MarshalConstraints(c, &f.constraints)
	pop.Platform = nil // file op is not platform specific
	pop.Op = &pb.Op_File{
		File: pfo,
	}

	_, err := state.add(f.action, c)
	if err != nil {
		return "", nil, nil, nil, err
	}
	pop.Inputs = state.inputs

	for i, st := range state.actions {
		output := pb.SkipOutput
		if i+1 == len(state.actions) {
			output = 0
		}

		var parent string
		if st.fa.state != nil {
			parent, err = st.fa.state.GetDir(ctx)
			if err != nil {
				return "", nil, nil, nil, err
			}
		}

		action, err := st.action.toProtoAction(ctx, parent, st.base)
		if err != nil {
			return "", nil, nil, nil, err
		}

		pfo.Actions = append(pfo.Actions, &pb.FileAction{
			Input:          getIndex(st.input, len(state.inputs), st.inputRelative),
			SecondaryInput: getIndex(st.input2, len(state.inputs), st.input2Relative),
			Output:         int64(output),
			Action:         action,
		})
	}

	dt, err := deterministicMarshal(pop)
	if err != nil {
		return "", nil, nil, nil, err
	}
	return cache.Store(dt, md, f.constraints.SourceLocations, c)
}

func normalizePath(parent, p string, keepSlash bool) string {
	origPath := p
	p = path.Clean(p)
	if !path.IsAbs(p) {
		p = path.Join("/", parent, p)
	}
	if keepSlash {
		if strings.HasSuffix(origPath, "/") && !strings.HasSuffix(p, "/") {
			p += "/"
		} else if strings.HasSuffix(origPath, "/.") {
			if p != "/" {
				p += "/"
			}
			p += "."
		}
	}
	return p
}

func (f *FileOp) Output() Output {
	return f.output
}

func (f *FileOp) Inputs() []Output {
	return f.action.allOutputs(map[Output]struct{}{}, []Output{})
}

func getIndex(input pb.InputIndex, len int, relative *int) int64 {
	if relative != nil {
		return int64(len + *relative)
	}
	return int64(input)
}
