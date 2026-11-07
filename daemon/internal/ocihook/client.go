package ocihook

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/moby/sys/reexec"
)

const reexecName = "dockerd-oci-hook"

func init() {
	reexec.Register(reexecName, runHook)
}

// runHook is the entrypoint of the hook process. It expects the arguments
// { [0] = reexecName, [1] = <socket path>, [2] = <token> } and the
// container's [specs.State] as JSON on stdin, as the runtime provides it.
// The state is forwarded to the server without decoding it.
func runHook() {
	if err := run(os.Args[1:], os.Stdin); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader) error {
	if len(args) != 2 {
		return fmt.Errorf("%s expects 2 args, received %d", reexecName, len(args))
	}
	sock, token := args[0], args[1]

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, token+"\x00"); err != nil {
		return fmt.Errorf("sending token: %w", err)
	}
	if _, err := io.Copy(conn, stdin); err != nil {
		return fmt.Errorf("sending container state: %w", err)
	}
	// Signal the end of the state, so a truncated state fails to decode
	// instead of leaving the server waiting for more.
	if err := conn.CloseWrite(); err != nil {
		return fmt.Errorf("sending container state: %w", err)
	}

	return readResponse(conn)
}

// readResponse reads the server's response to a hook invocation, and
// returns the error it reports, if any. A connection closed without any
// response, as when the daemon dies, is an error.
func readResponse(r io.Reader) error {
	resp, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	switch {
	case len(resp) == 0:
		return errors.New("connection closed without a response")
	case resp[0] == statusOK && len(resp) == 1:
		return nil
	case resp[0] == statusError:
		return errors.New(string(resp[1:]))
	default:
		return fmt.Errorf("malformed response %q", resp)
	}
}
