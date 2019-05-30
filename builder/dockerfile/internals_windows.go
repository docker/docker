package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/system"
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

func parseChownFlag(builder *Builder, state *dispatchState, chown, ctrRootPath string, identityMapping *idtools.IdentityMapping) (idtools.Identity, error) {
	if builder.options.Platform == "windows" {
		return getAccountIdentity(builder, chown, ctrRootPath, state)
	}

	return identityMapping.RootPair(), nil
}

func getAccountIdentity(builder *Builder, accountName string, ctrRootPath string, state *dispatchState) (idtools.Identity, error) {
	// If this is potentially a string SID then attempt to convert it to verify
	// this, otherwise continue looking for the account.
	if strings.HasPrefix(accountName, "S-") || strings.HasPrefix(accountName, "s-") {
		sid, err := windows.StringToSid(accountName)

		if err == nil {
			accountSid, err := sid.String()

			if err != nil {
				return idtools.Identity{SID: ""}, errors.Wrapf(err, "error converting SID to string")
			}

			return idtools.Identity{SID: accountSid}, nil
		}
	}

	// Attempt to obtain the SID using the name.
	sid, _, accType, err := windows.LookupSID("", accountName)

	// If this is a SID that is built-in and hence the same across all systems then use that.
	if err == nil && (accType == windows.SidTypeAlias || accType == windows.SidTypeWellKnownGroup) {
		accountSid, err := sid.String()

		if err != nil {
			return idtools.Identity{SID: ""}, errors.Wrapf(err, "error converting SID to string")
		}

		return idtools.Identity{SID: accountSid}, nil
	}

	// Check if the account name is one unique to containers.
	if strings.EqualFold(accountName, "ContainerAdministrator") {
		return idtools.Identity{SID: system.ContainerAdministratorSidString}, nil

	} else if strings.EqualFold(accountName, "ContainerUser") {
		return idtools.Identity{SID: system.ContainerUserSidString}, nil
	}

	// All other lookups failed, so therefore determine if the account in
	// question exists in the container and if so, obtain its SID.
	return lookupNTAccount(builder, accountName, state)
}

func lookupNTAccount(builder *Builder, accountName string, state *dispatchState) (idtools.Identity, error) {

	source, _ := filepath.Split(os.Args[0])

	target := "C:\\Docker"
	targetExecutable := target + "\\containerutility.exe"

	optionsPlatform, err := platforms.Parse(builder.options.Platform)
	if err != nil {
		return idtools.Identity{}, err
	}

	runConfig := copyRunConfig(state.runConfig,
		withCmdCommentString("internal run to obtain NT account information.", optionsPlatform.OS))

	runConfig.Cmd = []string{targetExecutable, "getaccountsid", accountName}

	hostConfig := &container.HostConfig{Mounts: []mount.Mount{
		{
			Type:     mount.TypeBind,
			Source:   source,
			Target:   target,
			ReadOnly: true,
		},
	},
	}

	container, err := builder.containerManager.Create(runConfig, hostConfig)
	if err != nil {
		return idtools.Identity{}, err
	}

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)

	if err := builder.containerManager.Run(builder.clientCtx, container.ID, stdout, stderr); err != nil {
		if err, ok := err.(*statusCodeError); ok {
			return idtools.Identity{}, &jsonmessage.JSONError{
				Message: stderr.String(),
				Code:    err.StatusCode(),
			}
		}
		return idtools.Identity{}, err
	}

	accountSid := stdout.String()

	return idtools.Identity{SID: accountSid}, nil
}
