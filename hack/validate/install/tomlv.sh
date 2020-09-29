#!/bin/sh

# When updating TOMLV_COMMIT, consider updating github.com/BurntSushi/toml
# in vendor.conf accordingly
: ${TOMLV_COMMIT:=3012a1dbe2e4bd1391d42b32f0577cb7bbc7f005} # v0.3.1

install_tomlv() {
	echo "Install tomlv version $TOMLV_COMMIT"
	GO111MODULE=on go get "github.com/BurntSushi/toml/cmd/tomlv@${TOMLV_COMMIT}"
}

if ! type tomlv; then
    install_tomlv
fi