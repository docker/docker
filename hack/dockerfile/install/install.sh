#!/bin/bash

set -e
set -x

RM_GOPATH=0

TMP_GOPATH=${TMP_GOPATH:-""}

: "${PREFIX:=/usr/local/bin}"

if [ -z "$TMP_GOPATH" ]; then
	GOPATH="$(mktemp -d)"
	export GOPATH
	RM_GOPATH=1
else
	export GOPATH="$TMP_GOPATH"
fi
case "$(go env GOARCH)" in
	mips* | ppc64)
		# pie build mode is not supported on mips architectures
		export GO_BUILDMODE=""
		;;
	*)
		export GO_BUILDMODE="-buildmode=pie"
		;;
esac

dir="$(dirname $0)"

bin=$1
shift

if [ ! -f "${dir}/${bin}.installer" ]; then
	echo "Could not find installer for \"$bin\""
	exit 1
fi

. ${dir}/${bin}.installer
install_${bin} "$@"
