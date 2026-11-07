package progress

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestOutputOnPrematureClose(t *testing.T) {
	content := []byte("TESTING")
	reader := io.NopCloser(bytes.NewReader(content))
	progressChan := make(chan Progress, 10)

	pr := NewProgressReader(reader, ChanOutput(progressChan), int64(len(content)), "Test", "Read")

	part := make([]byte, 4)
	_, err := io.ReadFull(pr, part)
	if err != nil {
		pr.Close()
		t.Fatal(err)
	}

drainLoop:
	for {
		select {
		case <-progressChan:
		default:
			break drainLoop
		}
	}

	pr.Close()

	select {
	case <-progressChan:
	default:
		t.Fatalf("Expected some output when closing prematurely")
	}
}

func TestProgressReaderSetsStart(t *testing.T) {
	content := []byte("TESTING")
	reader := io.NopCloser(bytes.NewReader(content))
	progressChan := make(chan Progress, 10)

	before := time.Now().Unix()
	pr := NewProgressReader(reader, ChanOutput(progressChan), int64(len(content)), "Test", "Read")

	if _, err := io.ReadAll(pr); err != nil {
		pr.Close()
		t.Fatal(err)
	}
	pr.Close()
	close(progressChan)

	var updates int
	var start int64
	for p := range progressChan {
		updates++
		if p.Start <= 0 {
			t.Errorf("expected Start to be populated (> 0), got %d", p.Start)
		}
		switch {
		case start == 0:
			start = p.Start
		case p.Start != start:
			t.Errorf("expected a stable Start across updates, got %d and %d", start, p.Start)
		}
	}
	if updates == 0 {
		t.Fatal("expected at least one progress update")
	}
	if start < before {
		t.Errorf("expected Start (%d) to be >= reader creation time (%d)", start, before)
	}
}

func TestCompleteSilently(t *testing.T) {
	content := []byte("TESTING")
	reader := io.NopCloser(bytes.NewReader(content))
	progressChan := make(chan Progress, 10)

	pr := NewProgressReader(reader, ChanOutput(progressChan), int64(len(content)), "Test", "Read")

	out, err := io.ReadAll(pr)
	if err != nil {
		pr.Close()
		t.Fatal(err)
	}
	if string(out) != "TESTING" {
		pr.Close()
		t.Fatalf("Unexpected output %q from reader", string(out))
	}

drainLoop:
	for {
		select {
		case <-progressChan:
		default:
			break drainLoop
		}
	}

	pr.Close()

	select {
	case <-progressChan:
		t.Fatalf("Should have closed silently when read is complete")
	default:
	}
}
