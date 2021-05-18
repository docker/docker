// +build go1.10

package pbufio

import "bufio"

func writerSize(bw *bufio.Writer) int {
	return bw.Size()
}

func readerSize(br *bufio.Reader) int {
	return br.Size()
}
