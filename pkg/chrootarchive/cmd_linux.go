package chrootarchive

import (
	"os/exec"
	"syscall"

	"github.com/docker/docker/pkg/mount"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

func configureSysProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &unix.SysProcAttr{
		Cloneflags: unix.CLONE_NEWNET | unix.CLONE_NEWPID | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWNS,
		Pdeathsig:  syscall.SIGKILL,
	}
}

// As part of setting up this process we created a new mount ns and a new pid ns.
// This configures the mount ns to take advantage of that to further isolate the process.
func setupMountNS() error {
	// Make everything in new ns slave.
	// Don't use `private` here as this could race where the mountns gets a
	//   reference to a mount and an unmount from the host does not propagate,
	//   which could potentially cause transient errors for other operations,
	//   even though this should be relatively small window here `slave` should
	//   not cause any problems.
	if err := mount.MakeRSlave("/"); err != nil {
		return errors.Wrap(err, "error remounting rootfs as with slave mounts")
	}

	// Remount /proc so it is accounting for the new namespaces
	if err := mount.Unmount("/proc"); err != nil {
		return errors.Wrap(err, "error unmounting /proc")
	}

	if err := unix.Mount("proc", "/proc", "proc", 0, "hidepid=2"); err != nil {
		return errors.Wrap(err, "error remounting /proc")
	}
	return nil
}
