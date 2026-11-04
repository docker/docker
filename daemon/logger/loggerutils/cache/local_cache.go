package cache

import (
	"context"
	"fmt"
	"strconv"

	"github.com/containerd/log"
	"github.com/docker/go-units"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/logger/local"
)

const (
	// DriverName is the name of the driver used for local log caching
	DriverName = local.Name

	cachePrefix      = "cache-"
	cacheDisabledKey = cachePrefix + "disabled"
)

var builtInCacheLogOpts = map[string]bool{
	cacheDisabledKey: true,
}

var _ logger.LogReader = (*loggerWithCache)(nil)

// WithLocalCache wraps the passed in logger with a logger caches all writes locally
// in addition to writing to the passed in logger.
func WithLocalCache(l logger.Logger, info logger.Info) (logger.Logger, error) {
	initLogger, err := logger.GetLogDriver(DriverName)
	if err != nil {
		return nil, err
	}

	cacher, err := initLogger(info)
	if err != nil {
		return nil, fmt.Errorf("error initializing local log cache driver: %w", err)
	}

	if container.LogMode(info.Config["mode"]) == container.LogModeUnset || container.LogMode(info.Config["mode"]) == container.LogModeNonBlock {
		var size int64 = -1
		if s, exists := info.Config["max-buffer-size"]; exists {
			size, err = units.RAMInBytes(s)
			if err != nil {
				return nil, err
			}
		}
		cacher = logger.NewRingLogger(cacher, info, size)
	}

	return &loggerWithCache{
		l:     l,
		cache: cacher,
	}, nil
}

type loggerWithCache struct {
	l     logger.Logger
	cache logger.Logger
}

var _ logger.SizedLogger = &loggerWithCache{}

// BufSize returns the buffer size of the underlying logger.
// Returns -1 if the logger doesn't match SizedLogger interface.
func (l *loggerWithCache) BufSize() int {
	if sl, ok := l.l.(logger.SizedLogger); ok {
		return sl.BufSize()
	}
	return -1
}

func (l *loggerWithCache) Log(msg *logger.Message) error {
	// copy the message as the original will be reset once the call to `Log` is complete
	dup := logger.NewMessage()
	dumbCopyMessage(dup, msg)

	// Log the duplicate first so the caller retains ownership of the
	// original message if either logger returns an error.
	if err := l.l.Log(dup); err != nil {
		logger.PutMessage(dup)
		return err
	}
	return l.cache.Log(msg)
}

func (l *loggerWithCache) Name() string {
	return l.l.Name()
}

func (l *loggerWithCache) ReadLogs(ctx context.Context, config logger.ReadConfig) *logger.LogWatcher {
	return l.cache.(logger.LogReader).ReadLogs(ctx, config)
}

func (l *loggerWithCache) Close() error {
	err := l.l.Close()
	if err := l.cache.Close(); err != nil {
		log.G(context.TODO()).WithError(err).Warn("error while shutting cache logger")
	}
	return err
}

// ShouldUseCache reads the log opts to determine if caching should be enabled
func ShouldUseCache(cfg map[string]string) bool {
	if cfg[cacheDisabledKey] == "" {
		return true
	}
	b, err := strconv.ParseBool(cfg[cacheDisabledKey])
	if err != nil {
		// This shouldn't happen since the values are validated beforehand.
		return false
	}
	return !b
}

// dumbCopyMessage is a bit of a fake copy but avoids extra allocations which
// are not necessary for this use case.
func dumbCopyMessage(dst, src *logger.Message) {
	dst.Source = src.Source
	dst.Timestamp = src.Timestamp
	dst.PLogMetaData = src.PLogMetaData
	dst.Err = src.Err
	dst.Attrs = src.Attrs
	dst.Line = append(dst.Line[:0], src.Line...)
}
