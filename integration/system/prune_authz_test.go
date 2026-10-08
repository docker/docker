package system

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/request"
	"github.com/moby/moby/v2/pkg/authorization"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

func TestPruneProgressAuthorization(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "cannot start multiple daemons on windows")
	skip.If(t, testEnv.IsRemoteDaemon, "requires a local test daemon")
	ctx := testutil.StartSpan(baseContext, t)
	var denyRequest, denyResponse atomic.Bool
	responses := make(chan authorization.Request, 1)
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Plugin.Activate" {
			assert.Check(t, json.NewEncoder(w).Encode(map[string]any{"Implements": []string{"authz"}}) == nil)
			return
		}
		var req authorization.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			assert.Check(t, err == nil)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		allow := true
		if strings.HasPrefix(req.RequestURI, "/v1.57/volumes/prune?") {
			if r.URL.Path == "/AuthZPlugin.AuthZReq" {
				allow = !denyRequest.Load()
			} else {
				responses <- req
				allow = !denyResponse.Load()
			}
		}
		assert.Check(t, json.NewEncoder(w).Encode(authorization.Response{Allow: allow, Msg: "prune response denied"}) == nil)
	}))
	defer plugin.Close()
	const pluginName = "prune-progress-authz"
	const specDir = "/etc/docker/plugins"
	assert.NilError(t, os.MkdirAll(specDir, 0o755))
	spec := filepath.Join(specDir, pluginName+".spec")
	assert.NilError(t, os.WriteFile(spec, []byte(plugin.URL), 0o644))
	defer os.Remove(spec)
	d := daemon.New(t, daemon.WithContainerdSocket(""))
	d.Start(t, "--iptables=false", "--ip6tables=false", "--authorization-plugin="+pluginName)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	const path = "/v1.57/volumes/prune?stream=true&filters=%7B%22all%22%3A%5B%22true%22%5D%7D"
	for _, mode := range []string{"deny request", "deny response", "allow"} {
		t.Run(mode, func(t *testing.T) {
			_, err := apiClient.VolumeCreate(ctx, client.VolumeCreateOptions{Name: "prune-authorization-volume"})
			assert.NilError(t, err)
			denyRequest.Store(mode == "deny request")
			denyResponse.Store(mode == "deny response")
			resp, body, err := request.Post(ctx, path, request.Host(d.Sock()))
			assert.NilError(t, err)
			defer body.Close()
			if mode != "allow" {
				assert.Equal(t, resp.StatusCode, http.StatusForbidden)
				raw, err := io.ReadAll(body)
				assert.NilError(t, err)
				var denied map[string]any
				assert.NilError(t, json.Unmarshal(raw, &denied))
				assert.Equal(t, len(denied), 1, "only the denial message may reach the client")
				assert.Assert(t, denied["message"] != nil)
			} else {
				assert.Equal(t, resp.StatusCode, http.StatusOK)
				assert.Equal(t, resp.Header.Get("Content-Type"), "application/json")
				var report volume.PruneReport
				assert.NilError(t, json.NewDecoder(body).Decode(&report))
				assert.DeepEqual(t, report.VolumesDeleted, []string{"prune-authorization-volume"})
			}
			if mode == "deny request" {
				_, err := apiClient.VolumeInspect(ctx, "prune-authorization-volume", client.VolumeInspectOptions{})
				assert.NilError(t, err)
				return
			}
			recorded := <-responses
			assert.Equal(t, recorded.ResponseStatusCode, http.StatusOK)
			assert.Equal(t, recorded.ResponseHeaders["Content-Type"], "application/json")
			var report volume.PruneReport
			assert.NilError(t, json.Unmarshal(recorded.ResponseBody, &report))
			assert.DeepEqual(t, report.VolumesDeleted, []string{"prune-authorization-volume"})
		})
	}
}
