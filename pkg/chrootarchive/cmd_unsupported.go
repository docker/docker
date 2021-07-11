// +build !linux

package chrootarchive

import "os/exec"

func configureSysProc(cmd *exec.Cmd) {
}

func setupMountNS() error {
	return nil
}

func dropCapabilities() error {
	return nil
}
