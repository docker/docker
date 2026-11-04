package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/logger/internal/logdriver"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/pkg/plugingetter"
	"github.com/moby/moby/v2/pkg/plugins"
)

var pluginGetter plugingetter.PluginGetter

const extName = "LogDriver"

// logPlugin defines the available functions that logging plugins must implement.
type logPlugin interface {
	StartLogging(streamPath string, info Info) (err error)
	StopLogging(streamPath string) (err error)
	Capabilities() (capability Capability, err error)
	ReadLogs(info Info, config ReadConfig) (stream io.ReadCloser, err error)
}

// RegisterPluginGetter sets the plugingetter
func RegisterPluginGetter(g plugingetter.PluginGetter) {
	pluginGetter = g
}

// getPlugin returns a logging driver by its name.
// If the driver is empty, it looks for the local driver.
func getPlugin(name string, mode int) (Creator, error) {
	p, err := pluginGetter.Get(name, extName, mode)
	if err != nil {
		return nil, fmt.Errorf("error looking up logging plugin %s: %v", name, err)
	}

	client, err := makePluginClient(p)
	if err != nil {
		return nil, err
	}
	return makePluginCreator(name, client, p.ScopedPath), nil
}

func makePluginClient(p plugingetter.CompatPlugin) (logPlugin, error) {
	if pc, ok := p.(plugingetter.PluginWithV1Client); ok {
		return &logPluginProxy{pc.Client()}, nil
	}
	pa, ok := p.(plugingetter.PluginAddr)
	if !ok {
		return nil, errdefs.System(fmt.Errorf("got unknown plugin type %T", p))
	}

	if pa.Protocol() != plugins.ProtocolSchemeHTTPV1 {
		return nil, fmt.Errorf("plugin protocol not supported: %s", p)
	}

	addr := pa.Addr()
	c, err := plugins.NewClientWithTimeout(addr.Network()+"://"+addr.String(), nil, pa.Timeout())
	if err != nil {
		return nil, fmt.Errorf("error making plugin client: %w", err)
	}
	return &logPluginProxy{c}, nil
}

func makePluginCreator(name string, l logPlugin, scopePath func(s string) string) Creator {
	return func(logCtx Info) (logger Logger, retErr error) {
		defer func() {
			if retErr != nil {
				_, _ = pluginGetter.Get(name, extName, plugingetter.Release)
			}
		}()

		unscopedPath := filepath.Join("/", "run", "docker", "logging")
		logRoot := scopePath(unscopedPath)
		if err := os.MkdirAll(logRoot, 0o700); err != nil {
			return nil, err
		}

		id := stringid.GenerateRandomID()
		a := &pluginAdapter{
			driverName: name,
			id:         id,
			plugin:     l,
			fifoPath:   filepath.Join(logRoot, id),
			logInfo:    logCtx,
		}

		caps, err := a.plugin.Capabilities()
		if err == nil {
			a.capabilities = caps
		}

		stream, err := openPluginStream(a)
		if err != nil {
			return nil, err
		}

		a.stream = stream
		a.enc = logdriver.NewLogEntryEncoder(a.stream)

		if err := l.StartLogging(filepath.Join(unscopedPath, id), logCtx); err != nil {
			return nil, fmt.Errorf("error creating logger: %w", err)
		}

		if caps.ReadLogs {
			return &pluginAdapterWithRead{a}, nil
		}

		return a, nil
	}
}
