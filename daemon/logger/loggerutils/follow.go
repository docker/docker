package loggerutils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/logger"
)

type follow struct {
	LogFile   *LogFile
	Watcher   *logger.LogWatcher
	Decoder   Decoder
	Forwarder *forwarder

	log *log.Entry
	c   chan logPos
}

// Do follows the log file as it is written, starting from f at read.
func (fl *follow) Do(ctx context.Context, f *os.File, read logPos) {
	fl.log = log.G(ctx).WithFields(log.Fields{
		"module": "logger",
		"file":   f.Name(),
	})
	// Optimization: allocate the write-notifications channel only once and
	// reuse it for multiple invocations of nextPos().
	fl.c = make(chan logPos, 1)

	defer func() {
		if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			fl.log.WithError(err).Warn("error closing current log file")
		}
	}()

	for {
		wrote, ok := fl.nextPos(ctx, read)
		if !ok {
			return
		}

		if wrote.rotation != read.rotation {
			// Flush the current file before moving on to the next.
			if _, err := f.Seek(read.size, io.SeekStart); err != nil {
				fl.Watcher.Err <- err
				return
			}
			if !fl.forward(ctx, f) {
				return
			}

			// Open the new file, which has the same name as the old
			// file thanks to file rotation. Make no mistake: they
			// are different files, with distinct identities.
			// Atomically capture the wrote position to make
			// absolutely sure that the position corresponds to the
			// file we have opened; more rotations could have
			// occurred since we previously received it.
			if err := f.Close(); err != nil {
				fl.log.WithError(err).Warn("error closing rotated log file")
			}
			var err error
			func() {
				fl.LogFile.fsopMu.RLock()
				st := <-fl.LogFile.read
				defer func() {
					fl.LogFile.read <- st
					fl.LogFile.fsopMu.RUnlock()
				}()
				f, err = open(f.Name())
				wrote = st.pos
			}()
			// We tried to open the file inside a critical section
			// so we shouldn't have been racing the rotation of the
			// file. Any error, even fs.ErrNotFound, is exceptional.
			if err != nil {
				fl.Watcher.Err <- fmt.Errorf("logger: error opening log file for follow after rotation: %w", err)
				return
			}

			if nrot := wrote.rotation - read.rotation; nrot > 1 {
				fl.log.WithField("missed-rotations", nrot).
					Warn("file rotations were missed while following logs; some log messages have been skipped over")
			}

			// Set up our read position to start from the top of the file.
			read.size = 0
		}

		if !fl.forward(ctx, io.NewSectionReader(f, read.size, wrote.size-read.size)) {
			return
		}
		read = wrote
	}
}

// nextPos waits until the write position of the LogFile being followed has
// advanced from current and returns the new position.
func (fl *follow) nextPos(ctx context.Context, current logPos) (next logPos, ok bool) {
	var st logReadState
	select {
	case <-ctx.Done():
		return current, false
	case <-fl.Watcher.WatchConsumerGone():
		return current, false
	case st = <-fl.LogFile.read:
	}

	// Have any logs been written since we last checked?
	if st.pos == current { // Nope.
		// Add ourself to the notify list.
		st.wait = append(st.wait, fl.c)
	} else { // Yes.
		// "Notify" ourself immediately.
		fl.c <- st.pos
	}
	fl.LogFile.read <- st

	select {
	case <-fl.LogFile.closed: // No more logs will be written.
		select { // Have we followed to the end?
		case next = <-fl.c: // No: received a new position.
		default: // Yes.
			return current, false
		}
	case <-fl.Watcher.WatchConsumerGone():
		return current, false
	case next = <-fl.c:
	}
	return next, true
}

// forward decodes log messages from r and forwards them to the log watcher.
//
// The return value, cont, signals whether following should continue.
func (fl *follow) forward(ctx context.Context, r io.Reader) (cont bool) {
	fl.Decoder.Reset(r)
	return fl.Forwarder.Do(ctx, fl.Watcher, fl.Decoder.Decode)
}
