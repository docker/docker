variable "DOCKER_DEBUG" {
  default = ""
}
variable "DOCKER_STATIC" {
  default = "1"
}
variable "DOCKER_LDFLAGS" {
  default = ""
}
variable "DOCKER_BUILDTAGS" {
  default = ""
}
variable "DOCKER_GITCOMMIT" {
  default = null
}

# Docker version such as 23.0.0-dev. Automatically generated through Git ref.
variable "VERSION" {
  default = ""
}

# The platform name, such as "Docker Engine - Community".
variable "PLATFORM" {
  default = ""
}

# The product name, used to set version.ProductName, which is used to set
# BuildKit's ExportedProduct variable in order to show useful error messages
# to users when a certain version of the product doesn't support a BuildKit feature.
variable "PRODUCT" {
  default = ""
}

# Sets the version.DefaultProductLicense string, such as "Community Engine".
# This field can contain a summary of the product license of the daemon if a
# commercial license has been applied to the daemon.
variable "DEFAULT_PRODUCT_LICENSE" {
  default = ""
}

# The name of the packager (e.g. "Docker, Inc."). This used to set CompanyName
# in the manifest.
variable "PACKAGER_NAME" {
  default = ""
}

# Special target: https://github.com/docker/metadata-action#bake-definition
target "docker-metadata-action" {
  tags = ["moby-bin:local"]
}

# Defines the output folder
variable "DESTDIR" {
  default = ""
}
function "bindir" {
  params = [defaultdir]
  result = DESTDIR != "" ? DESTDIR : "./bundles/${defaultdir}"
}

target "_common" {
  args = {
    BUILDKIT_CONTEXT_KEEP_GIT_DIR = 1
    DOCKER_DEBUG = DOCKER_DEBUG
    DOCKER_STATIC = DOCKER_STATIC
    DOCKER_LDFLAGS = DOCKER_LDFLAGS
    DOCKER_BUILDTAGS = DOCKER_BUILDTAGS
    DOCKER_GITCOMMIT = DOCKER_GITCOMMIT
    VERSION = VERSION
    PLATFORM = PLATFORM
    PRODUCT = PRODUCT
    DEFAULT_PRODUCT_LICENSE = DEFAULT_PRODUCT_LICENSE
    PACKAGER_NAME = PACKAGER_NAME
  }
}

group "default" {
  targets = ["binary"]
}

target "_platforms" {
  platforms = [
    "linux/amd64",
    "linux/arm/v5",
    "linux/arm/v6",
    "linux/arm/v7",
    "linux/arm64",
    "linux/ppc64le",
    "linux/s390x",
    "windows/amd64",
    "windows/arm64"
  ]
}

#
# build dockerd and docker-proxy
#

target "binary" {
  inherits = ["_common"]
  target = "binary"
  output = [bindir(DOCKER_STATIC == "1" ? "binary" : "dynbinary")]
}

target "dynbinary" {
  inherits = ["binary"]
  output = [bindir("dynbinary")]
  args = {
    DOCKER_STATIC = "0"
  }
}

target "binary-cross" {
  inherits = ["binary", "_platforms"]
}

target "binary-smoketest" {
  inherits = ["_common"]
  target = "smoketest"
  output = ["type=cacheonly"]
  platforms = [
    "linux/amd64",
    "linux/arm/v6",
    "linux/arm/v7",
    "linux/arm64",
    "linux/ppc64le",
    "linux/s390x"
  ]
}

#
# same as binary but with extra tools as well (containerd, runc, ...)
#

target "all" {
  inherits = ["_common"]
  target = "all"
  output = [bindir(DOCKER_STATIC == "1" ? "binary" : "dynbinary")]
}

target "all-cross" {
  inherits = ["all", "_platforms"]
}

# Export the pinned tools and fixtures for integration tests on a Linux host.
target "test-integration-deps" {
  inherits = ["_common"]
  target = "test-integration-deps"
  args = {
    DOCKER_STATIC = "1"
  }
  output = [bindir("test-integration-deps")]
}

#
# bin image
#

target "bin-image" {
  inherits = ["all", "docker-metadata-action"]
  output = ["type=docker"]
}

target "bin-image-cross" {
  inherits = ["bin-image"]
  output = ["type=image"]
  platforms = [
    "linux/amd64",
    "linux/arm/v6",
    "linux/arm/v7",
    "linux/arm64",
    "linux/ppc64le",
    "linux/s390x",
    "windows/amd64"
  ]
}

#
# dind
#

target "dind" {
  inherits = ["_common"]
  target = "dind"
  tags = ["docker-dind"]
  output = ["type=docker"]
}

#
# dev
#

variable "SYSTEMD" {
  default = "false"
}

variable "FIREWALLD" {
  default = "false"
}

target "dev" {
  inherits = ["_common"]
  target = "dev"
  args = {
    SYSTEMD = SYSTEMD
    FIREWALLD = FIREWALLD
  }
  tags = ["docker-dev"]
  output = ["type=docker"]
}

#
# govulncheck
#

variable "GOVULNCHECK_FORMAT" {
  default = null
}

target "govulncheck" {
  inherits = ["_common"]
  dockerfile = "./hack/dockerfiles/govulncheck.Dockerfile"
  target = "output"
  args = {
    FORMAT = GOVULNCHECK_FORMAT
  }
  no-cache-filter = ["run"]
  output = ["${DESTDIR}"]
}
