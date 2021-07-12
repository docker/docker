///usr/bin/env true; exec /usr/bin/env go run "$0" "$@"

package main

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/google/shlex"
)

const (
	// TESTFLAGS environment variable contains the test flags to add to all `go test` invocations in addition to the default ones.
	TESTFLAGS = "TESTFLAGS"

	// TESTDIRS environment variable contains the packages to test.
	TESTDIRS = "TESTDIRS"

	// TIMEOUT environment variable contains the test timeout to use.
	TIMEOUT = "TIMEOUT"

	// defaultTimeout represents the default test timeout value used if TIMEOUT is not set.
	defaultTimeout = "5m"

	// profilePath represents the path where the final test profile will be written.
	profilePath = "bundles/profile.out"
)

func init() {
	log.SetFlags(0)
}

// envOrDefault returns the value of environment variable env if it is set and def otherwise.
func envOrDefault(env, def string) string {
	if s, ok := os.LookupEnv(env); ok {
		return s
	}
	return def
}

// partitionStrings partitions strings based on the general predicate p.
func partitionStrings(p func(s string) bool, ss ...string) (t, f []string) {
	t, f = ss[:0:0], ss[:0:0]
	for _, s := range ss {
		if p(s) {
			t = append(t, s)
		} else {
			f = append(f, s)
		}
	}
	return t, f
}

// filterStrings filters strings based on the general predicate p.
func filterStrings(p func(s string) bool, ss ...string) []string {
	t := ss[:0:0]
	for _, s := range ss {
		if p(s) {
			t = append(t, s)
		}
	}
	return t
}

// pkgPath returns fully-qualified Go package name given a directory relative to repository root.
func pkgPath(dir string) string {
	const repo = "github.com/docker/docker"
	if dir == "" {
		return repo
	}
	return fmt.Sprintf("%s/%s", repo, dir)
}

// goTest executes `go test` over packages in pkgs.
// goTest returns the exit code of the executed `go test` command or go execution error.
func goTest(testFlags []string, sequential bool, coverprofile string, pkgs ...string) (int, error) {
	args := append([]string{
		"test",
		"-json",
		"-tags='netgo seccomp libdm_no_deferred_remove'",
		"-cover",
		"-covermode=atomic",
		fmt.Sprintf("-coverprofile=%s", coverprofile),
	}, testFlags...)
	if sequential {
		args = append(args, "-p=1")
	}
	cmd := exec.Command("go", append(args, pkgs...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	switch err := cmd.Run().(type) {
	case nil, *exec.ExitError:
		return cmd.ProcessState.ExitCode(), nil
	default:
		return 0, err
	}
}

// mergeProfiles writes profile lines from each profile in profiles to final test profile at path.
// mergeProfiles assumes that a valid profile file already exists at path.
// mergeProfiles assumes that all profiles use the same `covermode`.
func mergeProfiles(path string, profiles ...*os.File) error {
	// NOTE: This does not produce output identical to what would be generated if coverage was
	// directly computed over all packages using go testing framework.
	// In case that is an issue, at least the output would need to be sorted line-by-line.
	// Potentially, just functionality provided by "sort" package could be enough, otherwise
	// "golang.org/x/tools/cover" could be used to parse profiles if just sorting does not suffice.

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open profile at %q: %w", path, err)
	}
	for _, p := range profiles {
		sc := bufio.NewScanner(p)
		sc.Scan() // skip first line
		for sc.Scan() {
			if _, err := fmt.Fprintln(f, sc.Text()); err != nil {
				return fmt.Errorf("failed to write profile line to %q: %w", path, err)
			}
		}
		if err := sc.Err(); err != nil && err != io.EOF {
			return fmt.Errorf("failed to scan %q: %w", p.Name(), err)
		}
	}
	return nil
}

func main() {
	testDirBuf, err := exec.Command("go", "list", envOrDefault(TESTDIRS, "./...")).Output()
	if err != nil {
		log.Fatalf("Failed to list packages: %s", err)
	}

	testDirs := filterStrings(func(dir string) bool {
		return !strings.HasPrefix(dir, pkgPath("integration")) // exclude integration tests
	}, strings.Fields(string(testDirBuf))...)
	if len(testDirs) == 0 {
		log.Fatal("No packages to test")
	}

	// NOTE: libnetwork tests require sequential execution.
	// See https://github.com/moby/moby/issues/42458#issuecomment-873216754 for more info.

	libnetwork, rest := partitionStrings(func(dir string) bool {
		return strings.HasPrefix(dir, pkgPath("libnetwork"))
	}, testDirs...)

	// NOTE: `go list` outputs sorted list of packages, hence we can search with `sort.SearchStrings`.
	// NOTE(2):`docker-proxy` is `libnetwork/drivers/bridge` test dependency.
	bridgePkg := pkgPath("libnetwork/drivers/bridge")
	if i := sort.SearchStrings(libnetwork, bridgePkg); i < len(libnetwork) && libnetwork[i] == bridgePkg {
		// Ensure docker-proxy is available and install if it is not.
		if _, err := exec.LookPath("docker-proxy"); err != nil {
			switch err := err.(type) {
			case *exec.Error:
				if err.Err != exec.ErrNotFound {
					log.Fatalf("Failed to lookup `docker-proxy` executable in `PATH`: %s", err)
				}
			default:
				log.Fatalf("Unknown error returned by `exec.LookPath`: %s", err)
			}
			if err := exec.Command("hack/make.sh", "binary-proxy", "install-proxy").Run(); err != nil {
				log.Fatalf("Failed to install `docker-proxy`: %s", err)
			}
		}
	}

	testFlags := []string{
		fmt.Sprintf("%s=%s", "-test.timeout", envOrDefault(TIMEOUT, defaultTimeout)),
	}
	if s, ok := os.LookupEnv(TESTFLAGS); ok {
		flags, err := shlex.Split(s)
		if err != nil {
			log.Fatalf("Failed to parse `TESTFLAGS`: %s", err)
		}
		testFlags = append(testFlags, flags...)
	}

	logExecutionError := func(f func(string, ...interface{}), err error) {
		f("Failed to execute `go test`: %s", err)
	}
	mustExecute := func(c int, err error) int {
		if err != nil {
			logExecutionError(log.Fatalf, err)
		}
		return c
	}
	os.Exit(func() int {
		switch {
		case len(libnetwork) == 0:
			// No libnetwork tests required.
			return mustExecute(goTest(testFlags, false, profilePath, rest...))
		case len(rest) == 0:
			// Only libnetwork tests requested.
			return mustExecute(goTest(testFlags, true, profilePath, libnetwork...))
		}

		code := mustExecute(goTest(testFlags, false, profilePath, rest...))

		f, err := ioutil.TempFile("", "profile-libnetwork.out")
		if err != nil {
			log.Fatalf("Failed to create temporary file: %s", err)
		}
		tmpPath := f.Name()
		defer func() {
			if err := f.Close(); err != nil {
				log.Printf("Failed to close temporary file %q: %s", tmpPath, err)
			}
			if err := os.Remove(tmpPath); err != nil {
				log.Printf("Failed to remove temporary file %q: %s", tmpPath, err)
			}
		}()

		c, err := goTest(testFlags, true, tmpPath, libnetwork...)
		if err != nil {
			logExecutionError(log.Printf, err)
			return 1
		}

		if err := mergeProfiles(profilePath, f); err != nil {
			log.Printf("Failed to merge profiles: %s", err)
			return 1
		}

		if code == 0 {
			return c
		}
		return code
	}())
}
