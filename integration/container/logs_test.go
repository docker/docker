package container // import "github.com/docker/docker/integration/container"

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/daemon/logger/local"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/integration/internal/termtest"
	"github.com/docker/docker/pkg/stdcopy"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// Regression test for #35370
// Makes sure that when following we don't get an EOF error when there are no logs
func TestLogsFollowTailEmpty(t *testing.T) {
	// FIXME(vdemeester) fails on a e2e run on linux...
	skip.If(t, testEnv.IsRemoteDaemon)
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	id := container.Run(ctx, t, apiClient, container.WithCmd("sleep", "100000"))

	logs, err := apiClient.ContainerLogs(ctx, id, containertypes.LogsOptions{ShowStdout: true, Tail: "2"})
	if logs != nil {
		defer logs.Close()
	}
	assert.Check(t, err)

	_, err = stdcopy.StdCopy(io.Discard, io.Discard, logs)
	assert.Check(t, err)
}

func TestLogs(t *testing.T) {
	drivers := []string{local.Name, jsonfilelog.Name}

	for _, logDriver := range drivers {
		t.Run("driver "+logDriver, func(t *testing.T) {
			testLogs(t, logDriver)
		})
	}
}

func testLogs(t *testing.T, logDriver string) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testCases := []struct {
		desc        string
		logOps      containertypes.LogsOptions
		expectedOut string
		expectedErr string
		tty         bool
	}{
		// TTY, only one output stream
		{
			desc: "tty/stdout and stderr",
			tty:  true,
			logOps: containertypes.LogsOptions{
				ShowStdout: true,
				ShowStderr: true,
			},
			expectedOut: "this is fineaccidents happen",
		},
		{
			desc: "tty/only stdout",
			tty:  true,
			logOps: containertypes.LogsOptions{
				ShowStdout: true,
				ShowStderr: false,
			},
			expectedOut: "this is fineaccidents happen",
		},
		{
			desc: "tty/only stderr",
			tty:  true,
			logOps: containertypes.LogsOptions{
				ShowStdout: false,
				ShowStderr: true,
			},
			expectedOut: "",
		},
		// Without TTY, both stdout and stderr
		{
			desc: "without tty/stdout and stderr",
			tty:  false,
			logOps: containertypes.LogsOptions{
				ShowStdout: true,
				ShowStderr: true,
			},
			expectedOut: "this is fine",
			expectedErr: "accidents happen",
		},
		{
			desc: "without tty/only stdout",
			tty:  false,
			logOps: containertypes.LogsOptions{
				ShowStdout: true,
				ShowStderr: false,
			},
			expectedOut: "this is fine",
			expectedErr: "",
		},
		{
			desc: "without tty/only stderr",
			tty:  false,
			logOps: containertypes.LogsOptions{
				ShowStdout: false,
				ShowStderr: true,
			},
			expectedOut: "",
			expectedErr: "accidents happen",
		},
	}

	pollTimeout := time.Second * 10
	if testEnv.DaemonInfo.OSType == "windows" {
		pollTimeout = StopContainerWindowsPollTimeout
	}

	for _, tC := range testCases {
		tC := tC
		t.Run(tC.desc, func(t *testing.T) {
			t.Parallel()
			tty := tC.tty
			id := container.Run(ctx, t, apiClient,
				container.WithCmd("sh", "-c", "echo -n this is fine; echo -n accidents happen >&2"),
				container.WithTty(tty),
				container.WithLogDriver(logDriver),
				// Logs do not need networking, and Windows HNS endpoint
				// setup is sensitive to these parallel short-lived starts.
				container.WithNetworkMode("none"),
			)
			defer apiClient.ContainerRemove(ctx, id, containertypes.RemoveOptions{Force: true})

			poll.WaitOn(t, container.IsStopped(ctx, apiClient, id),
				poll.WithDelay(time.Millisecond*100),
				poll.WithTimeout(pollTimeout))

			logs, err := apiClient.ContainerLogs(ctx, id, tC.logOps)
			assert.NilError(t, err)
			defer logs.Close()

			var stdout, stderr bytes.Buffer
			if tty {
				// TTY, only one output stream
				_, err = io.Copy(&stdout, logs)
			} else {
				_, err = stdcopy.StdCopy(&stdout, &stderr, logs)
			}
			assert.NilError(t, err)

			stdoutStr := stdout.String()

			if tty && testEnv.DaemonInfo.OSType == "windows" {
				stdoutStr = stripEscapeCodes(t, stdoutStr)

				// Special case for Windows Server 2019
				// Check only that the raw output stream contains strings
				// that were printed to container's stdout and stderr.
				// This is a workaround for the backspace being outputted in an unexpected place
				// which breaks the parsed output: https://github.com/moby/moby/issues/43710
				if strings.Contains(testEnv.DaemonInfo.OperatingSystem, "Windows Server Version 1809") {
					if tC.logOps.ShowStdout {
						assert.Check(t, cmp.Contains(stdout.String(), "this is fine"))
						assert.Check(t, cmp.Contains(stdout.String(), "accidents happen"))
					} else {
						assert.DeepEqual(t, stdoutStr, "")
					}
					return
				}
			}

			assert.DeepEqual(t, stdoutStr, tC.expectedOut)
			assert.DeepEqual(t, stderr.String(), tC.expectedErr)
		})
	}
}

// This hack strips the escape codes that appear in the Windows TTY output and don't have
// any effect on the text content.
// This doesn't handle all escape sequences, only ones that were encountered during testing.
func stripEscapeCodes(t *testing.T, input string) string {
	t.Logf("Stripping: %q\n", input)
	output, err := termtest.StripANSICommands(input)
	assert.NilError(t, err)
	return output
}
