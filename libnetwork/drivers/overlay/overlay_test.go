// +build linux

package overlay

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/libnetwork/datastore"
	"github.com/docker/docker/libnetwork/discoverapi"
	"github.com/docker/docker/libnetwork/driverapi"
	"github.com/docker/docker/libnetwork/netlabel"
	_ "github.com/docker/docker/libnetwork/testutils"
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/libkv/store"
	"github.com/docker/libkv/store/boltdb"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func init() {
	boltdb.Register()
}

type driverTester struct {
	t *testing.T
	d *driver
}

const testNetworkType = "overlay"

func setupDriver(t *testing.T) *driverTester {
	dt := &driverTester{t: t}
	config := make(map[string]interface{})

	tmp, err := ioutil.TempFile("", "libnetwork-")
	if err != nil {
		t.Fatalf("Error creating temp file: %v", err)
	}
	err = tmp.Close()
	if err != nil {
		t.Fatalf("Error closing temp file: %v", err)
	}
	defaultPrefix := filepath.Join(os.TempDir(), "libnetwork", "test", "overlay")

	config[netlabel.GlobalKVClient] = discoverapi.DatastoreConfigData{
		Scope:    datastore.GlobalScope,
		Provider: "boltdb",
		Address:  filepath.Join(defaultPrefix, filepath.Base(tmp.Name())),
		Config: &store.Config{
			Bucket:            "libnetwork",
			ConnectionTimeout: 3 * time.Second,
		},
	}

	if err := Init(dt, config); err != nil {
		t.Fatal(err)
	}

	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := iface.Addrs()
	if err != nil || len(addrs) == 0 {
		t.Fatal(err)
	}
	data := discoverapi.NodeDiscoveryData{
		Address: addrs[0].String(),
		Self:    true,
	}
	dt.d.DiscoverNew(discoverapi.NodeDiscovery, data)
	return dt
}

func cleanupDriver(t *testing.T, dt *driverTester) {
	ch := make(chan struct{})
	go func() {
		Fini(dt.d)
		close(ch)
	}()

	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("test timed out because Fini() did not return on time")
	}
}

func (dt *driverTester) GetPluginGetter() plugingetter.PluginGetter {
	return nil
}

func (dt *driverTester) RegisterDriver(name string, drv driverapi.Driver,
	cap driverapi.Capability) error {
	if name != testNetworkType {
		dt.t.Fatalf("Expected driver register name to be %q. Instead got %q",
			testNetworkType, name)
	}

	if _, ok := drv.(*driver); !ok {
		dt.t.Fatalf("Expected driver type to be %T. Instead got %T",
			&driver{}, drv)
	}

	dt.d = drv.(*driver)
	return nil
}

func TestOverlayInit(t *testing.T) {
	if err := Init(&driverTester{t: t}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayFiniWithoutConfig(t *testing.T) {
	dt := &driverTester{t: t}
	if err := Init(dt, nil); err != nil {
		t.Fatal(err)
	}

	cleanupDriver(t, dt)
}

func TestOverlayConfig(t *testing.T) {
	dt := setupDriver(t)

	time.Sleep(1 * time.Second)

	d := dt.d
	if d.notifyCh == nil {
		t.Fatal("Driver notify channel wasn't initialized after Config method")
	}

	if d.exitCh == nil {
		t.Fatal("Driver serfloop exit channel wasn't initialized after Config method")
	}

	if d.serfInstance == nil {
		t.Fatal("Driver serfinstance  hasn't been initialized after Config method")
	}

	cleanupDriver(t, dt)
}

func TestOverlayType(t *testing.T) {
	dt := &driverTester{t: t}
	if err := Init(dt, nil); err != nil {
		t.Fatal(err)
	}

	if dt.d.Type() != testNetworkType {
		t.Fatalf("Expected Type() to return %q. Instead got %q", testNetworkType,
			dt.d.Type())
	}
}

// Test that the netlink socket close unblock the watchMiss to avoid deadlock
func TestNetlinkSocket(t *testing.T) {
	// This is the same code used by the overlay driver to create the netlink interface
	// for the watch miss
	nlSock, err := nl.Subscribe(syscall.NETLINK_ROUTE, syscall.RTNLGRP_NEIGH)
	if err != nil {
		t.Fatal()
	}
	// set the receive timeout to not remain stuck on the RecvFrom if the fd gets closed
	tv := unix.NsecToTimeval(soTimeout.Nanoseconds())
	err = nlSock.SetReceiveTimeout(&tv)
	if err != nil {
		t.Fatal()
	}
	n := &network{id: "testnetid"}
	ch := make(chan error)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		n.watchMiss(nlSock, fmt.Sprintf("/proc/%d/task/%d/ns/net", os.Getpid(), syscall.Gettid()))
		ch <- nil
	}()
	time.Sleep(5 * time.Second)
	nlSock.Close()
	select {
	case <-ch:
	case <-ctx.Done():
		{
			t.Fatalf("Timeout expired")
		}
	}
}
