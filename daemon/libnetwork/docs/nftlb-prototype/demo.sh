#!/usr/bin/env bash
# Demonstrate load balancing scenarios from the prototype plan.

set -euo pipefail

PROTOTYPE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${PROTOTYPE_DIR}/lib/common.sh"

client_curl() {
	local url=$1
	docker exec "$CONTAINER_CLIENT" curl -s --max-time 2 "$url"
}

# Send one UDP datagram and print the response body.
udp_probe() {
	local host=$1 port=$2
	echo -n probe | docker exec -i "$CONTAINER_CLIENT" nc -u -w2 "$host" "$port" 2>/dev/null || true
}

host_udp_probe() {
	local host=$1 port=$2
	if in_isolated_container_netns; then
		echo -n probe | docker run --rm -i --network bridge "$CLIENT_IMAGE" \
			nc -u -w2 "$host" "$port" 2>/dev/null || true
	else
		echo -n probe | nc -u -w2 "$host" "$port" 2>/dev/null || true
	fi
}

require_cmds curl docker nc

pass=0
fail=0

check_rr() {
	local label=$1
	shift
	local -a responses=("$@")
	local -A seen=()
	local r nonempty=0
	for r in "${responses[@]}"; do
		[[ -z $r ]] && continue
		nonempty=1
		seen[$r]=1
	done
	if [[ $nonempty -eq 0 ]]; then
		log "FAIL ${label}: no responses (empty)"
		fail=$((fail + 1))
		return
	fi
	local unique=${#seen[@]}
	if [[ $unique -ge 2 ]]; then
		log "PASS ${label}: saw ${unique} distinct backends (${responses[*]})"
		pass=$((pass + 1))
	else
		log "FAIL ${label}: expected multiple backends, got: ${responses[*]}"
		fail=$((fail + 1))
	fi
}

demo_host_nat() {
	log "=== Demo 1: host ingress NAT TCP (${NAT_PUBLISH_PORT}, target ${NAT_TARGET_PORT}) ==="
	local -a resp=()
	local i out
	for i in $(seq 1 6); do
		out=$(curl -s --max-time 2 "127.0.0.1:${NAT_PUBLISH_PORT}" || true)
		resp+=("$out")
		echo "  request $i -> $out"
	done
	check_rr "host NAT RR" "${resp[@]}"
}

demo_host_dsr() {
	log "=== Demo 2: host ingress DSR TCP (${DSR_PUBLISH_PORT}, target ${DSR_TARGET_PORT}) ==="
	log "  (published-port ingress is NAT'ed irrespective of the load-balancer mode)"
	local -a resp=()
	local i out
	for i in $(seq 1 6); do
		out=$(curl -s --max-time 2 "127.0.0.1:${DSR_PUBLISH_PORT}" || true)
		resp+=("$out")
		echo "  request $i -> $out"
	done
	check_rr "host DSR RR" "${resp[@]}"
}

demo_overlay_nat() {
	log "=== Demo 3: overlay VIP NAT TCP (${NAT_VIP}:${NAT_TARGET_PORT}) ==="
	local -a resp=()
	local i out
	for i in $(seq 1 6); do
		out=$(client_curl "${NAT_VIP}:${NAT_TARGET_PORT}" || true)
		resp+=("$out")
		echo "  request $i -> $out"
	done
	check_rr "overlay NAT RR" "${resp[@]}"
}

demo_overlay_dsr() {
	log "=== Demo 4: overlay VIP DSR TCP (${DSR_VIP}:${DSR_TARGET_PORT}) ==="
	local -a resp=()
	local i out
	for i in $(seq 1 6); do
		out=$(client_curl "${DSR_VIP}:${DSR_TARGET_PORT}" || true)
		resp+=("$out")
		echo "  request $i -> $out"
	done
	check_rr "overlay DSR RR" "${resp[@]}"
}

demo_host_nat_udp() {
	log "=== Demo 5: host ingress NAT UDP (${NAT_UDP_PUBLISH_PORT}, target ${NAT_UDP_TARGET_PORT}) ==="
	local -a resp=()
	local i out
	for i in $(seq 1 6); do
		out=$(echo -n probe | nc -u -w2 127.0.0.1 "$NAT_UDP_PUBLISH_PORT" 2>/dev/null || true)
		resp+=("$out")
		echo "  request $i -> $out"
	done
	check_rr "host NAT UDP RR" "${resp[@]}"
}

demo_host_dsr_udp() {
	log "=== Demo 6: host ingress DSR UDP (${DSR_UDP_PUBLISH_PORT}, target ${DSR_UDP_TARGET_PORT}) ==="
	local -a resp=()
	local i out
	for i in $(seq 1 6); do
		out=$(echo -n probe | nc -u -w2 127.0.0.1 "$DSR_UDP_PUBLISH_PORT" 2>/dev/null || true)
		resp+=("$out")
		echo "  request $i -> $out"
	done
	check_rr "host DSR UDP RR" "${resp[@]}"
}

demo_overlay_nat_udp() {
	log "=== Demo 7: overlay VIP NAT UDP (${NAT_VIP}:${NAT_UDP_TARGET_PORT}) ==="
	local -a resp=()
	local i out
	for i in $(seq 1 6); do
		out=$(udp_probe "$NAT_VIP" "$NAT_UDP_TARGET_PORT")
		resp+=("$out")
		echo "  request $i -> $out"
	done
	check_rr "overlay NAT UDP RR" "${resp[@]}"
}

demo_overlay_dsr_udp() {
	log "=== Demo 8: overlay VIP DSR UDP (${DSR_VIP}:${DSR_UDP_TARGET_PORT}) ==="
	local -a resp=()
	local i out
	for i in $(seq 1 6); do
		out=$(udp_probe "$DSR_VIP" "$DSR_UDP_TARGET_PORT")
		resp+=("$out")
		echo "  request $i -> $out"
	done
	check_rr "overlay DSR UDP RR" "${resp[@]}"
}

demo_soft_drain() {
	log "=== Demo 9: soft drain nat backend ${NAT_BACKEND_IPS[1]} (persistent connection) ==="
	local target_ip=${NAT_BACKEND_IPS[1]}
	local target_name=${NAT_BACKENDS[1]}
	local hold_script="${PROTOTYPE_DIR}/lib/soft-drain-hold.sh"
	local status_file="/tmp/nftlb-hold-status"
	local continue_file="/tmp/nftlb-drain-continue"
	local second third line

	docker exec "$CONTAINER_CLIENT" rm -f "$status_file" "$continue_file"
	docker cp "$hold_script" "${CONTAINER_CLIENT}:/tmp/soft-drain-hold.sh"

	docker exec -d "$CONTAINER_CLIENT" bash /tmp/soft-drain-hold.sh \
		"$NAT_VIP" "$NAT_TARGET_PORT" "$target_name" "$status_file" "$continue_file"

	local ready=0 attempt=0
	while [[ $ready -eq 0 && $attempt -lt 100 ]]; do
		if docker exec "$CONTAINER_CLIENT" test -f "$status_file" 2>/dev/null; then
			line=$(docker exec "$CONTAINER_CLIENT" grep '^READY|' "$status_file" 2>/dev/null | head -1 || true)
			if [[ -n $line ]]; then
				echo "  hold: $line"
				ready=1
				break
			fi
			if docker exec "$CONTAINER_CLIENT" grep -q '^ERROR|' "$status_file" 2>/dev/null; then
				line=$(docker exec "$CONTAINER_CLIENT" grep '^ERROR|' "$status_file" | head -1)
				log "FAIL soft-drain: ${line#ERROR|}"
				fail=$((fail + 1))
				return
			fi
		fi
		attempt=$((attempt + 1))
		sleep 0.1
	done

	if [[ $ready -eq 0 ]]; then
		log "FAIL soft-drain: timed out waiting for persistent connection to ${target_name}"
		fail=$((fail + 1))
		return
	fi

	if [[ ${line#READY|} != "$target_name" ]]; then
		log "FAIL soft-drain: expected READY from ${target_name}, got: ${line}"
		fail=$((fail + 1))
		return
	fi

	log "draining ${target_name} (${target_ip}) while holding established connection"
	"${PROTOTYPE_DIR}/nft-ctl.sh" remove-backend svc-nat "$target_name"

	docker exec "$CONTAINER_CLIENT" touch "$continue_file"

	attempt=0
	second=
	third=
	while [[ $attempt -lt 100 ]]; do
		if docker exec "$CONTAINER_CLIENT" test -f "$status_file" 2>/dev/null; then
			second=$(docker exec "$CONTAINER_CLIENT" grep '^SECOND|' "$status_file" 2>/dev/null | head -1 | cut -d'|' -f2- || true)
			third=$(docker exec "$CONTAINER_CLIENT" grep '^THIRD|' "$status_file" 2>/dev/null | head -1 | cut -d'|' -f2- || true)
			if [[ -n $second && -n $third ]]; then
				echo "  hold: SECOND|${second}"
				echo "  hold: THIRD|${third}"
				break
			fi
		fi
		attempt=$((attempt + 1))
		sleep 0.1
	done

	if [[ $second == "$target_name" && $third == "$target_name" ]]; then
		log "PASS soft-drain: established connection still served by ${target_name} (SECOND/THIRD)"
	else
		log "FAIL soft-drain: established connection broken (second='${second}' third='${third}')"
		fail=$((fail + 1))
		return
	fi

	local saw_other=0
	local i out
	for i in $(seq 1 12); do
		out=$(docker exec "$CONTAINER_CLIENT" curl -s --max-time 2 "${NAT_VIP}:${NAT_TARGET_PORT}" || true)
		echo "  post-drain new-flow request $i -> $out"
		if [[ $out == *"${target_name}"* ]]; then
			log "FAIL soft-drain: new flow reached drained backend ${target_name}"
			fail=$((fail + 1))
			return
		fi
		if [[ -n $out && $out != *"${target_name}"* ]]; then
			saw_other=1
		fi
	done

	if [[ $saw_other -eq 1 ]]; then
		log "PASS soft-drain: new flows avoid drained backend ${target_name}"
		pass=$((pass + 1))
	else
		log "FAIL soft-drain: no responses from remaining backends on new flows"
		fail=$((fail + 1))
	fi
}

main() {
	require_root
	demo_host_nat
	demo_host_dsr
	demo_overlay_nat
	demo_overlay_dsr
	demo_host_nat_udp
	demo_host_dsr_udp
	demo_overlay_nat_udp
	demo_overlay_dsr_udp
	demo_soft_drain
	echo ""
	log "results: ${pass} passed, ${fail} failed"
	[[ $fail -eq 0 ]]
}

main "$@"
