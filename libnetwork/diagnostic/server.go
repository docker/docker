package diagnostic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/docker/docker/libnetwork/internal/caller"
	stackdump "github.com/moby/sys/signal"
	"github.com/sirupsen/logrus"
)

// HTTPHandlerFunc TODO
type HTTPHandlerFunc func(interface{}, http.ResponseWriter, *http.Request)

type httpHandlerCustom struct {
	ctx interface{}
	F   func(interface{}, http.ResponseWriter, *http.Request)
}

// ServeHTTP TODO
func (h httpHandlerCustom) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.F(h.ctx, w, r)
}

var diagPaths2Func = map[string]HTTPHandlerFunc{
	"/":          notImplemented,
	"/help":      help,
	"/ready":     ready,
	"/stackdump": stackTrace,
}

// Server when the debug is enabled exposes a
// This data structure is protected by the Agent mutex so does not require and additional mutex here
type Server struct {
	enable            int32
	srv               *http.Server
	port              int
	mux               *http.ServeMux
	registeredHanders map[string]bool
	sync.Mutex
}

// New creates a new diagnostic server
func New() *Server {
	return &Server{
		registeredHanders: make(map[string]bool),
	}
}

// Init initialize the mux for the http handling and register the base hooks
func (s *Server) Init() {
	s.mux = http.NewServeMux()

	// Register local handlers
	s.RegisterHandler(s, diagPaths2Func)
}

// RegisterHandler allows to register new handlers to the mux and to a specific path
func (s *Server) RegisterHandler(ctx interface{}, hdlrs map[string]HTTPHandlerFunc) {
	s.Lock()
	defer s.Unlock()
	for path, fun := range hdlrs {
		if _, ok := s.registeredHanders[path]; ok {
			continue
		}
		s.mux.Handle(path, httpHandlerCustom{ctx, fun})
		s.registeredHanders[path] = true
	}
}

// ServeHTTP this is the method called bu the ListenAndServe, and is needed to allow us to
// use our custom mux
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// EnableDiagnostic opens a TCP socket to debug the passed network DB
func (s *Server) EnableDiagnostic(ip string, port int) {
	s.Lock()
	defer s.Unlock()

	s.port = port

	if s.enable == 1 {
		logrus.Info("The server is already up and running")
		return
	}

	logrus.Infof("Starting the diagnostic server listening on %d for commands", port)
	srv := &http.Server{Addr: fmt.Sprintf("%s:%d", ip, port), Handler: s}
	s.srv = srv
	s.enable = 1
	go func(n *Server) {
		// Ignore ErrServerClosed that is returned on the Shutdown call
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logrus.Errorf("ListenAndServe error: %s", err)
			atomic.SwapInt32(&n.enable, 0)
		}
	}(s)
}

// DisableDiagnostic stop the dubug and closes the tcp socket
func (s *Server) DisableDiagnostic() {
	s.Lock()
	defer s.Unlock()

	s.srv.Shutdown(context.Background()) // nolint:errcheck
	s.srv = nil
	s.enable = 0
	logrus.Info("Disabling the diagnostic server")
}

// IsDiagnosticEnabled returns true when the debug is enabled
func (s *Server) IsDiagnosticEnabled() bool {
	s.Lock()
	defer s.Unlock()
	return s.enable == 1
}

func notImplemented(ctx interface{}, w http.ResponseWriter, r *http.Request) {
	r.ParseForm() // nolint:errcheck
	_, json := ParseHTTPFormOptions(r)
	rsp := WrongCommand("not implemented", fmt.Sprintf("URL path: %s no method implemented check /help\n", r.URL.Path))

	// audit logs
	log := logrus.WithFields(logrus.Fields{"component": "diagnostic", "remoteIP": r.RemoteAddr, "method": caller.Name(0), "url": r.URL.String()})
	log.Info("command not implemented done")

	HTTPReply(w, rsp, json) // nolint:errcheck
}

func help(ctx interface{}, w http.ResponseWriter, r *http.Request) {
	r.ParseForm() // nolint:errcheck
	_, json := ParseHTTPFormOptions(r)

	// audit logs
	log := logrus.WithFields(logrus.Fields{"component": "diagnostic", "remoteIP": r.RemoteAddr, "method": caller.Name(0), "url": r.URL.String()})
	log.Info("help done")

	n, ok := ctx.(*Server)
	var result string
	if ok {
		for path := range n.registeredHanders {
			result += fmt.Sprintf("%s\n", path)
		}
		HTTPReply(w, CommandSucceed(&StringCmd{Info: result}), json) // nolint:errcheck
	}
}

func ready(ctx interface{}, w http.ResponseWriter, r *http.Request) {
	r.ParseForm() // nolint:errcheck
	_, json := ParseHTTPFormOptions(r)

	// audit logs
	log := logrus.WithFields(logrus.Fields{"component": "diagnostic", "remoteIP": r.RemoteAddr, "method": caller.Name(0), "url": r.URL.String()})
	log.Info("ready done")
	HTTPReply(w, CommandSucceed(&StringCmd{Info: "OK"}), json) // nolint:errcheck
}

func stackTrace(ctx interface{}, w http.ResponseWriter, r *http.Request) {
	r.ParseForm() // nolint:errcheck
	_, json := ParseHTTPFormOptions(r)

	// audit logs
	log := logrus.WithFields(logrus.Fields{"component": "diagnostic", "remoteIP": r.RemoteAddr, "method": caller.Name(0), "url": r.URL.String()})
	log.Info("stack trace")

	path, err := stackdump.DumpStacks("/tmp/")
	if err != nil {
		log.WithError(err).Error("failed to write goroutines dump")
		HTTPReply(w, FailCommand(err), json) // nolint:errcheck
	} else {
		log.Info("stack trace done")
		HTTPReply(w, CommandSucceed(&StringCmd{Info: fmt.Sprintf("goroutine stacks written to %s", path)}), json) // nolint:errcheck
	}
}

// DebugHTTPForm helper to print the form url parameters
func DebugHTTPForm(r *http.Request) {
	for k, v := range r.Form {
		logrus.Debugf("Form[%q] = %q\n", k, v)
	}
}

// JSONOutput contains details on JSON output printing
type JSONOutput struct {
	enable      bool
	prettyPrint bool
}

// ParseHTTPFormOptions easily parse the JSON printing options
func ParseHTTPFormOptions(r *http.Request) (bool, *JSONOutput) {
	_, unsafe := r.Form["unsafe"]
	v, json := r.Form["json"]
	var pretty bool
	if len(v) > 0 {
		pretty = v[0] == "pretty"
	}
	return unsafe, &JSONOutput{enable: json, prettyPrint: pretty}
}

// HTTPReply helper function that takes care of sending the message out
func HTTPReply(w http.ResponseWriter, r *HTTPResult, j *JSONOutput) (int, error) {
	var response []byte
	if j.enable {
		w.Header().Set("Content-Type", "application/json")
		var err error
		if j.prettyPrint {
			response, err = json.MarshalIndent(r, "", "  ")
			if err != nil {
				response, _ = json.MarshalIndent(FailCommand(err), "", "  ")
			}
		} else {
			response, err = json.Marshal(r)
			if err != nil {
				response, _ = json.Marshal(FailCommand(err))
			}
		}
	} else {
		response = []byte(r.String())
	}
	return fmt.Fprint(w, string(response))
}
