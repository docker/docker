//+build !windows

package daemon

import "github.com/go-check/check"

func (s *DockerSuite) TestContainerTopValidatePSArgs(c *check.C) {
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
		c.Logf("tested %q, got err=%v", psArgs, err)
		if errExpected && err == nil {
			c.Fatalf("expected error, got %v (%q)", err, psArgs)
		}
		if !errExpected && err != nil {
			c.Fatalf("expected nil, got %v (%q)", err, psArgs)
		}
	}
}

func (s *DockerSuite) TestContainerTopParsePSOutput(c *check.C) {
	tests := []struct {
		output      []byte
		pids        []int
		errExpected bool
	}{
		{[]byte(`  PID COMMAND
   42 foo
   43 bar
  100 baz
`), []int{42, 43}, false},
		{[]byte(`  UID COMMAND
   42 foo
   43 bar
  100 baz
`), []int{42, 43}, true},
		// unicode space (U+2003, 0xe2 0x80 0x83)
		{[]byte(` PID COMMAND
   42 foo
   43 bar
  100 baz
`), []int{42, 43}, true},
		// the first space is U+2003, the second one is ascii.
		{[]byte(` PID COMMAND
   42 foo
   43 bar
  100 baz
`), []int{42, 43}, true},
	}

	for _, f := range tests {
		_, err := parsePSOutput(f.output, f.pids)
		c.Logf("tested %q, got err=%v", string(f.output), err)
		if f.errExpected && err == nil {
			c.Fatalf("expected error, got %v (%q)", err, string(f.output))
		}
		if !f.errExpected && err != nil {
			c.Fatalf("expected nil, got %v (%q)", err, string(f.output))
		}
	}
}
