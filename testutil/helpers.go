package testutil // import "github.com/docker/docker/testutil"

import (
	"io"
)

// DevZero acts like /dev/zero but in an OS-independent fashion.
var DevZero io.Reader = devZero{}

type devZero struct{}

func (d devZero) Read(p []byte) (n int, err error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
