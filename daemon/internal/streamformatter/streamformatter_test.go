package streamformatter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/v2/daemon/internal/progress"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestFormatStatus(t *testing.T) {
	res := FormatStatus("ID", "%s%d", "a", 1)
	expected := `{"status":"a1","id":"ID"}` + streamNewline
	assert.Check(t, is.Equal(expected, string(res)))
}

func TestFormatError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "regular error",
			err:      errors.New("something bad happened"),
			expected: `{"error":"something bad happened","errorDetail":{"message":"something bad happened"}}`,
		},
		{
			name:     "JSON error",
			err:      &jsonstream.Error{Code: http.StatusBadRequest, Message: "JSON error"},
			expected: `{"error":"JSON error","errorDetail":{"code":400,"message":"JSON error"}}`,
		},
		{
			name:     "wrapped JSON error",
			err:      fmt.Errorf("pull failed: %w", &jsonstream.Error{Code: http.StatusBadRequest, Message: "JSON error"}),
			expected: `{"error":"JSON error","errorDetail":{"code":400,"message":"JSON error"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := FormatError(tc.err)
			assert.Check(t, is.Equal(tc.expected+streamNewline, string(res)))
		})
	}
}

func TestJsonProgressFormatterFormatProgress(t *testing.T) {
	sf := &jsonProgressFormatter{}
	jsonProgress := &jsonstream.Progress{
		Current: 15,
		Total:   30,
		Start:   1,
	}
	aux := "aux message"
	res := sf.formatProgress("id", "action", jsonProgress, aux)
	msg := &jsonstream.Message{}

	assert.NilError(t, json.Unmarshal(res, msg))

	rawAux := json.RawMessage(`"` + aux + `"`)
	expected := &jsonstream.Message{
		ID:       "id",
		Status:   "action",
		Aux:      &rawAux,
		Progress: jsonProgress,
	}
	assert.DeepEqual(t, msg, expected, cmpJSONMessageOpt())
}

func cmpJSONMessageOpt() cmp.Option {
	progressMessagePath := func(path cmp.Path) bool {
		return path.String() == "ProgressMessage"
	}
	return cmp.Options{
		// Ignore deprecated property that is a derivative of Progress
		cmp.FilterPath(progressMessagePath, cmp.Ignore()),
	}
}

func TestJsonProgressFormatterFormatStatus(t *testing.T) {
	sf := jsonProgressFormatter{}
	res := sf.formatStatus("ID", "%s%d", "a", 1)
	assert.Check(t, is.Equal(`{"status":"a1","id":"ID"}`+streamNewline, string(res)))
}

func TestJSONProgressOutputPassesStart(t *testing.T) {
	var buf bytes.Buffer
	err := NewJSONProgressOutput(&buf, false).WriteProgress(progress.Progress{
		ID:      "id",
		Action:  "Downloading",
		Current: 15,
		Total:   30,
		Start:   12345,
	})
	assert.NilError(t, err)
	expected := `{"status":"Downloading","progressDetail":{"current":15,"total":30,"start":12345},"id":"id"}` + streamNewline
	assert.Check(t, is.Equal(expected, buf.String()))
}

func TestNewJSONProgressOutput(t *testing.T) {
	b := bytes.Buffer{}
	b.Write(FormatStatus("id", "Downloading"))
	_ = NewJSONProgressOutput(&b, false)
	assert.Check(t, is.Equal(`{"status":"Downloading","id":"id"}`+streamNewline, b.String()))
}

func TestAuxFormatterEmit(t *testing.T) {
	b := bytes.Buffer{}
	aux := &AuxFormatter{Writer: &b}
	sampleAux := &struct {
		Data string
	}{"Additional data"}
	err := aux.Emit("", sampleAux)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(`{"aux":{"Data":"Additional data"}}`+streamNewline, b.String()))
}
