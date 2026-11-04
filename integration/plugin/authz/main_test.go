//go:build !windows

package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/environment"
	"github.com/moby/moby/v2/pkg/authorization"
	"github.com/moby/moby/v2/pkg/plugins"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gotest.tools/v3/skip"
)

var (
	testEnv     *environment.Execution
	d           *daemon.Daemon
	server      *httptest.Server
	ctrl        *authorizationController
	baseContext context.Context
)

func TestMain(m *testing.M) {
	shutdown := testutil.ConfigureTracing()

	ctx, span := otel.Tracer("").Start(context.Background(), "integration/plugin/authz.TestMain")
	baseContext = ctx

	var err error
	testEnv, err = environment.New(ctx)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.End()
		shutdown(ctx)
		panic(err)
	}
	err = environment.EnsureFrozenImagesLinux(ctx, testEnv)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.End()
		shutdown(ctx)
		panic(err)
	}

	testEnv.Print()
	setupSuite()
	exitCode := m.Run()
	teardownSuite()

	if exitCode != 0 {
		span.SetAttributes(attribute.Int("exit", exitCode))
		span.SetStatus(codes.Error, "m.Run() exited with non-zero exit code")
	}
	shutdown(ctx)

	os.Exit(exitCode)
}

func setupTest(t *testing.T) context.Context {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := testutil.StartSpan(baseContext, t)
	environment.ProtectAll(ctx, t, testEnv)

	d = daemon.New(t, daemon.WithExperimental())

	t.Cleanup(func() {
		if d != nil {
			d.Stop(t)
		}
		testEnv.Clean(ctx, t)
	})
	return ctx
}

func setupSuite() {
	mux := http.NewServeMux()
	server = httptest.NewServer(otelhttp.NewHandler(mux, ""))
	ctrl = &authorizationController{}
	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		b, err := json.Marshal(plugins.Manifest{Implements: []string{authorization.AuthZApiImplements}})
		if err != nil {
			panic("could not marshal json for /Plugin.Activate: " + err.Error())
		}
		w.Write(b)
	})

	mux.HandleFunc("/AuthZPlugin.AuthZReq", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			panic("could not read body for /AuthZPlugin.AuthZReq: " + err.Error())
		}
		authReq := authorization.Request{}
		err = json.Unmarshal(body, &authReq)
		if err != nil {
			panic("could not unmarshal json for /AuthZPlugin.AuthZReq: " + err.Error())
		}

		assertBody(authReq.RequestURI, authReq.RequestHeaders, authReq.RequestBody)
		assertAuthHeaders(authReq.RequestHeaders)

		// Count only server version api
		if strings.HasSuffix(authReq.RequestURI, serverVersionAPI) {
			ctrl.versionReqCount++
		}

		ctrl.requestsURIs = append(ctrl.requestsURIs, authReq.RequestURI)

		reqRes := ctrl.reqRes
		if isAllowed(authReq.RequestURI) {
			reqRes = authorization.Response{Allow: true}
		}
		if reqRes.Err != "" {
			w.WriteHeader(http.StatusInternalServerError)
		}
		b, err := json.Marshal(reqRes)
		if err != nil {
			panic("could not marshal json for /AuthZPlugin.AuthZReq: " + err.Error())
		}

		ctrl.reqUser = authReq.User
		_, _ = w.Write(b)
	})

	mux.HandleFunc("/AuthZPlugin.AuthZRes", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			panic("could not read body for /AuthZPlugin.AuthZRes: " + err.Error())
		}
		authReq := authorization.Request{}
		err = json.Unmarshal(body, &authReq)
		if err != nil {
			panic("could not unmarshal json for /AuthZPlugin.AuthZRes: " + err.Error())
		}

		assertBody(authReq.RequestURI, authReq.ResponseHeaders, authReq.ResponseBody)
		assertAuthHeaders(authReq.ResponseHeaders)

		// Count only server version api
		if strings.HasSuffix(authReq.RequestURI, serverVersionAPI) {
			ctrl.versionResCount++
		}
		resRes := ctrl.resRes
		if isAllowed(authReq.RequestURI) {
			resRes = authorization.Response{Allow: true}
		}
		if resRes.Err != "" {
			w.WriteHeader(http.StatusInternalServerError)
		}
		b, err := json.Marshal(resRes)
		if err != nil {
			panic("could not marshal json for /AuthZPlugin.AuthZRes: " + err.Error())
		}
		ctrl.resUser = authReq.User
		w.Write(b)
	})
}

func teardownSuite() {
	if server == nil {
		return
	}

	server.Close()
}

// assertAuthHeaders validates authentication headers are removed
func assertAuthHeaders(headers map[string]string) {
	for k := range headers {
		if strings.Contains(strings.ToLower(k), "auth") || strings.Contains(strings.ToLower(k), "x-registry") {
			panic(fmt.Sprintf("Found authentication headers in request '%v'", headers))
		}
	}
}

// assertBody asserts that body is removed for non text/json requests
func assertBody(requestURI string, headers map[string]string, body []byte) {
	if strings.Contains(strings.ToLower(requestURI), "auth") && len(body) > 0 {
		panic("Body included for authentication endpoint " + string(body))
	}

	for k, v := range headers {
		if strings.EqualFold(k, "Content-Type") && strings.HasPrefix(v, "text/") || v == "application/json" {
			return
		}
	}
	if len(body) > 0 {
		panic(fmt.Sprintf("Body included while it should not (Headers: '%v')", headers))
	}
}
