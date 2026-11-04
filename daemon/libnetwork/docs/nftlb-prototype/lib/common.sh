#!/usr/bin/env bash
# Shared helpers for the nftables load balancing prototype.

[[ -n "${NFTLB_COMMON_LOADED:-}" ]] && return 0
NFTLB_COMMON_LOADED=1

set -euo pipefail

PROTOTYPE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Network constants (mirror Swarm address plan)
readonly GWBR_NAME="gwbr0"
readonly GWBR_SUBNET="172.18.0.0/16"
readonly GWBR_HOST_IP="172.18.0.1"
readonly LB_GW_IP="172.18.0.2"
readonly OVERLAY_NS="nftlb-overlay"
readonly OVERLAY_BR="br-overlay"
readonly OVERLAY_SUBNET="10.255.0.0/24"
readonly OVERLAY_GW="10.255.0.1"
readonly LB_OVERLAY_IP="10.255.0.2"

readonly NAT_VIP="10.255.0.10"
readonly NAT_PUBLISH_PORT="8080"
readonly NAT_TARGET_PORT="80"
readonly NAT_UDP_PUBLISH_PORT="8080"
readonly NAT_UDP_TARGET_PORT="8123"

readonly DSR_VIP="10.255.0.20"
readonly DSR_PUBLISH_PORT="9000"
readonly DSR_TARGET_PORT="8080"
readonly DSR_UDP_PUBLISH_PORT="9000"
readonly DSR_UDP_TARGET_PORT="8124"

readonly LB_BUCKETS=1024

readonly CONTAINER_LB="nftlb-lb-sbox"
readonly CONTAINER_CLIENT="nftlb-client"
readonly CLIENT_IMAGE="nicolaka/netshoot"
readonly NAT_BACKENDS=(nftlb-nat-backend-1 nftlb-nat-backend-2 nftlb-nat-backend-3)
readonly NAT_BACKEND_IPS=(10.255.0.11 10.255.0.12 10.255.0.13)
readonly DSR_BACKENDS=(nftlb-dsr-backend-1 nftlb-dsr-backend-2)
readonly DSR_BACKEND_IPS=(10.255.0.21 10.255.0.22)
readonly CLIENT_IP="10.255.0.100"

log() {
	echo "[nftlb] $*"
}

warn() {
	echo "[nftlb] WARNING: $*" >&2
}

# Primary IPv4 used for routed egress (best-effort host ingress address).
host_ingress_ip() {
	ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "src") print $(i + 1)}'
}

# True when setup runs inside a container without host networking.
in_isolated_container_netns() {
	[[ -f /.dockerenv ]] || return 1
	local ingress_ip
	ingress_ip=$(host_ingress_ip)
	[[ -n $ingress_ip ]] || return 1
	# Docker default bridge / custom bridge container IPs are not LAN-routable.
	[[ $ingress_ip == 172.1[6789].* ]]
}

die() {
	echo "[nftlb] ERROR: $*" >&2
	exit 1
}

require_root() {
	[[ ${EUID:-$(id -u)} -eq 0 ]] || die "must run as root"
}

require_cmds() {
	local cmd
	for cmd in "$@"; do
		command -v "$cmd" >/dev/null 2>&1 || die "required command not found: $cmd"
	done
}

container_pid() {
	local name=$1
	local pid
	pid=$(docker inspect -f '{{.State.Pid}}' "$name" 2>/dev/null) || die "container not found: $name"
	[[ "$pid" != "0" ]] || die "container not running: $name"
	echo "$pid"
}

container_netns() {
	docker inspect --format='{{.NetworkSettings.SandboxKey}}' "$1"
}

nsenter_net() {
	local netns=$1
	shift
	nsenter --net="$netns" "$@"
}

nsenter_container() {
	local name=$1
	shift
	nsenter --net="$(container_netns "$name")" "$@"
}

nft_load() {
	local netns=$1
	shift
	nsenter_net "$netns" nft -f "$@"
}

nft_run() {
	local netns=$1
	shift
	nsenter_net "$netns" nft "$@"
}

nft_host() {
	nft "$@"
}

registry_file() {
	echo "${PROTOTYPE_DIR}/.nftlb-registry.json"
}

ensure_registry() {
	local f
	f=$(registry_file)
	if [[ ! -f $f ]]; then
		echo '{"services":{},"backends":{},"bucket_state":{}}' >"$f"
	fi
}
