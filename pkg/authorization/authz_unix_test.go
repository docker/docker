// +build !windows

// TODO Windows: This uses a Unix socket for testing. This might be possible
// to port to Windows using a named pipe instead.

package authorization // import "github.com/docker/docker/pkg/authorization"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/pkg/plugins"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/gorilla/mux"
)

const (
	pluginAddress = "authz-test-plugin.sock"
)

func TestAuthZRequestPluginError(t *testing.T) {
	server := authZPluginTestServer{t: t}
	server.start()
	defer server.stop()

	authZPlugin := createTestPlugin(t, server.socketAddress())

	request := Request{
		User:           "user",
		RequestBody:    []byte("sample body"),
		RequestURI:     "www.authz.com/auth",
		RequestMethod:  http.MethodGet,
		RequestHeaders: map[string]string{"header": "value"},
	}
	server.replayResponse = Response{
		Err: "an error",
	}

	actualResponse, err := authZPlugin.AuthZRequest(&request)
	if err != nil {
		t.Fatalf("Failed to authorize request %v", err)
	}

	if !reflect.DeepEqual(server.replayResponse, *actualResponse) {
		t.Fatal("Response must be equal")
	}
	if !reflect.DeepEqual(request, server.recordedRequest) {
		t.Fatal("Requests must be equal")
	}
}

func TestAuthZRequestPlugin(t *testing.T) {
	server := authZPluginTestServer{t: t}
	server.start()
	defer server.stop()

	authZPlugin := createTestPlugin(t, server.socketAddress())

	request := Request{
		User:           "user",
		RequestBody:    []byte("sample body"),
		RequestURI:     "www.authz.com/auth",
		RequestMethod:  http.MethodGet,
		RequestHeaders: map[string]string{"header": "value"},
	}
	server.replayResponse = Response{
		Allow: true,
		Msg:   "Sample message",
	}

	actualResponse, err := authZPlugin.AuthZRequest(&request)
	if err != nil {
		t.Fatalf("Failed to authorize request %v", err)
	}

	if !reflect.DeepEqual(server.replayResponse, *actualResponse) {
		t.Fatal("Response must be equal")
	}
	if !reflect.DeepEqual(request, server.recordedRequest) {
		t.Fatal("Requests must be equal")
	}
}

func TestAuthZResponsePlugin(t *testing.T) {
	server := authZPluginTestServer{t: t}
	server.start()
	defer server.stop()

	authZPlugin := createTestPlugin(t, server.socketAddress())

	request := Request{
		User:        "user",
		RequestURI:  "something.com/auth",
		RequestBody: []byte("sample body"),
	}
	server.replayResponse = Response{
		Allow: true,
		Msg:   "Sample message",
	}

	actualResponse, err := authZPlugin.AuthZResponse(&request)
	if err != nil {
		t.Fatalf("Failed to authorize request %v", err)
	}

	if !reflect.DeepEqual(server.replayResponse, *actualResponse) {
		t.Fatal("Response must be equal")
	}
	if !reflect.DeepEqual(request, server.recordedRequest) {
		t.Fatal("Requests must be equal")
	}
}

func TestResponseModifier(t *testing.T) {
	r := httptest.NewRecorder()
	m := NewResponseModifier(r)
	m.Header().Set("h1", "v1")
	m.Write([]byte("body"))
	m.WriteHeader(http.StatusInternalServerError)

	m.FlushAll()
	if r.Header().Get("h1") != "v1" {
		t.Fatalf("Header value must exists %s", r.Header().Get("h1"))
	}
	if !reflect.DeepEqual(r.Body.Bytes(), []byte("body")) {
		t.Fatalf("Body value must exists %s", r.Body.Bytes())
	}
	if r.Code != http.StatusInternalServerError {
		t.Fatalf("Status code must be correct %d", r.Code)
	}
}

func TestDrainBody(t *testing.T) {
	tests := []struct {
		length             int // length is the message length send to drainBody
		expectedBodyLength int // expectedBodyLength is the expected body length after drainBody is called
	}{
		{10, 10},                           // Small message size
		{maxBodySize - 1, maxBodySize - 1}, // Max message size
		{maxBodySize * 2, 0},               // Large message size (skip copying body)

	}

	for _, test := range tests {
		msg := strings.Repeat("a", test.length)
		body, closer, err := drainBody(ioutil.NopCloser(bytes.NewReader([]byte(msg))))
		if err != nil {
			t.Fatal(err)
		}
		if len(body) != test.expectedBodyLength {
			t.Fatalf("Body must be copied, actual length: '%d'", len(body))
		}
		if closer == nil {
			t.Fatal("Closer must not be nil")
		}
		modified, err := ioutil.ReadAll(closer)
		if err != nil {
			t.Fatalf("Error must not be nil: '%v'", err)
		}
		if len(modified) != len(msg) {
			t.Fatalf("Result should not be truncated. Original length: '%d', new length: '%d'", len(msg), len(modified))
		}
	}
}

func TestSendBody(t *testing.T) {
	var (
		url       = "nothing.com"
		testcases = []struct {
			contentType string
			expected    bool
		}{
			{
				contentType: "application/json",
				expected:    true,
			},
			{
				contentType: "Application/json",
				expected:    true,
			},
			{
				contentType: "application/JSON",
				expected:    true,
			},
			{
				contentType: "APPLICATION/JSON",
				expected:    true,
			},
			{
				contentType: "application/json; charset=utf-8",
				expected:    true,
			},
			{
				contentType: "application/json;charset=utf-8",
				expected:    true,
			},
			{
				contentType: "application/json; charset=UTF8",
				expected:    true,
			},
			{
				contentType: "application/json;charset=UTF8",
				expected:    true,
			},
			{
				contentType: "text/html",
				expected:    false,
			},
			{
				contentType: "",
				expected:    false,
			},
		}
	)

	for _, testcase := range testcases {
		header := http.Header{}
		header.Set("Content-Type", testcase.contentType)

		if b := sendBody(url, header); b != testcase.expected {
			t.Fatalf("Unexpected Content-Type; Expected: %t, Actual: %t", testcase.expected, b)
		}
	}
}

func TestResponseModifierOverride(t *testing.T) {
	r := httptest.NewRecorder()
	m := NewResponseModifier(r)
	m.Header().Set("h1", "v1")
	m.Write([]byte("body"))
	m.WriteHeader(http.StatusInternalServerError)

	overrideHeader := make(http.Header)
	overrideHeader.Add("h1", "v2")
	overrideHeaderBytes, err := json.Marshal(overrideHeader)
	if err != nil {
		t.Fatalf("override header failed %v", err)
	}

	m.OverrideHeader(overrideHeaderBytes)
	m.OverrideBody([]byte("override body"))
	m.OverrideStatusCode(http.StatusNotFound)
	m.FlushAll()
	if r.Header().Get("h1") != "v2" {
		t.Fatalf("Header value must exists %s", r.Header().Get("h1"))
	}
	if !reflect.DeepEqual(r.Body.Bytes(), []byte("override body")) {
		t.Fatalf("Body value must exists %s", r.Body.Bytes())
	}
	if r.Code != http.StatusNotFound {
		t.Fatalf("Status code must be correct %d", r.Code)
	}
}

func TestAuthZRequestID(t *testing.T) {
	plugins := []Plugin{newAuthZPluginRequestID()}
	authCtx := NewCtx(plugins, "", "", "GET", "request_URI_foo")

	r, err := http.NewRequest("GET", "request_URI_foo", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Error creating http request: %s", err)
	}

	w := httptest.NewRecorder()
	if err := authCtx.AuthZRequest(w, r); err != nil {
		t.Fatalf("Unexpected error from the plugin: %s", err)
	}

	m := NewResponseModifier(w)
	if err := authCtx.AuthZResponse(m, r); err != nil {
		t.Fatalf("Unexpected error from the plugin: %s", err)
	}
}

type authZPluginRequestID struct {
	requestID string
}

func newAuthZPluginRequestID() Plugin {
	return &authZPluginRequestID{}
}

func (a *authZPluginRequestID) Name() string {
	return "authorization_plugin_to_verify_request_id"
}

func (a *authZPluginRequestID) AuthZRequest(authReq *Request) (*Response, error) {
	if authReq.RequestID == "" {
		return nil, fmt.Errorf("expected non-empty RequestID field")
	}
	a.requestID = authReq.RequestID
	return &Response{Allow: true}, nil
}

func (a *authZPluginRequestID) AuthZResponse(authReq *Request) (*Response, error) {
	if authReq.RequestID != a.requestID {
		return nil, fmt.Errorf("expected RequestID %s, got %s", a.requestID, authReq.RequestID)
	}
	return &Response{Allow: true}, nil
}

// createTestPlugin creates a new sample authorization plugin
func createTestPlugin(t *testing.T, socketAddress string) *authorizationPlugin {
	client, err := plugins.NewClient("unix:///"+socketAddress, &tlsconfig.Options{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("Failed to create client %v", err)
	}

	return &authorizationPlugin{name: "plugin", plugin: client}
}

// AuthZPluginTestServer is a simple server that implements the authZ plugin interface
type authZPluginTestServer struct {
	listener net.Listener
	t        *testing.T
	// request stores the request sent from the daemon to the plugin
	recordedRequest Request
	// response stores the response sent from the plugin to the daemon
	replayResponse Response
	server         *httptest.Server
	tmpDir         string
}

func (t *authZPluginTestServer) socketAddress() string {
	return path.Join(t.tmpDir, pluginAddress)
}

// start starts the test server that implements the plugin
func (t *authZPluginTestServer) start() {
	var err error
	t.tmpDir, err = ioutil.TempDir("", "authz")
	if err != nil {
		t.t.Fatal(err)
	}

	r := mux.NewRouter()
	l, err := net.Listen("unix", t.socketAddress())
	if err != nil {
		t.t.Fatal(err)
	}
	t.listener = l
	r.HandleFunc("/Plugin.Activate", t.activate)
	r.HandleFunc("/"+AuthZApiRequest, t.auth)
	r.HandleFunc("/"+AuthZApiResponse, t.auth)
	t.server = &httptest.Server{
		Listener: l,
		Config: &http.Server{
			Handler: r,
			Addr:    pluginAddress,
		},
	}
	t.server.Start()
}

// stop stops the test server that implements the plugin
func (t *authZPluginTestServer) stop() {
	t.server.Close()
	_ = os.RemoveAll(t.tmpDir)
	if t.listener != nil {
		t.listener.Close()
	}
}

// auth is a used to record/replay the authentication api messages
func (t *authZPluginTestServer) auth(w http.ResponseWriter, r *http.Request) {
	t.recordedRequest = Request{}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		t.t.Fatal(err)
	}
	r.Body.Close()
	json.Unmarshal(body, &t.recordedRequest)
	b, err := json.Marshal(t.replayResponse)
	if err != nil {
		t.t.Fatal(err)
	}
	w.Write(b)
}

func (t *authZPluginTestServer) activate(w http.ResponseWriter, r *http.Request) {
	b, err := json.Marshal(plugins.Manifest{Implements: []string{AuthZApiImplements}})
	if err != nil {
		t.t.Fatal(err)
	}
	w.Write(b)
}
