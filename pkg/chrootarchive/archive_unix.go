// +build !windows

package chrootarchive // import "github.com/docker/docker/pkg/chrootarchive"

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/reexec"
	"github.com/pkg/errors"
)

// untar is the entry-point for docker-untar on re-exec. This is not used on
// Windows as it does not support chroot, hence no point sandboxing through
// chroot and rexec.
func untar() {
	runtime.LockOSThread()
	if err := setupMountNS(); err != nil {
		fatal(err)
	}

	flag.Parse()

	var options archive.TarOptions

	// read the options from the pipe "ExtraFiles"
	if err := json.NewDecoder(os.NewFile(3, "options")).Decode(&options); err != nil {
		fatal(err)
	}

	dst := flag.Arg(0)
	var root string
	if len(flag.Args()) > 1 {
		root = flag.Arg(1)
	}

	if root == "" {
		root = dst
	}

	if err := chroot(root); err != nil {
		fatal(err)
	}

	if err := dropCapabilities(); err != nil {
		fatal(err)
	}

	if err := archive.Unpack(os.Stdin, dst, &options); err != nil {
		fatal(err)
	}
	// fully consume stdin in case it is zero padded
	if _, err := flush(os.Stdin); err != nil {
		fatal(err)
	}

	os.Exit(0)
}

func invokeUnpack(decompressedArchive io.Reader, dest string, options *archive.TarOptions, root string) error {
	if root == "" {
		return errors.New("must specify a root to chroot to")
	}

	// We can't pass a potentially large exclude list directly via cmd line
	// because we easily overrun the kernel's max argument/environment size
	// when the full image list is passed (e.g. when this is used by
	// `docker load`). We will marshall the options via a pipe to the
	// child
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("Untar pipe failure: %v", err)
	}

	if root != "" {
		relDest, err := filepath.Rel(root, dest)
		if err != nil {
			return err
		}
		if relDest == "." {
			relDest = "/"
		}
		if relDest[0] != '/' {
			relDest = "/" + relDest
		}
		dest = relDest
	}

	cmd := reexec.Command("docker-untar", dest, root)
	cmd.Stdin = decompressedArchive
	configureSysProc(cmd)

	cmd.ExtraFiles = append(cmd.ExtraFiles, r)
	output := bytes.NewBuffer(nil)
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Start(); err != nil {
		w.Close()
		return fmt.Errorf("Untar error on re-exec cmd: %v", err)
	}

	// write the options to the pipe for the untar exec to read
	if err := json.NewEncoder(w).Encode(options); err != nil {
		w.Close()
		return fmt.Errorf("Untar json encode to pipe failed: %v", err)
	}
	w.Close()

	if err := cmd.Wait(); err != nil {
		// when `xz -d -c -q | docker-untar ...` failed on docker-untar side,
		// we need to exhaust `xz`'s output, otherwise the `xz` side will be
		// pending on write pipe forever
		io.Copy(ioutil.Discard, decompressedArchive)

		return fmt.Errorf("Error processing tar file(%v): %s", err, output)
	}
	return nil
}

func tar() {
	runtime.LockOSThread()

	if err := setupMountNS(); err != nil {
		fatal(err)
	}

	flag.Parse()

	src := flag.Arg(0)
	var root string
	if len(flag.Args()) > 1 {
		root = flag.Arg(1)
	}

	if root == "" {
		root = src
	}

	if err := realChroot(root); err != nil {
		fatal(err)
	}

	if err := dropCapabilities(); err != nil {
		fatal(errors.Wrap(err, "error dropping capabilities"))
	}

	var options archive.TarOptions
	if err := json.NewDecoder(os.Stdin).Decode(&options); err != nil {
		fatal(err)
	}

	rdr, err := archive.TarWithOptions(src, &options)
	if err != nil {
		fatal(err)
	}
	defer rdr.Close()

	if _, err := io.Copy(os.Stdout, rdr); err != nil {
		fatal(err)
	}

	os.Exit(0)
}

func invokePack(srcPath string, options *archive.TarOptions, root string) (io.ReadCloser, error) {
	if root == "" {
		return nil, errors.New("root path must not be empty")
	}

	relSrc, err := filepath.Rel(root, srcPath)
	if err != nil {
		return nil, err
	}
	if relSrc == "." {
		relSrc = "/"
	}
	if relSrc[0] != '/' {
		relSrc = "/" + relSrc
	}

	// make sure we didn't trim a trailing slash with the call to `Rel`
	if strings.HasSuffix(srcPath, "/") && !strings.HasSuffix(relSrc, "/") {
		relSrc += "/"
	}

	cmd := reexec.Command("docker-tar", relSrc, root)
	configureSysProc(cmd)

	errBuff := bytes.NewBuffer(nil)
	cmd.Stderr = errBuff

	tarR, tarW := io.Pipe()
	cmd.Stdout = tarW

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.Wrap(err, "error getting options pipe for tar process")
	}

	if err := cmd.Start(); err != nil {
		return nil, errors.Wrap(err, "tar error on re-exec cmd")
	}

	go func() {
		err := cmd.Wait()
		err = errors.Wrapf(err, "error processing tar file: %s", errBuff)
		tarW.CloseWithError(err)
	}()

	if err := json.NewEncoder(stdin).Encode(options); err != nil {
		stdin.Close()
		return nil, errors.Wrap(err, "tar json encode to pipe failed")
	}
	stdin.Close()

	return tarR, nil
}
