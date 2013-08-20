#!/bin/bash

# Downloads dependencies into vendor/ directory
if [[ ! -d vendor ]]; then
  mkdir vendor
fi
vendor_dir=${PWD}/vendor

git_clone () {
  PKG=$1
  REV=$2
  if [[ ! -d src/$PKG ]]; then
    cd $vendor_dir && git clone http://$PKG src/$PKG && cd src/$PKG && git checkout -f $REV
  fi
}

git_clone github.com/kr/pty 27435c699

git_clone github.com/gorilla/context/ 708054d61e5

git_clone github.com/gorilla/mux/ 9b36453141c

git_clone github.com/dotcloud/tar/ d06045a6d9

# Docker requires code.google.com/p/go.net/websocket
PKG=code.google.com/p/go.net REV=78ad7f42aa2e
if [[ ! -d src/$PKG ]]; then
  cd $vendor_dir && hg clone https://$PKG src/$PKG && cd src/$PKG && hg checkout -r $REV
fi
