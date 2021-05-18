package ws

import (
	"encoding/binary"
)

// Cipher applies XOR cipher to the payload using mask.
// Offset is used to cipher chunked data (e.g. in io.Reader implementations).
//
// To convert masked data into unmasked data, or vice versa, the following
// algorithm is applied.  The same algorithm applies regardless of the
// direction of the translation, e.g., the same steps are applied to
// mask the data as to unmask the data.
func Cipher(payload []byte, mask [4]byte, offset int) {
	n := len(payload)
	if n < 8 {
		for i := 0; i < n; i++ {
			payload[i] ^= mask[(offset+i)%4]
		}
		return
	}

	// Calculate position in mask due to previously processed bytes number.
	mpos := offset % 4
	// Count number of bytes will processed one by one from the beginning of payload.
	ln := remain[mpos]
	// Count number of bytes will processed one by one from the end of payload.
	// This is done to process payload by 8 bytes in each iteration of main loop.
	rn := (n - ln) % 8

	for i := 0; i < ln; i++ {
		payload[i] ^= mask[(mpos+i)%4]
	}
	for i := n - rn; i < n; i++ {
		payload[i] ^= mask[(mpos+i)%4]
	}

	// NOTE: we use here binary.LittleEndian regardless of what is real
	// endianess on machine is. To do so, we have to use binary.LittleEndian in
	// the masking loop below as well.
	var (
		m  = binary.LittleEndian.Uint32(mask[:])
		m2 = uint64(m)<<32 | uint64(m)
	)
	// Skip already processed right part.
	// Get number of uint64 parts remaining to process.
	n = (n - ln - rn) >> 3
	for i := 0; i < n; i++ {
		var (
			j     = ln + (i << 3)
			chunk = payload[j : j+8]
		)
		p := binary.LittleEndian.Uint64(chunk)
		p = p ^ m2
		binary.LittleEndian.PutUint64(chunk, p)
	}
}

// remain maps position in masking key [0,4) to number
// of bytes that need to be processed manually inside Cipher().
var remain = [4]int{0, 3, 2, 1}
