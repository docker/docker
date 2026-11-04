//go:build linux

package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"github.com/moby/sys/mount"
	"github.com/moby/sys/mountinfo"
	"github.com/vishvananda/netlink"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

//nolint:dupword
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

//nolint:dupword
const mountsFixtureOverlay2 = `23 28 0:22 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
24 28 0:4 / /proc rw,nosuid,nodev,noexec,relatime shared:13 - proc proc rw
25 28 0:6 / /dev rw,nosuid,relatime shared:2 - devtmpfs udev rw,size=491380k,nr_inodes=122845,mode=755
26 25 0:23 / /dev/pts rw,nosuid,noexec,relatime shared:3 - devpts devpts rw,gid=5,mode=620,ptmxmode=000
27 28 0:24 / /run rw,nosuid,noexec,relatime shared:5 - tmpfs tmpfs rw,size=100884k,mode=755
28 0 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw,data=ordered
29 23 0:7 / /sys/kernel/security rw,nosuid,nodev,noexec,relatime shared:8 - securityfs securityfs rw
30 25 0:25 / /dev/shm rw,nosuid,nodev shared:4 - tmpfs tmpfs rw
31 27 0:26 / /run/lock rw,nosuid,nodev,noexec,relatime shared:6 - tmpfs tmpfs rw,size=5120k
32 23 0:27 / /sys/fs/cgroup ro,nosuid,nodev,noexec shared:9 - tmpfs tmpfs ro,mode=755
33 32 0:28 / /sys/fs/cgroup/unified rw,nosuid,nodev,noexec,relatime shared:10 - cgroup2 cgroup rw
34 32 0:29 / /sys/fs/cgroup/systemd rw,nosuid,nodev,noexec,relatime shared:11 - cgroup cgroup rw,xattr,name=systemd
35 23 0:30 / /sys/fs/pstore rw,nosuid,nodev,noexec,relatime shared:12 - pstore pstore rw
36 32 0:31 / /sys/fs/cgroup/blkio rw,nosuid,nodev,noexec,relatime shared:14 - cgroup cgroup rw,blkio
37 32 0:32 / /sys/fs/cgroup/memory rw,nosuid,nodev,noexec,relatime shared:15 - cgroup cgroup rw,memory
38 32 0:33 / /sys/fs/cgroup/hugetlb rw,nosuid,nodev,noexec,relatime shared:16 - cgroup cgroup rw,hugetlb
39 32 0:34 / /sys/fs/cgroup/freezer rw,nosuid,nodev,noexec,relatime shared:17 - cgroup cgroup rw,freezer
40 32 0:35 / /sys/fs/cgroup/perf_event rw,nosuid,nodev,noexec,relatime shared:18 - cgroup cgroup rw,perf_event
41 32 0:36 / /sys/fs/cgroup/pids rw,nosuid,nodev,noexec,relatime shared:19 - cgroup cgroup rw,pids
42 32 0:37 / /sys/fs/cgroup/cpuset rw,nosuid,nodev,noexec,relatime shared:20 - cgroup cgroup rw,cpuset
43 32 0:38 / /sys/fs/cgroup/cpu,cpuacct rw,nosuid,nodev,noexec,relatime shared:21 - cgroup cgroup rw,cpu,cpuacct
44 32 0:39 / /sys/fs/cgroup/rdma rw,nosuid,nodev,noexec,relatime shared:22 - cgroup cgroup rw,rdma
45 32 0:40 / /sys/fs/cgroup/devices rw,nosuid,nodev,noexec,relatime shared:23 - cgroup cgroup rw,devices
46 32 0:41 / /sys/fs/cgroup/net_cls,net_prio rw,nosuid,nodev,noexec,relatime shared:24 - cgroup cgroup rw,net_cls,net_prio
47 24 0:42 / /proc/sys/fs/binfmt_misc rw,relatime shared:25 - autofs systemd-1 rw,fd=33,pgrp=1,timeout=0,minproto=5,maxproto=5,direct,pipe_ino=11725
48 23 0:8 / /sys/kernel/debug rw,relatime shared:26 - debugfs debugfs rw
49 25 0:19 / /dev/mqueue rw,relatime shared:27 - mqueue mqueue rw
50 25 0:43 / /dev/hugepages rw,relatime shared:28 - hugetlbfs hugetlbfs rw,pagesize=2M
80 23 0:20 / /sys/kernel/config rw,relatime shared:29 - configfs configfs rw
82 23 0:44 / /sys/fs/fuse/connections rw,relatime shared:30 - fusectl fusectl rw
84 28 252:15 / /boot/efi rw,relatime shared:31 - vfat /dev/vda15 rw,fmask=0022,dmask=0022,codepage=437,iocharset=iso8859-1,shortname=mixed,errors=remount-ro
391 28 0:49 / /var/lib/lxcfs rw,nosuid,nodev,relatime shared:208 - fuse.lxcfs lxcfs rw,user_id=0,group_id=0,allow_other
401 48 0:11 / /sys/kernel/debug/tracing rw,relatime shared:213 - tracefs tracefs rw
421 47 0:93 / /proc/sys/fs/binfmt_misc rw,relatime shared:223 - binfmt_misc binfmt_misc rw
510 27 0:3 net:[4026531993] /run/docker/netns/default rw shared:255 - nsfs nsfs rw
60 27 0:3 net:[4026532265] /run/docker/netns/ingress_sbox rw shared:40 - nsfs nsfs rw
162 27 0:3 net:[4026532331] /run/docker/netns/1-bj0aarwy1n rw shared:41 - nsfs nsfs rw
450 28 0:51 / /var/lib/docker/overlay2/3a4b807fcb98c208573f368c5654a6568545a7f92404a07d0045eb5c85acaf67/merged rw,relatime shared:231 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/E6KNVZ2QUCIXY5VT7E5LO3PVCA:/var/lib/docker/overlay2/l/64XI57TRGG6QS4K6DCSREZXBN2:/var/lib/docker/overlay2/l/TWXZ4ANJR6BDLDZMWZ4Y6AICAR:/var/lib/docker/overlay2/l/VRLSNSG3PKZELC5O66TVTQ7EH5:/var/lib/docker/overlay2/l/HOLV4F57X56TRLVACMRLFVW7YD:/var/lib/docker/overlay2/l/JJQFBBBT6LWLQS35XBADV6BLAM:/var/lib/docker/overlay2/l/FZTPKHZGP2Z6DBPFEEL2IK3I5Y,upperdir=/var/lib/docker/overlay2/3a4b807fcb98c208573f368c5654a6568545a7f92404a07d0045eb5c85acaf67/diff,workdir=/var/lib/docker/overlay2/3a4b807fcb98c208573f368c5654a6568545a7f92404a07d0045eb5c85acaf67/work
569 27 0:3 net:[4026532353] /run/docker/netns/7de1071d0d8b rw shared:245 - nsfs nsfs rw
245 27 0:50 / /run/user/0 rw,nosuid,nodev,relatime shared:160 - tmpfs tmpfs rw,size=100880k,mode=700
482 28 0:69 / /var/lib/docker/overlay2/df4ee7b0bac7bda30e6e3d24a1153b288ebda50ffe68aae7ae0f38bc9286a01a/merged rw,relatime shared:250 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/CNZ3ATGGHMUTPPJBBU2OL4GLL6:/var/lib/docker/overlay2/l/64XI57TRGG6QS4K6DCSREZXBN2:/var/lib/docker/overlay2/l/TWXZ4ANJR6BDLDZMWZ4Y6AICAR:/var/lib/docker/overlay2/l/VRLSNSG3PKZELC5O66TVTQ7EH5:/var/lib/docker/overlay2/l/HOLV4F57X56TRLVACMRLFVW7YD:/var/lib/docker/overlay2/l/JJQFBBBT6LWLQS35XBADV6BLAM:/var/lib/docker/overlay2/l/FZTPKHZGP2Z6DBPFEEL2IK3I5Y,upperdir=/var/lib/docker/overlay2/df4ee7b0bac7bda30e6e3d24a1153b288ebda50ffe68aae7ae0f38bc9286a01a/diff,workdir=/var/lib/docker/overlay2/df4ee7b0bac7bda30e6e3d24a1153b288ebda50ffe68aae7ae0f38bc9286a01a/work
528 28 0:77 / /var/lib/docker/containers/404a7f860e600bfc144f7b5d9140d80bf3072fbb97659f98bc47039fd73d2695/mounts/shm rw,nosuid,nodev,noexec,relatime shared:260 - tmpfs shm rw,size=65536k
649 27 0:3 net:[4026532429] /run/docker/netns/7f85bc5ef3ba rw shared:265 - nsfs nsfs rw
`

func TestCleanupMounts(t *testing.T) {
	d := &Daemon{
		root: "/var/lib/docker/",
	}

	t.Run("aufs", func(t *testing.T) {
		const expected = "/var/lib/docker/containers/d045dc441d2e2e1d5b3e328d47e5943811a40819fb47497c5f5a5df2d6d13c37/shm"
		var unmounted int
		err := d.cleanupMountsFromReaderByID(strings.NewReader(mountsFixture), "", func(target string) error {
			if target == expected {
				unmounted++
			}
			return nil
		})
		assert.NilError(t, err)
		assert.Equal(t, unmounted, 1, "Expected to unmount the shm (and the shm only)")
	})

	t.Run("overlay2", func(t *testing.T) {
		const expected = "/var/lib/docker/containers/404a7f860e600bfc144f7b5d9140d80bf3072fbb97659f98bc47039fd73d2695/mounts/shm"
		var unmounted int
		err := d.cleanupMountsFromReaderByID(strings.NewReader(mountsFixtureOverlay2), "", func(target string) error {
			if target == expected {
				unmounted++
			}
			return nil
		})
		assert.NilError(t, err)
		assert.Equal(t, unmounted, 1, "Expected to unmount the shm (and the shm only)")
	})

	// Verify escaped mountpoint characters are decoded before matching and unmounting.
	t.Run("escaped mountpoint", func(t *testing.T) {
		const expected = "/var/lib/docker data/containers/404a7f860e600bfc144f7b5d9140d80bf3072fbb97659f98bc47039fd73d2695/mounts/shm"
		const mountInfo = `123 456 0:42 / /var/lib/docker\040data/containers/404a7f860e600bfc144f7b5d9140d80bf3072fbb97659f98bc47039fd73d2695/mounts/shm rw,relatime - tmpfs tmpfs rw` //nolint:dupword

		var unmounted string
		d2 := &Daemon{root: "/var/lib/docker data"}
		err := d2.cleanupMountsFromReaderByID(strings.NewReader(mountInfo), "", func(target string) error {
			unmounted = target
			return nil
		})
		assert.NilError(t, err)
		assert.Equal(t, unmounted, expected)
	})
}

func TestCleanupMountsByID(t *testing.T) {
	d := &Daemon{
		root: "/var/lib/docker/",
	}
	t.Run("overlay2", func(t *testing.T) {
		const mntID = "3a4b807fcb98c208573f368c5654a6568545a7f92404a07d0045eb5c85acaf67"
		const expected = "/var/lib/docker/overlay2/3a4b807fcb98c208573f368c5654a6568545a7f92404a07d0045eb5c85acaf67/merged"
		var unmounted int
		err := d.cleanupMountsFromReaderByID(strings.NewReader(mountsFixtureOverlay2), mntID, func(target string) error {
			if target == expected {
				unmounted++
			}
			return nil
		})
		assert.NilError(t, err)
		assert.Equal(t, unmounted, 1, "Expected to unmount the root (and that only)")
	})
}

func TestNotCleanupMounts(t *testing.T) {
	d := &Daemon{
		repository: "",
	}
	var unmounted int
	const mountInfo = `234 232 0:59 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k`
	err := d.cleanupMountsFromReaderByID(strings.NewReader(mountInfo), "", func(target string) error {
		unmounted++
		return nil
	})
	assert.NilError(t, err)
	assert.Equal(t, unmounted, 0, "Expected not to clean up /dev/shm")
}

func TestValidateContainerIsolationLinux(t *testing.T) {
	d := Daemon{}

	_, err := d.verifyContainerSettings(&configStore{}, &containertypes.HostConfig{Isolation: containertypes.IsolationHyperV}, nil, false)
	assert.Check(t, is.Error(err, "invalid isolation 'hyperv' on linux"))
}

func TestShouldUnmountRoot(t *testing.T) {
	for _, test := range []struct {
		desc   string
		root   string
		info   *mountinfo.Info
		expect bool
	}{
		{
			desc:   "root is at /",
			root:   "/docker",
			info:   &mountinfo.Info{Root: "/docker", Mountpoint: "/docker"},
			expect: true,
		},
		{
			desc:   "root is at in a submount from `/`",
			root:   "/foo/docker",
			info:   &mountinfo.Info{Root: "/docker", Mountpoint: "/foo/docker"},
			expect: true,
		},
		{
			desc:   "root is mounted in from a parent mount namespace same root dir", // dind is an example of this
			root:   "/docker",
			info:   &mountinfo.Info{Root: "/docker/volumes/1234657/_data", Mountpoint: "/docker"},
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
	mounted, err := mountinfo.Mounted(p)
	assert.Check(t, err)
	assert.Check(t, mounted == expect, "expected %v, actual %v", expect, mounted)
}

func TestRootMountCleanup(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("root required")
	}

	t.Parallel()

	testRoot, err := os.MkdirTemp("", t.Name())
	assert.NilError(t, err)
	defer os.RemoveAll(testRoot)
	cfg := &configStore{}

	err = mount.MakePrivate(testRoot)
	assert.NilError(t, err)
	defer mount.Unmount(testRoot)

	cfg.ExecRoot = filepath.Join(testRoot, "exec")
	cfg.Root = filepath.Join(testRoot, "daemon")

	err = os.Mkdir(cfg.ExecRoot, 0o755)
	assert.NilError(t, err)
	err = os.Mkdir(cfg.Root, 0o755)
	assert.NilError(t, err)

	d := &Daemon{root: cfg.Root}
	d.configStore.Store(cfg)
	unmountFile := getUnmountOnShutdownPath(&cfg.Config)

	t.Run("regular dir no mountpoint", func(t *testing.T) {
		err = setupDaemonRootPropagation(&cfg.Config)
		assert.NilError(t, err)
		_, err = os.Stat(unmountFile)
		assert.NilError(t, err)
		checkMounted(t, cfg.Root, true)

		assert.Assert(t, d.cleanupMounts(&cfg.Config))
		checkMounted(t, cfg.Root, false)

		_, err = os.Stat(unmountFile)
		assert.Assert(t, os.IsNotExist(err))
	})

	t.Run("root is a private mountpoint", func(t *testing.T) {
		err = mount.MakePrivate(cfg.Root)
		assert.NilError(t, err)
		defer mount.Unmount(cfg.Root)

		err = setupDaemonRootPropagation(&cfg.Config)
		assert.NilError(t, err)
		assert.Check(t, ensureShared(cfg.Root))

		_, err = os.Stat(unmountFile)
		assert.Assert(t, os.IsNotExist(err))
		assert.Assert(t, d.cleanupMounts(&cfg.Config))
		checkMounted(t, cfg.Root, true)
	})

	// mount is pre-configured with a shared mount
	t.Run("root is a shared mountpoint", func(t *testing.T) {
		err = mount.MakeShared(cfg.Root)
		assert.NilError(t, err)
		defer mount.Unmount(cfg.Root)

		err = setupDaemonRootPropagation(&cfg.Config)
		assert.NilError(t, err)

		if _, err := os.Stat(unmountFile); err == nil {
			t.Fatal("unmount file should not exist")
		}

		assert.Assert(t, d.cleanupMounts(&cfg.Config))
		checkMounted(t, cfg.Root, true)
		assert.Assert(t, mount.Unmount(cfg.Root))
	})

	// does not need mount but unmount file exists from previous run
	t.Run("old mount file is cleaned up on setup if not needed", func(t *testing.T) {
		err = mount.MakeShared(testRoot)
		assert.NilError(t, err)
		defer mount.MakePrivate(testRoot)
		err = os.WriteFile(unmountFile, nil, 0o644)
		assert.NilError(t, err)

		err = setupDaemonRootPropagation(&cfg.Config)
		assert.NilError(t, err)

		_, err = os.Stat(unmountFile)
		assert.Check(t, os.IsNotExist(err), err)
		checkMounted(t, cfg.Root, false)
		assert.Assert(t, d.cleanupMounts(&cfg.Config))
	})
}

func TestIfaceAddrs(t *testing.T) {
	CIDR := func(cidr string) *net.IPNet {
		t.Helper()
		nw, err := types.ParseCIDR(cidr)
		assert.NilError(t, err)
		return nw
	}

	for _, tt := range []struct {
		name string
		nws  []*net.IPNet
	}{
		{
			name: "Single",
			nws:  []*net.IPNet{CIDR("172.101.202.254/16")},
		},
		{
			name: "Multiple",
			nws: []*net.IPNet{
				CIDR("172.101.202.254/16"),
				CIDR("172.102.202.254/16"),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer netnsutils.SetupTestOSContext(t)()

			createBridge(t, "test", tt.nws...)

			ipv4Nw, err := ifaceAddrs("test", netlink.FAMILY_V4)
			if err != nil {
				t.Fatal(err)
			}
			ipv6Nw, err := ifaceAddrs("test", netlink.FAMILY_V6)
			if err != nil {
				t.Fatal(err)
			}

			ipnets := make([]*net.IPNet, len(ipv4Nw))
			for i := range ipv4Nw {
				ipnets[i] = ipv4Nw[i].IPNet
			}
			assert.Check(t, is.DeepEqual(ipnets, tt.nws,
				cmpopts.SortSlices(func(a, b *net.IPNet) bool { return a.String() < b.String() })))
			// IPv6 link-local address
			assert.Check(t, is.Len(ipv6Nw, 1))
		})
	}
}

func createBridge(t *testing.T, name string, bips ...*net.IPNet) {
	t.Helper()

	link := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
		},
	}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("Failed to create interface via netlink: %v", err)
	}
	for _, bip := range bips {
		if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: bip}); err != nil {
			t.Fatal(err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
}
