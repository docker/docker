# -*- mode: ruby -*-
# vi: set ft=ruby :

Vagrant.configure("2") do |config|
# Fedora box is used for testing cgroup v2 support
  config.vm.box = "fedora/32-cloud-base"
  config.vm.provider :virtualbox do |v|
    v.memory = 2048
    v.cpus = 2
  end
  config.vm.provider :libvirt do |v|
    v.memory = 2048
    v.cpus = 2
  end
  config.vm.provision "shell", inline: <<-SHELL
    set -eux -o pipefail
    # configuration
    GO_VERSION="1.15"

    # install gcc and Golang
    dnf -y install gcc
    curl -fsSL "https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz" | tar Cxz /usr/local

    # setup env vars
    cat >> /etc/profile.d/sh.local <<EOF
PATH=/usr/local/go/bin:$PATH
GO111MODULE=on
export PATH GO111MODULE
EOF
    source /etc/profile.d/sh.local

    # enter /root/go/src/github.com/containerd/cgroups
    mkdir -p /root/go/src/github.com/containerd
    ln -s /vagrant /root/go/src/github.com/containerd/cgroups
    cd /root/go/src/github.com/containerd/cgroups

    # create /test.sh
    cat > /test.sh <<EOF
#!/bin/bash
set -eux -o pipefail
cd /root/go/src/github.com/containerd/cgroups
go test -v ./...
EOF
    chmod +x /test.sh
  SHELL
end
