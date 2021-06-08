# -*- mode: ruby -*-
# vi: set ft=ruby :

#   Copyright The containerd Authors.
#
#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

# Vagrantfile for cgroup2 and SELinux
Vagrant.configure("2") do |config|
  config.vm.box = "fedora/34-cloud-base"
  memory = 4096
  cpus = 2
  config.vm.provider :virtualbox do |v|
    v.memory = memory
    v.cpus = cpus
  end
  config.vm.provider :libvirt do |v|
    v.memory = memory
    v.cpus = cpus
  end

  # Disabled by default. To run:
  #   vagrant up --provision-with=upgrade-packages
  # To upgrade only specific packages:
  #   UPGRADE_PACKAGES=selinux vagrant up --provision-with=upgrade-packages
  #
  config.vm.provision "upgrade-packages", type: "shell", run: "never" do |sh|
    sh.upload_path = "/tmp/vagrant-upgrade-packages"
    sh.env = {
        'UPGRADE_PACKAGES': ENV['UPGRADE_PACKAGES'],
    }
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        set -eux -o pipefail
        dnf -y upgrade ${UPGRADE_PACKAGES}
    SHELL
  end

  # To re-run, installing CNI from RPM:
  #   INSTALL_PACKAGES="containernetworking-plugins" vagrant up --provision-with=install-packages
  #
  config.vm.provision "install-packages", type: "shell", run: "once" do |sh|
    sh.upload_path = "/tmp/vagrant-install-packages"
    sh.env = {
        'INSTALL_PACKAGES': ENV['INSTALL_PACKAGES'],
    }
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        set -eux -o pipefail
        dnf -y install \
            container-selinux \
            curl \
            gcc \
            git \
            iptables \
            libseccomp-devel \
            libselinux-devel \
            lsof \
            make \
            ${INSTALL_PACKAGES}
    SHELL
  end

  # To re-run this provisioner, installing a different version of go:
  #   GO_VERSION="1.14.6" vagrant up --provision-with=install-golang
  #
  config.vm.provision "install-golang", type: "shell", run: "once" do |sh|
    sh.upload_path = "/tmp/vagrant-install-golang"
    sh.env = {
        'GO_VERSION': ENV['GO_VERSION'] || "1.16.4",
    }
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        set -eux -o pipefail
        curl -fsSL "https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz" | tar Cxz /usr/local
        cat >> /etc/environment <<EOF
PATH=/usr/local/go/bin:$PATH
EOF
        source /etc/environment
        cat >> /etc/profile.d/sh.local <<EOF
GOPATH=\\$HOME/go
PATH=\\$GOPATH/bin:\\$PATH
export GOPATH PATH
EOF
    source /etc/profile.d/sh.local
    SHELL
  end

  config.vm.provision "setup-gopath", type: "shell", run: "once" do |sh|
    sh.upload_path = "/tmp/vagrant-setup-gopath"
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        source /etc/environment
        source /etc/profile.d/sh.local
        set -eux -o pipefail
        mkdir -p ${GOPATH}/src/github.com/containerd
        ln -fnsv /vagrant ${GOPATH}/src/github.com/containerd/containerd
    SHELL
  end

  config.vm.provision "install-runc", type: "shell", run: "once" do |sh|
    sh.upload_path = "/tmp/vagrant-install-runc"
    sh.env = {
        'RUNC_FLAVOR': ENV['RUNC_FLAVOR'] || "runc",
    }
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        source /etc/environment
        source /etc/profile.d/sh.local
        set -eux -o pipefail
        ${GOPATH}/src/github.com/containerd/containerd/script/setup/install-runc
        type runc
        runc --version
        chcon -v -t container_runtime_exec_t $(type -ap runc)
    SHELL
  end

  config.vm.provision "install-cni", type: "shell", run: "once" do |sh|
    sh.upload_path = "/tmp/vagrant-install-cni"
    sh.env = {
        'CNI_BINARIES': 'bridge dhcp flannel host-device host-local ipvlan loopback macvlan portmap ptp tuning vlan',
    }
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        source /etc/environment
        source /etc/profile.d/sh.local
        set -eux -o pipefail
        ${GOPATH}/src/github.com/containerd/containerd/script/setup/install-cni
        PATH=/opt/cni/bin:$PATH type ${CNI_BINARIES} || true
    SHELL
  end

  config.vm.provision "install-cri-tools", type: "shell", run: "once" do |sh|
    sh.upload_path = "/tmp/vagrant-install-cri-tools"
    sh.env = {
        'CRI_TOOLS_VERSION': ENV['CRI_TOOLS_VERSION'] || '16911795a3c33833fa0ec83dac1ade3172f6989e',
        'GOBIN': '/usr/local/bin',
    }
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        source /etc/environment
        source /etc/profile.d/sh.local
        set -eux -o pipefail
        ${GOPATH}/src/github.com/containerd/containerd/script/setup/install-critools
        type crictl critest
        critest --version
    SHELL
  end

  config.vm.provision "install-containerd", type: "shell", run: "once" do |sh|
    sh.upload_path = "/tmp/vagrant-install-containerd"
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        source /etc/environment
        source /etc/profile.d/sh.local
        set -eux -o pipefail
        cd ${GOPATH}/src/github.com/containerd/containerd
        make BUILDTAGS="seccomp selinux no_aufs no_btrfs no_devmapper no_zfs" binaries install
        type containerd
        containerd --version
        chcon -v -t container_runtime_exec_t /usr/local/bin/{containerd,containerd-shim*}
        ./script/setup/config-containerd
    SHELL
  end

  config.vm.provision "install-gotestsum", type: "shell",  run: "once" do |sh|
      sh.upload_path = "/tmp/vagrant-install-gotestsum"
      sh.inline = <<~SHELL
        #!/usr/bin/env bash
        source /etc/environment
        source /etc/profile.d/sh.local
        set -eux -o pipefail
        ${GOPATH}/src/github.com/containerd/containerd/script/setup/install-gotestsum
        sudo cp ${GOPATH}/bin/gotestsum /usr/local/bin/
      SHELL
  end

  # SELinux is Enforcing by default.
  # To set SELinux as Disabled on a VM that has already been provisioned:
  #   SELINUX=Disabled vagrant up --provision-with=selinux
  # To set SELinux as Permissive on a VM that has already been provsioned
  #   SELINUX=Permissive vagrant up --provision-with=selinux
  config.vm.provision "selinux", type: "shell", run: "never" do |sh|
    sh.upload_path = "/tmp/vagrant-selinux"
    sh.env = {
        'SELINUX': ENV['SELINUX'] || "Enforcing"
    }
    sh.inline = <<~SHELL
        /vagrant/script/setup/config-selinux
        /vagrant/script/setup/config-containerd
    SHELL
  end

  # SELinux is permissive by default (via provisioning) in this VM. To re-run with SELinux enforcing:
  #   vagrant up --provision-with=selinux-enforcing,test-integration
  #
  config.vm.provision "test-integration", type: "shell", run: "never" do |sh|
    sh.upload_path = "/tmp/test-integration"
    sh.env = {
        'RUNC_FLAVOR': ENV['RUNC_FLAVOR'] || "runc",
        'GOTEST': ENV['GOTEST'] || "go test",
        'GOTESTSUM_JUNITFILE': ENV['GOTESTSUM_JUNITFILE'],
    }
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        source /etc/environment
        source /etc/profile.d/sh.local
        set -eux -o pipefail
        rm -rf /var/lib/containerd-test /run/containerd-test
        cd ${GOPATH}/src/github.com/containerd/containerd
        make integration EXTRA_TESTFLAGS="-timeout 15m -no-criu -test.v" TEST_RUNTIME=io.containerd.runc.v2 RUNC_FLAVOR=$RUNC_FLAVOR
    SHELL
  end

  # SELinux is permissive by default (via provisioning) in this VM. To re-run with SELinux enforcing:
  #   vagrant up --provision-with=selinux-enforcing,test-cri
  #
  config.vm.provision "test-cri", type: "shell", run: "never" do |sh|
    sh.upload_path = "/tmp/test-cri"
    sh.env = {
        'GOTEST': ENV['GOTEST'] || "go test",
        'REPORT_DIR': ENV['REPORT_DIR'],
    }
    sh.inline = <<~SHELL
        #!/usr/bin/env bash
        source /etc/environment
        source /etc/profile.d/sh.local
        set -eux -o pipefail
        systemctl disable --now containerd || true
        rm -rf /var/lib/containerd /run/containerd
        function cleanup()
        {
            journalctl -u containerd > /tmp/containerd.log
            systemctl stop containerd
        }
        selinux=$(getenforce)
        if [[ $selinux == Enforcing ]]; then
            setenforce 0
        fi
        systemctl enable --now ${GOPATH}/src/github.com/containerd/containerd/containerd.service
        if [[ $selinux == Enforcing ]]; then
            setenforce 1
        fi
        trap cleanup EXIT
        ctr version
        critest --parallel=$(nproc) --report-dir="${REPORT_DIR}" --ginkgo.skip='HostIpc is true'
    SHELL
  end

end
