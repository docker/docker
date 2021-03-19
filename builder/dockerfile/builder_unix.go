// +build !windows

package dockerfile // import "github.com/docker/docker/builder/dockerfile"

func defaultShell() []string {
	return []string{"/bin/sh", "-c"}
}
