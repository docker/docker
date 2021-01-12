// +build !windows

package stdio

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	strings "strings"

	"github.com/containerd/fifo"
	"github.com/containerd/ttrpc"
	"github.com/docker/docker/pkg/reexec"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func init() {
	// TODO: DO NOT MERGE: This needs to be a dedicated binary
	// This is here for convenience while testing.
	//
	// The reason it should be dedicated is to reduce memory overhead of docker-stdio since it is per container.
	reexec.Register("docker-stdio", RunCommand)
}

type stringSliceFlag struct {
	values []string
}

func (s *stringSliceFlag) Set(name string) error {
	s.values = append(s.values, name)
	return nil
}

func (s *stringSliceFlag) String() string {
	return "[]"
}

func dumpStack() {
	var (
		buf       []byte
		stackSize int
	)
	bufferLen := 16384
	for stackSize == len(buf) {
		buf = make([]byte, bufferLen)
		stackSize = runtime.Stack(buf, true)
		bufferLen *= 2
	}
	buf = buf[:stackSize]
	fmt.Fprintln(os.Stderr, string(buf))
}

func RunCommand() {
	var (
		rpcAddr    string
		fdAddr     string
		stdinPath  string
		stdoutPath string
		stderrPath string
	)

	flag.StringVar(&rpcAddr, "rpc-addr", "", "Path to listen for connections to attach streams")
	flag.StringVar(&fdAddr, "fd-addr", "", "Path to listen for connections to attach streams")
	flag.StringVar(&stdinPath, "stdin", "", "Path to the container's stdin pipe")
	flag.StringVar(&stdoutPath, "stdout", "", "Path to the container's stdout pipe")
	flag.StringVar(&stderrPath, "stderr", "", "Path to the container's stderr pipe")
	flag.Parse()

	if rpcAddr == "" {
		panic("rpc-addr must not be set")
	}
	if fdAddr == "" {
		panic("fd-adrr must not be set")
	}

	ctx, cancel := context.WithCancel(context.Background())

	chSig := make(chan os.Signal, 3)
	signal.Notify(chSig, unix.SIGTERM, unix.SIGINT, unix.SIGUSR1)
	go func() {
		for s := range chSig {
			logrus.WithField("signal", s).Debug("Caught signal")
			switch s {
			case unix.SIGUSR1:
				dumpStack()
			default:
				cancel()
			}
		}
	}()

	var (
		stdin          io.WriteCloser
		stdout, stderr io.ReadCloser
		err            error
	)

	logrus.SetLevel(logrus.DebugLevel)
	logrus.SetOutput(os.Stderr)

	if stdinPath != "" {
		stdin, err = fifo.OpenFifo(ctx, stdinPath, unix.O_WRONLY|unix.O_NONBLOCK, 0600)
		if err != nil {
			logrus.WithError(err).Error("Error opening stdin pipe")
			os.Exit(1)
		}
		defer stdin.Close()
	}

	if stdoutPath != "" {
		stdout, err = fifo.OpenFifo(ctx, stdoutPath, unix.O_RDONLY|unix.O_NONBLOCK, 0600)
		if err != nil {
			logrus.WithError(err).Error("Error opening stdout pipe")
			os.Exit(1)
		}
		defer stdout.Close()
	}
	if stderrPath != "" {
		stderr, err = fifo.OpenFifo(ctx, stderrPath, unix.O_RDONLY|unix.O_NONBLOCK, 0600)
		if err != nil {
			logrus.WithError(err).Error("Error opening stderr pipe", err)
			os.Exit(1)
		}
		defer stderr.Close()
	}

	if err := os.MkdirAll(filepath.Dir(rpcAddr), 0700); err != nil {
		logrus.WithError(err).Error("Error creating rpc-addr parent dir")
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(fdAddr), 0700); err != nil {
		logrus.WithError(err).Error("Error creating fd-addr parent dir")
		os.Exit(1)
	}

	if err := run(ctx, rpcAddr, fdAddr, stdin, stdout, stderr); !ignorableError(err) {
		logrus.WithError(err).Error("Error running stdio service")
		os.Exit(2)
	}

	logrus.Debug("docker-stdio shutting down")
}

func run(ctx context.Context, rpcAddr, fdAddr string, stdin io.WriteCloser, stdout, stderr io.ReadCloser) error {
	ttrpcsrv, err := ttrpc.NewServer(ttrpc.WithUnaryServerInterceptor(func(ctx context.Context, u ttrpc.Unmarshaler, info *ttrpc.UnaryServerInfo, method ttrpc.Method) (interface{}, error) {
		logrus.WithField("URI", info.FullMethod).Debug("Call")
		return method(ctx, u)
	}))
	if err != nil {
		return fmt.Errorf("error creating ttrpc server: %w", err)
	}
	defer ttrpcsrv.Close()

	unix.Unlink(rpcAddr)
	rpc, err := net.Listen("unix", rpcAddr)
	if err != nil {
		return fmt.Errorf("error creating ttrpc listener: %w", err)
	}
	defer rpc.Close()

	unix.Unlink(fdAddr)
	fds, err := NewFdServer(fdAddr)
	if err != nil {
		return fmt.Errorf("error creating fd listener: %w", err)
	}
	defer fds.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := NewServer(stdin, stdout, stderr, cancel)
	defer s.Shutdown(ctx, nil)

	RegisterStdioService(ttrpcsrv, s)

	chErr := make(chan error, 1)
	go func() {
		chErr <- fds.Serve(ctx, nil)
		cancel()
	}()

	go func() {
		<-ctx.Done()
		ttrpcsrv.Close()
		rpc.Close()
	}()

	if err := ttrpcsrv.Serve(ctx, rpc); err != nil {
		return fmt.Errorf("error running ttrpc service: %w", err)
	}

	cancel()
	fds.Close()

	if err := <-chErr; err != nil {
		return err
	}

	<-ctx.Done()
	return nil
}

func ignorableError(err error) bool {
	if err == nil {
		return true
	}

	if errors.Is(err, context.Canceled) {
		return true
	}

	if errors.Is(err, ttrpc.ErrServerClosed) {
		return true
	}

	// TODO: go1.16 should have an error type for this!
	if strings.Contains(err.Error(), "use of closed network connection") {
		return true
	}

	return false
}
