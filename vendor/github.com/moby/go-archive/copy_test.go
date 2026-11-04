package archive

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func TestNewNameRebaser(t *testing.T) {
	tests := []struct {
		name     string
		oldBase  string
		newBase  string
		input    string
		expected string
	}{
		{
			name:     "exact oldBase match",
			oldBase:  "dir",
			newBase:  "dst",
			input:    "dir",
			expected: "dst",
		},
		{
			name:     "oldBase directory prefix match",
			oldBase:  "dir",
			newBase:  "dst",
			input:    "dir/sub/file.txt",
			expected: "dst/sub/file.txt",
		},
		{
			name:     "sibling directory escape attempt (issue #52948)",
			oldBase:  "dir",
			newBase:  "dst",
			input:    "dir2/sub/file.txt",
			expected: "dst/dir2/sub/file.txt",
		},
		{
			name:     "completely different directory",
			oldBase:  "dir",
			newBase:  "dst",
			input:    "other/file.txt",
			expected: "dst/other/file.txt",
		},
		{
			name:     "leading slash in input",
			oldBase:  "dir",
			newBase:  "dst",
			input:    "/other/file.txt",
			expected: "dst/other/file.txt",
		},
		{
			name:     "empty newBase exact match",
			oldBase:  "dir",
			newBase:  "",
			input:    "dir",
			expected: "",
		},
		{
			name:     "empty newBase directory prefix match",
			oldBase:  "dir",
			newBase:  "",
			input:    "dir/sub/file.txt",
			expected: "sub/file.txt",
		},
		{
			name:     "empty newBase sibling escape attempt",
			oldBase:  "dir",
			newBase:  "",
			input:    "dir2/sub/file.txt",
			expected: "dir2/sub/file.txt",
		},
		{
			name:     "empty oldBase",
			oldBase:  "",
			newBase:  "dst",
			input:    "file.txt",
			expected: "dst/file.txt",
		},
		{
			name:     "empty oldBase and newBase",
			oldBase:  "",
			newBase:  "",
			input:    "/file.txt",
			expected: "file.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rebaser := newNameRebaser(tt.oldBase, tt.newBase)
			got := rebaser(tt.input)
			if got != tt.expected {
				t.Errorf("newNameRebaser(%q, %q)(%q) = %q; want %q", tt.oldBase, tt.newBase, tt.input, got, tt.expected)
			}
		})
	}
}

func TestRebaseArchiveEntriesConfinement(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	entries := []string{"dir/a", "dir2/b", "other/c"}
	for _, name := range entries {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(name)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader failed: %v", err)
		}
		if _, err := tw.Write([]byte(name)); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	rebasedStream := RebaseArchiveEntries(&buf, "dir", "dst")
	tr := tar.NewReader(rebasedStream)

	expectedNames := []string{"dst/a", "dst/dir2/b", "dst/other/c"}
	var gotNames []string

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tr.Next failed: %v", err)
		}
		gotNames = append(gotNames, hdr.Name)
	}

	if len(gotNames) != len(expectedNames) {
		t.Fatalf("got %d entries %v; want %d entries %v", len(gotNames), gotNames, len(expectedNames), expectedNames)
	}

	for i := range gotNames {
		if gotNames[i] != expectedNames[i] {
			t.Errorf("entry %d: got %q; want %q", i, gotNames[i], expectedNames[i])
		}
	}
}
