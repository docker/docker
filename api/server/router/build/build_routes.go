package build // import "github.com/docker/docker/api/server/router/build"

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/system"
	units "github.com/docker/go-units"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type invalidIsolationError string

func (e invalidIsolationError) Error() string {
	return fmt.Sprintf("Unsupported isolation: %q", string(e))
}

func (e invalidIsolationError) InvalidParameter() {}

func newImageBuildOptions(ctx context.Context, r *http.Request) (*types.ImageBuildOptions, error) {
	options := &types.ImageBuildOptions{
		Version:        types.BuilderV1, // Builder V1 is the default, but can be overridden
		Dockerfile:     r.FormValue("dockerfile"),
		SuppressOutput: httputils.BoolValue(r, "q"),
		NoCache:        httputils.BoolValue(r, "nocache"),
		ForceRemove:    httputils.BoolValue(r, "forcerm"),
		MemorySwap:     httputils.Int64ValueOrZero(r, "memswap"),
		Memory:         httputils.Int64ValueOrZero(r, "memory"),
		CPUShares:      httputils.Int64ValueOrZero(r, "cpushares"),
		CPUPeriod:      httputils.Int64ValueOrZero(r, "cpuperiod"),
		CPUQuota:       httputils.Int64ValueOrZero(r, "cpuquota"),
		CPUSetCPUs:     r.FormValue("cpusetcpus"),
		CPUSetMems:     r.FormValue("cpusetmems"),
		CgroupParent:   r.FormValue("cgroupparent"),
		NetworkMode:    r.FormValue("networkmode"),
		Tags:           r.Form["t"],
		ExtraHosts:     r.Form["extrahosts"],
		SecurityOpt:    r.Form["securityopt"],
		Squash:         httputils.BoolValue(r, "squash"),
		Target:         r.FormValue("target"),
		RemoteContext:  r.FormValue("remote"),
		SessionID:      r.FormValue("session"),
		BuildID:        r.FormValue("buildid"),
	}

	if runtime.GOOS != "windows" && options.SecurityOpt != nil {
		return nil, errdefs.InvalidParameter(errors.New("The daemon on this platform does not support setting security options on build"))
	}

	version := httputils.VersionFromContext(ctx)
	if httputils.BoolValue(r, "forcerm") && versions.GreaterThanOrEqualTo(version, "1.12") {
		options.Remove = true
	} else if r.FormValue("rm") == "" && versions.GreaterThanOrEqualTo(version, "1.12") {
		options.Remove = true
	} else {
		options.Remove = httputils.BoolValue(r, "rm")
	}
	if httputils.BoolValue(r, "pull") && versions.GreaterThanOrEqualTo(version, "1.16") {
		options.PullParent = true
	}
	if versions.GreaterThanOrEqualTo(version, "1.32") {
		apiPlatform := r.FormValue("platform")
		if apiPlatform != "" {
			sp, err := platforms.Parse(apiPlatform)
			if err != nil {
				return nil, err
			}

			if err := system.ValidatePlatform(sp); err != nil {
				return nil, err
			}
			options.Platform = &sp
		}
	}
	if versions.GreaterThanOrEqualTo(version, "1.40") {
		outputsJSON := r.FormValue("outputs")
		if outputsJSON != "" {
			var outputs []types.ImageBuildOutput
			if err := json.Unmarshal([]byte(outputsJSON), &outputs); err != nil {
				return nil, err
			}
			options.Outputs = outputs
		}
	}

	if s := r.Form.Get("shmsize"); s != "" {
		shmSize, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, err
		}
		options.ShmSize = shmSize
	}

	if i := r.FormValue("isolation"); i != "" {
		options.Isolation = container.Isolation(i)
		if !options.Isolation.IsValid() {
			return nil, invalidIsolationError(options.Isolation)
		}
	}

	if ulimitsJSON := r.FormValue("ulimits"); ulimitsJSON != "" {
		var buildUlimits = []*units.Ulimit{}
		if err := json.Unmarshal([]byte(ulimitsJSON), &buildUlimits); err != nil {
			return nil, errors.Wrap(errdefs.InvalidParameter(err), "error reading ulimit settings")
		}
		options.Ulimits = buildUlimits
	}

	// Note that there are two ways a --build-arg might appear in the
	// json of the query param:
	//     "foo":"bar"
	// and "foo":nil
	// The first is the normal case, ie. --build-arg foo=bar
	// or  --build-arg foo
	// where foo's value was picked up from an env var.
	// The second ("foo":nil) is where they put --build-arg foo
	// but "foo" isn't set as an env var. In that case we can't just drop
	// the fact they mentioned it, we need to pass that along to the builder
	// so that it can print a warning about "foo" being unused if there is
	// no "ARG foo" in the Dockerfile.
	if buildArgsJSON := r.FormValue("buildargs"); buildArgsJSON != "" {
		var buildArgs = map[string]*string{}
		if err := json.Unmarshal([]byte(buildArgsJSON), &buildArgs); err != nil {
			return nil, errors.Wrap(errdefs.InvalidParameter(err), "error reading build args")
		}
		options.BuildArgs = buildArgs
	}

	if labelsJSON := r.FormValue("labels"); labelsJSON != "" {
		var labels = map[string]string{}
		if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
			return nil, errors.Wrap(errdefs.InvalidParameter(err), "error reading labels")
		}
		options.Labels = labels
	}

	if cacheFromJSON := r.FormValue("cachefrom"); cacheFromJSON != "" {
		var cacheFrom = []string{}
		if err := json.Unmarshal([]byte(cacheFromJSON), &cacheFrom); err != nil {
			return nil, err
		}
		options.CacheFrom = cacheFrom
	}

	if bv := r.FormValue("version"); bv != "" {
		v, err := parseVersion(bv)
		if err != nil {
			return nil, err
		}
		options.Version = v
	}

	return options, nil
}

func parseVersion(s string) (types.BuilderVersion, error) {
	switch types.BuilderVersion(s) {
	case types.BuilderV1:
		return types.BuilderV1, nil
	case types.BuilderBuildKit:
		return types.BuilderBuildKit, nil
	default:
		return "", errors.Errorf("invalid version %q", s)
	}
}

func (br *buildRouter) postPrune(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	fltrs, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return errors.Wrap(err, "could not parse filters")
	}
	ksfv := r.FormValue("keep-storage")
	if ksfv == "" {
		ksfv = "0"
	}
	ks, err := strconv.Atoi(ksfv)
	if err != nil {
		return errors.Wrapf(err, "keep-storage is in bytes and expects an integer, got %v", ksfv)
	}

	opts := types.BuildCachePruneOptions{
		All:         httputils.BoolValue(r, "all"),
		Filters:     fltrs,
		KeepStorage: int64(ks),
	}

	report, err := br.backend.PruneCache(ctx, opts)
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, report)
}

func (br *buildRouter) postCancel(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	w.Header().Set("Content-Type", "application/json")

	id := r.FormValue("id")
	if id == "" {
		return errors.Errorf("build ID not provided")
	}

	return br.backend.Cancel(ctx, id)
}

func (br *buildRouter) postBuild(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	var (
		notVerboseBuffer = bytes.NewBuffer(nil)
		version          = httputils.VersionFromContext(ctx)
	)

	w.Header().Set("Content-Type", "application/json")

	body := r.Body
	var ww io.Writer = w
	if body != nil {
		// there is a possibility that output is written before request body
		// has been fully read so we need to protect against it.
		// this can be removed when
		// https://github.com/golang/go/issues/15527
		// https://github.com/golang/go/issues/22209
		// has been fixed
		body, ww = wrapOutputBufferedUntilRequestRead(body, ww)
	}

	output := ioutils.NewWriteFlusher(ww)
	defer func() { _ = output.Close() }()

	errf := func(err error) error {

		if httputils.BoolValue(r, "q") && notVerboseBuffer.Len() > 0 {
			_, _ = output.Write(notVerboseBuffer.Bytes())
		}

		// Do not write the error in the http output if it's still empty.
		// This prevents from writing a 200(OK) when there is an internal error.
		if !output.Flushed() {
			return err
		}
		_, err = output.Write(streamformatter.FormatError(err))
		if err != nil {
			logrus.Warnf("could not write error response: %v", err)
		}
		return nil
	}

	buildOptions, err := newImageBuildOptions(ctx, r)
	if err != nil {
		return errf(err)
	}
	buildOptions.AuthConfigs = getAuthConfigs(r.Header)

	if buildOptions.Squash && !br.daemon.HasExperimental() {
		return errdefs.InvalidParameter(errors.New("squash is only supported with experimental mode"))
	}

	out := io.Writer(output)
	if buildOptions.SuppressOutput {
		out = notVerboseBuffer
	}

	// Currently, only used if context is from a remote url.
	// Look at code in DetectContextFromRemoteURL for more information.
	createProgressReader := func(in io.ReadCloser) io.ReadCloser {
		progressOutput := streamformatter.NewJSONProgressOutput(out, true)
		return progress.NewProgressReader(in, progressOutput, r.ContentLength, "Downloading context", buildOptions.RemoteContext)
	}

	wantAux := versions.GreaterThanOrEqualTo(version, "1.30")

	imgID, err := br.backend.Build(ctx, backend.BuildConfig{
		Source:         body,
		Options:        buildOptions,
		ProgressWriter: buildProgressWriter(out, wantAux, createProgressReader),
	})
	if err != nil {
		return errf(err)
	}

	// Everything worked so if -q was provided the output from the daemon
	// should be just the image ID and we'll print that to stdout.
	if buildOptions.SuppressOutput {
		_, _ = fmt.Fprintln(streamformatter.NewStdoutWriter(output), imgID)
	}
	return nil
}

func getAuthConfigs(header http.Header) map[string]types.AuthConfig {
	authConfigs := map[string]types.AuthConfig{}
	authConfigsEncoded := header.Get("X-Registry-Config")

	if authConfigsEncoded == "" {
		return authConfigs
	}

	authConfigsJSON := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authConfigsEncoded))
	// Pulling an image does not error when no auth is provided so to remain
	// consistent with the existing api decode errors are ignored
	_ = json.NewDecoder(authConfigsJSON).Decode(&authConfigs)
	return authConfigs
}

type syncWriter struct {
	w  io.Writer
	mu sync.Mutex
}

func (s *syncWriter) Write(b []byte) (count int, err error) {
	s.mu.Lock()
	count, err = s.w.Write(b)
	s.mu.Unlock()
	return
}

func buildProgressWriter(out io.Writer, wantAux bool, createProgressReader func(io.ReadCloser) io.ReadCloser) backend.ProgressWriter {
	out = &syncWriter{w: out}

	var aux *streamformatter.AuxFormatter
	if wantAux {
		aux = &streamformatter.AuxFormatter{Writer: out}
	}

	return backend.ProgressWriter{
		Output:             out,
		StdoutFormatter:    streamformatter.NewStdoutWriter(out),
		StderrFormatter:    streamformatter.NewStderrWriter(out),
		AuxFormatter:       aux,
		ProgressReaderFunc: createProgressReader,
	}
}

type flusher interface {
	Flush()
}

func wrapOutputBufferedUntilRequestRead(rc io.ReadCloser, out io.Writer) (io.ReadCloser, io.Writer) {
	var fl flusher = &ioutils.NopFlusher{}
	if f, ok := out.(flusher); ok {
		fl = f
	}

	w := &wcf{
		buf:     bytes.NewBuffer(nil),
		Writer:  out,
		flusher: fl,
	}
	r := bufio.NewReader(rc)
	_, err := r.Peek(1)
	if err != nil {
		return rc, out
	}
	rc = &rcNotifier{
		Reader: r,
		Closer: rc,
		notify: w.notify,
	}
	return rc, w
}

type rcNotifier struct {
	io.Reader
	io.Closer
	notify func()
}

func (r *rcNotifier) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if err != nil {
		r.notify()
	}
	return n, err
}

func (r *rcNotifier) Close() error {
	r.notify()
	return r.Closer.Close()
}

type wcf struct {
	io.Writer
	flusher
	mu      sync.Mutex
	ready   bool
	buf     *bytes.Buffer
	flushed bool
}

func (w *wcf) Flush() {
	w.mu.Lock()
	w.flushed = true
	if !w.ready {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.flusher.Flush()
}

func (w *wcf) Flushed() bool {
	w.mu.Lock()
	b := w.flushed
	w.mu.Unlock()
	return b
}

func (w *wcf) Write(b []byte) (int, error) {
	w.mu.Lock()
	if !w.ready {
		n, err := w.buf.Write(b)
		w.mu.Unlock()
		return n, err
	}
	w.mu.Unlock()
	return w.Writer.Write(b)
}

func (w *wcf) notify() {
	w.mu.Lock()
	if !w.ready {
		if w.buf.Len() > 0 {
			_, _ = io.Copy(w.Writer, w.buf)
		}
		if w.flushed {
			w.flusher.Flush()
		}
		w.ready = true
	}
	w.mu.Unlock()
}
