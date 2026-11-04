package container

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/common"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/stringid"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/moby/moby/v2/daemon/pkg/oci"
	testContainer "github.com/moby/moby/v2/integration/internal/container"
	net "github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/request"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestCreateFailsWhenIdentifierDoesNotExist(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testCases := []struct {
		doc           string
		image         string
		expectedError string
	}{
		{
			doc:           "image and tag",
			image:         "test456:v1",
			expectedError: "No such image: test456:v1",
		},
		{
			doc:           "image no tag",
			image:         "test456",
			expectedError: "No such image: test456",
		},
		{
			doc:           "digest",
			image:         "sha256:0cb40641836c461bc97c793971d84d758371ed682042457523e4ae701efeaaaa",
			expectedError: "No such image: sha256:0cb40641836c461bc97c793971d84d758371ed682042457523e4ae701efeaaaa",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.doc, func(t *testing.T) {
			t.Parallel()
			ctx := testutil.StartSpan(ctx, t)
			_, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config:           &container.Config{Image: tc.image},
				HostConfig:       &container.HostConfig{},
				NetworkingConfig: &network.NetworkingConfig{},
			})
			assert.Check(t, is.ErrorContains(err, tc.expectedError))
			assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
		})
	}
}

func TestCreateByImageID(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	img, err := apiClient.ImageInspect(ctx, "busybox")
	assert.NilError(t, err)

	imgIDWithAlgorithm := img.ID
	assert.Assert(t, imgIDWithAlgorithm != "")

	imgID, _ := strings.CutPrefix(img.ID, "sha256:")
	assert.Assert(t, imgID != "")

	imgShortID := stringid.TruncateID(img.ID)
	assert.Assert(t, imgShortID != "")

	testCases := []struct {
		doc             string
		image           string
		expectedErrType func(error) bool
		expectedErr     string
	}{
		{
			doc:   "image ID with algorithm",
			image: imgIDWithAlgorithm,
		},
		{
			// test case for https://github.com/moby/moby/issues/20972
			doc:   "image ID without algorithm",
			image: imgID,
		},
		{
			doc:   "image short-ID",
			image: imgShortID,
		},
		{
			doc:             "image with ID and algorithm as tag",
			image:           "busybox:" + imgIDWithAlgorithm,
			expectedErrType: cerrdefs.IsInvalidArgument,
			expectedErr:     "Error response from daemon: invalid reference format",
		},
		{
			doc:             "image with ID as tag",
			image:           "busybox:" + imgID,
			expectedErrType: cerrdefs.IsNotFound,
			expectedErr:     "Error response from daemon: No such image: busybox:" + imgID,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.doc, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)
			resp, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config: &container.Config{Image: tc.image},
			})
			if tc.expectedErr != "" {
				assert.Check(t, is.DeepEqual(resp, client.ContainerCreateResult{}))
				assert.Check(t, is.Error(err, tc.expectedErr))
				assert.Check(t, is.ErrorType(err, tc.expectedErrType))
			} else {
				assert.NilError(t, err)
				assert.Check(t, resp.ID != "")
			}
			// cleanup the container if one was created.
			_, _ = apiClient.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
		})
	}
}

// TestCreateLinkToNonExistingContainer verifies that linking to a non-existing
// container returns an "invalid parameter" (400) status, and not the underlying
// "non exists" (404).
func TestCreateLinkToNonExistingContainer(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "legacy links are not supported on windows")
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	_, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "busybox",
		},
		HostConfig: &container.HostConfig{
			Links: []string{"no-such-container"},
		},
	})
	assert.Check(t, is.ErrorContains(err, "could not get container for no-such-container"))
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
}

func TestCreateWithInvalidEnv(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testCases := []struct {
		env           string
		expectedError string
	}{
		{
			env:           "",
			expectedError: "invalid environment variable:",
		},
		{
			env:           "=",
			expectedError: "invalid environment variable: =",
		},
		{
			env:           "=foo",
			expectedError: "invalid environment variable: =foo",
		},
	}

	for index, tc := range testCases {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			ctx := testutil.StartSpan(ctx, t)
			_, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config: &container.Config{
					Image: "busybox",
					Env:   []string{tc.env},
				},
			})
			assert.Check(t, is.ErrorContains(err, tc.expectedError))
			assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
		})
	}
}

// Test case for #30166 (target was not validated)
func TestCreateTmpfsMountsTarget(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()

	testCases := []struct {
		target        string
		expectedError string
	}{
		{
			target:        ".",
			expectedError: "mount path must be absolute",
		},
		{
			target:        "foo",
			expectedError: "mount path must be absolute",
		},
		{
			target:        "/",
			expectedError: "destination can't be '/'",
		},
		{
			target:        "//",
			expectedError: "destination can't be '/'",
		},
	}

	for _, tc := range testCases {
		_, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config: &container.Config{
				Image: "busybox",
			},
			HostConfig: &container.HostConfig{
				Tmpfs: map[string]string{tc.target: ""},
			},
		})
		assert.Check(t, is.ErrorContains(err, tc.expectedError))
		assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
	}
}

func TestCreateWithCustomMaskedPaths(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testCases := []struct {
		doc         string
		privileged  bool
		maskedPaths []string
		expected    []string
	}{
		{
			doc:         "default masked paths",
			maskedPaths: nil,
			expected:    oci.DefaultSpec().Linux.MaskedPaths,
		},
		{
			doc:         "no masked paths",
			maskedPaths: []string{},
			expected:    []string{},
		},
		{
			doc:         "custom masked paths",
			maskedPaths: []string{"/proc/kcore", "/proc/keys"},
			expected:    []string{"/proc/kcore", "/proc/keys"},
		},
		{
			// privileged containers should have no masked paths by default
			doc:         "privileged",
			privileged:  true,
			maskedPaths: nil,
			expected:    nil,
		},
	}

	for i, tc := range testCases {
		t.Run(tc.doc, func(t *testing.T) {
			skip.If(t, tc.privileged && testEnv.IsUserNamespace(), "privileged mode is incompatible with user namespaces")
			t.Parallel()

			// Create the container.
			ctr, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config: &container.Config{
					Image: "busybox",
					Cmd:   []string{"true"},
				},
				HostConfig: &container.HostConfig{
					Privileged:  tc.privileged,
					MaskedPaths: tc.maskedPaths,
				},
				Name: fmt.Sprintf("create-masked-paths-%d", i),
			})
			assert.NilError(t, err)

			inspect, err := apiClient.ContainerInspect(ctx, ctr.ID, client.ContainerInspectOptions{})
			assert.NilError(t, err)
			assert.DeepEqual(t, inspect.Container.HostConfig.MaskedPaths, tc.expected)

			// Start the container.
			_, err = apiClient.ContainerStart(ctx, ctr.ID, client.ContainerStartOptions{})
			assert.NilError(t, err)

			// It should die down by itself, but stop it to be sure.
			_, err = apiClient.ContainerStop(ctx, ctr.ID, client.ContainerStopOptions{})
			assert.NilError(t, err)

			inspect, err = apiClient.ContainerInspect(ctx, ctr.ID, client.ContainerInspectOptions{})
			assert.NilError(t, err)
			assert.DeepEqual(t, inspect.Container.HostConfig.MaskedPaths, tc.expected)
		})
	}
}

func TestCreateWithCustomReadonlyPaths(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testCases := []struct {
		doc           string
		privileged    bool
		readonlyPaths []string
		expected      []string
	}{
		{
			doc:           "default readonly paths",
			readonlyPaths: nil,
			expected:      oci.DefaultSpec().Linux.ReadonlyPaths,
		},
		{
			doc:           "empty readonly paths",
			readonlyPaths: []string{},
			expected:      []string{},
		},
		{
			doc:           "custom readonly paths",
			readonlyPaths: []string{"/proc/asound", "/proc/bus"},
			expected:      []string{"/proc/asound", "/proc/bus"},
		},
		{
			// privileged containers should have no readonly paths by default
			doc:           "privileged",
			privileged:    true,
			readonlyPaths: nil,
			expected:      nil,
		},
	}

	for i, tc := range testCases {
		t.Run(tc.doc, func(t *testing.T) {
			skip.If(t, tc.privileged && testEnv.IsUserNamespace(), "privileged mode is incompatible with user namespaces")
			t.Parallel()
			ctr, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config: &container.Config{
					Image: "busybox",
					Cmd:   []string{"true"},
				},
				HostConfig: &container.HostConfig{
					Privileged:    tc.privileged,
					ReadonlyPaths: tc.readonlyPaths,
				},
				Name: fmt.Sprintf("create-readonly-paths-%d", i),
			})
			assert.NilError(t, err)

			ctrInspect, err := apiClient.ContainerInspect(ctx, ctr.ID, client.ContainerInspectOptions{})
			assert.NilError(t, err)
			assert.DeepEqual(t, ctrInspect.Container.HostConfig.ReadonlyPaths, tc.expected)

			// Start the container.
			_, err = apiClient.ContainerStart(ctx, ctr.ID, client.ContainerStartOptions{})
			assert.NilError(t, err)

			// It should die down by itself, but stop it to be sure.
			_, err = apiClient.ContainerStop(ctx, ctr.ID, client.ContainerStopOptions{})
			assert.NilError(t, err)

			ctrInspect, err = apiClient.ContainerInspect(ctx, ctr.ID, client.ContainerInspectOptions{})
			assert.NilError(t, err)
			assert.DeepEqual(t, ctrInspect.Container.HostConfig.ReadonlyPaths, tc.expected)
		})
	}
}

func TestCreateWithInvalidHealthcheckParams(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testCases := []struct {
		doc           string
		interval      time.Duration
		timeout       time.Duration
		retries       int
		startPeriod   time.Duration
		startInterval time.Duration
		expectedErr   string
	}{
		{
			doc:         "test invalid Interval in Healthcheck: less than 0s",
			interval:    -10 * time.Millisecond,
			timeout:     time.Second,
			retries:     1000,
			expectedErr: fmt.Sprintf("Interval in Healthcheck cannot be less than %s", container.MinimumDuration),
		},
		{
			doc:         "test invalid Interval in Healthcheck: larger than 0s but less than 1ms",
			interval:    500 * time.Microsecond,
			timeout:     time.Second,
			retries:     1000,
			expectedErr: fmt.Sprintf("Interval in Healthcheck cannot be less than %s", container.MinimumDuration),
		},
		{
			doc:         "test invalid Timeout in Healthcheck: less than 1ms",
			interval:    time.Second,
			timeout:     -100 * time.Millisecond,
			retries:     1000,
			expectedErr: fmt.Sprintf("Timeout in Healthcheck cannot be less than %s", container.MinimumDuration),
		},
		{
			doc:         "test invalid Retries in Healthcheck: less than 0",
			interval:    time.Second,
			timeout:     time.Second,
			retries:     -10,
			expectedErr: "Retries in Healthcheck cannot be negative",
		},
		{
			doc:         "test invalid StartPeriod in Healthcheck: not 0 and less than 1ms",
			interval:    time.Second,
			timeout:     time.Second,
			retries:     1000,
			startPeriod: 100 * time.Microsecond,
			expectedErr: fmt.Sprintf("StartPeriod in Healthcheck cannot be less than %s", container.MinimumDuration),
		},
		{
			doc:           "test invalid StartInterval in Healthcheck: not 0 and less than 1ms",
			interval:      time.Second,
			timeout:       time.Second,
			retries:       1000,
			startPeriod:   time.Second,
			startInterval: 100 * time.Microsecond,
			expectedErr:   fmt.Sprintf("StartInterval in Healthcheck cannot be less than %s", container.MinimumDuration),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.doc, func(t *testing.T) {
			t.Parallel()
			ctx := testutil.StartSpan(ctx, t)
			cfg := container.Config{
				Image: "busybox",
				Healthcheck: &container.HealthConfig{
					Interval:      tc.interval,
					Timeout:       tc.timeout,
					Retries:       tc.retries,
					StartInterval: tc.startInterval,
				},
			}
			if tc.startPeriod != 0 {
				cfg.Healthcheck.StartPeriod = tc.startPeriod
			}

			resp, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config: &cfg,
			})
			assert.Check(t, is.Equal(len(resp.Warnings), 0))
			assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
			assert.ErrorContains(t, err, tc.expectedErr)
		})
	}
}

// Make sure that anonymous volumes can be overwritten by tmpfs
// https://github.com/moby/moby/issues/40446
func TestCreateTmpfsOverrideAnonymousVolume(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "windows does not support tmpfs")
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	id := testContainer.Create(ctx, t, apiClient,
		testContainer.WithVolume("/foo"),
		testContainer.WithTmpfs("/foo"),
		testContainer.WithVolume("/bar"),
		testContainer.WithTmpfs("/bar:size=999"),
		testContainer.WithCmd("/bin/sh", "-c", "mount | grep '/foo' | grep tmpfs && mount | grep '/bar' | grep tmpfs"),
	)

	defer func() {
		_, err := apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
		assert.NilError(t, err)
	}()

	inspect, err := apiClient.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	// tmpfs do not currently get added to inspect.Mounts
	// Normally an anonymous volume would, except now tmpfs should prevent that.
	assert.Assert(t, is.Len(inspect.Container.Mounts, 0))

	wait := apiClient.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	_, err = apiClient.ContainerStart(ctx, id, client.ContainerStartOptions{})
	assert.NilError(t, err)

	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()

	select {
	case <-timeout.C:
		t.Fatal("timeout waiting for container to exit")
	case status := <-wait.Result:
		var errMsg string
		if status.Error != nil {
			errMsg = status.Error.Message
		}
		assert.Equal(t, int(status.StatusCode), 0, errMsg)
	case err := <-wait.Error:
		assert.NilError(t, err)
	}
}

// Test that if the referenced image platform does not match the requested platform on container create that we get an
// error.
func TestCreateDifferentPlatform(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	img, err := apiClient.ImageInspect(ctx, "busybox:latest")
	assert.NilError(t, err)
	assert.Assert(t, img.Architecture != "")

	t.Run("different os", func(t *testing.T) {
		ctx := testutil.StartSpan(ctx, t)
		p := ocispec.Platform{
			OS:           img.Os + "DifferentOS",
			Architecture: img.Architecture,
			Variant:      img.Variant,
		}
		_, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config:   &container.Config{Image: "busybox:latest"},
			Platform: &p,
		})
		assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
	})
	t.Run("different cpu arch", func(t *testing.T) {
		ctx := testutil.StartSpan(ctx, t)
		p := ocispec.Platform{
			OS:           img.Os,
			Architecture: img.Architecture + "DifferentArch",
			Variant:      img.Variant,
		}
		_, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config:   &container.Config{Image: "busybox:latest"},
			Platform: &p,
		})
		assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
	})
}

func TestCreateVolumesFromNonExistingContainer(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	_, err := apiClient.ContainerCreate(
		ctx,
		client.ContainerCreateOptions{
			Config:     &container.Config{Image: "busybox"},
			HostConfig: &container.HostConfig{VolumesFrom: []string{"nosuchcontainer"}},
		})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
}

// Test that we can create a container from an image that is for a different platform even if a platform was not specified
// This is for the regression detailed here: https://github.com/moby/moby/issues/41552
func TestCreatePlatformSpecificImageNoPlatform(t *testing.T) {
	ctx := setupTest(t)

	skip.If(t, testEnv.DaemonInfo.Architecture == "arm", "test only makes sense to run on non-arm systems")
	skip.If(t, testEnv.DaemonInfo.OSType != "linux", "test image is only available on linux")
	apiClient := testEnv.APIClient()

	_, err := apiClient.ContainerCreate(
		ctx,
		client.ContainerCreateOptions{
			Config: &container.Config{Image: "arm32v7/hello-world"},
		})
	assert.NilError(t, err)
}

func TestCreateInvalidHostConfig(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testCases := []struct {
		doc         string
		hc          container.HostConfig
		expectedErr string
	}{
		{
			doc:         "invalid IpcMode",
			hc:          container.HostConfig{IpcMode: "invalid"},
			expectedErr: "Error response from daemon: invalid IPC mode: invalid",
		},
		{
			doc:         "invalid PidMode",
			hc:          container.HostConfig{PidMode: "invalid"},
			expectedErr: "Error response from daemon: invalid PID mode: invalid",
		},
		{
			doc:         "invalid PidMode without container ID",
			hc:          container.HostConfig{PidMode: "container"},
			expectedErr: "Error response from daemon: invalid PID mode: container",
		},
		{
			doc:         "invalid UTSMode",
			hc:          container.HostConfig{UTSMode: "invalid"},
			expectedErr: "Error response from daemon: invalid UTS mode: invalid",
		},
		{
			doc:         "invalid Annotations",
			hc:          container.HostConfig{Annotations: map[string]string{"": "a"}},
			expectedErr: "Error response from daemon: invalid Annotations: the empty string is not permitted as an annotation key",
		},
		{
			doc:         "invalid CPUShares",
			hc:          container.HostConfig{Resources: container.Resources{CPUShares: -1}},
			expectedErr: "Error response from daemon: invalid CPU shares (-1): value must be a positive integer",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.doc, func(t *testing.T) {
			t.Parallel()
			ctx := testutil.StartSpan(ctx, t)
			cfg := container.Config{
				Image: "busybox",
			}
			resp, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config:     &cfg,
				HostConfig: &tc.hc,
			})
			assert.Check(t, is.Equal(len(resp.Warnings), 0))
			assert.Check(t, cerrdefs.IsInvalidArgument(err), "got: %T", err)
			assert.Error(t, err, tc.expectedErr)
		})
	}
}

func TestCreateValidation(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		skipOn    string
		expStatus int
		expError  string
	}{
		{
			name:      "empty body",
			body:      ``,
			expStatus: http.StatusBadRequest,
			expError:  `invalid JSON: EOF`, // TODO(thaJeztah): this could use a nicer error message.
		},
		{
			name:      "empty config",
			body:      `{}`,
			expStatus: http.StatusBadRequest,
			expError:  `config cannot be empty in order to create a container`,
		},
		{
			name:      "invalid port syntax", // issue https://github.com/moby/moby/issues/14230 for invalid port syntax
			body:      `{"Image": "busybox", "HostConfig": {"NetworkMode": "default", "PortBindings": {"19039;1230": [{}]}}}`,
			expStatus: http.StatusBadRequest,
			expError:  `invalid JSON: invalid port '19039;1230': invalid syntax`,
		},
		{
			name:      "invalid memory-limit: value too low",
			body:      `{"Image": "busybox", "HostConfig": {"CpuShares": 100, "Memory": 524287}}`,
			skipOn:    "windows", // TODO Windows: Port once memory is supported
			expStatus: http.StatusBadRequest,
			expError:  `Minimum memory limit allowed is 6MB`,
		},
		{
			name:      "invalid restart policy name",
			body:      `{"Image": "busybox", "HostConfig": {"RestartPolicy": {"Name": "something", "MaximumRetryCount": 0}}}`,
			expStatus: http.StatusBadRequest,
			expError:  `invalid restart policy: unknown policy 'something'`,
		},
		{
			name:      "invalid restart policy: retry not allowed",
			body:      `{"Image": "busybox", "HostConfig": {"RestartPolicy": {"Name": "always", "MaximumRetryCount": 2}}}`,
			expStatus: http.StatusBadRequest,
			expError:  `invalid restart policy: maximum retry count can only be used with 'on-failure'`,
		},
		{
			name:      "invalid restart policy: retry negative",
			body:      `{"Image": "busybox", "HostConfig": {"RestartPolicy": {"Name": "on-failure", "MaximumRetryCount": -2}}}`,
			expStatus: http.StatusBadRequest,
			expError:  `invalid restart policy: maximum retry count cannot be negative`,
		},
		{
			name:      "restart policy: default retry count",
			body:      `{"Image": "busybox", "HostConfig": {"RestartPolicy": {"Name": "on-failure", "MaximumRetryCount": 0}}}`,
			expStatus: http.StatusCreated,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			skip.If(t, testEnv.DaemonInfo.OSType == tc.skipOn)
			res, _, err := request.Post(testutil.GetContext(t), "/containers/create", request.RawString(tc.body), request.JSON)
			assert.NilError(t, err)
			assert.Equal(t, res.StatusCode, tc.expStatus)

			if tc.expError != "" {
				var respErr common.ErrorResponse
				assert.NilError(t, request.ReadJSONResponse(res, &respErr))
				assert.ErrorContains(t, respErr, tc.expError)
			}
		})
	}
}

func TestCreateWithMultipleEndpointSettings(t *testing.T) {
	ctx := setupTest(t)

	testcases := []struct {
		apiVersion  string
		expectedErr string
	}{
		{apiVersion: "1.44"},
		{apiVersion: "1.43", expectedErr: "Container cannot be created with multiple network endpoints"},
	}

	for _, tc := range testcases {
		t.Run("with API v"+tc.apiVersion, func(t *testing.T) {
			apiClient, err := client.New(client.FromEnv, client.WithAPIVersion(tc.apiVersion))
			assert.NilError(t, err)

			config := container.Config{
				Image: "busybox",
			}
			networkingConfig := network.NetworkingConfig{
				EndpointsConfig: map[string]*network.EndpointSettings{
					"net1": {},
					"net2": {},
					"net3": {},
				},
			}
			_, err = apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config:           &config,
				NetworkingConfig: &networkingConfig,
			})
			if tc.expectedErr == "" {
				assert.NilError(t, err)
			} else {
				assert.ErrorContains(t, err, tc.expectedErr)
			}
		})
	}
}

func TestCreateWithCustomMACs(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.44"), "requires API v1.44")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	net.CreateNoError(ctx, t, apiClient, "testnet")

	attachCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res := testContainer.RunAttach(attachCtx, t, apiClient,
		testContainer.WithCmd("ip", "-o", "link", "show"),
		testContainer.WithNetworkMode("bridge"),
		testContainer.WithMacAddress("bridge", "02:32:1c:23:00:04"))

	assert.Equal(t, res.ExitCode, 0)
	assert.Equal(t, res.Stderr.String(), "")

	scanner := bufio.NewScanner(res.Stdout)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// The expected output is:
		// 1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue qlen 1000\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
		// 134: eth0@if135: <BROADCAST,MULTICAST,UP,LOWER_UP,M-DOWN> mtu 1400 qdisc noqueue \    link/ether 02:42:ac:11:00:04 brd ff:ff:ff:ff:ff:ff
		if len(fields) < 11 {
			continue
		}

		ifaceName := fields[1]
		if ifaceName[:3] != "eth" {
			continue
		}

		mac := fields[len(fields)-3]
		assert.Equal(t, mac, "02:32:1c:23:00:04")
	}
}

// Tests that when using containerd backed storage the containerd container has the image referenced stored.
func TestContainerdContainerImageInfo(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.46"), "requires API v1.46")

	ctx := setupTest(t)

	apiClient := testEnv.APIClient()
	defer apiClient.Close()

	result, err := apiClient.Info(ctx, client.InfoOptions{})
	assert.NilError(t, err)

	info := result.Info
	skip.If(t, info.Containerd == nil, "requires containerd")

	// Currently a containerd container is only created when the container is started.
	// So start the container and then inspect the containerd container to verify the image info.
	id := testContainer.Run(ctx, t, apiClient, func(cfg *testContainer.TestContainerConfig) {
		// busybox is the default (as of this writing) used by the test client, but lets be explicit here.
		cfg.Config.Image = "busybox"
	})
	defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

	c8dClient, err := containerd.New(info.Containerd.Address, containerd.WithDefaultNamespace(info.Containerd.Namespaces.Containers))
	assert.NilError(t, err)
	defer c8dClient.Close()

	ctr, err := c8dClient.ContainerService().Get(ctx, id)
	assert.NilError(t, err)

	if testEnv.UsingSnapshotter() {
		assert.Equal(t, ctr.Image, "docker.io/library/busybox:latest")
	} else {
		// This field is not set when not using containerd backed storage.
		assert.Equal(t, ctr.Image, "")
	}
}

// Make sure that the working directory, when created by the daemon, is
// owned by the container's user so that it is writable.
// https://github.com/docker/cli/issues/3464
func TestCreateWorkingDirForUser(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "Windows does not set up the working directory at create time")
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	id := testContainer.Create(ctx, t, apiClient,
		testContainer.WithWorkingDir("/foo"),
		testContainer.WithUser("1000:1000"),
		testContainer.WithCmd("touch", "/foo/bar"),
	)

	defer func() {
		_, err := apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
		assert.NilError(t, err)
	}()

	wait := apiClient.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	_, err := apiClient.ContainerStart(ctx, id, client.ContainerStartOptions{})
	assert.NilError(t, err)

	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()

	select {
	case <-timeout.C:
		t.Fatal("timeout waiting for container to exit")
	case status := <-wait.Result:
		var errMsg string
		if status.Error != nil {
			errMsg = status.Error.Message
		}
		assert.Equal(t, int(status.StatusCode), 0, errMsg)
	case err := <-wait.Error:
		assert.NilError(t, err)
	}
}
