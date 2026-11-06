package daemon

import (
	"context"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/daemon/server/backend"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestGetInspectData(t *testing.T) {
	c := &container.Container{
		ID:              "inspect-me",
		NetworkSettings: &network.Settings{},
		HostConfig:      &containertypes.HostConfig{},
		State:           &container.State{},
		ExecCommands:    container.NewExecStore(),
	}

	d := &Daemon{
		linkIndex: newLinkIndex(),
	}
	cfg := &configStore{}
	d.configStore.Store(cfg)

	_, _, err := d.getInspectData(&cfg.Config, c)
	assert.Check(t, err)
}

func TestContainerInspect(t *testing.T) {
	c := &container.Container{
		ID:              "inspect-me",
		NetworkSettings: &network.Settings{},
		HostConfig:      &containertypes.HostConfig{},
		State:           &container.State{},
		ExecCommands:    container.NewExecStore(),
	}

	d := &Daemon{
		linkIndex: newLinkIndex(),
	}
	if d.UsesSnapshotter() {
		t.Skip("does not apply to containerd snapshotters, which don't have RWLayer set")
	}
	cfg := &configStore{}
	d.configStore.Store(cfg)

	_, _, err := d.containerInspect(&cfg.Config, c)
	assert.Check(t, is.ErrorContains(err, "RWLayer of container inspect-me is unexpectedly nil"))

	c.State.Dead = true
	_, _, err = d.containerInspect(&cfg.Config, c)
	assert.Check(t, err)
}

type fakeInspectImageService struct {
	ImageService
	sizeRw     int64
	sizeRootFs int64
	err        error
	called     bool
}

func (s *fakeInspectImageService) GetContainerLayerSize(ctx context.Context, _ string) (int64, int64, error) {
	s.called = true
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if s.err != nil {
		return 0, 0, s.err
	}
	return s.sizeRw, s.sizeRootFs, nil
}

func TestContainerInspect_Size(t *testing.T) {
	c := &container.Container{
		ID:              "inspect-size-ok",
		NetworkSettings: &network.Settings{},
		HostConfig:      &containertypes.HostConfig{},
		State:           &container.State{Dead: true},
		ExecCommands:    container.NewExecStore(),
		Config:          &containertypes.Config{},
	}

	imgSvc := &fakeInspectImageService{sizeRw: 1024, sizeRootFs: 2048}
	d := &Daemon{
		linkIndex:    newLinkIndex(),
		imageService: imgSvc,
	}
	d.containers = container.NewMemoryStore()
	d.containers.Add(c.ID, c)

	cfg := &configStore{}
	d.configStore.Store(cfg)

	// Test with options.Size = true
	resp, _, err := d.ContainerInspect(t.Context(), c.ID, backend.ContainerInspectOptions{Size: true})
	assert.NilError(t, err)
	assert.Assert(t, resp != nil)
	assert.Assert(t, resp.SizeRw != nil)
	assert.Check(t, is.Equal(*resp.SizeRw, int64(1024)))
	assert.Assert(t, resp.SizeRootFs != nil)
	assert.Check(t, is.Equal(*resp.SizeRootFs, int64(2048)))
	assert.Check(t, imgSvc.called)

	// Test with options.Size = false
	imgSvc.called = false
	respNoSize, _, err := d.ContainerInspect(t.Context(), c.ID, backend.ContainerInspectOptions{Size: false})
	assert.NilError(t, err)
	assert.Assert(t, respNoSize != nil)
	assert.Check(t, respNoSize.SizeRw == nil)
	assert.Check(t, respNoSize.SizeRootFs == nil)
	assert.Check(t, !imgSvc.called)
}

func TestContainerInspect_SizeContextCancellation(t *testing.T) {
	c := &container.Container{
		ID:              "inspect-size-cancel",
		NetworkSettings: &network.Settings{},
		HostConfig:      &containertypes.HostConfig{},
		State:           &container.State{Dead: true},
		ExecCommands:    container.NewExecStore(),
		Config:          &containertypes.Config{},
	}

	imgSvc := &fakeInspectImageService{sizeRw: 1024, sizeRootFs: 2048}
	d := &Daemon{
		linkIndex:    newLinkIndex(),
		imageService: imgSvc,
	}
	d.containers = container.NewMemoryStore()
	d.containers.Add(c.ID, c)

	cfg := &configStore{}
	d.configStore.Store(cfg)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	resp, _, err := d.ContainerInspect(ctx, c.ID, backend.ContainerInspectOptions{Size: true})
	assert.Check(t, is.ErrorIs(err, context.Canceled))
	assert.Check(t, resp == nil)
}
