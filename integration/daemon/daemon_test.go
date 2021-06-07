package daemon // import "github.com/docker/docker/integration/daemon"

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestConfigDaemonLibtrustID(t *testing.T) {
	skip.If(t, runtime.GOOS != "linux")

	d := daemon.New(t)
	defer d.Stop(t)

	trustKey := filepath.Join(d.RootDir(), "key.json")
	err := ioutil.WriteFile(trustKey, []byte(`{"crv":"P-256","d":"dm28PH4Z4EbyUN8L0bPonAciAQa1QJmmyYd876mnypY","kid":"WTJ3:YSIP:CE2E:G6KJ:PSBD:YX2Y:WEYD:M64G:NU2V:XPZV:H2CR:VLUB","kty":"EC","x":"Mh5-JINSjaa_EZdXDttri255Z5fbCEOTQIZjAcScFTk","y":"eUyuAjfxevb07hCCpvi4Zi334Dy4GDWQvEToGEX4exQ"}`), 0644)
	assert.NilError(t, err)

	config := filepath.Join(d.RootDir(), "daemon.json")
	err = ioutil.WriteFile(config, []byte(`{"deprecated-key-path": "`+trustKey+`"}`), 0644)
	assert.NilError(t, err)

	d.Start(t, "--config-file", config)
	info := d.Info(t)
	assert.Equal(t, info.ID, "WTJ3:YSIP:CE2E:G6KJ:PSBD:YX2Y:WEYD:M64G:NU2V:XPZV:H2CR:VLUB")
}

func TestDaemonConfigValidation(t *testing.T) {
	skip.If(t, runtime.GOOS != "linux")

	d := daemon.New(t)
	dockerBinary, err := d.BinaryPath()
	assert.NilError(t, err)
	params := []string{"--validate", "--config-file"}

	dest := os.Getenv("DOCKER_INTEGRATION_DAEMON_DEST")
	if dest == "" {
		dest = os.Getenv("DEST")
	}
	testdata := filepath.Join(dest, "..", "..", "integration", "daemon", "testdata")

	const (
		validOut  = "configuration OK"
		failedOut = "unable to configure the Docker daemon with file"
	)

	tests := []struct {
		name        string
		args        []string
		expectedOut string
	}{
		{
			name:        "config with no content",
			args:        append(params, filepath.Join(testdata, "empty-config-1.json")),
			expectedOut: validOut,
		},
		{
			name:        "config with {}",
			args:        append(params, filepath.Join(testdata, "empty-config-2.json")),
			expectedOut: validOut,
		},
		{
			name:        "invalid config",
			args:        append(params, filepath.Join(testdata, "invalid-config-1.json")),
			expectedOut: failedOut,
		},
		{
			name:        "malformed config",
			args:        append(params, filepath.Join(testdata, "malformed-config.json")),
			expectedOut: failedOut,
		},
		{
			name:        "valid config",
			args:        append(params, filepath.Join(testdata, "valid-config-1.json")),
			expectedOut: validOut,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(dockerBinary, tc.args...)
			out, err := cmd.CombinedOutput()
			assert.Check(t, is.Contains(string(out), tc.expectedOut))
			if tc.expectedOut == failedOut {
				assert.ErrorContains(t, err, "", "expected an error, but got none")
			} else {
				assert.NilError(t, err)
			}
		})
	}
}

func TestConfigDaemonSeccompProfiles(t *testing.T) {
	skip.If(t, runtime.GOOS != "linux")

	d := daemon.New(t)
	defer d.Stop(t)

	tests := []struct {
		doc             string
		profile         string
		expectedProfile string
	}{
		{
			doc:             "empty profile set",
			profile:         "",
			expectedProfile: "default",
		},
		{
			doc:             "unconfined profile",
			profile:         config.SeccompProfileUnconfined,
			expectedProfile: "unconfined",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.doc, func(t *testing.T) {
			d.Start(t, "--seccomp-profile", tc.profile)
			info = d.Info(t)
			assert.Assert(t, is.Contains(info.SecurityOptions, "name=seccomp,profile="+tc.expectedProfile))
			d.Stop(t)

			cfg := filepath.Join(d.RootDir(), "daemon.json")
			err := ioutil.WriteFile(cfg, []byte(`{"seccomp-profile": "`+tc.profile+`"}`), 0644)
			assert.NilError(t, err)

			d.Start(t, "--config-file", cfg)
			info := d.Info(t)
			assert.Assert(t, is.Contains(info.SecurityOptions, "name=seccomp,profile="+tc.expectedProfile))
			d.Stop(t)
		})
	}
}
