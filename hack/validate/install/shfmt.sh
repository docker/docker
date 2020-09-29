#!/usr/bin/env bash

: "${SHFMT_COMMIT:=01725bdd30658db1fe1b9e02173c3060061fe86f}" # v3.0.2

install_shfmt() {
	echo "Install shfmt version $SHFMT_COMMIT"
	GO111MODULE=on go get "github.com/mvdan/sh/cmd/shfmt@${SHFMT_COMMIT}"
}

if ! type shfmt; then
    install_shfmt
fi