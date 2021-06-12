package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	winio "github.com/Microsoft/go-winio"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/reexec"
	"github.com/docker/docker/pkg/system"
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

var pathDenyList = map[string]bool{
	"c:\\":        true,
	"c:\\windows": true,
}

func init() {
	reexec.Register("windows-fix-permissions", fixPermissionsReexec)
}

func fixPermissions(source, destination string, identity idtools.Identity, _ bool) error {
	if identity.SID == "" {
		return nil
	}

	cmd := reexec.Command("windows-fix-permissions", source, destination, identity.SID)
	output, err := cmd.CombinedOutput()

	return errors.Wrapf(err, "failed to exec windows-fix-permissions: %s", output)
}

func fixPermissionsReexec() {
	err := fixPermissionsWindows(os.Args[1], os.Args[2], os.Args[3])
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(1)
	}
}

func fixPermissionsWindows(source, destination, SID string) error {

	privileges := []string{winio.SeRestorePrivilege, system.SeTakeOwnershipPrivilege}

	err := winio.EnableProcessPrivileges(privileges)
	if err != nil {
		return err
	}

	defer winio.DisableProcessPrivileges(privileges)

	sid, err := windows.StringToSid(SID)
	if err != nil {
		return err
	}

	// Owners on *nix have read/write/delete/read control and write DAC.
	// Add an ACE that grants this to the user/group specified with the
	// chown option. Currently Windows is not honoring the owner change,
	// however, they are aware of this and it should be fixed at some
	// point.

	sddlString := system.SddlAdministratorsLocalSystem
	sddlString += "(A;OICI;GRGWGXRCWDSD;;;" + SID + ")"

	securityDescriptor, err := windows.SecurityDescriptorFromString(sddlString)
	if err != nil {
		return err
	}

	dacl, _, err := securityDescriptor.DACL()
	if err != nil {
		return err
	}

	return windows.SetNamedSecurityInfo(destination, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION, sid, nil, dacl, nil)
}

func validateCopySourcePath(imageSource *imageMount, origPath, platform string) error {
	origPath = filepath.FromSlash(origPath)
	p := strings.ToLower(filepath.Clean(origPath))
	if !filepath.IsAbs(p) {
		if filepath.VolumeName(p) != "" {
			if p[len(p)-2:] == ":." { // case where clean returns weird c:. paths
				p = p[:len(p)-1]
			}
			p += "\\"
		} else {
			p = filepath.Join("c:\\", p)
		}
	}
	if _, ok := pathDenyList[p]; ok {
		return errors.New("copy from c:\\ or c:\\windows is not allowed on windows")
	}
	return nil
}
