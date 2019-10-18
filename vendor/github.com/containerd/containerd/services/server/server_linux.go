/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package server

import (
	"context"
	"os"

	"github.com/containerd/cgroups"
	"github.com/containerd/containerd/log"
	srvconfig "github.com/containerd/containerd/services/server/config"
	"github.com/containerd/containerd/sys"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// apply sets config settings on the server process
func apply(ctx context.Context, config *srvconfig.Config) error {
	if config.OOMScore != 0 {
		log.G(ctx).Debugf("changing OOM score to %d", config.OOMScore)
		if err := sys.SetOOMScore(os.Getpid(), config.OOMScore); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to change OOM score to %d", config.OOMScore)
		}
	}
	if config.Cgroup.Path != "" {
		cg, err := cgroups.Load(cgroups.V1, cgroups.StaticPath(config.Cgroup.Path))
		if err != nil {
			if err != cgroups.ErrCgroupDeleted {
				return err
			}
			if cg, err = cgroups.New(cgroups.V1, cgroups.StaticPath(config.Cgroup.Path), &specs.LinuxResources{}); err != nil {
				return err
			}
		}
		if err := cg.Add(cgroups.Process{
			Pid: os.Getpid(),
		}); err != nil {
			return err
		}
	}
	return nil
}
