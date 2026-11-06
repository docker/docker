package container

import (
	"fmt"
	"strings"
	"testing"

	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/moby/moby/v2/integration/internal/container"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

func TestExecConsoleSize(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.42"), "requires API v1.42")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithImage("busybox"))

	result, err := container.Exec(ctx, apiClient, cID, []string{"stty", "size"},
		func(ec *client.ExecCreateOptions) {
			ec.TTY = true
			ec.ConsoleSize = client.ConsoleSize{
				Height: 57,
				Width:  123,
			}
		},
	)

	assert.NilError(t, err)
	assert.Equal(t, strings.TrimSpace(result.Stdout()), "57 123")
}

func TestFailedExecExitCode(t *testing.T) {
	testCases := []struct {
		doc              string
		command          []string
		expectedExitCode int
	}{
		{
			doc:              "executable not found",
			command:          []string{"nonexistent"},
			expectedExitCode: 127,
		},
		{
			doc:              "executable cannot be invoked",
			command:          []string{"/etc"},
			expectedExitCode: 126,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.doc, func(t *testing.T) {
			ctx := setupTest(t)
			apiClient := testEnv.APIClient()

			cID := container.Run(ctx, t, apiClient)

			result, err := container.Exec(ctx, apiClient, cID, tc.command)
			assert.NilError(t, err)

			assert.Equal(t, result.ExitCode, tc.expectedExitCode)
		})
	}
}

func TestContainerExecDefaultUmask(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	users := []string{"", "1000", "nobody"}
	prettyUser := func(s string) string {
		if s == "" {
			return "unspecified"
		}
		return s
	}

	for _, runUser := range users {
		for _, execUser := range users {
			t.Run(fmt.Sprintf("user_%s_%s", prettyUser(runUser), prettyUser(execUser)), func(t *testing.T) {
				cID := container.Run(ctx, t, apiClient, container.WithUser(runUser))
				result, err := container.Exec(ctx, apiClient, cID, []string{"sh", "-c", "umask"}, func(ec *client.ExecCreateOptions) {
					ec.User = execUser
				})
				assert.NilError(t, err)
				assert.Equal(t, "0022", strings.TrimSpace(result.Stdout()))
			})
		}
	}
}

func TestContainerExecWithUmask(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.56"), "requires API v1.56")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	users := []string{"", "1000", "nobody"}
	prettyUser := func(s string) string {
		if s == "" {
			return "unspecified"
		}
		return s
	}

	tests := []struct {
		umask    uint32
		expected string
	}{
		{
			umask:    0o000,
			expected: "0000",
		},
		{
			umask:    0o777,
			expected: "0777",
		},
	}
	prettyUmask := func(u uint32) string {
		return fmt.Sprintf("%04o", u)
	}

	for _, runUser := range users {
		for _, execUser := range users {
			for _, tc := range tests {
				t.Run(fmt.Sprintf("user_%s_%s_umask_%s", prettyUser(runUser), prettyUser(execUser), prettyUmask(tc.umask)), func(t *testing.T) {
					cID := container.Run(ctx, t, apiClient, container.WithUser(runUser), func(c *container.TestContainerConfig) {
						c.HostConfig.Umask = &tc.umask
					})
					result, err := container.Exec(ctx, apiClient, cID, []string{"sh", "-c", "umask"}, func(ec *client.ExecCreateOptions) {
						ec.User = execUser
					})
					assert.NilError(t, err)
					assert.Equal(t, tc.expected, strings.TrimSpace(result.Stdout()))
				})
			}
		}
	}
}
