// +build linux

package seccomp // import "github.com/docker/docker/profiles/seccomp"

import (
	"encoding/json"
	"io/ioutil"
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"gotest.tools/v3/assert"
)

func TestLoadProfile(t *testing.T) {
	f, err := ioutil.ReadFile("fixtures/example.json")
	if err != nil {
		t.Fatal(err)
	}
	rs := createSpec()
	p, err := LoadProfile(string(f), &rs)
	if err != nil {
		t.Fatal(err)
	}
	var expectedErrno uint = 12345
	expected := specs.LinuxSeccomp{
		DefaultAction: "SCMP_ACT_ERRNO",
		Syscalls: []specs.LinuxSyscall{
			{
				Names:  []string{"clone"},
				Action: "SCMP_ACT_ALLOW",
				Args: []specs.LinuxSeccompArg{{
					Index:    0,
					Value:    2114060288,
					ValueTwo: 0,
					Op:       "SCMP_CMP_MASKED_EQ",
				}},
			},
			{

				Names:  []string{"open"},
				Action: "SCMP_ACT_ALLOW",
				Args:   []specs.LinuxSeccompArg{},
			},
			{
				Names:  []string{"close"},
				Action: "SCMP_ACT_ALLOW",
				Args:   []specs.LinuxSeccompArg{},
			},
			{
				Names:    []string{"syslog"},
				Action:   "SCMP_ACT_ERRNO",
				ErrnoRet: &expectedErrno,
				Args:     []specs.LinuxSeccompArg{},
			},
		},
	}

	assert.DeepEqual(t, expected, *p)
}

func TestLoadProfileWithDefaultErrnoRet(t *testing.T) {
	var profile = []byte(`{
"defaultAction": "SCMP_ACT_ERRNO",
"defaultErrnoRet": 6
}`)
	rs := createSpec()
	p, err := LoadProfile(string(profile), &rs)
	if err != nil {
		t.Fatal(err)
	}

	expectedErrnoRet := uint(6)
	expected := specs.LinuxSeccomp{
		DefaultAction:   "SCMP_ACT_ERRNO",
		DefaultErrnoRet: &expectedErrnoRet,
	}

	assert.DeepEqual(t, expected, *p)
}

func TestLoadProfileWithListenerPath(t *testing.T) {
	var profile = []byte(`{
"defaultAction": "SCMP_ACT_ERRNO",
"listenerPath": "/var/run/seccompaget.sock",
"listenerMetadata": "opaque-metadata"
}`)
	rs := createSpec()
	p, err := LoadProfile(string(profile), &rs)
	if err != nil {
		t.Fatal(err)
	}

	expected := specs.LinuxSeccomp{
		DefaultAction:    "SCMP_ACT_ERRNO",
		ListenerPath:     "/var/run/seccompaget.sock",
		ListenerMetadata: "opaque-metadata",
	}

	assert.DeepEqual(t, expected, *p)
}

// TestLoadLegacyProfile tests loading a seccomp profile in the old format
// (before https://github.com/docker/docker/pull/24510)
func TestLoadLegacyProfile(t *testing.T) {
	f, err := ioutil.ReadFile("fixtures/default-old-format.json")
	if err != nil {
		t.Fatal(err)
	}
	rs := createSpec()
	if _, err := LoadProfile(string(f), &rs); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDefaultProfile(t *testing.T) {
	f, err := ioutil.ReadFile("default.json")
	if err != nil {
		t.Fatal(err)
	}
	rs := createSpec()
	if _, err := LoadProfile(string(f), &rs); err != nil {
		t.Fatal(err)
	}
}

func TestUnmarshalDefaultProfile(t *testing.T) {
	expected := DefaultProfile()
	if expected == nil {
		t.Skip("seccomp not supported")
	}

	f, err := ioutil.ReadFile("default.json")
	if err != nil {
		t.Fatal(err)
	}
	var profile Seccomp
	err = json.Unmarshal(f, &profile)
	if err != nil {
		t.Fatal(err)
	}
	assert.DeepEqual(t, expected.Architectures, profile.Architectures)
	assert.DeepEqual(t, expected.ArchMap, profile.ArchMap)
	assert.DeepEqual(t, expected.DefaultAction, profile.DefaultAction)
	assert.DeepEqual(t, expected.Syscalls, profile.Syscalls)
}

func TestMarshalUnmarshalFilter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in    string
		out   string
		error bool
	}{
		{in: `{"arches":["s390x"],"minKernel":3}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":3.12}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":true}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":"0.0"}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":"3"}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":".3"}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":"3."}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":"true"}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":"3.12.1\""}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":"4.15abc"}`, error: true},
		{in: `{"arches":["s390x"],"minKernel":null}`, out: `{"arches":["s390x"]}`},
		{in: `{"arches":["s390x"],"minKernel":""}`, out: `{"arches":["s390x"],"minKernel":""}`}, // FIXME: try to fix omitempty for this
		{in: `{"arches":["s390x"],"minKernel":"0.5"}`, out: `{"arches":["s390x"],"minKernel":"0.5"}`},
		{in: `{"arches":["s390x"],"minKernel":"0.50"}`, out: `{"arches":["s390x"],"minKernel":"0.50"}`},
		{in: `{"arches":["s390x"],"minKernel":"5.0"}`, out: `{"arches":["s390x"],"minKernel":"5.0"}`},
		{in: `{"arches":["s390x"],"minKernel":"50.0"}`, out: `{"arches":["s390x"],"minKernel":"50.0"}`},
		{in: `{"arches":["s390x"],"minKernel":"4.15"}`, out: `{"arches":["s390x"],"minKernel":"4.15"}`},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			var filter Filter
			err := json.Unmarshal([]byte(tc.in), &filter)
			if tc.error {
				if err == nil {
					t.Fatal("expected an error")
				} else if !strings.Contains(err.Error(), "invalid kernel version") {
					t.Fatal("unexpected error:", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(filter)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.out {
				t.Fatalf("expected %s, got %s", tc.out, string(out))
			}
		})
	}
}

func TestLoadConditional(t *testing.T) {
	f, err := ioutil.ReadFile("fixtures/conditional_include.json")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		doc      string
		cap      string
		expected []string
	}{
		{doc: "no caps", expected: []string{"chmod", "ptrace"}},
		{doc: "with syslog", cap: "CAP_SYSLOG", expected: []string{"chmod", "syslog", "ptrace"}},
		{doc: "no ptrace", cap: "CAP_SYS_ADMIN", expected: []string{"chmod"}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.doc, func(t *testing.T) {
			rs := createSpec(tc.cap)
			p, err := LoadProfile(string(f), &rs)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Syscalls) != len(tc.expected) {
				t.Fatalf("expected %d syscalls in profile, have %d", len(tc.expected), len(p.Syscalls))
			}
			for i, v := range p.Syscalls {
				if v.Names[0] != tc.expected[i] {
					t.Fatalf("expected %s syscall, have %s", tc.expected[i], v.Names[0])
				}
			}
		})
	}
}

// createSpec() creates a minimum spec for testing
func createSpec(caps ...string) specs.Spec {
	rs := specs.Spec{
		Process: &specs.Process{
			Capabilities: &specs.LinuxCapabilities{},
		},
	}
	if caps != nil {
		rs.Process.Capabilities.Bounding = append(rs.Process.Capabilities.Bounding, caps...)
	}
	return rs
}
