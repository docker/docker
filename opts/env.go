package opts // import "github.com/docker/docker/opts"

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/pkg/errors"
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
	if strings.TrimSpace(arr[0]) == "" {
		return "", errors.Errorf("invalid environment variable: %q", val)
	}
	if len(arr) > 1 {
		return val, nil
	}
	if !doesEnvExist(val) {
		return val, nil
	}
	return fmt.Sprintf("%s=%s", val, os.Getenv(val)), nil
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
