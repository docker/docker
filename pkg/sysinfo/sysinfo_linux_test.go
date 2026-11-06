package sysinfo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/containerd/containerd/v2/pkg/seccomp"
)

func TestReadProcBool(t *testing.T) {
	tmpDir := t.TempDir()

	procFile := filepath.Join(tmpDir, "read-proc-bool")
	if err := os.WriteFile(procFile, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !readProcBool(procFile) {
		t.Fatal("expected proc bool to be true, got false")
	}

	if err := os.WriteFile(procFile, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if readProcBool(procFile) {
		t.Fatal("expected proc bool to be false, got true")
	}

	if readProcBool(filepath.Join(tmpDir, "no-exist")) {
		t.Fatal("should be false for non-existent entry")
	}
}

func TestCgroupEnabled(t *testing.T) {
	cgroupDir := t.TempDir()

	if cgroupEnabled(cgroupDir, "test") {
		t.Fatal("cgroupEnabled should be false")
	}

	if err := os.WriteFile(filepath.Join(cgroupDir, "test"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	if !cgroupEnabled(cgroupDir, "test") {
		t.Fatal("cgroupEnabled should be true")
	}
}

// TestNew verifies that sysInfo is initialized with the expected values.
func TestNew(t *testing.T) {
	sysInfo := New()
	if sysInfo == nil {
		t.Fatal("sysInfo should not be nil")
	}
	if expected := seccomp.IsEnabled(); sysInfo.Seccomp != expected {
		t.Errorf("got Seccomp %v, wanted %v", sysInfo.Seccomp, expected)
	}
	if expected := apparmorSupported(); sysInfo.AppArmor != expected {
		t.Errorf("got AppArmor %v, wanted %v", sysInfo.AppArmor, expected)
	}
	if expected := cgroupnsSupported(); sysInfo.CgroupNamespaces != expected {
		t.Errorf("got CgroupNamespaces %v, wanted %v", sysInfo.CgroupNamespaces, expected)
	}
	if expected := timeNsSupported(); sysInfo.TimeNamespaces != expected {
		t.Errorf("got TimeNamespaces %v, wanted %v", sysInfo.TimeNamespaces, expected)
	}
}

func TestNewV2WithCgroup2Controllers(t *testing.T) {
	t.Run("delegated controllers", func(t *testing.T) {
		si := newV2(WithCgroup2Controllers("/delegated/docker.service", []string{"cpu", "memory", "pids"}))
		if !si.CPUCfs || !si.CPUShares {
			t.Fatal("expected CPU CFS and shares support when cpu is delegated")
		}
		if !si.MemoryLimit {
			t.Fatal("expected memory limit support when memory is delegated")
		}
		if !si.PidsLimit {
			t.Fatal("expected pids limit support when pids is delegated")
		}
		if si.BlkioWeight {
			t.Fatal("did not expect I/O support when io is not delegated")
		}
	})

	t.Run("explicitly empty", func(t *testing.T) {
		si := newV2(WithCgroup2Controllers("/delegated/docker.service", []string{}))
		if si.CPUCfs || si.MemoryLimit || si.PidsLimit {
			t.Fatal("did not expect resource support without delegated controllers")
		}
	})
}

func TestIsCpusetListAvailable(t *testing.T) {
	cases := []struct {
		provided  string
		available string
		res       bool
		err       bool
	}{
		{"1", "0-4", true, false},
		{"01,3", "0-4", true, false},
		{"", "0-7", true, false},
		{"1--42", "0-7", false, true},
		{"1-42", "00-1,8,9", false, true},
		{"1,41-42", "43,45", false, false},
		{"0-3", "", false, false},
	}
	for _, c := range cases {
		available, err := parseUintList(c.available, 0)
		if err != nil {
			t.Fatal(err)
		}
		r, err := isCpusetListAvailable(c.provided, available)
		if (c.err && err == nil) && r != c.res {
			t.Fatalf("Expected pair: %v, %v for %s, %s. Got %v, %v instead", c.res, c.err, c.provided, c.available, (c.err && err == nil), r)
		}
	}
}

func TestParseUintList(t *testing.T) {
	yes := struct{}{}
	valids := map[string]map[int]struct{}{
		"":             {},
		"7":            {7: yes},
		"1-6":          {1: yes, 2: yes, 3: yes, 4: yes, 5: yes, 6: yes},
		"0-7":          {0: yes, 1: yes, 2: yes, 3: yes, 4: yes, 5: yes, 6: yes, 7: yes},
		"0,3-4,7,8-10": {0: yes, 3: yes, 4: yes, 7: yes, 8: yes, 9: yes, 10: yes},
		"0-0,0,1-4":    {0: yes, 1: yes, 2: yes, 3: yes, 4: yes},
		"03,1-3":       {1: yes, 2: yes, 3: yes},
		"3,2,1":        {1: yes, 2: yes, 3: yes},
		"0-2,3,1":      {0: yes, 1: yes, 2: yes, 3: yes},
	}
	for k, v := range valids {
		out, err := parseUintList(k, 0)
		if err != nil {
			t.Fatalf("Expected not to fail, got %v", err)
		}
		if !reflect.DeepEqual(out, v) {
			t.Fatalf("Expected %v, got %v", v, out)
		}
	}

	invalids := []string{
		"this",
		"1--",
		"1-10,,10",
		"10-1",
		"-1",
		"-1,0",
	}
	for _, v := range invalids {
		if out, err := parseUintList(v, 0); err == nil {
			t.Fatalf("Expected failure with %s but got %v", v, out)
		}
	}
}

func TestParseUintListMaximumLimits(t *testing.T) {
	v := "10,1000"
	if _, err := parseUintList(v, 0); err != nil {
		t.Fatalf("Expected not to fail, got %v", err)
	}
	if _, err := parseUintList(v, 1000); err != nil {
		t.Fatalf("Expected not to fail, got %v", err)
	}
	if out, err := parseUintList(v, 100); err == nil {
		t.Fatalf("Expected failure with %s but got %v", v, out)
	}
}
