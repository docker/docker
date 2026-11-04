package container

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/logger/jsonfilelog"
	"github.com/moby/moby/v2/daemon/logger/local"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/termtest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
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

	logs, err := apiClient.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, Tail: "2"})
	assert.Check(t, err)
	defer logs.Close()

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

	tests := []struct {
		desc        string
		logOps      client.ContainerLogsOptions
		expectedOut string
		expectedErr string
		tty         bool
	}{
		// TTY, only one output stream
		{
			desc: "tty/stdout and stderr",
			tty:  true,
			logOps: client.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: true,
			},
			expectedOut: "this is fineaccidents happen",
		},
		{
			desc: "tty/only stdout",
			tty:  true,
			logOps: client.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: false,
			},
			expectedOut: "this is fineaccidents happen",
		},
		{
			desc: "tty/only stderr",
			tty:  true,
			logOps: client.ContainerLogsOptions{
				ShowStdout: false,
				ShowStderr: true,
			},
			expectedOut: "",
		},
		// Without TTY, both stdout and stderr
		{
			desc: "without tty/stdout and stderr",
			tty:  false,
			logOps: client.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: true,
			},
			expectedOut: "this is fine",
			expectedErr: "accidents happen",
		},
		{
			desc: "without tty/only stdout",
			tty:  false,
			logOps: client.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: false,
			},
			expectedOut: "this is fine",
			expectedErr: "",
		},
		{
			desc: "without tty/only stderr",
			tty:  false,
			logOps: client.ContainerLogsOptions{
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

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			tty := tc.tty
			id := container.Run(ctx, t, apiClient,
				container.WithCmd("sh", "-c", "echo -n this is fine; echo -n accidents happen >&2"),
				container.WithTty(tty),
				container.WithLogDriver(logDriver),
				// Logs do not need networking, and Windows HNS endpoint
				// setup is sensitive to these parallel short-lived starts.
				container.WithNetworkMode("none"),
			)
			defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

			poll.WaitOn(t, container.IsStopped(ctx, apiClient, id), poll.WithTimeout(pollTimeout))

			logs, err := apiClient.ContainerLogs(ctx, id, tc.logOps)
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
					if tc.logOps.ShowStdout {
						assert.Check(t, is.Contains(stdout.String(), "this is fine"))
						assert.Check(t, is.Contains(stdout.String(), "accidents happen"))
					} else {
						assert.DeepEqual(t, stdoutStr, "")
					}
					return
				}
			}

			assert.DeepEqual(t, stdoutStr, tc.expectedOut)
			assert.DeepEqual(t, stderr.String(), tc.expectedErr)
		})
	}
}

// TestLogsUntil verifies that the "until" option of the container logs API
// excludes log entries that are newer than the given time, and that an
// "until" value of "0" returns all logs, the same as omitting the option.
func TestLogsUntil(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	// Sleep before the last line so that it is logged clearly later than
	// the other lines.
	id := container.Run(ctx, t, apiClient,
		container.WithCmd("sh", "-c", "echo log1; echo log2; sleep 1; echo log3"),
		container.WithNetworkMode("none"),
	)

	pollTimeout := 10 * time.Second
	if testEnv.DaemonInfo.OSType == "windows" {
		pollTimeout = StopContainerWindowsPollTimeout
	}
	poll.WaitOn(t, container.IsStopped(ctx, apiClient, id), poll.WithTimeout(pollTimeout))

	readLogs := func(t *testing.T, until string) []string {
		t.Helper()
		logs, err := apiClient.ContainerLogs(ctx, id, client.ContainerLogsOptions{
			ShowStdout: true,
			Timestamps: true,
			Until:      until,
		})
		assert.NilError(t, err)
		defer logs.Close()

		var stdout bytes.Buffer
		_, err = stdcopy.StdCopy(&stdout, io.Discard, logs)
		assert.NilError(t, err)
		return strings.Split(strings.TrimSpace(stdout.String()), "\n")
	}

	// entryTime returns the timestamp that prefixes a log entry.
	entryTime := func(t *testing.T, entry string) time.Time {
		t.Helper()
		ts, _, _ := strings.Cut(entry, " ")
		tm, err := time.Parse(time.RFC3339Nano, ts)
		assert.NilError(t, err)
		return tm
	}

	allLogs := readLogs(t, "")
	assert.Assert(t, is.Len(allLogs, 3))

	t.Run("until before last entry", func(t *testing.T) {
		second, last := entryTime(t, allLogs[1]), entryTime(t, allLogs[2])
		assert.Assert(t, second.Before(last))
		until := second.Add(last.Sub(second) / 2).Format(time.RFC3339Nano)

		assert.Check(t, is.DeepEqual(readLogs(t, until), allLogs[:2]))
	})

	t.Run("until zero", func(t *testing.T) {
		assert.Check(t, is.DeepEqual(readLogs(t, "0"), allLogs))
	})
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
