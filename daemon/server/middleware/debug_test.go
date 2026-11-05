package middleware

import (
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestMaskSecretKeys(t *testing.T) {
	tests := []struct {
		doc      string
		input    map[string]any
		expected map[string]any
	}{
		{
			doc:      "secret/config create and update requests",
			input:    map[string]any{"Data": "foo", "Name": "name", "Labels": map[string]any{}},
			expected: map[string]any{"Data": "*****", "Name": "name", "Labels": map[string]any{}},
		},
		{
			doc: "masking other fields (recursively)",
			input: map[string]any{
				"password":     "pass",
				"secret":       "secret",
				"jointoken":    "jointoken",
				"unlockkey":    "unlockkey",
				"signingcakey": "signingcakey",
				"other": map[string]any{
					"password":     "pass",
					"secret":       "secret",
					"jointoken":    "jointoken",
					"unlockkey":    "unlockkey",
					"signingcakey": "signingcakey",
				},
			},
			expected: map[string]any{
				"password":     "*****",
				"secret":       "*****",
				"jointoken":    "*****",
				"unlockkey":    "*****",
				"signingcakey": "*****",
				"other": map[string]any{
					"password":     "*****",
					"secret":       "*****",
					"jointoken":    "*****",
					"unlockkey":    "*****",
					"signingcakey": "*****",
				},
			},
		},
		{
			// Credentials from registry.AuthConfig (POST /auth). Matching is
			// case-insensitive (strings.EqualFold), and only exact credential
			// field names are scrubbed, so non-secret fields (username,
			// serveraddress) are preserved.
			doc: "registry auth credentials (POST /auth)",
			input: map[string]any{
				"username":      "user",
				"serveraddress": "registry.example.test",
				"auth":          "dXNlcjpwYXNz",
				"IdEnTiTyToKeN": "id-token",
				"registrytoken": "reg-token",
			},
			expected: map[string]any{
				"username":      "user",
				"serveraddress": "registry.example.test",
				"auth":          "*****",
				"IdEnTiTyToKeN": "*****",
				"registrytoken": "*****",
			},
		},
		{
			doc: "case insensitive field matching",
			input: map[string]any{
				"PASSWORD": "pass",
				"other": map[string]any{
					"PASSWORD": "pass",
				},
			},
			expected: map[string]any{
				"PASSWORD": "*****",
				"other": map[string]any{
					"PASSWORD": "*****",
				},
			},
		},
	}

	for _, testcase := range tests {
		t.Run(testcase.doc, func(t *testing.T) {
			maskSecretKeys(testcase.input)
			assert.Check(t, is.DeepEqual(testcase.expected, testcase.input))
		})
	}
}
