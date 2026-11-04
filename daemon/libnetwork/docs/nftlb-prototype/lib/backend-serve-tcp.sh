#!/usr/bin/env bash
set -euo pipefail
TEXT=${TEXT:?}
while true; do
	while IFS= read -r line; do
		[[ -z "${line//$'\r'/}" ]] && break
	done || break
	printf 'HTTP/1.1 200 OK\r\nConnection: keep-alive\r\nContent-Length: %d\r\n\r\n%s' \
		"${#TEXT}" "$TEXT"
done
