package daemon

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/mount"
)

// cleanupMounts umounts shm/mqueue mounts for old containers
func (daemon *Daemon) cleanupMounts() error {
	logrus.Debugf("Cleaning up old shm/mqueue mounts: start.")
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()

	return daemon.cleanupMountsFromReader(f, mount.Unmount)
}

func (daemon *Daemon) cleanupMountsFromReader(reader io.Reader, unmount func(target string) error) error {
	sc := bufio.NewScanner(reader)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Split(line, " ")
		if strings.HasPrefix(fields[4], daemon.repository) {
			mnt := fields[4]
			mountBase := filepath.Base(mnt)
			if mountBase == "mqueue" || mountBase == "shm" {
				logrus.Debugf("Unmounting %+v", mnt)
				if err := unmount(mnt); err != nil {
					return err
				}
			}
		}
	}

	if err := sc.Err(); err != nil {
		return err
	}

	logrus.Debugf("Cleaning up old shm/mqueue mounts: done.")
	return nil
}
