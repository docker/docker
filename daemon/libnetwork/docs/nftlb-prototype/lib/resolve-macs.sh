#!/usr/bin/env bash
# Resolve overlay MAC addresses for DSR backends.

set -euo pipefail

source "${BASH_SOURCE%/*}/common.sh"

resolve_mac_from_container() {
	local ip=$1
	local container
	for c in "${DSR_BACKENDS[@]}"; do
		local cip
		cip=$(nsenter_container "$c" ip -4 -o addr show dev eth0 2>/dev/null | awk '{print $4}' | cut -d/ -f1)
		if [[ $cip == "$ip" ]]; then
			nsenter_container "$c" ip link show eth0 | awk '/link\/ether/ {print $2; exit}'
			return 0
		fi
	done
	return 1
}

resolve_mac() {
	local ip=$1
	local mac

	mac=$(resolve_mac_from_container "$ip" 2>/dev/null || true)
	if [[ -n $mac ]]; then
		echo "$mac"
		return 0
	fi

	local netns
	netns=$(container_netns "$CONTAINER_LB")
	nsenter_net "$netns" ping -c2 -W1 "$ip" >/dev/null 2>&1 || true
	mac=$(nsenter_net "$netns" ip neigh show "$ip" dev eth0 2>/dev/null | awk '/lladdr/ {print $5; exit}')
	[[ -n $mac ]] || die "could not resolve MAC for $ip"
	echo "$mac"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	require_root
	[[ $# -ge 1 ]] || die "usage: resolve-macs.sh <ip> [ip...]"
	while [[ $# -gt 0 ]]; do
		resolve_mac "$1"
		shift
	done
fi
