package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/moby/moby/api/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/runconfig"
	"github.com/moby/moby/v2/daemon/internal/timestamp"
	"github.com/moby/moby/v2/daemon/internal/versions"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	networkSettings "github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/daemon/server/httpstatus"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/daemon/server/httputils/contenttype"
	"github.com/moby/moby/v2/daemon/server/httputils/logstream"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/pkg/ioutils"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"golang.org/x/net/websocket"
)

// errLegacyCapabilities is returned when a client sends the deprecated
// HostConfig.Capabilities field, which this daemon can no longer fulfill.
var errLegacyCapabilities = errdefs.InvalidParameter(errors.New("the HostConfig.Capabilities field is not supported by this daemon; use CapAdd and CapDrop to set capabilities"))

// commitRequest may contain an optional [container.Config].
type commitRequest struct {
	*container.Config
}

// decodeCommitRequest decodes the request body and returns a [container.Config]
// if present, or nil otherwise. It returns an error when failing to decode
// due to invalid JSON, or if the request contains multiple JSON documents.
//
// The client posts a bare [*container.Config] (see [Client.ContainerCommit]
// and [container.CommitOptions]), but it may be empty / nil, in which case
// it must be ignored, and no overrides to be applied.
//
// [Client.ContainerCommit]: https://github.com/moby/moby/blob/c4afa7715715a1020e50b19ad60728c4479fb0a5/client/container_commit.go#L52
// [container.CommitOptions]: https://github.com/moby/moby/blob/c4afa7715715a1020e50b19ad60728c4479fb0a5/api/types/container/options.go#L30
func decodeCommitRequest(r *http.Request) (*container.Config, error) {
	var w commitRequest
	if err := httputils.ReadJSON(r, &w); err != nil {
		return nil, err
	}
	return w.Config, nil
}

func (c *containerRouter) postCommit(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	config, err := decodeCommitRequest(r)
	if err != nil {
		return err
	}

	ref, err := httputils.RepoTagReference(r.Form.Get("repo"), r.Form.Get("tag"))
	if err != nil {
		return errdefs.InvalidParameter(err)
	}

	var noPause bool
	if r.Form.Has("pause") && !httputils.BoolValue(r, "pause") {
		noPause = true
	}

	imgID, err := c.backend.CreateImageFromContainer(ctx, r.Form.Get("container"), &backend.CreateImageConfig{
		NoPause: noPause,
		Tag:     ref,
		Author:  r.Form.Get("author"),
		Comment: r.Form.Get("comment"),
		Config:  config,
		Changes: r.Form["changes"],
	})
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusCreated, &container.CommitResponse{ID: imgID})
}

func (c *containerRouter) getContainersJSON(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	filter, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	var limit int
	if tmpLimit := r.Form.Get("limit"); tmpLimit != "" {
		val, err := strconv.Atoi(tmpLimit)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		limit = val
	}

	containers, err := c.backend.Containers(ctx, &backend.ContainerListOptions{
		All:     httputils.BoolValue(r, "all"),
		Size:    httputils.BoolValue(r, "size"),
		Limit:   limit,
		Filters: filter,
	})
	if err != nil {
		return err
	}

	version := httputils.VersionFromContext(ctx)

	if versions.LessThan(version, "1.46") {
		for i := range containers {
			// Ignore HostConfig.Annotations because it was added in API v1.46.
			containers[i].HostConfig.Annotations = nil
		}
	}

	if versions.LessThan(version, "1.48") {
		// ImageManifestDescriptor information was added in API 1.48
		for i := range containers {
			containers[i].ImageManifestDescriptor = nil
		}
	}

	if versions.LessThan(version, "1.52") {
		for i := range containers {
			containers[i].Health = nil
		}
	}

	return httputils.WriteJSON(w, http.StatusOK, containers)
}

func (c *containerRouter) getContainersStats(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	stream := httputils.BoolValueOrDefault(r, "stream", true)
	if !stream {
		w.Header().Set("Content-Type", "application/json")
	}
	var oneShot bool
	if versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.41") {
		oneShot = httputils.BoolValueOrDefault(r, "one-shot", false)
	}

	return c.backend.ContainerStats(ctx, vars["name"], &backend.ContainerStatsConfig{
		Stream:  stream,
		OneShot: oneShot,
		OutStream: func() io.Writer {
			// Assume that when this is called the request is OK.
			w.WriteHeader(http.StatusOK)
			if !stream {
				return w
			}
			wf := ioutils.NewWriteFlusher(w)
			wf.Flush()
			return wf
		},
	})
}

var jsonTypes = []string{
	types.MediaTypeJSONLines,
	types.MediaTypeNDJSON,
	types.MediaTypeJSONSequence,
}

func (c *containerRouter) getContainersLogs(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	// Args are validated before the stream starts because when it starts we're
	// sending HTTP 200 by writing an empty chunk of data to tell the client that
	// daemon is going to stream. By sending this initial HTTP 200 we can't report
	// any error after the stream starts (i.e. container not found, wrong parameters)
	// with the appropriate status code.
	stdout, stderr := httputils.BoolValue(r, "stdout"), httputils.BoolValue(r, "stderr")
	if !stdout && !stderr {
		return errdefs.InvalidParameter(errors.New("must specify at least one of 'stdout' or 'stderr'"))
	}

	var since time.Time
	if v := r.Form.Get("since"); v != "" {
		var err error
		since, err = timestamp.ParseUnixTimestamp(v)
		if err != nil {
			return errdefs.InvalidParameter(fmt.Errorf(`invalid value for "since": %w`, err))
		}
	}

	var until time.Time
	if v := r.Form.Get("until"); v != "" && v != "0" {
		var err error
		until, err = timestamp.ParseUnixTimestamp(v)
		if err != nil {
			return errdefs.InvalidParameter(fmt.Errorf(`invalid value for "until": %w`, err))
		}
	}

	var logFormat string
	// TODO(thaJeztah): this is currently experimental; there is no implementation
	// for this feature yet in the client, and the response struct is not yet part
	// of the API type definitions. Change this to API 1.55 once the remaining parts
	// are completed.
	if versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.54") {
		// Opt-in through "format" query-arg or JSON stream "Accept" header if
		// set; wildcards ("*/*", "application/*") are not considered.
		logFormat = r.Form.Get("format")
		if logFormat != "" && logFormat != "json" {
			return errdefs.InvalidParameter(errors.New("unsupported log format"))
		}
		if logFormat == "" && contenttype.MatchAcceptStrict(r.Header, jsonTypes) != "" {
			logFormat = "json"
		}
	}

	containerName := vars["name"]
	logsConfig := &backend.ContainerLogsOptions{
		Follow:     httputils.BoolValue(r, "follow"),
		Timestamps: httputils.BoolValue(r, "timestamps"),
		Since:      since,
		Until:      until,
		Tail:       r.Form.Get("tail"),
		ShowStdout: stdout,
		ShowStderr: stderr,
		Details:    httputils.BoolValue(r, "details"),
	}

	msgs, tty, err := c.backend.ContainerLogs(ctx, containerName, logsConfig)
	if err != nil {
		return err
	}

	if logFormat == "json" {
		w.Header().Set("Content-Type", contenttype.Negotiate(r.Header, jsonTypes, types.MediaTypeNDJSON))
		logstream.WriteJSON(ctx, w, msgs, logsConfig)
		return nil
	}

	contentType := types.MediaTypeRawStream
	if !tty && versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.42") {
		contentType = types.MediaTypeMultiplexedStream
	}
	w.Header().Set("Content-Type", contentType)

	// if has a tty, we're not muxing streams. if it doesn't, we are. simple.
	// this is the point of no return for writing a response. once we call
	// WriteLogStream, the response has been started and errors will be
	// returned in band by WriteLogStream
	logstream.Write(ctx, w, msgs, logsConfig, !tty)
	return nil
}

func (c *containerRouter) getContainersExport(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	return c.backend.ContainerExport(ctx, vars["name"], w)
}

func (c *containerRouter) postContainersStart(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	ctx, span := otel.Tracer("").Start(ctx, "containerRouter.postContainersStart")
	defer span.End()

	// If contentLength is -1, we can assumed chunked encoding
	// or more technically that the length is unknown
	// https://golang.org/src/pkg/net/http/request.go#L139
	// net/http otherwise seems to swallow any headers related to chunked encoding
	// including r.TransferEncoding
	// allow a nil body for backwards compatibility
	//
	// A non-nil json object is at least 7 characters.
	if r.ContentLength > 7 || r.ContentLength == -1 {
		return errdefs.InvalidParameter(errors.New("starting container with non-empty request body was deprecated since API v1.22 and removed in v1.24"))
	}

	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	if err := c.backend.ContainerStart(ctx, vars["name"], r.Form.Get("checkpoint"), r.Form.Get("checkpoint-dir")); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (c *containerRouter) postContainersStop(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var (
		options backend.ContainerStopOptions
		version = httputils.VersionFromContext(ctx)
	)
	if versions.GreaterThanOrEqualTo(version, "1.42") {
		options.Signal = r.Form.Get("signal")
	}
	if tmpSeconds := r.Form.Get("t"); tmpSeconds != "" {
		valSeconds, err := strconv.Atoi(tmpSeconds)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		options.Timeout = &valSeconds
	}

	if err := c.backend.ContainerStop(ctx, vars["name"], options); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (c *containerRouter) postContainersKill(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	name := vars["name"]
	if err := c.backend.ContainerKill(ctx, name, r.Form.Get("signal")); err != nil {
		return errors.Wrapf(err, "cannot kill container: %s", name)
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (c *containerRouter) postContainersRestart(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var (
		options backend.ContainerStopOptions
		version = httputils.VersionFromContext(ctx)
	)
	if versions.GreaterThanOrEqualTo(version, "1.42") {
		options.Signal = r.Form.Get("signal")
	}
	if tmpSeconds := r.Form.Get("t"); tmpSeconds != "" {
		valSeconds, err := strconv.Atoi(tmpSeconds)
		if err != nil {
			return err
		}
		options.Timeout = &valSeconds
	}

	if err := c.backend.ContainerRestart(ctx, vars["name"], options); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (c *containerRouter) postContainersPause(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	if err := c.backend.ContainerPause(vars["name"]); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)

	return nil
}

func (c *containerRouter) postContainersUnpause(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	if err := c.backend.ContainerUnpause(vars["name"]); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)

	return nil
}

func (c *containerRouter) postContainersWait(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	// Behavior changed in version 1.30 to handle wait condition and to
	// return headers immediately.
	version := httputils.VersionFromContext(ctx)
	legacyBehaviorPre130 := versions.LessThan(version, "1.30")
	legacyRemovalWaitPre134 := false

	// The wait condition defaults to "not-running".
	waitCondition := container.WaitConditionNotRunning
	if !legacyBehaviorPre130 {
		if err := httputils.ParseForm(r); err != nil {
			return err
		}
		if v := r.Form.Get("condition"); v != "" {
			switch container.WaitCondition(v) {
			case container.WaitConditionNotRunning:
				waitCondition = container.WaitConditionNotRunning
			case container.WaitConditionNextExit:
				waitCondition = container.WaitConditionNextExit
			case container.WaitConditionRemoved:
				waitCondition = container.WaitConditionRemoved
				legacyRemovalWaitPre134 = versions.LessThan(version, "1.34")
			default:
				return errdefs.InvalidParameter(errors.Errorf("invalid condition: %q", v))
			}
		}
	}

	waitC, err := c.backend.ContainerWait(ctx, vars["name"], waitCondition)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")

	if !legacyBehaviorPre130 {
		// Write response header immediately.
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	// Block on the result of the wait operation.
	status := <-waitC

	// With API < 1.34, wait on WaitConditionRemoved did not return
	// in case container removal failed. The only way to report an
	// error back to the client is to not write anything (i.e. send
	// an empty response which will be treated as an error).
	if legacyRemovalWaitPre134 && status.Err() != nil {
		return nil
	}

	var waitError *container.WaitExitError
	if status.Err() != nil {
		waitError = &container.WaitExitError{Message: status.Err().Error()}
	}

	return json.NewEncoder(w).Encode(&container.WaitResponse{
		StatusCode: int64(status.ExitCode()),
		Error:      waitError,
	})
}

func (c *containerRouter) getContainersChanges(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	changes, err := c.backend.ContainerChanges(ctx, vars["name"])
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, changes)
}

func (c *containerRouter) getContainersTop(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	procList, err := c.backend.ContainerTop(vars["name"], r.Form.Get("ps_args"))
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, procList)
}

func (c *containerRouter) postContainerRename(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	name := vars["name"]
	newName := r.Form.Get("name")
	if err := c.backend.ContainerRename(name, newName); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (c *containerRouter) postContainerUpdate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var updateConfig container.UpdateConfig
	if err := httputils.ReadJSON(r, &updateConfig); err != nil {
		return err
	}
	version := httputils.VersionFromContext(ctx)
	if versions.LessThan(version, "1.40") {
		updateConfig.PidsLimit = nil
	}
	if versions.LessThan(version, "1.55") {
		updateConfig.BlkioWeightDevice = nil
		updateConfig.BlkioDeviceReadBps = nil
		updateConfig.BlkioDeviceWriteBps = nil
		updateConfig.BlkioDeviceReadIOps = nil
		updateConfig.BlkioDeviceWriteIOps = nil
	}

	if updateConfig.PidsLimit != nil && *updateConfig.PidsLimit <= 0 {
		// Both `0` and `-1` are accepted to set "unlimited" when updating.
		// Historically, any negative value was accepted, so treat them as
		// "unlimited" as well.
		var unlimited int64
		updateConfig.PidsLimit = &unlimited
	}

	hostConfig := &container.HostConfig{
		Resources:     updateConfig.Resources,
		RestartPolicy: updateConfig.RestartPolicy,
	}

	name := vars["name"]
	resp, err := c.backend.ContainerUpdate(name, hostConfig)
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, resp)
}

func (c *containerRouter) postContainersCreate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	if err := httputils.CheckForJSON(r); err != nil {
		return err
	}

	name := r.Form.Get("name")

	// Use a tee-reader to allow reading the body for legacy fields.
	var requestBody bytes.Buffer
	rdr := io.TeeReader(r.Body, &requestBody)

	// TODO(thaJeztah): do we prefer [backend.ContainerCreateConfig] here?
	sysInfo, err := c.backend.RawSysInfo()
	if err != nil {
		return err
	}
	req, err := runconfig.DecodeCreateRequest(rdr, sysInfo)
	if err != nil {
		return err
	}
	// TODO(thaJeztah): update code below to take [container.CreateRequest] or [backend.ContainerCreateConfig] directly.
	config, hostConfig, networkingConfig := req.Config, req.HostConfig, req.NetworkingConfig

	// The NetworkMode "default" is used as a way to express a container should
	// be attached to the OS-dependant default network, in an OS-independent
	// way. Doing this conversion as soon as possible ensures we have less
	// NetworkMode to handle down the path (including in the
	// backward-compatibility layer we have just below).
	//
	// Note that this is not the only place where this conversion has to be
	// done (as there are various other places where containers get created).
	if hostConfig.NetworkMode == "" || hostConfig.NetworkMode.IsDefault() {
		hostConfig.NetworkMode = networkSettings.DefaultNetwork
		if nw, ok := networkingConfig.EndpointsConfig[network.NetworkDefault]; ok {
			networkingConfig.EndpointsConfig[hostConfig.NetworkMode.NetworkName()] = nw
			delete(networkingConfig.EndpointsConfig, network.NetworkDefault)
		}
	}

	version := httputils.VersionFromContext(ctx)

	if versions.LessThan(version, "1.40") {
		// Ignore BindOptions.NonRecursive because it was added in API 1.40.
		for _, m := range hostConfig.Mounts {
			if bo := m.BindOptions; bo != nil {
				bo.NonRecursive = false
			}
		}

		// Older clients (API < 1.40) expects the default to be shareable, make them happy
		if hostConfig.IpcMode.IsEmpty() {
			hostConfig.IpcMode = container.IPCModeShareable
		}
	}

	if versions.LessThan(version, "1.41") {
		// Older clients expect the default to be "host" on cgroup v1 hosts
		if hostConfig.CgroupnsMode.IsEmpty() && !sysInfo.CgroupUnified {
			hostConfig.CgroupnsMode = container.CgroupnsModeHost
		}
	}

	var platform *ocispec.Platform
	if versions.GreaterThanOrEqualTo(version, "1.41") {
		if v := r.Form.Get("platform"); v != "" {
			p, err := platforms.Parse(v)
			if err != nil {
				return errdefs.InvalidParameter(err)
			}
			platform = &p
		}
	}

	if versions.LessThan(version, "1.42") {
		for _, m := range hostConfig.Mounts {
			// Ignore BindOptions.CreateMountpoint because it was added in API 1.42.
			if bo := m.BindOptions; bo != nil {
				bo.CreateMountpoint = false
			}

			// These combinations are invalid, but weren't validated in API < 1.42.
			// We reset them here, so that validation doesn't produce an error.
			if o := m.VolumeOptions; o != nil && m.Type != mount.TypeVolume {
				m.VolumeOptions = nil
			}
			if o := m.TmpfsOptions; o != nil && m.Type != mount.TypeTmpfs {
				m.TmpfsOptions = nil
			}
			if bo := m.BindOptions; bo != nil {
				// Ignore BindOptions.CreateMountpoint because it was added in API 1.42.
				bo.CreateMountpoint = false
			}
		}

		if runtime.GOOS == "linux" {
			// ConsoleSize is not respected by Linux daemon before API 1.42
			hostConfig.ConsoleSize = [2]uint{0, 0}
		}
	}

	if versions.GreaterThanOrEqualTo(version, "1.42") {
		for _, m := range hostConfig.Mounts {
			if o := m.VolumeOptions; o != nil && m.Type != mount.TypeVolume {
				return errdefs.InvalidParameter(fmt.Errorf("VolumeOptions must not be specified on mount type %q", m.Type))
			}
			if o := m.BindOptions; o != nil && m.Type != mount.TypeBind {
				return errdefs.InvalidParameter(fmt.Errorf("BindOptions must not be specified on mount type %q", m.Type))
			}
			if o := m.TmpfsOptions; o != nil && m.Type != mount.TypeTmpfs {
				return errdefs.InvalidParameter(fmt.Errorf("TmpfsOptions must not be specified on mount type %q", m.Type))
			}
		}
	}

	if versions.LessThan(version, "1.43") {
		// Ignore Annotations because it was added in API v1.43.
		hostConfig.Annotations = nil
	}

	defaultReadOnlyNonRecursive := false
	if versions.LessThan(version, "1.44") {
		if config.Healthcheck != nil {
			// StartInterval was added in API 1.44
			config.Healthcheck.StartInterval = 0
		}

		// Set ReadOnlyNonRecursive to true because it was added in API 1.44
		// Before that all read-only mounts were non-recursive.
		// Keep that behavior for clients on older APIs.
		defaultReadOnlyNonRecursive = true

		for _, m := range hostConfig.Mounts {
			if m.Type == mount.TypeBind {
				if m.BindOptions != nil && m.BindOptions.ReadOnlyForceRecursive {
					// NOTE: that technically this is a breaking change for older
					// API versions, and we should ignore the new field.
					// However, this option may be incorrectly set by a client with
					// the expectation that the failing to apply recursive read-only
					// is enforced, so we decided to produce an error instead,
					// instead of silently ignoring.
					return errdefs.InvalidParameter(errors.New("BindOptions.ReadOnlyForceRecursive needs API v1.44 or newer"))
				}
			}
		}

		// Creating a container connected to several networks is not supported until v1.44.
		if len(networkingConfig.EndpointsConfig) > 1 {
			l := make([]string, 0, len(networkingConfig.EndpointsConfig))
			for k := range networkingConfig.EndpointsConfig {
				l = append(l, k)
			}
			return errdefs.InvalidParameter(errors.Errorf("Container cannot be created with multiple network endpoints: %s", strings.Join(l, ", ")))
		}
	}

	if versions.LessThan(version, "1.45") {
		for _, m := range hostConfig.Mounts {
			if m.VolumeOptions != nil && m.VolumeOptions.Subpath != "" {
				return errdefs.InvalidParameter(errors.New("VolumeOptions.Subpath needs API v1.45 or newer"))
			}
		}
	}

	if versions.LessThan(version, "1.48") {
		for _, epConfig := range networkingConfig.EndpointsConfig {
			// Before 1.48, all endpoints had the same priority, so
			// reinitialize this field.
			epConfig.GwPriority = 0
		}
		for _, m := range hostConfig.Mounts {
			if m.Type == mount.TypeImage {
				return errdefs.InvalidParameter(errors.New(`Mount type "Image" needs API v1.48 or newer`))
			}
		}
	}

	var warnings []string
	if warn := handleVolumeDriverBC(version, hostConfig); warn != "" {
		warnings = append(warnings, warn)
	}
	if versions.LessThan(version, "1.52") {
		var legacyConfig struct {
			// Mac Address of the container.
			//
			// MacAddress field is deprecated since API v1.44. Use EndpointSettings.MacAddress instead.
			MacAddress network.HardwareAddr `json:",omitempty"`

			HostConfig struct {
				// Capabilities was removed in commit 24f173a003 for
				// API version 1.41, favoring CapAdd and CapDrop instead.
				Capabilities []string `json:",omitempty"`
			}
		}
		_ = json.Unmarshal(requestBody.Bytes(), &legacyConfig)

		if err := rejectLegacyCapabilities(legacyConfig.HostConfig.Capabilities, version); err != nil {
			return err
		}

		if warn, err := handleMACAddressBC(hostConfig, networkingConfig, version, legacyConfig.MacAddress); err != nil {
			return err
		} else if warn != "" {
			warnings = append(warnings, warn)
		}
	}

	if warn, err := handleSysctlBC(hostConfig, networkingConfig, version); err != nil {
		return err
	} else if warn != "" {
		warnings = append(warnings, warn)
	}

	if warn := handlePortBindingsBC(hostConfig, version); warn != "" {
		warnings = append(warnings, warn)
	}

	if hostConfig.PidsLimit != nil && *hostConfig.PidsLimit <= 0 {
		// Don't set a limit if either no limit was specified, or "unlimited" was
		// explicitly set.
		// Both `0` and `-1` are accepted as "unlimited", and historically any
		// negative value was accepted, so treat those as "unlimited" as well.
		hostConfig.PidsLimit = nil
	}

	if versions.LessThan(version, "1.56") {
		// Ignore Umask because it was added in API v1.56.
		hostConfig.Umask = nil
	}

	ccr, err := c.backend.ContainerCreate(ctx, backend.ContainerCreateConfig{
		Name:                        name,
		Config:                      config,
		HostConfig:                  hostConfig,
		NetworkingConfig:            networkingConfig,
		Platform:                    platform,
		DefaultReadOnlyNonRecursive: defaultReadOnlyNonRecursive,
	})

	// Log warnings for debugging, regardless if the request was successful or not.
	if len(ccr.Warnings) > 0 {
		log.G(ctx).WithField("warnings", ccr.Warnings).Debug("warnings encountered during container create request")
	}

	if err != nil {
		return err
	}
	ccr.Warnings = append(ccr.Warnings, warnings...)
	return httputils.WriteJSON(w, http.StatusCreated, ccr)
}

// handleVolumeDriverBC handles the use of the container-wide "VolumeDriver"
// option when the Mounts API is used for volumes. It produces a warning
// on API 1.48 and up. Older versions of the API did not produce a warning,
// but the CLI would do so.
func handleVolumeDriverBC(version string, hostConfig *container.HostConfig) (warning string) {
	if hostConfig.VolumeDriver == "" || versions.LessThan(version, "1.48") {
		return ""
	}
	for _, m := range hostConfig.Mounts {
		if m.Type != mount.TypeVolume {
			continue
		}
		if m.VolumeOptions != nil && m.VolumeOptions.DriverConfig != nil && m.VolumeOptions.DriverConfig.Name != "" {
			// Driver was configured for this mount, so no ambiguity.
			continue
		}
		return "WARNING: the container-wide volume-driver configuration is ignored for volumes specified via 'mount'. Use '--mount type=volume,volume-driver=...' instead"
	}
	return ""
}

// rejectLegacyCapabilities returns an error if the deprecated
// HostConfig.Capabilities field is present in a request for API version 1.40.
//
// The Capabilities field was added in API 1.40 (Docker 19.03) as a way to
// specify the exact set of kernel capabilities for a container, overriding
// the CapAdd/CapDrop mechanism. It was removed before API 1.41 (Docker 20.10)
// because putting the burden of providing the full capability list on the
// client was considered a poor design (see 24f173a003).
//
// The field has since been deleted from the API type, so json.Unmarshal
// silently drops it. This daemon cannot honor the field, so it is best to
// return an explicit error rather than silently ignoring the field when it is
// expected to be supported.
func rejectLegacyCapabilities(capabilities []string, version string) error {
	if len(capabilities) > 0 && versions.Equal(version, "1.40") {
		return errLegacyCapabilities
	}
	return nil
}

// handleMACAddressBC takes care of backward-compatibility for the container-wide MAC address by mutating the
// networkingConfig to set the endpoint-specific MACAddress field introduced in API v1.44. It returns a warning message
// or an error if the container-wide field was specified for API >= v1.44.
func handleMACAddressBC(hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, version string, deprecatedMacAddress network.HardwareAddr) (string, error) {
	// For older versions of the API, migrate the container-wide MAC address to EndpointsConfig.
	if versions.LessThan(version, "1.44") {
		if len(deprecatedMacAddress) == 0 {
			// If a MAC address is supplied in EndpointsConfig, discard it because the old API
			// would have ignored it.
			for _, ep := range networkingConfig.EndpointsConfig {
				ep.MacAddress = nil
			}
			return "", nil
		}
		if !hostConfig.NetworkMode.IsBridge() && !hostConfig.NetworkMode.IsUserDefined() {
			return "", errdefs.InvalidParameter(errors.New("conflicting options: mac-address and the network mode"))
		}

		epConfig, err := epConfigForNetMode(version, hostConfig.NetworkMode, networkingConfig)
		if err != nil {
			return "", err
		}
		epConfig.MacAddress = deprecatedMacAddress
		return "", nil
	}

	// The container-wide MacAddress parameter is deprecated and should now be specified in EndpointsConfig.
	if len(deprecatedMacAddress) == 0 {
		return "", nil
	}

	if versions.GreaterThanOrEqualTo(version, "1.52") {
		return "", errdefs.InvalidParameter(errors.New("container-wide MAC address no longer supported; use endpoint-specific MAC address instead"))
	}

	var warning string
	if hostConfig.NetworkMode.IsBridge() || hostConfig.NetworkMode.IsUserDefined() {
		ep, err := epConfigForNetMode(version, hostConfig.NetworkMode, networkingConfig)
		if err != nil {
			return "", errors.Wrap(err, "unable to migrate container-wide MAC address to a specific network")
		}
		// ep is the endpoint that needs the container-wide MAC address; migrate the address
		// to it, or bail out if there's a mismatch.
		if len(ep.MacAddress) == 0 {
			ep.MacAddress = deprecatedMacAddress
		} else if !slices.Equal(ep.MacAddress, deprecatedMacAddress) {
			return "", errdefs.InvalidParameter(errors.New("the container-wide MAC address must match the endpoint-specific MAC address for the main network, or be left empty"))
		}
	}
	warning = "The container-wide MacAddress field is now deprecated. It should be specified in EndpointsConfig instead."

	return warning, nil
}

// handleSysctlBC migrates top level network endpoint-specific '--sysctl'
// settings to an DriverOpts for an endpoint. This is necessary because sysctls
// are applied during container task creation, but sysctls that name an interface
// (for example 'net.ipv6.conf.eth0.forwarding') cannot be applied until the
// interface has been created. So, these settings are removed from hostConfig.Sysctls
// and added to DriverOpts[netlabel.EndpointSysctls].
//
// Because interface names ('ethN') are allocated sequentially, and the order of
// network connections is not deterministic on container restart, only 'eth0'
// would work reliably in a top-level '--sysctl' option, and then only when
// there's a single initial network connection. So, settings for 'eth0' are
// migrated to the primary interface, identified by 'hostConfig.NetworkMode'.
// Settings for other interfaces are treated as errors.
//
// In the DriverOpts, because the interface name cannot be determined in advance, the
// interface name is replaced by "IFNAME". For example, 'net.ipv6.conf.eth0.forwarding'
// becomes 'net.ipv6.conf.IFNAME.forwarding'. The value in DriverOpts is a
// comma-separated list.
//
// A warning is generated when settings are migrated.
func handleSysctlBC(
	hostConfig *container.HostConfig,
	netConfig *network.NetworkingConfig,
	version string,
) (string, error) {
	if !hostConfig.NetworkMode.IsPrivate() {
		return "", nil
	}

	var ep *network.EndpointSettings
	var toDelete []string
	var netIfSysctls []string
	for k, v := range hostConfig.Sysctls {
		// If the sysctl name matches "net.*.*.eth0.*" ...
		if spl := strings.SplitN(k, ".", 5); len(spl) == 5 && spl[0] == "net" && strings.HasPrefix(spl[3], "eth") {
			netIfSysctl := fmt.Sprintf("net.%s.%s.IFNAME.%s=%s", spl[1], spl[2], spl[4], v)
			// Find the EndpointConfig to migrate settings to, if not already found.
			if ep == nil {
				// Per-endpoint sysctls were introduced in API version 1.46. Migration is
				// needed, but refuse to do it automatically for API 1.48 and newer.
				if versions.GreaterThan(version, "1.47") {
					return "", fmt.Errorf("interface specific sysctl setting %q must be supplied using driver option '%s'",
						k, netlabel.EndpointSysctls)
				}
				var err error
				ep, err = epConfigForNetMode(version, hostConfig.NetworkMode, netConfig)
				if err != nil {
					return "", fmt.Errorf("unable to find a network for sysctl %s: %w", k, err)
				}
			}
			// Only try to migrate settings for "eth0", anything else would always
			// have behaved unpredictably.
			if spl[3] != "eth0" {
				return "", fmt.Errorf(`unable to determine network endpoint for sysctl %s, use driver option '%s' to set per-interface sysctls`,
					k, netlabel.EndpointSysctls)
			}
			// Prepare the migration.
			toDelete = append(toDelete, k)
			netIfSysctls = append(netIfSysctls, netIfSysctl)
		}
	}
	if ep == nil {
		return "", nil
	}

	newDriverOpt := strings.Join(netIfSysctls, ",")
	warning := fmt.Sprintf(`Migrated sysctl %q to DriverOpts{%q:%q}.`,
		strings.Join(toDelete, ","),
		netlabel.EndpointSysctls, newDriverOpt)

	// Append existing per-endpoint sysctls to the migrated sysctls (give priority
	// to per-endpoint settings).
	if ep.DriverOpts == nil {
		ep.DriverOpts = map[string]string{}
	}
	if oldDriverOpt, ok := ep.DriverOpts[netlabel.EndpointSysctls]; ok {
		newDriverOpt += "," + oldDriverOpt
	}
	ep.DriverOpts[netlabel.EndpointSysctls] = newDriverOpt

	// Delete migrated settings from the top-level sysctls.
	for _, k := range toDelete {
		delete(hostConfig.Sysctls, k)
	}

	return warning, nil
}

// handlePortBindingsBC handles backward-compatibility for empty port bindings.
//
// Before Engine v29.0, an empty list of port bindings for a container port was
// treated as if a PortBinding with an unspecified IP address and HostPort was
// provided. The daemon was doing this backfilling on ContainerStart.
//
// Preserve this behavior for older API versions but emit a warning for API
// v1.52 and drop that behavior for newer API versions.
//
// See https://github.com/moby/moby/pull/50710#discussion_r2315840899 for more
// context.
func handlePortBindingsBC(hostConfig *container.HostConfig, version string) string {
	var emptyPBs []string

	for port, bindings := range hostConfig.PortBindings {
		if len(bindings) > 0 {
			continue
		}
		/*
			API 1.53 shipped in a minor release. We cannot introduce a breaking change there, so
			we must still backfill empty port bindings. This change can be re-introduced for the
			API version that ships in 30.x. Note that some networking tests will need fixing.
			See https://github.com/moby/moby/issues/51727

			if versions.GreaterThan(version, "1.52") && len(bindings) == 0 {
				// Starting with API 1.53, no backfilling is done. An empty slice
				// of port bindings is treated as "no port bindings" by the daemon,
				// but it still needs to backfill empty slices when loading the
				// on-disk state for containers created by older versions of the
				// Engine. Drop the PortBindings entry to ensure that no backfilling
				// will happen when restarting the daemon.
				delete(hostConfig.PortBindings, port)
				continue
			}
		*/

		if versions.GreaterThanOrEqualTo(version, "1.52") {
			emptyPBs = append(emptyPBs, port.String())
		}

		hostConfig.PortBindings[port] = []network.PortBinding{{}}
	}

	if len(emptyPBs) > 0 {
		return fmt.Sprintf("Following container port(s) have an empty list of port-bindings: %s. Such bindings will be discarded in a future version.", strings.Join(emptyPBs, ", "))
	}

	return ""
}

// epConfigForNetMode finds, or creates, an entry in netConfig.EndpointsConfig
// corresponding to nwMode.
//
// nwMode.NetworkName() may be the network's name, its id, or its short-id.
//
// The corresponding endpoint in netConfig.EndpointsConfig may be keyed on a
// different one of name/id/short-id. If there's any ambiguity (there are
// endpoints but the names don't match), return an error and do not create a new
// endpoint, because it might be a duplicate.
func epConfigForNetMode(
	version string,
	nwMode container.NetworkMode,
	netConfig *network.NetworkingConfig,
) (*network.EndpointSettings, error) {
	nwName := nwMode.NetworkName()

	// It's always safe to create an EndpointsConfig entry under nwName if there are
	// no entries already (because there can't be an entry for this network nwName
	// refers to under any other name/short-id/id).
	if len(netConfig.EndpointsConfig) == 0 {
		es := &network.EndpointSettings{}
		netConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			nwName: es,
		}
		return es, nil
	}

	// There cannot be more than one entry in EndpointsConfig with API < 1.44.
	if versions.LessThan(version, "1.44") {
		// No need to check for a match between NetworkMode and the names/ids in EndpointsConfig,
		// the old version of the API would pick this network anyway.
		for _, ep := range netConfig.EndpointsConfig {
			return ep, nil
		}
	}

	// There is existing endpoint config - if it's not indexed by NetworkMode.Name(), we
	// can't tell which network the container-wide settings are intended for. NetworkMode,
	// the keys in EndpointsConfig and the NetworkID in EndpointsConfig may mix network
	// name/id/short-id. It's not safe to create EndpointsConfig under the NetworkMode
	// name to store the container-wide setting, because that may result in two sets
	// of EndpointsConfig for the same network and one set will be discarded later. So,
	// reject the request ...
	ep, ok := netConfig.EndpointsConfig[nwName]
	if !ok {
		return nil, errdefs.InvalidParameter(
			errors.New("HostConfig.NetworkMode must match the identity of a network in NetworkSettings.Networks"))
	}

	return ep, nil
}

func (c *containerRouter) deleteContainers(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	name := vars["name"]
	config := &backend.ContainerRmConfig{
		ForceRemove:  httputils.BoolValue(r, "force"),
		RemoveVolume: httputils.BoolValue(r, "v"),
		RemoveLink:   httputils.BoolValue(r, "link"),
	}

	if err := c.backend.ContainerRm(name, config); err != nil {
		return err
	}

	w.WriteHeader(http.StatusNoContent)

	return nil
}

func (c *containerRouter) postContainersResize(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	height, err := httputils.Uint32Value(r, "h")
	if err != nil {
		return errdefs.InvalidParameter(errors.Wrapf(err, "invalid resize height %q", r.Form.Get("h")))
	}
	width, err := httputils.Uint32Value(r, "w")
	if err != nil {
		return errdefs.InvalidParameter(errors.Wrapf(err, "invalid resize width %q", r.Form.Get("w")))
	}

	return c.backend.ContainerResize(ctx, vars["name"], height, width)
}

func (c *containerRouter) postContainersAttach(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	containerName := vars["name"]
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return errdefs.InvalidParameter(errors.Errorf("error attaching to container %s, hijack connection missing", containerName))
	}

	contentType := types.MediaTypeRawStream
	_, upgrade := r.Header["Upgrade"]
	setupStreams := func(multiplexed bool, cancel func()) (io.ReadCloser, io.Writer, io.Writer, error) {
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return nil, nil, nil, err
		}

		// set raw mode
		if _, err := conn.Write([]byte{}); err != nil {
			conn.Close()
			return nil, nil, nil, err
		}

		if upgrade {
			if multiplexed && versions.GreaterThanOrEqualTo(httputils.VersionFromContext(ctx), "1.42") {
				contentType = types.MediaTypeMultiplexedStream
			}
			if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: %v\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", contentType); err != nil {
				conn.Close()
				return nil, nil, nil, err
			}
		} else {
			if _, err := fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n"); err != nil {
				conn.Close()
				return nil, nil, nil, err
			}
		}

		go notifyClosed(ctx, conn, cancel)

		closer := func() error {
			httputils.CloseStreams(conn)
			return nil
		}
		return ioutils.NewReadCloserWrapper(conn, closer), conn, conn, nil
	}

	if err := c.backend.ContainerAttach(containerName, &backend.ContainerAttachConfig{
		GetStreams: setupStreams,
		UseStdin:   httputils.BoolValue(r, "stdin"),
		UseStdout:  httputils.BoolValue(r, "stdout"),
		UseStderr:  httputils.BoolValue(r, "stderr"),
		Logs:       httputils.BoolValue(r, "logs"),
		Stream:     httputils.BoolValue(r, "stream"),
		DetachKeys: r.FormValue("detachKeys"),
		MuxStreams: true,
	}); err != nil {
		log.G(ctx).WithError(err).Errorf("Handler for %s %s returned error", r.Method, r.URL.Path)
		// Remember to close stream if error happens
		conn, _, errHijack := hijacker.Hijack()
		if errHijack != nil {
			log.G(ctx).WithError(err).Errorf("Handler for %s %s: unable to close stream; error when hijacking connection", r.Method, r.URL.Path)
		} else {
			statusCode := httpstatus.FromError(err)
			statusText := http.StatusText(statusCode)
			fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\n\r\n%s\r\n", statusCode, statusText, contentType, err.Error())
			httputils.CloseStreams(conn)
		}
	}
	return nil
}

func (c *containerRouter) wsContainersAttach(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	containerName := vars["name"]

	done := make(chan struct{})
	started := make(chan struct{})

	version := httputils.VersionFromContext(ctx)

	setupStreams := func(multiplexed bool, cancel func()) (io.ReadCloser, io.Writer, io.Writer, error) {
		wsChan := make(chan *websocket.Conn)
		h := func(conn *websocket.Conn) {
			wsChan <- conn
			<-done
		}

		srv := websocket.Server{Handler: h, Handshake: nil}
		go func() {
			close(started)
			srv.ServeHTTP(w, r)
		}()

		conn := <-wsChan
		// In case version 1.28 and above, a binary frame will be sent.
		// See 28176 for details.
		if versions.GreaterThanOrEqualTo(version, "1.28") {
			conn.PayloadType = websocket.BinaryFrame
		}

		// TODO: Close notifications
		return conn, conn, conn, nil
	}

	useStdin, useStdout, useStderr := true, true, true
	if versions.GreaterThanOrEqualTo(version, "1.42") {
		useStdin = httputils.BoolValue(r, "stdin")
		useStdout = httputils.BoolValue(r, "stdout")
		useStderr = httputils.BoolValue(r, "stderr")
	}

	err := c.backend.ContainerAttach(containerName, &backend.ContainerAttachConfig{
		GetStreams: setupStreams,
		UseStdin:   useStdin,
		UseStdout:  useStdout,
		UseStderr:  useStderr,
		Logs:       httputils.BoolValue(r, "logs"),
		Stream:     httputils.BoolValue(r, "stream"),
		DetachKeys: r.FormValue("detachKeys"),
		MuxStreams: false, // never multiplex, as we rely on websocket to manage distinct streams
	})
	close(done)
	select {
	case <-started:
		if err != nil {
			log.G(ctx).Errorf("Error attaching websocket: %s", err)
		} else {
			log.G(ctx).Debug("websocket connection was closed by client")
		}
		return nil
	default:
	}
	return err
}

func (c *containerRouter) postContainersPrune(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	pruneFilters, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	pruneReport, err := c.backend.ContainerPrune(ctx, pruneFilters)
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, pruneReport)
}
