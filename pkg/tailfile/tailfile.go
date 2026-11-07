// Package tailfile provides helper functions to read the nth lines of any
// ReadSeeker.
package tailfile

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
)

const blockSize = 1024

var eol = []byte("\n")

// ErrNonPositiveLinesNumber is an error returned if the lines number was negative.
var ErrNonPositiveLinesNumber = errors.New("The number of lines to extract from the file must be positive")

// TailFile returns last n lines of the passed in file.
func TailFile(f *os.File, n int) ([][]byte, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	rAt := io.NewSectionReader(f, 0, size)
	r, nLines, err := NewTailReader(context.Background(), rAt, n)
	if err != nil {
		return nil, err
	}

	buf := make([][]byte, 0, nLines)
	s := bufio.NewScanner(r)

	for s.Scan() {
		buf = append(buf, s.Bytes())
	}
	return buf, nil
}

// SizeReaderAt provides a ReaderAt and the size of its underlying data.
// The reported size may be stale if the underlying data is truncated.
// If truncation is detected while locating the tail, scanning restarts at the
// shortened end.
// Other concurrent mutations are not supported.
// The returned tail reader is not a snapshot of the underlying data.
type SizeReaderAt interface {
	io.ReaderAt
	Size() int64
}

// NewTailReader scopes the passed in reader to just the last N lines passed in
func NewTailReader(ctx context.Context, r SizeReaderAt, reqLines int) (*io.SectionReader, int, error) {
	return NewTailReaderWithDelimiter(ctx, r, reqLines, eol)
}

// NewTailReaderWithDelimiter scopes the passed in reader to just the last N lines passed in
// In this case a "line" is defined by the passed in delimiter.
//
// Delimiter lengths should be generally small, no more than 12 bytes
func NewTailReaderWithDelimiter(ctx context.Context, r SizeReaderAt, reqLines int, delimiter []byte) (*io.SectionReader, int, error) {
	if reqLines < 1 {
		return nil, 0, ErrNonPositiveLinesNumber
	}
	if len(delimiter) == 0 {
		return nil, 0, errors.New("must provide a delimiter")
	}
	if int64(len(delimiter)) >= r.Size() {
		return io.NewSectionReader(bytes.NewReader(nil), 0, 0), 0, nil
	}

	s := newScanner(r, delimiter)
	for {
		size := s.end
		s.pos = size
		s.idx = 0
		var (
			tailEnd int64
			found   int
		)
		for s.Scan(ctx) {
			found++
			if found == 1 {
				tailEnd = s.End()
			}
			if found == reqLines {
				break
			}
		}
		var tailStart int64
		if found == reqLines {
			tailStart = s.Start(ctx)
		}
		if err := s.Err(); err != nil {
			return nil, 0, err
		}
		// Truncation invalidates the recorded delimiters. Each retry starts
		// at a strictly smaller end, so a stale Size cannot prevent progress.
		if s.end < size {
			continue
		}
		if found == 0 {
			return io.NewSectionReader(bytes.NewReader(nil), 0, 0), 0, nil
		}

		return io.NewSectionReader(r, tailStart, tailEnd-tailStart), found, nil
	}
}

func newScanner(r SizeReaderAt, delim []byte) *scanner {
	size := r.Size()
	readSize := min(blockSize, int(size))
	// silly case...
	if len(delim) >= readSize/2 {
		readSize = len(delim)*2 + 2
	}

	return &scanner{
		r:     r,
		pos:   size,
		end:   size,
		buf:   make([]byte, readSize),
		delim: delim,
	}
}

type scanner struct {
	r     SizeReaderAt
	pos   int64
	end   int64
	buf   []byte
	delim []byte
	err   error
	idx   int
}

// Start locates the start of the current record, advancing the scanner if needed.
func (s *scanner) Start(ctx context.Context) int64 {
	if s.idx > 0 {
		idx := bytes.LastIndex(s.buf[:s.idx], s.delim)
		if idx >= 0 {
			return s.pos + int64(idx) + int64(len(s.delim))
		}
	}

	if !s.Scan(ctx) {
		return 0
	}
	return s.End()
}

func (s *scanner) End() int64 {
	return s.pos + int64(s.idx) + int64(len(s.delim))
}

func (s *scanner) Err() error {
	return s.err
}

func (s *scanner) Scan(ctx context.Context) bool {
	if s.err != nil {
		return false
	}

	for {
		select {
		case <-ctx.Done():
			s.err = ctx.Err()
			return false
		default:
		}

		idx := s.idx - len(s.delim)
		if idx < 0 {
			readSize := min(int(s.pos), len(s.buf))

			if readSize < len(s.delim) {
				return false
			}

			offset := s.pos - int64(readSize)
			n, err := s.r.ReadAt(s.buf[:readSize], offset)
			if err != nil && !errors.Is(err, io.EOF) {
				s.err = err
				return false
			}
			if n < readSize {
				end := offset + int64(n)
				if n == 0 && offset > 0 {
					// Probe for the new end instead of stepping one empty block at a time.
					end, err = s.findEnd(ctx, offset)
					if err != nil {
						s.err = err
						return false
					}
				}
				s.end = end
				return false
			}

			s.pos = offset
			idx = n
		}

		s.idx = bytes.LastIndex(s.buf[:idx], s.delim)
		if s.idx >= 0 {
			return true
		}

		if len(s.delim) > 1 && s.pos > 0 {
			// in this case, there may be a partial delimiter at the front of the buffer, so set the position forward
			// up to the maximum size partial that could be there so it can be read again in the next iteration with any
			// potential remainder.
			// An example where delimiter is `####`:
			// [##asdfqwerty]
			//    ^
			// This resets the position to where the arrow is pointing.
			// It could actually check if a partial exists and at the front, but that is pretty similar to the indexing
			// code above though a bit more complex since each byte has to be checked (`len(delimiter)-1`) factorial).
			// It's much simpler and cleaner to just re-read `len(delimiter)-1` bytes again.
			s.pos += int64(len(s.delim)) - 1
		}
	}
}

// findEnd returns the current end of the data, given that a read at hi
// returned nothing. It binary searches for the last readable byte, so a large
// truncation costs O(log n) reads rather than one read per block.
func (s *scanner) findEnd(ctx context.Context, hi int64) (int64, error) {
	var (
		lo int64
		b  [1]byte
	)
	for lo < hi {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		mid := lo + (hi-lo)/2
		n, err := s.r.ReadAt(b[:], mid)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if n == 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, nil
}
