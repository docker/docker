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
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	ctr "github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/internal/test/request"
	"github.com/docker/docker/oci"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/poll"
	"gotest.tools/skip"
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
				"",
			)
			assert.Check(t, is.ErrorContains(err, tc.expectedError))
			assert.Check(t, errdefs.IsNotFound(err))
		})
	}
}

func TestCreateWithInvalidEnv(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()

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
		tc := tc
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			_, err := client.ContainerCreate(context.Background(),
				&container.Config{
					Image: "busybox",
					Env:   []string{tc.env},
				},
				&container.HostConfig{},
				&network.NetworkingConfig{},
				"",
			)
			assert.Check(t, is.ErrorContains(err, tc.expectedError))
			assert.Check(t, errdefs.IsInvalidParameter(err))
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

func TestCreateWithCapabilities(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME: test should be able to run on LCOW")
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"), "Capabilities was added in API v1.40")

	defer setupTest(t)()
	ctx := context.Background()
	clientNew := request.NewAPIClient(t)
	clientOld := request.NewAPIClient(t, client.WithVersion("1.39"))

	testCases := []struct {
		doc           string
		hostConfig    container.HostConfig
		expected      []string
		expectedError string
		oldClient     bool
	}{
		{
			doc:        "no capabilities",
			hostConfig: container.HostConfig{},
		},
		{
			doc: "empty capabilities",
			hostConfig: container.HostConfig{
				Capabilities: []string{},
			},
			expected: []string{},
		},
		{
			doc: "valid capabilities",
			hostConfig: container.HostConfig{
				Capabilities: []string{"CAP_NET_RAW", "CAP_SYS_CHROOT"},
			},
			expected: []string{"CAP_NET_RAW", "CAP_SYS_CHROOT"},
		},
		{
			doc: "invalid capabilities",
			hostConfig: container.HostConfig{
				Capabilities: []string{"NET_RAW"},
			},
			expectedError: `invalid Capabilities: unknown capability: "NET_RAW"`,
		},
		{
			doc: "duplicate capabilities",
			hostConfig: container.HostConfig{
				Capabilities: []string{"CAP_SYS_NICE", "CAP_SYS_NICE"},
			},
			expected: []string{"CAP_SYS_NICE", "CAP_SYS_NICE"},
		},
		{
			doc: "capabilities API v1.39",
			hostConfig: container.HostConfig{
				Capabilities: []string{"CAP_NET_RAW", "CAP_SYS_CHROOT"},
			},
			expected:  nil,
			oldClient: true,
		},
		{
			doc: "empty capadd",
			hostConfig: container.HostConfig{
				Capabilities: []string{"CAP_NET_ADMIN"},
				CapAdd:       []string{},
			},
			expected: []string{"CAP_NET_ADMIN"},
		},
		{
			doc: "empty capdrop",
			hostConfig: container.HostConfig{
				Capabilities: []string{"CAP_NET_ADMIN"},
				CapDrop:      []string{},
			},
			expected: []string{"CAP_NET_ADMIN"},
		},
		{
			doc: "capadd capdrop",
			hostConfig: container.HostConfig{
				CapAdd:  []string{"SYS_NICE", "CAP_SYS_NICE"},
				CapDrop: []string{"SYS_NICE", "CAP_SYS_NICE"},
			},
		},
		{
			doc: "conflict with capadd",
			hostConfig: container.HostConfig{
				Capabilities: []string{"CAP_NET_ADMIN"},
				CapAdd:       []string{"SYS_NICE"},
			},
			expectedError: `conflicting options: Capabilities and CapAdd`,
		},
		{
			doc: "conflict with capdrop",
			hostConfig: container.HostConfig{
				Capabilities: []string{"CAP_NET_ADMIN"},
				CapDrop:      []string{"NET_RAW"},
			},
			expectedError: `conflicting options: Capabilities and CapDrop`,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.doc, func(t *testing.T) {
			t.Parallel()
			client := clientNew
			if tc.oldClient {
				client = clientOld
			}

			c, err := client.ContainerCreate(context.Background(),
				&container.Config{Image: "busybox"},
				&tc.hostConfig,
				&network.NetworkingConfig{},
				"",
			)
			if tc.expectedError == "" {
				assert.NilError(t, err)
				ci, err := client.ContainerInspect(ctx, c.ID)
				assert.NilError(t, err)
				assert.Check(t, ci.HostConfig != nil)
				assert.DeepEqual(t, tc.expected, ci.HostConfig.Capabilities)
			} else {
				assert.ErrorContains(t, err, tc.expectedError)
				assert.Check(t, errdefs.IsInvalidParameter(err))
			}
		})
	}
}

func TestCreateWithCustomReadonlyPaths(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	testCases := []struct {
		doc           string
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

			resp, err := client.ContainerCreate(ctx, &cfg, &container.HostConfig{}, nil, "")
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
