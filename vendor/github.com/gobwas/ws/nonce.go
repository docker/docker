package ws

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"math/rand"
)

const (
	// RFC6455: The value of this header field MUST be a nonce consisting of a
	// randomly selected 16-byte value that has been base64-encoded (see
	// Section 4 of [RFC4648]).  The nonce MUST be selected randomly for each
	// connection.
	nonceKeySize = 16
	nonceSize    = 24 // base64.StdEncoding.EncodedLen(nonceKeySize)

	// RFC6455: The value of this header field is constructed by concatenating
	// /key/, defined above in step 4 in Section 4.2.2, with the string
	// "258EAFA5- E914-47DA-95CA-C5AB0DC85B11", taking the SHA-1 hash of this
	// concatenated value to obtain a 20-byte value and base64- encoding (see
	// Section 4 of [RFC4648]) this 20-byte hash.
	acceptSize = 28 // base64.StdEncoding.EncodedLen(sha1.Size)
)

// initNonce fills given slice with random base64-encoded nonce bytes.
func initNonce(dst []byte) {
	// NOTE: bts does not escape.
	bts := make([]byte, nonceKeySize)
	if _, err := rand.Read(bts); err != nil {
		panic(fmt.Sprintf("rand read error: %s", err))
	}
	base64.StdEncoding.Encode(dst, bts)
}

// checkAcceptFromNonce reports whether given accept bytes are valid for given
// nonce bytes.
func checkAcceptFromNonce(accept, nonce []byte) bool {
	if len(accept) != acceptSize {
		return false
	}
	// NOTE: expect does not escape.
	expect := make([]byte, acceptSize)
	initAcceptFromNonce(expect, nonce)
	return bytes.Equal(expect, accept)
}

// initAcceptFromNonce fills given slice with accept bytes generated from given
// nonce bytes. Given buffer should be exactly acceptSize bytes.
func initAcceptFromNonce(accept, nonce []byte) {
	const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	if len(accept) != acceptSize {
		panic("accept buffer is invalid")
	}
	if len(nonce) != nonceSize {
		panic("nonce is invalid")
	}

	p := make([]byte, nonceSize+len(magic))
	copy(p[:nonceSize], nonce)
	copy(p[nonceSize:], magic)

	sum := sha1.Sum(p)
	base64.StdEncoding.Encode(accept, sum[:])

	return
}

func writeAccept(bw *bufio.Writer, nonce []byte) (int, error) {
	accept := make([]byte, acceptSize)
	initAcceptFromNonce(accept, nonce)
	// NOTE: write accept bytes as a string to prevent heap allocation –
	// WriteString() copy given string into its inner buffer, unlike Write()
	// which may write p directly to the underlying io.Writer – which in turn
	// will lead to p escape.
	return bw.WriteString(btsToString(accept))
}
