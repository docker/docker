package container // import "github.com/docker/docker/container"

import (
	"time"

	"github.com/sirupsen/logrus"
)

const (
	loggerCloseTimeout = 10 * time.Second
)

// Reset puts a container into a state where it can be restarted again.
func (container *Container) Reset(lock bool) {
	if lock {
		container.Lock()
		defer container.Unlock()
	}

	if err := container.CloseStreams(); err != nil {
		logrus.Errorf("%s: %s", container.ID, err)
	}

	if container.LogDriver != nil {
		if container.LogCopier != nil {
			exit := make(chan struct{})
			go func() {
				container.LogCopier.Wait()
				close(exit)
			}()

			timer := time.NewTimer(loggerCloseTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				logrus.Warn("Logger didn't exit in time: logs may be truncated")
			case <-exit:
			}
		}
		container.LogDriver.Close()
		container.LogCopier = nil
		container.LogDriver = nil
	}
}
