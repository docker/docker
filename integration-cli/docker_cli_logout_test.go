package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-check/check"
	"gotest.tools/assert"
)

func (s *DockerRegistryAuthHtpasswdSuite) TestLogoutWithExternalAuth(c *check.C) {
	s.d.StartWithBusybox(c)

	osPath := os.Getenv("PATH")
	defer os.Setenv("PATH", osPath)

	workingDir, err := os.Getwd()
	assert.NilError(c, err)
	absolute, err := filepath.Abs(filepath.Join(workingDir, "fixtures", "auth"))
	assert.NilError(c, err)
	testPath := fmt.Sprintf("%s%c%s", osPath, filepath.ListSeparator, absolute)

	os.Setenv("PATH", testPath)

	repoName := fmt.Sprintf("%v/dockercli/busybox:authtest", privateRegistryURL)

	tmp, err := ioutil.TempDir("", "integration-cli-")
	assert.NilError(c, err)
	defer os.RemoveAll(tmp)

	externalAuthConfig := `{ "credsStore": "shell-test" }`

	configPath := filepath.Join(tmp, "config.json")
	err = ioutil.WriteFile(configPath, []byte(externalAuthConfig), 0644)
	assert.NilError(c, err)

	_, err = s.d.Cmd("--config", tmp, "login", "-u", s.reg.Username(), "-p", s.reg.Password(), privateRegistryURL)
	assert.NilError(c, err)

	b, err := ioutil.ReadFile(configPath)
	assert.NilError(c, err)
	assert.Assert(c, !strings.Contains(string(b), `"auth":`))
	assert.Assert(c, strings.Contains(string(b), privateRegistryURL))

	_, err = s.d.Cmd("--config", tmp, "tag", "busybox", repoName)
	assert.NilError(c, err)
	_, err = s.d.Cmd("--config", tmp, "push", repoName)
	assert.NilError(c, err)
	_, err = s.d.Cmd("--config", tmp, "logout", privateRegistryURL)
	assert.NilError(c, err)

	b, err = ioutil.ReadFile(configPath)
	assert.NilError(c, err)
	assert.Assert(c, !strings.Contains(string(b), privateRegistryURL))

	// check I cannot pull anymore
	out, err := s.d.Cmd("--config", tmp, "pull", repoName)
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "no basic auth credentials"))
}

// #23100
func (s *DockerRegistryAuthHtpasswdSuite) TestLogoutWithWrongHostnamesStored(c *check.C) {
	osPath := os.Getenv("PATH")
	defer os.Setenv("PATH", osPath)

	workingDir, err := os.Getwd()
	assert.NilError(c, err)
	absolute, err := filepath.Abs(filepath.Join(workingDir, "fixtures", "auth"))
	assert.NilError(c, err)
	testPath := fmt.Sprintf("%s%c%s", osPath, filepath.ListSeparator, absolute)

	os.Setenv("PATH", testPath)

	cmd := exec.Command("docker-credential-shell-test", "store")
	stdin := bytes.NewReader([]byte(fmt.Sprintf(`{"ServerURL": "https://%s", "Username": "%s", "Secret": "%s"}`, privateRegistryURL, s.reg.Username(), s.reg.Password())))
	cmd.Stdin = stdin
	assert.NilError(c, cmd.Run())

	tmp, err := ioutil.TempDir("", "integration-cli-")
	assert.NilError(c, err)

	externalAuthConfig := fmt.Sprintf(`{ "auths": {"https://%s": {}}, "credsStore": "shell-test" }`, privateRegistryURL)

	configPath := filepath.Join(tmp, "config.json")
	err = ioutil.WriteFile(configPath, []byte(externalAuthConfig), 0644)
	assert.NilError(c, err)

	dockerCmd(c, "--config", tmp, "login", "-u", s.reg.Username(), "-p", s.reg.Password(), privateRegistryURL)

	b, err := ioutil.ReadFile(configPath)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(b), fmt.Sprintf(`"https://%s": {}`, privateRegistryURL)))
	assert.Assert(c, strings.Contains(string(b), fmt.Sprintf(`"%s": {}`, privateRegistryURL)))

	dockerCmd(c, "--config", tmp, "logout", privateRegistryURL)

	b, err = ioutil.ReadFile(configPath)
	assert.NilError(c, err)
	assert.Assert(c, !strings.Contains(string(b), fmt.Sprintf(`"https://%s": {}`, privateRegistryURL)))
	assert.Assert(c, !strings.Contains(string(b), fmt.Sprintf(`"%s": {}`, privateRegistryURL)))
}
