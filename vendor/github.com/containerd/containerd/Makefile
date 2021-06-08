#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.


# Go command to use for build
GO ?= go

# Root directory of the project (absolute path).
ROOTDIR=$(dir $(abspath $(lastword $(MAKEFILE_LIST))))

# Base path used to install.
DESTDIR ?= /usr/local
TEST_IMAGE_LIST ?=

# Used to populate variables in version package.
VERSION=$(shell git describe --match 'v[0-9]*' --dirty='.m' --always)
REVISION=$(shell git rev-parse HEAD)$(shell if ! git diff --no-ext-diff --quiet --exit-code; then echo .m; fi)
PACKAGE=github.com/containerd/containerd
SHIM_CGO_ENABLED ?= 0

ifneq "$(strip $(shell command -v $(GO) 2>/dev/null))" ""
	GOOS ?= $(shell $(GO) env GOOS)
	GOARCH ?= $(shell $(GO) env GOARCH)
else
	ifeq ($(GOOS),)
		# approximate GOOS for the platform if we don't have Go and GOOS isn't
		# set. We leave GOARCH unset, so that may need to be fixed.
		ifeq ($(OS),Windows_NT)
			GOOS = windows
		else
			UNAME_S := $(shell uname -s)
			ifeq ($(UNAME_S),Linux)
				GOOS = linux
			endif
			ifeq ($(UNAME_S),Darwin)
				GOOS = darwin
			endif
			ifeq ($(UNAME_S),FreeBSD)
				GOOS = freebsd
			endif
		endif
	else
		GOOS ?= $$GOOS
		GOARCH ?= $$GOARCH
	endif
endif

ifndef GODEBUG
	EXTRA_LDFLAGS += -s -w
	DEBUG_GO_GCFLAGS :=
	DEBUG_TAGS :=
else
	DEBUG_GO_GCFLAGS := -gcflags=all="-N -l"
	DEBUG_TAGS := static_build
endif

WHALE = "🇩"
ONI = "👹"

RELEASE=containerd-$(VERSION:v%=%).${GOOS}-${GOARCH}
CRIRELEASE=cri-containerd-$(VERSION:v%=%)-${GOOS}-${GOARCH}
CRICNIRELEASE=cri-containerd-cni-$(VERSION:v%=%)-${GOOS}-${GOARCH}

PKG=github.com/containerd/containerd

# Project binaries.
COMMANDS=ctr containerd containerd-stress
MANPAGES=ctr.8 containerd.8 containerd-config.8 containerd-config.toml.5

ifdef BUILDTAGS
    GO_BUILDTAGS = ${BUILDTAGS}
endif
GO_BUILDTAGS ?=
GO_BUILDTAGS += ${DEBUG_TAGS}
GO_TAGS=$(if $(GO_BUILDTAGS),-tags "$(GO_BUILDTAGS)",)
GO_LDFLAGS=-ldflags '-X $(PKG)/version.Version=$(VERSION) -X $(PKG)/version.Revision=$(REVISION) -X $(PKG)/version.Package=$(PACKAGE) $(EXTRA_LDFLAGS)'
SHIM_GO_LDFLAGS=-ldflags '-X $(PKG)/version.Version=$(VERSION) -X $(PKG)/version.Revision=$(REVISION) -X $(PKG)/version.Package=$(PACKAGE) -extldflags "-static" $(EXTRA_LDFLAGS)'

# Project packages.
PACKAGES=$(shell $(GO) list ${GO_TAGS} ./... | grep -v /vendor/ | grep -v /integration)
TEST_REQUIRES_ROOT_PACKAGES=$(filter \
    ${PACKAGES}, \
    $(shell \
	for f in $$(git grep -l testutil.RequiresRoot | grep -v Makefile); do \
		d="$$(dirname $$f)"; \
		[ "$$d" = "." ] && echo "${PKG}" && continue; \
		echo "${PKG}/$$d"; \
	done | sort -u) \
    )

ifdef SKIPTESTS
    PACKAGES:=$(filter-out ${SKIPTESTS},${PACKAGES})
    TEST_REQUIRES_ROOT_PACKAGES:=$(filter-out ${SKIPTESTS},${TEST_REQUIRES_ROOT_PACKAGES})
endif

#Replaces ":" (*nix), ";" (windows) with newline for easy parsing
GOPATHS=$(shell echo ${GOPATH} | tr ":" "\n" | tr ";" "\n")

TESTFLAGS_RACE=
GO_BUILD_FLAGS=
# See Golang issue re: '-trimpath': https://github.com/golang/go/issues/13809
GO_GCFLAGS=$(shell				\
	set -- ${GOPATHS};			\
	echo "-gcflags=-trimpath=$${1}/src";	\
	)

BINARIES=$(addprefix bin/,$(COMMANDS))

#include platform specific makefile
-include Makefile.$(GOOS)

# Flags passed to `go test`
TESTFLAGS ?= $(TESTFLAGS_RACE) $(EXTRA_TESTFLAGS)
TESTFLAGS_PARALLEL ?= 8

# Use this to replace `go test` with, for instance, `gotestsum`
GOTEST ?= $(GO) test

OUTPUTDIR = $(join $(ROOTDIR), _output)
CRIDIR=$(OUTPUTDIR)/cri

.PHONY: clean all AUTHORS build binaries test integration generate protos checkprotos coverage ci check help install uninstall vendor release mandir install-man genman install-cri-deps cri-release cri-cni-release cri-integration install-deps bin/cri-integration.test
.DEFAULT: default

all: binaries

check: proto-fmt ## run all linters
	@echo "$(WHALE) $@"
	GOGC=75 golangci-lint run

ci: check binaries checkprotos coverage coverage-integration ## to be used by the CI

AUTHORS: .mailmap .git/HEAD
	git log --format='%aN <%aE>' | sort -fu > $@

generate: protos
	@echo "$(WHALE) $@"
	@PATH="${ROOTDIR}/bin:${PATH}" $(GO) generate -x ${PACKAGES}

protos: bin/protoc-gen-gogoctrd ## generate protobuf
	@echo "$(WHALE) $@"
	@PATH="${ROOTDIR}/bin:${PATH}" protobuild --quiet ${PACKAGES}

check-protos: protos ## check if protobufs needs to be generated again
	@echo "$(WHALE) $@"
	@test -z "$$(git status --short | grep ".pb.go" | tee /dev/stderr)" || \
		((git diff | cat) && \
		(echo "$(ONI) please run 'make protos' when making changes to proto files" && false))

check-api-descriptors: protos ## check that protobuf changes aren't present.
	@echo "$(WHALE) $@"
	@test -z "$$(git status --short | grep ".pb.txt" | tee /dev/stderr)" || \
		((git diff $$(find . -name '*.pb.txt') | cat) && \
		(echo "$(ONI) please run 'make protos' when making changes to proto files and check-in the generated descriptor file changes" && false))

proto-fmt: ## check format of proto files
	@echo "$(WHALE) $@"
	@test -z "$$(find . -path ./vendor -prune -o -path ./protobuf/google/rpc -prune -o -name '*.proto' -type f -exec grep -Hn -e "^ " {} \; | tee /dev/stderr)" || \
		(echo "$(ONI) please indent proto files with tabs only" && false)
	@test -z "$$(find . -path ./vendor -prune -o -name '*.proto' -type f -exec grep -Hn "Meta meta = " {} \; | grep -v '(gogoproto.nullable) = false' | tee /dev/stderr)" || \
		(echo "$(ONI) meta fields in proto files must have option (gogoproto.nullable) = false" && false)

build: ## build the go packages
	@echo "$(WHALE) $@"
	@$(GO) build ${DEBUG_GO_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} ${EXTRA_FLAGS} ${GO_LDFLAGS} ${PACKAGES}

test: ## run tests, except integration tests and tests that require root
	@echo "$(WHALE) $@"
	@$(GOTEST) ${TESTFLAGS} ${PACKAGES}

root-test: ## run tests, except integration tests
	@echo "$(WHALE) $@"
	@$(GOTEST) ${TESTFLAGS} ${TEST_REQUIRES_ROOT_PACKAGES} -test.root

integration: ## run integration tests
	@echo "$(WHALE) $@"
	@cd "${ROOTDIR}/integration/client" && $(GO) mod download && $(GOTEST) -v ${TESTFLAGS} -test.root -parallel ${TESTFLAGS_PARALLEL} .

# TODO integrate cri integration bucket with coverage
bin/cri-integration.test:
	@echo "$(WHALE) $@"
	@$(GO) test -c ./integration -o bin/cri-integration.test

cri-integration: binaries bin/cri-integration.test ## run cri integration tests
	@echo "$(WHALE) $@"
	@./script/test/cri-integration.sh
	@rm -rf bin/cri-integration.test

benchmark: ## run benchmarks tests
	@echo "$(WHALE) $@"
	@$(GO) test ${TESTFLAGS} -bench . -run Benchmark -test.root

FORCE:

define BUILD_BINARY
@echo "$(WHALE) $@"
@$(GO) build ${DEBUG_GO_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o $@ ${GO_LDFLAGS} ${GO_TAGS}  ./$<
endef

# Build a binary from a cmd.
bin/%: cmd/% FORCE
	$(call BUILD_BINARY)

bin/containerd-shim: cmd/containerd-shim FORCE # set !cgo and omit pie for a static shim build: https://github.com/golang/go/issues/17789#issuecomment-258542220
	@echo "$(WHALE) bin/containerd-shim"
	@CGO_ENABLED=${SHIM_CGO_ENABLED} $(GO) build ${GO_BUILD_FLAGS} -o bin/containerd-shim ${SHIM_GO_LDFLAGS} ${GO_TAGS} ./cmd/containerd-shim

bin/containerd-shim-runc-v1: cmd/containerd-shim-runc-v1 FORCE # set !cgo and omit pie for a static shim build: https://github.com/golang/go/issues/17789#issuecomment-258542220
	@echo "$(WHALE) bin/containerd-shim-runc-v1"
	@CGO_ENABLED=${SHIM_CGO_ENABLED} $(GO) build ${GO_BUILD_FLAGS} -o bin/containerd-shim-runc-v1 ${SHIM_GO_LDFLAGS} ${GO_TAGS} ./cmd/containerd-shim-runc-v1

bin/containerd-shim-runc-v2: cmd/containerd-shim-runc-v2 FORCE # set !cgo and omit pie for a static shim build: https://github.com/golang/go/issues/17789#issuecomment-258542220
	@echo "$(WHALE) bin/containerd-shim-runc-v2"
	@CGO_ENABLED=${SHIM_CGO_ENABLED} $(GO) build ${GO_BUILD_FLAGS} -o bin/containerd-shim-runc-v2 ${SHIM_GO_LDFLAGS} ${GO_TAGS} ./cmd/containerd-shim-runc-v2

binaries: $(BINARIES) ## build binaries
	@echo "$(WHALE) $@"

man: mandir $(addprefix man/,$(MANPAGES))
	@echo "$(WHALE) $@"

mandir:
	@mkdir -p man

# Kept for backwards compatibility
genman: man/containerd.8 man/ctr.8

man/containerd.8: FORCE
	@echo "$(WHALE) $@"
	$(GO) run cmd/gen-manpages/main.go $(@F) $(@D)

man/ctr.8: FORCE
	@echo "$(WHALE) $@"
	$(GO) run cmd/gen-manpages/main.go $(@F) $(@D)

man/%: docs/man/%.md FORCE
	@echo "$(WHALE) $@"
	go-md2man -in "$<" -out "$@"

define installmanpage
mkdir -p $(DESTDIR)/man/man$(2);
gzip -c $(1) >$(DESTDIR)/man/man$(2)/$(3).gz;
endef

install-man:
	@echo "$(WHALE) $@"
	$(foreach manpage,$(addprefix man/,$(MANPAGES)), $(call installmanpage,$(manpage),$(subst .,,$(suffix $(manpage))),$(notdir $(manpage))))

releases/$(RELEASE).tar.gz: $(BINARIES)
	@echo "$(WHALE) $@"
	@rm -rf releases/$(RELEASE) releases/$(RELEASE).tar.gz
	@install -d releases/$(RELEASE)/bin
	@install $(BINARIES) releases/$(RELEASE)/bin
	@tar -czf releases/$(RELEASE).tar.gz -C releases/$(RELEASE) bin
	@rm -rf releases/$(RELEASE)

release: releases/$(RELEASE).tar.gz
	@echo "$(WHALE) $@"
	@cd releases && sha256sum $(RELEASE).tar.gz >$(RELEASE).tar.gz.sha256sum

# install of cri deps into release output directory
ifeq ($(GOOS),windows)
install-cri-deps: $(BINARIES)
	mkdir -p $(CRIDIR)
	DESTDIR=$(CRIDIR) script/setup/install-cni-windows
	cp bin/* $(CRIDIR)
else
install-cri-deps: $(BINARIES)
	@rm -rf ${CRIDIR}
	@install -d ${CRIDIR}/usr/local/bin
	@install -D -m 755 bin/* ${CRIDIR}/usr/local/bin
	@install -d ${CRIDIR}/opt/containerd/cluster
	@cp -r contrib/gce ${CRIDIR}/opt/containerd/cluster/
	@install -d ${CRIDIR}/etc/systemd/system
	@install -m 644 containerd.service ${CRIDIR}/etc/systemd/system
	echo "CONTAINERD_VERSION: '$(VERSION:v%=%)'" | tee ${CRIDIR}/opt/containerd/cluster/version

	DESTDIR=$(CRIDIR) script/setup/install-runc
	DESTDIR=$(CRIDIR) script/setup/install-cni
	DESTDIR=$(CRIDIR) script/setup/install-critools
	DESTDIR=$(CRIDIR) script/setup/install-imgcrypt

	@install -d $(CRIDIR)/bin
	@install $(BINARIES) $(CRIDIR)/bin
endif

ifeq ($(GOOS),windows)
releases/$(CRIRELEASE).tar.gz: install-cri-deps
	@echo "$(WHALE) $@"
	@cd $(CRIDIR) && tar -czf ../../releases/$(CRIRELEASE).tar.gz *

releases/$(CRICNIRELEASE).tar.gz: install-cri-deps
	@echo "$(WHALE) $@"
	@cd $(CRIDIR) && tar -czf ../../releases/$(CRICNIRELEASE).tar.gz *
else
releases/$(CRIRELEASE).tar.gz: install-cri-deps
	@echo "$(WHALE) $@"
	@tar -czf releases/$(CRIRELEASE).tar.gz -C $(CRIDIR) etc/crictl.yaml etc/systemd usr opt/containerd

releases/$(CRICNIRELEASE).tar.gz: install-cri-deps
	@echo "$(WHALE) $@"
	@tar -czf releases/$(CRICNIRELEASE).tar.gz -C $(CRIDIR) etc usr opt
endif

cri-release: releases/$(CRIRELEASE).tar.gz
	@echo "$(WHALE) $@"
	@cd releases && sha256sum $(CRIRELEASE).tar.gz >$(CRIRELEASE).tar.gz.sha256sum && ln -sf $(CRIRELEASE).tar.gz cri-containerd.tar.gz

cri-cni-release: releases/$(CRICNIRELEASE).tar.gz
	@echo "$(WHALE) $@"
	@cd releases && sha256sum $(CRICNIRELEASE).tar.gz >$(CRICNIRELEASE).tar.gz.sha256sum && ln -sf $(CRICNIRELEASE).tar.gz cri-cni-containerd.tar.gz

clean: ## clean up binaries
	@echo "$(WHALE) $@"
	@rm -f $(BINARIES)
	@rm -f releases/*.tar.gz*
	@rm -rf $(OUTPUTDIR)
	@rm -rf bin/cri-integration.test

clean-test: ## clean up debris from previously failed tests
	@echo "$(WHALE) $@"
	$(eval containers=$(shell find /run/containerd/runc -mindepth 2 -maxdepth 3  -type d -exec basename {} \;))
	$(shell pidof containerd containerd-shim runc | xargs -r -n 1 kill -9)
	@( for container in $(containers); do \
	    grep $$container /proc/self/mountinfo | while read -r mountpoint; do \
		umount $$(echo $$mountpoint | awk '{print $$5}'); \
	    done; \
	    find /sys/fs/cgroup -name $$container -print0 | xargs -r -0 rmdir; \
	done )
	@rm -rf /run/containerd/runc/*
	@rm -rf /run/containerd/fifo/*
	@rm -rf /run/containerd-test/*
	@rm -rf bin/cri-integration.test

install: ## install binaries
	@echo "$(WHALE) $@ $(BINARIES)"
	@mkdir -p $(DESTDIR)/bin
	@install $(BINARIES) $(DESTDIR)/bin

uninstall:
	@echo "$(WHALE) $@"
	@rm -f $(addprefix $(DESTDIR)/bin/,$(notdir $(BINARIES)))

ifeq ($(GOOS),windows)
install-deps:
	# TODO: need a script for hcshim something like containerd/cri/hack/install/windows/install-hcsshim.sh
	script/setup/install-critools
	script/setup/install-cni-windows
else
install-deps: ## install cri dependencies
	script/setup/install-seccomp
	script/setup/install-runc
	script/setup/install-critools
	script/setup/install-cni
endif

coverage: ## generate coverprofiles from the unit tests, except tests that require root
	@echo "$(WHALE) $@"
	@rm -f coverage.txt
	@$(GO) test -i ${TESTFLAGS} ${PACKAGES} 2> /dev/null
	@( for pkg in ${PACKAGES}; do \
		$(GO) test ${TESTFLAGS} \
			-cover \
			-coverprofile=profile.out \
			-covermode=atomic $$pkg || exit; \
		if [ -f profile.out ]; then \
			cat profile.out >> coverage.txt; \
			rm profile.out; \
		fi; \
	done )

root-coverage: ## generate coverage profiles for unit tests that require root
	@echo "$(WHALE) $@"
	@$(GO) test -i ${TESTFLAGS} ${TEST_REQUIRES_ROOT_PACKAGES} 2> /dev/null
	@( for pkg in ${TEST_REQUIRES_ROOT_PACKAGES}; do \
		$(GO) test ${TESTFLAGS} \
			-cover \
			-coverprofile=profile.out \
			-covermode=atomic $$pkg -test.root || exit; \
		if [ -f profile.out ]; then \
			cat profile.out >> coverage.txt; \
			rm profile.out; \
		fi; \
	done )

vendor: ## vendor
	@echo "$(WHALE) $@"
	@$(GO) mod tidy
	@$(GO) mod vendor

help: ## this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST) | sort
