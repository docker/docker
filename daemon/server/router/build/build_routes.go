package build

import (
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
	"syscall"

	"github.com/moby/moby/api/types/prune"

	"github.com/containerd/log"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/progress"
	"github.com/moby/moby/v2/daemon/internal/streamformatter"
	"github.com/moby/moby/v2/daemon/internal/versions"
	"github.com/moby/moby/v2/daemon/server/buildbackend"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/pkg/ioutils"
	"github.com/pkg/errors"
)

type invalidParam struct {
	error
}

func (e invalidParam) InvalidParameter() {}

func newImageBuildOptions(ctx context.Context, r *http.Request) (*buildbackend.BuildOptions, error) {
	options := &buildbackend.BuildOptions{
		Version:        build.BuilderV1, // Builder V1 is the default, but can be overridden
		Dockerfile:     r.FormValue("dockerfile"),
		SuppressOutput: httputils.BoolValue(r, "q"),
		NoCache:        httputils.BoolValue(r, "nocache"),
		ForceRemove:    httputils.BoolValue(r, "forcerm"),
		PullParent:     httputils.BoolValue(r, "pull"),
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
		// SecurityOpt only supports "credentials-spec" on Windows, and not used on other platforms.
		return nil, invalidParam{errors.New("security options are not supported on " + runtime.GOOS)}
	}

	if httputils.BoolValue(r, "forcerm") {
		options.Remove = true
	} else if r.FormValue("rm") == "" {
		options.Remove = true
	} else {
		options.Remove = httputils.BoolValue(r, "rm")
	}
	version := httputils.VersionFromContext(ctx)
	if versions.GreaterThanOrEqualTo(version, "1.32") {
		options.Platform = r.FormValue("platform")
	}
	if versions.GreaterThanOrEqualTo(version, "1.40") {
		outputsJSON := r.FormValue("outputs")
		if outputsJSON != "" {
			var outputs []buildbackend.BuildOutput
			if err := json.Unmarshal([]byte(outputsJSON), &outputs); err != nil {
				return nil, invalidParam{errors.Wrap(err, "invalid outputs specified")}
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
			return nil, invalidParam{errors.Errorf("unsupported isolation: %q", i)}
		}
	}

	if ulimitsJSON := r.FormValue("ulimits"); ulimitsJSON != "" {
		buildUlimits := []*container.Ulimit{}
		if err := json.Unmarshal([]byte(ulimitsJSON), &buildUlimits); err != nil {
			return nil, invalidParam{errors.Wrap(err, "error reading ulimit settings")}
		}
		options.Ulimits = buildUlimits
	}

	// Note that there are two ways a --build-arg might appear in the
	// json of the query param:
	// "foo":"bar"
	// and "foo":nil
	// The first is the normal case, ie. --build-arg foo=bar
	// or --build-arg foo
	// where foo's value was picked up from an env var.
	// The second ("foo":nil) is where they put --build-arg foo
	// but "foo" isn't set as an env var. In that case we can't just drop
	// the fact they mentioned it, we need to pass that along to the builder
	// so that it can print a warning about "foo" being unused if there is
	// no "ARG foo" in the Dockerfile.
	if buildArgsJSON := r.FormValue("buildargs"); buildArgsJSON != "" {
		buildArgs := map[string]*string{}
		if err := json.Unmarshal([]byte(buildArgsJSON), &buildArgs); err != nil {
			return nil, invalidParam{errors.Wrap(err, "error reading build args")}
		}
		options.BuildArgs = buildArgs
	}

	if labelsJSON := r.FormValue("labels"); labelsJSON != "" {
		labels := map[string]string{}
		if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
			return nil, invalidParam{errors.Wrap(err, "error reading labels")}
		}
		options.Labels = labels
	}

	if cacheFromJSON := r.FormValue("cachefrom"); cacheFromJSON != "" {
		cacheFrom := []string{}
		if err := json.Unmarshal([]byte(cacheFromJSON), &cacheFrom); err != nil {
			return nil, invalidParam{errors.Wrap(err, "error reading cache-from")}
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

func parseVersion(s string) (build.BuilderVersion, error) {
	switch build.BuilderVersion(s) {
	case build.BuilderV1:
		return build.BuilderV1, nil
	case build.BuilderBuildKit:
		return build.BuilderBuildKit, nil
	default:
		return "", invalidParam{errors.Errorf("invalid version %q", s)}
	}
}

func (br *buildRouter) postPrune(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	fltrs, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	opts := buildbackend.CachePruneOptions{
		All:     httputils.BoolValue(r, "all"),
		Filters: fltrs,
	}

	parseBytesFromFormValue := func(name string) (int64, error) {
		if fv := r.FormValue(name); fv != "" {
			bs, err := strconv.Atoi(fv)
			if err != nil {
				return 0, invalidParam{errors.Wrapf(err, "%s is in bytes and expects an integer, got %v", name, fv)}
			}
			return int64(bs), nil
		}
		return 0, nil
	}

	version := httputils.VersionFromContext(ctx)
	if versions.GreaterThanOrEqualTo(version, "1.48") {
		if bs, err := parseBytesFromFormValue("reserved-space"); err != nil {
			return err
		} else {
			if bs == 0 {
				// Deprecated parameter. Only checked if reserved-space is not used.
				bs, err = parseBytesFromFormValue("keep-storage")
				if err != nil {
					return err
				}
			}
			opts.ReservedSpace = bs
		}

		if bs, err := parseBytesFromFormValue("max-used-space"); err != nil {
			return err
		} else {
			opts.MaxUsedSpace = bs
		}

		if bs, err := parseBytesFromFormValue("min-free-space"); err != nil {
			return err
		} else {
			opts.MinFreeSpace = bs
		}
	} else {
		// Only keep-storage was valid in pre-1.48 versions.
		if bs, err := parseBytesFromFormValue("keep-storage"); err != nil {
			return err
		} else {
			opts.ReservedSpace = bs
		}
	}

	return httputils.WritePruneResponse(ctx, w, r, func(ctx context.Context, progress func(prune.Object)) (any, error) {
		opts.Progress = progress
		opts.Progress = progress
		return br.backend.PruneCache(ctx, opts)
	})
}

func (br *buildRouter) postCancel(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	w.Header().Set("Content-Type", "application/json")

	id := r.FormValue("id")
	if id == "" {
		return invalidParam{errors.New("build ID not provided")}
	}

	return br.backend.Cancel(ctx, id)
}

func (br *buildRouter) postBuild(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	var (
		notVerboseBuffer = bytes.NewBuffer(nil)
		version          = httputils.VersionFromContext(ctx)
	)

	w.Header().Set("Content-Type", "application/json")

	// Enable full-duplex so the server can write the response while still
	// reading the request body (build context tar). This replaces the old
	// wrapOutputBufferedUntilRequestRead workaround that buffered all output
	// until the first byte of the body was consumed; full-duplex support
	// landed in Go 1.21 (see https://go.dev/cl/472636).
	rc := http.NewResponseController(w)
	if err := rc.EnableFullDuplex(); err != nil {
		log.G(ctx).WithError(err).Warn("failed to enable full-duplex HTTP; falling back to default behavior")
	}

	output := ioutils.NewWriteFlusher(w)
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
		// don't log broken pipe errors as this is the normal case when a client aborts.
		if err != nil && !errors.Is(err, syscall.EPIPE) {
			log.G(ctx).WithError(err).Warn("could not write error response")
		}
		return nil
	}

	buildOptions, err := newImageBuildOptions(ctx, r)
	if err != nil {
		return errf(err)
	}
	buildOptions.AuthConfigs = getAuthConfigs(r.Header)

	if buildOptions.Squash && !br.daemon.HasExperimental() {
		return invalidParam{errors.New("squash is only supported with experimental mode")}
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

	imgID, err := br.backend.Build(ctx, buildbackend.BuildConfig{
		Source:         r.Body,
		Options:        buildOptions,
		ProgressWriter: buildProgressWriter(out, wantAux, createProgressReader),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.G(ctx).Debug("build canceled")
		}
		return errf(err)
	}

	// Everything worked so if -q was provided the output from the daemon
	// should be just the image ID and we'll print that to stdout.
	if buildOptions.SuppressOutput {
		_, _ = fmt.Fprintln(streamformatter.NewStdoutWriter(output), imgID)
	}
	return nil
}

func getAuthConfigs(header http.Header) map[string]registry.AuthConfig {
	authConfigs := map[string]registry.AuthConfig{}
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

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

func buildProgressWriter(out io.Writer, wantAux bool, createProgressReader func(io.ReadCloser) io.ReadCloser) buildbackend.ProgressWriter {
	// see https://github.com/moby/moby/pull/21406
	out = &syncWriter{w: out}

	var aux *streamformatter.AuxFormatter
	if wantAux {
		aux = &streamformatter.AuxFormatter{Writer: out}
	}

	return buildbackend.ProgressWriter{
		Output:             out,
		StdoutFormatter:    streamformatter.NewStdoutWriter(out),
		StderrFormatter:    streamformatter.NewStderrWriter(out),
		AuxFormatter:       aux,
		ProgressReaderFunc: createProgressReader,
	}
}
