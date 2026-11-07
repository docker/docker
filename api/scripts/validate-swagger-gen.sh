#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
API_DIR="${SCRIPT_DIR}/.."

TMP_DIR="$(mktemp -d)"
trap "rm -rf ${TMP_DIR}" EXIT
GEN_FILES=()

echo "Validating generated code..."
echo "Separating generated files from handwritten files..."
while IFS= read -r file; do
	GEN_FILES+=("$file")
done < <(grep -rl "// Code generated" "${API_DIR}/types" || true)

echo "Copying generated files into temporary folder..."
for f in "${GEN_FILES[@]}"; do
	mkdir -p "${TMP_DIR}/$(dirname "${f#${API_DIR}/}")"
	cp "$f" "${TMP_DIR}/${f#${API_DIR}/}"
done

cp "${API_DIR}/openapi.yaml" "${TMP_DIR}/"
cp "${API_DIR}/swagger.yaml" "${TMP_DIR}/"
cp "${API_DIR}/swagger-gen.yaml" "${TMP_DIR}/"
cp "${API_DIR}/go.mod" "${TMP_DIR}/"
cp -r "${API_DIR}/templates" "${TMP_DIR}/" 2> /dev/null || true

echo "Generating swagger types in temporary folder..."
(
	cd "${TMP_DIR}"
	"${SCRIPT_DIR}/generate-swagger-api.sh" "${TMP_DIR}" > "${TMP_DIR}/generate.log" 2>&1 || {
		cat "${TMP_DIR}/generate.log" >&2
		exit 1
	}
)

echo "Run diff for all generated files..."
DIFF_FOUND=false
for f in "${GEN_FILES[@]}"; do
	REL="${f#${API_DIR}/}"
	if ! diff -q "${TMP_DIR}/${REL}" "${API_DIR}/${REL}" > /dev/null 2>&1; then
		echo "Difference found in ${REL}"
		diff -u "${TMP_DIR}/${REL}" "${API_DIR}/${REL}" || true
		DIFF_FOUND=true
	fi
done

if [ "$DIFF_FOUND" = true ]; then
	echo
	echo "Swagger validation failed. Please run:"
	echo "  ./scripts/generate-swagger-api.sh"
	echo "and commit updated generated files."
	exit 1
fi

echo "Swagger file is up to date."
