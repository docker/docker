package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	ctr "github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/oci"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestCreateFailsWhenIdentifierDoesNotExist(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()

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
		tc := tc
		t.Run(tc.doc, func(t *testing.T) {
			t.Parallel()
			_, err := client.ContainerCreate(context.Background(),
				&container.Config{Image: tc.image},
				&container.HostConfig{},
				&network.NetworkingConfig{},
				nil,
				"",
			)
			assert.Check(t, is.ErrorContains(err, tc.expectedError))
			assert.Check(t, errdefs.IsNotFound(err))
		})
	}
}

// TestCreateLinkToNonExistingContainer verifies that linking to a non-existing
// container returns an "invalid parameter" (400) status, and not the underlying
// "non exists" (404).
func TestCreateLinkToNonExistingContainer(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "legacy links are not supported on windows")
	defer setupTest(t)()
	c := testEnv.APIClient()

	_, err := c.ContainerCreate(context.Background(),
		&container.Config{
			Image: "busybox",
		},
		&container.HostConfig{
			Links: []string{"no-such-container"},
		},
		&network.NetworkingConfig{},
		nil,
		"",
	)
	assert.Check(t, is.ErrorContains(err, "could not get container for no-such-container"))
	assert.Check(t, errdefs.IsInvalidParameter(err))
}

func TestCreateWithInvalidEnv(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()

	testCases := []struct {
		env             string
		expectedWarning string
	}{
		{
			env:             "",
			expectedWarning: `invalid environment variable: ""`,
		},
		{
			env:             "=",
			expectedWarning: `invalid environment variable: "="`,
		},
		{
			env:             "=foo",
			expectedWarning: `invalid environment variable: "=foo"`,
		},
	}

	for index, tc := range testCases {
		tc := tc
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			resp, err := client.ContainerCreate(context.Background(),
				&container.Config{
					Image: "busybox",
					Env:   []string{tc.env},
				},
				&container.HostConfig{},
				&network.NetworkingConfig{},
				nil,
				"",
			)
			assert.NilError(t, err)
			var found bool
			for _, w := range resp.Warnings {
				if w == tc.expectedWarning {
					found = true
					break
				}
			}
			assert.Check(t, found)
		})
	}
}

// Test case for #30166 (target was not validated)
func TestCreateTmpfsMountsTarget(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	defer setupTest(t)()
	client := testEnv.APIClient()

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
		_, err := client.ContainerCreate(context.Background(),
			&container.Config{
				Image: "busybox",
			},
			&container.HostConfig{
				Tmpfs: map[string]string{tc.target: ""},
			},
			&network.NetworkingConfig{},
			nil,
			"",
		)
		assert.Check(t, is.ErrorContains(err, tc.expectedError))
		assert.Check(t, errdefs.IsInvalidParameter(err))
	}
}

func TestCreateWithCustomMaskedPaths(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	testCases := []struct {
		maskedPaths []string
		expected    []string
	}{
		{
			maskedPaths: []string{},
			expected:    []string{},
		},
		{
			maskedPaths: nil,
			expected:    oci.DefaultSpec().Linux.MaskedPaths,
		},
		{
			maskedPaths: []string{"/proc/kcore", "/proc/keys"},
			expected:    []string{"/proc/kcore", "/proc/keys"},
		},
	}

	checkInspect := func(t *testing.T, ctx context.Context, name string, expected []string) {
		_, b, err := client.ContainerInspectWithRaw(ctx, name, false)
		assert.NilError(t, err)

		var inspectJSON map[string]interface{}
		err = json.Unmarshal(b, &inspectJSON)
		assert.NilError(t, err)

		cfg, ok := inspectJSON["HostConfig"].(map[string]interface{})
		assert.Check(t, is.Equal(true, ok), name)

		maskedPaths, ok := cfg["MaskedPaths"].([]interface{})
		assert.Check(t, is.Equal(true, ok), name)

		mps := []string{}
		for _, mp := range maskedPaths {
			mps = append(mps, mp.(string))
		}

		assert.DeepEqual(t, expected, mps)
	}

	for i, tc := range testCases {
		name := fmt.Sprintf("create-masked-paths-%d", i)
		config := container.Config{
			Image: "busybox",
			Cmd:   []string{"true"},
		}
		hc := container.HostConfig{}
		if tc.maskedPaths != nil {
			hc.MaskedPaths = tc.maskedPaths
		}

		// Create the container.
		c, err := client.ContainerCreate(context.Background(),
			&config,
			&hc,
			&network.NetworkingConfig{},
			nil,
			name,
		)
		assert.NilError(t, err)

		checkInspect(t, ctx, name, tc.expected)

		// Start the container.
		err = client.ContainerStart(ctx, c.ID, types.ContainerStartOptions{})
		assert.NilError(t, err)

		poll.WaitOn(t, ctr.IsInState(ctx, client, c.ID, "exited"), poll.WithDelay(100*time.Millisecond))

		checkInspect(t, ctx, name, tc.expected)
	}
}

func TestCreateWithCustomReadonlyPaths(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	testCases := []struct {
		readonlyPaths []string
		expected      []string
	}{
		{
			readonlyPaths: []string{},
			expected:      []string{},
		},
		{
			readonlyPaths: nil,
			expected:      oci.DefaultSpec().Linux.ReadonlyPaths,
		},
		{
			readonlyPaths: []string{"/proc/asound", "/proc/bus"},
			expected:      []string{"/proc/asound", "/proc/bus"},
		},
	}

	checkInspect := func(t *testing.T, ctx context.Context, name string, expected []string) {
		_, b, err := client.ContainerInspectWithRaw(ctx, name, false)
		assert.NilError(t, err)

		var inspectJSON map[string]interface{}
		err = json.Unmarshal(b, &inspectJSON)
		assert.NilError(t, err)

		cfg, ok := inspectJSON["HostConfig"].(map[string]interface{})
		assert.Check(t, is.Equal(true, ok), name)

		readonlyPaths, ok := cfg["ReadonlyPaths"].([]interface{})
		assert.Check(t, is.Equal(true, ok), name)

		rops := []string{}
		for _, rop := range readonlyPaths {
			rops = append(rops, rop.(string))
		}
		assert.DeepEqual(t, expected, rops)
	}

	for i, tc := range testCases {
		name := fmt.Sprintf("create-readonly-paths-%d", i)
		config := container.Config{
			Image: "busybox",
			Cmd:   []string{"true"},
		}
		hc := container.HostConfig{}
		if tc.readonlyPaths != nil {
			hc.ReadonlyPaths = tc.readonlyPaths
		}

		// Create the container.
		c, err := client.ContainerCreate(context.Background(),
			&config,
			&hc,
			&network.NetworkingConfig{},
			nil,
			name,
		)
		assert.NilError(t, err)

		checkInspect(t, ctx, name, tc.expected)

		// Start the container.
		err = client.ContainerStart(ctx, c.ID, types.ContainerStartOptions{})
		assert.NilError(t, err)

		poll.WaitOn(t, ctr.IsInState(ctx, client, c.ID, "exited"), poll.WithDelay(100*time.Millisecond))

		checkInspect(t, ctx, name, tc.expected)
	}
}

func TestCreateWithInvalidHealthcheckParams(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	testCases := []struct {
		doc         string
		interval    time.Duration
		timeout     time.Duration
		retries     int
		startPeriod time.Duration
		expectedErr string
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
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.doc, func(t *testing.T) {
			t.Parallel()
			cfg := container.Config{
				Image: "busybox",
				Healthcheck: &container.HealthConfig{
					Interval: tc.interval,
					Timeout:  tc.timeout,
					Retries:  tc.retries,
				},
			}
			if tc.startPeriod != 0 {
				cfg.Healthcheck.StartPeriod = tc.startPeriod
			}

			resp, err := client.ContainerCreate(ctx, &cfg, &container.HostConfig{}, nil, nil, "")
			assert.Check(t, is.Equal(len(resp.Warnings), 0))

			if versions.LessThan(testEnv.DaemonAPIVersion(), "1.32") {
				assert.Check(t, errdefs.IsSystem(err))
			} else {
				assert.Check(t, errdefs.IsInvalidParameter(err))
			}
			assert.ErrorContains(t, err, tc.expectedErr)
		})
	}
}

// Make sure that anonymous volumes can be overritten by tmpfs
// https://github.com/moby/moby/issues/40446
func TestCreateTmpfsOverrideAnonymousVolume(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "windows does not support tmpfs")
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	id := ctr.Create(ctx, t, client,
		ctr.WithVolume("/foo"),
		ctr.WithTmpfs("/foo"),
		ctr.WithVolume("/bar"),
		ctr.WithTmpfs("/bar:size=999"),
		ctr.WithCmd("/bin/sh", "-c", "mount | grep '/foo' | grep tmpfs && mount | grep '/bar' | grep tmpfs"),
	)

	defer func() {
		err := client.ContainerRemove(ctx, id, types.ContainerRemoveOptions{Force: true})
		assert.NilError(t, err)
	}()

	inspect, err := client.ContainerInspect(ctx, id)
	assert.NilError(t, err)
	// tmpfs do not currently get added to inspect.Mounts
	// Normally an anonymous volume would, except now tmpfs should prevent that.
	assert.Assert(t, is.Len(inspect.Mounts, 0))

	chWait, chErr := client.ContainerWait(ctx, id, container.WaitConditionNextExit)
	assert.NilError(t, client.ContainerStart(ctx, id, types.ContainerStartOptions{}))

	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()

	select {
	case <-timeout.C:
		t.Fatal("timeout waiting for container to exit")
	case status := <-chWait:
		var errMsg string
		if status.Error != nil {
			errMsg = status.Error.Message
		}
		assert.Equal(t, int(status.StatusCode), 0, errMsg)
	case err := <-chErr:
		assert.NilError(t, err)
	}
}

// Test that if the referenced image platform does not match the requested platform on container create that we get an
// error.
func TestCreateDifferentPlatform(t *testing.T) {
	defer setupTest(t)()
	c := testEnv.APIClient()
	ctx := context.Background()

	img, _, err := c.ImageInspectWithRaw(ctx, "busybox:latest")
	assert.NilError(t, err)
	assert.Assert(t, img.Architecture != "")

	t.Run("different os", func(t *testing.T) {
		p := specs.Platform{
			OS:           img.Os + "DifferentOS",
			Architecture: img.Architecture,
			Variant:      img.Variant,
		}
		_, err := c.ContainerCreate(ctx, &containertypes.Config{Image: "busybox:latest"}, &containertypes.HostConfig{}, nil, &p, "")
		assert.Assert(t, client.IsErrNotFound(err), err)
	})
	t.Run("different cpu arch", func(t *testing.T) {
		p := specs.Platform{
			OS:           img.Os,
			Architecture: img.Architecture + "DifferentArch",
			Variant:      img.Variant,
		}
		_, err := c.ContainerCreate(ctx, &containertypes.Config{Image: "busybox:latest"}, &containertypes.HostConfig{}, nil, &p, "")
		assert.Assert(t, client.IsErrNotFound(err), err)
	})
}

func TestCreateVolumesFromNonExistingContainer(t *testing.T) {
	defer setupTest(t)()
	cli := testEnv.APIClient()

	_, err := cli.ContainerCreate(
		context.Background(),
		&container.Config{Image: "busybox"},
		&container.HostConfig{VolumesFrom: []string{"nosuchcontainer"}},
		nil,
		nil,
		"",
	)
	assert.Check(t, errdefs.IsInvalidParameter(err))
}
