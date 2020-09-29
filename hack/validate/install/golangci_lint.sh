#!/bin/sh

: "${GOLANGCI_LINT_COMMIT=v1.23.8}"

install_golangci_lint() {
	echo "Installing golangci-lint version ${GOLANGCI_LINT_COMMIT}"
	GO111MODULE=on go get "github.com/golangci/golangci-lint/cmd/golangci-lint@${GOLANGCI_LINT_COMMIT}"
}


if ! type golangci-lint; then
    install_golangci_lint
fi