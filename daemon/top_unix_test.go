//+build !windows

package daemon // import "github.com/docker/docker/daemon"

import (
	"testing"
)

func TestContainerTopValidatePSArgs(t *testing.T) {
	tests := map[string]bool{
		"ae -o uid=PID":             true,
		"ae -o \"uid= PID\"":        true,  // ascii space (0x20)
		"ae -o \"uid= PID\"":        false, // unicode space (U+2003, 0xe2 0x80 0x83)
		"ae o uid=PID":              true,
		"aeo uid=PID":               true,
		"ae -O uid=PID":             true,
		"ae -o pid=PID2 -o uid=PID": true,
		"ae -o pid=PID":             false,
		"ae -o pid=PID -o uid=PIDX": true, // FIXME: we do not need to prohibit this
		"aeo pid=PID":               false,
		"ae":                        false,
		"":                          false,
	}
	for psArgs, errExpected := range tests {
		err := validatePSArgs(psArgs)
		t.Logf("tested %q, got err=%v", psArgs, err)
		if errExpected && err == nil {
			t.Fatalf("expected error, got %v (%q)", err, psArgs)
		}
		if !errExpected && err != nil {
			t.Fatalf("expected nil, got %v (%q)", err, psArgs)
		}
	}
}

func TestCorrectPidValue(t *testing.T) {
	tests := []struct {
		line         string
		pidValue     string
		expectedLine string
	}{
		{
			string("abcd 1234root 3456"),
			string("1234root"),
			string("abcd 1234 root 3456"),
		},
		{
			string("abcd 123456 3456"),
			string("123456"),
			string("abcd 123456 3456"),
		},
	}

	for _, f := range tests {
		newline := correctPidValue(f.line, f.pidValue)
		if f.expectedLine != newline {
			t.Fatalf("expected line: %v, got %v", f.expectedLine, newline)
		}
	}
}

func TestContainerTopParsePSOutput(t *testing.T) {
	tests := []struct {
		output      []byte
		pids        []uint32
		errExpected bool
	}{
		{[]byte(`  PID COMMAND
   42 foo
   43 bar
		- -
  100 baz
`), []uint32{42, 43}, false},
		{[]byte(`  UID COMMAND
   42 foo
   43 bar
		- -
  100 baz
`), []uint32{42, 43}, true},
		// unicode space (U+2003, 0xe2 0x80 0x83)
		{[]byte(` PID COMMAND
   42 foo
   43 bar
		- -
  100 baz
`), []uint32{42, 43}, true},
		// the first space is U+2003, the second one is ascii.
		{[]byte(` PID COMMAND
   42 foo
   43 bar
  100 baz
`), []uint32{42, 43}, true},
		{[]byte(`  PID COMMAND
   42root
`), []uint32{42}, false},
		{[]byte(`  PID COMMAND
   42root
   43 bar
   100 baz
`), []uint32{42, 43, 100}, false},
		{[]byte(`  PID COMMAND
   43 bar
   42root
   100 baz
`), []uint32{43, 42, 100}, false},
		{[]byte(`  PID COMMAND
   43 bar
   100 baz
   42root
`), []uint32{43, 100, 42}, false},
		{[]byte(`  PID COMMAND
   43 bar
   100123
   42root
`), []uint32{43, 100123, 42}, false},
	}

	for _, f := range tests {
		_, err := parsePSOutput(f.output, f.pids)
		t.Logf("tested %q, got err=%v", string(f.output), err)
		if f.errExpected && err == nil {
			t.Fatalf("expected error, got %v (%q)", err, string(f.output))
		}
		if !f.errExpected && err != nil {
			t.Fatalf("expected nil, got %v (%q)", err, string(f.output))
		}
	}
}
