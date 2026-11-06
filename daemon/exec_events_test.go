package daemon

import (
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestExecEventAttributes(t *testing.T) {
	t.Run("labels merged under reserved keys", func(t *testing.T) {
		got := execEventAttributes(
			map[string]string{"com.example.hook": "post_start", "execID": "spoofed"},
			map[string]string{"execID": "abc123"},
		)
		assert.Check(t, is.DeepEqual(got, map[string]string{
			"com.example.hook": "post_start",
			"execID":           "abc123",
		}))
	})

	t.Run("no labels passes attributes through", func(t *testing.T) {
		attrs := map[string]string{"execID": "abc123"}
		got := execEventAttributes(nil, attrs)
		assert.Check(t, is.DeepEqual(got, attrs))
	})
}
