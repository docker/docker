### Build and test Moby on Windows

This page explains how to get the software you need to build, test, and run the
Moby source code for Windows and setup the required software and services:

- Windows containers
- GitHub account
- Git

## Prerequisites

### 1. Windows Server 2016 or Windows 10 with all Windows updates applied

The major build number must be at least 14393. This can be confirmed, for example,
by running the following from an elevated PowerShell prompt - this sample output
is from a fully up to date machine as at mid-November 2016:


    PS C:\> $(gin).WindowsBuildLabEx
    14393.447.amd64fre.rs1_release_inmarket.161102-0100

### 2. Git for Windows (or another git client) must be installed

https://git-scm.com/download/win.

### 3. The machine must be configured to run containers

For example, by following the quick start guidance at
https://msdn.microsoft.com/en-us/virtualization/windowscontainers/quick_start/quick_start or https://github.com/docker/labs/blob/master/windows/windows-containers/README.md

### 4. If building in a Hyper-V VM

For Windows Server 2016 using Windows Server containers as the default option,
it is recommended you have at least 1GB of memory assigned;
For Windows 10 where Hyper-V Containers are employed, you should have at least
4GB of memory assigned.
Note also, to run Hyper-V containers in a VM, it is necessary to configure the VM
for nested virtualization.

## Usage

The following steps should be run from an elevated Windows PowerShell prompt.

>**Note**:  In a default installation of containers on Windows following the quick-start guidance at https://msdn.microsoft.com/en-us/virtualization/windowscontainers/quick_start/quick_start,
the `docker.exe` client must run elevated to be able to connect to the daemon).

### 1. Windows containers

To test and run the Windows Moby engine, you need a system that supports Windows Containers:

- Windows 10 Anniversary Edition
- Windows Server 2016 running in a VM, on bare metal or in the cloud

Check out the [getting started documentation](https://github.com/docker/labs/blob/master/windows/windows-containers/README.md) for details.

### 2. GitHub account

To contribute to the Docker project, you need a <a href="https://github.com" target="_blank">GitHub account</a>.
A free account is fine. All the Moby project repositories are public and visible to everyone.

This guide assumes that you have basic familiarity with Git and Github terminology
and usage.
Refer to [GitHub For Beginners: Don’t Get Scared, Get Started](http://readwrite.com/2013/09/30/understanding-github-a-journey-for-beginners-part-1/)
to get up to speed on Github.

### 3. Git

In PowerShell, run:

    Invoke-Webrequest "https://github.com/git-for-windows/git/releases/download/v2.7.2.windows.1/Git-2.7.2-64-bit.exe" -OutFile git.exe -UseBasicParsing
    Start-Process git.exe -ArgumentList '/VERYSILENT /SUPPRESSMSGBOXES /CLOSEAPPLICATIONS /DIR=c:\git\' -Wait
    setx /M PATH "$env:Path;c:\git\cmd"

You are now ready clone and build the Moby source code.

### 4. Clone Moby

In a new (to pick up the path change) PowerShell prompt, run:

    git clone https://github.com/moby/moby
    cd moby

This clones the main Moby repository. Check out [Moby Project](https://mobyproject.org)
to learn about the other software that powers the Moby platform.

### 5. Build and run

Create a builder-container with the Moby source code. You can change the source
code on your system and rebuild any time:

    docker build -t nativebuildimage -f .\Dockerfile.windows .
    docker build -t nativebuildimage -f Dockerfile.windows -m 2GB .  # (if using Hyper-V containers)

To build Moby, run:

    $DOCKER_GITCOMMIT=(git rev-parse --short HEAD)
    docker run --name binaries -e DOCKER_GITCOMMIT=$DOCKER_GITCOMMIT nativebuildimage hack\make.ps1 -Binary
    docker run --name binaries -e DOCKER_GITCOMMIT=$DOCKER_GITCOMMIT -m 2GB nativebuildimage hack\make.ps1 -Binary  # (if using Hyper-V containers)

Copy out the resulting Windows Moby Engine binary to `dockerd.exe` in the
current directory:

    docker cp binaries:C:\go\src\github.com\docker\docker\bundles\docker.exe docker.exe
    docker cp binaries:C:\go\src\github.com\docker\docker\bundles\dockerd.exe dockerd.exe

To test it, stop the system Docker daemon and start the one you just built:

    Stop-Service Docker
    .\dockerd.exe -D

The other make targets work too, to run unit tests try:
`docker run --rm docker-builder sh -c 'cd /c/go/src/github.com/docker/docker; hack/make.sh test-unit'`.

### 6. Remove the interim binaries container

_(Optional)_

    docker rm binaries

### 7. Remove the image

_(Optional)_

It may be useful to keep this image around if you need to build multiple times.
Then you can take advantage of the builder cache to have an image which has all
the components required to build the binaries already installed.

    docker rmi nativebuildimage

## Validation

The validation tests can only run directly on the host.
This is because they calculate information from the git repo, but the .git directory
is not passed into the image as it is excluded via `.dockerignore`.
Run the following from a Windows PowerShell prompt (elevation is not required):
(Note Go must be installed to run these tests)

    hack\make.ps1 -DCO -PkgImports -GoFormat

## Unit tests

To run unit tests, ensure you have created the nativebuildimage above.
Then run one of the following from an (elevated) Windows PowerShell prompt:

    docker run --rm nativebuildimage hack\make.ps1 -TestUnit
    docker run --rm -m 2GB nativebuildimage hack\make.ps1 -TestUnit  # (if using Hyper-V containers)

To run unit tests and binary build, ensure you have created the nativebuildimage above.
Then run one of the following from an (elevated) Windows PowerShell prompt:

    docker run nativebuildimage hack\make.ps1 -All
    docker run -m 2GB nativebuildimage hack\make.ps1 -All  # (if using Hyper-V containers)

## Windows limitations

Don't attempt to use a bind mount to pass a local directory as the bundles
target directory.
It does not work (golang attempts for follow a mapped folder incorrectly).
Instead, use docker cp as per the example.

`go.zip` is not removed from the image as it is used by the Windows CI servers
to ensure the host and image are running consistent versions of go.

Nanoserver support is a work in progress. Although the image will build if the
`FROM` statement is updated, it will not work when running autogen through `hack\make.ps1`.
It is suspected that the required GCC utilities (eg gcc, windres, windmc) silently
quit due to the use of console hooks which are not available.

The docker integration tests do not currently run in a container on Windows,
predominantly due to Windows not supporting privileged mode, so anything using a volume would fail.
They (along with the rest of the docker CI suite) can be run using
https://github.com/jhowardmsft/docker-w2wCIScripts/blob/master/runCI/Invoke-DockerCI.ps1.

## Where to go next

In the next section, you'll [learn how to set up and configure Git for
contributing to Moby](set-up-git.md).
