package distribution // import "github.com/docker/docker/distribution"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Microsoft/hcsshim/osversion"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/distribution"
	"github.com/docker/distribution/manifest/manifestlist"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/docker/docker/pkg/system"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows/registry"
)

var _ distribution.Describable = &v2LayerDescriptor{}

func (ld *v2LayerDescriptor) Descriptor() distribution.Descriptor {
	if ld.src.MediaType == schema2.MediaTypeForeignLayer && len(ld.src.URLs) > 0 {
		return ld.src
	}
	return distribution.Descriptor{}
}

func (ld *v2LayerDescriptor) open(ctx context.Context) (distribution.ReadSeekCloser, error) {
	blobs := ld.repo.Blobs(ctx)
	rsc, err := blobs.Open(ctx, ld.digest)

	if len(ld.src.URLs) == 0 {
		return rsc, err
	}

	// We're done if the registry has this blob.
	if err == nil {
		// Seek does an HTTP GET.  If it succeeds, the blob really is accessible.
		if _, err = rsc.Seek(0, io.SeekStart); err == nil {
			return rsc, nil
		}
		rsc.Close()
	}

	// Find the first URL that results in a 200 result code.
	for _, url := range ld.src.URLs {
		logrus.Debugf("Pulling %v from foreign URL %v", ld.digest, url)
		rsc = transport.NewHTTPReadSeeker(http.DefaultClient, url, nil)

		// Seek does an HTTP GET.  If it succeeds, the blob really is accessible.
		_, err = rsc.Seek(0, io.SeekStart)
		if err == nil {
			break
		}
		logrus.Debugf("Download for %v failed: %v", ld.digest, err)
		rsc.Close()
		rsc = nil
	}
	return rsc, err
}

func filterManifests(manifests []manifestlist.ManifestDescriptor, p specs.Platform) []manifestlist.ManifestDescriptor {
	version := osversion.Get()
	osVersion := fmt.Sprintf("%d.%d.%d", version.MajorVersion, version.MinorVersion, version.Build)
	logrus.Debugf("will prefer Windows entries with version %s", osVersion)

	var matches []manifestlist.ManifestDescriptor
	foundWindowsMatch := false
	for _, manifestDescriptor := range manifests {
		if (manifestDescriptor.Platform.Architecture == runtime.GOARCH) &&
			((p.OS != "" && manifestDescriptor.Platform.OS == p.OS) || // Explicit user request for an OS we know we support
				(p.OS == "" && system.IsOSSupported(manifestDescriptor.Platform.OS))) { // No user requested OS, but one we can support
			if strings.EqualFold("windows", manifestDescriptor.Platform.OS) {
				if err := checkImageCompatibility("windows", manifestDescriptor.Platform.OSVersion); err != nil {
					continue
				}
				foundWindowsMatch = true
			}
			matches = append(matches, manifestDescriptor)
			logrus.Debugf("found match %s/%s %s with media type %s, digest %s", manifestDescriptor.Platform.OS, runtime.GOARCH, manifestDescriptor.Platform.OSVersion, manifestDescriptor.MediaType, manifestDescriptor.Digest.String())
		} else {
			logrus.Debugf("ignoring %s/%s %s with media type %s, digest %s", manifestDescriptor.Platform.OS, manifestDescriptor.Platform.Architecture, manifestDescriptor.Platform.OSVersion, manifestDescriptor.MediaType, manifestDescriptor.Digest.String())
		}
	}
	if foundWindowsMatch {
		sort.Stable(manifestsByVersion{getComparableOSVersion(), matches})
	}
	return matches
}

type manifestsByVersion struct {
	comparableOSVersion uint32
	list                []manifestlist.ManifestDescriptor
}

func (mbv manifestsByVersion) getVersion(i int) uint64 {
	v := getComparableImageVersion(mbv.list[i].Platform.OSVersion)
	if v == mbv.comparableOSVersion { // prefer matching build and UBR
		return uint64(v) | 2<<32
	}
	if v>>16 == mbv.comparableOSVersion>>16 { // prefer compatible versions
		return uint64(v) | 1<<32
	}
	return uint64(v)
}

func (mbv manifestsByVersion) Less(i, j int) bool {
	// Prefer versions which have a greater version number
	// Move compatible versions to the top, prefer UBR match
	return (strings.EqualFold("windows", mbv.list[i].Platform.OS) && !strings.EqualFold("windows", mbv.list[j].Platform.OS)) ||
		mbv.getVersion(j) < mbv.getVersion(i)
}

func (mbv manifestsByVersion) Len() int {
	return len(mbv.list)
}

func (mbv manifestsByVersion) Swap(i, j int) {
	mbv.list[i], mbv.list[j] = mbv.list[j], mbv.list[i]
}

// checkImageCompatibility blocks pulling incompatible images based on a later OS build
// Fixes https://github.com/moby/moby/issues/36184.
func checkImageCompatibility(imageOS, imageOSVersion string) error {
	if imageOS == "windows" {
		hostOSV := osversion.Get()
		if uint16(getComparableImageVersion(imageOSVersion)>>16) > hostOSV.Build {
			errMsg := fmt.Sprintf("a Windows version %s image is incompatible with a %s host", imageOSVersion, hostOSV.ToString())
			logrus.Debugf(errMsg)
			return errors.New(errMsg)
		}
	}
	return nil
}

func formatPlatform(platform specs.Platform) string {
	if platform.OS == "" {
		platform = platforms.DefaultSpec()
	}
	return fmt.Sprintf("%s %s", platforms.Format(platform), osversion.Get().ToString())
}

// return build.ubr as uint32
func getComparableImageVersion(imageOSVersion string) uint32 {
	version := uint32(0)
	splitImageOSVersion := strings.Split(imageOSVersion, ".") // eg 10.0.16299.nnnn
	if len(splitImageOSVersion) >= 3 {
		if imageOSBuild, err := strconv.Atoi(splitImageOSVersion[2]); err == nil {
			version = uint32(imageOSBuild) << 16
		}
	}
	if len(splitImageOSVersion) >= 4 {
		if ubr, err := strconv.Atoi(splitImageOSVersion[3]); err == nil {
			version |= uint32(ubr)
		}
	}
	return version
}

var (
	osv  uint32
	once sync.Once
)

func getComparableOSVersion() uint32 {
	once.Do(func() {
		osv = uint32(osversion.Get().Build) << 16
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
		if err != nil {
			return
		}
		defer k.Close()
		d, _, err := k.GetIntegerValue("UBR")
		if err != nil {
			return
		}
		osv |= uint32(d & 0xffff)
	})
	return osv
}
