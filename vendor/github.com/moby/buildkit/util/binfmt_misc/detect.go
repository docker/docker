package binfmt_misc

import (
	"strings"
	"sync"

	"github.com/containerd/containerd/platforms"
	"github.com/sirupsen/logrus"
)

var once sync.Once
var arr []string

func SupportedPlatforms() []string {
	once.Do(func() {
		def := defaultPlatform()
		arr = append(arr, def)
		if p := "linux/amd64"; def != p && amd64Supported() == nil {
			arr = append(arr, p)
		}
		if p := "linux/arm64"; def != p && arm64Supported() == nil {
			arr = append(arr, p)
		}
		if !strings.HasPrefix(def, "linux/arm/") && armSupported() == nil {
			arr = append(arr, "linux/arm/v7", "linux/arm/v6")
		} else if def == "linux/arm/v7" {
			arr = append(arr, "linux/arm/v6")
		}
	})
	return arr
}

//WarnIfUnsupported validates the platforms and show warning message if there is,
//the end user could fix the issue based on those warning, and thus no need to drop
//the platform from the candidates.
func WarnIfUnsupported(pfs []string) {
	def := defaultPlatform()
	for _, p := range pfs {
		if p != def {
			if p == "linux/amd64" {
				if err := amd64Supported(); err != nil {
					printPlatfromWarning(p, err)
				}
			}
			if p == "linux/arm64" {
				if err := arm64Supported(); err != nil {
					printPlatfromWarning(p, err)
				}
			}
			if strings.HasPrefix(p, "linux/arm/v6") || strings.HasPrefix(p, "linux/arm/v7") {
				if err := armSupported(); err != nil {
					printPlatfromWarning(p, err)
				}
			}
		}
	}
}

func defaultPlatform() string {
	return platforms.Format(platforms.Normalize(platforms.DefaultSpec()))
}

func printPlatfromWarning(p string, err error) {
	if strings.Contains(err.Error(), "exec format error") {
		logrus.Warnf("platform %s cannot pass the validation, kernel support for miscellaneous binary may have not enabled.", p)
	} else if strings.Contains(err.Error(), "no such file or directory") {
		logrus.Warnf("platforms %s cannot pass the validation, '-F' flag might have not set for 'binfmt_misc'.", p)
	} else {
		logrus.Warnf("platforms %s cannot pass the validation: %s", p, err.Error())
	}
}
