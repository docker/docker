package overlay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
	"os/user"
	"runtime"

	"golang.org/x/sys/unix"

	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/libkv/store/consul"
	"github.com/docker/docker/libnetwork/datastore"
	"github.com/docker/docker/libnetwork/discoverapi"
	"github.com/docker/docker/libnetwork/driverapi"
	"github.com/docker/docker/libnetwork/netlabel"
	_ "github.com/docker/docker/libnetwork/testutils"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

func init() {
	consul.Register()
}

type driverTester struct {
	t *testing.T
	d *driver
}

const testNetworkType = "overlay"

func setupDriver(t *testing.T) *driverTester {
	dt := &driverTester{t: t}
	config := make(map[string]interface{})
	config[netlabel.GlobalKVClient] = discoverapi.DatastoreConfigData{
		Scope:    datastore.GlobalScope,
		Provider: "consul",
		Address:  "127.0.0.01:8500",
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

func TestCreateVxlan(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Log("not linux OS, skip testing.")
		return
	}
	user, err := user.Current()
	if err != nil {
		t.Fatalf("error getting current user: %v", err)
	}
	if user.Username != "root" {
		t.Log("not root user, skip testing.")
		return
	}

	err = deleteInterface("non-existing")
	var e netlink.LinkNotFoundError
	if !errors.As(err, &e) {
		t.Fatalf("expected LinkNotFoundError, got %q", err)
	}

	vxlanName := "vx-001003-70qc4"
	err = createVxlan(vxlanName, 4099, 1450)
	if err != nil {
		t.Fatalf("error creating vxlan: %v", err)
	}
	// create vxlan with duplicated name
	err = createVxlan(vxlanName, 4099, 1450)
	if err == nil || !errors.Is(err, os.ErrExist) {
		deleteVxlan(t, vxlanName)
		t.Fatalf("expected os.ErrExist, got %q", err)
	}
	deleteVxlan(t, vxlanName)
	err = createVxlan(vxlanName, 4099, 1450)
	if err != nil {
		t.Fatalf("error re-creating vxlan: %v", err)
	}
	deleteVxlan(t, vxlanName)
}

func deleteVxlan(t *testing.T, vxlanName string) {
	if err := deleteInterface(vxlanName); err != nil {
		t.Errorf("error deleting vxlan: %v", err)
	}
}
