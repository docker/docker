// +build !windows

package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"os"
	"path/filepath"

	"github.com/docker/docker/pkg/containerfs"
	"github.com/docker/docker/pkg/idtools"
)

func fixPermissions(source, destination string, identity idtools.Identity, overrideSkip bool) error {
	var (
		skipChownRoot bool
		err           error
	)
	if !overrideSkip {
		destEndpoint := &copyEndpoint{driver: containerfs.NewLocalDriver(), path: destination}
		skipChownRoot, err = isExistingDirectory(destEndpoint)
		if err != nil {
			return err
		}
	}

	// We Walk on the source rather than on the destination because we don't
	// want to change permissions on things we haven't created or modified.
	return filepath.Walk(source, func(fullpath string, _ os.FileInfo, _ error) error {
		// Do not alter the walk root iff. it existed before, as it doesn't fall under
		// the domain of "things we should chown".
		if skipChownRoot && source == fullpath {
			return nil
		}

		// Path is prefixed by source: substitute with destination instead.
		cleaned, err := filepath.Rel(source, fullpath)
		if err != nil {
			return err
		}

		srcSt, err := os.Stat(source)
		if err != nil {
			return err
		}

		fullpath = filepath.Join(destination, cleaned)
		err = os.Lchown(fullpath, identity.UID, identity.GID)
		if err != nil {
			return err
		}

		return os.Chmod(fullpath, srcSt.Mode())
	})
}

func validateCopySourcePath(imageSource *imageMount, origPath, platform string) error {
	return nil
}
