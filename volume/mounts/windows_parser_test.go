package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"gotest.tools/v3/assert"
)

func TestWindowsParseMountRaw(t *testing.T) {
	valid := []string{
		`d:\`,
		`d:`,
		`d:\path`,
		`d:\path with space`,
		`c:\:d:\`,
		`c:\windows\:d:`,
		`c:\windows:d:\s p a c e`,
		`c:\windows:d:\s p a c e:RW`,
		`c:\program files:d:\s p a c e i n h o s t d i r`,
		`0123456789name:d:`,
		`MiXeDcAsEnAmE:d:`,
		`name:D:`,
		`name:D::rW`,
		`name:D::RW`,
		`name:D::RO`,
		`c:/:d:/forward/slashes/are/good/too`,
		`c:/:d:/including with/spaces:ro`,
		`c:\Windows`,                // With capital
		`c:\Program Files (x86)`,    // With capitals and brackets
		`\\?\c:\windows\:d:`,        // Long path handling (source)
		`c:\windows\:\\?\d:\`,       // Long path handling (target)
		`\\.\pipe\foo:\\.\pipe\foo`, // named pipe
		`//./pipe/foo://./pipe/foo`, // named pipe forward slashes
	}

	invalid := map[string]string{
		``:                                 "invalid volume specification: ",
		`.`:                                "invalid volume specification: ",
		`..\`:                              "invalid volume specification: ",
		`c:\:..\`:                          "invalid volume specification: ",
		`c:\:d:\:xyzzy`:                    "invalid volume specification: ",
		`c:`:                               "cannot be `c:`",
		`c:\`:                              "cannot be `c:`",
		`c:\notexist:d:`:                   `source path does not exist: c:\notexist`,
		`c:\windows\system32\ntdll.dll:d:`: `source path must be a directory`,
		`name<:d:`:                         `invalid volume specification`,
		`name>:d:`:                         `invalid volume specification`,
		`name::d:`:                         `invalid volume specification`,
		`name":d:`:                         `invalid volume specification`,
		`name\:d:`:                         `invalid volume specification`,
		`name*:d:`:                         `invalid volume specification`,
		`name|:d:`:                         `invalid volume specification`,
		`name?:d:`:                         `invalid volume specification`,
		`name/:d:`:                         `invalid volume specification`,
		`d:\pathandmode:rw`:                `invalid volume specification`,
		`d:\pathandmode:ro`:                `invalid volume specification`,
		`con:d:`:                           `cannot be a reserved word for Windows filenames`,
		`PRN:d:`:                           `cannot be a reserved word for Windows filenames`,
		`aUx:d:`:                           `cannot be a reserved word for Windows filenames`,
		`nul:d:`:                           `cannot be a reserved word for Windows filenames`,
		`com1:d:`:                          `cannot be a reserved word for Windows filenames`,
		`com2:d:`:                          `cannot be a reserved word for Windows filenames`,
		`com3:d:`:                          `cannot be a reserved word for Windows filenames`,
		`com4:d:`:                          `cannot be a reserved word for Windows filenames`,
		`com5:d:`:                          `cannot be a reserved word for Windows filenames`,
		`com6:d:`:                          `cannot be a reserved word for Windows filenames`,
		`com7:d:`:                          `cannot be a reserved word for Windows filenames`,
		`com8:d:`:                          `cannot be a reserved word for Windows filenames`,
		`com9:d:`:                          `cannot be a reserved word for Windows filenames`,
		`lpt1:d:`:                          `cannot be a reserved word for Windows filenames`,
		`lpt2:d:`:                          `cannot be a reserved word for Windows filenames`,
		`lpt3:d:`:                          `cannot be a reserved word for Windows filenames`,
		`lpt4:d:`:                          `cannot be a reserved word for Windows filenames`,
		`lpt5:d:`:                          `cannot be a reserved word for Windows filenames`,
		`lpt6:d:`:                          `cannot be a reserved word for Windows filenames`,
		`lpt7:d:`:                          `cannot be a reserved word for Windows filenames`,
		`lpt8:d:`:                          `cannot be a reserved word for Windows filenames`,
		`lpt9:d:`:                          `cannot be a reserved word for Windows filenames`,
		`c:\windows\system32\ntdll.dll`:    `Only directories can be mapped on this platform`,
		`\\.\pipe\foo:c:\pipe`:             `'c:\pipe' is not a valid pipe path`,
	}

	parser := NewWindowsParser()
	if p, ok := parser.(*windowsParser); ok {
		p.fi = mockFiProvider{}
	}

	for _, path := range valid {
		if _, err := parser.ParseMountRaw(path, "local"); err != nil {
			t.Errorf("ParseMountRaw(`%q`) should succeed: error %q", path, err)
		}
	}

	for path, expectedError := range invalid {
		if mp, err := parser.ParseMountRaw(path, "local"); err == nil {
			t.Errorf("ParseMountRaw(`%q`) should have failed validation. Err '%v' - MP: %v", path, err, mp)
		} else {
			if !strings.Contains(err.Error(), expectedError) {
				t.Errorf("ParseMountRaw(`%q`) error should contain %q, got %v", path, expectedError, err.Error())
			}
		}
	}
}

func TestWindowsParseMountRawSplit(t *testing.T) {
	cases := []struct {
		bind      string
		driver    string
		expType   mount.Type
		expDest   string
		expSource string
		expName   string
		expDriver string
		expRW     bool
		fail      bool
	}{
		{`c:\:d:`, "local", mount.TypeBind, `d:`, `c:\`, ``, "", true, false},
		{`c:\:d:\`, "local", mount.TypeBind, `d:\`, `c:\`, ``, "", true, false},
		{`c:\:d:\:ro`, "local", mount.TypeBind, `d:\`, `c:\`, ``, "", false, false},
		{`c:\:d:\:rw`, "local", mount.TypeBind, `d:\`, `c:\`, ``, "", true, false},
		{`c:\:d:\:foo`, "local", mount.TypeBind, `d:\`, `c:\`, ``, "", false, true},
		{`name:d::rw`, "local", mount.TypeVolume, `d:`, ``, `name`, "local", true, false},
		{`name:d:`, "local", mount.TypeVolume, `d:`, ``, `name`, "local", true, false},
		{`name:d::ro`, "local", mount.TypeVolume, `d:`, ``, `name`, "local", false, false},
		{`name:c:`, "", mount.TypeVolume, ``, ``, ``, "", true, true},
		{`driver/name:c:`, "", mount.TypeVolume, ``, ``, ``, "", true, true},
		{`\\.\pipe\foo:\\.\pipe\bar`, "local", mount.TypeNamedPipe, `\\.\pipe\bar`, `\\.\pipe\foo`, "", "", true, false},
		{`\\.\pipe\foo:c:\foo\bar`, "local", mount.TypeNamedPipe, ``, ``, "", "", true, true},
		{`c:\foo\bar:\\.\pipe\foo`, "local", mount.TypeNamedPipe, ``, ``, "", "", true, true},
	}

	parser := NewWindowsParser()
	if p, ok := parser.(*windowsParser); ok {
		p.fi = mockFiProvider{}
	}

	for i, c := range cases {
		c := c
		t.Run(fmt.Sprintf("%d_%s", i, c.bind), func(t *testing.T) {
			m, err := parser.ParseMountRaw(c.bind, c.driver)
			if c.fail {
				assert.ErrorContains(t, err, "", "expected an error")
				return
			}

			assert.NilError(t, err)
			assert.Equal(t, m.Destination, c.expDest)
			assert.Equal(t, m.Source, c.expSource)
			assert.Equal(t, m.Name, c.expName)
			assert.Equal(t, m.Driver, c.expDriver)
			assert.Equal(t, m.RW, c.expRW)
			assert.Equal(t, m.Type, c.expType)
		})
	}
}

// TestWindowsParseMountSpecBindWithFileinfoError makes sure that the parser returns
// the error produced by the fileinfo provider.
//
// Some extra context for the future in case of changes and possible wtf are we
// testing this for:
//
// Currently this "fileInfoProvider" returns (bool, bool, error)
// The 1st bool is "does this path exist"
// The 2nd bool is "is this path a dir"
// Then of course the error is an error.
//
// The issue is the parser was ignoring the error and only looking at the
// "does this path exist" boolean, which is always false if there is an error.
// Then the error returned to the caller was a (slightly, maybe) friendlier
// error string than what comes from `os.Stat`
// So ...the caller was always getting an error saying the path doesn't exist
// even if it does exist but got some other error (like a permission error).
// This is confusing to users.
func TestWindowsParseMountSpecBindWithFileinfoError(t *testing.T) {
	parser := NewWindowsParser()
	testErr := fmt.Errorf("some crazy error")
	if pr, ok := parser.(*windowsParser); ok {
		pr.fi = &mockFiProviderWithError{err: testErr}
	}

	_, err := parser.ParseMountSpec(mount.Mount{
		Type:   mount.TypeBind,
		Source: `c:\bananas`,
		Target: `c:\bananas`,
	})
	assert.ErrorContains(t, err, testErr.Error())
}
