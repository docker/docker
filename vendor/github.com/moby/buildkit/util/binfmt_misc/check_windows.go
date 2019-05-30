// +build windows

package binfmt_misc

import (
	"os/exec"
)

func withChroot(cmd *exec.Cmd, dir string) {
}
