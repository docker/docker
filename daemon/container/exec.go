package container

import (
	"context"
	"maps"
	"runtime"
	"sync"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"github.com/moby/moby/v2/daemon/internal/stream"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/logger"
)

// ExecConfig holds the configurations for execs. The Daemon keeps
// track of both running and finished execs so that they can be
// examined both during and after completion.
type ExecConfig struct {
	sync.Mutex
	Started      chan struct{}
	StreamConfig *stream.Config
	ID           string
	Running      bool
	ExitCode     *int
	OpenStdin    bool
	OpenStderr   bool
	OpenStdout   bool
	CanRemove    bool
	Container    *Container
	DetachKeys   []byte
	Entrypoint   string
	Args         []string
	Tty          bool
	Privileged   bool
	User         string
	WorkingDir   string
	Env          []string
	Process      types.Process
	ConsoleSize  *[2]uint

	// CaptureStdout tees the exec's stdout into the container's logging
	// driver, stamping each message with the exec's identity.
	CaptureStdout bool
	// CaptureStderr tees the exec's stderr into the container's logging
	// driver, stamping each message with the exec's identity.
	CaptureStderr bool
	// Labels holds the user-defined metadata declared at exec create time.
	Labels map[string]string
	// LogCopier feeds the exec's output to the container's logging driver
	// when CaptureStdout or CaptureStderr is set. Its copy goroutines
	// terminate when the exec's streams are closed.
	LogCopier *logger.Copier
}

// NewExecConfig initializes the a new exec configuration
func NewExecConfig(c *Container) *ExecConfig {
	return &ExecConfig{
		ID:           stringid.GenerateRandomID(),
		Container:    c,
		StreamConfig: stream.NewConfig(),
		Started:      make(chan struct{}),
	}
}

// InitializeStdio is called by libcontainerd to connect the stdio.
func (c *ExecConfig) InitializeStdio(iop *cio.DirectIO) (cio.IO, error) {
	if c.CaptureStdout || c.CaptureStderr {
		// Attach the log-capture pipes before the process's output starts
		// flowing, so no early output is missed.
		c.startLogCapture()
	}
	c.StreamConfig.CopyToPipe(iop)

	if c.StreamConfig.Stdin() == nil && !c.Tty && runtime.GOOS == "windows" {
		if iop.Stdin != nil {
			if err := iop.Stdin.Close(); err != nil {
				log.G(context.TODO()).Errorf("error closing exec stdin: %+v", err)
			}
		}
	}

	return &rio{IO: iop, sc: c.StreamConfig}, nil
}

// CloseStreams closes the stdio streams for the exec
func (c *ExecConfig) CloseStreams() error {
	err := c.StreamConfig.CloseStreams()
	if c.LogCopier != nil {
		// Streams are closed: the copy goroutines drain to EOF; wait for
		// them so every captured line reached the driver before the exec
		// is reported as terminated.
		c.LogCopier.Wait()
	}
	return err
}

// SetExitCode sets the exec config's exit code
func (c *ExecConfig) SetExitCode(code int) {
	c.ExitCode = &code
}

// ExecStore keeps track of the exec configurations.
type ExecStore struct {
	byID map[string]*ExecConfig
	mu   sync.RWMutex
}

// NewExecStore initializes a new exec store.
func NewExecStore() *ExecStore {
	return &ExecStore{
		byID: make(map[string]*ExecConfig),
	}
}

// Commands returns the exec configurations in the store.
func (e *ExecStore) Commands() map[string]*ExecConfig {
	e.mu.RLock()
	byID := make(map[string]*ExecConfig, len(e.byID))
	maps.Copy(byID, e.byID)
	e.mu.RUnlock()
	return byID
}

// Add adds a new exec configuration to the store.
func (e *ExecStore) Add(id string, Config *ExecConfig) {
	e.mu.Lock()
	e.byID[id] = Config
	e.mu.Unlock()
}

// Get returns an exec configuration by its id.
func (e *ExecStore) Get(id string) *ExecConfig {
	e.mu.RLock()
	res := e.byID[id]
	e.mu.RUnlock()
	return res
}

// Delete removes an exec configuration from the store.
func (e *ExecStore) Delete(id string) {
	e.mu.Lock()
	delete(e.byID, id)
	e.mu.Unlock()
}

// List returns the list of exec ids in the store.
func (e *ExecStore) List() []string {
	var IDs []string
	e.mu.RLock()
	for id := range e.byID {
		IDs = append(IDs, id)
	}
	e.mu.RUnlock()
	return IDs
}
