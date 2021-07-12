package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"gotest.tools/v3/assert"
)

func TestLinuxParseMountRaw(t *testing.T) {
	valid := []string{
		"/home",
		"/home:/home",
		"/home:/something/else",
		"/with space",
		"/home:/with space",
		"relative:/absolute-path",
		"hostPath:/containerPath:ro",
		"/hostPath:/containerPath:rw",
		"/rw:/ro",
		"/hostPath:/containerPath:shared",
		"/hostPath:/containerPath:rshared",
		"/hostPath:/containerPath:slave",
		"/hostPath:/containerPath:rslave",
		"/hostPath:/containerPath:private",
		"/hostPath:/containerPath:rprivate",
		"/hostPath:/containerPath:ro,shared",
		"/hostPath:/containerPath:ro,slave",
		"/hostPath:/containerPath:ro,private",
		"/hostPath:/containerPath:ro,z,shared",
		"/hostPath:/containerPath:ro,Z,slave",
		"/hostPath:/containerPath:Z,ro,slave",
		"/hostPath:/containerPath:slave,Z,ro",
		"/hostPath:/containerPath:Z,slave,ro",
		"/hostPath:/containerPath:slave,ro,Z",
		"/hostPath:/containerPath:rslave,ro,Z",
		"/hostPath:/containerPath:ro,rshared,Z",
		"/hostPath:/containerPath:ro,Z,rprivate",
	}

	invalid := map[string]string{
		"":                                "invalid volume specification",
		"./":                              "mount path must be absolute",
		"../":                             "mount path must be absolute",
		"/:../":                           "mount path must be absolute",
		"/:path":                          "mount path must be absolute",
		":":                               "invalid volume specification",
		"/tmp:":                           "invalid volume specification",
		":test":                           "invalid volume specification",
		":/test":                          "invalid volume specification",
		"tmp:":                            "invalid volume specification",
		":test:":                          "invalid volume specification",
		"::":                              "invalid volume specification",
		":::":                             "invalid volume specification",
		"/tmp:::":                         "invalid volume specification",
		":/tmp::":                         "invalid volume specification",
		"/path:rw":                        "invalid volume specification",
		"/path:ro":                        "invalid volume specification",
		"/rw:rw":                          "invalid volume specification",
		"path:ro":                         "invalid volume specification",
		"/path:/path:sw":                  `invalid mode`,
		"/path:/path:rwz":                 `invalid mode`,
		"/path:/path:ro,rshared,rslave":   `invalid mode`,
		"/path:/path:ro,z,rshared,rslave": `invalid mode`,
		"/path:shared":                    "invalid volume specification",
		"/path:slave":                     "invalid volume specification",
		"/path:private":                   "invalid volume specification",
		"name:/absolute-path:shared":      "invalid volume specification",
		"name:/absolute-path:rshared":     "invalid volume specification",
		"name:/absolute-path:slave":       "invalid volume specification",
		"name:/absolute-path:rslave":      "invalid volume specification",
		"name:/absolute-path:private":     "invalid volume specification",
		"name:/absolute-path:rprivate":    "invalid volume specification",
	}

	parser := NewLinuxParser()
	if p, ok := parser.(*linuxParser); ok {
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

func TestLinuxParseMountRawSplit(t *testing.T) {
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
		{"/tmp:/tmp1", "", mount.TypeBind, "/tmp1", "/tmp", "", "", true, false},
		{"/tmp:/tmp2:ro", "", mount.TypeBind, "/tmp2", "/tmp", "", "", false, false},
		{"/tmp:/tmp3:rw", "", mount.TypeBind, "/tmp3", "/tmp", "", "", true, false},
		{"/tmp:/tmp4:foo", "", mount.TypeBind, "", "", "", "", false, true},
		{"name:/named1", "", mount.TypeVolume, "/named1", "", "name", "", true, false},
		{"name:/named2", "external", mount.TypeVolume, "/named2", "", "name", "external", true, false},
		{"name:/named3:ro", "local", mount.TypeVolume, "/named3", "", "name", "local", false, false},
		{"local/name:/tmp:rw", "", mount.TypeVolume, "/tmp", "", "local/name", "", true, false},
		{"/tmp:tmp", "", mount.TypeBind, "", "", "", "", true, true},
	}

	parser := NewLinuxParser()
	if p, ok := parser.(*linuxParser); ok {
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

// TestLinuxParseMountSpecBindWithFileinfoError makes sure that the parser returns
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
func TestLinuxParseMountSpecBindWithFileinfoError(t *testing.T) {
	parser := NewLinuxParser()
	testErr := fmt.Errorf("some crazy error")
	if pr, ok := parser.(*linuxParser); ok {
		pr.fi = &mockFiProviderWithError{err: testErr}
	}

	_, err := parser.ParseMountSpec(mount.Mount{
		Type:   mount.TypeBind,
		Source: `/bananas`,
		Target: `/bananas`,
	})
	assert.ErrorContains(t, err, testErr.Error())
}

func TestConvertTmpfsOptions(t *testing.T) {
	type testCase struct {
		opt                  mount.TmpfsOptions
		readOnly             bool
		expectedSubstrings   []string
		unexpectedSubstrings []string
	}
	cases := []testCase{
		{
			opt:                  mount.TmpfsOptions{SizeBytes: 1024 * 1024, Mode: 0700},
			readOnly:             false,
			expectedSubstrings:   []string{"size=1m", "mode=700"},
			unexpectedSubstrings: []string{"ro"},
		},
		{
			opt:                  mount.TmpfsOptions{},
			readOnly:             true,
			expectedSubstrings:   []string{"ro"},
			unexpectedSubstrings: []string{},
		},
	}
	p := NewLinuxParser()
	for _, c := range cases {
		data, err := p.ConvertTmpfsOptions(&c.opt, c.readOnly)
		if err != nil {
			t.Fatalf("could not convert %+v (readOnly: %v) to string: %v",
				c.opt, c.readOnly, err)
		}
		t.Logf("data=%q", data)
		for _, s := range c.expectedSubstrings {
			if !strings.Contains(data, s) {
				t.Fatalf("expected substring: %s, got %v (case=%+v)", s, data, c)
			}
		}
		for _, s := range c.unexpectedSubstrings {
			if strings.Contains(data, s) {
				t.Fatalf("unexpected substring: %s, got %v (case=%+v)", s, data, c)
			}
		}
	}
}
