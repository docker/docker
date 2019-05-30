package binfmt_misc

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
)

func check(bin string) error {
	tmpdir, err := ioutil.TempDir("", "qemu-check")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)
	pp := filepath.Join(tmpdir, "check")

	r, err := gzip.NewReader(bytes.NewReader([]byte(bin)))
	if err != nil {
		return err
	}
	defer r.Close()

	f, err := os.OpenFile(pp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0700)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	f.Close()

	cmd := exec.Command("/check")
	withChroot(cmd, tmpdir)
	err = cmd.Run()
	return err
}
