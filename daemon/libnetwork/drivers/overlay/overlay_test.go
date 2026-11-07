//go:build linux

package overlay

import (
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/pkg/plugingetter"
)

type driverTester struct {
	t *testing.T
	d *driver
}

const testNetworkType = "overlay"

func (dt *driverTester) GetPluginGetter() plugingetter.PluginGetter {
	return nil
}

func (dt *driverTester) RegisterDriver(name string, drv driverapi.Driver, capability driverapi.Capability) error {
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

func (dt *driverTester) RegisterNetworkAllocator(name string, _ driverapi.NetworkAllocator) error {
	dt.t.Fatalf("Unexpected call to RegisterNetworkAllocator for %q", name)
	return nil
}

func TestOverlayInit(t *testing.T) {
	if err := Register(&driverTester{t: t}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
