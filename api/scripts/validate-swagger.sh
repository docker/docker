#!/usr/bin/env bash
set -e

# Expected to be in api directory
cd "$(dirname "${BASH_SOURCE[0]}")/.."

echo "Validating openapi.yaml..."

fy-tool --yaml-1.2 --null-output openapi.yaml
yamllint -f parsable -c validate/yamllint.yaml openapi.yaml

(
	cd scripts/swagger-spec
	# The test cache ignores the specifications outside this module.
	go test -count=1 ./...
)

if out=$(openapi-spec-validator --schema 3.2 openapi.yaml); then
	echo "Validation done! ${out}"
else
	echo "${out}" >&2
	false
fi

echo "Validating swagger.yaml..."
fy-tool --yaml-1.2 --null-output swagger.yaml
swagger validate swagger.yaml
