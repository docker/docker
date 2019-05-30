// +build linux

package daemon

import (
	"context"
	"time"

	"github.com/containerd/containerd"
	libcontainerdtypes "github.com/docker/docker/libcontainerd/types"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type mockProcess struct {
}

func (m *mockProcess) Delete(_ context.Context) (uint32, time.Time, error) {
	return 0, time.Time{}, nil
}

// Mock containerd client implementation, for unit tests.
type MockContainerdClient struct {
}

func (c *MockContainerdClient) Version(ctx context.Context) (containerd.Version, error) {
	return containerd.Version{}, nil
}
func (c *MockContainerdClient) Restore(ctx context.Context, containerID string, attachStdio libcontainerdtypes.StdioCallback) (alive bool, pid int, p libcontainerdtypes.Process, err error) {
	return false, 0, &mockProcess{}, nil
}
func (c *MockContainerdClient) Create(ctx context.Context, containerID string, spec *specs.Spec, runtimeOptions interface{}) error {
	return nil
}
func (c *MockContainerdClient) Start(ctx context.Context, containerID, checkpointDir string, withStdin bool, attachStdio libcontainerdtypes.StdioCallback) (pid int, err error) {
	return 0, nil
}
func (c *MockContainerdClient) SignalProcess(ctx context.Context, containerID, processID string, signal int) error {
	return nil
}
func (c *MockContainerdClient) Exec(ctx context.Context, containerID, processID string, spec *specs.Process, withStdin bool, attachStdio libcontainerdtypes.StdioCallback) (int, error) {
	return 0, nil
}
func (c *MockContainerdClient) ResizeTerminal(ctx context.Context, containerID, processID string, width, height int) error {
	return nil
}
func (c *MockContainerdClient) CloseStdin(ctx context.Context, containerID, processID string) error {
	return nil
}
func (c *MockContainerdClient) Pause(ctx context.Context, containerID string) error  { return nil }
func (c *MockContainerdClient) Resume(ctx context.Context, containerID string) error { return nil }
func (c *MockContainerdClient) Stats(ctx context.Context, containerID string) (*libcontainerdtypes.Stats, error) {
	return nil, nil
}
func (c *MockContainerdClient) ListPids(ctx context.Context, containerID string) ([]uint32, error) {
	return nil, nil
}
func (c *MockContainerdClient) Summary(ctx context.Context, containerID string) ([]libcontainerdtypes.Summary, error) {
	return nil, nil
}
func (c *MockContainerdClient) DeleteTask(ctx context.Context, containerID string) (uint32, time.Time, error) {
	return 0, time.Time{}, nil
}
func (c *MockContainerdClient) Delete(ctx context.Context, containerID string) error { return nil }
func (c *MockContainerdClient) Status(ctx context.Context, containerID string) (containerd.ProcessStatus, error) {
	return "null", nil
}
func (c *MockContainerdClient) UpdateResources(ctx context.Context, containerID string, resources *libcontainerdtypes.Resources) error {
	return nil
}
func (c *MockContainerdClient) CreateCheckpoint(ctx context.Context, containerID, checkpointDir string, exit bool) error {
	return nil
}
