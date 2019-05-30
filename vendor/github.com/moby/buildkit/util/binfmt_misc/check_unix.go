// +build !windows

package binfmt_misc

import (
	"os/exec"
	"syscall"
)

func withChroot(cmd *exec.Cmd, dir string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot: dir,
	}
}
