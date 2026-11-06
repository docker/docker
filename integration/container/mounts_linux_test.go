package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	mounttypes "github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/stringid"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/volume"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/pkg/parsers/kernel"
	"github.com/moby/sys/mount"
	"github.com/moby/sys/mountinfo"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/fs"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// testNonExistingPlugin is a special plugin-name, which overrides defaultTimeOut in tests.
// this is a copy of https://github.com/moby/moby/blob/9e00a63d65434cdedc444e79a2b33a7c202b10d8/pkg/plugins/client.go#L253-L254
const testNonExistingPlugin = "this-plugin-does-not-exist"

// TestContainerMultipleMountsSamePath verifies that when a host path has
// multiple bind mount entries in /proc/self/mountinfo with different
// propagation (e.g., slave then shared), Docker reads the most recent
// entry and allows containers to use the path with shared propagation.
//
// Regression test for https://github.com/moby/moby/issues/43714
func TestContainerMultipleMountsSamePath(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsUserNamespace)
	skip.If(t, testEnv.IsRootless, "cannot be tested because RootlessKit executes the daemon in private mount namespace (https://github.com/rootless-containers/rootlesskit/issues/97)")

	ctx := setupTest(t)

	tmpDir := fs.NewDir(t, "propagation-test", fs.WithMode(0o755))
	src := tmpDir.Path()

	// First bind mount + make slave → mountinfo entry 1 (master:N)
	assert.NilError(t, syscall.Mount(src, src, "", syscall.MS_BIND, ""))
	t.Cleanup(func() { syscall.Unmount(src, syscall.MNT_DETACH) })
	assert.NilError(t, syscall.Mount("", src, "", syscall.MS_SLAVE, ""))

	// Second bind mount + make shared → mountinfo entry 2 (shared:N)
	assert.NilError(t, syscall.Mount(src, src, "", syscall.MS_BIND, ""))
	t.Cleanup(func() { syscall.Unmount(src, syscall.MNT_DETACH) })
	assert.NilError(t, syscall.Mount("", src, "", syscall.MS_SHARED, ""))

	apiClient := testEnv.APIClient()
	ctrID := container.Run(ctx, t, apiClient,
		container.WithCmd("true"),
		container.WithMount(mounttypes.Mount{
			Type:   mounttypes.TypeBind,
			Source: src,
			Target: "/volume-dest",
			BindOptions: &mounttypes.BindOptions{
				Propagation: mounttypes.PropagationShared,
			},
		}),
	)
	poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, ctrID))
}
func TestContainerNetworkMountsNoChown(t *testing.T) {
	// chown only applies to Linux bind mounted volumes; must be same host to verify
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsNonRootClient, "skipping test that requires root on client side")

	ctx := setupTest(t)

	tmpDir := fs.NewDir(t, "network-file-mounts", fs.WithMode(0o755), fs.WithFile("nwfile", "network file bind mount", fs.WithMode(0o644)))
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

	cli, err := client.New(client.FromEnv)
	assert.NilError(t, err)
	defer cli.Close()

	ctrCreate, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           &config,
		HostConfig:       &hostConfig,
		NetworkingConfig: &network.NetworkingConfig{},
	})
	assert.NilError(t, err)
	// container will exit immediately because of no tty, but we only need the start sequence to test the condition
	_, err = cli.ContainerStart(ctx, ctrCreate.ID, client.ContainerStartOptions{})
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
	info, err := os.Stat(tmpNWFileMount)
	assert.NilError(t, err)
	fi := info.Sys().(*syscall.Stat_t)
	assert.Check(t, is.Equal(fi.Uid, uint32(0)), "bind mounted network file should not change ownership from root")
}

func assertContainerMountPoint(t *testing.T, mountPoint, expected containertypes.MountPoint) {
	t.Helper()

	if expected.Source != "" {
		assert.Check(t, is.Equal(mountPoint.Source, expected.Source))
	}
	if expected.Name != "" {
		assert.Check(t, is.Equal(mountPoint.Name, expected.Name))
	}
	if expected.Driver != "" {
		assert.Check(t, is.Equal(mountPoint.Driver, expected.Driver))
	}
	if expected.Propagation != "" {
		assert.Check(t, is.Equal(mountPoint.Propagation, expected.Propagation))
	}
	assert.Check(t, is.Equal(mountPoint.RW, expected.RW))
	assert.Check(t, is.Equal(mountPoint.Type, expected.Type))
	assert.Check(t, is.Equal(mountPoint.Mode, expected.Mode))
	assert.Check(t, is.Equal(mountPoint.Destination, expected.Destination))
}

func TestContainerBindMountInspect(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	type testCase struct {
		desc     string
		spec     mounttypes.Mount
		expected containertypes.MountPoint
	}

	const destPath = "/foo"
	tmpDir1 := fs.NewDir(t, "test-mounts-api-1", fs.WithMode(0o755))
	tests := []testCase{{
		desc:     "readonly bind",
		spec:     mounttypes.Mount{Type: "bind", Source: tmpDir1.Path(), Target: destPath, ReadOnly: true},
		expected: containertypes.MountPoint{Type: "bind", RW: false, Destination: destPath, Source: tmpDir1.Path()},
	}}

	tmpDir3 := fs.NewDir(t, "test-mounts-api-3", fs.WithMode(0o755))
	if err := mount.Mount(tmpDir3.Path(), tmpDir3.Path(), "none", "bind,shared"); err == nil {
		t.Cleanup(func() {
			assert.NilError(t, mount.Unmount(tmpDir3.Path()))
		})
		tests = append(tests, testCase{
			desc:     "readonly bind shared propagation",
			spec:     mounttypes.Mount{Type: "bind", Source: tmpDir3.Path(), Target: destPath, ReadOnly: true, BindOptions: &mounttypes.BindOptions{Propagation: "shared"}},
			expected: containertypes.MountPoint{Type: "bind", RW: false, Destination: destPath, Source: tmpDir3.Path(), Propagation: "shared"},
		})
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)
			ctr, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config: &containertypes.Config{
					Image: "busybox",
					Cmd:   []string{"true"},
				},
				HostConfig:       &containertypes.HostConfig{Mounts: []mounttypes.Mount{tc.spec}},
				NetworkingConfig: &network.NetworkingConfig{},
			})
			assert.NilError(t, err)

			inspect, err := apiClient.ContainerInspect(ctx, ctr.ID, client.ContainerInspectOptions{})
			assert.NilError(t, err)
			assert.Assert(t, is.Len(inspect.Container.Mounts, 1))
			assertContainerMountPoint(t, inspect.Container.Mounts[0], tc.expected)

			_, err = apiClient.ContainerRemove(ctx, ctr.ID, client.ContainerRemoveOptions{
				RemoveVolumes: true,
				Force:         true,
			})
			assert.NilError(t, err)
		})
	}
}

func TestMountDaemonRoot(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()
	result, err := apiClient.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info := result.Info

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

			ctx := testutil.StartSpan(ctx, t)

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

					ctx := testutil.StartSpan(ctx, t)

					c, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
						Config: &containertypes.Config{
							Image: "busybox",
							Cmd:   []string{"true"},
						},
						HostConfig: hc,
					})
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
						if _, err := apiClient.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
							panic(err)
						}
					}()

					inspect, err := apiClient.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if len(inspect.Container.Mounts) != 1 {
						t.Fatalf("unexpected number of mounts: %+v", inspect.Container.Mounts)
					}

					m := inspect.Container.Mounts[0]
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
	skip.If(t, testEnv.IsRootless, "cannot be tested because RootlessKit executes the daemon in private mount namespace (https://github.com/rootless-containers/rootlesskit/issues/97)")
	skip.If(t, testEnv.IsUserNamespace, "non-recursive bind mounts of directories containing submounts fail with EINVAL in a user namespace")

	ctx := setupTest(t)

	tmpDir1 := fs.NewDir(t, "tmpdir1", fs.WithMode(0o755), fs.WithDir("mnt", fs.WithMode(0o755)))
	tmpDir1Mnt := filepath.Join(tmpDir1.Path(), "mnt")
	tmpDir2 := fs.NewDir(t, "tmpdir2", fs.WithMode(0o755), fs.WithFile("file", "should not be visible when NonRecursive", fs.WithMode(0o644)))

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

	apiClient := testEnv.APIClient()
	containers := []string{
		container.Run(ctx, t, apiClient, container.WithMount(implicit), container.WithCmd(recursiveVerifier...)),
		container.Run(ctx, t, apiClient, container.WithMount(recursive), container.WithCmd(recursiveVerifier...)),
		container.Run(ctx, t, apiClient, container.WithMount(nonRecursive), container.WithCmd(nonRecursiveVerifier...)),
	}

	for _, c := range containers {
		poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, c))
	}
}

func TestContainerVolumesMountedAsShared(t *testing.T) {
	// Volume propagation is linux only. Also it creates directories for
	// bind mounting, so needs to be same host.
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsUserNamespace)
	skip.If(t, testEnv.IsRootless, "cannot be tested because RootlessKit executes the daemon in private mount namespace (https://github.com/rootless-containers/rootlesskit/issues/97)")

	ctx := setupTest(t)

	// Prepare a source directory to bind mount
	tmpDir1 := fs.NewDir(t, "volume-source", fs.WithMode(0o755), fs.WithDir("mnt1", fs.WithMode(0o755)))
	tmpDir1Mnt := filepath.Join(tmpDir1.Path(), "mnt1")

	// Convert this directory into a shared mount point so that we do
	// not rely on propagation properties of parent mount.
	if err := mount.MakePrivate(tmpDir1.Path()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mount.Unmount(tmpDir1.Path()); err != nil {
			t.Fatal(err)
		}
	}()
	if err := mount.MakeShared(tmpDir1.Path()); err != nil {
		t.Fatal(err)
	}

	sharedMount := mounttypes.Mount{
		Type:   mounttypes.TypeBind,
		Source: tmpDir1.Path(),
		Target: "/volume-dest",
		BindOptions: &mounttypes.BindOptions{
			Propagation: mounttypes.PropagationShared,
		},
	}

	bindMountCmd := []string{"mount", "--bind", "/volume-dest/mnt1", "/volume-dest/mnt1"}

	apiClient := testEnv.APIClient()
	containerID := container.Run(ctx, t, apiClient, container.WithPrivileged(true), container.WithMount(sharedMount), container.WithCmd(bindMountCmd...))
	poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, containerID))

	// Make sure a bind mount under a shared volume propagated to host.
	if mounted, _ := mountinfo.Mounted(tmpDir1Mnt); !mounted {
		t.Fatalf("Bind mount under shared volume did not propagate to host")
	}

	mount.Unmount(tmpDir1Mnt)
}

func TestContainerVolumesMountedAsSlave(t *testing.T) {
	// Volume propagation is linux only. Also it creates directories for
	// bind mounting, so needs to be same host.
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsUserNamespace)
	skip.If(t, testEnv.IsRootless, "cannot be tested because RootlessKit executes the daemon in private mount namespace (https://github.com/rootless-containers/rootlesskit/issues/97)")

	ctx := testutil.StartSpan(baseContext, t)

	// Prepare a source directory to bind mount
	tmpDir1 := fs.NewDir(t, "volume-source", fs.WithMode(0o755), fs.WithDir("mnt1", fs.WithMode(0o755)))
	tmpDir1Mnt := filepath.Join(tmpDir1.Path(), "mnt1")

	// Prepare a source directory with file in it. We will bind mount this
	// directory and see if file shows up.
	tmpDir2 := fs.NewDir(t, "volume-source2", fs.WithMode(0o755), fs.WithFile("slave-testfile", "Test", fs.WithMode(0o644)))

	// Convert this directory into a shared mount point so that we do
	// not rely on propagation properties of parent mount.
	if err := mount.MakePrivate(tmpDir1.Path()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mount.Unmount(tmpDir1.Path()); err != nil {
			t.Fatal(err)
		}
	}()
	if err := mount.MakeShared(tmpDir1.Path()); err != nil {
		t.Fatal(err)
	}

	slaveMount := mounttypes.Mount{
		Type:   mounttypes.TypeBind,
		Source: tmpDir1.Path(),
		Target: "/volume-dest",
		BindOptions: &mounttypes.BindOptions{
			Propagation: mounttypes.PropagationSlave,
		},
	}

	topCmd := []string{"top"}

	apiClient := testEnv.APIClient()
	containerID := container.Run(ctx, t, apiClient, container.WithTty(true), container.WithMount(slaveMount), container.WithCmd(topCmd...), container.WithSecurityOpt("label=disable"))

	// Bind mount tmpDir2/ onto tmpDir1/mnt1. If mount propagates inside
	// container then contents of tmpDir2/slave-testfile should become
	// visible at "/volume-dest/mnt1/slave-testfile"
	if err := mount.Mount(tmpDir2.Path(), tmpDir1Mnt, "none", "bind"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mount.Unmount(tmpDir1Mnt); err != nil {
			t.Fatal(err)
		}
	}()

	mountCmd := []string{"cat", "/volume-dest/mnt1/slave-testfile"}

	if result, err := container.Exec(ctx, apiClient, containerID, mountCmd); err == nil {
		if result.Stdout() != "Test" {
			t.Fatalf("Bind mount under slave volume did not propagate to container")
		}
	} else {
		t.Fatal(err)
	}
}

// TestContainerVolumeAnonymous verifies that anonymous volumes created through
// the Mounts API get a random name generated, and have the "AnonymousLabel"
// (com.docker.volume.anonymous) label set.
//
// regression test for https://github.com/moby/moby/issues/48748
func TestContainerVolumeAnonymous(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	t.Run("no driver specified", func(t *testing.T) {
		mntOpts := mounttypes.Mount{Type: mounttypes.TypeVolume, Target: "/foo"}
		cID := container.Create(ctx, t, apiClient, container.WithMount(mntOpts))

		inspect := container.Inspect(ctx, t, apiClient, cID)
		assert.Assert(t, is.Len(inspect.HostConfig.Mounts, 1))
		assert.Check(t, is.Equal(inspect.HostConfig.Mounts[0], mntOpts))

		assert.Assert(t, is.Len(inspect.Mounts, 1))
		vol := inspect.Mounts[0]
		assert.Check(t, is.Len(vol.Name, 64), "volume name should be 64 bytes (from stringid.GenerateRandomID())")
		assert.Check(t, is.Equal(vol.Driver, volume.DefaultDriverName))

		res, err := apiClient.VolumeInspect(ctx, vol.Name, client.VolumeInspectOptions{})
		assert.NilError(t, err)

		// see [daemon.AnonymousLabel]; we don't want to import the daemon package here.
		const expectedAnonymousLabel = "com.docker.volume.anonymous"
		assert.Check(t, is.Contains(res.Volume.Labels, expectedAnonymousLabel))
		assert.Check(t, is.Equal(res.Volume.Driver, volume.DefaultDriverName))
	})

	t.Run("inspect mount point fields", func(t *testing.T) {
		const destPath = "/foo"
		selinuxSharedLabel := "z"

		tests := []struct {
			desc     string
			spec     mounttypes.Mount
			expected containertypes.MountPoint
		}{
			{
				desc:     "anonymous volume with trailing slash",
				spec:     mounttypes.Mount{Type: mounttypes.TypeVolume, Target: destPath + "/"},
				expected: containertypes.MountPoint{Driver: volume.DefaultDriverName, Type: mounttypes.TypeVolume, RW: true, Destination: destPath, Mode: selinuxSharedLabel},
			},
			{
				desc:     "readonly named volume",
				spec:     mounttypes.Mount{Type: mounttypes.TypeVolume, Target: destPath, ReadOnly: true, Source: stringid.GenerateRandomID()},
				expected: containertypes.MountPoint{Type: mounttypes.TypeVolume, RW: false, Destination: destPath, Mode: selinuxSharedLabel},
			},
			{
				desc:     "named volume with driver",
				spec:     mounttypes.Mount{Type: mounttypes.TypeVolume, Target: destPath, Source: stringid.GenerateRandomID(), VolumeOptions: &mounttypes.VolumeOptions{DriverConfig: &mounttypes.Driver{Name: volume.DefaultDriverName}}},
				expected: containertypes.MountPoint{Driver: volume.DefaultDriverName, Type: mounttypes.TypeVolume, RW: true, Destination: destPath, Mode: selinuxSharedLabel},
			},
			{
				desc:     "anonymous volume no copy",
				spec:     mounttypes.Mount{Type: mounttypes.TypeVolume, Target: destPath, VolumeOptions: &mounttypes.VolumeOptions{NoCopy: true}},
				expected: containertypes.MountPoint{Driver: volume.DefaultDriverName, Type: mounttypes.TypeVolume, RW: true, Destination: destPath, Mode: selinuxSharedLabel},
			},
		}

		for _, tc := range tests {
			t.Run(tc.desc, func(t *testing.T) {
				ctx := testutil.StartSpan(ctx, t)
				ctr, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
					Config: &containertypes.Config{
						Image: "busybox",
						Cmd:   []string{"true"},
					},
					HostConfig:       &containertypes.HostConfig{Mounts: []mounttypes.Mount{tc.spec}},
					NetworkingConfig: &network.NetworkingConfig{},
				})
				assert.NilError(t, err)

				inspect, err := apiClient.ContainerInspect(ctx, ctr.ID, client.ContainerInspectOptions{})
				assert.NilError(t, err)
				assert.Assert(t, is.Len(inspect.Container.Mounts, 1))
				mountPoint := inspect.Container.Mounts[0]
				if tc.spec.Source != "" {
					tc.expected.Name = tc.spec.Source
				}
				assertContainerMountPoint(t, mountPoint, tc.expected)

				_, err = apiClient.ContainerRemove(ctx, ctr.ID, client.ContainerRemoveOptions{
					RemoveVolumes: true,
					Force:         true,
				})
				assert.NilError(t, err)

				if tc.spec.Source != "" {
					_, err = apiClient.VolumeRemove(ctx, tc.spec.Source, client.VolumeRemoveOptions{Force: true})
					assert.NilError(t, err)
				}
			})
		}
	})

	// Verify that specifying a custom driver is still taken into account.
	t.Run("custom driver", func(t *testing.T) {
		config := container.NewTestConfig(container.WithMount(mounttypes.Mount{
			Type:   mounttypes.TypeVolume,
			Target: "/foo",
			VolumeOptions: &mounttypes.VolumeOptions{
				DriverConfig: &mounttypes.Driver{
					Name: testNonExistingPlugin,
				},
			},
		}))
		_, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config:           config.Config,
			HostConfig:       config.HostConfig,
			NetworkingConfig: config.NetworkingConfig,
			Platform:         config.Platform,
			Name:             config.Name,
		})
		// We use [testNonExistingPlugin] for this, which produces an error
		// when used, which we use as indicator that the driver was passed
		// through. We should have a cleaner way for this, but that would
		// require a custom volume plugin to be installed.
		assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
		assert.Check(t, is.ErrorContains(err, fmt.Sprintf(`plugin %q not found`, testNonExistingPlugin)))
	})
}

// Regression test for #38995 and #43390.
func TestContainerCopyLeaksMounts(t *testing.T) {
	ctx := setupTest(t)

	bindMount := mounttypes.Mount{
		Type:   mounttypes.TypeBind,
		Source: "/var",
		Target: "/hostvar",
		BindOptions: &mounttypes.BindOptions{
			Propagation: mounttypes.PropagationRSlave,
		},
	}

	apiClient := testEnv.APIClient()
	cid := container.Run(ctx, t, apiClient, container.WithMount(bindMount), container.WithCmd("sleep", "120s"))

	getMounts := func() string {
		t.Helper()
		res, err := container.Exec(ctx, apiClient, cid, []string{"cat", "/proc/self/mountinfo"})
		assert.NilError(t, err)
		assert.Equal(t, res.ExitCode, 0)
		return res.Stdout()
	}

	mountsBefore := getMounts()

	_, err := apiClient.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: "/etc/passwd"})
	assert.NilError(t, err)

	mountsAfter := getMounts()

	assert.Equal(t, mountsBefore, mountsAfter)
}

func TestContainerBindMountReadOnlyDefault(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, !isRROSupported(), "requires recursive read-only mounts")

	ctx := setupTest(t)

	// The test will run a container with a simple readonly /dev bind mount (-v /dev:/dev:ro)
	// It will then check /proc/self/mountinfo for the mount type of /dev/shm (submount of /dev)
	// If /dev/shm is rw, that will mean that the read-only mounts are NOT recursive by default.
	const nonRecursive = " /dev/shm rw,"
	// If /dev/shm is ro, that will mean that the read-only mounts ARE recursive by default.
	const recursive = " /dev/shm ro,"

	for _, tc := range []struct {
		clientVersion string
		expectedOut   string
		name          string
	}{
		{clientVersion: "", expectedOut: recursive, name: "latest should be the same as 1.44"},
		{clientVersion: "1.44", expectedOut: recursive, name: "submount should be recursive by default on 1.44"},

		{clientVersion: "1.43", expectedOut: nonRecursive, name: "older than 1.44 should be non-recursive by default"},

		// TODO: Remove when DefaultMinAPIVersion >= 1.44
		{clientVersion: config.MinAPIVersion, expectedOut: nonRecursive, name: "minimum API should be non-recursive by default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apiClient := testEnv.APIClient()

			minDaemonVersion := tc.clientVersion
			if minDaemonVersion == "" {
				minDaemonVersion = "1.44"
			}
			skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), minDaemonVersion), "requires API v"+minDaemonVersion)

			if tc.clientVersion != "" {
				c, err := client.New(client.FromEnv, client.WithAPIVersion(tc.clientVersion))
				assert.NilError(t, err, "failed to create client with version v%s", tc.clientVersion)
				apiClient = c
			}

			for _, tc2 := range []struct {
				subname  string
				mountOpt func(*container.TestContainerConfig)
			}{
				{"mount", container.WithMount(mounttypes.Mount{
					Type:     mounttypes.TypeBind,
					Source:   "/dev",
					Target:   "/dev",
					ReadOnly: true,
				})},
				{"bind mount", container.WithBindRaw("/dev:/dev:ro")},
			} {
				t.Run(tc2.subname, func(t *testing.T) {
					cid := container.Run(ctx, t, apiClient, tc2.mountOpt,
						container.WithCmd("sh", "-c", "grep /dev/shm /proc/self/mountinfo"),
					)
					out, err := container.Output(ctx, apiClient, cid)
					assert.NilError(t, err)

					assert.Check(t, is.Equal(out.Stderr, ""))
					// Output should be either:
					// 545 526 0:160 / /dev/shm ro,nosuid,nodev,noexec,relatime shared:90 - tmpfs shm rw,size=65536k
					// or
					// 545 526 0:160 / /dev/shm rw,nosuid,nodev,noexec,relatime shared:90 - tmpfs shm rw,size=65536k
					assert.Check(t, is.Contains(out.Stdout, tc.expectedOut))
				})
			}
		})
	}
}

func TestContainerBindMountRecursivelyReadOnly(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.44"), "requires API v1.44")
	skip.If(t, testEnv.IsNonRootClient, "skipping test that requires root on client side")

	ctx := setupTest(t)

	// 0o777 for allowing rootless containers to write to this directory
	tmpDir1 := fs.NewDir(t, "tmpdir1", fs.WithMode(0o777),
		fs.WithDir("mnt", fs.WithMode(0o777)))
	defer tmpDir1.Remove()
	tmpDir1Mnt := filepath.Join(tmpDir1.Path(), "mnt")
	tmpDir2 := fs.NewDir(t, "tmpdir2", fs.WithMode(0o777),
		fs.WithFile("file", "should not be writable when recursively read only", fs.WithMode(0o666)))
	defer tmpDir2.Remove()

	if err := mount.Mount(tmpDir2.Path(), tmpDir1Mnt, "none", "bind"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mount.Unmount(tmpDir1Mnt); err != nil {
			t.Fatal(err)
		}
	}()

	rroSupported := isRROSupported()

	nonRecursiveVerifier := []string{`/bin/sh`, `-xc`, `touch /foo/mnt/file; [ $? = 0 ]`}
	forceRecursiveVerifier := []string{`/bin/sh`, `-xc`, `touch /foo/mnt/file; [ $? != 0 ]`}

	// ro (recursive if kernel >= 5.12)
	ro := mounttypes.Mount{
		Type:     mounttypes.TypeBind,
		Source:   tmpDir1.Path(),
		Target:   "/foo",
		ReadOnly: true,
		BindOptions: &mounttypes.BindOptions{
			Propagation: mounttypes.PropagationRPrivate,
		},
	}
	roAsStr := ro.Source + ":" + ro.Target + ":ro,rprivate"
	roVerifier := nonRecursiveVerifier
	if rroSupported {
		roVerifier = forceRecursiveVerifier
	}

	// Non-recursive
	nonRecursive := ro
	nonRecursive.BindOptions = &mounttypes.BindOptions{
		ReadOnlyNonRecursive: true,
		Propagation:          mounttypes.PropagationRPrivate,
	}

	// Force recursive
	forceRecursive := ro
	forceRecursive.BindOptions = &mounttypes.BindOptions{
		ReadOnlyForceRecursive: true,
		Propagation:            mounttypes.PropagationRPrivate,
	}

	apiClient := testEnv.APIClient()

	containers := []string{
		container.Run(ctx, t, apiClient, container.WithMount(ro), container.WithCmd(roVerifier...), container.WithSecurityOpt("label=disable")),
		container.Run(ctx, t, apiClient, container.WithBindRaw(roAsStr), container.WithCmd(roVerifier...), container.WithSecurityOpt("label=disable")),

		container.Run(ctx, t, apiClient, container.WithMount(nonRecursive), container.WithCmd(nonRecursiveVerifier...), container.WithSecurityOpt("label=disable")),
	}

	if rroSupported {
		containers = append(containers,
			container.Run(ctx, t, apiClient, container.WithMount(forceRecursive), container.WithCmd(forceRecursiveVerifier...), container.WithSecurityOpt("label=disable")),
		)
	}

	for _, c := range containers {
		poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, c))
	}
}

func isRROSupported() bool {
	return kernel.CheckKernelVersion(5, 12, 0)
}

func TestContainerBindMountIDMapped(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "id-mapped mounts require a rootful daemon")
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.57"), "requires API v1.57")
	skip.If(t, !kernel.CheckKernelVersion(5, 12, 0), "id-mapped mounts require kernel 5.12")

	ctx := setupTest(t)

	// The test runs as root, so the file is owned by uid/gid 0 on the
	// backing filesystem. With a "match-user" ID mapping, it appears owned
	// by the container's running user.
	tmpDir := fs.NewDir(t, "idmapped", fs.WithMode(0o755),
		fs.WithFile("file", "hello", fs.WithMode(0o644)))
	defer tmpDir.Remove()

	idMapped := mounttypes.Mount{
		Type:   mounttypes.TypeBind,
		Source: tmpDir.Path(),
		Target: "/foo",
		BindOptions: &mounttypes.BindOptions{
			IDMapping: &mounttypes.IDMapping{Source: mounttypes.IDMappingSourceMatchUser},
		},
	}
	mappedVerifier := []string{`/bin/sh`, `-xc`, `[ "$(stat -c %u:%g /foo/file)" = "1234:1234" ] && echo written-by-1234 > /foo/new.txt`}

	// Without an IDMapping the same file keeps its host ownership: uid/gid 0,
	// or the overflow ID when the daemon runs with userns-remap, as the
	// host-owned file is not covered by the container's user namespace.
	plain := mounttypes.Mount{
		Type:   mounttypes.TypeBind,
		Source: tmpDir.Path(),
		Target: "/foo",
	}
	plainOwner := "0:0"
	if testEnv.IsUserNamespace() {
		plainOwner = "65534:65534"
	}
	plainVerifier := []string{`/bin/sh`, `-xc`, `[ "$(stat -c %u:%g /foo/file)" = "` + plainOwner + `" ]`}

	apiClient := testEnv.APIClient()

	// Start the id-mapped container by hand: kernel support for id-mapped
	// mounts depends on the filesystem backing the test directory, which
	// cannot be probed from a kernel version alone.
	idMappedCtr := container.Create(ctx, t, apiClient, container.WithMount(idMapped), container.WithUser("1234:1234"), container.WithCmd(mappedVerifier...), container.WithSecurityOpt("label=disable"))
	if _, err := apiClient.ContainerStart(ctx, idMappedCtr, client.ContainerStartOptions{}); err != nil {
		if strings.Contains(err.Error(), "MOUNT_ATTR_IDMAP") {
			t.Skipf("filesystem backing the test directory does not support id-mapped mounts: %v", err)
		}
		t.Fatal(err)
	}

	containers := []string{
		idMappedCtr,
		container.Run(ctx, t, apiClient, container.WithMount(plain), container.WithCmd(plainVerifier...), container.WithSecurityOpt("label=disable")),
	}
	for _, c := range containers {
		poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, c))
	}

	// The file created by the container user through the mount is owned by
	// the mount source's owner on the backing filesystem.
	fi, err := os.Stat(tmpDir.Join("new.txt"))
	assert.NilError(t, err)
	st := fi.Sys().(*syscall.Stat_t)
	assert.Check(t, is.Equal(st.Uid, uint32(0)))
	assert.Check(t, is.Equal(st.Gid, uint32(0)))
}
