package compression

import (
	"encoding/binary"
	"hash/crc32"
	"io"

	"github.com/moby/buildkit/util/compression/go126flate"
	"github.com/pkg/errors"
)

const go126GzipDefaultCompression = go126flate.DefaultCompression

// go126GzipWriter writes the fixed gzip framing used for image layers around
// the Go 1.26 DEFLATE stream. Image layers do not use optional gzip headers.
type go126GzipWriter struct {
	w           io.Writer
	level       int
	wroteHeader bool
	closed      bool
	buf         [10]byte
	compressor  *go126flate.Writer
	digest      uint32
	size        uint32
	err         error
}

func newGo126GzipWriterLevel(w io.Writer, level int) (io.WriteCloser, error) {
	if level < go126flate.HuffmanOnly || level > go126flate.BestCompression {
		return nil, errors.Errorf("gzip: invalid compression level: %d", level)
	}
	return &go126GzipWriter{w: w, level: level}, nil
}

func (z *go126GzipWriter) writeHeader() error {
	z.wroteHeader = true
	z.buf = [10]byte{0: 0x1f, 1: 0x8b, 2: 8, 9: 255}
	switch z.level {
	case go126flate.BestCompression:
		z.buf[8] = 2
	case go126flate.BestSpeed:
		z.buf[8] = 4
	}
	if _, err := z.w.Write(z.buf[:10]); err != nil {
		return err
	}
	compressor, err := go126flate.NewWriter(z.w, z.level)
	if err != nil {
		return errors.WithStack(err)
	}
	z.compressor = compressor
	return nil
}

func (z *go126GzipWriter) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if !z.wroteHeader {
		z.err = z.writeHeader()
		if z.err != nil {
			return 0, z.err
		}
	}
	z.size += uint32(len(p))
	z.digest = crc32.Update(z.digest, crc32.IEEETable, p)
	n, err := z.compressor.Write(p)
	z.err = err
	return n, err
}

func (z *go126GzipWriter) Close() error {
	if z.err != nil {
		return z.err
	}
	if z.closed {
		return nil
	}
	z.closed = true
	if !z.wroteHeader {
		if z.err = z.writeHeader(); z.err != nil {
			return z.err
		}
	}
	if z.err = z.compressor.Close(); z.err != nil {
		return z.err
	}
	binary.LittleEndian.PutUint32(z.buf[:4], z.digest)
	binary.LittleEndian.PutUint32(z.buf[4:8], z.size)
	_, z.err = z.w.Write(z.buf[:8])
	return z.err
}
