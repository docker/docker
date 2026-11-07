package container

import (
	"context"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/logger"
)

const (
	loggerCloseTimeout = 10 * time.Second
)

// Reset puts a container into a state where it can be restarted again.
//
// Callers are expected to obtain a lock on the container.
func (container *Container) Reset() {
	if err := container.CloseStreams(); err != nil {
		log.G(context.TODO()).WithFields(log.Fields{
			"container": container.ID,
			"error":     err,
		}).Error("failed to close container streams")
	}

	// Re-create a brand new stdin pipe once the container exited
	if container.Config.OpenStdin {
		container.StreamConfig.NewInputPipes()
	}

	if container.LogDriver == nil {
		return
	}

	// Detach the log driver from the container under the lock, then
	// close it asynchronously. Closing can block for a long time if
	// the underlying driver is unresponsive (e.g. a stuck fluentd or
	// syslog backend). Holding the container lock during that time
	// would prevent any other operation on this container, and also
	// stall startup of other containers that share the same log
	// driver configuration.
	logDriver := container.LogDriver
	logCopier := container.LogCopier
	container.LogDriver = nil
	container.LogCopier = nil

	go closeLogger(container.ID, logDriver, logCopier)
}

// closeLogger waits for the log copier to finish (with a timeout) and
// then closes the log driver. It runs in its own goroutine because the
// driver's Close method may block indefinitely when the underlying
// logging backend is unresponsive.
func closeLogger(containerID string, logDriver logger.Logger, logCopier *logger.Copier) {
	if logCopier != nil {
		exit := make(chan struct{})
		go func() {
			logCopier.Wait()
			close(exit)
		}()

		timer := time.NewTimer(loggerCloseTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			log.G(context.TODO()).WithFields(log.Fields{
				"container": containerID,
			}).Warn("logger didn't exit in time: logs may be truncated")
		case <-exit:
		}
	}
	if err := logDriver.Close(); err != nil {
		log.G(context.TODO()).WithFields(log.Fields{
			"container": containerID,
			"error":     err,
		}).Warn("error closing log driver")
	}
}
