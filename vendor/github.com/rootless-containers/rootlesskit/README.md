# RootlessKit: Linux-native fakeroot using user namespaces

RootlessKit is a Linux-native implementation of "fake root" using  [`user_namespaces(7)`](http://man7.org/linux/man-pages/man7/user_namespaces.7.html).

The purpose of RootlessKit is to run [Docker and Kubernetes as an unprivileged user (known as "Rootless mode")](https://github.com/rootless-containers/usernetes), so as to protect the real root on the host from potential container-breakout attacks.

<!-- START doctoc generated TOC please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION, INSTEAD RE-RUN doctoc TO UPDATE -->


- [What RootlessKit actually does](#what-rootlesskit-actually-does)
- [Similar projects](#similar-projects)
- [Projects using RootlessKit](#projects-using-rootlesskit)
- [Setup](#setup)
  - [Requirements](#requirements)
  - [subuid](#subuid)
  - [sysctl](#sysctl)
- [Usage](#usage)
- [Full CLI options](#full-cli-options)
- [State directory](#state-directory)
- [Environment variables](#environment-variables)
- [Additional documents](#additional-documents)

<!-- END doctoc generated TOC please keep comment here to allow auto update -->

## What RootlessKit actually does

RootlessKit creates [`user_namespaces(7)`](http://man7.org/linux/man-pages/man7/user_namespaces.7.html) and [`mount_namespaces(7)`](http://man7.org/linux/man-pages/man7/mount_namespaces.7.html),
and executes [`newuidmap(1)`](http://man7.org/linux/man-pages/man1/newuidmap.1.html)/[`newgidmap(1)`](http://man7.org/linux/man-pages/man1/newgidmap.1.html) along with [`subuid(5)`](http://man7.org/linux/man-pages/man5/subuid.5.html) and [`subgid(5)`](http://man7.org/linux/man-pages/man5/subgid.5.html).

RootlessKit also supports isolating [`network_namespaces(7)`](http://man7.org/linux/man-pages/man7/network_namespaces.7.html) with userspace NAT using ["slirp"](./docs/network.md).
Kernel-mode NAT using SUID-enabled [`lxc-user-nic(1)`](https://linuxcontainers.org/lxc/manpages/man1/lxc-user-nic.1.html) is also experimentally supported.

## Similar projects

Tools based on `LD_PRELOAD` (not enough to run rootless containers and yet lacks support for static binaries):
* [`fakeroot`](https://wiki.debian.org/FakeRoot)

Tools based on `ptrace(2)` (not enough to run rootless containers and yet slow):
* [`fakeroot-ng`](https://fakeroot-ng.lingnu.com/)
* [`proot`](https://proot-me.github.io/)

Tools based on `user_namespaces(7)` (as in RootlessKit, but without support for `--copy-up`, `--net`, ...):
* [`unshare -r`](http://man7.org/linux/man-pages/man1/unshare.1.html)
* [`podman unshare`](https://github.com/containers/libpod/blob/master/docs/source/markdown/podman-unshare.1.md)
* [`become-root`](https://github.com/giuseppe/become-root)

## Projects using RootlessKit

Container engines:
* [Docker/Moby](https://get.docker.com/rootless)
* [Podman](https://podman.io/) (since Podman v1.8.0)
* [nerdctl](https://github.com/containerd/nerdctl): Docker-compatible CLI for containerd

Container image builders:
* [BuildKit](https://github.com/moby/buildkit): Next-generation `docker build` backend

Kubernetes distributions:
* [Usernetes](https://github.com/rootless-containers/usernetes): Docker & Kubernetes, installable under a non-root user's `$HOME`.
* [k3s](https://k3s.io/): Lightweight Kubernetes

## Setup

```console
$ go get github.com/rootless-containers/rootlesskit/cmd/rootlesskit
$ go get github.com/rootless-containers/rootlesskit/cmd/rootlessctl
```

or just run `make` to make binaries under `./bin` directory.

### Requirements

### subuid

* `newuidmap` and `newgidmap` need to be installed on the host. These commands are provided by the `uidmap` package on most distributions.

* `/etc/subuid` and `/etc/subgid` should contain more than 65536 sub-IDs. e.g. `penguin:231072:65536`. These files are automatically configured on most distributions.

```console
$ id -u
1001
$ whoami
penguin
$ grep "^$(whoami):" /etc/subuid
penguin:231072:65536
$ grep "^$(whoami):" /etc/subgid
penguin:231072:65536
```

See also https://rootlesscontaine.rs/getting-started/common/subuid/

### sysctl

Some distros setting up sysctl:

- Debian (excluding Ubuntu) and Arch:  `sudo sh -c "echo 1 > /proc/sys/kernel/unprivileged_userns_clone"`
- RHEL/CentOS 7 (excluding RHEL/CentOS 8): `sudo sh -c "echo 28633 > /proc/sys/user/max_user_namespaces"`

To persist sysctl configurations, edit `/etc/sysctl.conf` or add a file under `/etc/sysctl.d`.

See also https://rootlesscontaine.rs/getting-started/common/sysctl/

## Usage

Inside `rootlesskit bash`, your UID is mapped to 0 but it is not the real root:

```console
(host)$ rootlesskit bash
(rootlesskit)# id
uid=0(root) gid=0(root) groups=0(root),65534(nogroup)
(rootlesskit)# ls -l /etc/shadow
-rw-r----- 1 nobody nogroup 1050 Aug 21 19:02 /etc/shadow
(rootlesskit)# cat /etc/shadow
cat: /etc/shadow: Permission denied
```

Environment variables are kept untouched:

```console
(host)$ rootlesskit bash
(rootlesskit)# echo $USER
penguin
(rootlesskit)# echo $HOME
/home/penguin
(rootlesskit)# echo $XDG_RUNTIME_DIR
/run/user/1001
```

Filesystems can be isolated from the host with `--copy-up`:

```console
(host)$ rootlesskit --copy-up=/etc bash
(rootlesskit)# rm /etc/resolv.conf
(rootlesskit)# vi /etc/resolv.conf
```

You can even create network namespaces with [Slirp](./docs/network.md):

```console
(host)$ rootlesskit --copy-up=/etc --copy-up=/run --net=slirp4netns --disable-host-loopback bash
(rootleesskit)# ip netns add foo
...
```

## Full CLI options

```console
$ rootlesskit --help
NAME:
   rootlesskit - Linux-native fakeroot using user namespaces

USAGE:
   rootlesskit [global options] [arguments...]

VERSION:
   0.14.0-beta.0

DESCRIPTION:
   RootlessKit is a Linux-native implementation of "fake root" using user_namespaces(7).
   
   Web site: https://github.com/rootless-containers/rootlesskit
   
   Examples:
     # spawn a shell with a new user namespace and a mount namespace
     rootlesskit bash
   
     # make /etc writable
     rootlesskit --copy-up=/etc bash
   
     # set mount propagation to rslave
     rootlesskit --propagation=rslave bash
   
     # create a network namespace with slirp4netns, and expose 80/tcp on the namespace as 8080/tcp on the host
     rootlesskit --copy-up=/etc --net=slirp4netns --disable-host-loopback --port-driver=builtin -p 127.0.0.1:8080:80/tcp bash
   
   Note: RootlessKit requires /etc/subuid and /etc/subgid to be configured by the real root user.
   See https://rootlesscontaine.rs/getting-started/common/ .

OPTIONS:
  Misc:                          
    --debug                      debug mode (default: false)
    --help, -h                   show help (default: false)
    --version, -v                print the version (default: false)
                                 
  Mount:                         
    --copy-up value              mount a filesystem and copy-up the contents. e.g. "--copy-up=/etc" (typically required for non-host network)
    --copy-up-mode value         copy-up mode [tmpfs+symlink] (default: "tmpfs+symlink")
    --propagation value          mount propagation [rprivate, rslave] (default: "rprivate")
                                 
  Network:                       
    --net value                  network driver [host, slirp4netns, vpnkit, lxc-user-nic(experimental)] (default: "host")
    --mtu value                  MTU for non-host network (default: 65520 for slirp4netns, 1500 for others) (default: 0)
    --cidr value                 CIDR for slirp4netns network (default: 10.0.2.0/24)
    --ifname value               Network interface name (default: tap0 for slirp4netns and vpnkit, eth0 for lxc-user-nic)
    --disable-host-loopback      prohibit connecting to 127.0.0.1:* on the host namespace (default: false)
                                 
  Network [lxc-user-nic]:        
    --lxc-user-nic-binary value  path of lxc-user-nic binary for --net=lxc-user-nic (default: "/usr/lib/x86_64-linux-gnu/lxc/lxc-user-nic")
    --lxc-user-nic-bridge value  lxc-user-nic bridge name (default: "lxcbr0")
                                 
  Network [slirp4netns]:         
    --slirp4netns-binary value   path of slirp4netns binary for --net=slirp4netns (default: "slirp4netns")
    --slirp4netns-sandbox value  enable slirp4netns sandbox (experimental) [auto, true, false] (the default is planned to be "auto" in future) (default: "false")
    --slirp4netns-seccomp value  enable slirp4netns seccomp (experimental) [auto, true, false] (the default is planned to be "auto" in future) (default: "false")
                                 
  Network [vpnkit]:              
    --vpnkit-binary value        path of VPNKit binary for --net=vpnkit (default: "vpnkit")
                                 
  Port:                          
    --port-driver value          port driver for non-host network. [none, builtin, slirp4netns] (default: "none")
    --publish value, -p value    publish ports. e.g. "127.0.0.1:8080:80/tcp"
                                 
  Process:                       
    --pidns                      create a PID namespace (default: false)
    --cgroupns                   create a cgroup namespace (default: false)
    --utsns                      create a UTS namespace (default: false)
    --ipcns                      create an IPC namespace (default: false)
    --reaper value               enable process reaper. Requires --pidns. [auto,true,false] (default: "auto")
    --evacuate-cgroup2 value     evacuate processes into the specified subgroup. Requires --pidns and --cgroupns
                                 
  State:                         
    --state-dir value            state directory
                                 
```

## State directory

The following files will be created in the state directory, which can be specified with `--state-dir`:
* `lock`: lock file
* `child_pid`: decimal PID text that can be used for `nsenter(1)`.
* `api.sock`: REST API socket. See [`./docs/api.md`](./docs/api.md) and [`./docs/port.md`](./docs/port.md).

If `--state-dir` is not specified, RootlessKit creates a temporary state directory on `/tmp` and removes it on exit.

Undocumented files are subject to change.

## Environment variables

The following environment variables will be set for the child process:
* `ROOTLESSKIT_STATE_DIR` (since v0.3.0): absolute path to the state dir
* `ROOTLESSKIT_PARENT_EUID` (since v0.8.0): effective UID
* `ROOTLESSKIT_PARENT_EGID` (since v0.8.0): effective GID

Undocumented environment variables are subject to change.

## Additional documents
- [`./docs/network.md`](./docs/network.md): Networking (`--net`, `--mtu`, `--cidr`, `--disable-host-loopback`, `--slirp4netns-*`, ...)
- [`./docs/port.md`](./docs/port.md): Port forwarding (`--port-driver`, `-p`, ...)
- [`./docs/mount.md`](./docs/mount.md): Mount (`--propagation`, ...)
- [`./docs/process.md`](./docs/process.md): Process (`--pidns`, `--reaper`, `--cgroupns`, `--evacuate-cgroup2`, ...)
- [`./docs/api.md`](./docs/api.md): REST API
