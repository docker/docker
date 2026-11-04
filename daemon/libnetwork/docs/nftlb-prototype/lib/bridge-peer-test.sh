#!/usr/bin/env bash
# Verify host ingress from a peer on another container (bridge / same-L2).

set -euo pipefail

source "${BASH_SOURCE%/*}/common.sh"

readonly PEER_NETNS="nftlb-peer"
readonly PEER_IF="ipv1"
readonly PEER_IP="172.17.0.3"

cleanup_peer() {
	ip netns del "$PEER_NETNS" 2>/dev/null || true
	ip link del "$PEER_IF" 2>/dev/null || true
}

setup_ipvlan_peer() {
	local ingress_ip=$1
	[[ $ingress_ip == 172.17.* ]] || return 1
	cleanup_peer
	ip netns add "$PEER_NETNS"
	ip link add "$PEER_IF" link eth0 type ipvlan mode l2
	ip link set "$PEER_IF" netns "$PEER_NETNS"
	nsenter --net="/var/run/netns/${PEER_NETNS}" ip addr add "${PEER_IP}/16" dev "$PEER_IF"
	nsenter --net="/var/run/netns/${PEER_NETNS}" ip link set "$PEER_IF" up
	nsenter --net="/var/run/netns/${PEER_NETNS}" ip link set lo up
}

peer_curl() {
	local runner=$1
	local url=$2
	case "$runner" in
	docker-bridge)
		docker run --rm --network bridge "$CLIENT_IMAGE" curl -s --max-time 3 "$url"
		;;
	ipvlan)
		nsenter --net="/var/run/netns/${PEER_NETNS}" curl -s --max-time 3 "$url"
		;;
	*)
		die "unknown peer runner: $runner"
		;;
	esac
}

peer_udp_probe() {
	local runner=$1
	local port=$2
	case "$runner" in
	docker-bridge)
		echo -n probe | docker run --rm -i --network bridge "$CLIENT_IMAGE" \
			nc -u -w1 "${INGRESS_IP}" "$port" 2>/dev/null || true
		;;
	ipvlan)
		echo -n probe | nsenter --net="/var/run/netns/${PEER_NETNS}" \
			nc -u -w1 "${INGRESS_IP}" "$port" 2>/dev/null || true
		;;
	*)
		die "unknown peer runner: $runner"
		;;
	esac
}

check_service() {
	local runner=$1
	local port=$2
	local label=$3
	local url="http://${INGRESS_IP}:${port}"
	local out
	out=$(peer_curl "$runner" "$url" || true)
	if [[ -n $out ]]; then
		log "PASS ${label} (${runner}): ${out}"
		return 0
	fi
	log "FAIL ${label} (${runner}): no response from ${url}"
	return 1
}

check_udp_service() {
	local runner=$1
	local port=$2
	local label=$3
	local out
	out=$(peer_udp_probe "$runner" "$port")
	if [[ -n $out ]]; then
		log "PASS ${label} (${runner}): ${out}"
		return 0
	fi
	log "FAIL ${label} (${runner}): no UDP response on port ${port}"
	return 1
}

# Published-port DSR ingress is SNAT'd onto gwbr0 (backends see gw IP, not the peer).
check_ingress_dsr_snat() {
	local runner=$1
	local peer_ip=$2
	local url="http://${INGRESS_IP}:${DSR_PUBLISH_PORT}"
	local gw="${GWBR_HOST_IP}"

	peer_curl "$runner" "$url" >/dev/null &
	local pid=$!
	sleep 0.15
	local snat=0
	if grep -E "src=${peer_ip//./\\.} .*dport=${DSR_PUBLISH_PORT} .*dst=${gw//./\\.}" \
		/proc/net/nf_conntrack 2>/dev/null | grep -q .; then
		snat=1
	fi
	wait "$pid" 2>/dev/null || true
	if [[ $snat -eq 1 ]]; then
		log "PASS ingress DSR SNAT (${runner}): host masquerades ${peer_ip} onto gwbr0 (${gw})"
		return 0
	fi
	log "FAIL ingress DSR SNAT (${runner}): expected gw ${gw} in conntrack for ${peer_ip}"
	return 1
}

# Overlay VIP DSR: no IP SNAT; client IP preserved (MAC rewrite + port redirect only).
check_overlay_dsr_client_ip() {
	local url="http://${DSR_VIP}:${DSR_TARGET_PORT}"
	local client="${CLIENT_IP}"
	local ok=0
	local b

	docker exec "$CONTAINER_CLIENT" curl -s --max-time 3 "$url" >/dev/null &
	local pid=$!
	sleep 0.15
	for b in "${DSR_BACKENDS[@]}"; do
		if nsenter --net="$(container_netns "$b")" \
			grep -E "src=${client//./\\.} dst=${DSR_VIP//./\\.} .*dport=${DSR_TARGET_PORT} " \
			/proc/net/nf_conntrack 2>/dev/null | grep -q .; then
			ok=1
			break
		fi
	done
	wait "$pid" 2>/dev/null || true
	if [[ $ok -eq 1 ]]; then
		log "PASS overlay DSR client IP: backend conntrack shows ${client} (no IP SNAT)"
		return 0
	fi
	log "FAIL overlay DSR client IP: expected backend to see overlay client ${client}"
	return 1
}

main() {
	require_root
	require_cmds docker ip nsenter curl nc

	local ingress_ip
	ingress_ip=$(host_ingress_ip)
	[[ -n $ingress_ip ]] || die "could not detect ingress IP"

	INGRESS_IP=$ingress_ip
	log "ingress IP for peer clients: ${INGRESS_IP}"
	log "NAT published port: ${NAT_PUBLISH_PORT}, DSR published port: ${DSR_PUBLISH_PORT}"

	trap cleanup_peer EXIT

	local pass=0 fail=0
	local runner peer_ip
	for runner in docker-bridge; do
		peer_ip=$(docker run --rm --network bridge "$CLIENT_IMAGE" ip -4 -o addr show eth0 \
			| awk '{print $4}' | cut -d/ -f1)
		check_service "$runner" "$NAT_PUBLISH_PORT" "bridge peer NAT" && pass=$((pass + 1)) || fail=$((fail + 1))
		check_service "$runner" "$DSR_PUBLISH_PORT" "bridge peer DSR" && pass=$((pass + 1)) || fail=$((fail + 1))
		check_udp_service "$runner" "$NAT_UDP_PUBLISH_PORT" "bridge peer NAT UDP" && pass=$((pass + 1)) || fail=$((fail + 1))
		check_udp_service "$runner" "$DSR_UDP_PUBLISH_PORT" "bridge peer DSR UDP" && pass=$((pass + 1)) || fail=$((fail + 1))
		check_ingress_dsr_snat "$runner" "$peer_ip" && pass=$((pass + 1)) || fail=$((fail + 1))
	done

	if setup_ipvlan_peer "$ingress_ip"; then
		log "same-L2 ipvlan peer on eth0 (${PEER_IP})"
		check_service ipvlan "$NAT_PUBLISH_PORT" "ipvlan peer NAT" && pass=$((pass + 1)) || fail=$((fail + 1))
		check_service ipvlan "$DSR_PUBLISH_PORT" "ipvlan peer DSR" && pass=$((pass + 1)) || fail=$((fail + 1))
		check_udp_service ipvlan "$NAT_UDP_PUBLISH_PORT" "ipvlan peer NAT UDP" && pass=$((pass + 1)) || fail=$((fail + 1))
		check_udp_service ipvlan "$DSR_UDP_PUBLISH_PORT" "ipvlan peer DSR UDP" && pass=$((pass + 1)) || fail=$((fail + 1))
		check_ingress_dsr_snat ipvlan "$PEER_IP" && pass=$((pass + 1)) || fail=$((fail + 1))
	else
		log "skip ipvlan peer (ingress IP ${ingress_ip} is not on eth0 172.17.0.0/16)"
	fi

	check_overlay_dsr_client_ip && pass=$((pass + 1)) || fail=$((fail + 1))

	log "bridge peer results: ${pass} passed, ${fail} failed"
	[[ $fail -eq 0 ]]
}

main "$@"
