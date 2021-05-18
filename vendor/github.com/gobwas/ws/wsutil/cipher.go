package wsutil

import (
	"io"

	"github.com/gobwas/pool/pbytes"
	"github.com/gobwas/ws"
)

// CipherReader implements io.Reader that applies xor-cipher to the bytes read
// from source.
// It could help to unmask WebSocket frame payload on the fly.
type CipherReader struct {
	r    io.Reader
	mask [4]byte
	pos  int
}

// NewCipherReader creates xor-cipher reader from r with given mask.
func NewCipherReader(r io.Reader, mask [4]byte) *CipherReader {
	return &CipherReader{r, mask, 0}
}

// Reset resets CipherReader to read from r with given mask.
func (c *CipherReader) Reset(r io.Reader, mask [4]byte) {
	c.r = r
	c.mask = mask
	c.pos = 0
}

// Read implements io.Reader interface. It applies mask given during
// initialization to every read byte.
func (c *CipherReader) Read(p []byte) (n int, err error) {
	n, err = c.r.Read(p)
	ws.Cipher(p[:n], c.mask, c.pos)
	c.pos += n
	return
}

// CipherWriter implements io.Writer that applies xor-cipher to the bytes
// written to the destination writer. It does not modify the original bytes.
type CipherWriter struct {
	w    io.Writer
	mask [4]byte
	pos  int
}

// NewCipherWriter creates xor-cipher writer to w with given mask.
func NewCipherWriter(w io.Writer, mask [4]byte) *CipherWriter {
	return &CipherWriter{w, mask, 0}
}

// Reset reset CipherWriter to write to w with given mask.
func (c *CipherWriter) Reset(w io.Writer, mask [4]byte) {
	c.w = w
	c.mask = mask
	c.pos = 0
}

// Write implements io.Writer interface. It applies masking during
// initialization to every sent byte. It does not modify original slice.
func (c *CipherWriter) Write(p []byte) (n int, err error) {
	cp := pbytes.GetLen(len(p))
	defer pbytes.Put(cp)

	copy(cp, p)
	ws.Cipher(cp, c.mask, c.pos)
	n, err = c.w.Write(cp)
	c.pos += n

	return
}
