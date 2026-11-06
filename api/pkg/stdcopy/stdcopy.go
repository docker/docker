package stdcopy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// StdType is the type of standard stream
// a writer can multiplex to.
type StdType byte

const (
	Stdin     StdType = 0 // Stdin represents standard input stream. It is present for completeness and should NOT be used. When reading the stream with [StdCopy] it is output on [Stdout].
	Stdout    StdType = 1 // Stdout represents standard output stream.
	Stderr    StdType = 2 // Stderr represents standard error steam.
	Systemerr StdType = 3 // Systemerr represents errors originating from the system. When reading the stream with [StdCopy] it is returned as an error.

	ExecStdout StdType = 4 // ExecStdout represents the standard output stream of an exec process whose output is captured in the container's logs. When reading the stream with [StdCopy] it is output on [Stdout].
	ExecStderr StdType = 5 // ExecStderr represents the standard error stream of an exec process whose output is captured in the container's logs. When reading the stream with [StdCopy] it is output on [Stderr].
)

const (
	stdWriterPrefixLen = 8
	stdWriterFdIndex   = 0
	stdWriterSizeIndex = 4

	startingBufLen = 32*1024 + stdWriterPrefixLen + 1
)

// StdCopy is a modified version of [io.Copy] to de-multiplex messages
// from "multiplexedSource" and copy them to destination streams
// "destOut" and "destErr".
//
// StdCopy demultiplexes "multiplexedSource", assuming that it contains
// two streams, previously multiplexed using a writer created with
// [NewStdWriter].
//
// As it reads from "multiplexedSource", StdCopy writes [Stdout] and
// [ExecStdout] messages to "destOut", and [Stderr] and [ExecStderr] messages
// to "destErr". For backward-compatibility, [Stdin] messages are output to
// "destOut". The [Systemerr] stream provides errors produced by the daemon.
// It is returned as an error, and terminates processing the stream. Callers
// that need to tell exec output apart from the container's own output must
// demultiplex the stream themselves based on the frame headers.
//
// StdCopy it reads until it hits [io.EOF] on "multiplexedSource", after
// which it returns a nil error. In other words: any error returned indicates
// a real underlying error, which may be when an unknown [StdType] stream
// is received.
//
// The "written" return holds the total number of bytes written to "destOut"
// and "destErr" combined.
func StdCopy(destOut, destErr io.Writer, multiplexedSource io.Reader) (written int64, _ error) {
	var (
		buf       = make([]byte, startingBufLen)
		bufLen    = len(buf)
		nr, nw    int
		err       error
		out       io.Writer
		frameSize int
	)

	for {
		// Make sure we have at least a full header
		for nr < stdWriterPrefixLen {
			var nr2 int
			nr2, err = multiplexedSource.Read(buf[nr:])
			nr += nr2
			if errors.Is(err, io.EOF) {
				if nr < stdWriterPrefixLen {
					return written, nil
				}
				break
			}
			if err != nil {
				return 0, err
			}
		}

		// Check the first byte to know where to write
		stream := StdType(buf[stdWriterFdIndex])
		switch stream {
		case Stdin:
			fallthrough
		case Stdout, ExecStdout:
			// Write on stdout
			out = destOut
		case Stderr, ExecStderr:
			// Write on stderr
			out = destErr
		case Systemerr:
			// If we're on Systemerr, we won't write anywhere.
			// NB: if this code changes later, make sure you don't try to write
			// to outstream if Systemerr is the stream
			out = nil
		default:
			return 0, fmt.Errorf("unrecognized stream: %d", stream)
		}

		// Retrieve the size of the frame
		frameSize = int(binary.BigEndian.Uint32(buf[stdWriterSizeIndex : stdWriterSizeIndex+4]))

		// Check if the buffer is big enough to read the frame.
		// Extend it if necessary.
		if frameSize+stdWriterPrefixLen > bufLen {
			buf = append(buf, make([]byte, frameSize+stdWriterPrefixLen-bufLen+1)...)
			bufLen = len(buf)
		}

		// While the amount of bytes read is less than the size of the frame + header, we keep reading
		for nr < frameSize+stdWriterPrefixLen {
			var nr2 int
			nr2, err = multiplexedSource.Read(buf[nr:])
			nr += nr2
			if errors.Is(err, io.EOF) {
				if nr < frameSize+stdWriterPrefixLen {
					return written, nil
				}
				break
			}
			if err != nil {
				return 0, err
			}
		}

		// we might have an error from the source mixed up in our multiplexed
		// stream. if we do, return it.
		if stream == Systemerr {
			return written, fmt.Errorf("error from daemon in stream: %s", string(buf[stdWriterPrefixLen:frameSize+stdWriterPrefixLen]))
		}

		// Write the retrieved frame (without header)
		nw, err = out.Write(buf[stdWriterPrefixLen : frameSize+stdWriterPrefixLen])
		if err != nil {
			return 0, err
		}

		// If the frame has not been fully written: error
		if nw != frameSize {
			return 0, io.ErrShortWrite
		}
		written += int64(nw)

		// Move the rest of the buffer to the beginning
		copy(buf, buf[frameSize+stdWriterPrefixLen:])
		// Move the index
		nr -= frameSize + stdWriterPrefixLen
	}
}
