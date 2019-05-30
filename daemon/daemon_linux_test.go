// +build linux

package daemon // import "github.com/docker/docker/daemon"

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/pkg/mount"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

const mountsFixture = `142 78 0:38 / / rw,relatime - aufs none rw,si=573b861da0b3a05b,dio
143 142 0:60 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
144 142 0:67 / /dev rw,nosuid - tmpfs tmpfs rw,mode=755
145 144 0:78 / /dev/pts rw,nosuid,noexec,relatime - devpts devpts rw,gid=5,mode=620,ptmxmode=666
146 144 0:49 / /dev/mqueue rw,nosuid,nodev,noexec,relatime - mqueue mqueue rw
147 142 0:84 / /sys rw,nosuid,nodev,noexec,relatime - sysfs sysfs rw
148 147 0:86 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,mode=755
149 148 0:22 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/cpuset rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpuset
150 148 0:25 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/cpu rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpu
151 148 0:27 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/cpuacct rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpuacct
152 148 0:28 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/memory rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,memory
153 148 0:29 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/devices rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,devices
154 148 0:30 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/freezer rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,freezer
155 148 0:31 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/blkio rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,blkio
156 148 0:32 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/perf_event rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,perf_event
157 148 0:33 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/hugetlb rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,hugetlb
158 148 0:35 /docker/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a /sys/fs/cgroup/systemd rw,nosuid,nodev,noexec,relatime - cgroup systemd rw,name=systemd
159 142 8:4 /home/mlaventure/gopath /home/mlaventure/gopath rw,relatime - ext4 /dev/disk/by-uuid/d99e196c-1fc4-4b4f-bab9-9962b2b34e99 rw,errors=remount-ro,data=ordered
160 142 8:4 /var/lib/docker/volumes/9a428b651ee4c538130143cad8d87f603a4bf31b928afe7ff3ecd65480692b35/_data /var/lib/docker rw,relatime - ext4 /dev/disk/by-uuid/d99e196c-1fc4-4b4f-bab9-9962b2b34e99 rw,errors=remount-ro,data=ordered
164 142 8:4 /home/mlaventure/gopath/src/github.com/docker/docker /go/src/github.com/docker/docker rw,relatime - ext4 /dev/disk/by-uuid/d99e196c-1fc4-4b4f-bab9-9962b2b34e99 rw,errors=remount-ro,data=ordered
165 142 8:4 /var/lib/docker/containers/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/disk/by-uuid/d99e196c-1fc4-4b4f-bab9-9962b2b34e99 rw,errors=remount-ro,data=ordered
166 142 8:4 /var/lib/docker/containers/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a/hostname /etc/hostname rw,relatime - ext4 /dev/disk/by-uuid/d99e196c-1fc4-4b4f-bab9-9962b2b34e99 rw,errors=remount-ro,data=ordered
167 142 8:4 /var/lib/docker/containers/5425782a95e643181d8a485a2bab3c0bb21f51d7dfc03511f0e6fbf3f3aa356a/hosts /etc/hosts rw,relatime - ext4 /dev/disk/by-uuid/d99e196c-1fc4-4b4f-bab9-9962b2b34e99 rw,errors=remount-ro,data=ordered
168 144 0:39 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
169 144 0:12 /14 /dev/console rw,nosuid,noexec,relatime - devpts devpts rw,gid=5,mode=620,ptmxmode=000
83 147 0:10 / /sys/kernel/security rw,relatime - securityfs none rw
89 142 0:87 / /tmp rw,relatime - tmpfs none rw
97 142 0:60 / /run/docker/netns/default rw,nosuid,nodev,noexec,relatime - proc proc rw
100 160 8:4 /var/lib/docker/volumes/9a428b651ee4c538130143cad8d87f603a4bf31b928afe7ff3ecd65480692b35/_data/aufs /var/lib/docker/aufs rw,relatime - ext4 /dev/disk/by-uuid/d99e196c-1fc4-4b4f-bab9-9962b2b34e99 rw,errors=remount-ro,data=ordered
115 100 0:102 / /var/lib/docker/aufs/mnt/0ecda1c63e5b58b3d89ff380bf646c95cc980252cf0b52466d43619aec7c8432 rw,relatime - aufs none rw,si=573b861dbc01905b,dio
116 160 0:107 / /var/lib/docker/containers/d045dc441d2e2e1d5b3e328d47e5943811a40819fb47497c5f5a5df2d6d13c37/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
118 142 0:102 / /run/docker/libcontainerd/d045dc441d2e2e1d5b3e328d47e5943811a40819fb47497c5f5a5df2d6d13c37/rootfs rw,relatime - aufs none rw,si=573b861dbc01905b,dio
242 142 0:60 / /run/docker/netns/c3664df2a0f7 rw,nosuid,nodev,noexec,relatime - proc proc rw
120 100 0:122 / /var/lib/docker/aufs/mnt/03ca4b49e71f1e49a41108829f4d5c70ac95934526e2af8984a1f65f1de0715d rw,relatime - aufs none rw,si=573b861eb147805b,dio
171 142 0:122 / /run/docker/libcontainerd/e406ff6f3e18516d50e03dbca4de54767a69a403a6f7ec1edc2762812824521e/rootfs rw,relatime - aufs none rw,si=573b861eb147805b,dio
310 142 0:60 / /run/docker/netns/71a18572176b rw,nosuid,nodev,noexec,relatime - proc proc rw
`

func TestCleanupMounts(t *testing.T) {
	d := &Daemon{
		root: "/var/lib/docker/",
	}

	expected := "/var/lib/docker/containers/d045dc441d2e2e1d5b3e328d47e5943811a40819fb47497c5f5a5df2d6d13c37/shm"
	var unmounted int
	unmount := func(target string) error {
		if target == expected {
			unmounted++
		}
		return nil
	}

	d.cleanupMountsFromReaderByID(strings.NewReader(mountsFixture), "", unmount)

	if unmounted != 1 {
		t.Fatal("Expected to unmount the shm (and the shm only)")
	}
}

func TestCleanupMountsByID(t *testing.T) {
	d := &Daemon{
		root: "/var/lib/docker/",
	}

	expected := "/var/lib/docker/aufs/mnt/03ca4b49e71f1e49a41108829f4d5c70ac95934526e2af8984a1f65f1de0715d"
	var unmounted int
	unmount := func(target string) error {
		if target == expected {
			unmounted++
		}
		return nil
	}

	d.cleanupMountsFromReaderByID(strings.NewReader(mountsFixture), "03ca4b49e71f1e49a41108829f4d5c70ac95934526e2af8984a1f65f1de0715d", unmount)

	if unmounted != 1 {
		t.Fatal("Expected to unmount the auf root (and that only)")
	}
}

func TestNotCleanupMounts(t *testing.T) {
	d := &Daemon{
		repository: "",
	}
	var unmounted bool
	unmount := func(target string) error {
		unmounted = true
		return nil
	}
	mountInfo := `234 232 0:59 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k`
	d.cleanupMountsFromReaderByID(strings.NewReader(mountInfo), "", unmount)
	if unmounted {
		t.Fatal("Expected not to clean up /dev/shm")
	}
}

func TestValidateContainerIsolationLinux(t *testing.T) {
	d := Daemon{}

	_, err := d.verifyContainerSettings("linux", &containertypes.HostConfig{Isolation: containertypes.IsolationHyperV}, nil, false)
	assert.Check(t, is.Error(err, "invalid isolation 'hyperv' on linux"))
}

func TestShouldUnmountRoot(t *testing.T) {
	for _, test := range []struct {
		desc   string
		root   string
		info   *mount.Info
		expect bool
	}{
		{
			desc:   "root is at /",
			root:   "/docker",
			info:   &mount.Info{Root: "/docker", Mountpoint: "/docker"},
			expect: true,
		},
		{
			desc:   "root is at in a submount from `/`",
			root:   "/foo/docker",
			info:   &mount.Info{Root: "/docker", Mountpoint: "/foo/docker"},
			expect: true,
		},
		{
			desc:   "root is mounted in from a parent mount namespace same root dir", // dind is an example of this
			root:   "/docker",
			info:   &mount.Info{Root: "/docker/volumes/1234657/_data", Mountpoint: "/docker"},
			expect: false,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			for _, options := range []struct {
				desc     string
				Optional string
				expect   bool
			}{
				{desc: "shared", Optional: "shared:", expect: true},
				{desc: "slave", Optional: "slave:", expect: false},
				{desc: "private", Optional: "private:", expect: false},
			} {
				t.Run(options.desc, func(t *testing.T) {
					expect := options.expect
					if expect {
						expect = test.expect
					}
					if test.info != nil {
						test.info.Optional = options.Optional
					}
					assert.Check(t, is.Equal(expect, shouldUnmountRoot(test.root, test.info)))
				})
			}
		})
	}
}

func checkMounted(t *testing.T, p string, expect bool) {
	t.Helper()
	mounted, err := mount.Mounted(p)
	assert.Check(t, err)
	assert.Check(t, mounted == expect, "expected %v, actual %v", expect, mounted)
}

func TestRootMountCleanup(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("root required")
	}

	t.Parallel()

	testRoot, err := ioutil.TempDir("", t.Name())
	assert.NilError(t, err)
	defer os.RemoveAll(testRoot)
	cfg := &config.Config{}

	err = mount.MakePrivate(testRoot)
	assert.NilError(t, err)
	defer mount.Unmount(testRoot)

	cfg.ExecRoot = filepath.Join(testRoot, "exec")
	cfg.Root = filepath.Join(testRoot, "daemon")

	err = os.Mkdir(cfg.ExecRoot, 0755)
	assert.NilError(t, err)
	err = os.Mkdir(cfg.Root, 0755)
	assert.NilError(t, err)

	d := &Daemon{configStore: cfg, root: cfg.Root}
	unmountFile := getUnmountOnShutdownPath(cfg)

	t.Run("regular dir no mountpoint", func(t *testing.T) {
		err = setupDaemonRootPropagation(cfg)
		assert.NilError(t, err)
		_, err = os.Stat(unmountFile)
		assert.NilError(t, err)
		checkMounted(t, cfg.Root, true)

		assert.Assert(t, d.cleanupMounts())
		checkMounted(t, cfg.Root, false)

		_, err = os.Stat(unmountFile)
		assert.Assert(t, os.IsNotExist(err))
	})

	t.Run("root is a private mountpoint", func(t *testing.T) {
		err = mount.MakePrivate(cfg.Root)
		assert.NilError(t, err)
		defer mount.Unmount(cfg.Root)

		err = setupDaemonRootPropagation(cfg)
		assert.NilError(t, err)
		assert.Check(t, ensureShared(cfg.Root))

		_, err = os.Stat(unmountFile)
		assert.Assert(t, os.IsNotExist(err))
		assert.Assert(t, d.cleanupMounts())
		checkMounted(t, cfg.Root, true)
	})

	// mount is pre-configured with a shared mount
	t.Run("root is a shared mountpoint", func(t *testing.T) {
		err = mount.MakeShared(cfg.Root)
		assert.NilError(t, err)
		defer mount.Unmount(cfg.Root)

		err = setupDaemonRootPropagation(cfg)
		assert.NilError(t, err)

		if _, err := os.Stat(unmountFile); err == nil {
			t.Fatal("unmount file should not exist")
		}

		assert.Assert(t, d.cleanupMounts())
		checkMounted(t, cfg.Root, true)
		assert.Assert(t, mount.Unmount(cfg.Root))
	})

	// does not need mount but unmount file exists from previous run
	t.Run("old mount file is cleaned up on setup if not needed", func(t *testing.T) {
		err = mount.MakeShared(testRoot)
		assert.NilError(t, err)
		defer mount.MakePrivate(testRoot)
		err = ioutil.WriteFile(unmountFile, nil, 0644)
		assert.NilError(t, err)

		err = setupDaemonRootPropagation(cfg)
		assert.NilError(t, err)

		_, err = os.Stat(unmountFile)
		assert.Check(t, os.IsNotExist(err), err)
		checkMounted(t, cfg.Root, false)
		assert.Assert(t, d.cleanupMounts())
	})

}
