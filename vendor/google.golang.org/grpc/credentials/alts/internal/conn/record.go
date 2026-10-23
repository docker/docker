/*
 *
 * Copyright 2018 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package conn contains an implementation of a secure channel created by gRPC
// handshakers.
package conn

import (
	"encoding/binary"
	"fmt"
	"math"
	"net"

	core "google.golang.org/grpc/credentials/alts/internal"
	imem "google.golang.org/grpc/internal/mem"
	"google.golang.org/grpc/internal/transport/readyreader"
	"google.golang.org/grpc/mem"
)

// ALTSRecordCrypto is the interface for gRPC ALTS record protocol.
type ALTSRecordCrypto interface {
	// Encrypt encrypts the plaintext, computes the tag (if any) of dst and
	// plaintext, and appends the result to dst, returning the updated slice.
	// dst and plaintext may fully overlap or not at all.
	Encrypt(dst, plaintext []byte) ([]byte, error)
	// EncryptionOverhead returns the tag size (if any) in bytes.
	EncryptionOverhead() int
	// Decrypt decrypts ciphertext and verifies the tag (if any). If successful,
	// this function appends the resulting plaintext to dst, returning the
	// updated slice. dst and ciphertext may alias exactly or not at all. To
	// reuse ciphertext's storage for the decrypted output, use ciphertext[:0]
	// as dst. Even if the function fails, the contents of dst, up to its
	// capacity, may be overwritten.
	Decrypt(dst, ciphertext []byte) ([]byte, error)
}

// ALTSRecordFunc is a function type for factory functions that create
// ALTSRecordCrypto instances.
type ALTSRecordFunc func(s core.Side, keyData []byte) (ALTSRecordCrypto, error)

const (
	// MsgLenFieldSize is the byte size of the frame length field of a
	// framed message.
	MsgLenFieldSize = 4
	// The byte size of the message type field of a framed message.
	msgTypeFieldSize = 4
	// The bytes size limit for a ALTS record message.
	altsRecordLengthLimit = 1024 * 1024 // 1 MiB
	// The default bytes size of a ALTS record message.
	altsRecordDefaultLength = 4 * 1024 // 4KiB
	// Message type value included in ALTS record framing.
	altsRecordMsgType = uint32(0x06)
	// The maximum write buffer size, which can hold multiple ALTS frames.
	altsWriteBufferMaxSize = 512 * 1024 // 512KiB
	// The initial buffer used to read from the network.
	// It includes an additional 512 Bytes to hold two 16KiB records plus
	// small framing overheads.
	altsReadBufferInitialSize = 32*1024 + 512 // 32.5KiB
)

var (
	protocols    = make(map[string]ALTSRecordFunc)
	writeBufPool *imem.BinaryTieredBufferPool
	// readBufPool pools buffers of at least `altsReadBufferInitialSize` size.
	// Since the read buffer size is slightly larger than 32KB, using a regular
	// BinaryTieredBufferPool results in allocating buffers of almost double the
	// required length.
	readBufPool = imem.NewDirtySimplePool()

	// Compile-time check to ensure conn implements ReadyReader.
	_ readyreader.Reader = &conn{}
)

func init() {
	pool, err := imem.NewDirtyBinaryTieredBufferPool(
		8,
		12, // Go page size, 4KB
		14, // 16KB (max HTTP/2 frame size used by gRPC)
		15, // 32KB (default buffer size for gRPC)
		16, // 64KB
		17, // 128KB
		19, // 512KB, max write buffer size
	)
	if err != nil {
		panic(fmt.Sprintf("Failed to create write buffer pool: %v", err))
	}
	writeBufPool = pool
}

// RegisterProtocol register a ALTS record encryption protocol.
func RegisterProtocol(protocol string, f ALTSRecordFunc) error {
	if _, ok := protocols[protocol]; ok {
		return fmt.Errorf("protocol %v is already registered", protocol)
	}
	protocols[protocol] = f
	return nil
}

// conn represents a secured connection. It implements the net.Conn interface.
type conn struct {
	net.Conn
	reader readyreader.Reader
	crypto ALTSRecordCrypto
	// buf holds data that has been read from the connection and decrypted,
	// but has not yet been returned by Read. It is a sub-slice of protected.
	buf                []byte
	payloadLengthLimit int
	// protectedHandle buffer holds data read from the network but have not yet
	// been decrypted. This data might not compose a complete frame.
	//
	// The buffer pointer to points to a buffer from the readBufPool. The handle
	// should only be returned to the pool once nextFrame and buf are empty.
	protectedHandle *[]byte
	// nextFrame stores the next frame (in protected buffer) info.
	nextFrame []byte
	// overhead is the calculated overhead of each frame.
	overhead  int
	constPool constBufferPool // stored as a field to avoid heap allocations.
}

// NewConn creates a new secure channel instance given the other party role and
// handshaking result.
func NewConn(c net.Conn, side core.Side, recordProtocol string, key []byte, protected []byte) (net.Conn, error) {
	return NewConnWithMaxFrameSize(c, side, recordProtocol, key, protected, 0)
}

// NewConnWithMaxFrameSize creates a new secure channel instance given the
// other party role, handshaking result, and negotiated maximum frame size.
func NewConnWithMaxFrameSize(c net.Conn, side core.Side, recordProtocol string, key []byte, protected []byte, negotiatedMaxFrameSize int) (net.Conn, error) {
	newCrypto := protocols[recordProtocol]
	if newCrypto == nil {
		return nil, fmt.Errorf("negotiated unknown next_protocol %q", recordProtocol)
	}
	crypto, err := newCrypto(side, key)
	if err != nil {
		return nil, fmt.Errorf("protocol %q: %v", recordProtocol, err)
	}
	overhead := MsgLenFieldSize + msgTypeFieldSize + crypto.EncryptionOverhead()

	// Clamp maxRecordLen to be at least altsRecordDefaultLength.
	maxRecordLen := max(altsRecordDefaultLength, negotiatedMaxFrameSize)
	payloadLengthLimit := maxRecordLen - overhead
	// We pre-allocate protected to be of size 32KB during initialization.
	// We increase the size of the buffer by the required amount if it can't
	// hold a complete encrypted record.
	protectedHandle := readBufPool.Get(max(altsReadBufferInitialSize, len(protected)))
	protectedBuf := *protectedHandle
	// Copy additional data from hanshaker service.
	copy(protectedBuf, protected)
	protectedBuf = protectedBuf[:len(protected)]

	altsConn := &conn{
		Conn:               c,
		reader:             readyreader.New(c),
		crypto:             crypto,
		payloadLengthLimit: payloadLengthLimit,
		protectedHandle:    protectedHandle,
		nextFrame:          protectedBuf,
		overhead:           overhead,
	}
	return altsConn, nil
}

type constBufferPool struct {
	buffer []byte
}

func (p *constBufferPool) Get(int) *[]byte {
	return &p.buffer
}

func (p *constBufferPool) Put(*[]byte) {}

// Read reads and decrypts a frame from the underlying connection, and copies the
// decrypted payload into b. If the size of the payload is greater than len(b),
// Read retains the remaining bytes in an internal buffer, and subsequent calls
// to Read will read from this buffer until it is exhausted.
func (p *conn) Read(b []byte) (n int, err error) {
	p.constPool.buffer = b
	_, n, err = p.ReadOnReady(len(b), &p.constPool)
	return n, err
}

func (p *conn) ReadOnReady(bufSize int, pool mem.BufferPool) (*[]byte, int, error) {
	if len(p.buf) == 0 {
		var framedMsg []byte
		var protected []byte
		if p.protectedHandle != nil {
			protected = *p.protectedHandle
			protected = protected[:cap(protected)]
		}
		var err error
		framedMsg, p.nextFrame, err = ParseFramedMsg(p.nextFrame, altsRecordLengthLimit)
		if err != nil {
			return nil, 0, err
		}
		// Check whether the next frame to be decrypted has been
		// completely received yet.
		if len(framedMsg) == 0 {
			copy(protected, p.nextFrame)
			protected = protected[:len(p.nextFrame)]
			// Always copy next incomplete frame to the beginning of
			// the protected buffer and reset nextFrame to it.
			p.nextFrame = protected
		}
		// Check whether a complete frame has been received yet.
		for len(framedMsg) == 0 {
			if p.protectedHandle != nil && len(protected) == cap(protected) {
				// We can parse the length header to know exactly how large
				// the buffer needs to be to hold the entire frame.
				length, didParse := parseMessageLength(protected)
				if !didParse {
					// The protected buffer is initialized with a capacity of
					// larger than 4B. It should always hold the message length
					// header.
					panic(fmt.Sprintf("protected buffer length shorter than expected: %d vs %d", len(protected), MsgLenFieldSize))
				}
				oldProtectedBuf := protected
				oldBufHandle := p.protectedHandle
				// The new buffer must be able to hold the message length header
				// and the entire message.
				requiredCapacity := int(length) + MsgLenFieldSize
				p.protectedHandle = readBufPool.Get(requiredCapacity)
				protected = *p.protectedHandle
				// Copy the contents of the old buffer and set the length of the
				// new buffer to the number of bytes already read.
				copy(protected, oldProtectedBuf)
				protected = protected[:len(oldProtectedBuf)]
				readBufPool.Put(oldBufHandle)
			}
			if p.protectedHandle == nil {
				// Connection was idle, need to re-allocate the read buffer.
				newBuf, nRead, err := p.reader.ReadOnReady(altsReadBufferInitialSize, readBufPool)
				if err != nil {
					return nil, 0, err
				}
				p.protectedHandle = newBuf
				protected = (*newBuf)[:nRead]
			} else {
				nRead, err := p.Conn.Read(protected[len(protected):cap(protected)])
				if err != nil {
					return nil, 0, err
				}
				protected = protected[:len(protected)+nRead]
			}
			framedMsg, p.nextFrame, err = ParseFramedMsg(protected, altsRecordLengthLimit)
			if err != nil {
				return nil, 0, err
			}
		}
		// Now we have a complete frame, decrypted it.
		msg := framedMsg[MsgLenFieldSize:]
		if len(msg) < msgTypeFieldSize {
			return nil, 0, fmt.Errorf("received frame with size %v which is shorter than message type field size %v", len(msg), msgTypeFieldSize)
		}
		msgType := binary.LittleEndian.Uint32(msg[:msgTypeFieldSize])
		if msgType&0xff != altsRecordMsgType {
			return nil, 0, fmt.Errorf("received frame with incorrect message type %v, expected lower byte %v",
				msgType, altsRecordMsgType)
		}
		ciphertext := msg[msgTypeFieldSize:]

		// Decrypt directly into the buffer, avoiding a copy from p.buf if
		// possible.
		if bufSize >= len(ciphertext) {
			allocatedBuf := pool.Get(bufSize)
			dec, err := p.crypto.Decrypt((*allocatedBuf)[:0], ciphertext)
			if err != nil {
				pool.Put(allocatedBuf)
				return nil, 0, err
			}
			p.dropProtectedIfEmtpy()
			return allocatedBuf, len(dec), nil
		}
		// Decrypt requires that if the dst and ciphertext alias, they
		// must alias exactly. Code here used to use msg[:0], but msg
		// starts MsgLenFieldSize+msgTypeFieldSize bytes earlier than
		// ciphertext, so they alias inexactly. Using ciphertext[:0]
		// arranges the appropriate aliasing without needing to copy
		// ciphertext or use a separate destination buffer. For more info
		// check: https://golang.org/pkg/crypto/cipher/#AEAD.
		p.buf, err = p.crypto.Decrypt(ciphertext[:0], ciphertext)
		if err != nil {
			return nil, 0, err
		}
	}

	allocatedBuf := pool.Get(bufSize)
	n := copy(*allocatedBuf, p.buf)
	p.buf = p.buf[n:]
	p.dropProtectedIfEmtpy()
	return allocatedBuf, n, nil
}

func (p *conn) dropProtectedIfEmtpy() {
	if len(p.buf) > 0 || len(p.nextFrame) > 0 {
		return
	}
	// Potentially idle connection, release the read buffer.
	p.nextFrame = nil
	p.buf = nil
	if p.protectedHandle != nil {
		readBufPool.Put(p.protectedHandle)
		p.protectedHandle = nil
	}
}

// Write encrypts, frames, and writes bytes from b to the underlying connection.
func (p *conn) Write(b []byte) (n int, err error) {
	n = len(b)
	// Calculate the output buffer size with framing and encryption overhead.
	numOfFrames := int(math.Ceil(float64(len(b)) / float64(p.payloadLengthLimit)))
	size := len(b) + numOfFrames*p.overhead
	partialBSize := len(b)
	if size > altsWriteBufferMaxSize {
		size = altsWriteBufferMaxSize
		frameLen := p.payloadLengthLimit + p.overhead
		numOfFramesInMaxWriteBuf := altsWriteBufferMaxSize / frameLen
		partialBSize = numOfFramesInMaxWriteBuf * p.payloadLengthLimit
	}
	// Get a writeBuf of the required length.
	bufHandle := writeBufPool.Get(size)
	defer writeBufPool.Put(bufHandle)
	writeBuf := *bufHandle

	for partialBStart := 0; partialBStart < len(b); partialBStart += partialBSize {
		partialBEnd := min(partialBStart+partialBSize, len(b))
		partialB := b[partialBStart:partialBEnd]
		writeBufIndex := 0
		for len(partialB) > 0 {
			payloadLen := min(len(partialB), p.payloadLengthLimit)
			buf := partialB[:payloadLen]
			partialB = partialB[payloadLen:]

			// Write buffer contains: length, type, payload, and tag
			// if any.

			// 1. Fill in type field.
			msg := writeBuf[writeBufIndex+MsgLenFieldSize:]
			binary.LittleEndian.PutUint32(msg, altsRecordMsgType)

			// 2. Encrypt the payload and create a tag if any.
			msg, err = p.crypto.Encrypt(msg[:msgTypeFieldSize], buf)
			if err != nil {
				return n, err
			}

			// 3. Fill in the size field.
			binary.LittleEndian.PutUint32(writeBuf[writeBufIndex:], uint32(len(msg)))

			// 4. Increase writeBufIndex.
			writeBufIndex += len(buf) + p.overhead
		}
		nn, err := p.Conn.Write(writeBuf[:writeBufIndex])
		if err != nil {
			// We need to calculate the actual data size that was
			// written. This means we need to remove header,
			// encryption overheads, and any partially-written
			// frame data.
			numOfWrittenFrames := int(math.Floor(float64(nn) / float64(p.payloadLengthLimit+p.overhead)))
			return partialBStart + numOfWrittenFrames*p.payloadLengthLimit, err
		}
	}
	return n, nil
}
