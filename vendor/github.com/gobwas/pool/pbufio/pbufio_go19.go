// +build !go1.10

package pbufio

import "bufio"

func writerSize(bw *bufio.Writer) int {
	return bw.Available() + bw.Buffered()
}

// readerSize returns buffer size of the given buffered reader.
// NOTE: current workaround implementation resets underlying io.Reader.
func readerSize(br *bufio.Reader) int {
	br.Reset(sizeReader)
	br.ReadByte()
	n := br.Buffered() + 1
	br.Reset(nil)
	return n
}

var sizeReader optimisticReader

type optimisticReader struct{}

func (optimisticReader) Read(p []byte) (int, error) {
	return len(p), nil
}
