package mounts // import "github.com/docker/docker/volume/mounts"

func (p *linuxParser) HasResource(m *MountPoint, absolutePath string) bool {
	return false
}
