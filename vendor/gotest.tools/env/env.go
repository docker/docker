/*Package env provides functions to test code that read environment variables
or the current working directory.
*/
package env // import "gotest.tools/env"

import (
	"os"
	"strings"

	"gotest.tools/assert"
	"gotest.tools/x/subtest"
)

type helperT interface {
	Helper()
}

// Patch changes the value of an environment variable, and returns a
// function which will reset the the value of that variable back to the
// previous state.
func Patch(t assert.TestingT, key, value string) func() {
	if ht, ok := t.(helperT); ok {
		ht.Helper()
	}
	oldValue, ok := os.LookupEnv(key)
	assert.NilError(t, os.Setenv(key, value))
	cleanup := func() {
		if ht, ok := t.(helperT); ok {
			ht.Helper()
		}
		if !ok {
			assert.NilError(t, os.Unsetenv(key))
			return
		}
		assert.NilError(t, os.Setenv(key, oldValue))
	}
	if tc, ok := t.(subtest.TestContext); ok {
		tc.AddCleanup(cleanup)
	}
	return cleanup
}

// PatchAll sets the environment to env, and returns a function which will
// reset the environment back to the previous state.
func PatchAll(t assert.TestingT, env map[string]string) func() {
	if ht, ok := t.(helperT); ok {
		ht.Helper()
	}
	oldEnv := os.Environ()
	os.Clearenv()

	for key, value := range env {
		assert.NilError(t, os.Setenv(key, value), "setenv %s=%s", key, value)
	}
	cleanup := func() {
		if ht, ok := t.(helperT); ok {
			ht.Helper()
		}
		os.Clearenv()
		for key, oldVal := range ToMap(oldEnv) {
			assert.NilError(t, os.Setenv(key, oldVal), "setenv %s=%s", key, oldVal)
		}
	}
	if tc, ok := t.(subtest.TestContext); ok {
		tc.AddCleanup(cleanup)
	}
	return cleanup
}

// ToMap takes a list of strings in the format returned by os.Environ() and
// returns a mapping of keys to values.
func ToMap(env []string) map[string]string {
	result := map[string]string{}
	for _, raw := range env {
		key, value := getParts(raw)
		result[key] = value
	}
	return result
}

func getParts(raw string) (string, string) {
	if raw == "" {
		return "", ""
	}
	// Environment variables on windows can begin with =
	// http://blogs.msdn.com/b/oldnewthing/archive/2010/05/06/10008132.aspx
	parts := strings.SplitN(raw[1:], "=", 2)
	key := raw[:1] + parts[0]
	if len(parts) == 1 {
		return key, ""
	}
	return key, parts[1]
}

// ChangeWorkingDir to the directory, and return a function which restores the
// previous working directory.
func ChangeWorkingDir(t assert.TestingT, dir string) func() {
	if ht, ok := t.(helperT); ok {
		ht.Helper()
	}
	cwd, err := os.Getwd()
	assert.NilError(t, err)
	assert.NilError(t, os.Chdir(dir))
	cleanup := func() {
		if ht, ok := t.(helperT); ok {
			ht.Helper()
		}
		assert.NilError(t, os.Chdir(cwd))
	}
	if tc, ok := t.(subtest.TestContext); ok {
		tc.AddCleanup(cleanup)
	}
	return cleanup
}
