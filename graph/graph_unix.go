// +build !windows

package graph

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/system"
	"github.com/vbatts/tar-split/tar/asm"
	"github.com/vbatts/tar-split/tar/storage"
)

// setupInitLayer populates a directory with mountpoints suitable
// for bind-mounting dockerinit into the container. The mountpoint is simply an
// empty file at /.dockerinit
//
// This extra layer is used by all containers as the top-most ro layer. It protects
// the container from unwanted side-effects on the rw layer.
func SetupInitLayer(initLayer string) error {
	for pth, typ := range map[string]string{
		"/dev/pts":         "dir",
		"/dev/shm":         "dir",
		"/proc":            "dir",
		"/sys":             "dir",
		"/.dockerinit":     "file",
		"/.dockerenv":      "file",
		"/etc/resolv.conf": "file",
		"/etc/hosts":       "file",
		"/etc/hostname":    "file",
		"/dev/console":     "file",
		"/etc/mtab":        "/proc/mounts",
	} {
		parts := strings.Split(pth, "/")
		prev := "/"
		for _, p := range parts[1:] {
			prev = filepath.Join(prev, p)
			syscall.Unlink(filepath.Join(initLayer, prev))
		}

		if _, err := os.Stat(filepath.Join(initLayer, pth)); err != nil {
			if os.IsNotExist(err) {
				if err := system.MkdirAll(filepath.Join(initLayer, filepath.Dir(pth)), 0755); err != nil {
					return err
				}
				switch typ {
				case "dir":
					if err := system.MkdirAll(filepath.Join(initLayer, pth), 0755); err != nil {
						return err
					}
				case "file":
					f, err := os.OpenFile(filepath.Join(initLayer, pth), os.O_CREATE, 0755)
					if err != nil {
						return err
					}
					f.Close()
				default:
					if err := os.Symlink(typ, filepath.Join(initLayer, pth)); err != nil {
						return err
					}
				}
			} else {
				return err
			}
		}
	}

	// Layer is ready to use, if it wasn't before.
	return nil
}

func createRootFilesystemInDriver(graph *Graph, img *image.Image, layerData archive.ArchiveReader) error {
	if err := graph.driver.Create(img.ID, img.Parent); err != nil {
		return fmt.Errorf("Driver %s failed to create image rootfs %s: %s", graph.driver, img.ID, err)
	}
	return nil
}

func (graph *Graph) restoreBaseImages() ([]string, error) {
	return nil, nil
}

// storeImage stores file system layer data for the given image to the
// graph's storage driver. Image metadata is stored in a file
// at the specified root directory.
func (graph *Graph) storeImage(img *image.Image, layerData archive.ArchiveReader, root string) (err error) {
	// Store the layer. If layerData is not nil, unpack it into the new layer
	if layerData != nil {
		// this is saving the tar-split metadata
		mf, err := os.OpenFile(filepath.Join(root, tarDataFileName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(0600))
		if err != nil {
			return err
		}
		defer mf.Close()
		mfz := gzip.NewWriter(mf)
		defer mfz.Close()
		metaPacker := storage.NewJSONPacker(mfz)

		inflatedLayerData, err := archive.DecompressStream(layerData)
		if err != nil {
			return err
		}

		// we're passing nil here for the file putter, because the ApplyDiff will
		// handle the extraction of the archive
		its, err := asm.NewInputTarStream(inflatedLayerData, metaPacker, nil)
		if err != nil {
			return err
		}

		if img.Size, err = graph.driver.ApplyDiff(img.ID, img.Parent, archive.ArchiveReader(its)); err != nil {
			return err
		}
	}

	if err := graph.saveSize(root, int(img.Size)); err != nil {
		return err
	}

	f, err := os.OpenFile(jsonPath(root), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(0600))
	if err != nil {
		return err
	}

	defer f.Close()

	return json.NewEncoder(f).Encode(img)
}

// TarLayer returns a tar archive of the image's filesystem layer.
func (graph *Graph) TarLayer(img *image.Image) (arch archive.Archive, err error) {
	root := graph.imageRoot(img.ID)
	mFileName := filepath.Join(root, tarDataFileName)
	mf, err := os.Open(mFileName)
	if err != nil {
		if !os.IsNotExist(err) {
			logrus.Errorf("failed to open %q: %s", mFileName, err)
		}
		logrus.Debugf("[graph] TarLayer with traditional differ: %s", img.ID)
		return graph.driver.Diff(img.ID, img.Parent)
	}
	pR, pW := io.Pipe()
	// this will need to be in a goroutine, as we are returning the stream of a
	// tar archive, but can not close the metadata reader early (when this
	// function returns)...
	go func() {
		defer mf.Close()
		// let's reassemble!
		logrus.Debugf("[graph] TarLayer with reassembly: %s", img.ID)
		mfz, err := gzip.NewReader(mf)
		if err != nil {
			pW.CloseWithError(fmt.Errorf("[graph] error with %s:  %s", mFileName, err))
			return
		}
		defer mfz.Close()

		// get our relative path to the container
		fsLayer, err := graph.driver.Get(img.ID, "")
		if err != nil {
			pW.CloseWithError(err)
			return
		}
		defer graph.driver.Put(img.ID)

		metaUnpacker := storage.NewJSONUnpacker(mfz)
		fileGetter := storage.NewPathFileGetter(fsLayer)
		logrus.Debugf("[graph] %s is at %q", img.ID, fsLayer)
		ots := asm.NewOutputTarStream(fileGetter, metaUnpacker)
		defer ots.Close()
		if _, err := io.Copy(pW, ots); err != nil {
			pW.CloseWithError(err)
			return
		}
		pW.Close()
	}()
	return pR, nil
}
