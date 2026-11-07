### Run tests

Contributing includes testing your changes. If you change the Moby code, you
may need to add a new test or modify an existing test. Your contribution could
even be adding tests to Moby. For this reason, you need to know a little
about Moby's test infrastructure.

This section describes tests you can run in the `dry-run-test` branch of your Docker
fork. If you have followed along in this guide, you already have this branch.
If you don't have this branch, you can create it or simply use another branch.

## Understand how to test Moby

Moby tests use the Go language's test framework. In this framework, files
whose names end in `_test.go` contain test code; you'll find test files like
this throughout the Moby repo. Use these files for inspiration when writing
your own tests. For information on Go's test framework, see <a
href="http://golang.org/pkg/testing/" target="_blank">Go's testing package
documentation</a> and the <a href="http://golang.org/cmd/go/#hdr-Test_packages"
target="_blank">go test help</a>.

You are responsible for _unit testing_ your contribution when you add new or
change existing Moby code. A unit test is a piece of code that invokes a
single, small piece of code (_unit of work_) to verify the unit works as
expected.

Depending on your contribution, you may need to add _integration tests_. These
are tests that combine two or more work units into one component. These work
units each have unit tests and then, together, integration tests that test the
interface between the components. The `integration` and `integration-cli`
directories in the Docker repository contain integration test code.  Note that
`integration-cli` tests are now deprecated in the Moby project, and new tests
cannot be added to this suite - add `integration` tests instead using the API
client.

Testing is its own specialty. If you aren't familiar with testing techniques,
there is a lot of information available to you on the Web. For now, you should
understand that, the Docker maintainers may ask you to write a new test or
change an existing one.

## Run tests on your local host

Before submitting a pull request with a code change, you should run the entire
Moby Engine test suite. The `Makefile` contains a target for the entire test
suite, named `test`. Also, it contains several targets for
testing:

| Target                 | What this target does                          |
| ---------------------- | ---------------------------------------------- |
| `test`                 | Run the unit, integration, and docker-py tests |
| `test-unit`            | Run just the unit tests                        |
| `test-integration`     | Run the integration tests                      |
| `test-docker-py`       | Run the tests for the Docker API client        |

Running the entire test suite on your current repository can take over half an
hour. To run the test suite, do the following:

1.  Open a terminal on your local host.

2.  Change to the root of your Docker repository.

    ```bash
    $ cd moby-fork
    ```

3.  Make sure you are in your development branch.

    ```bash
    $ git checkout dry-run-test
    ```

4.  Run the `make test` command.

    ```bash
    $ make test
    ```

    This command does several things, it creates a container temporarily for
    testing. Inside that container, the `make`:

    * creates a new binary
    * cross-compiles all the binaries for the various operating systems
    * runs all the tests in the system

    It can take approximately one hour to run all the tests. The time depends
    on your host performance. The default timeout is 60 minutes, which is
    defined in `hack/make.sh` (`${TIMEOUT:=60m}`). You can modify the timeout
    value on the basis of your host performance. When they complete
    successfully, you see the output concludes with something like this:

    ```none
    Ran 68 tests in 79.135s
    ```

## Run targets inside a development container

If you are working inside a development container, you use the
`hack/test/unit` script to run unit-tests, and `hack/make.sh` script to run
integration and other tests. The `hack/make.sh` script doesn't
have a single target that runs all the tests. Instead, you provide a single
command line with multiple targets that does the same thing.

Try this now.

1.  Open a terminal and change to the `moby-fork` root.

2.  Start a Moby development image.

    If you are following along with this guide, you should have a
    `docker-dev:latest` image.

    ```bash
    $ docker run --privileged --rm -ti -v `pwd`:/go/src/github.com/docker/docker docker-dev:latest /bin/bash
    ```

3.  Run the unit tests using the `hack/test/unit` script.

    ```bash
    # hack/test/unit
    ```

4.  Run the tests using the `hack/make.sh` script.

    ```bash
    # hack/make.sh dynbinary binary test-integration test-docker-py
    ```

    The tests run just as they did within your local host.

    Of course, you can also run a subset of these targets too. For example, to run
    just the integration tests:

    ```bash
    # hack/make.sh dynbinary binary test-integration
    ```

    Most test targets require that you build these precursor targets first:
    `dynbinary binary`


## Run unit tests

We use golang standard [testing](https://golang.org/pkg/testing/)
package or [gocheck](https://labix.org/gocheck) for our unit tests.

You can use the `TESTDIRS` environment variable to run unit tests for
a single package.

```bash
$ TESTDIRS='github.com/moby/moby/v2/opts' make test-unit
```

You can also use the `TESTFLAGS` environment variable to run a single test. The
flag's value is passed as arguments to the `go test` command. For example, from
your local host you can run the `TestValidateIPAddress` test with this command:

```bash
$ TESTFLAGS='-test.run ^TestValidateIPAddress$' make test-unit
```

On unit tests, it's better to use `TESTFLAGS` in combination with
`TESTDIRS` to make it quicker to run a specific test.

```bash
$ TESTDIRS='github.com/moby/moby/v2/opts' TESTFLAGS='-test.run ^TestValidateIPAddress$' make test-unit
```

## Run integration tests

We use [gocheck](https://labix.org/gocheck) for our integration-cli tests.
You can use the `TESTFLAGS` environment variable to run a single test. The
flag's value is passed as arguments to the `go test` command. For example, from
your local host you can run the `TestDockerCLIBuildSuite` test with this command:

```bash
$ TESTFLAGS='-test.run TestDockerCLIBuildSuite' make test-integration
```

To run the same test inside your Docker development container, you do this:

```bash
# TESTFLAGS='-test.run TestDockerCLIBuildSuite' hack/make.sh binary test-integration
```

### Run integration tests directly in a Linux VM

The `vm` workflow runs the daemon and integration tests directly in a Lima
guest, using the guest's systemd, networking tools, and kernel. Docker is only
used on the build host to prepare the pinned runtime binaries, Go toolchain,
test reporter, and frozen test images:

```bash
docker buildx bake test-integration-deps
chmod 0755 bundles
```

BuildKit creates the parent `bundles/` directory with mode `0700` when it does
not exist. Make it accessible before copying the checkout so rootless daemons
and remapped container users can traverse the binary and data directories.

Build this target for the same architecture as the guest. Copy the contents of
`bundles/test-integration-deps/` into the guest's root directory, preserving the
ownership and permissions of existing guest directories. For example, with Lima:

```bash
tar -C bundles/test-integration-deps -c . | lima sudo tar -C / -x --no-same-owner --no-overwrite-dir
```

The local exporter creates its output directory with mode `0700` and assigns
files to the build host's user. The extraction flags keep this metadata from
overwriting the guest's `/` directory and install the dependencies as root.

Provision a dedicated RHEL-compatible or SUSE-compatible guest with the scripts in
[`hack/host/`](../../hack/host/). They load kernel modules, install native build
and runtime packages, and set up the rootless test account and cgroup delegation.
Run them from the build host:

```bash
lima sudo bash < hack/host/load-kernel-modules.sh
lima sudo bash < hack/host/provision.sh
```

Copy the source checkout into a writable directory in the guest. Then, from the
guest's source directory, run:

```bash
sudo git config --global --add safe.directory "$PWD"
sudo env \
  PATH=/usr/local/go/bin:/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin \
  DOCKER_GITCOMMIT="$(git rev-parse HEAD)" \
  TEST_SKIP_INTEGRATION_CLI=1 \
  TEST_INTEGRATION_DIR='./integration/container ./integration/build ./integration/system' \
  TEST_INTEGRATION_USE_GRAPHDRIVER=1 \
  DOCKER_GRAPHDRIVER=overlay2 \
  TIMEOUT=15m \
  hack/make.sh dynbinary test-integration
```

Use a dedicated test VM with no other Docker daemon running: tests start
additional daemons and modify host networking and other system configuration.
For rootless coverage, add `DOCKER_ROOTLESS=1` to the command. The test runner
still runs as root and starts the rootless daemons as `unprivilegeduser`, which
the provisioning script creates. Use `DOCKER_GRAPHDRIVER=fuse-overlayfs` when
the kernel predates 5.11. Reports and daemon logs are
written under `bundles/test-integration/`.

## Test the Windows binary against a Linux daemon

This explains how to test the Windows binary on a Windows machine set up as a
development environment. The tests will be run against a daemon
running on a remote Linux machine. You'll use **Git Bash** that came with the
Git for Windows installation. **Git Bash**, just as it sounds, allows you to
run a Bash terminal on Windows.

1.  If you don't have one open already, start a Git Bash terminal.

    ![Git Bash](images/git_bash.png)

2.  Change to the `moby` source directory.

    ```bash
    $ cd /c/gopath/src/github.com/docker/docker
    ```

3.  Set `DOCKER_REMOTE_DAEMON` as follows:

    ```bash
    $ export DOCKER_REMOTE_DAEMON=1
    ```

4.  Set `DOCKER_TEST_HOST` to the `tcp://IP_ADDRESS:2376` value; substitute your
    Linux machine's actual IP address. For example:

    ```bash
    $ export DOCKER_TEST_HOST=tcp://213.124.23.200:2376
    ```

5.  Make the binary and run the tests:

    ```bash
    $ hack/make.sh binary test-integration
    ```
    Some tests are skipped on Windows for various reasons. You can see which
    tests were skipped by re-running the make and passing in the
   `TESTFLAGS='-test.v'` value. For example

    ```bash
    $ TESTFLAGS='-test.v' hack/make.sh binary test-integration
    ```

    Should you wish to run a single test such as one with the name
    'TestExample', you can pass in `TESTFLAGS='-test.run /TestExample'`. For
    example

    ```bash
    $ TESTFLAGS='-test.run /TestExample' hack/make.sh binary test-integration
    ```

You can now choose to make changes to the Moby source or the tests. If you
make any changes, just run these commands again.

## [Public CI infrastructure](ci.docker.com/public)

The current infrastructure is maintained here: [Moby ci job](https://ci.docker.com/public/job/moby).  The Jenkins infrastructure for the Moby project is maintained and
managed by Docker Inc.  All contributions against the Jenkinsfile are
appreciated and welcomed!  However we might not be able to fully provide the
infrastructure to test against various architectures in our CI pipelines.  All
jobs can be triggered and re-ran by the Moby maintainers

## Where to go next

Congratulations, you have successfully completed the basics you need to
understand the Moby test framework.
In the next section you'll [learn how to debug Docker daemon, running inside the development container](debug.md).
