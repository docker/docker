#!/usr/bin/env bash
# Idempotent cleanup for the nftables load balancing prototype.

set -euo pipefail

PROTOTYPE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${PROTOTYPE_DIR}/lib/common.sh"

require_root
log "teardown"

nft delete table ip moby-ingress 2>/dev/null || true

for name in "$CONTAINER_LB" "$CONTAINER_CLIENT" "${NAT_BACKENDS[@]}" "${DSR_BACKENDS[@]}"; do
	docker rm -f "$name" >/dev/null 2>&1 || true
done

for ifn in \
	veth-lb-o-h veth-lb-g-h \
	veth-nat1-o-h veth-nat1-g-h \
	veth-nat2-o-h veth-nat2-g-h \
	veth-nat3-o-h veth-nat3-g-h \
	veth-dsr1-o-h veth-dsr1-g-h \
	veth-dsr2-o-h veth-dsr2-g-h \
	veth-cli-o-h
do ip link del "$ifn" 2>/dev/null || true; done

ip link del "$GWBR_NAME" 2>/dev/null || true
ip netns del "$OVERLAY_NS" 2>/dev/null || true
ip netns del nftlb-peer 2>/dev/null || true
ip link del ipv1 2>/dev/null || true

rm -f "$(registry_file)"

log "done"
