# Dear Packager,

If you are looking to make Docker available on your favorite software
distribution, this document is for you. It summarizes the requirements for
building and running the Docker client and the Docker daemon.

## Package Name

If possible, your package should be called "docker". If that name is already
taken, a second choice is "docker-engine". Another possible choice is "docker.io".

## Official Build vs Distro Build

The Docker project maintains its own build and release toolchain. It is pretty
neat and entirely based on Docker (surprise!). This toolchain is the canonical
way to build Docker. We encourage you to give it a try, and if the circumstances
allow you to use it, we recommend that you do.

You might not be able to use the official build toolchain - usually because your
distribution has a toolchain and packaging policy of its own. We get it! Your
house, your rules. The rest of this document should give you the information you
need to package Docker your way, without denaturing it in the process.

## Build Dependencies

The Dockerfile contains the most up-to-date list of build-time dependencies.

### Go Dependencies

All Go dependencies are vendored under "./vendor". They are used by the official
build, so the source of truth for the current version of each dependency is
whatever is in "./vendor".

If you would rather (or must, due to distro policy) package these dependencies
yourself, take a look at "vendor.conf" for an easy-to-parse list of the
exact version for each.

## Stripping Binaries

Please, please, please do not strip any compiled binaries. This is really
important.

In our own testing, stripping the resulting binaries sometimes results in a
binary that appears to work, but more often causes random panics, segfaults, and
other issues. Even if the binary appears to work, please don't strip.

See the following quotes from Dave Cheney, which explain this position better
from the upstream Golang perspective.

### [go issue #5855, comment #3](https://code.google.com/p/go/issues/detail?id=5855#c3)

> Super super important: Do not strip go binaries or archives. It isn't tested,
> often breaks, and doesn't work.

### [launchpad golang issue #1200255, comment #8](https://bugs.launchpad.net/ubuntu/+source/golang/+bug/1200255/comments/8)

> To quote myself: "Please do not strip Go binaries, it is not supported, not
> tested, is often broken, and doesn't do what you want"
>
> To unpack that a bit
>
> * not supported, as in, we don't support it, and recommend against it when
>   asked
> * not tested, we don't test stripped binaries as part of the build CI process
> * is often broken, stripping a go binary will produce anywhere from no, to
>   subtle, to outright execution failure, see above

### [launchpad golang issue #1200255, comment #13](https://bugs.launchpad.net/ubuntu/+source/golang/+bug/1200255/comments/13)

> To clarify my previous statements.
>
> * I do not disagree with the debian policy, it is there for a good reason
> * Having said that, it stripping Go binaries doesn't work, and nobody is
>   looking at making it work, so there is that.
>
> Thanks for patching the build formula.

## Building Docker

Please use our build script ("./hack/make.sh") for compilation.

### `DOCKER_BUILDTAGS`

There are build tags for disabling graphdrivers, if necessary. By default,
support for all graphdrivers are built in.

To disable btrfs:
```bash
export DOCKER_BUILDTAGS='exclude_graphdriver_btrfs'
```

To disable devicemapper:
```bash
export DOCKER_BUILDTAGS='exclude_graphdriver_devicemapper'
```

To disable aufs:
```bash
export DOCKER_BUILDTAGS='exclude_graphdriver_aufs'
```

NOTE: if you need to set more than one build tag, space separate them:
```bash
export DOCKER_BUILDTAGS='apparmor exclude_graphdriver_aufs'
```

## System Dependencies

### Runtime Dependencies

To function properly, the Docker daemon needs the following software to be
installed and available at runtime:

* iptables version 1.4 or later
* procps (or similar provider of a "ps" executable)
* e2fsprogs version 1.4.12 or later (in use: mkfs.ext4, tune2fs)
* xfsprogs (in use: mkfs.xfs)
* XZ Utils version 4.9 or later
* pigz (optional)

Additionally, the Docker client needs the following software to be installed and
available at runtime:

* Git version 1.7 or later

### Kernel Requirements

The Docker daemon has very specific kernel requirements. Most pre-packaged
kernels already include the necessary options enabled. If you are building your
own kernel, you should check out `contrib/check-config.sh`.

Note that in client mode, there are no specific kernel requirements, and that
the client will even run on alternative platforms such as Mac OS X / Darwin.

### Optional Dependencies

Some of Docker's features are activated by using optional command-line flags or
by having support for them in the kernel or userspace. A few examples include:

* AUFS graph driver (requires AUFS patches/support enabled in the kernel, and at
  least the "auplink" utility from aufs-tools)
* BTRFS graph driver (requires BTRFS support enabled in the kernel)
* ZFS graph driver (requires userspace zfs-utils and a corresponding kernel module)
* Libseccomp to allow running seccomp profiles with containers

## Daemon Init Script

Docker expects to run as a daemon at machine startup. Your package will need to
include a script for your distro's process supervisor of choice. Be sure to
check out the "contrib/init" folder in case a suitable init script already
exists.

In general, Docker should be run as root, similar to the following:

```bash
dockerd
```

Generally, it is encouraged that additional configuration be placed in
`/etc/docker/daemon.json`.
