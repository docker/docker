package streamformatter_test

import (
	"bytes"
	"testing"

	"github.com/moby/moby/client/pkg/progress"
	"github.com/moby/moby/client/pkg/streamformatter"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const streamNewline = "\r\n"

func TestRawProgressFormatterFormatStatus(t *testing.T) {
	var buf bytes.Buffer
	err := streamformatter.NewProgressOutput(&buf).WriteProgress(progress.Progress{
		ID:      "id", // not printed by rawProgressFormatter
		Message: "status",

		// Fields below must not be used if a Message is set.
		Action:  "action",
		Current: 15,
		Total:   30,
		Aux:     "aux message", // not printed by rawProgressFormatter
	})
	assert.NilError(t, err)

	expected := "status" + streamNewline
	assert.Check(t, is.Equal(buf.String(), expected))
}

func TestRawProgressFormatterFormatProgress(t *testing.T) {
	var buf bytes.Buffer
	err := streamformatter.NewProgressOutput(&buf).WriteProgress(progress.Progress{
		ID:      "id", // not printed by rawProgressFormatter
		Action:  "action",
		Current: 15,
		Total:   30,
		Aux:     "aux message", // not printed by rawProgressFormatter
	})
	assert.NilError(t, err)

	expected := `action [=========================>                         ]      15B/30B` + "\r"
	assert.Equal(t, buf.String(), expected)
}

func TestJSONProgressFormatterFormatProgress(t *testing.T) {
	var buf bytes.Buffer
	err := streamformatter.NewJSONProgressOutput(&buf, false).WriteProgress(progress.Progress{
		ID:      "id",
		Action:  "action",
		Current: 15,
		Total:   30,
		Aux:     "aux message",
	})
	assert.NilError(t, err)
	expected := `{"status":"action","progressDetail":{"current":15,"total":30},"id":"id","aux":"aux message"}` + streamNewline
	assert.Equal(t, buf.String(), expected)
}

func TestJSONProgressFormatterPassesStart(t *testing.T) {
	var buf bytes.Buffer
	err := streamformatter.NewJSONProgressOutput(&buf, false).WriteProgress(progress.Progress{
		ID:      "id",
		Action:  "Downloading",
		Current: 15,
		Total:   30,
		Start:   12345,
	})
	assert.NilError(t, err)
	expected := `{"status":"Downloading","progressDetail":{"current":15,"total":30,"start":12345},"id":"id"}` + streamNewline
	assert.Equal(t, buf.String(), expected)
}

func TestJSONProgressFormatterFormatStatus(t *testing.T) {
	var buf bytes.Buffer
	err := streamformatter.NewJSONProgressOutput(&buf, false).WriteProgress(progress.Progress{
		ID:      "ID",
		Message: "status",

		// Fields below must not be used if a Message is set.
		Action:  "action",
		Current: 15,
		Total:   30,
		Aux:     "aux message",
	})
	assert.NilError(t, err)
	expected := `{"status":"status","id":"ID"}` + streamNewline
	assert.Equal(t, buf.String(), expected)
}

func TestJSONProgressOutputWriteProgress(t *testing.T) {
	tests := []struct {
		doc        string
		newlines   bool
		lastUpdate bool
		expected   string
	}{
		{
			doc:      "no newlines",
			expected: `{"status":"Downloading","id":"id"}` + streamNewline,
		},
		{
			doc:        "no newlines last update",
			lastUpdate: true,
			expected:   `{"status":"Downloading","id":"id"}` + streamNewline,
		},
		{
			doc:        "newlines",
			newlines:   true,
			lastUpdate: false,
			expected:   `{"status":"Downloading","id":"id"}` + streamNewline,
		},
		{
			// Should print an extra (empty) message to add newlines after last message
			// (LastUpdate=true); see https://github.com/moby/moby/pull/1425
			doc:        "newlines last update",
			newlines:   true,
			lastUpdate: true,
			expected:   `{"status":"Downloading","id":"id"}` + streamNewline + `{}` + streamNewline,
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			var b bytes.Buffer
			po := streamformatter.NewJSONProgressOutput(&b, tc.newlines)

			err := po.WriteProgress(progress.Progress{
				ID:         "id",
				Message:    "Downloading",
				LastUpdate: tc.lastUpdate,
			})
			assert.NilError(t, err)
			assert.Check(t, is.Equal(b.String(), tc.expected))
		})
	}
}
