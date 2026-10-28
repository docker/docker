package drivers

import (
	"strings"
	"testing"

	volumetestutils "github.com/moby/moby/v2/daemon/volume/testutils"
	"github.com/moby/moby/v2/pkg/plugingetter"
	"github.com/moby/moby/v2/pkg/plugins"
)

func TestGetDriver(t *testing.T) {
	s := NewStore(nil)
	_, err := s.GetDriver("missing")
	if err == nil {
		t.Fatal("Expected error, was nil")
	}
	s.Register(volumetestutils.NewFakeDriver("fake"), "fake")

	d, err := s.GetDriver("fake")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "fake" {
		t.Fatalf("Expected fake driver, got %s\n", d.Name())
	}
}

type dummyNilV1Plugin struct {
	name string
}

func (d *dummyNilV1Plugin) Name() string {
	return d.name
}

func (d *dummyNilV1Plugin) ScopedPath(s string) string {
	return s
}

func (d *dummyNilV1Plugin) IsV1() bool {
	return true
}

func (d *dummyNilV1Plugin) Client() *plugins.Client {
	return nil
}

type refCountPluginGetter struct {
	plugin   plugingetter.CompatPlugin
	refCount int
}

func (g *refCountPluginGetter) Get(name, capability string, mode int) (plugingetter.CompatPlugin, error) {
	g.refCount += mode
	return g.plugin, nil
}

func (g *refCountPluginGetter) GetAllByCap(capability string) ([]plugingetter.CompatPlugin, error) {
	return nil, nil
}

func (g *refCountPluginGetter) GetAllManagedPluginsByCap(capability string) []plugingetter.CompatPlugin {
	return nil
}

func (g *refCountPluginGetter) Handle(capability string, callback func(string, *plugins.Client)) {
}

func TestCreateDriverNilClient(t *testing.T) {
	pluginName := "nil-plugin"
	pg := &refCountPluginGetter{
		plugin: &dummyNilV1Plugin{name: pluginName},
	}
	s := NewStore(pg)

	_, err := s.CreateDriver(pluginName)
	if err == nil {
		t.Fatal("Expected error from CreateDriver with nil-client plugin")
	}
	if !strings.Contains(err.Error(), "client is nil") {
		t.Fatalf("Expected error mentioning nil client, got: %v", err)
	}
	if !strings.Contains(err.Error(), pluginName) {
		t.Fatalf("Expected error to contain plugin name %q, got: %v", pluginName, err)
	}
	if pg.refCount != 0 {
		t.Fatalf("Expected refCount to be 0 after failed CreateDriver, got %d", pg.refCount)
	}
}
