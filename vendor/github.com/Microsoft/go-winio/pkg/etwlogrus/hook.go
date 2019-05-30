package etwlogrus

import (
	"github.com/Microsoft/go-winio/pkg/etw"
	"github.com/sirupsen/logrus"
)

// Hook is a Logrus hook which logs received events to ETW.
type Hook struct {
	provider      *etw.Provider
	closeProvider bool
}

// NewHook registers a new ETW provider and returns a hook to log from it. The
// provider will be closed when the hook is closed.
func NewHook(providerName string) (*Hook, error) {
	provider, err := etw.NewProvider(providerName, nil)
	if err != nil {
		return nil, err
	}

	return &Hook{provider, true}, nil
}

// NewHookFromProvider creates a new hook based on an existing ETW provider. The
// provider will not be closed when the hook is closed.
func NewHookFromProvider(provider *etw.Provider) (*Hook, error) {
	return &Hook{provider, false}, nil
}

// Levels returns the set of levels that this hook wants to receive log entries
// for.
func (h *Hook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.TraceLevel,
		logrus.DebugLevel,
		logrus.InfoLevel,
		logrus.WarnLevel,
		logrus.ErrorLevel,
		logrus.FatalLevel,
		logrus.PanicLevel,
	}
}

var logrusToETWLevelMap = map[logrus.Level]etw.Level{
	logrus.PanicLevel: etw.LevelAlways,
	logrus.FatalLevel: etw.LevelCritical,
	logrus.ErrorLevel: etw.LevelError,
	logrus.WarnLevel:  etw.LevelWarning,
	logrus.InfoLevel:  etw.LevelInfo,
	logrus.DebugLevel: etw.LevelVerbose,
	logrus.TraceLevel: etw.LevelVerbose,
}

// Fire receives each Logrus entry as it is logged, and logs it to ETW.
func (h *Hook) Fire(e *logrus.Entry) error {
	// Logrus defines more levels than ETW typically uses, but analysis is
	// easiest when using a consistent set of levels across ETW providers, so we
	// map the Logrus levels to ETW levels.
	level := logrusToETWLevelMap[e.Level]
	if !h.provider.IsEnabledForLevel(level) {
		return nil
	}

	// Reserve extra space for the message field.
	fields := make([]etw.FieldOpt, 0, len(e.Data)+1)

	fields = append(fields, etw.StringField("Message", e.Message))

	for k, v := range e.Data {
		fields = append(fields, etw.SmartField(k, v))
	}

	return h.provider.WriteEvent(
		"LogrusEntry",
		etw.WithEventOpts(etw.WithLevel(level)),
		fields)
}

// Close cleans up the hook and closes the ETW provider. If the provder was
// registered by etwlogrus, it will be closed as part of `Close`. If the
// provider was passed in, it will not be closed.
func (h *Hook) Close() error {
	if h.closeProvider {
		return h.provider.Close()
	}
	return nil
}
