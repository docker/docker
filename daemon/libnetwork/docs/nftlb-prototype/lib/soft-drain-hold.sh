#!/usr/bin/env bash
# Run inside the client container (netshoot). Holds a keep-alive HTTP connection
# to the NAT VIP until it lands on the target backend, waits for a drain signal
# file, then sends further requests on the same TCP socket.
#
# Usage: bash -s <vip> <port> <target_backend_name> <status_file> <continue_file>

set -euo pipefail

VIP=${1:?vip required}
PORT=${2:?port required}
TARGET=${3:?target backend name required}
STATUS=${4:?status file required}
CONTINUE=${5:?continue file required}

: >"$STATUS"
rm -f "$CONTINUE"

send_http_get() {
	printf 'GET / HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n' "$VIP" >&3
}

read_http_body() {
	local line len body
	while IFS= read -r line <&3; do
		line=${line//$'\r'/}
		[[ -z $line ]] && break
		[[ $line == Content-Length:* ]] && len=${line#Content-Length: }
	done
	if [[ -n ${len:-} ]]; then
		body=$(head -c "$len" <&3)
	else
		IFS= read -r body <&3 || true
		body=${body//$'\r'/}
	fi
	printf '%s' "$body"
}

open_connection() {
	exec 3<>"/dev/tcp/${VIP}/${PORT}"
}

close_connection() {
	exec 3<&-
	exec 3>&-
}

attempt=0
while :; do
	attempt=$((attempt + 1))
	if [[ $attempt -gt 200 ]]; then
		echo "ERROR|could not reach ${TARGET} after 200 attempts" >>"$STATUS"
		exit 1
	fi
	open_connection
	send_http_get
	body=$(read_http_body <&3)
	if [[ $body == "$TARGET" || $body == *"$TARGET"* ]]; then
		echo "READY|${TARGET}" >>"$STATUS"
		break
	fi
	close_connection
done

while [[ ! -f $CONTINUE ]]; do
	sleep 0.1
done

send_http_get
second=$(read_http_body <&3)
echo "SECOND|${second}" >>"$STATUS"

send_http_get
third=$(read_http_body <&3)
echo "THIRD|${third}" >>"$STATUS"

close_connection
