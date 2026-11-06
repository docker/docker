package cache

import (
	"archive/tar"
	"io"
	"path"
	"strings"

	"github.com/moby/sys/user"
	"github.com/pkg/errors"
)

// withIDMap returns writeTar, with the owners of the tar entries it writes
// mapped to the container IDs of idmap.
func withIDMap(writeTar func(io.Writer) error, idmap *user.IdentityMapping) func(io.Writer) error {
	if idmap == nil || idmap.Empty() {
		return writeTar
	}
	return func(w io.Writer) error {
		pr, pw := io.Pipe()
		mapped := make(chan error, 1)
		go func() {
			err := mapTar(w, pr, idmap)
			pr.CloseWithError(err)
			mapped <- err
		}()
		err := writeTar(pw)
		pw.CloseWithError(err)
		if mapErr := <-mapped; err == nil {
			err = mapErr
		}
		return err
	}
}

func mapTar(w io.Writer, r io.Reader, idmap *user.IdentityMapping) error {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		// Whiteouts are not owned by anyone, and are written with ID 0.
		if !strings.HasPrefix(path.Base(hdr.Name), ".wh.") {
			hdr.Uid, hdr.Gid, err = idmap.ToContainer(hdr.Uid, hdr.Gid)
			if err != nil {
				return errors.Wrapf(err, "failed to map the owner of %q", hdr.Name)
			}
		}
		capsToV2(hdr)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	// Drain the padding after the end of the archive.
	_, err := io.Copy(io.Discard, r)
	return err
}

const (
	paxCapsKey      = "SCHILY.xattr.security.capability"
	xattrCapsKey    = "security.capability"
	xattrCapsSz2    = 20
	versionOffset   = 3
	vfsCapRevision2 = 2
	vfsCapRevision3 = 3
)

// capsToV2 converts version 3 file capabilities, which store the host ID of
// the root of the user namespace that set them, to version 2.
func capsToV2(hdr *tar.Header) {
	caps, ok := hdr.PAXRecords[paxCapsKey]
	if !ok || len(caps) <= versionOffset || caps[versionOffset] != vfsCapRevision3 {
		return
	}
	v2 := []byte(caps)
	v2[versionOffset] = vfsCapRevision2
	v2 = v2[:min(len(v2), xattrCapsSz2)]

	hdr.PAXRecords[paxCapsKey] = string(v2)
	if _, ok := hdr.Xattrs[xattrCapsKey]; ok { //nolint:staticcheck // the tar reader sets Xattrs, which takes precedence when writing.
		hdr.Xattrs[xattrCapsKey] = string(v2) //nolint:staticcheck // the tar reader sets Xattrs, which takes precedence when writing.
	}
}
