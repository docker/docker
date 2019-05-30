package file

import (
	"os"

	"github.com/containerd/continuity/fs"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver/llbsolver/ops/fileoptypes"
	"github.com/moby/buildkit/solver/pb"
	"github.com/opencontainers/runc/libcontainer/user"
	"github.com/pkg/errors"
	copy "github.com/tonistiigi/fsutil/copy"
)

func readUser(chopt *pb.ChownOpt, mu, mg fileoptypes.Mount) (*copy.ChownOpt, error) {
	if chopt == nil {
		return nil, nil
	}
	var us copy.ChownOpt
	if chopt.User != nil {
		switch u := chopt.User.User.(type) {
		case *pb.UserOpt_ByName:
			if mu == nil {
				return nil, errors.Errorf("invalid missing user mount")
			}
			mmu, ok := mu.(*Mount)
			if !ok {
				return nil, errors.Errorf("invalid mount type %T", mu)
			}
			lm := snapshot.LocalMounter(mmu.m)
			dir, err := lm.Mount()
			if err != nil {
				return nil, err
			}
			defer lm.Unmount()

			passwdPath, err := user.GetPasswdPath()
			if err != nil {
				return nil, err
			}

			passwdPath, err = fs.RootPath(dir, passwdPath)
			if err != nil {
				return nil, err
			}

			ufile, err := os.Open(passwdPath)
			if err != nil {
				return nil, err
			}
			defer ufile.Close()

			users, err := user.ParsePasswdFilter(ufile, func(uu user.User) bool {
				return uu.Name == u.ByName.Name
			})
			if err != nil {
				return nil, err
			}

			if len(users) > 0 {
				us.Uid = users[0].Uid
				us.Gid = users[0].Gid
			}
		case *pb.UserOpt_ByID:
			us.Uid = int(u.ByID)
			us.Gid = int(u.ByID)
		}
	}

	if chopt.Group != nil {
		switch u := chopt.Group.User.(type) {
		case *pb.UserOpt_ByName:
			if mg == nil {
				return nil, errors.Errorf("invalid missing group mount")
			}
			mmg, ok := mg.(*Mount)
			if !ok {
				return nil, errors.Errorf("invalid mount type %T", mg)
			}
			lm := snapshot.LocalMounter(mmg.m)
			dir, err := lm.Mount()
			if err != nil {
				return nil, err
			}
			defer lm.Unmount()

			groupPath, err := user.GetGroupPath()
			if err != nil {
				return nil, err
			}

			groupPath, err = fs.RootPath(dir, groupPath)
			if err != nil {
				return nil, err
			}

			gfile, err := os.Open(groupPath)
			if err != nil {
				return nil, err
			}
			defer gfile.Close()

			groups, err := user.ParseGroupFilter(gfile, func(g user.Group) bool {
				return g.Name == u.ByName.Name
			})
			if err != nil {
				return nil, err
			}

			if len(groups) > 0 {
				us.Gid = groups[0].Gid
			}
		case *pb.UserOpt_ByID:
			us.Gid = int(u.ByID)
		}
	}

	return &us, nil
}
