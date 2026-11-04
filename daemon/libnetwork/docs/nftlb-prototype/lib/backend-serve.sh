#!/usr/bin/env bash
# TCP (HTTP keep-alive) + UDP echo for prototype backends.
set -euo pipefail

: "${TEXT:?}"
: "${TCP_PORT:?}"
: "${UDP_PORT:?}"

export TEXT
socat TCP-LISTEN:"${TCP_PORT}",reuseaddr,fork EXEC:"/bin/bash /backend-serve-tcp.sh" &
# UDP server that sends replies from the IP:PORT which the client addressed the request to.
socat -u UDP-RECVFROM:"${UDP_PORT}",ip-pktinfo,reuseaddr,reuseport,fork SYSTEM:'"socat -u TEXT:\"${TEXT}\n\" UDP-DATAGRAM:${SOCAT_PEERADDR}:${SOCAT_PEERPORT},reuseaddr,reuseport,bind=${SOCAT_IP_DSTADDR}:${UDP_PORT}"' &
wait
