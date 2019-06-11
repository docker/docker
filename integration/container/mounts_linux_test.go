package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	mounttypes "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/versions"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/system"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/fs"
	"gotest.tools/poll"
	"gotest.tools/skip"
)

func TestContainerNetworkMountsNoChown(t *testing.T) {
	// chown only applies to Linux bind mounted volumes; must be same host to verify
	skip.If(t, testEnv.IsRemoteDaemon)

	defer setupTest(t)()

	ctx := context.Background()

	tmpDir := fs.NewDir(t, "network-file-mounts", fs.WithMode(0755), fs.WithFile("nwfile", "network file bind mount", fs.WithMode(0644)))
	defer tmpDir.Remove()

	tmpNWFileMount := tmpDir.Join("nwfile")

	config := containertypes.Config{
		Image: "busybox",
	}
	hostConfig := containertypes.HostConfig{
		Mounts: []mounttypes.Mount{
			{
				Type:   "bind",
				Source: tmpNWFileMount,
				Target: "/etc/resolv.conf",
			},
			{
				Type:   "bind",
				Source: tmpNWFileMount,
				Target: "/etc/hostname",
			},
			{
				Type:   "bind",
				Source: tmpNWFileMount,
				Target: "/etc/hosts",
			},
		},
	}

	cli, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(t, err)
	defer cli.Close()

	ctrCreate, err := cli.ContainerCreate(ctx, &config, &hostConfig, &network.NetworkingConfig{}, "")
	assert.NilError(t, err)
	// container will exit immediately because of no tty, but we only need the start sequence to test the condition
	err = cli.ContainerStart(ctx, ctrCreate.ID, types.ContainerStartOptions{})
	assert.NilError(t, err)

	// Check that host-located bind mount network file did not change ownership when the container was started
	// Note: If the user specifies a mountpath from the host, we should not be
	// attempting to chown files outside the daemon's metadata directory
	// (represented by `daemon.repository` at init time).
	// This forces users who want to use user namespaces to handle the
	// ownership needs of any external files mounted as network files
	// (/etc/resolv.conf, /etc/hosts, /etc/hostname) separately from the
	// daemon. In all other volume/bind mount situations we have taken this
	// same line--we don't chown host file content.
	// See GitHub PR 34224 for details.
	statT, err := system.Stat(tmpNWFileMount)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(uint32(0), statT.UID()), "bind mounted network file should not change ownership from root")
}

func TestMountDaemonRoot(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()
	info, err := client.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		desc        string
		propagation mounttypes.Propagation
		expected    mounttypes.Propagation
	}{
		{
			desc:        "default",
			propagation: "",
			expected:    mounttypes.PropagationRSlave,
		},
		{
			desc:        "private",
			propagation: mounttypes.PropagationPrivate,
		},
		{
			desc:        "rprivate",
			propagation: mounttypes.PropagationRPrivate,
		},
		{
			desc:        "slave",
			propagation: mounttypes.PropagationSlave,
		},
		{
			desc:        "rslave",
			propagation: mounttypes.PropagationRSlave,
			expected:    mounttypes.PropagationRSlave,
		},
		{
			desc:        "shared",
			propagation: mounttypes.PropagationShared,
		},
		{
			desc:        "rshared",
			propagation: mounttypes.PropagationRShared,
			expected:    mounttypes.PropagationRShared,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			test := test
			t.Parallel()

			propagationSpec := fmt.Sprintf(":%s", test.propagation)
			if test.propagation == "" {
				propagationSpec = ""
			}
			bindSpecRoot := info.DockerRootDir + ":" + "/foo" + propagationSpec
			bindSpecSub := filepath.Join(info.DockerRootDir, "containers") + ":/foo" + propagationSpec

			for name, hc := range map[string]*containertypes.HostConfig{
				"bind root":    {Binds: []string{bindSpecRoot}},
				"bind subpath": {Binds: []string{bindSpecSub}},
				"mount root": {
					Mounts: []mounttypes.Mount{
						{
							Type:        mounttypes.TypeBind,
							Source:      info.DockerRootDir,
							Target:      "/foo",
							BindOptions: &mounttypes.BindOptions{Propagation: test.propagation},
						},
					},
				},
				"mount subpath": {
					Mounts: []mounttypes.Mount{
						{
							Type:        mounttypes.TypeBind,
							Source:      filepath.Join(info.DockerRootDir, "containers"),
							Target:      "/foo",
							BindOptions: &mounttypes.BindOptions{Propagation: test.propagation},
						},
					},
				},
			} {
				t.Run(name, func(t *testing.T) {
					hc := hc
					t.Parallel()

					c, err := client.ContainerCreate(ctx, &containertypes.Config{
						Image: "busybox",
						Cmd:   []string{"true"},
					}, hc, nil, "")

					if err != nil {
						if test.expected != "" {
							t.Fatal(err)
						}
						// expected an error, so this is ok and should not continue
						return
					}
					if test.expected == "" {
						t.Fatal("expected create to fail")
					}

					defer func() {
						if err := client.ContainerRemove(ctx, c.ID, types.ContainerRemoveOptions{Force: true}); err != nil {
							panic(err)
						}
					}()

					inspect, err := client.ContainerInspect(ctx, c.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(inspect.Mounts) != 1 {
						t.Fatalf("unexpected number of mounts: %+v", inspect.Mounts)
					}

					m := inspect.Mounts[0]
					if m.Propagation != test.expected {
						t.Fatalf("got unexpected propagation mode, expected %q, got: %v", test.expected, m.Propagation)
					}
				})
			}
		})
	}
}

func TestContainerBindMountNonRecursive(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"), "BindOptions.NonRecursive requires API v1.40")

	defer setupTest(t)()

	tmpDir1 := fs.NewDir(t, "tmpdir1", fs.WithMode(0755),
		fs.WithDir("mnt", fs.WithMode(0755)))
	defer tmpDir1.Remove()
	tmpDir1Mnt := filepath.Join(tmpDir1.Path(), "mnt")
	tmpDir2 := fs.NewDir(t, "tmpdir2", fs.WithMode(0755),
		fs.WithFile("file", "should not be visible when NonRecursive", fs.WithMode(0644)))
	defer tmpDir2.Remove()

	err := mount.Mount(tmpDir2.Path(), tmpDir1Mnt, "none", "bind,ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mount.Unmount(tmpDir1Mnt); err != nil {
			t.Fatal(err)
		}
	}()

	// implicit is recursive (NonRecursive: false)
	implicit := mounttypes.Mount{
		Type:     "bind",
		Source:   tmpDir1.Path(),
		Target:   "/foo",
		ReadOnly: true,
	}
	recursive := implicit
	recursive.BindOptions = &mounttypes.BindOptions{
		NonRecursive: false,
	}
	recursiveVerifier := []string{"test", "-f", "/foo/mnt/file"}
	nonRecursive := implicit
	nonRecursive.BindOptions = &mounttypes.BindOptions{
		NonRecursive: true,
	}
	nonRecursiveVerifier := []string{"test", "!", "-f", "/foo/mnt/file"}

	ctx := context.Background()
	client := testEnv.APIClient()
	containers := []string{
		container.Run(ctx, t, client, container.WithMount(implicit), container.WithCmd(recursiveVerifier...)),
		container.Run(ctx, t, client, container.WithMount(recursive), container.WithCmd(recursiveVerifier...)),
		container.Run(ctx, t, client, container.WithMount(nonRecursive), container.WithCmd(nonRecursiveVerifier...)),
	}

	for _, c := range containers {
		poll.WaitOn(t, container.IsSuccessful(ctx, client, c), poll.WithDelay(100*time.Millisecond))
	}
}

func TestMultipleMountsDifferentSubPaths(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	volumeName := prepareVolume(t)

	mnt1 := mounttypes.Mount{
		Type:          mounttypes.TypeVolume,
		Source:        volumeName,
		Target:        "/foo",
		VolumeOptions: &mounttypes.VolumeOptions{SubPath: "/bar"},
	}
	mnt2 := mounttypes.Mount{
		Type:          mounttypes.TypeVolume,
		Source:        volumeName,
		Target:        "/x",
		VolumeOptions: &mounttypes.VolumeOptions{SubPath: "/bar/baz"},
	}

	cID := container.Run(t, ctx, client,
		container.WithMount(mnt1),
		container.WithMount(mnt2),
		container.WithCmd("sh", "-c", "cat /foo/file"),
	)
	poll.WaitOn(t, container.IsSuccessful(ctx, client, cID), poll.WithDelay(100*time.Millisecond))

	err := client.VolumeRemove(ctx, volumeName, false)
	assert.Check(t, is.ErrorContains(err, "volume is in use"))

	err = client.ContainerRemove(ctx, cID, types.ContainerRemoveOptions{Force: true})
	assert.NilError(t, err)

	err = client.VolumeRemove(ctx, volumeName, false)
	assert.NilError(t, err)
}

func TestSubPathMultipleNestedMounts(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	volumeName := prepareVolume(t)

	mnt1 := mounttypes.Mount{
		Type:   mounttypes.TypeVolume,
		Source: volumeName,
		Target: "/foo",
	}
	mnt2 := mounttypes.Mount{
		Type:          mounttypes.TypeVolume,
		Source:        volumeName,
		Target:        "/foo/bar",
		VolumeOptions: &mounttypes.VolumeOptions{SubPath: "/bar"},
	}

	cID := container.Run(t, ctx, client,
		container.WithMount(mnt1),
		container.WithMount(mnt2),
		container.WithCmd("sh", "-c", "cat /foo/bar/file && cd /foo/bar/baz"),
	)
	poll.WaitOn(t, container.IsSuccessful(ctx, client, cID), poll.WithDelay(100*time.Millisecond))

	err := client.VolumeRemove(ctx, volumeName, false)
	assert.Check(t, is.ErrorContains(err, "volume is in use"))

	err = client.ContainerRemove(ctx, cID, types.ContainerRemoveOptions{Force: true})
	assert.NilError(t, err)

	err = client.VolumeRemove(ctx, volumeName, false)
	assert.NilError(t, err)
}

func TestNonExistentSubPath(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	volumeName := prepareVolume(t)

	mnt := mounttypes.Mount{
		Type:   mounttypes.TypeVolume,
		Source: volumeName,
		Target: "/bin",
		VolumeOptions: &mounttypes.VolumeOptions{
			NoCopy:  false,
			SubPath: "bin",
		},
	}
	cID := container.Create(t, ctx, client, container.WithMount(mnt))
	err := client.ContainerStart(ctx, cID, types.ContainerStartOptions{})
	assert.Check(t, is.ErrorContains(err, "directory bin does not exist under volume path"))
}

func prepareVolume(t *testing.T) string {
	client := testEnv.APIClient()
	ctx := context.Background()

	volumeName := t.Name() + "_vol_foo"
	_, err := client.VolumeCreate(ctx, volumetypes.VolumeCreateBody{Name: volumeName})
	assert.NilError(t, err)

	mnt := mounttypes.Mount{
		Type:   mounttypes.TypeVolume,
		Source: volumeName,
		Target: "/foo",
	}

	cID := container.Run(t, ctx, client, container.WithMount(mnt), container.WithCmd("sh", "-c", "mkdir -p /foo/bar/baz; echo xyz > /foo/bar/file"))
	poll.WaitOn(t,
		container.IsInState(ctx, client, cID, "exited"),
		poll.WithDelay(100*time.Millisecond),
	)
	err = client.ContainerRemove(ctx, cID, types.ContainerRemoveOptions{Force: true})
	assert.NilError(t, err)
	return volumeName
}
