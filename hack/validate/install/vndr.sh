#!/usr/bin/env bash

: "${VNDR_COMMIT:=85886e1ac99b8d96590e6e0d9f075dc7a711d132}" # v0.1.1

install_vndr() {
	echo "Install vndr version $VNDR_COMMIT"
	GO111MODULE=on go get "github.com/LK4d4/vndr@${VNDR_COMMIT}"
}

if ! type vndr; then
    install_vndr
fi