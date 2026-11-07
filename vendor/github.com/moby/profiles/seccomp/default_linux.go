// SPDX-FileCopyrightText: Copyright The Moby Authors
// SPDX-License-Identifier: Apache-2.0

package seccomp

import (
	"slices"

	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// These are the Linux socket domains currently defined by the UAPI:
// https://github.com/torvalds/linux/blob/master/include/linux/socket.h
// AF_UNSPEC is not a creatable domain, AF_MAX is a sentinel, and AF_LOCAL/AF_FILE
// and AF_ROUTE are aliases of AF_UNIX and AF_NETLINK respectively.
// A commented-out entry is not allowed: either the kernel no longer implements
// the domain, and the comment links the upstream commit that removed it, or the
// domain is deliberately blocked, and the comment says why. Keeping them listed
// makes this list a complete mirror of the UAPI header.
var allowedSocketDomains = []uint64{
	unix.AF_UNIX,
	unix.AF_INET,

	// AF_AX25 was removed in Linux 7.1 together with the rest of the amateur
	// radio subsystem:
	// https://git.kernel.org/torvalds/c/dd8d4bc28ad7252610d8e79c1313a2d1e3499a51
	//
	// unix.AF_AX25,

	// AF_IPX was removed in Linux 4.18:
	// https://git.kernel.org/torvalds/c/7a2e838d28cff6718a0bdf66164465402f8e40ed
	//
	// unix.AF_IPX,

	// AF_APPLETALK was removed in Linux 7.2:
	// https://git.kernel.org/torvalds/c/8a398a0c189ead8bbce98f5be70b8ea0e30b21f8
	//
	// unix.AF_APPLETALK,

	// AF_NETROM was removed in Linux 7.1 together with AF_AX25:
	// https://git.kernel.org/torvalds/c/dd8d4bc28ad7252610d8e79c1313a2d1e3499a51
	//
	// unix.AF_NETROM,

	unix.AF_BRIDGE,
	unix.AF_ATMPVC,
	unix.AF_X25,
	unix.AF_INET6,

	// AF_ROSE was removed in Linux 7.1 together with AF_AX25:
	// https://git.kernel.org/torvalds/c/dd8d4bc28ad7252610d8e79c1313a2d1e3499a51
	//
	// unix.AF_ROSE,

	// AF_DECnet was removed in Linux 6.1:
	// https://git.kernel.org/torvalds/c/1202cdd665315c525b5237e96e0bedc76d7e754f
	//
	// unix.AF_DECnet,

	unix.AF_NETBEUI,
	unix.AF_SECURITY,
	unix.AF_KEY,
	unix.AF_NETLINK,
	unix.AF_PACKET,
	unix.AF_ASH,

	// AF_ECONET was removed in Linux 3.5:
	// https://git.kernel.org/torvalds/c/349f29d841dbae854bd7367be7c250401f974f47
	//
	// unix.AF_ECONET,

	// AF_ATMSVC was removed in Linux 7.2:
	// https://git.kernel.org/torvalds/c/aa582dc25ace8951ad595c71fb93d21ed2ec4624
	//
	// unix.AF_ATMSVC,

	unix.AF_RDS,
	unix.AF_SNA,

	// AF_IRDA was removed in Linux 4.17:
	// https://git.kernel.org/torvalds/c/d64c2a76123f0300b08d0557ad56e9d599872a36
	//
	// unix.AF_IRDA,

	unix.AF_PPPOX,

	// AF_WANPIPE has had no implementation since before Linux 2.6.12, the
	// start of the kernel's git history. The unbuilt leftover was deleted in
	// Linux 2.6.21:
	// https://git.kernel.org/torvalds/c/ce0ecd594d78710422599918a608e96dd1ee6024
	//
	// unix.AF_WANPIPE,

	unix.AF_LLC,
	unix.AF_IB,
	unix.AF_MPLS,
	unix.AF_CAN,
	unix.AF_TIPC,
	unix.AF_BLUETOOTH,
	unix.AF_IUCV,
	unix.AF_RXRPC,

	// AF_ISDN was removed in Linux 7.1 together with the rest of the ISDN
	// subsystem:
	// https://git.kernel.org/torvalds/c/4f10f1dfb235a28bd86cf0b00d86a59696ddbe5b
	//
	// unix.AF_ISDN,

	unix.AF_PHONET,
	unix.AF_IEEE802154,

	// AF_CAIF was removed in Linux 7.1:
	// https://git.kernel.org/torvalds/c/6d5431555de032f5ad9e08a7fb372f37bf493903
	//
	// unix.AF_CAIF,

	// AF_ALG gives userspace direct access to the kernel cryptography API. The
	// vulnerabilities demonstrated by https://copy.fail/ have been fixed, but
	// general-purpose containers have no practical need for this interface.
	// Keep it blocked to avoid exposing an unnecessary kernel attack surface.
	//
	// unix.AF_ALG,

	unix.AF_NFC,

	// AF_VSOCK provides host/guest communication. Before Linux 7.0 it was global
	// across network namespaces, allowing a container to reach any visible VM by
	// CID. Linux 7.0 added opt-in namespace isolation for vhost-vsock and
	// loopback, but global mode remains the default. Keep it blocked for older
	// kernels and default-global configurations; intentional users can provide a
	// custom seccomp profile. See https://docs.kernel.org/admin-guide/sysctl/net.html.
	//
	// unix.AF_VSOCK,

	unix.AF_KCM,
	unix.AF_QIPCRTR,
	unix.AF_SMC,
	unix.AF_XDP,
	unix.AF_MCTP,
}

func socketSyscalls() []*Syscall {
	// Keep range detection independent of the declaration order above.
	s := slices.Clone(allowedSocketDomains)
	slices.Sort(s)
	return socketSyscallsForDomains(s)
}

func socketSyscallsForDomains(domains []uint64) []*Syscall {
	syscalls := make([]*Syscall, 0, len(domains))
	// runc treats repeated comparisons for one argument as separate OR rules,
	// so bounded ranges cannot use both a lower and an upper comparison.
	// See https://github.com/opencontainers/runc/issues/2735.
	//
	// A one-sided range starting at AF_UNIX is safe because AF_UNSPEC is not a
	// creatable domain. Collapse it only when doing so removes equality rules.
	rangeEnd := 0
	if len(domains) > 1 && domains[0] == unix.AF_UNIX && domains[1] == domains[0]+1 {
		// The first domains are represented by the range instead of equalities.
		rangeEnd = 2
		lastDomain := domains[1]
		for _, domain := range domains[2:] {
			// Stop at the first gap so the range does not include a blocked domain.
			if domain != lastDomain+1 {
				break
			}
			lastDomain = domain
			rangeEnd++
		}
		// OpLessThan is exclusive, so compare with the value after the range.
		syscalls = append(syscalls, socketSyscall(lastDomain+1, specs.OpLessThan))
	}

	// Every later run is bounded, so preserve it with one equality per domain.
	for _, domain := range domains[rangeEnd:] {
		syscalls = append(syscalls, socketSyscall(domain, specs.OpEqualTo))
	}
	return syscalls
}

func socketSyscall(value uint64, op specs.LinuxSeccompOperator) *Syscall {
	return &Syscall{
		LinuxSyscall: specs.LinuxSyscall{
			Names:  []string{"socket"},
			Action: specs.ActAllow,
			Args: []specs.LinuxSeccompArg{
				{
					Index: 0,
					Value: value,
					Op:    op,
				},
			},
		},
	}
}

func arches() []Architecture {
	return []Architecture{
		{
			Arch:      specs.ArchX86_64,
			SubArches: []specs.Arch{specs.ArchX86, specs.ArchX32},
		},
		{
			Arch:      specs.ArchAARCH64,
			SubArches: []specs.Arch{specs.ArchARM},
		},
		{
			Arch:      specs.ArchMIPS64,
			SubArches: []specs.Arch{specs.ArchMIPS, specs.ArchMIPS64N32},
		},
		{
			Arch:      specs.ArchMIPS64N32,
			SubArches: []specs.Arch{specs.ArchMIPS, specs.ArchMIPS64},
		},
		{
			Arch:      specs.ArchMIPSEL64,
			SubArches: []specs.Arch{specs.ArchMIPSEL, specs.ArchMIPSEL64N32},
		},
		{
			Arch:      specs.ArchMIPSEL64N32,
			SubArches: []specs.Arch{specs.ArchMIPSEL, specs.ArchMIPSEL64},
		},
		{
			Arch:      specs.ArchS390X,
			SubArches: []specs.Arch{specs.ArchS390},
		},
		{
			Arch:      specs.ArchRISCV64,
			SubArches: nil,
		},
		{
			Arch:      specs.ArchLOONGARCH64,
			SubArches: nil,
		},
	}
}

// DefaultProfile defines the allowed syscalls for the default seccomp profile.
func DefaultProfile() *Seccomp {
	nosys := uint(unix.ENOSYS)
	syscalls := []*Syscall{
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"accept",
					"accept4",
					"access",
					"adjtimex",
					"alarm",
					"bind",
					"brk",
					"cachestat", // kernel v6.5, libseccomp v2.5.5
					"capget",
					"capset",
					"chdir",
					"chmod",
					"chown",
					"chown32",
					"clock_adjtime",
					"clock_adjtime64",
					"clock_getres",
					"clock_getres_time64",
					"clock_gettime",
					"clock_gettime64",
					"clock_nanosleep",
					"clock_nanosleep_time64",
					"close",
					"close_range",
					"connect",
					"copy_file_range",
					"creat",
					"dup",
					"dup2",
					"dup3",
					"epoll_create",
					"epoll_create1",
					"epoll_ctl",
					"epoll_ctl_old",
					"epoll_pwait",
					"epoll_pwait2",
					"epoll_wait",
					"epoll_wait_old",
					"eventfd",
					"eventfd2",
					"execve",
					"execveat",
					"exit",
					"exit_group",
					"faccessat",
					"faccessat2",
					"fadvise64",
					"fadvise64_64",
					"fallocate",
					"fanotify_mark",
					"fchdir",
					"fchmod",
					"fchmodat",
					"fchmodat2", // kernel v6.6, libseccomp v2.5.5
					"fchown",
					"fchown32",
					"fchownat",
					"fcntl",
					"fcntl64",
					"fdatasync",
					"fgetxattr",
					"flistxattr",
					"flock",
					"fork",
					"fremovexattr",
					"fsetxattr",
					"fstat",
					"fstat64",
					"fstatat64",
					"fstatfs",
					"fstatfs64",
					"fsync",
					"ftruncate",
					"ftruncate64",
					"futex",
					"futex_requeue", // kernel v6.7, libseccomp v2.5.5
					"futex_time64",
					"futex_wait", // kernel v6.7, libseccomp v2.5.5
					"futex_waitv",
					"futex_wake", // kernel v6.7, libseccomp v2.5.5
					"futimesat",
					"getcpu",
					"getcwd",
					"getdents",
					"getdents64",
					"getegid",
					"getegid32",
					"geteuid",
					"geteuid32",
					"getgid",
					"getgid32",
					"getgroups",
					"getgroups32",
					"getitimer",
					"getpeername",
					"getpgid",
					"getpgrp",
					"getpid",
					"getppid",
					"getpriority",
					"getrandom",
					"getresgid",
					"getresgid32",
					"getresuid",
					"getresuid32",
					"getrlimit",
					"get_robust_list",
					"getrusage",
					"getsid",
					"getsockname",
					"getsockopt",
					"get_thread_area",
					"gettid",
					"gettimeofday",
					"getuid",
					"getuid32",
					"getxattr",
					"getxattrat", // kernel v6.13, libseccomp v2.6.0
					"inotify_add_watch",
					"inotify_init",
					"inotify_init1",
					"inotify_rm_watch",
					"io_cancel",
					"ioctl",
					"io_destroy",
					"io_getevents",
					"io_pgetevents",
					"io_pgetevents_time64",
					"ioprio_get",
					"ioprio_set",
					"io_setup",
					"io_submit",
					"ipc",
					"kill",
					"landlock_add_rule",
					"landlock_create_ruleset",
					"landlock_restrict_self",
					"lchown",
					"lchown32",
					"lgetxattr",
					"link",
					"linkat",
					"listen",
					"listmount", // kernel v6.8, libseccomp v2.6.0
					"listxattr",
					"listxattrat", // kernel v6.13, libseccomp v2.6.0
					"llistxattr",
					"_llseek",
					"lremovexattr",
					"lseek",
					"lsetxattr",
					"lstat",
					"lstat64",
					"madvise",
					"map_shadow_stack", // kernel v6.6, libseccomp v2.5.5
					"membarrier",
					"memfd_create",
					"memfd_secret",
					"mincore",
					"mkdir",
					"mkdirat",
					"mknod",
					"mknodat",
					"mlock",
					"mlock2",
					"mlockall",
					"mmap",
					"mmap2",
					"mprotect",
					"mq_getsetattr",
					"mq_notify",
					"mq_open",
					"mq_timedreceive",
					"mq_timedreceive_time64",
					"mq_timedsend",
					"mq_timedsend_time64",
					"mq_unlink",
					"mremap",
					"mseal", // kernel v6.9, libseccomp v2.6.0
					"msgctl",
					"msgget",
					"msgrcv",
					"msgsnd",
					"msync",
					"munlock",
					"munlockall",
					"munmap",
					"name_to_handle_at",
					"nanosleep",
					"newfstatat",
					"_newselect",
					"open",
					"openat",
					"openat2",
					"pause",
					"pidfd_open",
					"pidfd_send_signal",
					"pipe",
					"pipe2",
					"pkey_alloc",
					"pkey_free",
					"pkey_mprotect",
					"poll",
					"ppoll",
					"ppoll_time64",
					"prctl",
					"pread64",
					"preadv",
					"preadv2",
					"prlimit64",
					"process_mrelease",
					"pselect6",
					"pselect6_time64",
					"pwrite64",
					"pwritev",
					"pwritev2",
					"read",
					"readahead",
					"readlink",
					"readlinkat",
					"readv",
					"recv",
					"recvfrom",
					"recvmmsg",
					"recvmmsg_time64",
					"recvmsg",
					"remap_file_pages",
					"removexattr",
					"removexattrat", // kernel v6.13, libseccomp v2.6.0
					"rename",
					"renameat",
					"renameat2",
					"restart_syscall",
					"riscv_hwprobe", // kernel v6.12, libseccomp v2.6.0
					"rmdir",
					"rseq",
					"rt_sigaction",
					"rt_sigpending",
					"rt_sigprocmask",
					"rt_sigqueueinfo",
					"rt_sigreturn",
					"rt_sigsuspend",
					"rt_sigtimedwait",
					"rt_sigtimedwait_time64",
					"rt_tgsigqueueinfo",
					"sched_getaffinity",
					"sched_getattr",
					"sched_getparam",
					"sched_get_priority_max",
					"sched_get_priority_min",
					"sched_getscheduler",
					"sched_rr_get_interval",
					"sched_rr_get_interval_time64",
					"sched_setaffinity",
					"sched_setattr",
					"sched_setparam",
					"sched_setscheduler",
					"sched_yield",
					"seccomp",
					"select",
					"semctl",
					"semget",
					"semop",
					"semtimedop",
					"semtimedop_time64",
					"send",
					"sendfile",
					"sendfile64",
					"sendmmsg",
					"sendmsg",
					"sendto",
					"setfsgid",
					"setfsgid32",
					"setfsuid",
					"setfsuid32",
					"setgid",
					"setgid32",
					"setgroups",
					"setgroups32",
					"setitimer",
					"setpgid",
					"setpriority",
					"setregid",
					"setregid32",
					"setresgid",
					"setresgid32",
					"setresuid",
					"setresuid32",
					"setreuid",
					"setreuid32",
					"setrlimit",
					"set_robust_list",
					"setsid",
					"setsockopt",
					"set_thread_area",
					"set_tid_address",
					"setuid",
					"setuid32",
					"setxattr",
					"setxattrat", // kernel v6.13, libseccomp v2.6.0
					"shmat",
					"shmctl",
					"shmdt",
					"shmget",
					"shutdown",
					"sigaltstack",
					"signalfd",
					"signalfd4",
					"sigprocmask",
					"sigreturn",
					"socketcall",
					"socketpair",
					"splice",
					"stat",
					"stat64",
					"statfs",
					"statfs64",
					"statmount", // kernel v6.8, libseccomp v2.6.0
					"statx",
					"symlink",
					"symlinkat",
					"sync",
					"sync_file_range",
					"syncfs",
					"sysinfo",
					"tee",
					"tgkill",
					"time",
					"timer_create",
					"timer_delete",
					"timer_getoverrun",
					"timer_gettime",
					"timer_gettime64",
					"timer_settime",
					"timer_settime64",
					"timerfd_create",
					"timerfd_gettime",
					"timerfd_gettime64",
					"timerfd_settime",
					"timerfd_settime64",
					"times",
					"tkill",
					"truncate",
					"truncate64",
					"ugetrlimit",
					"umask",
					"uname",
					"unlink",
					"unlinkat",
					"uretprobe", // kernel v6.11, libseccomp v2.6.0
					"utime",
					"utimensat",
					"utimensat_time64",
					"utimes",
					"vfork",
					"vmsplice",
					"wait4",
					"waitid",
					"waitpid",
					"write",
					"writev",
				},
				Action: specs.ActAllow,
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"process_vm_readv",
					"process_vm_writev",
					"ptrace",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				MinKernel: &KernelVersion{4, 8},
			},
		},
	}

	// Allow socket(2) for the address families listed in allowedSocketDomains.
	// On ABIs that use socketcall(2), the socket arguments are behind a pointer
	// and cannot be filtered by seccomp. Because socketcall(2) is allowed above,
	// this domain allow-list applies only to the direct socket syscall.
	syscalls = append(syscalls, socketSyscalls()...)
	syscalls = append(syscalls, []*Syscall{
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names:  []string{"personality"},
				Action: specs.ActAllow,
				Args: []specs.LinuxSeccompArg{
					{
						Index: 0,
						Value: 0x0,
						Op:    specs.OpEqualTo,
					},
				},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names:  []string{"personality"},
				Action: specs.ActAllow,
				Args: []specs.LinuxSeccompArg{
					{
						Index: 0,
						Value: 0x0008,
						Op:    specs.OpEqualTo,
					},
				},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names:  []string{"personality"},
				Action: specs.ActAllow,
				Args: []specs.LinuxSeccompArg{
					{
						Index: 0,
						Value: 0x20000,
						Op:    specs.OpEqualTo,
					},
				},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names:  []string{"personality"},
				Action: specs.ActAllow,
				Args: []specs.LinuxSeccompArg{
					{
						Index: 0,
						Value: 0x20008,
						Op:    specs.OpEqualTo,
					},
				},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names:  []string{"personality"},
				Action: specs.ActAllow,
				Args: []specs.LinuxSeccompArg{
					{
						Index: 0,
						Value: 0xffffffff,
						Op:    specs.OpEqualTo,
					},
				},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"sync_file_range2",
					"swapcontext",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Arches: []string{"ppc64le"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"arm_fadvise64_64",
					"arm_sync_file_range",
					"sync_file_range2",
					"breakpoint",
					"cacheflush",
					"set_tls",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Arches: []string{"arm", "arm64"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"arch_prctl",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Arches: []string{"amd64", "x32"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"modify_ldt",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Arches: []string{"amd64", "x32", "x86"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"s390_pci_mmio_read",
					"s390_pci_mmio_write",
					"s390_runtime_instr",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Arches: []string{"s390", "s390x"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"riscv_flush_icache",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Arches: []string{"riscv64"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"open_by_handle_at",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_DAC_READ_SEARCH"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"bpf",
					"clone",
					"clone3",
					"fanotify_init",
					"fsconfig",
					"fsmount",
					"fsopen",
					"fspick",
					"lookup_dcookie",
					"lsm_get_self_attr", // kernel v6.8, libseccomp v2.6.0
					"lsm_list_modules",  // kernel v6.8, libseccomp v2.6.0
					"lsm_set_self_attr", // kernel v6.8, libseccomp v2.6.0
					"mount",
					"mount_setattr",
					"move_mount",
					"open_tree",
					"perf_event_open",
					"quotactl",
					"quotactl_fd",
					"setdomainname",
					"sethostname",
					"setns",
					"syslog",
					"umount",
					"umount2",
					"unshare",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_ADMIN"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"clone",
				},
				Action: specs.ActAllow,
				Args: []specs.LinuxSeccompArg{
					{
						Index:    0,
						Value:    unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP,
						ValueTwo: 0,
						Op:       specs.OpMaskedEqual,
					},
				},
			},
			Excludes: &Filter{
				Caps:   []string{"CAP_SYS_ADMIN"},
				Arches: []string{"s390", "s390x"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"clone",
				},
				Action: specs.ActAllow,
				Args: []specs.LinuxSeccompArg{
					{
						Index:    1,
						Value:    unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP,
						ValueTwo: 0,
						Op:       specs.OpMaskedEqual,
					},
				},
			},
			Comment: "s390 parameter ordering for clone is different",
			Includes: &Filter{
				Arches: []string{"s390", "s390x"},
			},
			Excludes: &Filter{
				Caps: []string{"CAP_SYS_ADMIN"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"clone3",
				},
				Action:   specs.ActErrno,
				ErrnoRet: &nosys,
			},
			Excludes: &Filter{
				Caps: []string{"CAP_SYS_ADMIN"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"reboot",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_BOOT"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"chroot",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_CHROOT"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"delete_module",
					"init_module",
					"finit_module",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_MODULE"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"acct",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_PACCT"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"kcmp",
					"pidfd_getfd",
					"process_madvise",
					"process_vm_readv",
					"process_vm_writev",
					"ptrace",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_PTRACE"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"iopl",
					"ioperm",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_RAWIO"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"settimeofday",
					"stime",
					"clock_settime",
					"clock_settime64",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_TIME"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"vhangup",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_TTY_CONFIG"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"get_mempolicy",
					"mbind",
					"set_mempolicy",
					"set_mempolicy_home_node", // kernel v5.17, libseccomp v2.5.4
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYS_NICE"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"syslog",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_SYSLOG"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"bpf",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_BPF"},
			},
		},
		{
			LinuxSyscall: specs.LinuxSyscall{
				Names: []string{
					"perf_event_open",
				},
				Action: specs.ActAllow,
			},
			Includes: &Filter{
				Caps: []string{"CAP_PERFMON"},
			},
		},
	}...)

	errnoRet := uint(unix.EPERM)
	return &Seccomp{
		LinuxSeccomp: specs.LinuxSeccomp{
			DefaultAction:   specs.ActErrno,
			DefaultErrnoRet: &errnoRet,
		},
		ArchMap:  arches(),
		Syscalls: syscalls,
	}
}
