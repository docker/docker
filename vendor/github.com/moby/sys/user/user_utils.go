package user

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// maxUserFileBytes caps how much data is read from any user-database file.
// User database files are expected to be relatively small. 10 MiB provides
// generous headroom while bounding memory usage.
const maxUserFileBytes = 10 << 20

// openUserFile attempts to open a user-database file with a limitedFile
// capped at maxUserFileBytes. It produces an error if the given path is
// a non-regular file.
func openUserFile(path string) (*limitedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, &os.PathError{
			Op:   "open",
			Path: path,
			Err:  errors.New("not a regular file"),
		}
	}

	return &limitedFile{
		File: f,
		// Allow one byte past the cap so an overflow surfaces as an
		// error rather than a silent EOF that the parser would treat as
		// a clean end-of-file (and miss any entries past the cap).
		LimitedReader: &io.LimitedReader{R: f, N: maxUserFileBytes + 1},
		name:          path,
	}, nil
}

type limitedFile struct {
	*os.File
	*io.LimitedReader
	name string
}

func (l *limitedFile) Read(p []byte) (int, error) {
	n, err := l.LimitedReader.Read(p)
	if l.LimitedReader.N == 0 {
		return n, &os.PathError{
			Op:   "read",
			Path: l.name,
			Err:  fmt.Errorf("file exceeds %d bytes", maxUserFileBytes),
		}
	}
	return n, err
}

// userArg is a parsed user argument for [GetExecUser].
type userArg struct {
	name      string
	uid       int
	isNumeric bool
}

// parseUserArg parses a user argument as either a user name or UID.
//
// If name is empty, parseUserArg returns "nil, nil" to indicate that no user
// argument was specified.
func parseUserArg(name string) (*userArg, error) {
	if name == "" {
		return nil, nil
	}
	uid, isNumeric, err := parseNumeric(name)
	if err != nil {
		return nil, err
	}
	return &userArg{name: name, uid: uid, isNumeric: isNumeric}, nil
}

// matches reports whether user u satisfies the argument. Numeric arguments
// are matched by UID only, others by name.
func (ua *userArg) matches(u User) bool {
	if ua.isNumeric {
		return u.Uid == ua.uid
	}
	return u.Name == ua.name
}

// groupArg is a parsed group argument for [GetAdditionalGroups] or [GetExecUser].
type groupArg struct {
	name      string
	gid       int
	isNumeric bool
}

// parseGroupArg parses a group argument as either a group name or GID.
//
// If name is empty, parseGroupArg returns "nil, nil" to indicate that no group
// argument was specified.
func parseGroupArg(name string) (*groupArg, error) {
	if name == "" {
		return nil, nil
	}
	gid, isNumeric, err := parseNumeric(name)
	if err != nil {
		return nil, err
	}
	return &groupArg{name: name, gid: gid, isNumeric: isNumeric}, nil
}

// matches reports whether group g satisfies the argument. Numeric arguments
// are matched by GID only, others by name.
func (ag groupArg) matches(g Group) bool {
	if ag.isNumeric {
		return g.Gid == ag.gid
	}
	return g.Name == ag.name
}
