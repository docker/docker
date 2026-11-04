#!/usr/bin/env bash
# Main setup: topology, static nft rulesets, nft-ctl population.

set -euo pipefail

PROTOTYPE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${PROTOTYPE_DIR}/lib/common.sh"

configure_iface_rp_filter() {
	local iface=$1
	# sysctl paths use the netdev name without "@ifindex" suffix.
	iface="${iface%%@*}"
	sysctl -w "net.ipv4.conf.${iface}.rp_filter=0" 2>/dev/null || true
}

# Host sysctls required for ingress forwarding and localhost published-port access.
configure_host_sysctls() {
	sysctl -w net.ipv4.ip_forward=1 >/dev/null
	sysctl -w "net.ipv4.conf.${GWBR_NAME}.route_localnet=1" >/dev/null
	configure_iface_rp_filter "$GWBR_NAME"
	ip route replace "${OVERLAY_SUBNET}" dev "${GWBR_NAME}" 2>/dev/null || true
}

create_host_gwbridge() {
	if ! ip link show "$GWBR_NAME" >/dev/null 2>&1; then
		ip link add name "$GWBR_NAME" type bridge
	fi
	ip addr flush dev "$GWBR_NAME" 2>/dev/null || true
	ip addr add "${GWBR_HOST_IP}/16" dev "$GWBR_NAME"
	ip link set "$GWBR_NAME" up
	configure_host_sysctls
}

create_overlay_ns() {
	if ! ip netns list | grep -qw "$OVERLAY_NS"; then
		ip netns add "$OVERLAY_NS"
	fi
	nsenter --net="/var/run/netns/${OVERLAY_NS}" ip link set lo up
	if ! nsenter --net="/var/run/netns/${OVERLAY_NS}" ip link show "$OVERLAY_BR" >/dev/null 2>&1; then
		nsenter --net="/var/run/netns/${OVERLAY_NS}" ip link add name "$OVERLAY_BR" type bridge
	fi
	nsenter --net="/var/run/netns/${OVERLAY_NS}" ip addr flush dev "$OVERLAY_BR" 2>/dev/null || true
	nsenter --net="/var/run/netns/${OVERLAY_NS}" ip addr add "${OVERLAY_GW}/24" dev "$OVERLAY_BR"
	nsenter --net="/var/run/netns/${OVERLAY_NS}" ip link set "$OVERLAY_BR" up
}

start_container() {
	local name=$1
	local tcp_port=$2
	local udp_port=$3
	local text=$4

	if docker ps -a --format '{{.Names}}' | grep -qx "$name"; then
		docker rm -f "$name" >/dev/null 2>&1 || true
	fi

	docker run -d --name "$name" --net=none \
		-v "${PROTOTYPE_DIR}/lib/backend-serve.sh:/backend-serve.sh:ro" \
		-v "${PROTOTYPE_DIR}/lib/backend-serve-tcp.sh:/backend-serve-tcp.sh:ro" \
		-e TEXT="$text" -e TCP_PORT="$tcp_port" -e UDP_PORT="$udp_port" \
		"$CLIENT_IMAGE" bash /backend-serve.sh >/dev/null
}

start_lb_container() {
	if docker ps -a --format '{{.Names}}' | grep -qx "$CONTAINER_LB"; then
		docker rm -f "$CONTAINER_LB" >/dev/null 2>&1 || true
	fi

	docker run -d --name "$CONTAINER_LB" --net=none \
		"$CLIENT_IMAGE" sleep infinity >/dev/null
}

start_client_container() {
	if docker ps -a --format '{{.Names}}' | grep -qx "$CONTAINER_CLIENT"; then
		docker rm -f "$CONTAINER_CLIENT" >/dev/null 2>&1 || true
	fi

	docker run -d --name "$CONTAINER_CLIENT" --net=none \
		"$CLIENT_IMAGE" sleep infinity >/dev/null
}

start_all_containers() {
	log "starting containers"
	start_lb_container
	for i in "${!NAT_BACKENDS[@]}"; do
		start_container "${NAT_BACKENDS[$i]}" "$NAT_TARGET_PORT" "$NAT_UDP_TARGET_PORT" \
			"${NAT_BACKENDS[$i]}"
	done
	for i in "${!DSR_BACKENDS[@]}"; do
		start_container "${DSR_BACKENDS[$i]}" "$DSR_TARGET_PORT" "$DSR_UDP_TARGET_PORT" \
			"${DSR_BACKENDS[$i]}"
	done
	start_client_container
}

# Create veth: host end name, peer moves to target netns as peer_name, attach host end to bridge
link_veth_to_bridge() {
	local host_if=$1
	local peer_if=$2
	local bridge=$3
	local netns=$4

	ip link del "$host_if" 2>/dev/null || true
	ip link add "$host_if" type veth peer name "$peer_if"
	ip link set "$peer_if" netns "$netns"
	ip link set "$host_if" master "$bridge"
	ip link set "$host_if" up
	nsenter --net="$netns" ip link set "$peer_if" up
}

# Link veth into overlay netns bridge from host side
link_veth_overlay() {
	local host_if=$1
	local peer_if=$2
	local overlay_ip=$3

	link_veth_to_bridge "$host_if" "$peer_if" "$OVERLAY_BR" "/var/run/netns/${OVERLAY_NS}"
	nsenter --net="/var/run/netns/${OVERLAY_NS}" ip addr add "${overlay_ip}/24" dev "$peer_if"
}

# Link container overlay iface via veth into overlay bridge
link_container_overlay() {
	local container=$1
	local host_if=$2
	local peer_if=$3
	local overlay_ip=$4

	local pid
	pid=$(container_pid "$container")
	ip link del "$host_if" 2>/dev/null || true
	ip link add "$host_if" type veth peer name "$peer_if"
	ip link set "$host_if" netns "/var/run/netns/${OVERLAY_NS}"
	ip link set "$peer_if" netns "/proc/${pid}/ns/net"
	nsenter --net="/var/run/netns/${OVERLAY_NS}" ip link set "$host_if" master "$OVERLAY_BR"
	nsenter --net="/var/run/netns/${OVERLAY_NS}" ip link set "$host_if" up
	nsenter --net="/var/run/netns/${OVERLAY_NS}" ip addr add "${overlay_ip}/24" dev "$host_if"
	nsenter --net="/proc/${pid}/ns/net" ip link set "$peer_if" name eth0
	nsenter --net="/proc/${pid}/ns/net" ip addr add "${overlay_ip}/24" dev eth0
	nsenter --net="/proc/${pid}/ns/net" ip link set eth0 up
	nsenter --net="/proc/${pid}/ns/net" ip route add default via "$OVERLAY_GW"
}

# Link container gw iface via veth to host gwbridge
link_container_gw() {
	local container=$1
	local host_if=$2
	local peer_if=$3
	local gw_ip=$4

	local pid
	pid=$(container_pid "$container")
	ip link del "$host_if" 2>/dev/null || true
	ip link add "$host_if" type veth peer name "$peer_if"
	ip link set "$host_if" master "$GWBR_NAME"
	ip link set "$host_if" up
	configure_iface_rp_filter "$host_if"
	ip link set "$peer_if" netns "/proc/${pid}/ns/net"
	nsenter --net="/proc/${pid}/ns/net" ip link set "$peer_if" name eth1
	nsenter --net="/proc/${pid}/ns/net" ip addr add "${gw_ip}/16" dev eth1
	nsenter --net="/proc/${pid}/ns/net" ip link set eth1 up
}

wire_topology() {
	log "wiring network topology"

	# lb-sbox: eth0 overlay, eth1 gwbridge
	link_container_overlay "$CONTAINER_LB" veth-lb-o-h veth-lb-o-c "$LB_OVERLAY_IP"
	link_container_gw "$CONTAINER_LB" veth-lb-g-h veth-lb-g-c "$LB_GW_IP"

	# NAT backends
	for i in "${!NAT_BACKENDS[@]}"; do
		local idx=$((i + 1))
		link_container_overlay "${NAT_BACKENDS[$i]}" "veth-nat${idx}-o-h" "veth-nat${idx}-o-c" "${NAT_BACKEND_IPS[$i]}"
		link_container_gw "${NAT_BACKENDS[$i]}" "veth-nat${idx}-g-h" "veth-nat${idx}-g-c" "172.18.0.$((10 + idx))"
	done

	# DSR backends
	for i in "${!DSR_BACKENDS[@]}"; do
		local idx=$((i + 1))
		link_container_overlay "${DSR_BACKENDS[$i]}" "veth-dsr${idx}-o-h" "veth-dsr${idx}-o-c" "${DSR_BACKEND_IPS[$i]}"
		link_container_gw "${DSR_BACKENDS[$i]}" "veth-dsr${idx}-g-h" "veth-dsr${idx}-g-c" "172.18.0.$((20 + idx))"
	done

	# Client: overlay only
	link_container_overlay "$CONTAINER_CLIENT" veth-cli-o-h veth-cli-o-c "$CLIENT_IP"
}

setup_dsr_backend_sysctls() {
	local container=$1
	nsenter_container "$container" sysctl -w net.ipv4.conf.eth0.arp_ignore=1 >/dev/null
	nsenter_container "$container" sysctl -w net.ipv4.conf.eth0.arp_announce=2 >/dev/null
	configure_iface_rp_filter_in_netns "$container" eth0
	configure_iface_rp_filter_in_netns "$container" eth1
	nsenter_container "$container" ip addr add "${DSR_VIP}/32" dev lo
}

configure_iface_rp_filter_in_netns() {
	local container=$1 iface=$2
	nsenter_container "$container" sysctl -w "net.ipv4.conf.${iface}.rp_filter=0" >/dev/null
}

# Ingress-published DSR: replies to the SNAT'd gwbr0 host IP hairpin via lb-sbox.
setup_dsr_backend_routes() {
	local container=$1
	nsenter_container "$container" ip route replace "${GWBR_HOST_IP}/32" via "${LB_GW_IP}" dev eth1
}

setup_topology() {
	require_root
	require_cmds docker ip nsenter sysctl
	create_host_gwbridge
	create_overlay_ns
	start_all_containers
	wire_topology
	for b in "${DSR_BACKENDS[@]}"; do
		setup_dsr_backend_sysctls "$b"
		setup_dsr_backend_routes "$b"
	done
	# LB sandbox must forward between gwbridge and overlay
	nsenter_container "$CONTAINER_LB" sysctl -w net.ipv4.ip_forward=1 >/dev/null
	configure_iface_rp_filter_in_netns "$CONTAINER_LB" eth0
	configure_iface_rp_filter_in_netns "$CONTAINER_LB" eth1
	log "topology ready"
}

load_rulesets() {
	log "loading static nft rulesets"
	nft delete table ip moby-ingress 2>/dev/null || true
	nft -f "${PROTOTYPE_DIR}/lib/nft/host.nft"
	local lb_ns
	lb_ns=$(container_netns "$CONTAINER_LB")
	nft_run "$lb_ns" delete table ip moby-lb-nat 2>/dev/null || true
	nft_run "$lb_ns" delete table netdev moby-lb-dsr 2>/dev/null || true
	nft_load "$lb_ns" "${PROTOTYPE_DIR}/lib/nft/lb-sbox.nft"

	local i bns
	for i in "${!NAT_BACKENDS[@]}"; do
		local idx=$((i + 1))
		bns=$(container_netns "${NAT_BACKENDS[$i]}")
		nft_run "$bns" delete table ip moby-task-ingress 2>/dev/null || true
		nft_load "$bns" "${PROTOTYPE_DIR}/lib/nft/backend.nft" -D ingress_eip="${NAT_BACKEND_IPS[$i]}"
	done
	for i in "${!DSR_BACKENDS[@]}"; do
		local idx=$((i + 1))
		bns=$(container_netns "${DSR_BACKENDS[$i]}")
		nft_run "$bns" delete table ip moby-task-ingress 2>/dev/null || true
		nft_load "$bns" "${PROTOTYPE_DIR}/lib/nft/backend.nft" -D ingress_eip="${DSR_BACKEND_IPS[$i]}"
	done
}

populate_services() {
	log "registering services and backends"
	"${PROTOTYPE_DIR}/nft-ctl.sh" add-service svc-nat NAT "$NAT_VIP" "$NAT_PUBLISH_PORT" TCP "$NAT_TARGET_PORT"
	"${PROTOTYPE_DIR}/nft-ctl.sh" add-service svc-nat-udp NAT "$NAT_VIP" "$NAT_UDP_PUBLISH_PORT" UDP "$NAT_UDP_TARGET_PORT"
	"${PROTOTYPE_DIR}/nft-ctl.sh" add-service svc-dsr DSR "$DSR_VIP" "$DSR_PUBLISH_PORT" TCP "$DSR_TARGET_PORT"
	"${PROTOTYPE_DIR}/nft-ctl.sh" add-service svc-dsr-udp DSR "$DSR_VIP" "$DSR_UDP_PUBLISH_PORT" UDP "$DSR_UDP_TARGET_PORT"

	local i
	for i in "${!NAT_BACKENDS[@]}"; do
		"${PROTOTYPE_DIR}/nft-ctl.sh" add-backend svc-nat "${NAT_BACKENDS[$i]}" "${NAT_BACKEND_IPS[$i]}"
		"${PROTOTYPE_DIR}/nft-ctl.sh" add-backend svc-nat-udp "${NAT_BACKENDS[$i]}" "${NAT_BACKEND_IPS[$i]}"
	done

	for i in "${!DSR_BACKENDS[@]}"; do
		"${PROTOTYPE_DIR}/nft-ctl.sh" add-backend svc-dsr "${DSR_BACKENDS[$i]}" "${DSR_BACKEND_IPS[$i]}"
		"${PROTOTYPE_DIR}/nft-ctl.sh" add-backend svc-dsr-udp "${DSR_BACKENDS[$i]}" "${DSR_BACKEND_IPS[$i]}"
	done
}

warn_ingress_scope() {
	if ! in_isolated_container_netns; then
		return 0
	fi
	local ip
	ip=$(host_ingress_ip)
	warn "running inside a container network namespace (/.dockerenv)."
	warn "Host ingress is programmed here; peers must target ${ip}, not your laptop LAN IP."
	warn "From another container on the same bridge:"
	warn "  docker run --rm --network bridge ${CLIENT_IMAGE} curl -s ${ip}:${NAT_PUBLISH_PORT}"
	warn "Or run: ${PROTOTYPE_DIR}/lib/bridge-peer-test.sh"
}

print_summary() {
	local ingress_ip
	ingress_ip=$(host_ingress_ip)
	cat <<EOF

nftables LB prototype is ready.

Host ingress (this network namespace):
  curl -s 127.0.0.1:${NAT_PUBLISH_PORT}    # NAT TCP (VIP ${NAT_VIP})
  curl -s 127.0.0.1:${DSR_PUBLISH_PORT}    # DSR TCP published port
  echo probe | nc -u -w1 127.0.0.1 ${NAT_UDP_PUBLISH_PORT}    # NAT UDP
  echo probe | nc -u -w1 127.0.0.1 ${DSR_UDP_PUBLISH_PORT}    # DSR UDP published port
  curl -s ${ingress_ip}:${NAT_PUBLISH_PORT}    # NAT TCP (same-bridge / remote peer)
  curl -s ${ingress_ip}:${DSR_PUBLISH_PORT}    # DSR TCP published port
  echo probe | nc -u -w1 ${ingress_ip} ${NAT_UDP_PUBLISH_PORT}    # NAT UDP
  echo probe | nc -u -w1 ${ingress_ip} ${DSR_UDP_PUBLISH_PORT}    # DSR UDP published port

Same-bridge peer test (Moby devcontainer):
  docker run --rm --network bridge ${CLIENT_IMAGE} curl -s ${ingress_ip}:${NAT_PUBLISH_PORT}
  ${PROTOTYPE_DIR}/lib/bridge-peer-test.sh

Overlay VIP (from client container):
  docker exec ${CONTAINER_CLIENT} curl -s ${NAT_VIP}:${NAT_TARGET_PORT}
  docker exec ${CONTAINER_CLIENT} curl -s ${DSR_VIP}:${DSR_TARGET_PORT}
  echo probe | docker exec -i ${CONTAINER_CLIENT} nc -u -w1 ${NAT_VIP} ${NAT_UDP_TARGET_PORT}
  echo probe | docker exec -i ${CONTAINER_CLIENT} nc -u -w1 ${DSR_VIP} ${DSR_UDP_TARGET_PORT}

Soft drain (example):
  ${PROTOTYPE_DIR}/nft-ctl.sh remove-backend svc-nat ${NAT_BACKENDS[1]}

Run demos:
  ${PROTOTYPE_DIR}/demo.sh

Teardown:
  ${PROTOTYPE_DIR}/teardown.sh

EOF
}

main() {
	require_root
	require_cmds docker ip nsenter nft jq sysctl curl
	"${PROTOTYPE_DIR}/teardown.sh" 2>/dev/null || true
	setup_topology
	load_rulesets
	ensure_registry
	populate_services
	warn_ingress_scope
	print_summary
}

main "$@"
