DEPS := $(shell go list -f '{{range .Imports}}{{.}} {{end}}' ./...)

test: subnet
	go test ./...

integ: subnet
	INTEG_TESTS=yes go test ./...

subnet:
	./test/setup_subnet.sh

cov:
	gocov test github.com/hashicorp/memberlist | gocov-html > /tmp/coverage.html
	open /tmp/coverage.html

deps:
	go get -t -d -v ./...
	echo $(DEPS) | xargs -n1 go get -d

.PHONY: test cov integ
