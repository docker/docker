package graphdriver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/daemon/graphdriver/vfs"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/integration/internal/requirement"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/plugins"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/skip"
)

type graphEventsCounter struct {
	activations int
	creations   int
	removals    int
	gets        int
	puts        int
	stats       int
	cleanups    int
	exists      int
	init        int
	metadata    int
	diff        int
	applydiff   int
	changes     int
	diffsize    int
}

func TestExternalGraphDriver(t *testing.T) {
	skip.If(t, runtime.GOOS == "windows")
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, !requirement.HasHubConnectivity(t))

	// Setup plugin(s)
	ec := make(map[string]*graphEventsCounter)
	sserver := setupPluginViaSpecFile(t, ec)
	jserver := setupPluginViaJSONFile(t, ec)
	// Create daemon
	d := daemon.New(t, daemon.WithExperimental)
	c := d.NewClientT(t)

	for _, tc := range []struct {
		name string
		test func(client.APIClient, *daemon.Daemon) func(*testing.T)
	}{
		{
			name: "json",
			test: testExternalGraphDriver("json", ec),
		},
		{
			name: "spec",
			test: testExternalGraphDriver("spec", ec),
		},
		{
			name: "pull",
			test: testGraphDriverPull,
		},
	} {
		t.Run(tc.name, tc.test(c, d))
	}

	sserver.Close()
	jserver.Close()
	err := os.RemoveAll("/etc/docker/plugins")
	assert.NilError(t, err)
}

func setupPluginViaSpecFile(t *testing.T, ec map[string]*graphEventsCounter) *httptest.Server {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)

	setupPlugin(t, ec, "spec", mux, []byte(server.URL))

	return server
}

func setupPluginViaJSONFile(t *testing.T, ec map[string]*graphEventsCounter) *httptest.Server {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)

	p := plugins.NewLocalPlugin("json-external-graph-driver", server.URL)
	b, err := json.Marshal(p)
	assert.NilError(t, err)

	setupPlugin(t, ec, "json", mux, b)

	return server
}

func setupPlugin(t *testing.T, ec map[string]*graphEventsCounter, ext string, mux *http.ServeMux, b []byte) {
	name := fmt.Sprintf("%s-external-graph-driver", ext)
	type graphDriverRequest struct {
		ID         string `json:",omitempty"`
		Parent     string `json:",omitempty"`
		MountLabel string `json:",omitempty"`
		ReadOnly   bool   `json:",omitempty"`
	}

	type graphDriverResponse struct {
		Err      error             `json:",omitempty"`
		Dir      string            `json:",omitempty"`
		Exists   bool              `json:",omitempty"`
		Status   [][2]string       `json:",omitempty"`
		Metadata map[string]string `json:",omitempty"`
		Changes  []archive.Change  `json:",omitempty"`
		Size     int64             `json:",omitempty"`
	}

	respond := func(w http.ResponseWriter, data interface{}) {
		w.Header().Set("Content-Type", "application/vnd.docker.plugins.v1+json")
		switch t := data.(type) {
		case error:
			fmt.Fprintln(w, fmt.Sprintf(`{"Err": %q}`, t.Error()))
		case string:
			fmt.Fprintln(w, t)
		default:
			json.NewEncoder(w).Encode(&data)
		}
	}

	decReq := func(b io.ReadCloser, out interface{}, w http.ResponseWriter) error {
		defer b.Close()
		if err := json.NewDecoder(b).Decode(&out); err != nil {
			http.Error(w, fmt.Sprintf("error decoding json: %s", err.Error()), 500)
		}
		return nil
	}

	base, err := ioutil.TempDir("", name)
	assert.NilError(t, err)
	vfsProto, err := vfs.Init(base, []string{}, nil, nil)
	assert.NilError(t, err, "error initializing graph driver")
	driver := graphdriver.NewNaiveDiffDriver(vfsProto, nil, nil)

	ec[ext] = &graphEventsCounter{}
	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].activations++
		respond(w, `{"Implements": ["GraphDriver"]}`)
	})

	mux.HandleFunc("/GraphDriver.Init", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].init++
		respond(w, "{}")
	})

	mux.HandleFunc("/GraphDriver.CreateReadWrite", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].creations++

		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}
		if err := driver.CreateReadWrite(req.ID, req.Parent, nil); err != nil {
			respond(w, err)
			return
		}
		respond(w, "{}")
	})

	mux.HandleFunc("/GraphDriver.Create", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].creations++

		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}
		if err := driver.Create(req.ID, req.Parent, nil); err != nil {
			respond(w, err)
			return
		}
		respond(w, "{}")
	})

	mux.HandleFunc("/GraphDriver.Remove", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].removals++

		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}

		if err := driver.Remove(req.ID); err != nil {
			respond(w, err)
			return
		}
		respond(w, "{}")
	})

	mux.HandleFunc("/GraphDriver.Get", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].gets++

		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}

		// TODO @gupta-ak: Figure out what to do here.
		dir, err := driver.Get(req.ID, req.MountLabel)
		if err != nil {
			respond(w, err)
			return
		}
		respond(w, &graphDriverResponse{Dir: dir.Path()})
	})

	mux.HandleFunc("/GraphDriver.Put", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].puts++

		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}

		if err := driver.Put(req.ID); err != nil {
			respond(w, err)
			return
		}
		respond(w, "{}")
	})

	mux.HandleFunc("/GraphDriver.Exists", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].exists++

		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}
		respond(w, &graphDriverResponse{Exists: driver.Exists(req.ID)})
	})

	mux.HandleFunc("/GraphDriver.Status", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].stats++
		respond(w, &graphDriverResponse{Status: driver.Status()})
	})

	mux.HandleFunc("/GraphDriver.Cleanup", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].cleanups++
		err := driver.Cleanup()
		if err != nil {
			respond(w, err)
			return
		}
		respond(w, `{}`)
	})

	mux.HandleFunc("/GraphDriver.GetMetadata", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].metadata++

		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}

		data, err := driver.GetMetadata(req.ID)
		if err != nil {
			respond(w, err)
			return
		}
		respond(w, &graphDriverResponse{Metadata: data})
	})

	mux.HandleFunc("/GraphDriver.Diff", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].diff++

		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}

		diff, err := driver.Diff(req.ID, req.Parent)
		if err != nil {
			respond(w, err)
			return
		}
		io.Copy(w, diff)
	})

	mux.HandleFunc("/GraphDriver.Changes", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].changes++
		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}

		changes, err := driver.Changes(req.ID, req.Parent)
		if err != nil {
			respond(w, err)
			return
		}
		respond(w, &graphDriverResponse{Changes: changes})
	})

	mux.HandleFunc("/GraphDriver.ApplyDiff", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].applydiff++
		diff := r.Body
		defer r.Body.Close()

		id := r.URL.Query().Get("id")
		parent := r.URL.Query().Get("parent")

		if id == "" {
			http.Error(w, fmt.Sprintf("missing id"), 409)
		}

		size, err := driver.ApplyDiff(id, parent, diff)
		if err != nil {
			respond(w, err)
			return
		}
		respond(w, &graphDriverResponse{Size: size})
	})

	mux.HandleFunc("/GraphDriver.DiffSize", func(w http.ResponseWriter, r *http.Request) {
		ec[ext].diffsize++

		var req graphDriverRequest
		if err := decReq(r.Body, &req, w); err != nil {
			return
		}

		size, err := driver.DiffSize(req.ID, req.Parent)
		if err != nil {
			respond(w, err)
			return
		}
		respond(w, &graphDriverResponse{Size: size})
	})

	err = os.MkdirAll("/etc/docker/plugins", 0755)
	assert.NilError(t, err)

	specFile := "/etc/docker/plugins/" + name + "." + ext
	err = ioutil.WriteFile(specFile, b, 0644)
	assert.NilError(t, err)
}

func testExternalGraphDriver(ext string, ec map[string]*graphEventsCounter) func(client.APIClient, *daemon.Daemon) func(*testing.T) {
	return func(c client.APIClient, d *daemon.Daemon) func(*testing.T) {
		return func(t *testing.T) {
			driverName := fmt.Sprintf("%s-external-graph-driver", ext)
			d.StartWithBusybox(t, "-s", driverName)

			ctx := context.Background()

			testGraphDriver(ctx, t, c, driverName, func(t *testing.T) {
				d.Restart(t, "-s", driverName)
			})

			_, err := c.Info(ctx)
			assert.NilError(t, err)

			d.Stop(t)

			// Don't check ec.exists, because the daemon no longer calls the
			// Exists function.
			assert.Check(t, is.Equal(ec[ext].activations, 2))
			assert.Check(t, is.Equal(ec[ext].init, 2))
			assert.Check(t, ec[ext].creations >= 1)
			assert.Check(t, ec[ext].removals >= 1)
			assert.Check(t, ec[ext].gets >= 1)
			assert.Check(t, ec[ext].puts >= 1)
			assert.Check(t, is.Equal(ec[ext].stats, 5))
			assert.Check(t, is.Equal(ec[ext].cleanups, 2))
			assert.Check(t, ec[ext].applydiff >= 1)
			assert.Check(t, is.Equal(ec[ext].changes, 1))
			assert.Check(t, is.Equal(ec[ext].diffsize, 0))
			assert.Check(t, is.Equal(ec[ext].diff, 0))
			assert.Check(t, is.Equal(ec[ext].metadata, 1))
		}
	}
}

func testGraphDriverPull(c client.APIClient, d *daemon.Daemon) func(*testing.T) {
	return func(t *testing.T) {
		d.Start(t)
		defer d.Stop(t)
		ctx := context.Background()

		r, err := c.ImagePull(ctx, "busybox:latest@sha256:bbc3a03235220b170ba48a157dd097dd1379299370e1ed99ce976df0355d24f0", types.ImagePullOptions{})
		assert.NilError(t, err)
		_, err = io.Copy(ioutil.Discard, r)
		assert.NilError(t, err)

		container.Run(ctx, t, c, container.WithImage("busybox:latest@sha256:bbc3a03235220b170ba48a157dd097dd1379299370e1ed99ce976df0355d24f0"))
	}
}

func TestGraphdriverPluginV2(t *testing.T) {
	skip.If(t, runtime.GOOS == "windows")
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, !requirement.HasHubConnectivity(t))
	skip.If(t, os.Getenv("DOCKER_ENGINE_GOARCH") != "amd64")
	skip.If(t, !requirement.Overlay2Supported(testEnv.DaemonInfo.KernelVersion))

	d := daemon.New(t, daemon.WithExperimental)
	d.Start(t)
	defer d.Stop(t)

	client := d.NewClientT(t)
	defer client.Close()
	ctx := context.Background()

	// install the plugin
	plugin := "cpuguy83/docker-overlay2-graphdriver-plugin"
	responseReader, err := client.PluginInstall(ctx, plugin, types.PluginInstallOptions{
		RemoteRef:            plugin,
		AcceptAllPermissions: true,
	})
	defer responseReader.Close()
	assert.NilError(t, err)
	// ensure it's done by waiting for EOF on the response
	_, err = io.Copy(ioutil.Discard, responseReader)
	assert.NilError(t, err)

	// restart the daemon with the plugin set as the storage driver
	d.Stop(t)
	d.StartWithBusybox(t, "-s", plugin, "--storage-opt", "overlay2.override_kernel_check=1")

	testGraphDriver(ctx, t, client, plugin, nil)
}

func testGraphDriver(ctx context.Context, t *testing.T, c client.APIClient, driverName string, afterContainerRunFn func(*testing.T)) {
	id := container.Run(ctx, t, c, container.WithCmd("sh", "-c", "echo hello > /hello"))

	if afterContainerRunFn != nil {
		afterContainerRunFn(t)
	}

	i, err := c.ContainerInspect(ctx, id)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(i.GraphDriver.Name, driverName))

	diffs, err := c.ContainerDiff(ctx, id)
	assert.NilError(t, err)
	assert.Check(t, is.Contains(diffs, containertypes.ContainerChangeResponseItem{
		Kind: archive.ChangeAdd,
		Path: "/hello",
	}), "diffs: %v", diffs)

	err = c.ContainerRemove(ctx, id, types.ContainerRemoveOptions{
		Force: true,
	})
	assert.NilError(t, err)
}
