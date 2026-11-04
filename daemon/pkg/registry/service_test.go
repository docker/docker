package registry

import (
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestParseRegistryHostName(t *testing.T) {
	tests := []struct {
		doc           string
		serverAddress string
		want          string
		wantErr       string
	}{
		{
			doc:           "localhost without scheme",
			serverAddress: "localhost",
			want:          "localhost",
		},
		{
			doc:           "localhost with port without scheme",
			serverAddress: "localhost:5000",
			want:          "localhost:5000",
		},
		{
			doc:           "hostname and port without scheme",
			serverAddress: "registry.example.com:5000",
			want:          "registry.example.com:5000",
		},
		{
			doc:           "ipv4 host and port without scheme",
			serverAddress: "127.0.0.1:8080",
			want:          "127.0.0.1:8080",
		},
		{
			doc:           "ipv6 host and port without scheme",
			serverAddress: "[::1]:5000",
			want:          "[::1]:5000",
		},
		{
			doc:           "https URL",
			serverAddress: "https://127.0.0.1:8080",
			want:          "127.0.0.1:8080",
		},
		{
			doc:           "https URL with path",
			serverAddress: "https://registry.example.com/foo",
			want:          "registry.example.com",
		},
		{
			doc:           "userinfo like input without scheme",
			serverAddress: "user:pass@example.com",
			want:          `example.com`,
		},
		{
			doc:           "unsupported scheme",
			serverAddress: "ftp://registry.example.com",
			wantErr:       `unsupported URL scheme "ftp"`,
		},
		{
			doc:           "malformed http URL",
			serverAddress: "http://[::1",
			wantErr:       `invalid server address: unable to parse:`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			got, err := parseRegistryHostName(tc.serverAddress)
			if tc.wantErr != "" {
				assert.Check(t, is.ErrorContains(err, tc.wantErr))
				assert.Check(t, is.Equal(got, tc.want))
				return
			}
			assert.Check(t, err)
			assert.Check(t, is.Equal(got, tc.want))
		})
	}
}
