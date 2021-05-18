package stdio

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	types "github.com/gogo/protobuf/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ StdioService = &server{}

type server struct {
	a      Attacher
	cancel func()
}

// NewServer creates a StdioService implementation wrapping the local attacher.
func NewServer(stdin io.WriteCloser, stdout, stderr io.ReadCloser, cancel func()) StdioService {
	return &server{a: NewLocalAttacher(stdin, stdout, stderr), cancel: cancel}
}

func newMissingFdError(fd *FileDescriptor) error {
	return status.Errorf(codes.InvalidArgument, "output stream fd %s@%d not found", fd.Name, fd.Fileno)
}

func fromFd(fd *FileDescriptor) *os.File {
	return os.NewFile(uintptr(fd.Fileno), fd.Name)
}

type fdImportError struct {
	fds []*FileDescriptor
}

func (e *fdImportError) Error() string {
	buf := &strings.Builder{}
	buf.WriteString("error importing fds: ")
	for _, fd := range e.fds {
		buf.WriteString(fmt.Sprintf("%s@%d ", fd.Name, fd.Fileno))
	}
	return strings.TrimSpace(buf.String())
}

func (s *server) AttachStreams(ctx context.Context, req *AttachStreamsRequest) (_ *types.Empty, retErr error) {
	var (
		stdin, stdout, stderr *os.File
	)
	defer func() {
		if retErr != nil {
			if stdin != nil {
				stdin.Close()
			}
			if stdout != nil {
				stdout.Close()
			}
			if stderr != nil {
				stderr.Close()
			}
		}
	}()

	var badFds []*FileDescriptor
	if req.Stdin != nil {
		stdin = fromFd(req.Stdin)
		if stdin == nil {
			badFds = append(badFds, req.Stdin)
		}
	}

	if req.Stdout != nil {
		stdout = fromFd(req.Stdout)
		if stdout == nil {
			badFds = append(badFds, req.Stdout)
		}
	}

	if req.Stderr != nil {
		stderr = fromFd(req.Stderr)
		if stderr == nil {
			badFds = append(badFds, req.Stderr)
		}
	}

	if len(badFds) > 0 {
		// At this point, all fds that we did import will be closed by the defer above.
		return nil, &fdImportError{badFds}
	}

	if err := s.a.Attach(ctx, stdin, stdout, stderr); err != nil {
		return nil, err
	}

	return &types.Empty{}, nil
}

func (s *server) AttachStreamsMultiplexed(ctx context.Context, req *AttachStreamsMultiplexedRequest) (_ *types.Empty, retErr error) {
	if req.Stream == nil {
		return nil, status.Errorf(codes.InvalidArgument, "missing stream")
	}
	var stream *os.File

	defer func() {
		if retErr != nil {
			stream.Close()
		}
	}()

	stream = fromFd(req.Stream)
	if stream == nil {
		return nil, &fdImportError{[]*FileDescriptor{req.Stream}}
	}

	if err := s.a.AttachMultiplexed(ctx, stream, req.Framing, req.DetachKeys, req.IncludeStdin, req.IncludeStdout, req.IncludeStderr); err != nil {
		return nil, err
	}

	return &types.Empty{}, nil
}

func (s *server) Shutdown(context.Context, *types.Empty) (*types.Empty, error) {
	s.a.Close()
	s.cancel()
	return &types.Empty{}, nil
}
