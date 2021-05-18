// +build !windows

package stdio

import (
	"context"
	"errors"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/docker/docker/pkg/reexec"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"
)

func TestMain(m *testing.M) {
	reexec.Register("docker-fd-test", runTestFdServer)
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

func runTestFdServer() {
	addr := os.Getenv("TEST_FD_ADDR")
	if addr == "" {
		panic("missing socket addr")
	}

	s, err := NewFdServer(addr)
	if err != nil {
		panic(err)
	}

	h := func(fds []int) {
		for _, fd := range fds {
			f := os.NewFile(uintptr(fd), "TEST_FD_"+strconv.Itoa(fd))
			f.Write([]byte{byte(fd)})
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	sig := make(chan os.Signal, 1)
	go func() {
		<-sig
		cancel()
		s.Close()
	}()
	signal.Notify(sig, unix.SIGTERM, unix.SIGINT)

	if err := s.Serve(ctx, h); err != nil && !errors.Is(err, context.Canceled) {
		panic(err)
	}
}

func TestFdServer(t *testing.T) {
	dir, err := ioutil.TempDir("", t.Name())
	assert.NilError(t, err)
	defer os.RemoveAll(dir)

	addr := filepath.Join(dir, "fd.sock")

	cmd := reexec.Command("docker-fd-test")
	cmd.Env = []string{"TEST_FD_ADDR=" + addr}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	assert.NilError(t, cmd.Start())

	defer func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	client, err := NewFdClient(ctx, addr)
	cancel()

	assert.NilError(t, err)
	defer client.Close()

	r, w, err := os.Pipe()
	assert.NilError(t, err)

	rfds, err := client.Sendfd(w)
	assert.NilError(t, err)
	assert.Assert(t, cmp.Len(rfds, 1))

	buf := make([]byte, 1)
	_, err = r.Read(buf)
	assert.NilError(t, err)
	// expect the data read from the pipe to be the fd number that was returned by `Sendfd`, which is the remote fd.j
	assert.Equal(t, int(buf[0]), rfds[0])
}
