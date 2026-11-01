package ioutils

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReadCloserWrapperClose(t *testing.T) {
	const text = "hello world"
	testErr := errors.New("this will be called when closing")
	wrapper := NewReadCloserWrapper(strings.NewReader(text), func() error {
		return testErr
	})

	buf, err := io.ReadAll(wrapper)
	if err != nil {
		t.Errorf("io.ReadAll(wrapper) err = %v", err)
	}
	if string(buf) != text {
		t.Errorf("expected %v, got: %v", text, string(buf))
	}
	err = wrapper.Close()
	if !errors.Is(err, testErr) {
		// readCloserWrapper should have called the anonymous func and thus, fail
		t.Errorf("expected %v, got: %v", testErr, err)
	}
}

type perpetualReader struct{}

func (p *perpetualReader) Read(buf []byte) (int, error) {
	for i := 0; i != len(buf); i++ {
		buf[i] = 'a'
	}
	return len(buf), nil
}

func TestCancelReadCloser(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	crc := NewCancelReadCloser(ctx, io.NopCloser(&perpetualReader{}))
	for {
		var buf [128]byte
		_, err := crc.Read(buf[:])
		if errors.Is(err, context.DeadlineExceeded) {
			break
		} else if os.IsTimeout(err) {
			break
		} else if err != nil {
			t.Fatalf("got unexpected error: %v", err)
		}
	}
}
