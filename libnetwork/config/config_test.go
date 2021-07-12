package config

import (
	"strings"
	"testing"

	"github.com/docker/docker/libnetwork/netlabel"
	_ "github.com/docker/docker/libnetwork/testutils"
)

func TestInvalidConfig(t *testing.T) {
	_, err := ParseConfig("invalid.toml")
	if err == nil {
		t.Fatal("Invalid Configuration file must fail")
	}
}

func TestConfig(t *testing.T) {
	_, err := ParseConfig("libnetwork.toml")
	if err != nil {
		t.Fatal("Error parsing a valid configuration file :", err)
	}
}

func TestOptionsLabels(t *testing.T) {
	c := &Config{}
	l := []string{
		"com.docker.network.key1=value1",
		"com.docker.storage.key1=value1",
		"com.docker.network.driver.key1=value1",
		"com.docker.network.driver.key2=value2",
	}
	f := OptionLabels(l)
	f(c)
	if len(c.Daemon.Labels) != 3 {
		t.Fatalf("Expecting 3 labels, seen %d", len(c.Daemon.Labels))
	}
	for _, l := range c.Daemon.Labels {
		if !strings.HasPrefix(l, netlabel.Prefix) {
			t.Fatalf("config must accept only libnetwork labels. Not : %s", l)
		}
	}
}

func TestValidName(t *testing.T) {
	if !IsValidName("test") {
		t.Fatal("Name validation fails for a name that must be accepted")
	}
	if IsValidName("") {
		t.Fatal("Name validation succeeds for a case when it is expected to fail")
	}
	if IsValidName("   ") {
		t.Fatal("Name validation succeeds for a case when it is expected to fail")
	}
}
