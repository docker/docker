package container

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mdlayher/socket"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/security"
	"github.com/moby/moby/v2/integration/internal/container"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

// kernelSupportsAFALG reports whether the host kernel has the AF_ALG address
// family registered. Starting with kernel 7.2, AF_ALG is deprecated and may
// be built without CONFIG_CRYPTO_USER_API, so socket creation fails with
// EAFNOSUPPORT before any LSM policy is consulted, which is indistinguishable
// from a denied test result.
func kernelSupportsAFALG() bool {
	fd, err := unix.Socket(unix.AF_ALG, unix.SOCK_SEQPACKET, 0)
	defer unix.Close(fd)
	if err != nil {
		return err != unix.EAFNOSUPPORT
	}
	return true
}

var (
	//go:embed testdata/af_alg.c
	afALGSource string

	//go:embed testdata/af_vsock.c
	afVSOCKSource string

	//go:embed testdata/socketcall.c
	socketcallSource string
)

// compileAndExecSocketDenied writes a C source file into the container,
// compiles it with the given compiler command, runs the binary as uid 1000,
// and asserts that socket creation fails.
func compileAndExecSocketDenied(ctx context.Context, t *testing.T, apiClient client.APIClient, cID string, name string, src string, cc []string) {
	t.Helper()

	binPath := "/tmp/" + name
	srcPath := binPath + ".c"

	res := container.ExecT(ctx, t, apiClient, cID, []string{
		"sh", "-c", "cat > " + srcPath + " << 'CEOF'\n" + src + "\nCEOF",
	})
	res.AssertSuccess(t)

	compileCmd := append(cc, srcPath, "-o", binPath)
	res = container.ExecT(ctx, t, apiClient, cID, compileCmd)
	res.AssertSuccess(t)

	res, err := container.Exec(ctx, apiClient, cID, []string{binPath},
		func(ec *client.ExecCreateOptions) {
			ec.User = "1000"
		},
	)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(res.ExitCode, 1), "expected %s socket program to fail", name)

	out := strings.ToLower(res.Combined())
	assert.Check(t, is.Contains(out, "socket"), "expected socket-related error message")
	// Seccomp returns EPERM ("not permitted"), AppArmor returns EACCES
	// ("permission denied"). Accept either.
	denied := strings.Contains(out, "not permitted") || strings.Contains(out, "permission denied")
	assert.Check(t, denied, "expected EPERM or EACCES, got: %s", res.Combined())
}

// TestExecSocketDenied verifies that AF_ALG and AF_VSOCK sockets cannot be
// created inside a container. AF_ALG is blocked by the default seccomp profile
// (via socket arg filtering) and by the default AppArmor profile (via
// "deny network alg"). Direct AF_VSOCK creation is blocked by seccomp. The
// socketcall compatibility path must either be denied or unable to communicate
// with the host.
func TestExecSocketDenied(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)
	t.Parallel()

	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithImage("debian:trixie-slim"), container.WithCmd("sleep", "infinity"))

	// Install build dependencies as root.
	res := container.ExecT(ctx, t, apiClient, cID, []string{
		"sh", "-c", "apt-get update && apt-get install -y --no-install-recommends gcc libc-dev linux-libc-dev",
	})
	res.AssertSuccess(t)

	gcc := []string{"gcc"}

	arch := testEnv.DaemonInfo.Architecture
	isAmd64 := arch == "amd64" || arch == "x86_64"

	t.Run("AF_ALG", func(t *testing.T) {
		compileAndExecSocketDenied(ctx, t, apiClient, cID, "AF_ALG", afALGSource, gcc)
	})
	t.Run("AF_VSOCK", func(t *testing.T) {
		compileAndExecSocketDenied(ctx, t, apiClient, cID, "AF_VSOCK", afVSOCKSource, gcc)
	})

	// Test socketcall(2) via int $0x80 to invoke the ia32 compat syscall
	// path from a native 64-bit binary. MAP_32BIT is used to place the
	// args array below 4 GB since the ia32 compat path truncates all
	// registers to 32 bits.
	//
	// The socketcall binary is compiled with -DSOCK_FAMILY and -DSOCK_TYPE
	// to set the address family and socket type at compile time.
	t.Run("socketcall_int80", func(t *testing.T) {
		skip.If(t, !isAmd64, "int $0x80 ia32 compat only available on amd64")
		// Seccomp cannot filter socketcall arguments (the address family
		// is behind a userspace pointer). Only an LSM (AppArmor or
		// SELinux) can deny AF_ALG via the security_socket_create hook.
		securityOptions := security.DecodeOptions(testEnv.DaemonInfo.SecurityOptions)
		hasAppArmor := slices.ContainsFunc(securityOptions, func(option security.Option) bool {
			return option.Name == "apparmor"
		})
		hasSeLinux := slices.ContainsFunc(securityOptions, func(option security.Option) bool {
			return option.Name == "selinux"
		})

		srcPath := "/tmp/socketcall.c"
		res := container.ExecT(ctx, t, apiClient, cID, []string{
			"sh", "-c", "cat > " + srcPath + " << 'CEOF'\n" + socketcallSource + "\nCEOF",
		})
		res.AssertSuccess(t)

		hasLSM := hasAppArmor || hasSeLinux
		// AF_ALG (38) via socketcall must be denied by the LSM
		// (AppArmor's "deny network alg" or SELinux's alg_socket deny),
		// which catches it at the security_socket_create hook even
		// though seccomp cannot filter socketcall args.
		t.Run("AF_ALG", func(t *testing.T) {
			if testEnv.IsLocalDaemon() {
				skip.If(t, !kernelSupportsAFALG(), "host kernel does not support AF_ALG")
			}
			skip.If(t, !hasLSM, "socketcall filtering requires AppArmor or SELinux")

			binPath := "/tmp/socketcall_af_alg"
			res := container.ExecT(ctx, t, apiClient, cID, append(gcc,
				"-DSOCK_FAMILY=AF_ALG", "-DSOCK_TYPE=SOCK_SEQPACKET",
				"-include", "linux/if_alg.h",
				srcPath, "-o", binPath,
			))
			res.AssertSuccess(t)

			res, err := container.Exec(ctx, apiClient, cID, []string{binPath},
				func(ec *client.ExecCreateOptions) {
					ec.User = "1000"
				},
			)
			assert.NilError(t, err)
			assert.Check(t, is.Equal(res.ExitCode, 1), "expected AF_ALG socketcall to fail, got: %s", res.Combined())
			assert.Check(t, is.Contains(strings.ToLower(res.Combined()), "permission denied"))
		})

		// A denied AF_VSOCK socket is sufficient. If socket creation is
		// permitted, verify that the socket cannot communicate with the host.
		t.Run("AF_VSOCK", func(t *testing.T) {
			skip.If(t, !hasLSM, "socketcall filtering requires AppArmor or SELinux")
			const probePath = "/tmp/socketcall_af_vsock_probe"
			res := container.ExecT(ctx, t, apiClient, cID, append(gcc,
				"-DSOCK_FAMILY=AF_VSOCK", "-DSOCK_TYPE=SOCK_STREAM",
				"-include", "linux/vm_sockets.h",
				srcPath, "-o", probePath,
			))
			res.AssertSuccess(t)

			res, err := container.Exec(ctx, apiClient, cID, []string{probePath},
				func(ec *client.ExecCreateOptions) {
					ec.User = "1000"
				},
			)
			assert.NilError(t, err)
			probeOutput := res.Combined()
			if res.ExitCode == 1 {
				out := strings.ToLower(probeOutput)
				denied := strings.Contains(out, "socket") &&
					(strings.Contains(out, "not permitted") || strings.Contains(out, "permission denied"))
				assert.Assert(t, denied, "unexpected AF_VSOCK creation failure: %s", probeOutput)
				return
			}
			assert.Assert(t, res.ExitCode == 0, "unexpected AF_VSOCK creation probe exit code %d: %s", res.ExitCode, probeOutput)

			skip.If(t, testEnv.IsRemoteDaemon, "communication check requires a local daemon")

			const (
				clientPayload         = "client-to-listener"
				serverPayload         = "listener-to-client"
				connectDeniedExitCode = 3
			)

			listener, err := socket.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0, "vsock", nil)
			if err != nil {
				t.Skipf("AF_VSOCK listener unavailable: %v", err)
			}
			defer listener.Close()

			if err := listener.Bind(&unix.SockaddrVM{
				CID:  unix.VMADDR_CID_LOCAL,
				Port: unix.VMADDR_PORT_ANY,
			}); err != nil {
				t.Skipf("AF_VSOCK listener unavailable: %v", err)
			}
			if err := listener.Listen(unix.SOMAXCONN); err != nil {
				t.Skipf("AF_VSOCK listener unavailable: %v", err)
			}

			addr, err := listener.Getsockname()
			if err != nil {
				t.Skipf("AF_VSOCK listener unavailable: %v", err)
			}
			vmAddr, ok := addr.(*unix.SockaddrVM)
			if !ok {
				t.Fatalf("unexpected AF_VSOCK listener address type %T", addr)
			}

			binPath := "/tmp/socketcall_af_vsock_connect"
			res = container.ExecT(ctx, t, apiClient, cID, append(gcc,
				"-DSOCK_FAMILY=AF_VSOCK", "-DSOCK_TYPE=SOCK_STREAM",
				"-DVSOCK_CONNECT", "-DPORT="+strconv.FormatUint(uint64(vmAddr.Port), 10),
				"-DCONNECT_DENIED_EXIT_CODE="+strconv.Itoa(connectDeniedExitCode),
				`-DCLIENT_PAYLOAD="`+clientPayload+`"`,
				`-DEXPECTED_SERVER_PAYLOAD="`+serverPayload+`"`,
				"-include", "linux/vm_sockets.h",
				srcPath, "-o", binPath,
			))
			res.AssertSuccess(t)

			type exchangeResult struct {
				accepted bool
				details  string
				err      error
			}

			deadline := time.Now().Add(10 * time.Second)
			if err := listener.SetDeadline(deadline); err != nil {
				t.Skipf("cannot set AF_VSOCK listener deadline: %v", err)
			}
			serverResult := make(chan exchangeResult, 1)
			go func() {
				conn, _, err := listener.Accept(t.Context(), 0)
				if err != nil {
					serverResult <- exchangeResult{err: fmt.Errorf("accept AF_VSOCK connection: %w", err)}
					return
				}
				defer conn.Close()

				result := exchangeResult{accepted: true, details: "listener accepted AF_VSOCK connection"}
				if err := conn.SetDeadline(deadline); err != nil {
					result.err = fmt.Errorf("set AF_VSOCK connection deadline: %w", err)
					serverResult <- result
					return
				}

				payload := make([]byte, len(clientPayload))
				n, err := io.ReadFull(conn, payload)
				result.details += fmt.Sprintf("; client payload read %d/%d bytes: %q", n, len(clientPayload), payload[:n])
				if err != nil {
					result.err = fmt.Errorf("read client payload: %w", err)
					serverResult <- result
					return
				}
				if string(payload) != clientPayload {
					result.err = fmt.Errorf("client payload mismatch: got %q, want %q", payload, clientPayload)
					serverResult <- result
					return
				}

				n64, err := io.Copy(conn, strings.NewReader(serverPayload))
				result.details += fmt.Sprintf("; server payload wrote %d/%d bytes: %q", n64, len(serverPayload), serverPayload[:n64])
				if err != nil {
					result.err = fmt.Errorf("write server payload: %w", err)
				} else if n64 != int64(len(serverPayload)) {
					result.err = fmt.Errorf("short server payload write: wrote %d of %d bytes", n64, len(serverPayload))
				}
				serverResult <- result
			}()

			execCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			res, execErr := container.Exec(execCtx, apiClient, cID, []string{binPath},
				func(ec *client.ExecCreateOptions) {
					ec.User = "1000"
				},
			)
			cancel()
			_ = listener.Close()
			exchange := <-serverResult

			clientOutput := ""
			clientExitCode := -1
			if execErr == nil {
				clientOutput = res.Combined()
				clientExitCode = res.ExitCode
			}
			if exchange.accepted {
				t.Fatalf("AF_VSOCK communication reached the host listener\nexchange: %s\nexchange error: %v\nclient exit code: %d\nclient output: %s\nclient exec error: %v", exchange.details, exchange.err, clientExitCode, clientOutput, execErr)
			}

			assert.NilError(t, execErr, "listener result: %v; client output: %s", exchange.err, clientOutput)
			assert.Assert(t, res.ExitCode == connectDeniedExitCode, "expected AF_VSOCK connect to fail with EPERM or EACCES; listener result: %v; client output: %s", exchange.err, clientOutput)
		})

		// AF_INET via socketcall must still work to ensure the deny
		// rule is targeted and does not break legitimate usage.
		t.Run("AF_INET", func(t *testing.T) {
			binPath := "/tmp/socketcall_af_inet"
			res := container.ExecT(ctx, t, apiClient, cID, append(gcc,
				"-DSOCK_FAMILY=AF_INET", "-DSOCK_TYPE=SOCK_STREAM",
				srcPath, "-o", binPath,
			))
			res.AssertSuccess(t)

			res, err := container.Exec(ctx, apiClient, cID, []string{binPath},
				func(ec *client.ExecCreateOptions) {
					ec.User = "1000"
				},
			)
			assert.NilError(t, err)
			assert.Check(t, is.Equal(res.ExitCode, 0), "expected AF_INET socketcall to succeed, got: %s", res.Combined())
		})
	})
}
