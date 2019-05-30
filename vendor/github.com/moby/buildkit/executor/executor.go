package executor

import (
	"context"
	"io"
	"net"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/solver/pb"
)

type Meta struct {
	Args           []string
	Env            []string
	User           string
	Cwd            string
	Tty            bool
	ReadonlyRootFS bool
	ExtraHosts     []HostIP
	NetMode        pb.NetMode
	SecurityMode   pb.SecurityMode
}

type Mount struct {
	Src      cache.Mountable
	Selector string
	Dest     string
	Readonly bool
}

type Executor interface {
	// TODO: add stdout/err
	Exec(ctx context.Context, meta Meta, rootfs cache.Mountable, mounts []Mount, stdin io.ReadCloser, stdout, stderr io.WriteCloser) error
}

type HostIP struct {
	Host string
	IP   net.IP
}
