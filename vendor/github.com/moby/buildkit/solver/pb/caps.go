package pb

import "github.com/moby/buildkit/util/apicaps"

var Caps apicaps.CapList

// Every backwards or forwards non-compatible change needs to add a new capability row.
// By default new capabilities should be experimental. After merge a capability is
// considered immutable. After a capability is marked stable it should not be disabled.

const (
	CapSourceImage                apicaps.CapID = "source.image"
	CapSourceImageResolveMode     apicaps.CapID = "source.image.resolvemode"
	CapSourceLocal                apicaps.CapID = "source.local"
	CapSourceLocalUnique          apicaps.CapID = "source.local.unique"
	CapSourceLocalSessionID       apicaps.CapID = "source.local.sessionid"
	CapSourceLocalIncludePatterns apicaps.CapID = "source.local.includepatterns"
	CapSourceLocalFollowPaths     apicaps.CapID = "source.local.followpaths"
	CapSourceLocalExcludePatterns apicaps.CapID = "source.local.excludepatterns"
	CapSourceLocalSharedKeyHint   apicaps.CapID = "source.local.sharedkeyhint"

	CapSourceGit        apicaps.CapID = "source.git"
	CapSourceGitKeepDir apicaps.CapID = "source.git.keepgitdir"
	CapSourceGitFullURL apicaps.CapID = "source.git.fullurl"

	CapSourceHTTP         apicaps.CapID = "source.http"
	CapSourceHTTPChecksum apicaps.CapID = "source.http.checksum"
	CapSourceHTTPPerm     apicaps.CapID = "source.http.perm"
	CapSourceHTTPUIDGID   apicaps.CapID = "soruce.http.uidgid"

	CapBuildOpLLBFileName apicaps.CapID = "source.buildop.llbfilename"

	CapExecMetaBase            apicaps.CapID = "exec.meta.base"
	CapExecMetaProxy           apicaps.CapID = "exec.meta.proxyenv"
	CapExecMetaNetwork         apicaps.CapID = "exec.meta.network"
	CapExecMetaSecurity        apicaps.CapID = "exec.meta.security"
	CapExecMetaSetsDefaultPath apicaps.CapID = "exec.meta.setsdefaultpath"
	CapExecMountBind           apicaps.CapID = "exec.mount.bind"
	CapExecMountCache          apicaps.CapID = "exec.mount.cache"
	CapExecMountCacheSharing   apicaps.CapID = "exec.mount.cache.sharing"
	CapExecMountSelector       apicaps.CapID = "exec.mount.selector"
	CapExecMountTmpfs          apicaps.CapID = "exec.mount.tmpfs"
	CapExecMountSecret         apicaps.CapID = "exec.mount.secret"
	CapExecMountSSH            apicaps.CapID = "exec.mount.ssh"
	CapExecCgroupsMounted      apicaps.CapID = "exec.cgroup"

	CapFileBase apicaps.CapID = "file.base"

	CapConstraints apicaps.CapID = "constraints"
	CapPlatform    apicaps.CapID = "platform"

	CapMetaIgnoreCache apicaps.CapID = "meta.ignorecache"
	CapMetaDescription apicaps.CapID = "meta.description"
	CapMetaExportCache apicaps.CapID = "meta.exportcache"
)

func init() {

	Caps.Init(apicaps.Cap{
		ID:      CapSourceImage,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceImageResolveMode,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceLocal,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceLocalUnique,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceLocalSessionID,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceLocalIncludePatterns,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceLocalFollowPaths,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceLocalExcludePatterns,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceLocalSharedKeyHint,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})
	Caps.Init(apicaps.Cap{
		ID:      CapSourceGit,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceGitKeepDir,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceGitFullURL,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceHTTP,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceHTTPChecksum,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceHTTPPerm,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapSourceHTTPUIDGID,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapBuildOpLLBFileName,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMetaBase,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMetaProxy,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMetaNetwork,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMetaSetsDefaultPath,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMetaSecurity,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMountBind,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMountCache,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMountCacheSharing,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMountSelector,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMountTmpfs,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMountSecret,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecMountSSH,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapExecCgroupsMounted,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapFileBase,
		Enabled: true,
		Status:  apicaps.CapStatusPrerelease,
		SupportedHint: map[string]string{
			"docker":   "Docker v19.03",
			"buildkit": "BuildKit v0.5.0",
		},
	})

	Caps.Init(apicaps.Cap{
		ID:      CapConstraints,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapPlatform,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapMetaIgnoreCache,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapMetaDescription,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

	Caps.Init(apicaps.Cap{
		ID:      CapMetaExportCache,
		Enabled: true,
		Status:  apicaps.CapStatusExperimental,
	})

}
