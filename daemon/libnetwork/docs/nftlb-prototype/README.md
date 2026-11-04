# nftables Load Balancing Prototype

Self-contained shell prototype that simulates Swarm overlay + ingress load balancing on a single host using **nftables only** (no IPVS, no iptables).

## Topology

```
Host (gwbr0 172.18.0.0/16)
  └── nft moby-ingress: DNAT published ports → lb-sbox gw (172.18.0.2)

overlay netns (br-overlay 10.255.0.0/24)
  ├── nftlb-lb-sbox   eth0=10.255.0.2  eth1=172.18.0.2
  ├── nftlb-nat-backend-{1,2,3}  10.255.0.11-13
  ├── nftlb-dsr-backend-{1,2}    10.255.0.21-22
  └── nftlb-client               10.255.0.100
```

Four services run concurrently (TCP and UDP):

| Service | Mode | Proto | VIP | Published | Target | Backends |
|---------|------|-------|-----|-----------|--------|----------|
| A | NAT | TCP | 10.255.0.10 | 8080 | 80 | nat-backend-1..3 |
| A′ | NAT | UDP | 10.255.0.10 | 8080 | 8123 | nat-backend-1..3 |
| B | DSR | TCP | 10.255.0.20 | 9000 | 8080 | dsr-backend-1..2 |
| B′ | DSR | UDP | 10.255.0.20 | 9000 | 8124 | dsr-backend-1..2 |

The demo deliberately publishes **the same port number** for TCP and UDP (8080 and 9000) to different services.

## O(1) ruleset design

Tables, chains, and rule counts are **fixed**. Services, ports, and backends are added by inserting **set/map elements** only (`nft-ctl.sh`), never by generating rules.

Backend selection is random with equal weights. **NAT** flows are persisted using conntrack. **DSR** handles (one-sided) connection tracking using a map updated in the packet path. Soft drain removes a backend from the corresponding map used for backend selection of new flows — established sessions continue to be tracked in conntrack (NAT) or the nftables dynamic map (DSR).

**DSR** uses lb-sbox `netdev` hooks to rewrite **MAC addresses only** (IP headers untouched):

| Path | lb-sbox hook | Host IP SNAT | Backend sees | Return path |
|------|--------------|--------------|--------------|-------------|
| Overlay VIP (`10.255.0.20:8080`) | `eth0` ingress | none | real client IP | direct to client (asymmetric) |
| Published port (host/peers → `:9000`) | `eth1` ip DNAT | yes, onto `gwbr0` | gwbridge IP | lb-sbox ip prerouting; hairpin reply SNAT |

When `publishPort != targetPort`, port translation is performed inside the backend container's network namespace. Only flows from published ports are translated. Flows originating from peers on the overlay network are not translated.

## Prerequisites

- Linux with nftables (kernel ≥ 5.x, `netdev` ingress support)
- root / `CAP_NET_ADMIN`
- `docker`, `ip`, `nsenter`, `nft`, `jq`, `curl`
- `nicolaka/netshoot:latest` (pulled on first `setup.sh`; backends use `socat` for TCP+UDP echo; if pull fails due to a credential helper, use `DOCKER_CONFIG=/tmp/docker-nocreds docker pull <image>` with an empty `config.json`)

## Quick start

```bash
cd daemon/libnetwork/docs/nftlb-prototype
sudo ./setup.sh
sudo ./demo.sh
sudo ./teardown.sh
```

### Manual checks

```bash
# Host ingress (same network namespace as setup.sh)
curl -s 127.0.0.1:8080   # NAT
curl -s 127.0.0.1:9000   # DSR (publish 9000 → target 8080)

# From another machine on the same L2/L3 network (replace with setup host IP)
curl -s <host-ip>:8080
curl -s <host-ip>:9000

# Overlay VIP from client container
docker exec nftlb-client curl -s 10.255.0.10:80
docker exec nftlb-client curl -s 10.255.0.20:8080

# Soft drain (no new flows to backend)
sudo ./nft-ctl.sh remove-backend svc-nat nftlb-nat-backend-2
```

### Same-bridge peer access (Moby devcontainer)

Host ingress is programmed in **the network namespace where you run `setup.sh`**. In a Moby devcontainer that is usually `eth0` (for example `172.17.0.2`), not your laptop's LAN address.

From another container on the same bridge, target the devcontainer IP printed by `./setup.sh`:

```bash
# Quick check (uses inner docker bridge; routed to eth0 in the devcontainer)
docker run --rm --network bridge nicolaka/netshoot curl -s 172.17.0.2:8080
docker run --rm --network bridge nicolaka/netshoot curl -s 172.17.0.2:9000

# Automated peer checks (inner docker bridge + ipvlan same-L2 peer on eth0)
sudo ./lib/bridge-peer-test.sh
```

Traffic path for a remote peer:

- **NAT overlay VIP** (`10.255.0.10:80`): lb-sbox IP DNAT/SNAT to backend. No port translation.
- **DSR overlay VIP** (`10.255.0.20:8080`): no host involvement; lb-sbox MAC rewrite on `eth0`; backends see real client IP and return directly.
- **Published port** (`8080`, `9000`): `eth0` PREROUTING DNAT → forward `eth0`→`gwbr0` (SNAT) → lb-sbox IP DNAT/SNAT to backends → backends REDIRECT to target port. Published-port flows are NAT'ed irrespective of the service mode.

If a peer on the **host** docker bridge (outside the devcontainer) still cannot connect, check that it targets the devcontainer's `eth0` IP and that the outer docker daemon is not filtering inter-container traffic.

### External / LAN access

For a physical machine on your Wi‑Fi/Ethernet, run `setup.sh` in the host root network namespace (`--network host` devcontainer, or bare metal) and use the machine's LAN IP. Inside a normal container without host networking, peers on your LAN cannot reach ingress rules in the devcontainer namespace.

`setup.sh` sets `ip_forward`, `route_localnet` on `gwbr0`, and `rp_filter=0` on `gwbr0` and gwbridge veth ports (matching production's per-interface `route_localnet` on the gateway bridge). Uplink `rp_filter` is left to the host. Bridge netfilter sysctls are left to the bridge driver (see `setup_bridgenetfiltering.go`).

## Files

| File | Role |
|------|------|
| `setup.sh` | Topology + load static rulesets + populate via nft-ctl |
| `teardown.sh` | Cleanup |
| `demo.sh` | Demo scenarios |
| `nft-ctl.sh` | Control plane (add-service, add-backend, drain-backend) |
| `lib/resolve-macs.sh` | DSR MAC resolution from lb-sbox |
| `lib/bridge-peer-test.sh` | Verify NAT/DSR ingress from bridge / ipvlan peers |
| `lib/nft/*.nft` | Static O(1) rulesets per netns role |

Demo 9 opens an HTTP keep-alive connection from `nftlb-client` to the NAT VIP, pins it to `nftlb-nat-backend-2`, soft-drains that backend, verifies two further requests on the **same TCP connection** still reach the drained backend, then verifies **new** `docker exec ... curl` flows avoid it. Re-run `./setup.sh` before `./demo.sh` (demo 9 drains a backend in-place).
