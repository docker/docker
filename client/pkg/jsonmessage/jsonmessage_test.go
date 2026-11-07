package jsonmessage

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/jsonstream"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestRenderTUIProgress(t *testing.T) {
	type expected struct {
		short string
		long  string
	}

	shortAndLong := func(short, long string) expected {
		return expected{short: short, long: long}
	}

	start := time.Date(2017, 12, 3, 15, 10, 1, 0, time.UTC)
	timeAfter := func(delta time.Duration) func() time.Time {
		return func() time.Time {
			return start.Add(delta)
		}
	}

	testcases := []struct {
		name     string
		progress jsonstream.Progress
		expected expected
		nowFunc  func() time.Time
	}{
		{
			name: "no progress",
		},
		{
			name:     "progress 1",
			progress: jsonstream.Progress{Current: 1},
			expected: shortAndLong("      1B", "      1B"),
		},
		{
			name: "some progress with a start time",
			progress: jsonstream.Progress{
				Current: 20,
				Total:   100,
				Start:   start.Unix(),
			},
			nowFunc: timeAfter(time.Second),
			expected: shortAndLong(
				"     20B/100B 4s",
				"[==========>                                        ]      20B/100B 4s",
			),
		},
		{
			name:     "some progress without a start time",
			progress: jsonstream.Progress{Current: 50, Total: 100},
			expected: shortAndLong(
				"     50B/100B",
				"[=========================>                         ]      50B/100B",
			),
		},
		{
			name:     "current more than total is not negative gh#7136",
			progress: jsonstream.Progress{Current: 50, Total: 40},
			expected: shortAndLong(
				"     50B",
				"[==================================================>]      50B",
			),
		},
		{
			name:     "with units",
			progress: jsonstream.Progress{Current: 50, Total: 100, Units: "units"},
			expected: shortAndLong(
				"50/100 units",
				"[=========================>                         ] 50/100 units",
			),
		},
		{
			name:     "current more than total with units is not negative ",
			progress: jsonstream.Progress{Current: 50, Total: 40, Units: "units"},
			expected: shortAndLong(
				"50 units",
				"[==================================================>] 50 units",
			),
		},
		{
			name:     "hide counts",
			progress: jsonstream.Progress{Current: 50, Total: 100, HideCounts: true},
			expected: shortAndLong(
				"",
				"[=========================>                         ] ",
			),
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			if testcase.nowFunc != nil {
				originalTimeNow := timeNow
				timeNow = testcase.nowFunc
				defer func() { timeNow = originalTimeNow }()
			}
			assert.Equal(t, RenderTUIProgress(testcase.progress, 100), testcase.expected.short)
			assert.Equal(t, RenderTUIProgress(testcase.progress, 200), testcase.expected.long)
		})
	}
}

func TestRenderTUIProgress_negativeCurrent(t *testing.T) {
	// Current can be negative on a decoded stream; the "=" repeat count must
	// stay non-negative or strings.Repeat panics (companion to gh#7136, which
	// clamped the trailing spaces but not the "=" run). A panic fails the test.
	out := RenderTUIProgress(jsonstream.Progress{Current: -100, Total: 100}, 200)
	assert.Assert(t, !strings.Contains(out, "="), "negative progress should clamp the bar; got %q", out)
}

func TestDisplay(t *testing.T) {
	messages := map[jsonstream.Message][]string{
		// Empty
		{}: {"\n", "\n"},
		// Status
		{
			Status: "status",
		}: {
			"status\n",
			"status\n",
		},
		// General
		{
			ID:     "ID",
			Status: "status",
		}: {
			"ID: status\n",
			"ID: status\n",
		},
		// Stream over status
		{
			Status: "status",
			Stream: "stream",
		}: {
			"stream",
			"stream",
		},
		// With progress, stream empty
		{
			Status:   "status",
			Stream:   "",
			Progress: &jsonstream.Progress{Current: 1},
		}: {
			"",
			fmt.Sprintf("%c[2K\rstatus       1B\r", 27),
		},
	}

	// The tests :)
	for jsonMessage, expectedMessages := range messages {
		// Without terminal
		data := bytes.NewBuffer([]byte{})
		if err := Display(jsonMessage, data, false, 0); err != nil {
			t.Fatal(err)
		}
		if data.String() != expectedMessages[0] {
			t.Fatalf("Expected %q,got %q", expectedMessages[0], data.String())
		}
		// With terminal
		data = bytes.NewBuffer([]byte{})
		if err := Display(jsonMessage, data, true, 0); err != nil {
			t.Fatal(err)
		}
		if data.String() != expectedMessages[1] {
			t.Fatalf("\nExpected %q\n     got %q", expectedMessages[1], data.String())
		}
	}
}

// Test jsonstream.Message with an Error. It returns an error with the given text, not the meaning of the HTTP code.
func TestDisplayWithJSONError(t *testing.T) {
	data := bytes.NewBuffer([]byte{})
	jsonMessage := jsonstream.Message{Error: &jsonstream.Error{Code: 404, Message: "Can't find it"}}

	err := Display(jsonMessage, data, true, 0)
	if err == nil || err.Error() != "Can't find it" {
		t.Fatalf("Expected a jsonstream.Error 404, got %q", err)
	}

	jsonMessage = jsonstream.Message{Error: &jsonstream.Error{Code: 401, Message: "Anything"}}
	err = Display(jsonMessage, data, true, 0)
	assert.Check(t, is.Error(err, "Anything"))
}

func TestDisplayStreamInvalidJSON(t *testing.T) {
	data := bytes.NewBuffer([]byte{})
	reader := strings.NewReader("This is not a 'valid' JSON []")
	exp := "invalid character "
	if err := DisplayStream(reader, data); err == nil || !strings.HasPrefix(err.Error(), exp) {
		t.Fatalf("Expected error (%s...), got %q", exp, err)
	}
}

func TestDisplayStreamWithMessagePrinter(t *testing.T) {
	testcases := []struct {
		name        string
		input       string
		expected    string
		expectedErr string
		printer     func(jsonstream.Message, io.Writer) error
		auxCallback func(jsonstream.Message)
	}{
		{
			name:     "message",
			input:    `{"status":"status"}`,
			expected: "custom",
			printer: func(jm jsonstream.Message, w io.Writer) error {
				assert.Equal(t, jm.Status, "status")
				_, err := fmt.Fprint(w, "custom")
				return err
			},
		},
		{
			name:        "message error",
			input:       `{"errorDetail":{"message":"boom"},"error":"boom"}`,
			expectedErr: "boom",
			printer: func(jm jsonstream.Message, _ io.Writer) error {
				assert.Assert(t, jm.Error != nil)
				assert.Equal(t, jm.Error.Message, "boom")
				return jm.Error
			},
		},
		{
			name:     "auxiliary message",
			input:    `{"aux":{"ID":"sha256:deadbeef"}}{"status":"status"}`,
			expected: "custom",
			printer: func(jm jsonstream.Message, w io.Writer) error {
				assert.Equal(t, jm.Status, "status")
				_, err := fmt.Fprint(w, "custom")
				return err
			},
			auxCallback: func(jm jsonstream.Message) {
				assert.Assert(t, jm.Aux != nil)
			},
		},
		{
			name:     "auxiliary message without callback",
			input:    `{"aux":{"ID":"sha256:deadbeef"}}`,
			expected: "",
			printer: func(jsonstream.Message, io.Writer) error {
				t.Fatal("message printer must not be called for auxiliary messages")
				return nil
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			opts := []DisplayOpt{
				WithMessagePrinter(testcase.printer),
			}
			if testcase.auxCallback != nil {
				opts = append(opts, WithAuxCallback(testcase.auxCallback))
			}

			var out bytes.Buffer
			err := DisplayStream(strings.NewReader(testcase.input), &out, opts...)
			if testcase.expectedErr != "" {
				assert.Error(t, err, testcase.expectedErr)
			} else {
				assert.NilError(t, err)
			}
			assert.Equal(t, out.String(), testcase.expected)
		})
	}
}

func TestDisplayJSONMessagesStream(t *testing.T) {
	messages := map[string][]string{
		// empty string
		"": {
			"",
			"",
		},
		// Without progress & ID
		`{ "status": "status" }`: {
			"status\n",
			"status\n",
		},
		// Without progress, with ID
		`{ "id": "ID","status": "status" }`: {
			"ID: status\n",
			"ID: status\n",
		},
		// With progressDetail
		`{ "id": "ID", "status": "status", "progressDetail": { "Current": 1} }`: {
			"", // progressbar is disabled in non-terminal
			fmt.Sprintf("\n%c[%dA%c[2K\rID: status       1B\r%c[%dB", 27, 1, 27, 27, 1),
		},
	}

	// Use $TERM which is unlikely to exist, forcing DisplayJSONMessageStream to
	// (hopefully) use &noTermInfo.
	t.Setenv("TERM", "xyzzy-non-existent-terminfo")

	for jsonMessage, expectedMessages := range messages {
		data := bytes.NewBuffer([]byte{})
		reader := strings.NewReader(jsonMessage)

		// Without terminal
		if err := DisplayStream(reader, data); err != nil {
			t.Fatal(err)
		}
		if data.String() != expectedMessages[0] {
			t.Fatalf("Expected an %q, got %q", expectedMessages[0], data.String())
		}

		// With terminal (forcing terminal mode)
		data = bytes.NewBuffer([]byte{})
		reader = strings.NewReader(jsonMessage)
		if err := DisplayJSONMessagesStream(reader, data, 0, true, nil); err != nil {
			t.Fatal(err)
		}
		if data.String() != expectedMessages[1] {
			t.Fatalf("\nExpected %q\n     got %q", expectedMessages[1], data.String())
		}
	}
}
