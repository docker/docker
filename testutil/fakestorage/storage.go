package fakestorage // import "github.com/docker/docker/testutil/fakestorage"

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/testutil"
	"github.com/docker/docker/testutil/environment"
	"github.com/docker/docker/testutil/fakecontext"
	"github.com/docker/docker/testutil/request"
	"github.com/docker/go-connections/nat"
	"gotest.tools/assert"
)

var testEnv *environment.Execution

type testingT interface {
	assert.TestingT
	logT
	skipT
	Fatal(args ...interface{})
	Fatalf(string, ...interface{})
}

type logT interface {
	Logf(string, ...interface{})
}

type skipT interface {
	Skip(...interface{})
}

// Fake is a static file server. It might be running locally or remotely
// on test host.
type Fake interface {
	Close() error
	URL() string
	CtxDir() string
}

// SetTestEnvironment sets a static test environment
// TODO: decouple this package from environment
func SetTestEnvironment(env *environment.Execution) {
	testEnv = env
}

// New returns a static file server that will be use as build context.
func New(t testingT, dir string, modifiers ...func(*fakecontext.Fake) error) Fake {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	if testEnv == nil {
		t.Fatal("fakstorage package requires SetTestEnvironment() to be called before use.")
	}
	ctx := fakecontext.New(t, dir, modifiers...)
	switch {
	case testEnv.IsRemoteDaemon() && strings.HasPrefix(request.DaemonHost(), "unix:///"):
		t.Skip("e2e run : daemon is remote but docker host points to a unix socket")
	case testEnv.IsLocalDaemon():
		return newLocalFakeStorage(ctx)
	default:
		return newRemoteFileServer(t, ctx, testEnv.APIClient())
	}
	return nil
}

// localFileStorage is a file storage on the running machine
type localFileStorage struct {
	*fakecontext.Fake
	*httptest.Server
}

func (s *localFileStorage) URL() string {
	return s.Server.URL
}

func (s *localFileStorage) CtxDir() string {
	return s.Fake.Dir
}

func (s *localFileStorage) Close() error {
	defer s.Server.Close()
	return s.Fake.Close()
}

func newLocalFakeStorage(ctx *fakecontext.Fake) *localFileStorage {
	handler := http.FileServer(http.Dir(ctx.Dir))
	server := httptest.NewServer(handler)
	return &localFileStorage{
		Fake:   ctx,
		Server: server,
	}
}

// remoteFileServer is a containerized static file server started on the remote
// testing machine to be used in URL-accepting docker build functionality.
type remoteFileServer struct {
	host      string // hostname/port web server is listening to on docker host e.g. 0.0.0.0:43712
	container string
	image     string
	client    client.APIClient
	ctx       *fakecontext.Fake
}

func (f *remoteFileServer) URL() string {
	u := url.URL{
		Scheme: "http",
		Host:   f.host}
	return u.String()
}

func (f *remoteFileServer) CtxDir() string {
	return f.ctx.Dir
}

func (f *remoteFileServer) Close() error {
	defer func() {
		if f.ctx != nil {
			f.ctx.Close()
		}
		if f.image != "" {
			if _, err := f.client.ImageRemove(context.Background(), f.image, types.ImageRemoveOptions{
				Force: true,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Error closing remote file server : %v\n", err)
			}
		}
		if err := f.client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Error closing remote file server : %v\n", err)
		}
	}()
	if f.container == "" {
		return nil
	}
	return f.client.ContainerRemove(context.Background(), f.container, types.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
}

func newRemoteFileServer(t testingT, ctx *fakecontext.Fake, c client.APIClient) *remoteFileServer {
	var (
		image     = fmt.Sprintf("fileserver-img-%s", strings.ToLower(testutil.GenerateRandomAlphaOnlyString(10)))
		container = fmt.Sprintf("fileserver-cnt-%s", strings.ToLower(testutil.GenerateRandomAlphaOnlyString(10)))
	)

	ensureHTTPServerImage(t)

	// Build the image
	if err := ctx.Add("Dockerfile", `FROM httpserver
COPY . /static`); err != nil {
		t.Fatal(err)
	}
	resp, err := c.ImageBuild(context.Background(), ctx.AsTarReader(t), types.ImageBuildOptions{
		NoCache: true,
		Tags:    []string{image},
	})
	assert.NilError(t, err)
	_, err = io.Copy(ioutil.Discard, resp.Body)
	assert.NilError(t, err)

	// Start the container
	b, err := c.ContainerCreate(context.Background(), &containertypes.Config{
		Image: image,
	}, &containertypes.HostConfig{}, nil, container)
	assert.NilError(t, err)
	err = c.ContainerStart(context.Background(), b.ID, types.ContainerStartOptions{})
	assert.NilError(t, err)

	// Find out the system assigned port
	i, err := c.ContainerInspect(context.Background(), b.ID)
	assert.NilError(t, err)
	newP, err := nat.NewPort("tcp", "80")
	assert.NilError(t, err)
	ports, exists := i.NetworkSettings.Ports[newP]
	if !exists || len(ports) != 1 {
		t.Fatalf("unable to find port 80/tcp for %s", container)
	}
	host := ports[0].HostIP
	port := ports[0].HostPort

	return &remoteFileServer{
		container: container,
		image:     image,
		host:      fmt.Sprintf("%s:%s", host, port),
		ctx:       ctx,
		client:    c,
	}
}
