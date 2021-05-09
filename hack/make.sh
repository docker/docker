#!/usr/bin/env bash
set -e

# This script builds various binary artifacts from a checkout of the docker
# source code.
#
# Requirements:
# - The current directory should be a checkout of the docker source code
#   (https://github.com/docker/docker). Whatever version is checked out
#   will be built.
# - The VERSION file, at the root of the repository, should exist, and
#   will be used as Docker binary version and package version.
# - The hash of the git commit will also be included in the Docker binary,
#   with the suffix -unsupported if the repository isn't clean.
# - The script is intended to be run inside the docker container specified
#   in the Dockerfile at the root of the source. In other words:
#   DO NOT CALL THIS SCRIPT DIRECTLY.
# - The right way to call this script is to invoke "make" from
#   your checkout of the Docker repository.
#   the Makefile will do a "docker build -t docker ." and then
#   "docker run hack/make.sh" in the resulting image.
#

set -o pipefail

export DOCKER_PKG='github.com/docker/docker'
export SCRIPTDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export MAKEDIR="$SCRIPTDIR/make"
export PKG_CONFIG=${PKG_CONFIG:-pkg-config}

echo

# List of bundles to create when no argument is passed
DEFAULT_BUNDLES=(
	binary-daemon
	dynbinary
	test-integration
	test-buildkit
	test-docker-py
	cross
)

VERSION=${VERSION:-dev}
! BUILDTIME=$(date -u -d "@${SOURCE_DATE_EPOCH:-$(date +%s)}" --rfc-3339 ns 2> /dev/null | sed -e 's/ /T/')
if [ "$DOCKER_GITCOMMIT" ]; then
	GITCOMMIT="$DOCKER_GITCOMMIT"
elif command -v git &> /dev/null && [ -e .git ] && git rev-parse &> /dev/null; then
	GITCOMMIT=$(git rev-parse --short HEAD)
	if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
		GITCOMMIT="$GITCOMMIT-unsupported"
		echo "#~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~"
		echo "# GITCOMMIT = $GITCOMMIT"
		echo "# The version you are building is listed as unsupported because"
		echo "# there are some files in the git repository that are in an uncommitted state."
		echo "# Commit these changes, or add to .gitignore to remove the -unsupported from the version."
		echo "# Here is the current list:"
		git status --porcelain --untracked-files=no
		echo "#~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~"
	fi
else
	echo >&2 'error: .git directory missing and DOCKER_GITCOMMIT not specified'
	echo >&2 '  Please either build with the .git directory accessible, or specify the'
	echo >&2 '  exact (--short) commit hash you are building using DOCKER_GITCOMMIT for'
	echo >&2 '  future accountability in diagnosing build issues.  Thanks!'
	exit 1
fi

if [ "$AUTO_GOPATH" ]; then
	rm -rf .gopath
	mkdir -p .gopath/src/"$(dirname "${DOCKER_PKG}")"
	ln -sf ../../../.. .gopath/src/"${DOCKER_PKG}"
	export GOPATH="${PWD}/.gopath"
fi

if [ ! "$GOPATH" ]; then
	echo >&2 'error: missing GOPATH; please see https://golang.org/doc/code.html#GOPATH'
	echo >&2 '  alternatively, set AUTO_GOPATH=1'
	exit 1
fi

# Adds $1_$2 to DOCKER_BUILDTAGS unless it already
# contains a word starting from $1_
add_buildtag() {
	[[ " $DOCKER_BUILDTAGS" == *" $1_"* ]] || DOCKER_BUILDTAGS+=" $1_$2"
}

if ${PKG_CONFIG} 'libsystemd >= 209' 2> /dev/null; then
	DOCKER_BUILDTAGS+=" journald"
elif ${PKG_CONFIG} 'libsystemd-journal' 2> /dev/null; then
	DOCKER_BUILDTAGS+=" journald journald_compat"
fi

# test whether "libdevmapper.h" is new enough to support deferred remove
# functionality. We favour libdm_dlsym_deferred_remove over
# libdm_no_deferred_remove in dynamic cases because the binary could be shipped
# with a newer libdevmapper than the one it was built with.
if
	command -v gcc &> /dev/null \
		&& ! (echo -e '#include <libdevmapper.h>\nint main() { dm_task_deferred_remove(NULL); }' | gcc -xc - -o /dev/null $(pkg-config --libs devmapper) &> /dev/null) \
		;
then
	add_buildtag libdm dlsym_deferred_remove
fi

# Use these flags when compiling the tests and final binary

IAMSTATIC='true'
if [ -z "$DOCKER_DEBUG" ]; then
	LDFLAGS='-w'
fi

LDFLAGS_STATIC=''
EXTLDFLAGS_STATIC='-static'
# ORIG_BUILDFLAGS is necessary for the cross target which cannot always build
# with options like -race.
ORIG_BUILDFLAGS=(-tags "netgo osusergo static_build $DOCKER_BUILDTAGS" -installsuffix netgo)
# see https://github.com/golang/go/issues/9369#issuecomment-69864440 for why -installsuffix is necessary here

BUILDFLAGS=(${BUILDFLAGS} "${ORIG_BUILDFLAGS[@]}")

LDFLAGS_STATIC_DOCKER="
	$LDFLAGS_STATIC
	-extldflags \"$EXTLDFLAGS_STATIC\"
"

if [ "$(uname -s)" = 'FreeBSD' ]; then
	# Tell cgo the compiler is Clang, not GCC
	# https://code.google.com/p/go/source/browse/src/cmd/cgo/gcc.go?spec=svne77e74371f2340ee08622ce602e9f7b15f29d8d3&r=e6794866ebeba2bf8818b9261b54e2eef1c9e588#752
	export CC=clang

	# "-extld clang" is a workaround for
	# https://code.google.com/p/go/issues/detail?id=6845
	LDFLAGS="$LDFLAGS -extld clang"
fi

bundle() {
	local bundle="$1"
	shift
	echo "---> Making bundle: $(basename "$bundle") (in $DEST)"
	source "$SCRIPTDIR/make/$bundle" "$@"
}

main() {
	bundle_dir="bundles"
	if [ -n "${PREFIX}" ]; then
		bundle_dir="${PREFIX}/${bundle_dir}"
	fi

	if [ -z "${KEEPBUNDLE-}" ]; then
		echo "Removing ${bundle_dir}/"
		rm -rf "${bundle_dir}"/*
		echo
	fi
	mkdir -p "${bundle_dir}"

	if [ $# -lt 1 ]; then
		bundles=(${DEFAULT_BUNDLES[@]})
	else
		bundles=($@)
	fi
	for bundle in ${bundles[@]}; do
		export DEST="${bundle_dir}/$(basename "$bundle")"
		# Cygdrive paths don't play well with go build -o.
		if [[ "$(uname -s)" == CYGWIN* ]]; then
			export DEST="$(cygpath -mw "$DEST")"
		fi
		mkdir -p "$DEST"
		ABS_DEST="$(cd "$DEST" && pwd -P)"
		bundle "$bundle"
		echo
	done
}

main "$@"
