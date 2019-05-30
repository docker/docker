package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"errors"
	"io/ioutil"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"gotest.tools/assert"
	"gotest.tools/assert/cmp"
)

type parseMountRawTestSet struct {
	valid   []string
	invalid map[string]string
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
	p := &linuxParser{}
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

type mockFiProvider struct{}

func (mockFiProvider) fileInfo(path string) (exists, isDir bool, err error) {
	dirs := map[string]struct{}{
		`c:\`:                    {},
		`c:\windows\`:            {},
		`c:\windows`:             {},
		`c:\program files`:       {},
		`c:\Windows`:             {},
		`c:\Program Files (x86)`: {},
		`\\?\c:\windows\`:        {},
	}
	files := map[string]struct{}{
		`c:\windows\system32\ntdll.dll`: {},
	}
	if _, ok := dirs[path]; ok {
		return true, true, nil
	}
	if _, ok := files[path]; ok {
		return true, false, nil
	}
	return false, false, nil
}

func TestParseMountRaw(t *testing.T) {

	previousProvider := currentFileInfoProvider
	defer func() { currentFileInfoProvider = previousProvider }()
	currentFileInfoProvider = mockFiProvider{}
	windowsSet := parseMountRawTestSet{
		valid: []string{
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
		},
		invalid: map[string]string{
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
		},
	}
	lcowSet := parseMountRawTestSet{
		valid: []string{
			`/foo`,
			`/foo/`,
			`/foo bar`,
			`c:\:/foo`,
			`c:\windows\:/foo`,
			`c:\windows:/s p a c e`,
			`c:\windows:/s p a c e:RW`,
			`c:\program files:/s p a c e i n h o s t d i r`,
			`0123456789name:/foo`,
			`MiXeDcAsEnAmE:/foo`,
			`name:/foo`,
			`name:/foo:rW`,
			`name:/foo:RW`,
			`name:/foo:RO`,
			`c:/:/forward/slashes/are/good/too`,
			`c:/:/including with/spaces:ro`,
			`/Program Files (x86)`, // With capitals and brackets
		},
		invalid: map[string]string{
			``:                                   "invalid volume specification: ",
			`.`:                                  "invalid volume specification: ",
			`c:`:                                 "invalid volume specification: ",
			`c:\`:                                "invalid volume specification: ",
			`../`:                                "invalid volume specification: ",
			`c:\:../`:                            "invalid volume specification: ",
			`c:\:/foo:xyzzy`:                     "invalid volume specification: ",
			`/`:                                  "destination can't be '/'",
			`/..`:                                "destination can't be '/'",
			`c:\notexist:/foo`:                   `source path does not exist: c:\notexist`,
			`c:\windows\system32\ntdll.dll:/foo`: `source path must be a directory`,
			`name<:/foo`:                         `invalid volume specification`,
			`name>:/foo`:                         `invalid volume specification`,
			`name::/foo`:                         `invalid volume specification`,
			`name":/foo`:                         `invalid volume specification`,
			`name\:/foo`:                         `invalid volume specification`,
			`name*:/foo`:                         `invalid volume specification`,
			`name|:/foo`:                         `invalid volume specification`,
			`name?:/foo`:                         `invalid volume specification`,
			`name/:/foo`:                         `invalid volume specification`,
			`/foo:rw`:                            `invalid volume specification`,
			`/foo:ro`:                            `invalid volume specification`,
			`con:/foo`:                           `cannot be a reserved word for Windows filenames`,
			`PRN:/foo`:                           `cannot be a reserved word for Windows filenames`,
			`aUx:/foo`:                           `cannot be a reserved word for Windows filenames`,
			`nul:/foo`:                           `cannot be a reserved word for Windows filenames`,
			`com1:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`com2:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`com3:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`com4:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`com5:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`com6:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`com7:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`com8:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`com9:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`lpt1:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`lpt2:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`lpt3:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`lpt4:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`lpt5:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`lpt6:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`lpt7:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`lpt8:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`lpt9:/foo`:                          `cannot be a reserved word for Windows filenames`,
			`\\.\pipe\foo:/foo`:                  `Linux containers on Windows do not support named pipe mounts`,
		},
	}
	linuxSet := parseMountRawTestSet{
		valid: []string{
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
		},
		invalid: map[string]string{
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
		},
	}

	linParser := &linuxParser{}
	winParser := &windowsParser{}
	lcowParser := &lcowParser{}
	tester := func(parser Parser, set parseMountRawTestSet) {

		for _, path := range set.valid {

			if _, err := parser.ParseMountRaw(path, "local"); err != nil {
				t.Errorf("ParseMountRaw(`%q`) should succeed: error %q", path, err)
			}
		}

		for path, expectedError := range set.invalid {
			if mp, err := parser.ParseMountRaw(path, "local"); err == nil {
				t.Errorf("ParseMountRaw(`%q`) should have failed validation. Err '%v' - MP: %v", path, err, mp)
			} else {
				if !strings.Contains(err.Error(), expectedError) {
					t.Errorf("ParseMountRaw(`%q`) error should contain %q, got %v", path, expectedError, err.Error())
				}
			}
		}
	}
	tester(linParser, linuxSet)
	tester(winParser, windowsSet)
	tester(lcowParser, lcowSet)

}

// testParseMountRaw is a structure used by TestParseMountRawSplit for
// specifying test cases for the ParseMountRaw() function.
type testParseMountRaw struct {
	bind      string
	driver    string
	expType   mount.Type
	expDest   string
	expSource string
	expName   string
	expDriver string
	expRW     bool
	fail      bool
}

func TestParseMountRawSplit(t *testing.T) {
	previousProvider := currentFileInfoProvider
	defer func() { currentFileInfoProvider = previousProvider }()
	currentFileInfoProvider = mockFiProvider{}
	windowsCases := []testParseMountRaw{
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
	lcowCases := []testParseMountRaw{
		{`c:\:/foo`, "local", mount.TypeBind, `/foo`, `c:\`, ``, "", true, false},
		{`c:\:/foo:ro`, "local", mount.TypeBind, `/foo`, `c:\`, ``, "", false, false},
		{`c:\:/foo:rw`, "local", mount.TypeBind, `/foo`, `c:\`, ``, "", true, false},
		{`c:\:/foo:foo`, "local", mount.TypeBind, `/foo`, `c:\`, ``, "", false, true},
		{`name:/foo:rw`, "local", mount.TypeVolume, `/foo`, ``, `name`, "local", true, false},
		{`name:/foo`, "local", mount.TypeVolume, `/foo`, ``, `name`, "local", true, false},
		{`name:/foo:ro`, "local", mount.TypeVolume, `/foo`, ``, `name`, "local", false, false},
		{`name:/`, "", mount.TypeVolume, ``, ``, ``, "", true, true},
		{`driver/name:/`, "", mount.TypeVolume, ``, ``, ``, "", true, true},
		{`\\.\pipe\foo:\\.\pipe\bar`, "local", mount.TypeNamedPipe, `\\.\pipe\bar`, `\\.\pipe\foo`, "", "", true, true},
		{`\\.\pipe\foo:/data`, "local", mount.TypeNamedPipe, ``, ``, "", "", true, true},
		{`c:\foo\bar:\\.\pipe\foo`, "local", mount.TypeNamedPipe, ``, ``, "", "", true, true},
	}
	linuxCases := []testParseMountRaw{
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
	linParser := &linuxParser{}
	winParser := &windowsParser{}
	lcowParser := &lcowParser{}
	tester := func(parser Parser, cases []testParseMountRaw) {
		for i, c := range cases {
			t.Logf("case %d", i)
			m, err := parser.ParseMountRaw(c.bind, c.driver)
			if c.fail {
				if err == nil {
					t.Errorf("Expected error, was nil, for spec %s\n", c.bind)
				}
				continue
			}

			if m == nil || err != nil {
				t.Errorf("ParseMountRaw failed for spec '%s', driver '%s', error '%v'", c.bind, c.driver, err.Error())
				continue
			}

			if m.Destination != c.expDest {
				t.Errorf("Expected destination '%s, was %s', for spec '%s'", c.expDest, m.Destination, c.bind)
			}

			if m.Source != c.expSource {
				t.Errorf("Expected source '%s', was '%s', for spec '%s'", c.expSource, m.Source, c.bind)
			}

			if m.Name != c.expName {
				t.Errorf("Expected name '%s', was '%s' for spec '%s'", c.expName, m.Name, c.bind)
			}

			if m.Driver != c.expDriver {
				t.Errorf("Expected driver '%s', was '%s', for spec '%s'", c.expDriver, m.Driver, c.bind)
			}

			if m.RW != c.expRW {
				t.Errorf("Expected RW '%v', was '%v' for spec '%s'", c.expRW, m.RW, c.bind)
			}
			if m.Type != c.expType {
				t.Fatalf("Expected type '%s', was '%s', for spec '%s'", c.expType, m.Type, c.bind)
			}
		}
	}

	tester(linParser, linuxCases)
	tester(winParser, windowsCases)
	tester(lcowParser, lcowCases)
}

func TestParseMountSpec(t *testing.T) {
	type c struct {
		input    mount.Mount
		expected MountPoint
	}
	testDir, err := ioutil.TempDir("", "test-mount-config")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(testDir)
	parser := NewParser(runtime.GOOS)
	cases := []c{
		{mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath, ReadOnly: true}, MountPoint{Type: mount.TypeBind, Source: testDir, Destination: testDestinationPath, Propagation: parser.DefaultPropagationMode()}},
		{mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath}, MountPoint{Type: mount.TypeBind, Source: testDir, Destination: testDestinationPath, RW: true, Propagation: parser.DefaultPropagationMode()}},
		{mount.Mount{Type: mount.TypeBind, Source: testDir + string(os.PathSeparator), Target: testDestinationPath, ReadOnly: true}, MountPoint{Type: mount.TypeBind, Source: testDir, Destination: testDestinationPath, Propagation: parser.DefaultPropagationMode()}},
		{mount.Mount{Type: mount.TypeBind, Source: testDir, Target: testDestinationPath + string(os.PathSeparator), ReadOnly: true}, MountPoint{Type: mount.TypeBind, Source: testDir, Destination: testDestinationPath, Propagation: parser.DefaultPropagationMode()}},
		{mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath}, MountPoint{Type: mount.TypeVolume, Destination: testDestinationPath, RW: true, CopyData: parser.DefaultCopyMode()}},
		{mount.Mount{Type: mount.TypeVolume, Target: testDestinationPath + string(os.PathSeparator)}, MountPoint{Type: mount.TypeVolume, Destination: testDestinationPath, RW: true, CopyData: parser.DefaultCopyMode()}},
	}

	for i, c := range cases {
		t.Logf("case %d", i)
		mp, err := parser.ParseMountSpec(c.input)
		if err != nil {
			t.Error(err)
		}

		if c.expected.Type != mp.Type {
			t.Errorf("Expected mount types to match. Expected: '%s', Actual: '%s'", c.expected.Type, mp.Type)
		}
		if c.expected.Destination != mp.Destination {
			t.Errorf("Expected mount destination to match. Expected: '%s', Actual: '%s'", c.expected.Destination, mp.Destination)
		}
		if c.expected.Source != mp.Source {
			t.Errorf("Expected mount source to match. Expected: '%s', Actual: '%s'", c.expected.Source, mp.Source)
		}
		if c.expected.RW != mp.RW {
			t.Errorf("Expected mount writable to match. Expected: '%v', Actual: '%v'", c.expected.RW, mp.RW)
		}
		if c.expected.Propagation != mp.Propagation {
			t.Errorf("Expected mount propagation to match. Expected: '%v', Actual: '%s'", c.expected.Propagation, mp.Propagation)
		}
		if c.expected.Driver != mp.Driver {
			t.Errorf("Expected mount driver to match. Expected: '%v', Actual: '%s'", c.expected.Driver, mp.Driver)
		}
		if c.expected.CopyData != mp.CopyData {
			t.Errorf("Expected mount copy data to match. Expected: '%v', Actual: '%v'", c.expected.CopyData, mp.CopyData)
		}
	}

}

// always returns the configured error
// this is used to test error handling
type mockFiProviderWithError struct{ err error }

func (m mockFiProviderWithError) fileInfo(path string) (bool, bool, error) {
	return false, false, m.err
}

// TestParseMountSpecBindWithFileinfoError makes sure that the parser returns
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
func TestParseMountSpecBindWithFileinfoError(t *testing.T) {
	previousProvider := currentFileInfoProvider
	defer func() { currentFileInfoProvider = previousProvider }()

	testErr := errors.New("some crazy error")
	currentFileInfoProvider = &mockFiProviderWithError{err: testErr}

	p := "/bananas"
	if runtime.GOOS == "windows" {
		p = `c:\bananas`
	}
	m := mount.Mount{Type: mount.TypeBind, Source: p, Target: p}

	parser := NewParser(runtime.GOOS)

	_, err := parser.ParseMountSpec(m)
	assert.Assert(t, err != nil)
	assert.Assert(t, cmp.Contains(err.Error(), "some crazy error"))
}
