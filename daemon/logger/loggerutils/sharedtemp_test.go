package loggerutils

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestSharedTempFileConverter(t *testing.T) {
	t.Parallel()

	t.Run("OneReaderAtATime", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		name := filepath.Join(dir, "test.txt")
		createFile(t, name, "hello, world!")

		uut := newSharedTempFileConverter(copyTransform(strings.ToUpper))
		uut.TempDir = dir

		for i := range 3 {
			t.Logf("Iteration %v", i)

			rdr := convertPath(t, uut, name)
			assert.Check(t, is.Equal("HELLO, WORLD!", readAll(t, rdr)))
			assert.Check(t, rdr.Close())
			assert.Check(t, is.Equal(fs.ErrClosed, rdr.Close()), "closing an already-closed reader should return an error")
		}

		assert.NilError(t, os.Remove(name))
		checkDirEmpty(t, dir)
	})

	t.Run("RobustToRenames", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		apath := filepath.Join(dir, "test.txt")
		createFile(t, apath, "file a")

		var conversions int
		uut := newSharedTempFileConverter(
			func(dst io.WriteSeeker, src io.ReadSeeker) error {
				conversions++
				return copyTransform(strings.ToUpper)(dst, src)
			},
		)
		uut.TempDir = dir

		ra1 := convertPath(t, uut, apath)

		// Rotate the file to a new name and write a new file in its place.
		bpath := apath
		apath = filepath.Join(dir, "test2.txt")
		assert.NilError(t, os.Rename(bpath, apath))
		createFile(t, bpath, "file b")

		rb1 := convertPath(t, uut, bpath) // Same path, different file.
		ra2 := convertPath(t, uut, apath) // New path, old file.
		assert.Check(t, is.Equal(2, conversions), "expected only one conversion per unique file")

		// Interleave reading and closing to shake out ref-counting bugs:
		// closing one reader shouldn't affect any other open readers.
		assert.Check(t, is.Equal("FILE A", readAll(t, ra1)))
		assert.NilError(t, ra1.Close())
		assert.Check(t, is.Equal("FILE A", readAll(t, ra2)))
		assert.NilError(t, ra2.Close())
		assert.Check(t, is.Equal("FILE B", readAll(t, rb1)))
		assert.NilError(t, rb1.Close())

		assert.NilError(t, os.Remove(apath))
		assert.NilError(t, os.Remove(bpath))
		checkDirEmpty(t, dir)
	})

	t.Run("ConcurrentRequests", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		name := filepath.Join(dir, "test.txt")
		createFile(t, name, "hi there")

		var conversions atomic.Uint32
		notify := make(chan chan struct{}, 1)
		firstConversionStarted := make(chan struct{})
		notify <- firstConversionStarted
		unblock := make(chan struct{})
		uut := newSharedTempFileConverter(
			func(dst io.WriteSeeker, src io.ReadSeeker) error {
				t.Log("Convert: enter")
				defer t.Log("Convert: exit")
				select {
				case c := <-notify:
					close(c)
				default:
				}
				<-unblock
				conversions.Add(1)
				return copyTransform(strings.ToUpper)(dst, src)
			},
		)
		uut.TempDir = dir

		closers := make(chan io.Closer, 4)
		var wg sync.WaitGroup
		wg.Add(3)
		for i := range 3 {
			go func() {
				defer wg.Done()
				t.Logf("goroutine %v: enter", i)
				defer t.Logf("goroutine %v: exit", i)
				f := convertPath(t, uut, name)
				assert.Check(t, is.Equal("HI THERE", readAll(t, f)), "in goroutine %v", i)
				closers <- f
			}()
		}

		select {
		case <-firstConversionStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("the first conversion should have started by now")
		}
		close(unblock)
		t.Log("starting wait")
		wg.Wait()
		t.Log("wait done")

		f := convertPath(t, uut, name)
		closers <- f
		close(closers)
		assert.Check(t, is.Equal("HI THERE", readAll(t, f)), "after all goroutines returned")
		for c := range closers {
			assert.Check(t, c.Close())
		}

		assert.Check(t, is.Equal(uint32(1), conversions.Load()))

		assert.NilError(t, os.Remove(name))
		checkDirEmpty(t, dir)
	})

	t.Run("ConvertError", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		name := filepath.Join(dir, "test.txt")
		createFile(t, name, "hi there")
		src, err := open(name)
		assert.NilError(t, err)
		defer func() { _ = src.Close() }()

		fakeErr := errors.New("fake error")
		var start sync.WaitGroup
		start.Add(3)
		uut := newSharedTempFileConverter(
			func(dst io.WriteSeeker, src io.ReadSeeker) error {
				start.Wait()
				runtime.Gosched()
				if fakeErr != nil {
					return fakeErr
				}
				return copyTransform(strings.ToUpper)(dst, src)
			},
		)
		uut.TempDir = dir

		var done sync.WaitGroup
		done.Add(3)
		for i := range 3 {
			go func() {
				defer done.Done()
				t.Logf("goroutine %v: enter", i)
				defer t.Logf("goroutine %v: exit", i)
				start.Done()
				_, err := uut.Do(src)
				assert.Check(t, is.ErrorIs(err, fakeErr), "in goroutine %v", i)
			}()
		}
		done.Wait()

		// Conversion errors should not be "sticky". A subsequent
		// request should retry from scratch.
		fakeErr = errors.New("another fake error")
		_, err = uut.Do(src)
		assert.Check(t, is.ErrorIs(err, fakeErr))

		fakeErr = nil
		f, err := uut.Do(src)
		assert.Check(t, err)
		assert.Check(t, is.Equal("HI THERE", readAll(t, f)))
		assert.Check(t, f.Close())

		// Files pending delete continue to show up in directory
		// listings on Windows RS5. Close the remaining handle before
		// deleting the file to prevent spurious failures with
		// checkDirEmpty.
		assert.Check(t, src.Close())
		assert.NilError(t, os.Remove(name))
		checkDirEmpty(t, dir)
	})
}

func createFile(t *testing.T, path string, content string) {
	t.Helper()
	f, err := openFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	assert.NilError(t, err)
	_, err = io.WriteString(f, content)
	assert.NilError(t, err)
	assert.NilError(t, f.Close())
}

func convertPath(t *testing.T, uut *sharedTempFileConverter, path string) *sharedFileReader {
	t.Helper()
	f, err := open(path)
	assert.NilError(t, err)
	defer func() { assert.NilError(t, f.Close()) }()
	r, err := uut.Do(f)
	assert.NilError(t, err)
	return r
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	v, err := io.ReadAll(r)
	assert.NilError(t, err)
	return string(v)
}

func checkDirEmpty(t *testing.T, path string) {
	t.Helper()
	ls, err := os.ReadDir(path)
	assert.NilError(t, err)
	assert.Check(t, is.Len(ls, 0), "directory should be free of temp files")
}

func copyTransform(f func(string) string) func(dst io.WriteSeeker, src io.ReadSeeker) error {
	return func(dst io.WriteSeeker, src io.ReadSeeker) error {
		s, err := io.ReadAll(src)
		if err != nil {
			return err
		}
		_, err = io.WriteString(dst, f(string(s)))
		return err
	}
}
