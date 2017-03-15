package opts

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ValidateEnv validates an environment variable and returns it.
// If no value is specified, it returns the current value using os.Getenv.
//
// As on ParseEnvFile and related to #16585, environment variable names
// are not validate what so ever, it's up to application inside docker
// to validate them or not.
//
// The only validation here is to check if name is empty, per #25099
func ValidateEnv(val string) (string, error) {
	arr := strings.Split(val, "=")
	if arr[0] == "" {
		return "", fmt.Errorf("invalid environment variable: %s", val)
	}
	if len(arr) > 1 {
		return val, nil
	}
	if !doesEnvExist(val) {
		return val, nil
	}
	return fmt.Sprintf("%s=%s", val, os.Getenv(val)), nil
}

// ExpandEnvWildcard takes a glob pattern and then returns a slice of strings
// of the form KEY=VALUE with environment variables where KEY matches the glob
// pattern.
func ExpandEnvWildcard(pattern string) []string {
	var opts []string

	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		name := parts[0]
		val := parts[1]
		matched, err := filepath.Match(pattern, name)
		if err != nil {
			break
		}
		if matched {
			opts = append(opts, fmt.Sprintf("%s=%s", name, val))
		}
	}

	return opts
}

func doesEnvExist(name string) bool {
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		if runtime.GOOS == "windows" {
			// Environment variable are case-insensitive on Windows. PaTh, path and PATH are equivalent.
			if strings.EqualFold(parts[0], name) {
				return true
			}
		}
		if parts[0] == name {
			return true
		}
	}
	return false
}
