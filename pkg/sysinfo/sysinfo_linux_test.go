package sysinfo // import "github.com/docker/docker/pkg/sysinfo"

import (
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"gotest.tools/assert"
)

func TestReadProcBool(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "test-sysinfo-proc")
	assert.NilError(t, err)
	defer os.RemoveAll(tmpDir)

	procFile := filepath.Join(tmpDir, "read-proc-bool")
	err = ioutil.WriteFile(procFile, []byte("1"), 0644)
	assert.NilError(t, err)

	if !readProcBool(procFile) {
		t.Fatal("expected proc bool to be true, got false")
	}

	if err := ioutil.WriteFile(procFile, []byte("0"), 0644); err != nil {
		t.Fatal(err)
	}
	if readProcBool(procFile) {
		t.Fatal("expected proc bool to be false, got true")
	}

	if readProcBool(path.Join(tmpDir, "no-exist")) {
		t.Fatal("should be false for non-existent entry")
	}

}

func TestCgroupEnabled(t *testing.T) {
	cgroupDir, err := ioutil.TempDir("", "cgroup-test")
	assert.NilError(t, err)
	defer os.RemoveAll(cgroupDir)

	if cgroupEnabled(cgroupDir, "test") {
		t.Fatal("cgroupEnabled should be false")
	}

	err = ioutil.WriteFile(path.Join(cgroupDir, "test"), []byte{}, 0644)
	assert.NilError(t, err)

	if !cgroupEnabled(cgroupDir, "test") {
		t.Fatal("cgroupEnabled should be true")
	}
}

func TestNew(t *testing.T) {
	sysInfo := New(false)
	assert.Assert(t, sysInfo != nil)
	checkSysInfo(t, sysInfo)

	sysInfo = New(true)
	assert.Assert(t, sysInfo != nil)
	checkSysInfo(t, sysInfo)
}

func checkSysInfo(t *testing.T, sysInfo *SysInfo) {
	// Check if Seccomp is supported, via CONFIG_SECCOMP.then sysInfo.Seccomp must be TRUE , else FALSE
	if err := unix.Prctl(unix.PR_GET_SECCOMP, 0, 0, 0, 0); err != unix.EINVAL {
		// Make sure the kernel has CONFIG_SECCOMP_FILTER.
		if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, 0, 0, 0); err != unix.EINVAL {
			assert.Assert(t, sysInfo.Seccomp)
		}
	} else {
		assert.Assert(t, !sysInfo.Seccomp)
	}
}

func TestNewAppArmorEnabled(t *testing.T) {
	// Check if AppArmor is supported. then it must be TRUE , else FALSE
	if _, err := os.Stat("/sys/kernel/security/apparmor"); err != nil {
		t.Skip("App Armor Must be Enabled")
	}

	sysInfo := New(true)
	assert.Assert(t, sysInfo.AppArmor)
}

func TestNewAppArmorDisabled(t *testing.T) {
	// Check if AppArmor is supported. then it must be TRUE , else FALSE
	if _, err := os.Stat("/sys/kernel/security/apparmor"); !os.IsNotExist(err) {
		t.Skip("App Armor Must be Disabled")
	}

	sysInfo := New(true)
	assert.Assert(t, !sysInfo.AppArmor)
}

func TestNewCgroupNamespacesEnabled(t *testing.T) {
	// If cgroup namespaces are supported in the kernel, then sysInfo.CgroupNamespaces should be TRUE
	if _, err := os.Stat("/proc/self/ns/cgroup"); err != nil {
		t.Skip("cgroup namespaces must be enabled")
	}

	sysInfo := New(true)
	assert.Assert(t, sysInfo.CgroupNamespaces)
}

func TestNewCgroupNamespacesDisabled(t *testing.T) {
	// If cgroup namespaces are *not* supported in the kernel, then sysInfo.CgroupNamespaces should be FALSE
	if _, err := os.Stat("/proc/self/ns/cgroup"); !os.IsNotExist(err) {
		t.Skip("cgroup namespaces must be disabled")
	}

	sysInfo := New(true)
	assert.Assert(t, !sysInfo.CgroupNamespaces)
}

func TestNumCPU(t *testing.T) {
	cpuNumbers := NumCPU()
	if cpuNumbers <= 0 {
		t.Fatal("CPU returned must be greater than zero")
	}
}
