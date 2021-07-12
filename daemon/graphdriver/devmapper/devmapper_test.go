// +build linux

package devmapper // import "github.com/docker/docker/daemon/graphdriver/devmapper"

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/daemon/graphdriver/graphtest"
	"github.com/docker/docker/pkg/parsers/kernel"
	"golang.org/x/sys/unix"
)

func init() {
	// Reduce the size of the base fs and loopback for the tests
	defaultDataLoopbackSize = 300 * 1024 * 1024
	defaultMetaDataLoopbackSize = 200 * 1024 * 1024
	defaultBaseFsSize = 300 * 1024 * 1024
	defaultUdevSyncOverride = true
	if err := initLoopbacks(); err != nil {
		panic(err)
	}
}

// initLoopbacks ensures that the loopback devices are properly created within
// the system running the device mapper tests.
func initLoopbacks() error {
	statT, err := getBaseLoopStats()
	if err != nil {
		return err
	}
	// create at least 128 loopback files, since a few first ones
	// might be already in use by the host OS
	for i := 0; i < 128; i++ {
		loopPath := fmt.Sprintf("/dev/loop%d", i)
		// only create new loopback files if they don't exist
		if _, err := os.Stat(loopPath); err != nil {
			if mkerr := syscall.Mknod(loopPath,
				uint32(statT.Mode|syscall.S_IFBLK), int((7<<8)|(i&0xff)|((i&0xfff00)<<12))); mkerr != nil { //nolint: unconvert
				return mkerr
			}
			os.Chown(loopPath, int(statT.Uid), int(statT.Gid))
		}
	}
	return nil
}

// getBaseLoopStats inspects /dev/loop0 to collect uid,gid, and mode for the
// loop0 device on the system.  If it does not exist we assume 0,0,0660 for the
// stat data
func getBaseLoopStats() (*syscall.Stat_t, error) {
	loop0, err := os.Stat("/dev/loop0")
	if err != nil {
		if os.IsNotExist(err) {
			return &syscall.Stat_t{
				Uid:  0,
				Gid:  0,
				Mode: 0660,
			}, nil
		}
		return nil, err
	}
	return loop0.Sys().(*syscall.Stat_t), nil
}

// This avoids creating a new driver for each test if all tests are run
// Make sure to put new tests between TestDevmapperSetup and TestDevmapperTeardown
func TestDevmapperSetup(t *testing.T) {
	graphtest.GetDriver(t, "devicemapper")
}

func TestDevmapperCreateEmpty(t *testing.T) {
	graphtest.DriverTestCreateEmpty(t, "devicemapper")
}

func TestDevmapperCreateBase(t *testing.T) {
	graphtest.DriverTestCreateBase(t, "devicemapper")
}

func TestDevmapperCreateSnap(t *testing.T) {
	graphtest.DriverTestCreateSnap(t, "devicemapper")
}

func TestDevmapperTeardown(t *testing.T) {
	graphtest.PutDriver(t)
}

func TestDevmapperReduceLoopBackSize(t *testing.T) {
	tenMB := int64(10 * 1024 * 1024)
	testChangeLoopBackSize(t, -tenMB, defaultDataLoopbackSize, defaultMetaDataLoopbackSize)
}

func TestDevmapperIncreaseLoopBackSize(t *testing.T) {
	tenMB := int64(10 * 1024 * 1024)
	testChangeLoopBackSize(t, tenMB, defaultDataLoopbackSize+tenMB, defaultMetaDataLoopbackSize+tenMB)
}

func testChangeLoopBackSize(t *testing.T, delta, expectDataSize, expectMetaDataSize int64) {
	driver := graphtest.GetDriver(t, "devicemapper").(*graphtest.Driver).Driver.(*graphdriver.NaiveDiffDriver).ProtoDriver.(*Driver)
	defer graphtest.PutDriver(t)
	// make sure data or metadata loopback size are the default size
	if s := driver.DeviceSet.Status(); s.Data.Total != uint64(defaultDataLoopbackSize) || s.Metadata.Total != uint64(defaultMetaDataLoopbackSize) {
		t.Fatal("data or metadata loop back size is incorrect")
	}
	if err := driver.Cleanup(); err != nil {
		t.Fatal(err)
	}
	// Reload
	d, err := Init(driver.home, []string{
		fmt.Sprintf("dm.loopdatasize=%d", defaultDataLoopbackSize+delta),
		fmt.Sprintf("dm.loopmetadatasize=%d", defaultMetaDataLoopbackSize+delta),
	}, nil, nil)
	if err != nil {
		t.Fatalf("error creating devicemapper driver: %v", err)
	}
	driver = d.(*graphdriver.NaiveDiffDriver).ProtoDriver.(*Driver)
	if s := driver.DeviceSet.Status(); s.Data.Total != uint64(expectDataSize) || s.Metadata.Total != uint64(expectMetaDataSize) {
		t.Fatal("data or metadata loop back size is incorrect")
	}
	if err := driver.Cleanup(); err != nil {
		t.Fatal(err)
	}
}

// Make sure devices.Lock() has been release upon return from cleanupDeletedDevices() function
func TestDevmapperLockReleasedDeviceDeletion(t *testing.T) {
	driver := graphtest.GetDriver(t, "devicemapper").(*graphtest.Driver).Driver.(*graphdriver.NaiveDiffDriver).ProtoDriver.(*Driver)
	defer graphtest.PutDriver(t)

	// Call cleanupDeletedDevices() and after the call take and release
	// DeviceSet Lock. If lock has not been released, this will hang.
	driver.DeviceSet.cleanupDeletedDevices()

	doneChan := make(chan bool, 1)

	go func() {
		driver.DeviceSet.Lock()
		defer driver.DeviceSet.Unlock()
		doneChan <- true
	}()

	select {
	case <-time.After(time.Second * 5):
		// Timer expired. That means lock was not released upon
		// function return and we are deadlocked. Release lock
		// here so that cleanup could succeed and fail the test.
		driver.DeviceSet.Unlock()
		t.Fatal("Could not acquire devices lock after call to cleanupDeletedDevices()")
	case <-doneChan:
	}
}

// Ensure that mounts aren't leakedriver. It's non-trivial for us to test the full
// reproducer of #34573 in a unit test, but we can at least make sure that a
// simple command run in a new namespace doesn't break things horribly.
func TestDevmapperMountLeaks(t *testing.T) {
	if !kernel.CheckKernelVersion(3, 18, 0) {
		t.Skipf("kernel version <3.18.0 and so is missing torvalds/linux@8ed936b5671bfb33d89bc60bdcc7cf0470ba52fe.")
	}

	driver := graphtest.GetDriver(t, "devicemapper", "dm.use_deferred_removal=false", "dm.use_deferred_deletion=false").(*graphtest.Driver).Driver.(*graphdriver.NaiveDiffDriver).ProtoDriver.(*Driver)
	defer graphtest.PutDriver(t)

	// We need to create a new (dummy) device.
	if err := driver.Create("some-layer", "", nil); err != nil {
		t.Fatalf("setting up some-layer: %v", err)
	}

	// Mount the device.
	_, err := driver.Get("some-layer", "")
	if err != nil {
		t.Fatalf("mounting some-layer: %v", err)
	}

	// Create a new subprocess which will inherit our mountpoint, then
	// intentionally leak it and stick around. We can't do this entirely within
	// Go because forking and namespaces in Go are really not handled well at
	// all.
	cmd := exec.Cmd{
		Path: "/bin/sh",
		Args: []string{
			"/bin/sh", "-c",
			"mount --make-rprivate / && sleep 1000s",
		},
		SysProcAttr: &syscall.SysProcAttr{
			Unshareflags: syscall.CLONE_NEWNS,
		},
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sub-command: %v", err)
	}
	defer func() {
		unix.Kill(cmd.Process.Pid, unix.SIGKILL)
		cmd.Wait()
	}()

	// Now try to "drop" the device.
	if err := driver.Put("some-layer"); err != nil {
		t.Fatalf("unmounting some-layer: %v", err)
	}
}
