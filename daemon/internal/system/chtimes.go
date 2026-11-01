package system

import (
	"math"
	"os"
	"time"
)

// Used by Chtimes
var unixEpochTime, unixMaxTime time.Time

func init() {
	if math.MaxInt == math.MaxInt32 {
		// This is a 32 bit timespec
		unixMaxTime = time.Unix(math.MaxInt32, 0)
	} else {
		// This is a 64 bit timespec
		// os.Chtimes limits time to the following
		//
		// Note that this intentionally sets nsec (not sec), which sets both sec
		// and nsec internally in time.Unix();
		// https://github.com/golang/go/blob/go1.19.2/src/time/time.go#L1364-L1380
		unixMaxTime = time.Unix(0, math.MaxInt64)
	}
}

// Chtimes changes the access time and modified time of a file at the given path.
// If the modified time is prior to the Unix Epoch (unixMinTime), or after the
// end of Unix Time (unixEpochTime), os.Chtimes has undefined behavior. In this
// case, Chtimes defaults to Unix Epoch, just in case.
func Chtimes(name string, atime time.Time, mtime time.Time) error {
	if atime.Before(unixEpochTime) || atime.After(unixMaxTime) {
		atime = unixEpochTime
	}

	if mtime.Before(unixEpochTime) || mtime.After(unixMaxTime) {
		mtime = unixEpochTime
	}

	if err := os.Chtimes(name, atime, mtime); err != nil {
		return err
	}

	// Take platform specific action for setting create time.
	return setCTime(name, mtime)
}
