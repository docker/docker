// Package fluentd provides the log driver for forwarding server logs
// to fluentd endpoints.
package fluentd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/log"
	"github.com/docker/go-units"
	"github.com/fluent/fluent-logger-golang/fluent"
	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/logger/loggerutils"
	"github.com/moby/moby/v2/errdefs"
)

type fluentd struct {
	tag           string
	containerID   string
	containerName string
	writer        *fluent.Fluent
	extra         map[string]string
}

type location struct {
	protocol string
	host     string
	port     int
	path     string
}

const (
	name = "fluentd"

	defaultBufferLimit = 1024 * 1024
	defaultHost        = "127.0.0.1"
	defaultPort        = 24224
	defaultProtocol    = "tcp"

	// logger tries to reconnect 2**32 - 1 times
	// failed (and panic) after 204 years [ 1.5 ** (2**32 - 1) - 1 seconds]
	defaultMaxRetries = math.MaxInt32
	defaultRetryWait  = 1000

	minReconnectInterval = 100 * time.Millisecond
	maxReconnectInterval = 10 * time.Second

	addressKey                = "fluentd-address"
	asyncKey                  = "fluentd-async"
	asyncReconnectIntervalKey = "fluentd-async-reconnect-interval"
	bufferLimitKey            = "fluentd-buffer-limit"
	maxRetriesKey             = "fluentd-max-retries"
	requestAckKey             = "fluentd-request-ack"
	retryWaitKey              = "fluentd-retry-wait"
	subSecondPrecisionKey     = "fluentd-sub-second-precision"
	// writeTimeoutKey can be used to specify the WriteTimeout config for fluentd.
	// Ref: https://github.com/fluent/fluent-logger-golang/blob/5538e904aeb515c10a624da620581bdf420d4b8a/fluent/fluent.go#L55
	// This allows fluentd to give up unhealthy connections and not be blocked forever
	// when downstream connections get unhealthy.
	writeTimeoutKey = "fluentd-write-timeout"
	// readTimeoutKey can be used to specify the ReadTimeout config for fluentd connections.
	readTimeoutKey = "fluentd-read-timeout"
)

// New creates a fluentd logger using the configuration passed in on
// the context. The supported context configuration variable is
// fluentd-address.
func New(info logger.Info) (logger.Logger, error) {
	fluentConfig, err := parseConfig(info.Config)
	if err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	tag, err := loggerutils.ParseLogTag(info, loggerutils.DefaultTemplate)
	if err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	extraAttrs, err := info.ExtraAttributes(nil)
	if err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	log.G(context.TODO()).WithFields(log.Fields{
		"container": info.ContainerID,
		"config":    fluentConfig,
	}).Debug("logging driver fluentd configured")

	writer, err := fluent.New(fluentConfig)
	if err != nil {
		return nil, err
	}
	return &fluentd{
		tag:           tag,
		containerID:   info.ContainerID,
		containerName: info.ContainerName,
		writer:        writer,
		extra:         extraAttrs,
	}, nil
}

func (f *fluentd) Log(msg *logger.Message) (err error) {
	defer func() {
		if err == nil {
			logger.PutMessage(msg)
		}
	}()

	data := map[string]string{
		"container_id":   f.containerID,
		"container_name": f.containerName,
		"source":         msg.Source,
		"log":            string(msg.Line),
	}
	maps.Copy(data, f.extra)
	if msg.PLogMetaData != nil {
		data["partial_message"] = "true"
		data["partial_id"] = msg.PLogMetaData.ID
		data["partial_ordinal"] = strconv.Itoa(msg.PLogMetaData.Ordinal)
		data["partial_last"] = strconv.FormatBool(msg.PLogMetaData.Last)
	}

	// fluent-logger-golang buffers logs from failures and disconnections,
	// and these are transferred again automatically.
	return f.writer.PostWithTime(f.tag, msg.Timestamp, data)
}

func (f *fluentd) Close() error {
	return f.writer.Close()
}

func (f *fluentd) Name() string {
	return name
}

// ValidateLogOpt looks for fluentd specific log option fluentd-address.
func ValidateLogOpt(cfg map[string]string) error {
	for key := range cfg {
		switch key {
		case logger.AttrEnv, logger.AttrEnvRegex, logger.AttrLabels, logger.AttrLabelsRegex, logger.AttrLogTag:
			// Common attributes handled through [logger.Info.ExtraAttributes] and [loggerutils.ParseLogTag].
			continue

		case addressKey:
		case asyncKey:
		case asyncReconnectIntervalKey:
		case bufferLimitKey:
		case maxRetriesKey:
		case requestAckKey:
		case retryWaitKey:
		case subSecondPrecisionKey:
		case writeTimeoutKey:
			// Accepted
		case readTimeoutKey:
			// Accepted
		default:
			return fmt.Errorf("unknown log opt '%s' for fluentd log driver", key)
		}
	}

	_, err := parseConfig(cfg)
	return err
}

func parseConfig(cfg map[string]string) (fluent.Config, error) {
	var config fluent.Config

	loc, err := parseAddress(cfg[addressKey])
	if err != nil {
		return config, fmt.Errorf("invalid fluentd-address (%s): %w", cfg[addressKey], err)
	}

	bufferLimit := defaultBufferLimit
	if cfg[bufferLimitKey] != "" {
		bl64, err := units.RAMInBytes(cfg[bufferLimitKey])
		if err != nil {
			return config, err
		}
		bufferLimit = int(bl64)
	}

	retryWait := defaultRetryWait
	if cfg[retryWaitKey] != "" {
		rwd, err := time.ParseDuration(cfg[retryWaitKey])
		if err != nil {
			return config, err
		}
		retryWait = int(rwd.Seconds() * 1000)
	}

	maxRetries := defaultMaxRetries
	if cfg[maxRetriesKey] != "" {
		mr64, err := strconv.ParseUint(cfg[maxRetriesKey], 10, 32)
		if err != nil {
			return config, err
		}

		// cap to MaxInt32 to prevent overflowing, and which is documented on
		// defaultMaxRetries to be the limit above which things fail.
		if mr64 > math.MaxInt32 {
			return config, errors.New("invalid fluentd-max-retries: value out of range")
		}
		maxRetries = int(mr64)
	}

	async := false
	if cfg[asyncKey] != "" {
		if async, err = strconv.ParseBool(cfg[asyncKey]); err != nil {
			return config, err
		}
	}

	var asyncReconnectInterval int
	if cfg[asyncReconnectIntervalKey] != "" {
		interval, err := time.ParseDuration(cfg[asyncReconnectIntervalKey])
		if err != nil {
			return config, fmt.Errorf("invalid value for %s: %w", asyncReconnectIntervalKey, err)
		}
		if interval != 0 {
			if interval < minReconnectInterval || interval > maxReconnectInterval {
				return config, fmt.Errorf("invalid value for %s: value (%q) must be between %s and %s",
					asyncReconnectIntervalKey, interval, minReconnectInterval, maxReconnectInterval)
			}
			asyncReconnectInterval = int(interval.Milliseconds())
		}
	}

	subSecondPrecision := false
	if cfg[subSecondPrecisionKey] != "" {
		if subSecondPrecision, err = strconv.ParseBool(cfg[subSecondPrecisionKey]); err != nil {
			return config, err
		}
	}

	requestAck := false
	if cfg[requestAckKey] != "" {
		if requestAck, err = strconv.ParseBool(cfg[requestAckKey]); err != nil {
			return config, err
		}
	}

	writeTimeout := time.Duration(0)
	if cfg[writeTimeoutKey] != "" {
		if d, err := time.ParseDuration(cfg[writeTimeoutKey]); err != nil {
			return config, fmt.Errorf("invalid value for %s: value must be a duration: %w", writeTimeoutKey, err)
		} else if d < 0 {
			return config, fmt.Errorf("invalid value for %s: value must be a duration that is non-negative", writeTimeoutKey)
		} else {
			writeTimeout = d
		}
	}

	readTimeout := time.Duration(0)
	if cfg[readTimeoutKey] != "" {
		if d, err := time.ParseDuration(cfg[readTimeoutKey]); err != nil {
			return config, fmt.Errorf("invalid value for %s: value must be a duration: %w", readTimeoutKey, err)
		} else if d < 0 {
			return config, fmt.Errorf("invalid value for %s: value must be a duration that is non-negative", readTimeoutKey)
		} else {
			readTimeout = d
		}
	}

	config = fluent.Config{
		FluentPort:             loc.port,
		FluentHost:             loc.host,
		FluentNetwork:          loc.protocol,
		FluentSocketPath:       loc.path,
		BufferLimit:            bufferLimit,
		RetryWait:              retryWait,
		MaxRetry:               maxRetries,
		Async:                  async,
		AsyncReconnectInterval: asyncReconnectInterval,
		SubSecondPrecision:     subSecondPrecision,
		RequestAck:             requestAck,
		ForceStopAsyncSend:     async,
		WriteTimeout:           writeTimeout,
		ReadTimeout:            readTimeout,
	}

	return config, nil
}

func parseAddress(address string) (*location, error) {
	if address == "" {
		return &location{
			protocol: defaultProtocol,
			host:     defaultHost,
			port:     defaultPort,
			path:     "",
		}, nil
	}

	if !strings.Contains(address, "://") {
		address = defaultProtocol + "://" + address
	}

	addr, err := url.Parse(address)
	if err != nil {
		return nil, err
	}

	switch addr.Scheme {
	case "unix":
		if strings.TrimLeft(addr.Path, "/") == "" {
			return nil, errors.New("path is empty")
		}
		return &location{protocol: addr.Scheme, path: addr.Path}, nil
	case "tcp", "tls":
		// continue processing below
	default:
		return nil, fmt.Errorf("unsupported scheme: '%s'", addr.Scheme)
	}

	if addr.Path != "" {
		return nil, errors.New("should not contain a path element")
	}

	host := defaultHost
	port := defaultPort

	if h := addr.Hostname(); h != "" {
		host = h
	}
	if p := addr.Port(); p != "" {
		// Port numbers are 16 bit: https://www.ietf.org/rfc/rfc793.html#section-3.1
		portNum, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid port: %w", err)
		}
		port = int(portNum)
	}
	return &location{
		protocol: addr.Scheme,
		host:     host,
		port:     port,
		path:     "",
	}, nil
}
