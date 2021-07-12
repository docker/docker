package container // import "github.com/docker/docker/container"

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types/container"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/moby/sys/signal"
	"gotest.tools/v3/assert"
)

func TestContainerStopSignal(t *testing.T) {
	c := &Container{
		Config: &container.Config{},
	}

	def, err := signal.ParseSignal(signal.DefaultStopSignal)
	if err != nil {
		t.Fatal(err)
	}

	s := c.StopSignal()
	if s != int(def) {
		t.Fatalf("Expected %v, got %v", def, s)
	}

	c = &Container{
		Config: &container.Config{StopSignal: "SIGKILL"},
	}
	s = c.StopSignal()
	if s != 9 {
		t.Fatalf("Expected 9, got %v", s)
	}
}

func TestContainerStopTimeout(t *testing.T) {
	c := &Container{
		Config: &container.Config{},
	}

	s := c.StopTimeout()
	if s != DefaultStopTimeout {
		t.Fatalf("Expected %v, got %v", DefaultStopTimeout, s)
	}

	stopTimeout := 15
	c = &Container{
		Config: &container.Config{StopTimeout: &stopTimeout},
	}
	s = c.StopTimeout()
	if s != stopTimeout {
		t.Fatalf("Expected %v, got %v", stopTimeout, s)
	}
}

func TestContainerSecretReferenceDestTarget(t *testing.T) {
	ref := &swarmtypes.SecretReference{
		File: &swarmtypes.SecretReferenceFileTarget{
			Name: "app",
		},
	}

	d := getSecretTargetPath(ref)
	expected := filepath.Join(containerSecretMountPath, "app")
	if d != expected {
		t.Fatalf("expected secret dest %q; received %q", expected, d)
	}
}

func TestContainerLogPathSetForJSONFileLogger(t *testing.T) {
	containerRoot, err := ioutil.TempDir("", "TestContainerLogPathSetForJSONFileLogger")
	assert.NilError(t, err)
	defer os.RemoveAll(containerRoot)

	c := &Container{
		Config: &container.Config{},
		HostConfig: &container.HostConfig{
			LogConfig: container.LogConfig{
				Type: jsonfilelog.Name,
			},
		},
		ID:   "TestContainerLogPathSetForJSONFileLogger",
		Root: containerRoot,
	}

	logger, err := c.StartLogger()
	assert.NilError(t, err)
	defer logger.Close()

	expectedLogPath, err := filepath.Abs(filepath.Join(containerRoot, fmt.Sprintf("%s-json.log", c.ID)))
	assert.NilError(t, err)
	assert.Equal(t, c.LogPath, expectedLogPath)
}

func TestContainerLogPathSetForRingLogger(t *testing.T) {
	containerRoot, err := ioutil.TempDir("", "TestContainerLogPathSetForRingLogger")
	assert.NilError(t, err)
	defer os.RemoveAll(containerRoot)

	c := &Container{
		Config: &container.Config{},
		HostConfig: &container.HostConfig{
			LogConfig: container.LogConfig{
				Type: jsonfilelog.Name,
				Config: map[string]string{
					"mode": string(container.LogModeNonBlock),
				},
			},
		},
		ID:   "TestContainerLogPathSetForRingLogger",
		Root: containerRoot,
	}

	logger, err := c.StartLogger()
	assert.NilError(t, err)
	defer logger.Close()

	expectedLogPath, err := filepath.Abs(filepath.Join(containerRoot, fmt.Sprintf("%s-json.log", c.ID)))
	assert.NilError(t, err)
	assert.Equal(t, c.LogPath, expectedLogPath)
}
