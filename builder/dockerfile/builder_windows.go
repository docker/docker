package dockerfile // import "github.com/docker/docker/builder/dockerfile"

func defaultShell() []string {
	return []string{"cmd", "/S", "/C"}
}
