#!/usr/bin/env bash
# nftables control plane: mutate sets/maps only (O(1) ruleset).

set -euo pipefail

source "${BASH_SOURCE%/*}/lib/common.sh"
source "${BASH_SOURCE%/*}/lib/resolve-macs.sh"

require_cmds nft jq docker nsenter

lb_netns() {
	container_netns "$CONTAINER_LB"
}

registry_read() {
	ensure_registry
	cat "$(registry_file)"
}

registry_write() {
	cat > "$(registry_file).tmp"
	mv "$(registry_file).tmp" "$(registry_file)"
}

# Partition 0..1023 into N equal intervals; print "lo-hi : backend" lines.
# Backends are passed as positional args (safe across subshells).
bucket_intervals() {
	local -a backends=("$@")
	local n=${#backends[@]}
	[[ $n -gt 0 ]] || return 0
	local per=$((LB_BUCKETS / n))
	local rem=$((LB_BUCKETS % n))
	local start=0
	local i
	for ((i = 0; i < n; i++)); do
		local size=$per
		[[ $i -lt $rem ]] && size=$((size + 1))
		local end=$((start + size - 1))
		echo "${start}-${end} : ${backends[$i]}"
		start=$((end + 1))
	done
}

rebalance_service() {
	local svc_id=$1
	local json
	json=$(registry_read)
	local mode vip port proto
	mode=$(echo "$json" | jq --arg id "$svc_id" -r '.services[$id].mode')
	vip=$(echo "$json" | jq --arg id "$svc_id" -r '.services[$id].vip')
	port=$(echo "$json" | jq --arg id "$svc_id" -r '.services[$id].publish_port')
	proto=$(echo "$json" | jq --arg id "$svc_id" -r '.services[$id].proto')
	local netns
	netns=$(lb_netns)

	local v
	local old_state=()
	while IFS= read -r v; do
		old_state+=("${v}")
	done < <(echo "$json" | jq --arg id "$svc_id" -r '.bucket_state[$id][]?')

	local new_state=()
	while IFS= read -r v; do
		new_state+=("${v}")
	done < <(bucket_intervals $(echo "$json" | jq --arg id "$svc_id" -r '.backends[$id][]?'))

	{
		if [[ ${#old_state[@]} -gt 0 ]]; then
			echo "delete element ip moby-lb-nat nat-publish-port {"
			for interval in "${old_state[@]}"; do
				echo " ${proto} . ${port} . ${interval},"
			done
			echo "}"

			case $mode in
			NAT*)
				echo "delete element ip moby-lb-nat nat-service-vip {"
				for interval in "${old_state[@]}"; do
					echo " ${vip} . ${interval},"
				done
				echo "}"
				;;
			DSR*)
				echo "delete element netdev moby-lb-dsr real-server {"
				for interval in "${old_state[@]}"; do
					local range=$(echo "$interval" | cut -d' ' -f1)
					local mac=$(resolve_mac $(echo "$interval" | cut -d' ' -f3))
					echo " ${vip} . ${range} : ${mac},"
				done
				echo "}"
				;;
			esac
		fi
		if [[ ${#new_state[@]} -gt 0 ]]; then
			# Add entries for NAT'ing from published ports irrespective of the service mode.
			# DSR services still need to NAT requests from published ports.
			echo "add element ip moby-lb-nat nat-publish-port {"
			for interval in "${new_state[@]}"; do
				echo " ${proto} . ${port} . ${interval},"
			done
			echo "}"

			case $mode in
			NAT*)
				echo "add element ip moby-lb-nat nat-service-vip {"
				for interval in "${new_state[@]}"; do
					echo " ${vip} . ${interval},"
				done
				echo "}"
				;;
			DSR*)
				echo "add element netdev moby-lb-dsr real-server {"
				for interval in "${new_state[@]}"; do
					local range=$(echo "$interval" | cut -d' ' -f1)
					local mac=$(resolve_mac $(echo "$interval" | cut -d' ' -f3))
					echo " ${vip} . ${range} : ${mac},"
				done
				echo "}"
				;;
			esac
		fi
	} | nft_run "$netns" -f -

	echo "$json" | jq '.bucket_state[$id] = $ARGS.positional' \
		--arg id "$svc_id" --args "${new_state[@]}" | registry_write
}

cmd_add_service() {
	local id=$1 mode=${2@U} vip=$3 publish_port=$4 proto=${5@L} target_port=$6

	registry_read | jq --arg id "$id" --arg mode "$mode" --arg vip "$vip" \
		--arg publish_port "$publish_port" --arg proto "$proto" --arg target_port "$target_port" \
		'.services[$id] = {$mode, $proto, $vip, publish_port: ($publish_port|tonumber), target_port: ($target_port|tonumber)} |
		 .backends[$id] //= {} | .bucket_state[$id] //= []' | registry_write

	# L2 target for overlay/client ARP; backends still bind VIP on lo for local delivery.
	nsenter_container "$CONTAINER_LB" ip addr add "${vip}/32" dev eth0 2>/dev/null || true
	nft_host add element ip moby-ingress published-port "{ ${proto} . ${publish_port} }"
	case $mode in
	NAT*)
		;;
	DSR*)
		nft_run "$(lb_netns)" add element netdev moby-lb-dsr virtual-service "{ ${vip} . ${proto} . ${target_port} }"
		;;
	*)
		die "unknown service mode: $mode"
		;;
	esac

	log "add-service ${id} ${mode} vip=${vip} ${publish_port}/${proto} -> ${target_port}"
}

cmd_add_backend() {
	local svc_id=$1 container=$2 backend=$3

	local json mode proto pub target
	json=$(registry_read)
	mode=$(echo "$json" | jq --arg id "$svc_id" -r '.services[$id].mode')
	[[ "$mode" != "null" ]] || die "unknown service: $svc_id"
	proto=$(echo "$json" | jq --arg id "$svc_id" -r '.services[$id].proto')
	pub=$(echo "$json" | jq --arg id "$svc_id" -r '.services[$id].publish_port')
	target=$(echo "$json" | jq --arg id "$svc_id" -r '.services[$id].target_port')

	echo "$json" | jq --arg id "$svc_id" --arg container "$container" --arg backend "$backend" \
		'.backends[$id] += {($container): $backend}' | registry_write

	local netns
	netns=$(container_netns "$container")
	nft_run "$netns" add element ip moby-task-ingress allowed-ports "{ ${proto} . ${target} }"
	if [[ "$pub" != "$target" ]]; then
		nft_run "$netns" add element ip moby-task-ingress remap-ports \
			"{ ${proto} . ${pub} : ${target} }"
	fi

	rebalance_service "$svc_id"
	log "add-backend ${svc_id} ${container} ${mode} ${pub}/${proto} -> ${target}"
}

cmd_remove_backend() {
	local svc_id=$1 container=$2
	registry_read | jq --arg id "$svc_id" --arg container "$container" \
		'.backends[$id] |= del(.[$container])' | registry_write
	rebalance_service "$svc_id"
	log "remove-backend ${svc_id} ${container}"
}

usage() {
	cat <<EOF
usage: nft-ctl.sh <command> [args]

commands:
  add-service <svc-id> (NAT|DSR) <vip> <publish-port> (TCP|UDP) [<target-port>]
  add-backend <svc_id> <container> <ip>
  remove-backend <svc_id> <container>
EOF
}

main() {
	require_root
	[[ $# -ge 1 ]] || { usage; exit 1; }
	case "$1" in
	add-service)
		[[ $# -ge 6 ]] || die "add-service <svc-id> (NAT|DSR) <vip> <publish-port> (TCP|UDP) [<target-port>]"
		cmd_add_service "$2" "$3" "$4" "$5" "$6" "${7:-$5}"
		;;
	add-backend)
		[[ $# -ge 4 ]] || die "add-backend <svc-id> <container> <ip>"
		cmd_add_backend "$2" "$3" "$4"
		;;
	remove-backend)
		[[ $# -ge 3 ]] || die "remove-backend <svc-id> <container>"
		cmd_remove_backend "$2" "$3"
		;;
	*)
		usage
		exit 1
		;;
	esac
}

main "$@"
