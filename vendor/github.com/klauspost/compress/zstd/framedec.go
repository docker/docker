// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"bytes"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd/internal/xxhash"
)

type frameDec struct {
	o      decoderOptions
	crc    hash.Hash64
	offset int64

	WindowSize uint64

	// maxWindowSize is the maximum windows size to support.
	// should never be bigger than max-int.
	maxWindowSize uint64

	// In order queue of blocks being decoded.
	decoding chan *blockDec

	// Frame history passed between blocks
	history history

	rawInput byteBuffer

	// Byte buffer that can be reused for small input blocks.
	bBuf byteBuf

	FrameContentSize uint64
	frameDone        sync.WaitGroup

	DictionaryID  *uint32
	HasCheckSum   bool
	SingleSegment bool

	// asyncRunning indicates whether the async routine processes input on 'decoding'.
	asyncRunningMu sync.Mutex
	asyncRunning   bool
}

const (
	// The minimum Window_Size is 1 KB.
	MinWindowSize = 1 << 10
	MaxWindowSize = 1 << 29
)

var (
	frameMagic          = []byte{0x28, 0xb5, 0x2f, 0xfd}
	skippableFrameMagic = []byte{0x2a, 0x4d, 0x18}
)

func newFrameDec(o decoderOptions) *frameDec {
	d := frameDec{
		o:             o,
		maxWindowSize: MaxWindowSize,
	}
	if d.maxWindowSize > o.maxDecodedSize {
		d.maxWindowSize = o.maxDecodedSize
	}
	return &d
}

// reset will read the frame header and prepare for block decoding.
// If nothing can be read from the input, io.EOF will be returned.
// Any other error indicated that the stream contained data, but
// there was a problem.
func (d *frameDec) reset(br byteBuffer) error {
	d.HasCheckSum = false
	d.WindowSize = 0
	var b []byte
	for {
		b = br.readSmall(4)
		if b == nil {
			return io.EOF
		}
		if !bytes.Equal(b[1:4], skippableFrameMagic) || b[0]&0xf0 != 0x50 {
			if debug {
				println("Not skippable", hex.EncodeToString(b), hex.EncodeToString(skippableFrameMagic))
			}
			// Break if not skippable frame.
			break
		}
		// Read size to skip
		b = br.readSmall(4)
		if b == nil {
			println("Reading Frame Size EOF")
			return io.ErrUnexpectedEOF
		}
		n := uint32(b[0]) | (uint32(b[1]) << 8) | (uint32(b[2]) << 16) | (uint32(b[3]) << 24)
		println("Skipping frame with", n, "bytes.")
		err := br.skipN(int(n))
		if err != nil {
			if debug {
				println("Reading discarded frame", err)
			}
			return err
		}
	}
	if !bytes.Equal(b, frameMagic) {
		println("Got magic numbers: ", b, "want:", frameMagic)
		return ErrMagicMismatch
	}

	// Read Frame_Header_Descriptor
	fhd, err := br.readByte()
	if err != nil {
		println("Reading Frame_Header_Descriptor", err)
		return err
	}
	d.SingleSegment = fhd&(1<<5) != 0

	if fhd&(1<<3) != 0 {
		return errors.New("Reserved bit set on frame header")
	}

	// Read Window_Descriptor
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#window_descriptor
	d.WindowSize = 0
	if !d.SingleSegment {
		wd, err := br.readByte()
		if err != nil {
			println("Reading Window_Descriptor", err)
			return err
		}
		printf("raw: %x, mantissa: %d, exponent: %d\n", wd, wd&7, wd>>3)
		windowLog := 10 + (wd >> 3)
		windowBase := uint64(1) << windowLog
		windowAdd := (windowBase / 8) * uint64(wd&0x7)
		d.WindowSize = windowBase + windowAdd
	}

	// Read Dictionary_ID
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#dictionary_id
	d.DictionaryID = nil
	if size := fhd & 3; size != 0 {
		if size == 3 {
			size = 4
		}
		b = br.readSmall(int(size))
		if b == nil {
			if debug {
				println("Reading Dictionary_ID", io.ErrUnexpectedEOF)
			}
			return io.ErrUnexpectedEOF
		}
		var id uint32
		switch size {
		case 1:
			id = uint32(b[0])
		case 2:
			id = uint32(b[0]) | (uint32(b[1]) << 8)
		case 4:
			id = uint32(b[0]) | (uint32(b[1]) << 8) | (uint32(b[2]) << 16) | (uint32(b[3]) << 24)
		}
		if debug {
			println("Dict size", size, "ID:", id)
		}
		if id > 0 {
			// ID 0 means "sorry, no dictionary anyway".
			// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#dictionary-format
			d.DictionaryID = &id
		}
	}

	// Read Frame_Content_Size
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#frame_content_size
	var fcsSize int
	v := fhd >> 6
	switch v {
	case 0:
		if d.SingleSegment {
			fcsSize = 1
		}
	default:
		fcsSize = 1 << v
	}
	d.FrameContentSize = 0
	if fcsSize > 0 {
		b := br.readSmall(fcsSize)
		if b == nil {
			println("Reading Frame content", io.ErrUnexpectedEOF)
			return io.ErrUnexpectedEOF
		}
		switch fcsSize {
		case 1:
			d.FrameContentSize = uint64(b[0])
		case 2:
			// When FCS_Field_Size is 2, the offset of 256 is added.
			d.FrameContentSize = uint64(b[0]) | (uint64(b[1]) << 8) + 256
		case 4:
			d.FrameContentSize = uint64(b[0]) | (uint64(b[1]) << 8) | (uint64(b[2]) << 16) | (uint64(b[3]) << 24)
		case 8:
			d1 := uint32(b[0]) | (uint32(b[1]) << 8) | (uint32(b[2]) << 16) | (uint32(b[3]) << 24)
			d2 := uint32(b[4]) | (uint32(b[5]) << 8) | (uint32(b[6]) << 16) | (uint32(b[7]) << 24)
			d.FrameContentSize = uint64(d1) | (uint64(d2) << 32)
		}
		if debug {
			println("field size bits:", v, "fcsSize:", fcsSize, "FrameContentSize:", d.FrameContentSize, hex.EncodeToString(b[:fcsSize]), "singleseg:", d.SingleSegment, "window:", d.WindowSize)
		}
	}
	// Move this to shared.
	d.HasCheckSum = fhd&(1<<2) != 0
	if d.HasCheckSum {
		if d.crc == nil {
			d.crc = xxhash.New()
		}
		d.crc.Reset()
	}

	if d.WindowSize == 0 && d.SingleSegment {
		// We may not need window in this case.
		d.WindowSize = d.FrameContentSize
		if d.WindowSize < MinWindowSize {
			d.WindowSize = MinWindowSize
		}
	}

	if d.WindowSize > d.maxWindowSize {
		printf("window size %d > max %d\n", d.WindowSize, d.maxWindowSize)
		return ErrWindowSizeExceeded
	}
	// The minimum Window_Size is 1 KB.
	if d.WindowSize < MinWindowSize {
		println("got window size: ", d.WindowSize)
		return ErrWindowSizeTooSmall
	}
	d.history.windowSize = int(d.WindowSize)
	if d.o.lowMem && d.history.windowSize < maxBlockSize {
		d.history.maxSize = d.history.windowSize * 2
	} else {
		d.history.maxSize = d.history.windowSize + maxBlockSize
	}
	// history contains input - maybe we do something
	d.rawInput = br
	return nil
}

// next will start decoding the next block from stream.
func (d *frameDec) next(block *blockDec) error {
	if debug {
		printf("decoding new block %p:%p", block, block.data)
	}
	err := block.reset(d.rawInput, d.WindowSize)
	if err != nil {
		println("block error:", err)
		// Signal the frame decoder we have a problem.
		d.sendErr(block, err)
		return err
	}
	block.input <- struct{}{}
	if debug {
		println("next block:", block)
	}
	d.asyncRunningMu.Lock()
	defer d.asyncRunningMu.Unlock()
	if !d.asyncRunning {
		return nil
	}
	if block.Last {
		// We indicate the frame is done by sending io.EOF
		d.decoding <- block
		return io.EOF
	}
	d.decoding <- block
	return nil
}

// sendEOF will queue an error block on the frame.
// This will cause the frame decoder to return when it encounters the block.
// Returns true if the decoder was added.
func (d *frameDec) sendErr(block *blockDec, err error) bool {
	d.asyncRunningMu.Lock()
	defer d.asyncRunningMu.Unlock()
	if !d.asyncRunning {
		return false
	}

	println("sending error", err.Error())
	block.sendErr(err)
	d.decoding <- block
	return true
}

// checkCRC will check the checksum if the frame has one.
// Will return ErrCRCMismatch if crc check failed, otherwise nil.
func (d *frameDec) checkCRC() error {
	if !d.HasCheckSum {
		return nil
	}
	var tmp [4]byte
	got := d.crc.Sum64()
	// Flip to match file order.
	tmp[0] = byte(got >> 0)
	tmp[1] = byte(got >> 8)
	tmp[2] = byte(got >> 16)
	tmp[3] = byte(got >> 24)

	// We can overwrite upper tmp now
	want := d.rawInput.readSmall(4)
	if want == nil {
		println("CRC missing?")
		return io.ErrUnexpectedEOF
	}

	if !bytes.Equal(tmp[:], want) {
		if debug {
			println("CRC Check Failed:", tmp[:], "!=", want)
		}
		return ErrCRCMismatch
	}
	if debug {
		println("CRC ok", tmp[:])
	}
	return nil
}

func (d *frameDec) initAsync() {
	if !d.o.lowMem && !d.SingleSegment {
		// set max extra size history to 10MB.
		d.history.maxSize = d.history.windowSize + maxBlockSize*5
	}
	// re-alloc if more than one extra block size.
	if d.o.lowMem && cap(d.history.b) > d.history.maxSize+maxBlockSize {
		d.history.b = make([]byte, 0, d.history.maxSize)
	}
	if cap(d.history.b) < d.history.maxSize {
		d.history.b = make([]byte, 0, d.history.maxSize)
	}
	if cap(d.decoding) < d.o.concurrent {
		d.decoding = make(chan *blockDec, d.o.concurrent)
	}
	if debug {
		h := d.history
		printf("history init. len: %d, cap: %d", len(h.b), cap(h.b))
	}
	d.asyncRunningMu.Lock()
	d.asyncRunning = true
	d.asyncRunningMu.Unlock()
}

// startDecoder will start decoding blocks and write them to the writer.
// The decoder will stop as soon as an error occurs or at end of frame.
// When the frame has finished decoding the *bufio.Reader
// containing the remaining input will be sent on frameDec.frameDone.
func (d *frameDec) startDecoder(output chan decodeOutput) {
	written := int64(0)

	defer func() {
		d.asyncRunningMu.Lock()
		d.asyncRunning = false
		d.asyncRunningMu.Unlock()

		// Drain the currently decoding.
		d.history.error = true
	flushdone:
		for {
			select {
			case b := <-d.decoding:
				b.history <- &d.history
				output <- <-b.result
			default:
				break flushdone
			}
		}
		println("frame decoder done, signalling done")
		d.frameDone.Done()
	}()
	// Get decoder for first block.
	block := <-d.decoding
	block.history <- &d.history
	for {
		var next *blockDec
		// Get result
		r := <-block.result
		if r.err != nil {
			println("Result contained error", r.err)
			output <- r
			return
		}
		if debug {
			println("got result, from ", d.offset, "to", d.offset+int64(len(r.b)))
			d.offset += int64(len(r.b))
		}
		if !block.Last {
			// Send history to next block
			select {
			case next = <-d.decoding:
				if debug {
					println("Sending ", len(d.history.b), "bytes as history")
				}
				next.history <- &d.history
			default:
				// Wait until we have sent the block, so
				// other decoders can potentially get the decoder.
				next = nil
			}
		}

		// Add checksum, async to decoding.
		if d.HasCheckSum {
			n, err := d.crc.Write(r.b)
			if err != nil {
				r.err = err
				if n != len(r.b) {
					r.err = io.ErrShortWrite
				}
				output <- r
				return
			}
		}
		written += int64(len(r.b))
		if d.SingleSegment && uint64(written) > d.FrameContentSize {
			println("runDecoder: single segment and", uint64(written), ">", d.FrameContentSize)
			r.err = ErrFrameSizeExceeded
			output <- r
			return
		}
		if block.Last {
			r.err = d.checkCRC()
			output <- r
			return
		}
		output <- r
		if next == nil {
			// There was no decoder available, we wait for one now that we have sent to the writer.
			if debug {
				println("Sending ", len(d.history.b), " bytes as history")
			}
			next = <-d.decoding
			next.history <- &d.history
		}
		block = next
	}
}

// runDecoder will create a sync decoder that will decode a block of data.
func (d *frameDec) runDecoder(dst []byte, dec *blockDec) ([]byte, error) {
	saved := d.history.b

	// We use the history for output to avoid copying it.
	d.history.b = dst
	// Store input length, so we only check new data.
	crcStart := len(dst)
	var err error
	for {
		err = dec.reset(d.rawInput, d.WindowSize)
		if err != nil {
			break
		}
		if debug {
			println("next block:", dec)
		}
		err = dec.decodeBuf(&d.history)
		if err != nil || dec.Last {
			break
		}
		if uint64(len(d.history.b)) > d.o.maxDecodedSize {
			err = ErrDecoderSizeExceeded
			break
		}
		if d.SingleSegment && uint64(len(d.history.b)) > d.o.maxDecodedSize {
			println("runDecoder: single segment and", uint64(len(d.history.b)), ">", d.o.maxDecodedSize)
			err = ErrFrameSizeExceeded
			break
		}
	}
	dst = d.history.b
	if err == nil {
		if d.HasCheckSum {
			var n int
			n, err = d.crc.Write(dst[crcStart:])
			if err == nil {
				if n != len(dst)-crcStart {
					err = io.ErrShortWrite
				} else {
					err = d.checkCRC()
				}
			}
		}
	}
	d.history.b = saved
	return dst, err
}
