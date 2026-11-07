#!/usr/bin/env bash
set -euo pipefail

# Usage: generate-swagger-spec.sh [api-directory]
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
API_DIR="$(cd "${1:-${SCRIPT_DIR}/..}" && pwd)"

# Replace the checked-in specification only after conversion succeeds.
SPEC="$(mktemp "${API_DIR}/swagger.yaml.XXXXXX")"
trap 'rm -f "${SPEC}"' EXIT
(
	cd "${SCRIPT_DIR}/swagger-spec"
	go run . "${API_DIR}/openapi.yaml" "${API_DIR}/swagger.yaml"
) > "${SPEC}"
chmod 644 "${SPEC}"
mv "${SPEC}" "${API_DIR}/swagger.yaml"
