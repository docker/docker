Swarm networking surface area
=============================

Scope: the Docker overlay network driver and the Swarm service mesh (VIP load balancing,
ingress routing mesh, service discovery), as observable from outside the daemon.

This document is a test-surface specification. For how the parts work, see
[design.md](../design.md), [network.md](../network.md) and [networkdb.md](../networkdb.md).

This spec uses the conventions in [README.md](README.md) for tiers, vantage points and dependencies.
The other specs in this directory use the same conventions. Unless a row says otherwise, a `ctr` in
this document is a standalone container attached to an `attachable` overlay.

In this spec, an overlay network is an overlay network other than the ingress network, unless a row
includes the ingress network. A node has a local attachment to a network while a task, a standalone
container or the node's load-balancer endpoint on that network is on the node. A network is
programmed on a node while the node has a local attachment to it. A node creates its load-balancer
endpoint on a network with the network, and deletes it with the network. Except on the ingress
network, the node deletes the network once no task or standalone container of the node is on it, so
the load-balancer endpoint does not outlast them.

A row that involves the ingress network holds for an ingress network that is not `internal` and does
not have the option `dsr`. A row that involves a node's gateway network holds for one that the
daemon created, or that the operator created with no options other than a subnet and a gateway
([§8.1](#81-gateway-network)). On an overlay network with more than one subnet, a row about traffic
to a VIP holds for the VIP and a backend task only when both are in the subnet of the load-balancer
endpoint of the client's node on that network ([§13](#13-known-divergences--non-goals)). On Linux
nodes, a row about a `mode=host` published port holds for a service that is attached to at least one
network that is not `internal`.

## Host requirements

The conventions for this section are in [README.md](README.md#host-requirements). The kernel
facilities and programs are requirements of each Linux node. The Windows facilities are
requirements of each Windows node.

### Kernel facilities

| Facility | Needed when | Kernel options | Needed for | Pinned by |
| --- | --- | --- | --- | --- |
| XFRM, with ESP in transport mode and the AEAD algorithm `rfc4106(gcm(aes))` with a 64-bit ICV | An `encrypted` overlay network | `XFRM`, `XFRM_USER`, `XFRM_ALGO`, `INET_ESP`, `CRYPTO_AES`, `CRYPTO_GCM`, `CRYPTO_SEQIV`. `INET_XFRM_MODE_TRANSPORT` on kernels that have it. | `encrypted` overlay networks ([§9](#9-encryption)) | The [§9](#9-encryption) row about the algorithm of the IPsec security associations |
| VXLAN devices | Always | `VXLAN` | Overlay networks ([§3](#3-observable-state-inside-a-container), [§4](#4-east-west-reachability), [§10](#10-wire-format--interop)) | The [§10.1](#101-all-overlay-networks) row about a node without VXLAN devices |
| IPv6 | A node with an IPv6 data-path address | `IPV6` | Overlay networks over an IPv6 underlay ([§10](#10-wire-format--interop)) | Not pinned |
| ESP over IPv6 | An `encrypted` overlay network, on a node with an IPv6 data-path address | `INET6_ESP` | `encrypted` overlay networks over an IPv6 underlay ([§9](#9-encryption)) | The [§9](#9-encryption) row about the algorithm of the IPsec security associations |
| Network namespaces, veth pairs and Linux bridges | Always | `NET_NS`, `VETH`, `BRIDGE` | Overlay networks and the gateway network ([§4](#4-east-west-reachability), [§8.1](#81-gateway-network)) | The [bridge-networking.md](bridge-networking.md#host-requirements) requirements |
| Netfilter hooks for bridged traffic (`br_netfilter`) | The daemon option `iptables` true | `BRIDGE_NETFILTER` | The gateway network, which does not allow inter-container communication ([§8.1](#81-gateway-network)) | The [bridge-networking.md](bridge-networking.md#31-isolation) row about `bridge-nf-call-iptables` |
| IPVS with firewall-mark virtual services, the round-robin scheduler and connection tracking | Always | `IP_VS`, `IP_VS_RR`, `IP_VS_NFCT`, `IP_VS_PROTO_TCP`, `IP_VS_PROTO_UDP`, `IP_VS_PROTO_SCTP` | VIP load balancing and the routing mesh ([§6](#6-load-balancing-east-west-vip), [§7.2](#72-modeingress)) | The [§6](#6-load-balancing-east-west-vip) row about a node without IPVS |
| SCTP | Always | `IP_SCTP`. `NF_CT_PROTO_SCTP` on kernels that have it. | SCTP through overlay networks, VIPs and published ports ([§4](#4-east-west-reachability), [§6](#6-load-balancing-east-west-vip), [§7](#7-published-ports)) | The [bridge-networking.md](bridge-networking.md#32-port-publishing) row about a host without SCTP |

### Programs

| Program | Needed when | Needed for | Pinned by |
| --- | --- | --- | --- |
| `iptables`, accepting the filter, mangle and nat tables, the `MARK`, `DNAT`, `SNAT` and `REDIRECT` targets, and the `ipvs`, `conntrack`, `sctp`, `bpf`, `policy`, `owner` and `mark` matches | Always | VIP load balancing and the routing mesh ([§6](#6-load-balancing-east-west-vip), [§7.2](#72-modeingress)), `encrypted` overlay networks ([§9](#9-encryption)) | The [bridge-networking.md](bridge-networking.md#14-firewall-backend-and-firewalld) row about a host without the `iptables` command |
| `ip6tables`, accepting the filter and mangle tables, the `MARK` target, and the `bpf`, `policy`, `owner` and `mark` matches | The iptables backend, and an `encrypted` overlay network on a node with an IPv6 data-path address | `encrypted` overlay networks over an IPv6 underlay ([§9](#9-encryption)) | Not pinned |

The daemon does not depend on which implementation of `iptables` and `ip6tables` the host has.
The kernel options that the programs need depend on that implementation:

| Program | Implementation | Kernel options |
| --- | --- | --- |
| `iptables` | Either | `NETFILTER`, `NETFILTER_ADVANCED`, `NETFILTER_XTABLES`, `NF_CONNTRACK`, `NF_NAT`, `NETFILTER_XT_MARK`, `NETFILTER_XT_NAT`, `NETFILTER_XT_TARGET_REDIRECT`, `NETFILTER_XT_MATCH_IPVS`, `NETFILTER_XT_MATCH_CONNTRACK`, `NETFILTER_XT_MATCH_SCTP`, `NETFILTER_XT_MATCH_BPF`, `NETFILTER_XT_MATCH_POLICY`, `NETFILTER_XT_MATCH_OWNER` |
| `iptables` | iptables-legacy | `IP_NF_IPTABLES`, `IP_NF_FILTER`, `IP_NF_MANGLE`, `IP_NF_NAT`. `IP_NF_IPTABLES_LEGACY` from Linux 6.9, and `NETFILTER_XTABLES_LEGACY` from Linux 6.17. |
| `iptables` | iptables-nft | `NF_TABLES`, `NF_TABLES_IPV4`, `NFT_COMPAT`, `NFT_NAT`. Before Linux 5.1, `NFT_CHAIN_NAT_IPV4` in place of `NFT_NAT`. Before Linux 5.2, `NFT_CHAIN_ROUTE_IPV4` for the `OUTPUT` chain of the mangle table. |
| `ip6tables` | Either | The options in the first row |
| `ip6tables` | iptables-legacy | `IP6_NF_IPTABLES`, `IP6_NF_FILTER`, `IP6_NF_MANGLE`. `IP6_NF_IPTABLES_LEGACY` from Linux 6.9, and `NETFILTER_XTABLES_LEGACY` from Linux 6.17. |
| `ip6tables` | iptables-nft | `NF_TABLES`, `NF_TABLES_IPV6`, `NFT_COMPAT`. Before Linux 5.2, `NFT_CHAIN_ROUTE_IPV6` for the `OUTPUT` chain of the mangle table. |

### Windows facilities

| Facility | Needed when | Needed for | Pinned by |
| --- | --- | --- | --- |
| Host Networking Service overlay networks with VSID policies | Always | Overlay networks on Windows nodes | Not pinned |

### Host configuration

These are requirements of each node, and of the physical network between nodes.

| Configuration | Needed when | Needed for | Pinned by |
| --- | --- | --- | --- |
| Each node's firewall and the physical network between nodes pass TCP to port 2377 of each manager, TCP and UDP on port 7946, and UDP on the data-path port, 4789 unless `POST /swarm/init` set another | A node in a Swarm | Every section | Not pinned |
| Each node's firewall and the physical network between nodes pass IP protocol 50 (ESP) | An `encrypted` overlay network | [§9](#9-encryption), [§10.3](#103-encrypted-overlay-networks) | Not pinned |

## Interop premise

Wire and protocol compatibility is a hard requirement. Users upgrade a Swarm node by node. During a
rollout, the cluster is heterogeneous. Nodes with different engine versions must share an overlay
network and an ingress network. Everything in [§10 Wire format](#10-wire-format--interop) is
contract.

## 1. Cluster & node configuration

These rows cover cluster configuration. The accepted range of the data-path port is 1024 to
49151. The reference for `docker swarm init --data-path-port` documents this range.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | `DataPathAddr` in `POST /swarm/init` or `POST /swarm/join` sets the address peers send the node's VXLAN traffic to. | B | `underlay` (outer destination address) | `os` |
| | Without `DataPathAddr`, peers send the node's VXLAN traffic to its advertise address. | B | `underlay` (outer destination address) | `os` |
| | `DataPathAddr` in `POST /swarm/init` or `POST /swarm/join` names a network interface. Peers then send the node's VXLAN traffic to the address that the interface had when the request was made. They continue to send it to that address after the interface's address changes, and after the daemon restarts. See [§13](#13-known-divergences--non-goals). | C | `underlay` (outer destination address) | `none` |
| Linux nodes | A node's VXLAN traffic to a peer has the outer source address that the node's route to that peer selects. This holds whether or not that address is the node's data-path address. See [§13](#13-known-divergences--non-goals). | C | `underlay` (outer source address) | `os` |
| | `DataPathPort` in `POST /swarm/init` sets the VXLAN UDP port for the whole cluster. See [§13](#13-known-divergences--non-goals). | B | `underlay` | `os` |
| A Swarm with the default data-path port | Linux and Windows nodes in that Swarm exchange overlay traffic. The default data-path port is the UDP port that Windows nodes always use for VXLAN. | B | `task` | `os` (Windows) |
| A node that is not yet in a Swarm | `docker swarm init` with a data-path port outside the accepted range exits non-zero. The node is then still not in a Swarm. | B | `api:solo` | `cli` |
| A node that is not yet in a Swarm | `POST /swarm/init` with a data-path port outside the accepted range responds with a 4xx or 5xx status. The node is then still not in a Swarm. | B | `api:solo` | `none` |
| A node that is not yet in a Swarm | `POST /swarm/init` with a data-path port outside the accepted range responds 500. See [§13](#13-known-divergences--non-goals). | C | `api:solo` | `none` |
| A node that is not yet in a Swarm | `POST /swarm/init` with a data-path port of 0 responds 200. `GET /info` on the node then reports 4789 as the data-path port in `Swarm.Cluster`. | B | `api:solo`, `api:mgr` | `none` |
| An overlay network, the ingress network included, that uses the default IPAM driver and is created without a subnet | Once the network is allocated ([§2](#2-network-lifecycle--api-surface)), `GET /networks/{id}` on a manager reports a subnet for it. That subnet is inside one of the `DefaultAddrPool` prefixes that `GET /info` on a manager reports in `Swarm.Cluster`. The prefix length of that subnet is the `SubnetSize` that `GET /info` reports. | B | `api:mgr` | `none` |
| A node that is not yet in a Swarm | `docker swarm init` with `--default-addr-pool` exits non-zero when `--default-addr-pool-mask-length` is greater than 29. It also exits non-zero when that mask length is less than the prefix length of one of the pools. In both cases, the node is then still not in a Swarm. | B | `api:solo` | `cli` |
| A node that is not yet in a Swarm | `POST /swarm/init` with `DefaultAddrPool` set responds with a 4xx or 5xx status when the request's `SubnetSize` is greater than 29. It also responds with a 4xx or 5xx status when the request's `SubnetSize` is not 0 and is less than the prefix length of one of the pools. In both cases, the node is then still not in a Swarm. | B | `api:solo` | `none` |
| A node that is not yet in a Swarm | `POST /swarm/init` with `DefaultAddrPool` set responds 500 when the request's `SubnetSize` is greater than 29. It also responds 500 when the request's `SubnetSize` is not 0 and is less than the prefix length of one of the pools. See [§13](#13-known-divergences--non-goals). | C | `api:solo` | `none` |
| A node that is not yet in a Swarm | `POST /swarm/init` with `DefaultAddrPool` set and a `SubnetSize` of 0 responds 200. `GET /info` on the node then reports a `SubnetSize` of 0 in `Swarm.Cluster`. See [§13](#13-known-divergences--non-goals). | C | `api:solo`, `api:mgr` | `none` |
| | On a manager, `GET /info` includes `Swarm.Cluster`. `Swarm.Cluster` includes the data-path port, `DefaultAddrPool` and `SubnetSize`. When `POST /swarm/init` gave a pool, the pool and size are those that the request gave. When the request gave no pool, they are `10.0.0.0/8` and 24, whatever `SubnetSize` the request gave. | C | `api:mgr` | `none` |
| | On a manager, `docker info` prints the data-path port, the default address pool and the subnet size. When `docker swarm init` gave `--default-addr-pool`, the pool and size are those that the command gave. When the command gave no `--default-addr-pool`, they are `10.0.0.0/8` and 24, even if it gave `--default-addr-pool-mask-length`. | C | `api:mgr` | `cli` |
| | On a worker, `GET /info` does not include `Swarm.Cluster`. | C | `api:wkr` | `none` |
| | On a worker, `docker info` does not print the data-path port or a default address pool. | C | `api:wkr` | `cli` |
| | Gossip uses TCP and UDP port 7946. Neither port is configurable. | C | `node` | `none` |
| | Peers send this node's gossip traffic, on TCP and UDP port 7946, to its advertise address. | B | `underlay` | `none` |
| | A node listens for gossip on TCP and UDP port 7946, on the host part of its listen address. | B | `node` | `none` |
| | A capture of gossip traffic, on TCP and UDP port 7946, does not show the cluster's network IDs, service names or task addresses in cleartext. This holds whether or not any network is `encrypted`. | B | `underlay` | `none` |
| | An IPv6 `DataPathAddr` in `POST /swarm/init` or `POST /swarm/join` produces IPv6 outer packets for overlay traffic. See [§13](#13-known-divergences--non-goals). | B | `underlay` | `os` |
| A Swarm whose nodes have data-path addresses of one IP version | `POST /swarm/join` to that Swarm with a data-path address of the other IP version responds 200. The node is then in the Swarm. See [§13](#13-known-divergences--non-goals). | C | `api:solo` | `none` |
| A Linux node that is not yet in a Swarm, with `live-restore` true | `docker swarm init` and `docker swarm join` exit non-zero. The node is then still not in a Swarm. | B | `api:solo` | `cli` |
| A Linux node that is not yet in a Swarm, with `live-restore` true | `POST /swarm/init` and `POST /swarm/join` respond with a 4xx or 5xx status. The node is then still not in a Swarm. | B | `api:solo` | `none` |
| A Linux node that is not yet in a Swarm, with `live-restore` true | `POST /swarm/init` and `POST /swarm/join` respond 500. See [§13](#13-known-divergences--non-goals). | C | `api:solo` | `none` |
| A Linux node that is already in a Swarm, with `live-restore` true | The daemon does not start. The daemon log contains an error. | B | `log` | `none` |
| A Linux node that is already in a Swarm, with `live-restore` true | The daemon log contains an error that contains `incompatible with swarm mode`. | C | `log` | `none` |
| A Linux node that is not yet in a Swarm, with `firewall-backend` set to `nftables`, with `iptables` true, and without `features.swarm-nftables` | `docker swarm init` and `docker swarm join` exit non-zero. The node is then still not in a Swarm. | C | `api:solo` | `cli` |
| A Linux node that is not yet in a Swarm, with `firewall-backend` set to `nftables`, with `iptables` true, and without `features.swarm-nftables` | `POST /swarm/init` and `POST /swarm/join` respond 500. See [§13](#13-known-divergences--non-goals). | C | `api:solo` | `none` |
| A Linux node that is already in a Swarm, with `firewall-backend` set to `nftables`, with `iptables` true, and without `features.swarm-nftables` | The daemon does not start on that node. The daemon log contains an error that contains `incompatible with swarm mode`. | C | `log` | `none` |

## 2. Network lifecycle & API surface

A Swarm-scoped network is a network that the Swarm managers keep. Its driver is a multi-host
driver, such as `overlay`, or a node-local driver, such as `bridge`. A network of a node-local
driver is Swarm-scoped when it is created with `Scope` set to `swarm` (`--scope swarm`).

A Swarm-scoped network is allocated after the network is created, and asynchronously. In this spec,
a network is allocated once `GET /networks/{id}` on a manager reports the network's `Driver`.

### 2.1 Overlay driver options

A row in this table that refers to another section covers only whether the daemon accepts and honors
the option. The Depends on column of that row also covers only that. The section that the row refers
to specifies the behavior that the option selects, and what that behavior depends on.

| Key | Expectation | Tier | Depends on |
| --- | --- | --- | --- |
| `com.docker.network.driver.mtu` | Sets the base MTU from which encapsulation overhead is subtracted. See [§3](#3-observable-state-inside-a-container). | B | `none` |
| `com.docker.network.driver.overlay.vxlanid_list` | Sets the VNIs, one per IPAM subnet. See [§2.3](#23-overlay-networks). | B | `none` |
| `encrypted` | Selects an encrypted overlay. See [§9](#9-encryption). | B | `none` |
| `dsr` | Selects direct server return for east-west VIP traffic on Linux nodes. See [§6](#6-load-balancing-east-west-vip). | B | `none` |
| `encrypted`, `dsr` | On Linux nodes, each key takes effect when it is present in the network's options, whatever its value. This includes the empty string and `false` as values. | C | `none` |

### 2.2 Swarm-scoped networks

The rows in this subsection hold for any Swarm-scoped network, whatever its driver.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A manager | On that manager, `POST /networks/create` with a name that no Swarm-scoped network has, and no network on that manager has, responds 201 when it sets `Scope` to `swarm` or names a multi-host driver, such as `overlay`. `GET /networks` on a manager then lists the network. | B | `api:mgr` | `none` |
| A manager | On that manager, `docker network create --scope=swarm` with a name that no Swarm-scoped network has, and no network on that manager has, exits zero. `docker network create --driver overlay` with such a name, in place of the first command, also exits zero. `docker network ls` on a manager then lists the network. | B | `api:mgr` | `cli` |
| | Within the allocation ceiling ([§11.2](#112-convergence)) after `POST /networks/create` for a Swarm-scoped network responds 201, `GET /networks/{id}` on a manager reports the network's `Driver`. This does not apply to a network that cannot be allocated ([§11.4](#114-scale-and-exhaustion)). | B | `api:mgr` | `none` |
| | On a worker, `POST /networks/create` with `Scope` set to `swarm` responds 403. It also responds 403 with a multi-host driver, such as `overlay`, whatever `Scope` the request gives. | B | `api:wkr` | `none` |
| | On a worker, `docker network create --scope=swarm` exits non-zero and prints the daemon's error. `docker network create --driver overlay` also exits non-zero and prints the daemon's error. See [§13](#13-known-divergences--non-goals). | B | `api:wkr` | `cli` |
| A Swarm with a Swarm-scoped network other than the ingress network ([§2.4](#24-the-ingress-network)), and a service that references the network | `DELETE /networks/{id}` for the network responds with a 4xx or 5xx status. The network stays listed. | B | `api:mgr` | `none` |
| A Swarm with a Swarm-scoped network other than the ingress network ([§2.4](#24-the-ingress-network)), and a service that references the network | `DELETE /networks/{id}` for the network responds 400, with a message that contains `is in use by service`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with a Swarm-scoped network other than the ingress network ([§2.4](#24-the-ingress-network)), and a task or container attached to it that is not shutting down | `DELETE /networks/{id}` for the network responds with a 4xx or 5xx status. The network stays listed. | B | `api:mgr` | `none` |
| A Swarm with a Swarm-scoped network other than the ingress network ([§2.4](#24-the-ingress-network)), and a task or container attached to it that is not shutting down | `DELETE /networks/{id}` for the network responds 400, with a message that contains `is in use by task`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with a Swarm-scoped network other than the ingress network, and a service that references the network | `docker network rm` for the network exits non-zero and prints the daemon's error. `docker network ls` on a manager then still lists the network. | B | `api:mgr` | `cli` |
| A Swarm with a Swarm-scoped network other than the ingress network, and a task or container attached to it that is not shutting down | `docker network rm` for the network exits non-zero and prints the daemon's error. `docker network ls` on a manager then still lists the network. | B | `api:mgr` | `cli` |
| A Swarm with a Swarm-scoped network | `POST /networks/create` with the name of that network responds 409. `GET /networks` on a manager then lists only one network with that name. | B | `api:mgr` | `none` |
| A Swarm with a Swarm-scoped network | `docker network create` with the name of that network exits non-zero and prints the daemon's error. `docker network ls` on a manager then lists only one network with that name. | B | `api:mgr` | `cli` |
| A manager with a network that is not Swarm-scoped and that has the name of a Swarm-scoped network | On that manager, `GET /networks` lists both networks. | B | `api:mgr` | `none` |
| A manager with a network that is not Swarm-scoped and that has the name of a Swarm-scoped network | On that manager, `GET /networks/{id}` and `DELETE /networks/{id}` with that name respond with a 4xx or 5xx status. `DELETE /networks/{id}` does not delete either network. | B | `api:mgr` | `none` |
| A manager with a network that is not Swarm-scoped and that has the name of a Swarm-scoped network | On that manager, `GET /networks/{id}` and `DELETE /networks/{id}` with that name respond 400, with a message that contains `is ambiguous`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A manager with a network that is not Swarm-scoped and that has the name of a Swarm-scoped network | On that manager, `docker network inspect` and `docker network rm` with that name exit non-zero and print the daemon's error. | B | `api:mgr` | `cli` |
| A manager with a network that is not Swarm-scoped and that has the name of a Swarm-scoped network | On that manager, `GET /networks/{id}` with that name and with `scope` set to `local` returns the network that is not Swarm-scoped. With `scope` set to `swarm`, it returns the Swarm-scoped network. | B | `api:mgr` | `none` |
| A manager with a network that is not Swarm-scoped whose name is a prefix of the ID of a Swarm-scoped network | On that manager, `GET /networks/{id}` with that name as `{id}` returns the Swarm-scoped network. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A node with a network that is not Swarm-scoped and that has the name of a Swarm-scoped network | On that node, the tasks of a service on the Swarm-scoped network are rejected. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A node with a network that is not Swarm-scoped and that has the name of a Swarm-scoped network, with a created or stopped standalone container whose endpoint settings for the Swarm-scoped network carry its ID | On that node, `POST /containers/{id}/start` for the container responds 500, with a message that contains `could not find network`. See [§13](#13-known-divergences--non-goals). | C | `api:active` | `none` |
| A node with a network that is not Swarm-scoped and that has the name of a Swarm-scoped network, with a created standalone container whose endpoint settings name that network by its name | On that node, `POST /containers/{id}/start` for the container responds 204. `GET /containers/{id}/json` then reports the ID of the network that is not Swarm-scoped as the `NetworkID` for that name. See [§13](#13-known-divergences--non-goals). | C | `api:active` | `none` |
| | Once a Swarm-scoped network is allocated ([§2](#2-network-lifecycle--api-surface)), `GET /networks/{id}` for that network reports `Scope` as `swarm`, and reports `Created`. It also reports the `Driver`, `Ingress`, `Attachable`, `Internal`, `EnableIPv6`, `ConfigFrom` and `Labels` given at create. For an overlay network, it also reports the `IPAM` of the allocation. | C | `api:mgr` | `none` |
| An allocated Swarm-scoped network | On a manager, `docker network inspect` for the network prints `swarm` as its `Scope`, and prints its `Driver`. | B | `api:mgr` | `cli` |
| A node with a local attachment to an overlay network | On that node, `docker network inspect --verbose` for that network prints `Services`. | B | `api:mgr`, `api:wkr` | `cli` |
| | On a worker, `GET /networks` lists a Swarm-scoped network other than the ingress network only while the worker has a local attachment to the network. | C | `api:wkr` | `none` |
| A manager | `GET /networks` with the filter `dangling=true` lists every Swarm-scoped network, including the ingress network and networks that services use. `GET /networks` with the filter `dangling=false` lists the Swarm-scoped networks that have a local attachment on that manager. Those networks are then in both responses. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A worker that does not have a local attachment to a Swarm-scoped network other than the ingress network | On that worker, `GET /networks/{id}` for that network responds 404. | C | `api:wkr` | `none` |
| A manager that does not have a local attachment to an overlay network | On that manager, `GET /networks/{id}` for that network responds 200. The response reports the `IPAM` of the network's allocation and an empty `Containers`. It does not include `Peers`. | C | `api:mgr` | `none` |
| | `docker network ls` on a manager lists each Swarm-scoped network. | B | `api:mgr` | `cli` |
| | On any node whose availability is `active`, `POST /containers/{id}/start` for a created or stopped container on an attachable Swarm-scoped network responds 204. The container is then running and has an address on the network. For a network created with `ConfigFrom`, this holds for a network of a node-local driver, on a node that has the config-only network that `ConfigFrom` names. | B | `api:active` | `none` |
| | On any node whose availability is `active`, `docker run -d --network` naming an attachable Swarm-scoped network exits zero. The container then runs and is attached to the network. For a network created with `ConfigFrom`, this holds for a network of a node-local driver, on a node that has the config-only network that `ConfigFrom` names. | B | `api:active` | `cli` |
| | On any node whose availability is `active`, `POST /networks/{id}/connect` to an attachable Swarm-scoped network, for a running standalone container, responds 200. The container then has an address on the network. For a network created with `ConfigFrom`, this holds for a network of a node-local driver, on a node that has the config-only network that `ConfigFrom` names. | B | `api:active` | `none` |
| | On any node whose availability is `active`, `docker network connect` naming an attachable Swarm-scoped network and a running standalone container exits zero. The container then has an address on the network. For a network created with `ConfigFrom`, this holds for a network of a node-local driver, on a node that has the config-only network that `ConfigFrom` names. | B | `api:active` | `cli` |
| A running standalone container on any node whose availability is `active` that is already attached to an attachable Swarm-scoped network | `POST /networks/{id}/connect` to that network, for that container, responds 409. The container's addresses on that network do not change. See [network-common.md §7](network-common.md#7-known-divergences--non-goals). | C | `api:active` | `none` |
| A standalone container on an attachable Swarm-scoped network | The container disconnects from the network, or stops. The container's attachment task then disappears from `GET /tasks` on a manager with the `runtime=attachment` filter. | B | `api:mgr`, `api:active` | `none` |
| A standalone container on an attachable Swarm-scoped network to which nothing else is attached | The container disconnects from the network, or stops. `DELETE /networks/{id}` for the network on a manager then responds 204. | B | `api:mgr`, `api:active` | `none` |
| A created or stopped container on an attachable Swarm-scoped network, on a node whose availability is `drain` or `pause` | `POST /containers/{id}/start` for the container responds with a 4xx or 5xx status. | B | `api:mgr`, `api:wkr` | `none` |
| A created or stopped container on an attachable Swarm-scoped network, on a node whose availability is `drain` or `pause` | `POST /containers/{id}/start` for the container responds 500 after 20 seconds, with a message that contains `attaching to network failed, make sure your network options are correct and check manager logs: context deadline exceeded`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr`, `api:wkr` | `none` |
| A running standalone container on a node whose availability is `drain` or `pause`, and an attachable Swarm-scoped network | `POST /networks/{id}/connect` to that network, for the container, responds with a 4xx or 5xx status. `GET /containers/{id}/json` then does not list the network in `NetworkSettings.Networks`. | B | `api:mgr`, `api:wkr` | `none` |
| A running standalone container on a node whose availability is `drain` or `pause`, and an attachable Swarm-scoped network | `POST /networks/{id}/connect` to that network, for the container, responds 500 after 20 seconds, with a message that contains `attaching to network failed, make sure your network options are correct and check manager logs: context deadline exceeded`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr`, `api:wkr` | `none` |
| A node whose availability is `drain` or `pause`, and an attachable Swarm-scoped network | On that node, `docker run -d --network` naming that network exits non-zero and prints the daemon's error. | B | `api:mgr`, `api:wkr` | `cli` |
| | `POST /containers/create` for a container on a non-attachable Swarm-scoped network responds 201. `GET /containers/{id}/json` then reports the container in the `created` state. | C | `api:mgr`, `api:wkr` | `none` |
| | `POST /containers/{id}/start` for a created or stopped container on a non-attachable Swarm-scoped network, the ingress network included, responds with a 4xx or 5xx status. | B | `api:mgr`, `api:wkr` | `none` |
| | `POST /containers/{id}/start` for a created or stopped container on a non-attachable Swarm-scoped network, the ingress network included, responds 500, with a message that contains `not manually attachable`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr`, `api:wkr` | `none` |
| | `docker run -d --network` naming a non-attachable Swarm-scoped network, the ingress network included, exits non-zero and prints the daemon's error. | B | `api:mgr`, `api:wkr` | `cli` |
| A created container without auto-remove (`--rm`) on a non-attachable Swarm-scoped network | `POST /containers/{id}/start` for the container fails. The container stays in the created state. | C | `api:mgr`, `api:wkr` | `none` |
| A created container with auto-remove (`--rm`) on a non-attachable Swarm-scoped network | `POST /containers/{id}/start` for the container fails. The container is removed. | C | `api:mgr`, `api:wkr` | `none` |
| A running standalone container, and a non-attachable Swarm-scoped network, the ingress network included | `POST /networks/{id}/connect` to that network, for the container, responds with a 4xx or 5xx status. `GET /containers/{id}/json` then does not list the network in `NetworkSettings.Networks`. | B | `api:mgr`, `api:wkr` | `none` |
| A running standalone container, and a non-attachable Swarm-scoped network, the ingress network included | `POST /networks/{id}/connect` to that network, for the container, responds 500, with a message that contains `not manually attachable`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr`, `api:wkr` | `none` |
| A running standalone container, and a non-attachable Swarm-scoped network, the ingress network included | `docker network connect` naming that network and the container exits non-zero and prints the daemon's error. | B | `api:mgr`, `api:wkr` | `cli` |
| A node in a Swarm, and a created or stopped container | `POST /networks/{id}/connect` for the container, with an `{id}` that is not the name or an ID prefix of any network, responds 200. `POST /containers/{id}/start` for the container then responds 500, with a message that contains `network <name> not found`. `<name>` is the `{id}` that the connect gave. See [§13](#13-known-divergences--non-goals). | C | `api:mgr`, `api:wkr` | `none` |
| A Swarm-scoped network of a node-local driver, such as `bridge`, with `ConfigFrom` naming a config-only network that exists on each node that runs a task | `GET /tasks` reports the tasks of a service on the network as `running`. See [§13](#13-known-divergences--non-goals). | B | `api:mgr` | `none` |
| A Swarm-scoped network of a node-local driver, such as `bridge`, with `ConfigFrom` naming a config-only network that exists on each node that runs a task | Each task of a service on the network has an address on the network in the subnet of its node's config-only network. See [§13](#13-known-divergences--non-goals). | B | `task` | `os` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a node that does not have that config-only network | On that node, the tasks of a service on the network are rejected. Their error in `GET /tasks` contains the name of the config-only network. | B | `api:mgr` | `none` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a node that does not have that config-only network | On that node, the error of a rejected task in `GET /tasks` contains `configuration network "<name>" does not exist`. | C | `api:mgr` | `none` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a node whose availability is `active` that does not have that config-only network, with a created or stopped container on the network | On that node, `POST /containers/{id}/start` for the container responds with a 4xx or 5xx status. | B | `api:active` | `none` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a node whose availability is `active` that does not have that config-only network, with a running standalone container that is not on the network | On that node, `POST /networks/{id}/connect` to the network, for the container, responds with a 4xx or 5xx status. `GET /containers/{id}/json` then does not list the network in `NetworkSettings.Networks`. | B | `api:active` | `none` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a node whose availability is `active` that does not have that config-only network, with a created or stopped container on the network | On that node, `POST /containers/{id}/start` for the container responds 500 after 20 seconds, with a message that contains `attaching to network failed, make sure your network options are correct and check manager logs: context deadline exceeded`. `POST /networks/{id}/connect` to the network for a running standalone container on that node responds the same way. See [§13](#13-known-divergences--non-goals). | C | `api:active` | `none` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a node with a local attachment to the network | On that node, `GET /networks/{id}` for the network reports the `IPAM` configuration of that node's config-only network. | B | `api:mgr`, `api:wkr` | `none` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a manager that does not have a local attachment to the network | On that manager, `GET /networks/{id}` for the network reports an empty `IPAM` and reports the network's `ConfigFrom`. | C | `api:mgr` | `none` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a node with a local attachment to the network | On that node, `DELETE /networks/{id}` for the config-only network responds 403. `GET /networks` on that node then still lists the config-only network. | B | `api:mgr`, `api:wkr` | `none` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a node with a local attachment to the network | On that node, `docker network rm` for the config-only network exits non-zero and prints the daemon's error. | B | `api:mgr`, `api:wkr` | `cli` |
| A Swarm-scoped network of a node-local driver, created with `ConfigFrom` naming a config-only network, and a node that has the config-only network and does not have a local attachment to the network | On that node, `DELETE /networks/{id}` for the config-only network responds 204. | C | `api:mgr`, `api:wkr` | `none` |
| | `POST /networks/create` for a Swarm-scoped network of a node-local driver with `EnableIPv4` false, at API version 1.48 or later, responds 400 with `IPv4 cannot be disabled in a Swarm scoped network`. The request also responds 400 with that message when the `com.docker.network.enable_ipv4` option is set to `false`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| | `docker network create --scope=swarm` with a node-local driver and `--ipv4=false` exits non-zero and prints the daemon's error. So does the same command with `--opt com.docker.network.enable_ipv4=false` in place of `--ipv4=false`. | C | `api:mgr` | `cli` |
| | A `POST /networks/create` request for a Swarm-scoped network of a node-local driver without `ConfigFrom` has a subnet and gateway in `IPAM.Config`. Its `Options` has only the driver's own options, such as `com.docker.network.bridge.*`, or no options. The request responds 201. `GET /networks/{id}` on a manager then reports an empty `IPAM` and does not report any `Options`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm-scoped network of a node-local driver that was created without `ConfigFrom`, with a subnet and gateway in `IPAM.Config` | On a node that runs a task on that network, `GET /networks/{id}` reports a subnet from the node's default address pools. It does not report any of the `Options` given at create. See [§13](#13-known-divergences--non-goals). | C | `api:wkr` | `none` |
| A manager with a config-only network that no other node has | On that manager, `POST /networks/create` for a Swarm-scoped network of a node-local driver, with `ConfigFrom` naming the config-only network, responds 201. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A manager that does not have a given config-only network, and another manager that has it | On the first manager, `POST /networks/create` for a Swarm-scoped network of any driver, with `ConfigFrom` naming that config-only network, responds 404. See [§13](#13-known-divergences--non-goals) and [network-common.md §7](network-common.md#7-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A manager with a config-only network | On that manager, `POST /networks/create` for an overlay network, with `ConfigFrom` naming the config-only network, responds 201. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A manager that does not have a given config-only network | On that manager, `docker network create --scope=swarm --config-from` naming that network exits non-zero and prints the daemon's error. So does `docker network create --driver overlay --config-from` naming that network. | C | `api:mgr` | `cli` |
| A manager that does not have a given managed plugin | On that manager, `POST /networks/create` for a Swarm-scoped network whose driver is that plugin responds 404, with a message that contains `plugin "<name>" not found`. | C | `api:mgr` | `none` |
| A Swarm whose leader manager does not have a given managed plugin, and another manager that has it | On that other manager, `POST /networks/create` for a Swarm-scoped network whose driver or IPAM driver is that plugin responds 400, with a message that contains `error during lookup of plugin <name>`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm whose leader manager has a given managed plugin | `POST /networks/create` for an overlay network whose IPAM driver is that plugin responds 201. This holds whether or not the manager that handles the request has the plugin. The network is then allocated ([§2](#2-network-lifecycle--api-surface)). | C | `api:mgr` | `none` |
| An allocated network, and a manager that does not have the driver or IPAM plugin of that network | Leadership moves to that manager. `GET /networks/{id}` still reports the network's allocation. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| An allocated network, and a manager that does not have the driver or IPAM plugin of that network | Leadership moves to that manager. The tasks of a service on the network stay in the `new` state. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| An allocated network, and a manager that does not have the driver or IPAM plugin of that network | Leadership moves to that manager. The daemon log of the leader manager contains `failed allocating network` and the network's ID. See [§13](#13-known-divergences--non-goals). | C | `log` | `none` |
| A manager with a managed plugin that provides a network driver | On that manager, `POST /networks/create` for a Swarm-scoped network whose `Driver` is the plugin's name without its tag, such as `name` for `name:latest`, responds 500, with a message that contains `could not resolve driver`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm whose leader manager has a managed plugin that provides an IPAM driver | `POST /networks/create` for an overlay network whose IPAM driver is the plugin's name without its tag responds 201. This holds whether or not the manager that handles the request has the plugin. The network is never allocated ([§2](#2-network-lifecycle--api-surface)). See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| | A service task whose network attachment requests an `ifname` that the OS refuses fails. Its error in `GET /tasks` contains the requested name. | B | `api:mgr` | `none` |
|  | For any value of `ifname`, `POST /services/create` for a service whose network attachment requests an `ifname` responds 201, and `GET /services` then lists the service. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A service whose network attachment requests an `ifname` that the OS refuses | The error of each task of the service in `GET /tasks` contains `error renaming interface`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| | `POST /services/create` for a service attached to the `host` network and to an overlay network responds 201. So does a request for a service on `host` that publishes a port in ingress mode. Each task of such a service fails. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |

### 2.3 Overlay networks

The rows in this subsection depend on the addresses that are allocated for a network: its subnets,
task addresses and VIPs. These addresses are allocated only for an overlay network. Where a row says
that `GET /services/{id}` then reports a VIP, it reports it within the allocation ceiling
([§11.2](#112-convergence)) after the request responds.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | On a manager, `POST /networks/create` for an overlay network with `EnableIPv4` false, at API version 1.48 or later, responds 400. At any version, the request also responds 400 when the `com.docker.network.enable_ipv4` option is set to `false`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| | On a manager, `docker network create --driver overlay --ipv4=false` exits non-zero and prints the daemon's error. So does the same command with `--opt com.docker.network.enable_ipv4=false` in place of `--ipv4=false`. | C | `api:mgr` | `cli` |
| | A user-specified `vxlanid_list` is honored: subnet *i* uses VNI *i*. | B | `underlay` | `os` |
| | `POST /networks/create` for an overlay network whose `vxlanid_list` has fewer VNIs than the network has subnets responds 201. Once the network is allocated ([§2](#2-network-lifecycle--api-surface)), `GET /networks/{id}` on a manager reports the same number of VNIs in `vxlanid_list` as the overlay has subnets. These are the requested VNIs, then an auto-allocated VNI for each remaining subnet. | C | `api:mgr` | `none` |
| | `POST /networks/create` for an overlay network whose `vxlanid_list` has more VNIs than the network has subnets responds 201. Once the network is allocated ([§2](#2-network-lifecycle--api-surface)), `GET /networks/{id}` on a manager reports the same number of VNIs in `vxlanid_list` as the overlay has subnets. These are the first VNIs of the requested list. `POST /networks/create` for another overlay network whose `vxlanid_list` has one of the remaining VNIs responds 201, and that network is then allocated. | C | `api:mgr` | `none` |
| An allocated overlay network | `POST /networks/create` for another overlay network, with a VNI in its `vxlanid_list` that `GET /networks/{id}` reports in the first network's `vxlanid_list`, responds 201. `GET /networks` on a manager then lists the new network. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| An allocated overlay network | `docker network create --driver overlay --opt com.docker.network.driver.overlay.vxlanid_list=<VNI>`, with a VNI that `docker network inspect` prints in the first network's `vxlanid_list`, exits zero. `docker network ls` on a manager then lists the new network. | C | `api:mgr` | `cli` |
| An overlay network that is not allocated | `GET /networks` lists the network. `GET /networks/{id}` returns the network. | C | `api:mgr` | `none` |
| | An overlay network that is not allocated does not prevent `GET /networks` from listing any other Swarm-scoped network. It also does not prevent `GET /networks/{id}` from returning any other Swarm-scoped network. | B | `api:mgr` | `none` |
| | Auto-allocated VNIs come from [4096, 2²⁴). | C | `underlay` | `none` |
| | `GET /networks/{id}` reports the effective `vxlanid_list` in `Options`. | C | `api:mgr` | `none` |
| | `POST /networks/create` for an overlay network responds 201 whatever value `com.docker.network.driver.mtu` has. `GET /networks/{id}` on a manager then reports that value in `Options`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| | `docker network create --driver overlay` exits zero whatever value the `mtu` option has. `docker network inspect` on a manager then prints that value in the network's options. | C | `api:mgr` | `cli` |
| | `POST /networks/create` for an overlay network with `AuxiliaryAddresses` in `IPAM.Config` responds 201. `GET /networks/{id}` on a manager then does not report these addresses. A task or VIP on the network can get one of these addresses. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A created or stopped container with a static IPv4 address on an attachable overlay, and another container on the overlay that uses that address | `POST /containers/{id}/start` responds with a 4xx or 5xx status. | B | `api:active` | `none` |
| A created or stopped container with a static IPv4 address on an attachable overlay, and another container on the overlay that uses that address | `POST /containers/{id}/start` responds 500 after 20 seconds. The response's message contains `attaching to network failed, make sure your network options are correct and check manager logs: context deadline exceeded`. See [§13](#13-known-divergences--non-goals). | C | `api:active` | `none` |
| A container on an attachable overlay | `docker run -d --network --ip` with that overlay and the container's IPv4 address on it exits non-zero and prints the daemon's error. | B | `api:active` | `cli` |
| An attachable overlay, and a node with a local attachment to it | On that node, `POST /containers/create` for a container on the overlay, with a static IPv4 address outside the overlay's subnets, responds 400. The response's message contains `no configured subnet contains IP address`. See [§13](#13-known-divergences--non-goals). | C | `api:active` | `none` |
| An attachable overlay, and a node without a local attachment to it | On that node, `POST /containers/create` for a container on the overlay, with a static IPv4 address outside the overlay's subnets, responds 201. `POST /containers/{id}/start` for that container then responds 500 after 20 seconds, with a message that contains `attaching to network failed`. See [§13](#13-known-divergences--non-goals). | C | `api:active` | `none` |
| An attachable overlay | `docker run -d --network --ip` with that overlay and an IPv4 address outside its subnets exits non-zero and prints the daemon's error. | B | `api:active` | `cli` |
| A standalone container on an attachable overlay | The container's attachment task disappears from `GET /tasks` on a manager. The attachment task disappears after the container disconnects from the overlay or stops ([§2.2](#22-swarm-scoped-networks)). On any node whose availability is `active`, `POST /containers/{id}/start` for another container on the overlay, with the first container's address as its static IPv4 address, then responds 204. | B | `api:mgr`, `api:active` | `none` |
| A service in vip mode | `GET /services/{id}` reports one VIP in `Endpoint.VirtualIPs` for each overlay network that the service is attached to. Each of these VIPs is from that network's subnet. | B | `api:mgr` | `none` |
| A service in vip mode that publishes an ingress-mode port | `GET /services/{id}` for the service includes one VIP on the ingress network. | B | `api:mgr` | `none` |
| A service in vip mode that is attached to a network | After `POST /services/{id}/update` that leaves the service in vip mode and attached to that network responds 200, `GET /services/{id}` reports the same VIP on that network as before. | B | `api:mgr` | `none` |
| | After the leader changes, `GET /services/{id}` reports the same VIPs as before. | B | `api:mgr` | `none` |
| A service in vip mode | `docker service inspect` prints one VIP in `Endpoint.VirtualIPs` for each overlay network that the service is attached to. | B | `api:mgr` | `cli` |
| | The VIP that `GET /networks/{id}?verbose=true` reports for a service is the address that load-balances to its tasks. | B | `api:mgr`, `api:wkr` (on a node with a local attachment to the network), `task` | `os` |
| | Within the ceiling of the convergence class ([§11.2](#112-convergence)) of a change to a service's tasks, two lists of tasks for the service are equal. The first list is the tasks that `GET /networks/{id}?verbose=true` reports. The second list is the service's running tasks, as [§5](#5-service-discovery) defines them. | B | `api:mgr`, `api:wkr` (on a node with a local attachment to the network) | `none` |
| A node with a local attachment to an overlay network | On that node, `GET /networks/{id}?verbose=true` for that network reports `Services`. `Services` gives the VIP of each service and the endpoint IP of each task. On the ingress network, it also gives each service's published ports. | C | `api:mgr`, `api:wkr` | `none` |
| A node that does not have a local attachment to an overlay network | On that node, `GET /networks/{id}?verbose=true` for that network does not report any services. | C | `api:mgr`, `api:wkr` | `none` |
| A node with a local attachment to an overlay network | On that node, `GET /networks/{id}` for the network lists in `Peers` each node that has a local attachment to the network. This includes the node itself. The `IP` of each entry is the advertise address of that node, whatever its data-path address is. | C | `api:mgr`, `api:wkr` | `none` |
| | On a manager, at API version 1.52 or later, `IPsInUse` in the `Status.IPAM` of `GET /networks/{id}`, with the network's name or a partial ID, increases when a task attaches. It decreases when a task leaves. See [§13](#13-known-divergences--non-goals). | B | `api:mgr` | `none` |
| An overlay network with an IPv4 subnet that has no `IPRange` | On a manager, at API version 1.52 or later, `GET /networks/{id}`, with the network's name or a partial ID, reports `IPsInUse` and `DynamicIPsAvailable` for the subnet. Their sum equals the number of addresses in the subnet at every point during churn. That number is 2^(32 − prefix length). See [§13](#13-known-divergences--non-goals). | B | `api:mgr` | `none` |
| An overlay network with a subnet that has an `IPRange` | Every task address, VIP and node load-balancer address is inside that `IPRange`. `GET /tasks`, `GET /services/{id}` and `GET /networks/{id}` report these addresses. The `Gateway` that `GET /networks/{id}` on a manager reports for the subnet is none of them. | B | `api:mgr` | `none` |
| An overlay network created with a subnet that has an `IPRange` and no `Gateway` | The `Gateway` that `GET /networks/{id}` on a manager reports for the subnet is the lowest address in `IPRange` other than the subnet's network address. | C | `api:mgr` | `none` |
| | On a manager, at API version 1.52 or later, `GET /networks/{id}` reports `Status.IPAM` with per-subnet `IPsInUse` and `DynamicIPsAvailable`. | C | `api:mgr` | `none` |
| | At API versions before 1.52, `GET /networks/{id}` does not include `Status`. | B | `api:mgr`, `api:wkr` | `none` |
| A worker with a local attachment to an overlay network | At API version 1.52 or later, the `Status.IPAM` of `GET /networks/{id}` on that worker reflects only the worker's own endpoints. It does not change when a task attaches on another node. | C | `api:wkr` | `none` |

A regression revealed the failed-allocation row. In that regression, one unallocated network made
listing and inspection fail for every Swarm-scoped network.
[moby/moby#53325](https://github.com/moby/moby/pull/53325) fixed the regression.

The lower bound of the range of auto-allocated VNIs avoids the 802.1Q VLAN range for Windows.

### 2.4 The ingress network

A Swarm-scoped network created with `Ingress` set is an ingress network. An overlay network named
`ingress` and created with the label `com.docker.swarm.internal` is also an ingress network, whatever
the label's value. Older daemons created the ingress network in this second form, without `Ingress`.
`GET /networks` reports `Ingress` as true for both forms.

An ingress network exists while `GET /networks` on a manager lists a network whose `Ingress` is
true. That network is the ingress network. `swarm init` creates it with `Ingress` set and names it
`ingress`. It can be removed, and an ingress network can then be created with another name. A copy
that a node keeps after the network is deleted ([§13](#13-known-divergences--non-goals)) does not
count.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A node that is not in a Swarm | `GET /networks` does not list a network whose `Ingress` is true. Within the allocation ceiling ([§11.2](#112-convergence)) after `POST /swarm/init` responds 200 on the node, an ingress network exists. Its name is `ingress`. | B | `api:solo`, `api:mgr` | `none` |
| A Swarm with an ingress network | `POST /networks/create` for an overlay network with `Ingress` set and `Attachable` not set responds 409. | B | `api:mgr` | `none` |
| A Swarm with an ingress network | `docker network create --driver overlay --ingress` exits non-zero and prints the daemon's error. | B | `api:mgr` | `cli` |
| | `POST /networks/create` with `Ingress` set and a node-local driver, or no `Driver`, responds 403. | B | `api:mgr` | `none` |
| | `docker network create --ingress` with a node-local driver, or without `--driver`, exits non-zero and prints the daemon's error. | B | `api:mgr` | `cli` |
| A manager on which a managed plugin provides a multi-host driver other than `overlay` | On that manager, `POST /networks/create` with `Ingress` set, `Attachable` not set, and that driver responds 501. | B | `api:mgr` | `none` |
| A manager on which a managed plugin provides a multi-host driver other than `overlay` | On that manager, `docker network create --ingress` with that driver exits non-zero, and prints the daemon's error. | B | `api:mgr` | `cli` |
| A manager on which a managed plugin provides a multi-host driver other than `overlay` | `overlay` is the only multi-host driver that is supported for the ingress network. On that manager, `POST /networks/create` with `Ingress` set, `Attachable` not set, and that driver responds 501. | C | `api:mgr` | `none` |
| | `POST /networks/create` with both `Ingress` and `Attachable` set and a multi-host driver responds 400. | B | `api:mgr` | `none` |
| | `docker network create --ingress --attachable` with a multi-host driver exits non-zero and prints the daemon's error. | B | `api:mgr` | `cli` |
| | `POST /services/create` or `POST /services/{id}/update` that names the ingress network in `TaskTemplate.Networks` responds 400. The request does not create or change the service. | B | `api:mgr` | `none` |
| | `docker service create --network` and `docker service update --network-add` that name the ingress network exit non-zero and print the daemon's error. The commands do not create or change the service. | B | `api:mgr` | `cli` |
| A Swarm with a service that publishes a port in ingress mode | `DELETE /networks/{id}` for the ingress network responds with a 4xx or 5xx status. The network stays listed. | B | `api:mgr` | `none` |
| A Swarm with a service that publishes a port in ingress mode | `DELETE /networks/{id}` for the ingress network responds 400, with a message that contains `ingress network cannot be removed because service`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with a service that publishes a port in ingress mode | `docker network rm` for the ingress network, given `y` at its confirmation prompt, exits non-zero and prints the daemon's error. `docker network ls` then still lists the network. | B | `api:mgr` | `cli` |
| A Swarm with an ingress network | `docker network rm` for the ingress network, given any answer other than `y` at its confirmation prompt, exits zero. `docker network ls` then still lists the network. | C | `api:mgr` | `cli` |
| A Swarm with an ingress network created with `Ingress` set, where no service publishes a port in ingress mode | `DELETE /networks/{id}` for the ingress network responds 204. `GET /networks` on a manager then does not list a network whose `Ingress` is true. | B | `api:mgr` | `none` |
| A Swarm with an ingress network created with `Ingress` set, where no service publishes a port in ingress mode | `docker network rm` for the ingress network, given `y` at its confirmation prompt, exits zero. `docker network ls` then does not list the network. | B | `api:mgr` | `cli` |
| A Swarm without an ingress network | `POST /networks/create` for an overlay network with `Ingress` set, `Attachable` not set and a name other than `ingress` responds 201. Within the allocation ceiling ([§11.2](#112-convergence)), `GET /networks` on a manager then lists that network with `Ingress` true. | B | `api:mgr` | `none` |
| A Swarm without an ingress network | `docker network create --driver overlay --ingress` with a name other than `ingress` exits zero. Within the allocation ceiling ([§11.2](#112-convergence)), `docker network ls` then lists the network. | B | `api:mgr` | `cli` |
| A Swarm without an ingress network | `POST /services/create` and `POST /services/{id}/update` with a port in ingress mode respond with a 4xx or 5xx status. The requests do not create or change the service. | B | `api:mgr` | `none` |
| A Swarm without an ingress network | `POST /services/create` and `POST /services/{id}/update` with a port in ingress mode respond 400, with a message that contains `service needs ingress network, but no ingress network is present`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with an ingress network created with `Ingress` set and with a custom subnet and gateway | `GET /networks/{id}` on a manager reports that subnet and gateway for the ingress network. | B | `api:mgr` | `none` |
| A Swarm with an ingress network created with `Ingress` set and with a custom subnet and gateway | A service created in that Swarm is reachable on its ingress-mode published ports on every node. | B | `client` | `os` |
| A Swarm with an ingress network created with `Ingress` set | `GET /networks` on every node lists that network and reports its `Ingress` as true. | C | `api:mgr`, `api:wkr` | `none` |
| A Swarm with an overlay network named `ingress` that was created with the label `com.docker.swarm.internal` and without `Ingress` | `GET /networks/{id}` on a manager reports `Ingress` as true for that network. `POST /networks/create` for an overlay network with `Ingress` set responds 409. | B | `api:mgr` | `none` |
| A Swarm with an overlay network named `ingress` that was created with the label `com.docker.swarm.internal` and without `Ingress` | `POST /services/create` that names that network in `TaskTemplate.Networks` responds 400. | B | `api:mgr` | `none` |
| A Swarm with an overlay network named `ingress` created with the label `com.docker.swarm.internal` and without `Ingress`, and a service with an ingress-mode port | `DELETE /networks/{id}` for that network responds with a 4xx or 5xx status. | B | `api:mgr` | `none` |
| A Swarm with an overlay network named `ingress` created with the label `com.docker.swarm.internal` and without `Ingress`, and a service with an ingress-mode port | `DELETE /networks/{id}` for that network responds 400, with a message that contains `ingress network cannot be removed because service`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with an overlay network named `ingress` that was created with the label `com.docker.swarm.internal` and without `Ingress` | A client can reach a service's ingress-mode published port at the address of a node that runs a task of the service. | B | `client` | `os` |
| A Swarm with an overlay network named `ingress` that was created with the label `com.docker.swarm.internal` | That network is an ingress network whatever value the label has, including the empty string. | C | `api:mgr` | `none` |
| A Swarm whose only ingress network is an overlay network named `ingress`, created with the label `com.docker.swarm.internal` and without `Ingress` | On a node without a local attachment to that network, a client cannot reach an ingress-mode published port at the node's address. See [§13](#13-known-divergences--non-goals). | C | `client` | `os` |
| A Swarm whose only ingress network is an overlay network named `ingress`, created with the label `com.docker.swarm.internal` and without `Ingress` | On a worker without a local attachment to that network, `GET /networks` does not list it. See [§13](#13-known-divergences--non-goals). | C | `api:wkr` | `none` |
| A Swarm without an ingress network | `POST /networks/create` for an overlay network named `ingress`, with the label `com.docker.swarm.internal`, with `Attachable` set and without `Ingress`, responds 201. On any node whose availability is `active`, `POST /containers/{id}/start` for a created or stopped container on that network then responds 204. See [§13](#13-known-divergences--non-goals). | C | `api:active` | `none` |
| A Swarm whose ingress network was created with `Ingress` set, under a name other than `ingress` | `POST /networks/create` for an overlay network named `ingress`, with the label `com.docker.swarm.internal` and without `Ingress`, responds 201. `GET /networks` on a manager then lists two networks whose `Ingress` is true. The new network is never allocated. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm whose ingress network was created with `Ingress` set, under a name other than `ingress` | `POST /networks/create` for an overlay network named `ingress`, with the label `com.docker.swarm.internal` and without `Ingress`, responds 201. The leader manager's log then contains `Cannot allocate ingress network`. See [§13](#13-known-divergences--non-goals). | C | `log` | `none` |
| A Swarm with an overlay network named `ingress` that was created with the label `com.docker.swarm.internal` and without `Ingress` | `DELETE /networks/{id}` for that network responds 204. Each node that had a local attachment to that network then still lists it in `GET /networks` until the node's daemon restarts. See [§13](#13-known-divergences--non-goals). | C | `api:mgr`, `api:wkr` | `none` |
| A Swarm with a manager that has a local attachment to an overlay network named `ingress` created with the label `com.docker.swarm.internal` and without `Ingress` | `DELETE /networks/{id}` for that network responds 204. Before the manager's daemon restarts, `POST /networks/create` on that manager with the name `ingress` responds 409. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with a node that has a local attachment to an overlay network named `ingress` created with the label `com.docker.swarm.internal` and without `Ingress` | `DELETE /networks/{id}` for that network responds 204. Before the node's daemon restarts, tasks on that node that attach to a new ingress network with an overlapping subnet are rejected. Their error in `GET /tasks` contains `invalid pool request`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |

## 3. Observable state inside a container

[network-common.md §2](network-common.md#2-endpoint-interfaces) covers the name and MAC address of
an endpoint's interface and a static address, for every network driver.
[network-common.md §3](network-common.md#3-default-gateway) covers the default route. Their rows
hold for overlay endpoints. An overlay endpoint does not give the container a gateway or default
route, whatever its `GwPriority`.

In this section, the inner MTU is the MTU of a container's interface on an overlay network. `base`
is the value of the network's `com.docker.network.driver.mtu` option, or 1500 when the option is 0
or not set. Each row about the inner MTU holds for each of a container's interfaces on overlay
networks, whatever other overlay networks the container is attached to.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | The IPv4 address of a container's interface on an overlay network is the `IPAddress` that `GET /containers/{id}/json` reports for that network. For a task, it is also the address that `GET /tasks` on a manager reports in `NetworksAttachments[].Addresses` for the network, without its prefix length. The address is in a subnet that `GET /networks/{id}` reports for the network. | B | `task`, `ctr`, `api` | `os` |
| Linux nodes with a standalone container whose endpoint settings for an overlay network set `MacAddress` | The MAC address of the container's interface on that network is the `NetworkSettings.Networks.<network>.MacAddress` that `GET /containers/{id}/json` reports. | B | `ctr`, `api` | `os` |
| Linux nodes with a container whose endpoint settings for an overlay network do not set `MacAddress` | The MAC address of the container's interface on that network is the `NetworkSettings.Networks.<network>.MacAddress` that `GET /containers/{id}/json` reports. | B | `task`, `ctr`, `api` | `os` |
| A running standalone container on an attachable Swarm-scoped network | After `POST /networks/{id}/disconnect` for the container responds 200, the container does not have the interface that it had on that network. See [network-common.md §7](network-common.md#7-known-divergences--non-goals). | B | `ctr` | `os` |
| Linux nodes with a container's endpoint on an overlay network that does not set `MacAddress` | The MAC address of the endpoint's interface is `02:42:` and then the four octets of its IPv4 address. | C | `task`, `ctr`, `api` | `none` |
| An underlay whose MTU is `base` or more | An IP packet the size of the inner MTU, with don't-fragment set, sent from a container's interface on an overlay network to a container on another node, arrives. It crosses the underlay as one outer packet that is not fragmented. This holds on both cleartext and `encrypted` networks. See [§13](#13-known-divergences--non-goals). | B | `task` → `task`, `underlay` | `os` |
| An underlay whose MTU is `base` | For an IP packet the size of the inner MTU, sent from a container's interface on an overlay network to a container on another node, the outer packet on the underlay is exactly the underlay MTU on a cleartext network. On an `encrypted` network, it is at most the underlay MTU and more than the underlay MTU minus 4. See [§13](#13-known-divergences--non-goals). | B | `underlay` | `os` |
| A node with an IPv4 data-path address, and a cleartext network | The inner MTU is `base − 50`. By default, the inner MTU is 1450. | C | `task` | `os` (Windows) |
| Linux nodes with an IPv6 data-path address, and a cleartext network | The inner MTU is `base − 70`. By default, the inner MTU is 1430. | C | `task` | `none` |
| Linux nodes with an IPv4 data-path address, and an `encrypted` network | The inner MTU is `base − 50 − 26`, rounded down to a multiple of 4. By default, the inner MTU is 1424. | C | `task` | `none` |
| Linux nodes with an IPv6 data-path address, and an `encrypted` network | The inner MTU is `base − 70 − 26`, rounded down to a multiple of 4. By default, the inner MTU is 1404. | C | `task` | `none` |
| Windows nodes, and a cleartext overlay without `com.docker.network.driver.mtu` | An endpoint's MTU is the MTU that Linux nodes report for the same network. | B | `task` on two nodes | `os` |
| A Linux node and a Windows node with tasks on a cleartext overlay network whose `com.docker.network.driver.mtu` is set to a value other than 1500 | The two nodes' inner MTUs on that network differ, because Windows nodes ignore the option. A frame larger than the smaller inner MTU, sent by a task on the node with the larger one, does not reach the other node. The sender does not get an ICMP error. TCP traffic between the two nodes is not affected. | C | `task` on two nodes | `os` |
| | A container on a non-internal overlay has a default route. | B | `task`, `ctr` | `os` |
| Linux nodes | A container on a multi-subnet overlay has a static route to each subnet of the overlay other than its own subnet. | C | `task` | `os` |

The row about different inner MTUs on two nodes documents a failure mode that is hard to diagnose
and common in the field. It occurs especially where the overlay runs inside further encapsulation.
In this failure mode, large TCP transfers work, because each side keeps its segments within the MSS
that the other side advertises. But large UDP datagrams and don't-fragment pings disappear without
an error.

## 4. East-west reachability

Unless a row says otherwise, every expectation below holds whether the sender and receiver are on
the same node or on different nodes. It also holds in all four combinations in which each end is
either a service task or a standalone container on an `attachable` overlay. Any one of these cases
can fail on its own. One reason is that the two placements, same node and different nodes, take
different paths through the overlay. The other reason is that tasks and standalone containers attach
to the overlay differently.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A unicast frame that a container sends reaches the destination container on the same overlay subnet. | B | `task`, `ctr` | `os` |
| | A broadcast frame that a container sends reaches the other containers on the same overlay subnet on its own node. It does not reach containers on other nodes. | C | `task`, `ctr` | `os` |
| | A multicast datagram that a container sends to a group reaches the other members of the group on the same overlay subnet on its own node. It does not reach members on other nodes. | C | `task`, `ctr` | `os` |
| | A unicast packet that a container on subnet A sends reaches the destination container on subnet B of the same overlay network. | B | `task`, `ctr` | `os` |
| | TCP, UDP and SCTP traffic that a container sends reaches the destination container on the same overlay. | B | `task`, `ctr` | `os` |
| | A don't-fragment packet larger than the MTU of the sender's interface on the overlay fails at the sender with `EMSGSIZE`. | B | `task`, `ctr` | `os` (Windows) |
| Two containers that do not have an overlay network in common | Neither container can reach an address that the other container has on an overlay network. | B | `task`, `ctr` | `os` |
| Linux nodes | Two containers on one node cannot reach each other's addresses on a gateway network ([§8.1](#81-gateway-network)) that the operator did not create. | B | `task`, `ctr` | `os` |

The row about the gateway network is an isolation property. Containers on unrelated overlays on a
node all have addresses on the node's gateway network. If they could reach each other at those
addresses, the overlays would not isolate them.

## 5. Service discovery

Service discovery in a Swarm is DNS. A container on a user-defined network sends its DNS queries to
the embedded DNS resolver, which is a DNS server that the daemon runs for the container. In this
section, a name resolves to an address when the embedded DNS resolver answers a DNS query for the
name's A record with that address. A name stops resolving when the embedded DNS resolver no longer
answers with an address for the name.

A row in this section names `none` when the embedded DNS resolver answers the query from its own
records. It does this the same way on every platform. Every row holds for a query
from a service task and for a query from a standalone container. This holds whatever kind of
workload the resolved name belongs to.

[container-dns.md](container-dns.md) specifies how a container resolves names, whatever its
network's driver. It specifies the `resolv.conf` that the container gets, how answers are built, and
what is forwarded upstream. Its expectations hold inside service tasks. They also hold for the names
and addresses of the endpoints in this section, including endpoints on other nodes. Each
expectation keeps the tier and dependencies that it has in container-dns.md. This section covers
the names that Swarm adds, on overlay networks unless a row says otherwise. For the rows in
container-dns.md that depend on a container's DNS options, a service task is created with those
options in the service's DNS config.

In this section, a service's running tasks are the tasks that `GET /tasks` reports with
`Status.State` and `DesiredState` both `running`. If a task has a healthcheck, it is a running task
only once its container has reported healthy. This excludes a task whose container has reported
unhealthy at any time, even if a later health check passed. `<service>` is the name of a service,
and `<alias>` is one of the service's network aliases.

Where a row says that a name resolves to the addresses of tasks, these are the tasks' addresses on
one of the overlay networks, other than the ingress network, that the querier shares with the
service. The answer holds the addresses on one such network only.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A service in vip mode that has a running task | `<service>` resolves to the service's VIP on one of the overlay networks, other than the ingress network, that the querier shares with it. | B | `task`, `ctr` | `none` |
| A service in dnsrr mode | `<service>` resolves to the addresses of the running tasks. | B | `task`, `ctr` | `none` |
| A service in `dnsrr` endpoint mode | `GET /services/{id}` for the service does not report a VIP in `Endpoint.VirtualIPs`. | B | `api:mgr` | `none` |
| | `tasks.<service>` resolves to the addresses of all running tasks. | B | `task`, `ctr` | `none` |
| | A service network alias resolves as `<service>` does: to the VIP in vip mode while the service has a running task, and to the addresses of the running tasks in dnsrr mode. | B | `task`, `ctr` | `none` |
| | `tasks.<alias>` resolves to the addresses of the running tasks of every service that has that alias. | B | `task`, `ctr` | `none` |
| | A task's container name resolves to its overlay address. | B | `task`, `ctr` | `none` |
| A task of a replicated service | The task's container name is `<service>.<slot>.<taskID>`. `<slot>` is the task's `Slot` that `GET /tasks` reports. | C | `task`, `ctr` | `none` |
| A task of a global service | The task's container name is `<service>.<nodeID>.<taskID>`. | C | `task`, `ctr` | `none` |
| A task with a healthcheck whose container has not yet reported healthy | The task's container name does not resolve. `tasks.<service>` and `tasks.<alias>` do not resolve to the task's address. In dnsrr mode, `<service>` and the service's aliases do not resolve to the task's address either. | B | `task`, `ctr` | `none` |
| | Once a task's desired state is not `running`, or its container has stopped or has reported unhealthy at any time, its container name stops resolving. `tasks.<service>` and, in dnsrr mode, `<service>` then stop resolving to the task's address. | B | `task`, `ctr` | `none` |
| | Once no container of a removed service is running, the service's name and aliases stop resolving. | B | `task`, `ctr` | `none` |
| A service in vip mode that still exists, such as one scaled to 0 replicas | Once no container of the service is running, the embedded resolver forwards a query for `<service>` upstream. It handles the query as it handles a name that it does not have a record for. | C | `task`, `ctr` | `none` |
| A service in vip mode that was created with no replicas | The embedded resolver forwards a query for `<service>` upstream. It handles the query as it handles a name that it does not have a record for. | C | `task`, `ctr` | `none` |
| A service in vip mode with running containers, each with a healthcheck, none of which has reported healthy | The embedded resolver forwards a query for `<service>` upstream. It handles the query as it handles a name that it does not have a record for. | C | `task`, `ctr` | `none` |
| Linux nodes, and a service in vip mode that still exists, such as one scaled to 0 replicas | Once no container of the service is running, a TCP connection to the service's VIP fails at the client with `EHOSTUNREACH`. The client does not get a TCP reset. | C | `task`, `ctr` | `os` |
| A standalone container on an attachable overlay, whose hostname is not its name | Once `POST /containers/{id}/rename` for the container responds 204, its new name resolves for containers on other nodes within the reassignment ceiling ([§11.2](#112-convergence)). Within the same ceiling, its old name stops resolving for those containers. | B | `task`, `ctr` | `none` |
| A service whose tasks have a network alias | A rolling update of the service in which at least one task keeps running throughout starts new tasks that also have that alias. Examples are an update in start-first order and an update of fewer tasks at a time than are running. The alias does not stop resolving for longer than the allocation ceiling: within any interval of that length, the alias resolves. | B | `task`, `ctr` | `none` |
| A container attached to a Swarm-scoped network and a local network, such as a user-defined bridge network, and an endpoint with the same name on each of those networks | A query from that container for the name gets the address on the Swarm-scoped network, and not the address on the local network. | B | `ctr` | `os` (Windows) |
| Two services that publish ports in ingress mode and do not share another network | For a task of either service, the other service's name does not resolve. The container names of the other service's tasks do not resolve for that task either. | B | `task` | `none` |
| A Swarm-scoped network of a node-local driver | A task's names resolve for the containers on its node that are attached to the network. They resolve as a container's names do on any user-defined network. | B | `task`, `ctr` | `none` |
| A Swarm-scoped network of a node-local driver | On that network, `<service>`, `tasks.<service>` and service aliases do not resolve. | C | `task`, `ctr` | `none` |
| A Swarm-scoped network of a node-local driver, and a task on it with a healthcheck | The task's names on that network resolve without waiting for its container to report healthy. | C | `task`, `ctr` | `none` |

## 6. Load balancing (east-west VIP)

The client that sends to a VIP can be a service task or a standalone container on an `attachable`
overlay. Every row in this section holds for either client. The backends behind a VIP are always
service tasks. A row observed at the receiving end is observed from `task`. A VIP load-balances
across a set of tasks when flows to the VIP reach every task in the set.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | Traffic to a service VIP reaches that service's tasks. | B | `task`, `ctr` | `os` |
| | The VIP load-balances TCP, UDP and SCTP traffic. | B | `task`, `ctr` | `os` |
| Linux nodes whose kernel does not offer IPVS, and a service in vip mode with a running task on a network that is programmed on the node | The daemon log of that node contains `Could not get ipvs family information from the kernel`. See [Host requirements](#host-requirements). | C | `log` | `none` |
| | All packets of a flow reach the same task. | B | `task`, `ctr` | `os` |
| | Traffic to any port on a service VIP reaches the service's tasks, whether or not the service publishes that port. | B | `task`, `ctr` | `os` |
| A node without a task of a service | Traffic to the service's VIP from a task or container on that node reaches the service's tasks. | B | `task` or `ctr` on a task-free node | `os` |
| NAT mode (the default) | The task observes a source address that is stable for each client node. That address is neither the client's nor any task's. | B | `task` | `os` |
| NAT mode | The source address that the task observes is the load-balancer address of the client's node on the overlay. | C | `task` | `os` |
| NAT mode | The destination the task observes is its own address. | B | `task` | `os` |
| DSR mode (the `dsr` option) | The task observes the client's real source address. | B | `task` | `os` |
| DSR mode | The destination the task observes is the VIP. | B | `task` | `os` |
| A task, and a sender task or container that shares an overlay subnet with it | The sender connects to the task's own address on that subnet. The connection arrives at the task with the sender's address on that subnet as its source. | B | `task` | `os` |
| | Every connection from a task to the VIP of its own service reaches a task of the service. | B | `task` | `os` |
| NAT mode | The VIP of a service load-balances the connections from one of its tasks across all of the service's tasks, that task included. | C | `task` | `os` |
| DSR mode | Every connection from a task to the VIP of its own service reaches the task itself. The task observes the VIP as the source address of the connection. See [§13](#13-known-divergences--non-goals). | C | `task` | `os` |

### 6.1 Distribution

The distribution property allows round-robin, equal-weight random, and 5-tuple hashing. It excludes
least-connections and other schedulers that depend on backend state. Clause (1), a limit, cannot
exclude them on its own because least-connections also converges to 1/*k* with identical backends.
Clause (2), invariance, tells them apart. There are defensible reasons to switch to a scheduler that
depends on backend state. For that reason, the property is tier C. Such a switch is a deliberate
change. The property does not fix a number of flows or a tolerance on *fᵢ(n)/n*. A difference in
connection holding time is one of the properties in clause (2), and tells least-connections apart
from round-robin.

> Let *H* be the set of healthy tasks, |*H*| = *k*, held constant. For *n* client flows let *fᵢ(n)*
> be the number of them that reach task *i*. Then (1) lim *fᵢ(n)/n* = 1/*k* for every *i* ∈ *H*,
> and (2) that limit is invariant to any observable property of the tasks: connection holding time,
> in-flight concurrency, latency, byte volume, or load.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
|  | Properties (1) and (2) above hold for east-west VIP. In DSR mode, they do not hold for flows from a task to the VIP of its own service. See [§13](#13-known-divergences--non-goals). | C | `task` | `os` |
|  | Properties (1) and (2) above hold for ingress. | C | `client`, `task` | `os` |

### 6.2 Health and drain

Every row in this subsection holds for flows through the service's VIP and for flows that enter
through an ingress-mode published port. A task shuts down gracefully when the orchestrator shuts it
down, for example after a scale-down, an update that replaces the task, the removal of its service
or a drain of its node. `POST /containers/{id}/stop` for the task's container is not a graceful
shutdown in these rows. A flow starts on a node when its client is on that node, or when it enters
through a published port on that node. In DSR mode, the rows do not apply to flows from a task to
the VIP of its own service ([§13](#13-known-divergences--non-goals)). In these rows, a task's
process ignores its stop signal and keeps accepting connections until it is killed at the end of the
stop grace period.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A task with a healthcheck does not receive traffic until it first reports healthy. | B | `task` | `os` |
| | A task that does not have a healthcheck receives traffic once it is running, without waiting for a health status. | B | `task` | `os` |
| A service whose tasks have a healthcheck and a stop grace period longer than the allocation ceiling ([§11.2](#112-convergence)) | Within the allocation ceiling after one of the service's tasks becomes unhealthy, no new flows from any node reach that task. | B | `task` | `os` |
| An established connection to a task | The task shuts down gracefully. The connection keeps exchanging data in both directions until the task closes it, within the task's stop grace period. | B | `task` | `os` |
| A task with a stop grace period of at least 10 seconds | When the task shuts down gracefully, no flow that starts on the task's node after the container's `kill` event reaches the task. This holds at once, without a ceiling, until the container's `die` event. See [§13](#13-known-divergences--non-goals). | B | `task` | `os` |
| A task with a stop grace period longer than the allocation ceiling ([§11.2](#112-convergence)) | The task shuts down gracefully. Within that ceiling after `GET /tasks` first reports a `DesiredState` other than `running` for the task, no flow that starts on another node reaches the task. See [§13](#13-known-divergences--non-goals). | B | `task` | `os` |

## 7. Published ports

A published port is a port number and a protocol, as the `PublishedPort` and `Protocol` of an
entry in a service's `EndpointSpec.Ports` give them. An entry without `Protocol` has the protocol
`tcp`. Thus `53/tcp` and `53/udp` are two different published ports. A published port is chosen
when the request that creates or updates the service sets its `PublishedPort`
([§7.2.1](#721-port-allocation)).

A service publishes a port in one of two modes:

- `mode=ingress`, the default, publishes the port through the routing mesh on every node.
- `mode=host` publishes the port directly on the node that runs each task.

The expectations in this section hold for both modes. [§7.1](#71-hairpin) covers hairpin paths.
[§7.2](#72-modeingress) covers what is specific to the routing mesh. [§7.3](#73-modehost) covers
what is specific to `mode=host`.

Every expectation in this section and its subsections holds whether or not the `dsr` option is set
on the overlay networks that a service is attached to, other than the ingress network.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A service that publishes a port | A flow from a client to that port at a node's address reaches a task of the service on its target port. | B | `task` | `os` |
| | A client reaches a task through a published port, whether the port is TCP, UDP or SCTP. See [§13](#13-known-divergences--non-goals). | B | `client` | `os` |
| A service that publishes a port | Replies from a task of the service to a client that connected to that port reach the client. | B | `client` | `os` |
| A service that publishes a UDP port, with tasks in which nothing listens on the target port | A UDP datagram from a client to that port at a node's address gets an ICMP port-unreachable error at the client. | B | `client` | `os` |
| A task that listens on a port that its service does not publish | A connection or datagram that a client sends to that port at a node's address does not reach the task. | B | `client` → `task` | `os` |
| | `POST /services/create` or `POST /services/{id}/update` with a spec that lists the same chosen published port twice responds 400, whatever the publish modes and target ports of the two entries are. The service is not created or not changed. | B | `api:mgr` | `none` |
| | `docker service create` with two `--publish` options that set the same published port exits non-zero and prints the daemon's error. The command does not create the service. | B | `api:mgr` | `cli` |
| A service with a chosen published port to some target port | `docker service update --publish-add` that publishes the same published port to another target port exits non-zero and prints the daemon's error. The command does not change the service. | B | `api:mgr` | `cli` |
| A service with a chosen published port | `docker service update --publish-add` with the same published port, target port and publish mode exits zero. `docker service inspect` then prints the same ports as before. | C | `api:mgr` | `cli` |
| A service with an auto-assigned published port | `docker service update --publish-add` that publishes that port's number to another target port exits zero. `docker service inspect` then prints that number for the new entry, and another number for the auto-assigned entry. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `cli` |
| | `POST /services/create` with a published port that does not set `Protocol` responds 201. `GET /services/{id}` then reports `tcp` as the port's protocol. It reports `ingress` as the publish mode of a port that does not set `PublishMode`. | C | `api:mgr` | `none` |
| | `POST /services/create` with a published port whose `Protocol` is not a protocol name, such as `bogus`, responds 201. `GET /services/{id}` then reports `tcp` as the port's protocol. It reports `ingress` as the publish mode of a port with an unknown `PublishMode`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| | `docker service create` with `--publish`, in its short and long forms, `--endpoint-mode` and `--network` with `alias=` exits zero when the endpoint mode is `vip` or every port is published in `mode=host`. Then `docker service inspect` prints the ports, publish modes, endpoint mode and aliases that the flags name. | B | `api:mgr` | `cli` |
| A service | `docker service update` with `--publish-add`, in its short and long forms, `--endpoint-mode` and `--network-add` with `alias=` exits zero when the endpoint mode is `vip` or every port is published in `mode=host`. Then `docker service inspect` prints the ports, publish modes, endpoint mode and aliases that the flags name. | B | `api:mgr` | `cli` |
| A removed service whose tasks are all stopped | The service's published ports do not answer. See [§13](#13-known-divergences--non-goals). | B | `client`, `node` | `os` |
| A service whose published port was changed, and whose tasks from before the change are all stopped | The old port does not answer. See [§13](#13-known-divergences--non-goals). | B | `client`, `node` | `os` |
| A service that publishes a port | `POST /services/{id}/update` changes the service's published port. A client reaches a task of the service on the new port at the address of a node that runs a task of the service. This holds once a task from after the update is running. | B | `client`, `node` | `os` |
| A daemon with `userland-proxy` true | A published port is reachable over IPv6 at the node's IPv6 addresses, from off-node. This holds for TCP and UDP. | B | `client` | `os` |
| A daemon with `userland-proxy` true | A published port is reachable over IPv6 from the node itself, at the node's own IPv6 address and at `::1`. This holds for TCP and UDP. | B | `node` | `os` |
| | When a node accepts an IPv6 TCP connection to a published port, a task of the service receives the connection. | B | `client`, `node` | `os` |
| A daemon with `userland-proxy` false, and a port published in ingress mode, or in `mode=host` by a task whose gateway network is `docker_gwbridge` | A client cannot reach a task of the service through that port over IPv6, at any IPv6 address of the node. This holds for TCP and UDP. | C | `client`, `node` | `os` |

These IPv6 rows do not mean that the service mesh supports IPv6. The service mesh is IPv4-only
([§13](#13-known-divergences--non-goals)). The ingress network and the gateway network do not give a
task an IPv6 address. The proxy accepts traffic on `[::]` and forwards it to the task over IPv4, as
it does for any IPv4-only container. Thus the task sees the connection come from the proxy. For this
reason, the real-source row in [§7.3](#73-modehost) does not hold for IPv6 clients.

A bug revealed the row about traffic left unserved. In that bug, IPv6 connections to a published
port were accepted and never served ([moby/moby#53091](https://github.com/moby/moby/issues/53091)).
The userland proxy is necessary for a published port to be reachable over IPv6. Without the proxy,
the port is not reachable over IPv6, and this prevents that failure. The row about that is tier C
because a change that makes IPv6 reachable without the proxy would be legitimate. Such a change also
calls for a change to this spec and to the documentation.

### 7.1 Hairpin

The six paths below are independent and can fail separately. The first five hold for both publish
modes. For `mode=host`, they hold on the nodes that run a task. In this subsection, the node's
address is one of its non-loopback IPv4 addresses. In this subsection, each task or container that
connects to the node's address must be attached to at least one network that is not `internal`.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A published port is reachable from the node itself via `127.0.0.1`. | B | `node` | `os` |
| | A published port is reachable from the node itself via the node's address. | B | `node` | `os` |
| Linux nodes | A published port is reachable from a container on a local bridge network, via the node's address. | B | `ctr` | `os` |
| A service that publishes a port, and a task or container attached to one of the service's overlay networks | The port is reachable via the node's address from the task or container on the same node. | B | `task`, `ctr` | `os` |
| A service that publishes a port, and a task or container attached only to overlay networks that the service is not on | The port is reachable via the node's address from the task or container on the same node. | B | `task`, `ctr` | `os` |
| A service that publishes a port in `mode=host` | A task of the service reaches its own published port via its node's address. | B | `task` | `os` |
| | Each other row of the table in §7.1 holds with `userland-proxy` true and with `userland-proxy` false. | B | `node`, `ctr`, `task` | `os` |

On Linux nodes, the two overlay rows and the `mode=host` self-hairpin row are the paths whose
traffic leaves a bridge and then enters the same bridge again. An overlay container reaches the
node's address through `docker_gwbridge`. The connection then reaches the ingress load balancer, or
the task that serves the port, through `docker_gwbridge` again. Only those paths are subject to the
bridge's inter-container communication setting. A gateway network that the daemon creates has
inter-container communication disabled ([§8.1](#81-gateway-network)). A published port is an
explicit export to anything that can route to the node. Thus that setting must not block it.

The userland-proxy row exists because the proxy hides these paths. With the proxy, the connection
ends in the host's network namespace and does not enter the bridge again. Without the proxy, the
traffic is DNATed back across the bridge. Access control should not depend on whether the userland
proxy is enabled. Access to a published port from an overlay regressed in 29.8.0 and 29.8.1. Nobody
noticed the regression because it broke access only with `userland-proxy` false
([moby/moby#53713](https://github.com/moby/moby/issues/53713)).

### 7.2 `mode=ingress`

The routing mesh publishes an ingress-mode port on every node, whichever nodes run the service's
tasks. The rows in this subsection hold with `userland-proxy` true and with `userland-proxy` false,
unless a row names a value.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A published port is reachable on every node, including nodes that do not run a task of the service. | B | `client` | `os` |
| A node with firewalld running, and a service that publishes a port in ingress mode | After a firewalld reload on the node, the published port is reachable on the node. | B | `client` | `os` |
| | All packets of a flow reach the same task. | B | `client` | `os` |
| | The task does not observe the client's real source address. | C | `task` | `os` |
| | A task is not reachable from another task via its ingress-network address, or via its service's VIP on the ingress network, on any port. | B | `task` → `task` | `os` |
| | A task's default route is not on the ingress network. | B | `task` | `os` |
| | `POST /services/create` or `POST /services/{id}/update` for a service in `dnsrr` endpoint mode that publishes a port in ingress mode responds 400. This holds whether or not the port is chosen. The service is not created or not changed. | B | `api:mgr` | `none` |
| | `docker service create` with `--endpoint-mode dnsrr` and a port published in ingress mode exits non-zero and prints the daemon's error. The command does not create the service. | B | `api:mgr` | `cli` |

#### 7.2.1 Port allocation

An ingress-mode port is auto-assigned when the request that creates or updates the service does not
set its `PublishedPort`. `GET /services/{id}` then reports a published port for it in
`Endpoint.Ports`. The auto-assignment range is the set of published ports that an auto-assigned
port can get. A port of either publish mode is chosen when the request sets its `PublishedPort`.
A service's ports are allocated after the request responds. Where a row in this subsection says that
`GET /services/{id}` then reports a port, it reports it within the allocation ceiling
([§11.2](#112-convergence)) after the request responds.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | `POST /services/create` for a service with an auto-assigned port responds 201. `GET /services/{id}` then reports a published port for it that no other service publishes in ingress mode. | B | `api:mgr` | `none` |
| | The auto-assignment range is 30000 to 32767. It is not configurable. | C | `api:mgr` | `none` |
| | `POST /services/create` for a service with a chosen ingress-mode port responds 201. `GET /services/{id}` then reports that port in `Endpoint.Ports`. | B | `api:mgr` | `none` |
| | `POST /services/create` for a service with a chosen ingress-mode port from 1 to 65535 responds 201. This includes a port in the auto-assignment range. `GET /services/{id}` then reports that port in `Endpoint.Ports`. | C | `api:mgr` | `none` |
| | `POST /services/create` for a service with a chosen ingress-mode port above 65535 responds 201. `GET /services/{id}` then does not report the port in `Endpoint.Ports`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with a service that publishes a chosen ingress-mode port in the auto-assignment range, and another service, created after it, with auto-assigned ports | `GET /services/{id}` for the other service does not report that port for any of its auto-assigned ports. | B | `api:mgr` | `none` |
| | `GET /services/{id}` reports each ingress-mode port of a service in `Endpoint.Ports`, with its published port. This holds for auto-assigned and chosen ports. | B | `api:mgr` | `none` |
| | A client reaches a task of a service at any node's address on the published port that `GET /services/{id}` reports for an ingress-mode port. This holds for auto-assigned and chosen ports. | B | `api:mgr`, `client` | `os` |
| A Swarm with a service that has an auto-assigned port | `POST /services/{id}/update` that keeps the number of ports, and keeps the name, protocol and target port of that port, responds 200. `GET /services/{id}` then reports the same published port for it. | B | `api:mgr` | `none` |
| A Swarm with a service that has an auto-assigned port | After `POST /services/{id}/update` adds or removes another port of the service, in either publish mode, the auto-assigned port's published port changes. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with a service that publishes a port in ingress mode | `POST /services/create` or `POST /services/{id}/update` for another service that chooses the same port in ingress mode responds with a 4xx or 5xx status. The other service is not created or not changed. See [§13](#13-known-divergences--non-goals). | B | `api:mgr` | `none` |
| A Swarm with a service that publishes a port in ingress mode | `POST /services/create` or `POST /services/{id}/update` for another service that chooses the same port in ingress mode responds 400, with a message that contains `is already in use by service`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with a service that publishes a port in ingress mode | `docker service create` or `docker service update --publish-add` that publishes the same port in ingress mode for another service exits non-zero and prints the daemon's error. The command does not create or change the other service. | B | `api:mgr` | `cli` |
| A Swarm with a service that publishes a TCP port | `POST /services/create` for another service that chooses the same port number for UDP responds 201. `GET /services/{id}` then reports each service's port. | B | `api:mgr` | `none` |
| A Swarm with a service that publishes a port in one publish mode | `POST /services/create` or `POST /services/{id}/update` for another service that chooses the same port in the other mode responds with a 4xx or 5xx status. This holds for either mode of the first service. The other service is not created or not changed. See [§13](#13-known-divergences--non-goals). | B | `api:mgr` | `none` |
| A Swarm with a service that publishes a port in one publish mode | `POST /services/create` or `POST /services/{id}/update` for another service that chooses the same port in the other mode responds 400, with a message that contains `is already in use by service`. This holds for either mode of the first service. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Swarm with a service that publishes a port in one publish mode | `docker service create` or `docker service update --publish-add` that publishes the same port in the other mode for another service exits non-zero and prints the daemon's error. The command does not create or change the other service. | B | `api:mgr` | `cli` |
| A Swarm with a service that publishes a port in ingress mode | On every node, `POST /containers/create` for a container that publishes the same port responds 201. | C | `api:mgr`, `api:wkr` | `none` |
| A Swarm with a service that publishes a port in ingress mode, and a created or stopped container on any of its nodes that publishes the same port | `POST /containers/{id}/start` for the container responds with a 4xx or 5xx status. | B | `api:mgr`, `api:wkr` | `none` |
| A Swarm with a service that publishes a port in ingress mode, and a created or stopped container on any of its nodes that publishes the same port | `POST /containers/{id}/start` for the container responds 500, with a message that contains `port is already allocated`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr`, `api:wkr` | `none` |
| A Swarm with a service that publishes a port in ingress mode | On every node, `docker run -d -p` with the same port exits non-zero and prints the daemon's error. | B | `api:mgr`, `api:wkr` | `cli` |
| A Linux node where a port is in use by a process or container | The process or container that uses the port keeps serving it after a service is created or updated to publish the port in ingress mode. | B | `client` | `os` |
| A node where a port is in use by a process or container | Once a service is created or updated to publish that port in ingress mode, and no task from an earlier version of the service's spec remains, a client that connects to the node's address on any of the service's published ports does not reach the service's tasks. See [§13](#13-known-divergences--non-goals). | C | `client` | `os` |
| A node where a port is in use by a process or container, and a service that publishes other ports in ingress mode | `POST /services/{id}/update` adds the port in use to the service in ingress mode. Until the tasks from before the update are gone, a client reaches them at the node's address on the service's earlier ports. | C | `client` | `os` |

### 7.3 `mode=host`

The rows in this subsection hold with `userland-proxy` true and with `userland-proxy` false,
unless a row names a value.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A `mode=host` published port is reachable only at the addresses of nodes that run a task of its service. | B | `client` | `os` |
| | An IPv4 client reaches the task through the published port on the task's node. | B | `client` | `os` |
| | A task observes the real source address of an IPv4 client that connects through the published port on the task's node. | B | `task` | `os` |
| A daemon with `userland-proxy` true | The task does not observe an IPv6 client's real source address. | C | `task` | `none` |
| | A connection to a published port on a node is delivered to the task on that node that published the port. | B | `client` | `os` |
| | `POST /services/create` for a service in `dnsrr` endpoint mode that publishes a port in `mode=host` responds 201. `GET /tasks/{id}` for a running task of the service then reports the port in `Status.PortStatus`. | B | `api:mgr` | `none` |
| A running task of a service with a `mode=host` port that does not set `PublishedPort` | `GET /tasks/{id}` for that task reports its assigned host port in `Status.PortStatus`. | B | `api:mgr` | `none` |
| A running task of a service with a `mode=host` port that does not set `PublishedPort` | On that task's node, `GET /containers/json` reports in the `Ports` of the task's container the host port that `GET /tasks/{id}` reports in `Status.PortStatus`. | B | `api:mgr`, `api:wkr` (on the task's node) | `none` |
| A running task of a service with a `mode=host` port that does not set `PublishedPort` | `docker service ps` on a manager prints the task's assigned host port. | B | `api:mgr` | `cli` |
| A running task of a service with a `mode=host` port that does not set `PublishedPort` | `docker container ls` on the task's node prints the task's assigned host port. | B | `api:mgr`, `api:wkr` (on the task's node) | `cli` |
| A Swarm with a service that publishes a port in `mode=host` | `POST /services/create` for another service that publishes the same port in `mode=host` responds 201. `GET /services` then lists both services. | B | `api:mgr` | `none` |
| | Tasks that publish the same chosen `mode=host` port ([§7.2.1](#721-port-allocation)) are never placed on the same node, whether they belong to one service or several. | B | `api:mgr` | `none` |
| A Swarm in which each node runs a task that publishes a chosen `mode=host` port ([§7.2.1](#721-port-allocation)) | A new task that publishes the same port in `mode=host` stays in the `pending` state that `GET /tasks/{id}` reports in `Status.State`. | B | `api:mgr` | `none` |
| | `POST /services/create` for a service that publishes a `mode=host` port with a `TargetPort` above 65535 responds 201. The service's tasks run, and `GET /tasks/{id}` reports no port in `Status.PortStatus`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |

## 8. Egress & the gateway network

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A container on a non-internal overlay has egress to the physical network. | B | `task` or `ctr` → `underlay` | `os` |
| A container attached only to overlay networks, at least one of them not `internal` | For the container's egress traffic, the source address that an underlay host observes is the node's address, not the container's. | B | `underlay` | `os` |
| | A container attached only to `internal` overlays does not have egress. | B | `task`, `ctr` | `os` |
| | A container on both an internal and a non-internal overlay has egress. Its default route is not on the internal overlay. | B | `task`, `ctr` | `os` |

### 8.1 Gateway network

Each node has a gateway bridge network. Containers on the node's non-internal overlays reach the
physical network through it. This subsection holds on Linux nodes.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | Within the leave-and-rejoin ceiling ([§11.2](#112-convergence)) after `POST /swarm/init` responds 200 on a node, `GET /networks` on the node lists a gateway bridge network. | C | `api:mgr` | `none` |
| A Swarm with an ingress network | Within the leave-and-rejoin ceiling ([§11.2](#112-convergence)) after `POST /swarm/join` responds 200 on a node, `GET /networks` on that node lists a gateway bridge network. | C | `api:mgr`, `api:wkr` | `none` |
| A node that joined a Swarm without an ingress network, and has not yet started a container on a non-internal overlay | `GET /networks` on the node does not list a gateway bridge network. | C | `api:mgr`, `api:wkr` | `none` |
| A node that joined a Swarm without an ingress network | Once a container on a non-internal overlay has started on the node, `GET /networks` on the node lists a gateway bridge network. | C | `api:active` | `none` |
| | A node's gateway bridge network has the name `docker_gwbridge`. | B | `api:mgr`, `api:wkr` | `none` |
| | The bridge interface of a node's gateway bridge network has the name `docker_gwbridge`. | B | `node` | `none` |
| A node where the operator created `docker_gwbridge` before its first container on a non-internal overlay, and before it joined a Swarm with an ingress network | After containers on non-internal overlays start on that node, `GET /networks/docker_gwbridge` there reports the operator's subnet and options. | B | `api:active` | `none` |
| A node where the operator created `docker_gwbridge` before its first container on a non-internal overlay, and before it joined a Swarm with an ingress network | The containers on non-internal overlays that then start on that node have gateway-network addresses in the operator's subnet. | B | `task`, `ctr` | `os` |
| A gateway network that the operator did not create | `GET /networks/{id}` for that network reports `com.docker.network.bridge.enable_icc` as `false` in `Options`. | C | `api:mgr`, `api:wkr` | `none` |
| A gateway network that the operator did not create | A container's interface on that network does not have an IPv6 address. | C | `task`, `ctr` | `os` |
| A container with an address on a node's gateway network, and an underlay host with a route to that address via the node's underlay address | Traffic from that host to that address does not reach the container on a port that the container does not publish. | B | `underlay` → `task` or `ctr` | `os` |

## 9. Encryption

Encrypted overlay networks are implemented on Linux nodes only
([§13](#13-known-divergences--non-goals)). This section holds on Linux nodes, except where a row
names Windows. The rows about the traffic that an underlay host observes name `os` because they
observe the data path. The missing-XFRM rows name `none` because the daemon alone decides their
verdict once XFRM is missing.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | No cleartext VXLAN traffic is observable between nodes on the underlay network for the VNIs of `encrypted` overlay networks. | B | `underlay` | `os` |
| | Traffic between nodes on the underlay network for the VNIs of `encrypted` overlay networks is observable as IPsec ESP. | C | `underlay` | `os` |
| | ESP is in transport mode and protects the VXLAN datagrams. | C | `underlay` | `os` |
| | An encrypted network's IPsec security associations use `rfc4106(gcm(aes))` with a 64-bit ICV. See [Host requirements](#host-requirements). | C | `node` | `none` |
| | A forged cleartext VXLAN datagram for the VNI of an `encrypted` overlay network, injected from the underlay, does not reach the network's tasks. | B | `underlay` → `task` | `os` |
| | Cleartext VXLAN for a cleartext network's VNI passes between nodes, whether or not those nodes also have `encrypted` networks. | B | `underlay` | `os` |
| Two nodes on which an encrypted network was programmed | After that network is deleted, a cleartext network that is allocated the same VNI passes cleartext VXLAN between those nodes. | B | `underlay` | `os` |
| A node where an encrypted network is programmed | The node's daemon is killed. Then that network is deleted from the Swarm, and the node's daemon is started. After these events, a cleartext network that is allocated the same VNI passes traffic to and from the node. | B | `task` | `os` |
| A cleartext network that is allocated the VNI of a deleted encrypted network | Containers on Windows nodes on that network can exchange traffic with containers on the Linux nodes that had containers on the encrypted network. | B | `task`, `ctr` | `os` |
| | Across two consecutive rotations of the network encryption keys, a UDP flow at a fixed rate between tasks on different nodes of an `encrypted` network has no gap in delivery longer than 1 second. This holds without any action by the operator. | B | `task`, `node` | `os` |
| A node that does not have the network encryption keys | On that node, a task attached to an `encrypted` network is rejected. | B | `api:mgr` | `none` |
| A node that does not have the network encryption keys | On that node, the error that `GET /tasks` reports for a rejected task on an `encrypted` network contains `cannot join secure network: encryption keys not present`. | C | `api:mgr` | `none` |
| A Linux host whose kernel does not offer the XFRM netlink interface | On that host, a task attached to an `encrypted` network is rejected. | B | `api:mgr` | `none` |
| A Linux host whose kernel does not offer the XFRM netlink interface | On that host, the error that `GET /tasks` reports for a rejected task on an `encrypted` network contains `cannot join secure network: required modules to install IPSEC rules are missing on host`. | C | `api:mgr` | `none` |
| A Linux host whose kernel does not offer the XFRM netlink interface | On that host, `POST /containers/{id}/start` for a created or stopped container on an attachable `encrypted` network responds with a 4xx or 5xx status. | B | `api:active` | `none` |
| A Linux host whose kernel does not offer the XFRM netlink interface | On that host, `POST /containers/{id}/start` for a created or stopped container on an attachable `encrypted` network responds 500 after 20 seconds, with `attaching to network failed, make sure your network options are correct and check manager logs: context deadline exceeded`. See [§13](#13-known-divergences--non-goals). | C | `api:active` | `none` |
| A Linux host whose kernel does not offer the XFRM netlink interface | On that host, `docker run -d --network` naming an attachable `encrypted` network exits non-zero. | B | `api:active` | `cli` |
| A Linux host whose kernel does not offer the XFRM netlink interface, and a created container without auto-remove (`--rm`) on an attachable `encrypted` network | After `POST /containers/{id}/start` for the container fails on that host, the container stays in the created state. | B | `api:active` | `none` |
| A Linux host whose kernel does not offer the XFRM netlink interface, and a created container with auto-remove (`--rm`) on an attachable `encrypted` network | After `POST /containers/{id}/start` for the container fails on that host, the container is removed. | B | `api:active` | `none` |
| A Linux host whose kernel offers the XFRM netlink interface but not `rfc4106(gcm(aes))` | On that host, a task attached to an `encrypted` network starts. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| A Linux host whose kernel offers the XFRM netlink interface but not `rfc4106(gcm(aes))` | The tasks on that host do not exchange traffic over an `encrypted` network with tasks on other nodes. See [§13](#13-known-divergences--non-goals). | C | `task` | `none` |
| A Linux host whose kernel offers the XFRM netlink interface but not `rfc4106(gcm(aes))` | The daemon log of that host contains a warning-level entry that contains `Failed Adding` for each IPsec security association that it fails to add. See [§13](#13-known-divergences--non-goals). | C | `log` | `none` |

## 10. Wire format & interop

Everything here is hard contract. See [Interop premise](#interop-premise).

This section groups the rows by the kind of overlay network that they apply to. Some rows are about
what an underlay host sees without the network's keys. Those rows apply only to cleartext networks.
The reason is that the VXLAN datagram of an encrypted network is not visible on the wire.

### 10.1 All overlay networks

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A container is reachable from other nodes whether its IP address and its MAC address are each assigned manually or automatically. | B | `task`, `ctr` | `os` |
| A Swarm whose managers run the release under test, and whose workers mix nodes of the previous release and the release under test | In that Swarm, the rows of [§4](#4-east-west-reachability), [§5](#5-service-discovery), [§6](#6-load-balancing-east-west-vip), [§7.2](#72-modeingress) and [§9](#9-encryption) about traffic between nodes hold between nodes of different versions. So do the rows of [§5](#5-service-discovery) about a name whose workload is on another node. Each of those rows keeps its own tier and dependencies. They hold on cleartext and `encrypted` overlays and on the ingress network. | B | `client`, `task`, `underlay` | `os` |
| A Linux node whose kernel does not offer VXLAN devices | On that node, a task attached to an overlay network is rejected. The error that `GET /tasks` reports for the task contains `error creating vxlan interface`. See [Host requirements](#host-requirements). | C | `api:mgr` | `none` |

### 10.2 Cleartext overlay networks

[§1](#1-cluster--node-configuration) pins the default data-path port. [§2.3](#23-overlay-networks)
pins the range of auto-allocated VNIs.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | UDP datagrams carry overlay traffic between hosts, over IPv4 or IPv6, to the configured data-path port. | B | `underlay` | `os` |
| | A UDP datagram that carries overlay traffic between hosts has a VXLAN header. The header has the VNI of the overlay subnet that the frame in the datagram is on. | B | `underlay` | `os` |
| | The VXLAN payload of a UDP datagram that carries overlay traffic between hosts is an Ethernet frame that carries container-to-container traffic. | B | `underlay` | `os` |

### 10.3 Encrypted overlay networks

Encrypted traffic between hosts is IPsec ESP. [§9](#9-encryption) covers the encryption and the rest
of what an encrypted network promises. The rows below are about the decrypted payload. The
decrypted payload is the same whatever the encryption is. To see the payload, an underlay host must
decrypt it with a node's keys. This subsection holds on Linux nodes, as [§9](#9-encryption) does.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | The decrypted payload is a UDP datagram to the configured data-path port. | B | `underlay` (decrypted with a node's keys) | `os` |
| | The UDP datagram in the decrypted payload has a VXLAN header. The header has the VNI of the overlay subnet that the frame in the datagram is on. | B | `underlay` (decrypted with a node's keys) | `os` |
| | The VXLAN payload of the UDP datagram in the decrypted payload is an Ethernet frame that carries container-to-container traffic. | B | `underlay` (decrypted with a node's keys) | `os` |

## 11. Failure & lifecycle

### 11.1 Restart and reclamation

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A service whose tasks are constrained to the node | After a graceful daemon restart, the service's tasks on the node are reachable at their addresses on their overlay networks within the node leave-and-rejoin ceiling ([§11.2](#112-convergence)). | B | `task` | `os` |
| | After a graceful daemon restart, the node's published ports answer within the node leave-and-rejoin ceiling ([§11.2](#112-convergence)). | B | `client` | `os` |
| | A graceful daemon restart on one node does not interrupt flows between clients and tasks on other nodes: no TCP flow is reset, and no TCP or UDP flow has a gap in delivery longer than 1 second. This holds for flows to a VIP and for flows that enter through a published port on another node. | B | `task`, `client` | `os` |
| A Linux node whose daemon was killed and started again, and a namespace, bridge or VXLAN device that the killed daemon left for an overlay network | Once a task or standalone container on that network starts on the node, no such namespace, bridge or VXLAN device remains on the node. See [§13](#13-known-divergences--non-goals). | B | `node` | `os` |
| A Linux node whose daemon was killed and started again, and a namespace, bridge or VXLAN device that the killed daemon left for an overlay network | Once a task or standalone container starts on the node on another overlay network that has one of that network's VNIs, no such namespace, bridge or VXLAN device remains on the node. See [§13](#13-known-divergences--non-goals). | B | `node` | `os` |
| A Linux node whose daemon was killed and started again, and a namespace that the killed daemon left for an overlay network | When a task or standalone container starts on the node on that network, or on another overlay network that has one of that network's VNIs, the daemon removes the namespace with its bridge and VXLAN device. See [§13](#13-known-divergences--non-goals). | C | `node` | `os` |
| A Windows node whose daemon was killed and started again, and an HNS overlay network that the killed daemon left for an overlay network | Once a task or standalone container on that overlay network starts on the node, the HNS network that the killed daemon left does not remain. See [§13](#13-known-divergences--non-goals). | B | `node` | `os` |
| A Windows node whose daemon was killed and started again, and an HNS overlay network that the killed daemon left for an overlay network | Once a task or standalone container starts on the node on another overlay network with a VNI that overlaps the HNS network's VNIs, the HNS network does not remain. See [§13](#13-known-divergences--non-goals). | B | `node` | `os` |
| A Windows node whose daemon was killed and started again, and an HNS overlay network that the killed daemon left for an overlay network | The HNS network remains until a task or standalone container starts on the node on that overlay network, or on another overlay network with a VNI that overlaps the HNS network's VNIs. After that, the HNS network does not remain. See [§13](#13-known-divergences--non-goals). | C | `node` | `os` |
| A node that used a VNI before its daemon was killed and restarted | A task or container on a network that is allocated that VNI starts on the node, and the network passes traffic. This holds whether the network is the one that used the VNI before the kill or another network. | B | `node`, `task` | `os` |
| A service whose tasks are constrained to the node | After the daemon is killed and restarted, the service's tasks on the node are reachable at their addresses on their overlay networks within the node leave-and-rejoin ceiling ([§11.2](#112-convergence)). | B | `task` | `os` |
| | After the daemon is killed and restarted, the node's published ports answer within the node leave-and-rejoin ceiling ([§11.2](#112-convergence)). | B | `client` | `os` |
| | After `POST /swarm/leave` responds 200 on a node, `GET /networks` on that node does not list any Swarm-scoped network, the ingress network included. The node's standalone containers keep running, and none of them is attached to a Swarm-scoped network. See [§13](#13-known-divergences--non-goals). | B | `api:solo` | `none` |
| | On a worker, `docker swarm leave` exits zero. `docker network ls` on that node then does not list any Swarm-scoped network, the ingress network included. | B | `api:solo` | `cli` |

### 11.2 Convergence

There are five convergence classes, each with a different dominant term. One bound cannot serve all
of them.

Each class has a B ceiling. The ceiling is chosen by judgment to encode "a human would call this
broken". It is never re-baselined. This keeps the ceiling free of flakes. The ceiling still catches
the regression that matters: convergence that goes from seconds to minutes, or to never.

| Class | Dominant term | B ceiling |
| --- | --- | --- |
| Reassignment: an address moving to another endpoint, a task replaced by one on another node, or a new task, service or endpoint | gossip propagation and FDB reprogramming | 300 s |
| Partition heal | reconnection to the failed peers, then full-state syncs | 180 s |
| Node leave and rejoin | full cluster re-formation and network re-programming | 120 s |
| Allocation: a Swarm-scoped network that can be allocated, or the endpoint (VIPs and published ports) of a service whose networks are allocated. Also the leader's failed attempt to allocate a network or a service's endpoint, and what follows from it | the allocator of the leader manager | 30 s |
| Failure detection: the nodes on one side of a partition declare the nodes on the other side failed | gossip probes and suspicion timeouts | 120 s |

Each ceiling starts when the change is complete. The change is complete when the new endpoint or
task is running, when the partition is removed, or when the rejoined node's task is running. For a
rejoined node that runs no task, the change is complete when `POST /swarm/join` responds. For
allocation, the change is complete when `POST /networks/create`, `POST /services/create` or `POST
/services/{id}/update` responds. For any other effect of `POST /services/{id}/update`, the change is
complete when `GET /services/{id}` reports `UpdateStatus.State` as `completed` for that update. An
update of a service's endpoint mode, networks, published ports or aliases replaces its tasks and
sets `UpdateStatus`. An update that does not replace a task is complete when the request responds.
For failure detection, the change is complete when the partition starts. For a removal, the change
is complete when the request that removes the object responds. For a restart of a node's daemon, the
change is complete when the daemon's API answers again and `GET /info` on the node reports
`Swarm.LocalNodeState` as `active`. For a change in a task's health, or the exit of its container,
the change is complete when the task's node emits the container's `health_status` or `die` event.
For a change of a task's desired state away from `running`, the change is complete when `GET /tasks`
first reports the new `DesiredState`. For a change of leader, the change is complete when `GET
/nodes` on a manager reports `ManagerStatus.Leader` as true for the new leader. For a row about what
holds once no container of a service is running, the change is complete when the last of those
containers emits its `die` event. For a row about a removal that reaches a node late, the ceiling
starts when the removal can reach the node.

A node partition divides the nodes of a Swarm into two sides, and blocks all traffic between the
nodes of one side and the nodes of the other, in both directions. The managers that hold a quorum
are on one side. During a node partition, consider each network to which nodes of both sides have a
local attachment. A side has declared the other side's nodes failed once, for each such network, no
node of the side lists a node of the other side in `Peers` in `GET /networks/{id}`.

Unless a row says otherwise, each end of the traffic in the rows below can be a service task or a
standalone container on an `attachable` overlay. This applies in all four combinations. Each row
states the node placements it covers. In the rows about a node partition, an end on the side without
the managers' quorum is a standalone container, because the orchestrator replaces the tasks on that
side.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | After overlay address X moves from endpoint S1 to S2, a client that could reach X at S1 reaches it at S2. The client does this within the reassignment ceiling, without a manual `ip neigh`, `arp -d` or host flush. This holds whether S2 is on S1's node or on another node. It also holds whether the client is on S1's node, S2's node or a third node. | B | `task`, `ctr` | `os` |
| | Overlay address X moves any number of times between endpoints. Within the reassignment ceiling, a client that could reach X at its previous endpoint reaches X at its current endpoint. This also holds on a node that learns of the removal of X's previous endpoint only after it learned of X's new endpoint. That node can be the node of the new endpoint or any other node. | B | `task`, `ctr` | `os` |
| | Overlay address X moves between endpoints. Within the reassignment ceiling, a client that could reach X at its previous endpoint reaches X at its new endpoint. This holds whether or not the MAC address moves with X. | B | `task`, `ctr` | `os` |
| | When a task is replaced by one on another node, a client that could reach the old task reaches the new one. It reaches the new task at the address that `GET /tasks` reports for it, at the address that `tasks.<service>` resolves to, and, in vip mode, through the service's VIP. The client does this within the reassignment ceiling, without manual intervention. This holds whether the client is on the old task's node, the new task's node or a third node. | B | `task`, `ctr` | `os` |
| | During a node partition, each side declares the other side's nodes failed within the failure-detection ceiling. | C | `api:mgr`, `api:wkr` | `none` |
| Linux nodes on one side of a node partition, which have declared the nodes on the other side failed | Those nodes do not hold overlay neighbor or FDB entries for the other side's endpoints. | C | `node` | `os` |
| A node partition in which the side with the managers' quorum has declared the other side's nodes failed, and a service with running tasks on both sides | Every flow that starts on a node of that side and goes to the service's VIP reaches a task on that side. | C | `task`, `ctr` | `os` |
| Linux nodes | A node's VXLAN device does not hold a neighbor or FDB entry for any of the node's own endpoints. | C | `node` | `os` |
| A node partition in which each side has declared the other side's nodes failed | After the partition heals, reachability between the two sides converges within the partition-heal ceiling. | B | `task`, `ctr` | `os` |
| A node partition between Linux nodes, in which each side has declared the other side's nodes failed | Reachability that is restored when the partition heals persists. A connection opened from either side still reaches the other side's containers after they stay silent for longer than the kernel's learned-FDB aging time. That aging time is 300 s by default. | B | `task`, `ctr` | `os` |
| A node partition between Linux nodes, in which each side has declared the other side's nodes failed | Containers on both sides send traffic as the partition heals. The reachability that is restored then persists. A connection opened from either side still reaches the other side's containers after they stay silent for longer than the kernel's learned-FDB aging time. | C | `task`, `ctr` | `os` |
| | A node leaves the Swarm with `POST /swarm/leave` and rejoins with `POST /swarm/join`. Within the leave-and-rejoin ceiling, the tasks placed on the node after the rejoin are reachable at their overlay addresses from that node and from other nodes. Such tasks include a global service's tasks. | B | `task`, `ctr` | `os` |
| | After a node leaves the Swarm with `POST /swarm/leave` and rejoins with `POST /swarm/join`, its published ports answer within the leave-and-rejoin ceiling. | B | `client` | `os` |
| | A node leaves the Swarm with `POST /swarm/leave` and rejoins with `POST /swarm/join`. Within the leave-and-rejoin ceiling, connections to a service's published port on any node reach the service's tasks placed on the rejoined node after the rejoin. An example is a global service's tasks. | B | `client` | `os` |
| A service with a running task on each of two or more nodes | One of those nodes leaves the Swarm with `POST /swarm/leave`. Within the leave-and-rejoin ceiling after the leave, every new flow to the service's VIP from a node still in the Swarm reaches a running task on a node still in the Swarm. So does every new flow to the service's published port on such a node. | B | `task`, `ctr`, `client` | `os` |
| Linux nodes | Within the leave-and-rejoin ceiling after `POST /swarm/leave` responds 200 on a node, the other nodes do not hold overlay neighbor or FDB entries for its endpoints. | C | `node` | `os` |

In the rows of this subsection, a node's overlay neighbor and FDB entries are the permanent entries
that the daemon programs. Entries that the kernel learns from traffic are not included. The B
persistence row holds whether or not containers send traffic as the partition heals. A bug revealed
the persistence row. In that bug, reachability after a heal depended on an entry that the kernel
learned from the peer's traffic. Reachability broke when that entry expired. The fix is in
[moby/moby#53663](https://github.com/moby/moby/pull/53663). The row names `os` because how learned
and permanent FDB entries interact is behavior of the kernel's VXLAN driver.

### 11.3 Churn

This paragraph applies to every row below in which a client reaches or resolves a service. Each such
row holds whether the client is a service task or a standalone container on an `attachable`
overlay. It holds for a client on the same node as one of the service's tasks. It also holds for a
client on a node that runs none of the service's tasks. The service's own end is always its tasks.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A service has a rolling update in which at least one task keeps running throughout. Examples are an update in start-first order and an update of fewer tasks at a time than are running. During the update, the VIP does not stop answering for longer than the allocation ceiling: within any interval of that length, some new flow to the VIP succeeds. See [§13](#13-known-divergences--non-goals). | B | `task`, `ctr` | `os` |
| | A service has a rolling update in which at least one task keeps running throughout. Examples are an update in start-first order and an update of fewer tasks at a time than are running. Once the update completes, every new flow reaches a running task within the reassignment ceiling. See [§13](#13-known-divergences--non-goals). | B | `task`, `ctr` | `os` |
| | A service's replica count is changed at least 10 times, alternating between 1 and 5. Each update is sent as soon as the previous update responds. Once the scaling stops, every new flow to the service's VIP reaches a running task within the reassignment ceiling. See [§13](#13-known-divergences--non-goals). | B | `task`, `ctr` | `os` |
| A Swarm-scoped network | `DELETE /networks/{id}` for the network, by its ID, runs concurrently with `POST /networks/create` for a network with the same name and driver. Afterwards, `DELETE /networks/{id}` for each network with that name that remains responds 204, and `GET /networks` then does not list it. | B | `api:mgr` | `none` |
| A Swarm-scoped network | `DELETE /networks/{id}` for the network, by its ID, runs concurrently with `POST /networks/create` for a network with the same name and driver. Once no network with that name remains, `POST /networks/create` with that name responds 201, and `GET /networks` then lists the network. | B | `api:mgr` | `none` |
| A created or stopped standalone container on a Swarm-scoped attachable network | `DELETE /networks/{id}` on a manager for that network races `POST /containers/{id}/start` for the container. One of two outcomes occurs. In the first, the delete responds with a 4xx or 5xx status, and the container starts attached to the network. In the second, the delete responds 204, `GET /networks` on a manager then does not list the network, and the start fails. | B | `api:mgr`, `api:active` | `none` |
| A created or stopped standalone container on a Swarm-scoped attachable network | `DELETE /networks/{id}` on a manager for that network races `POST /containers/{id}/start` for the container. When the delete responds 204, the start responds with a 4xx or 5xx status. | B | `api:mgr`, `api:active` | `none` |
| A created or stopped standalone container on a Swarm-scoped attachable network | `DELETE /networks/{id}` on a manager for that network races `POST /containers/{id}/start` for the container. When the delete responds 204, the start gives one of two responses. The first is 500 at once, with a message that contains `network <ref> not found`. `<ref>` is the network's full ID if the container has been started before. Otherwise it is the network name or ID that the container was created with. The second is 500 after 20 seconds, with `attaching to network failed, make sure your network options are correct and check manager logs: context deadline exceeded`. See [§13](#13-known-divergences--non-goals). | C | `api:mgr`, `api:active` | `none` |
| A node with exactly one task or container on a network | `DELETE /networks/{id}` on a manager for that network races the shutdown of that task or container. For a task, the shutdown comes from the removal of its service, or from an update that removes the network from the service's `TaskTemplate.Networks`. For a container, it comes from `POST /containers/{id}/stop`. One of two outcomes occurs. In the first, the delete responds with a 4xx or 5xx status, and the network stays listed. In the second, the delete responds 204, and once the shutdown completes, `GET /networks` on that node does not list the network. | B | `api:mgr`, `api:active` | `none` |
| | When `docker network rm` on a manager races `docker run -d --network` naming the network, one of two outcomes occurs. In the first, `docker network rm` exits non-zero, and the container runs attached to the network. In the second, `docker network rm` exits zero, `docker network ls` on a manager then does not list the network, and `docker run -d` exits non-zero. | B | `api:mgr`, `api:active` | `cli` |
| | After `POST /services/{id}/update` changes a service's endpoint mode from `vip` to `dnsrr`, `<service>` resolves to the addresses of the running tasks. | B | `task`, `ctr` | `none` |
| | After a service's endpoint mode changes from `vip` to `dnsrr`, its old VIP does not reach the service. | B | `task`, `ctr` | `os` |
| | After `POST /services/{id}/update` changes a service's endpoint mode from `dnsrr` to `vip`, `<service>` resolves to the service VIP. | B | `task`, `ctr` | `none` |
| | After a service's endpoint mode changes from `dnsrr` to `vip`, the service's VIP load-balances across the service's tasks. | B | `task`, `ctr` | `os` |
| A service, and an overlay network that the service is not attached to | After `POST /services/{id}/update` adds the network to `TaskTemplate.Networks`, the service is reachable on that network. | B | `task`, `ctr` | `os` |
| A service attached to a Swarm-scoped network | After `POST /services/{id}/update` removes the network from `TaskTemplate.Networks`, the service is not reachable on that network. | B | `task`, `ctr` | `os` |
| A service attached to a Swarm-scoped network | After `POST /services/{id}/update` removes the network from `TaskTemplate.Networks`, `GET /services/{id}` does not report a VIP on that network. | B | `api:mgr` | `none` |
| A service, and a Swarm-scoped network that the service is not attached to | `docker service update --network-add` naming that network, for the service, exits zero. `docker service inspect` then prints the network in `TaskTemplate.Networks`. | B | `api:mgr` | `cli` |
| A service attached to a Swarm-scoped network | `docker service update --network-rm` naming that network, for the service, exits zero. `docker service inspect` then does not print the network in `TaskTemplate.Networks`. | B | `api:mgr` | `cli` |

### 11.4 Scale and exhaustion

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | After at least 10 cycles in which an overlay network is created and deleted, `POST /networks/create` for an overlay network, with a `vxlanid_list` that has a VNI that a deleted network used, responds 201. `GET /networks/{id}` on a manager then reports that VNI in `Options`. | B | `api:mgr` | `none` |
| A Swarm with a service that publishes a port in ingress mode | After the service is removed, `POST /services/create` for another service that publishes the same port responds 201. `GET /services/{id}` then reports that port. | B | `api:mgr` | `none` |
| | An overlay network is created without `vxlanid_list`. The VNIs allocated to the network differ from the VNIs in the `vxlanid_list` of each other overlay network that exists when the network is created. | B | `api:mgr` | `none` |
| | At least 100 overlay networks, each with a task on each of two nodes, all pass traffic between the nodes. | B | `task` | `os` |
| | `POST /networks/create` for an overlay network that cannot be allocated responds 201. An example is a network whose address pool is exhausted. `GET /networks` on a manager then lists the network. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| | `docker network create` for an overlay network that cannot be allocated exits zero, and `docker network ls` on a manager then lists the network. | C | `api:mgr` | `cli` |
| An overlay network that cannot be allocated | The daemon log of the leader manager reports the failure at error level, in an entry that contains `Failed allocation for network` and the network's ID. On a manager that just became leader, the entry can contain `failed allocating network` and the network's ID in place of that text. | C | `log` | `none` |
| An overlay network that cannot be allocated | On a manager, `GET /networks/{id}` for that network responds 200 with an empty `Driver`, `IPAM` and `Options`, and without `Status`. This is so even when `POST /networks/create` named a driver or a subnet. The response does not report the failure in any field. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| An attachable overlay network that cannot be allocated, and a created or stopped container on that network, on any node | `POST /containers/{id}/start` for the container responds 500 after 20 seconds with `attaching to network failed, make sure your network options are correct and check manager logs: context deadline exceeded`. See [§13](#13-known-divergences--non-goals). | C | `api:active` | `none` |
| An attachable overlay network that cannot be allocated | On any node, `docker run -d --network` naming that network exits non-zero and prints the daemon's error. | B | `api:active` | `cli` |
| An overlay network that cannot be allocated | `POST /services/create` for a service on that network responds 201. The service's tasks stay in the `new` state, with desired state `running`, and do not start. See [§13](#13-known-divergences--non-goals). | C | `api:mgr` | `none` |
| An overlay network that cannot be allocated | After `POST /services/create` for a service on that network responds 201, the daemon log of the leader manager reports the failure at error level. The entry contains `Failed allocation for service` and the service's ID. See [§13](#13-known-divergences--non-goals). | C | `log` | `none` |
| A Swarm with a service that publishes a port | After a new service publishes the same port number under a different protocol, both services are reachable on their published ports. | B | `client` | `os` |
| A Swarm with two services that publish the same port number under different protocols | When one of the services is removed, the other service stays reachable on its published port. This holds throughout the removal and after it. | B | `client` | `os` |

### 11.5 Firewall-backend transition

This subsection is empty until moby/moby#53125. A switch of a node in a Swarm to or from the
nftables backend is not asserted ([§13](#13-known-divergences--non-goals)).

### 11.6 Failure reporting

When a node cannot program cluster-scoped configuration, no API surface reports it. Examples of
such configuration are encryption parameters for a peer, a load-balancer backend, a firewall rule
and an FDB entry. The affected service continues to look healthy from `api:mgr` while that node's
data plane is wrong. The daemon log is the only channel that reports the failure. This makes the
daemon log part of the observable contract.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | The daemon log of a node reports a node-local failure to program a load-balancer service or backend at error level. See [§13](#13-known-divergences--non-goals). | B | `log` | `none` |
| Linux nodes | A daemon log entry for a node-local failure to program a load-balancer service or backend contains `Failed to create an ipvs handle`, `Failed to create a new service for vip` or `Failed to create real server`. | C | `log` | `none` |
| Windows nodes | A daemon log entry for a node-local failure to program a load-balancer service or backend contains `Failed to add ILB policy` or `Failed to add ELB policy`. | C | `log` | `none` |
| Linux nodes | The daemon log of a node reports a node-local failure to publish an ingress port at error level. | B | `log` | `none` |
| A Linux node where a port is in use by a process or container, and a service that publishes that port in ingress mode | The daemon log of the node reports the node-local failure to publish that port at error level. The entry contains the port number. | B | `log` | `none` |
| Linux nodes | The daemon log entry for a node-local failure to publish an ingress port contains `Failed to add ingress`. | C | `log` | `none` |
| Linux nodes | The daemon log of a node reports a node-local failure to program an overlay peer's FDB or neighbor entry at warning level. The entry contains the network's ID, the address of the peer's endpoint, and the data-path address of the peer's node. | B | `log` | `none` |
| Linux nodes | The daemon log entry for a node-local failure to program an overlay peer's FDB or neighbor entry contains `overlay: failed to add peer entry`. Its `nid` field is the network's full ID. | C | `log` | `none` |
| Linux nodes | The daemon log of a node reports a node-local failure to program an IPsec security association or policy for a peer. The entry is at warning level. For a security association, it contains `Failed Adding fSA{` or `Failed Adding rSA{`. For a policy, it contains `Adding fSP{`. See [§13](#13-known-divergences--non-goals). | C | `log` | `none` |
| A node where a node-local failure to program the load-balancer service or a backend for a service occurred, and its cause still holds | A task is then added to the service. If the failure occurs again, the daemon log of the node reports it again. The report is not suppressed after the first one. | B | `log` | `none` |
| A node where a node-local failure to program the load-balancer service or a backend for a service occurred | The cause of the failure clears. A task that is then added to the service receives new flows to the service that start on that node. This holds when the failure was to program the load-balancer service, and when it was to program a backend. See [§13](#13-known-divergences--non-goals). | B | `task`, `client` | `none` |
| A node where a node-local failure to program the load-balancer service or a backend for a service occurred | The cause of the failure clears. A task is then added to the service. The daemon log of that node does not report a failure for that task. | B | `log` | `none` |
| A node that cannot program cluster-scoped configuration for a service | `GET /tasks` reports the node's tasks for the service as `running`. | C | `api:mgr` | `none` |
| A node that cannot program cluster-scoped configuration for a service | `GET /services?status=true` at API version 1.41 or later reports the service's `RunningTasks` equal to its `DesiredTasks`. | C | `api:mgr` | `none` |
| | `docker service ps --no-trunc` for a service lists the service's tasks, with their states and errors. | B | `api:mgr` | `cli` |
| A node that cannot program cluster-scoped configuration for a service | `docker service ls` prints the service's replicas as all running. | C | `api:mgr` | `cli` |

[§13](#13-known-divergences--non-goals) lists four gaps in this reporting and recovery as known
divergences:

- An unchanged condition is not reported again.
- Recovery is not reported at all.
- None of the entries names the service.
- Recovery is incomplete.

## 12. Firewall anchors & host integration

The content of Moby's firewall rules is not an interface. A backend change replaces exactly that
content. [bridge-networking.md §1.5](bridge-networking.md#15-rule-content) pins the bridge driver's
rules at tier C only, the rules for Swarm ingress ports included. This section covers the overlay
driver's own firewall state. It also covers how Swarm traffic meets the bridge driver's firewall
anchors. [bridge-networking.md](bridge-networking.md) specifies those anchors, `DOCKER-USER` among
them. This section holds on Linux nodes.

### 12.1 nftables

This subsection is empty until moby/moby#53125. The overlay driver's nftables table on a node in a
Swarm is not asserted ([§13](#13-known-divergences--non-goals)).

### 12.2 iptables

The rows in this subsection hold under the iptables backend.
[bridge-networking.md](bridge-networking.md#12-docker-user) specifies the `DOCKER-USER` chain
itself. Swarm ingress ports do not have a chain of their own. The bridge driver's anchors also cover
them because the bridge driver publishes them like any other published port.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | Traffic to and from a Swarm published port, in ingress mode and `mode=host`, is evaluated by an operator's `DOCKER-USER` rules. No rule that the daemon programs accepts that traffic before the `DOCKER-USER` chain. | B | `client` | `os` |
| A node where an encrypted network is programmed, and an operator's rule at the end of the filter table's `INPUT` chain that accepts incoming VXLAN traffic | Cleartext VXLAN for that network's VNI that arrives at the node does not reach the network's tasks or containers. | B | `underlay` → `task` or `ctr` | `os` |

### 12.3 Cross-generation cleanup

This subsection covers the upgrade path that a packager exercises. A node that runs release *N−1* is
upgraded to *N*, possibly with a different firewall backend. A generation is the firewall state that
one release creates under one firewall backend. A previous generation is such state that a daemon
left on the node before the current start of the daemon.
[bridge-networking.md](bridge-networking.md#13-cross-generation-cleanup) gives the daemon-wide
expectations and the reason why cleanup depends on names. The rows here cover state that belongs to
Swarm and the overlay driver.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A node with the state that a daemon of release 28.0.0 or earlier left with the iptables backend: a `DOCKER-INGRESS` chain in the filter and nat tables, jumps to it from the nat table's `PREROUTING` and `OUTPUT` chains for packets with a local destination address, a jump to it from the filter table's `FORWARD` chain, and a jump from the filter table's `FORWARD` chain to `DOCKER-ISOLATION-STAGE-1`. The row holds whatever created that state. | After the daemon starts, the filter and nat tables do not contain that chain. | B | `node` | `os` |
| A node with a `DOCKER-INGRESS` chain in the filter or nat table | After the daemon starts with the iptables backend, the filter and nat tables do not contain that chain. | C | `node` | `os` |
| A node with a `DOCKER-INGRESS` chain in the filter or nat table, and a jump from the filter table's `FORWARD` chain to `DOCKER-FORWARD` or `DOCKER-ISOLATION-STAGE-1` | After the daemon first starts after a switch to nftables, the filter and nat tables do not contain that chain. | C | `node` | `os` |
| | The name that cleanup uses for the `DOCKER-INGRESS` chain does not change. Cleanup searches the filter and nat tables for that chain. | C | `node` | `none` |

## 13. Known divergences & non-goals

Each row gives one of two things: a current behavior that differs from what a user might expect, or
a capability that Moby does not provide. A change to the behavior in any row is a behavior change.
Make such a change only on purpose.

| Topic | Current behavior | Depends on |
| --- | --- | --- |
| IPv6 addressing inside the overlay | Not supported. With the default IPAM driver, `POST /networks/create` accepts an overlay network with an IPv6 subnet, but the network is never allocated. An overlay network with `EnableIPv6` set and without an IPv6 subnet is accepted, and the network's attachments get IPv4 addresses only. Each attachment gets exactly one address. The service mesh is IPv4-only. | `none` |
| Outer source address of VXLAN traffic | Not guaranteed. The outer source address is the address that the node's route to each peer selects. It is meant to be the node's data-path address. When that route selects a different address, peers drop all of the node's overlay traffic. Neither node's daemon log has an error ([§1](#1-cluster--node-configuration), [moby/moby#53004](https://github.com/moby/moby/issues/53004)). | `none` |
| VXLAN on cleartext overlays | Not authenticated. A Linux node delivers VXLAN for a cleartext network's VNI from any host that can reach the node's data-path port. The frames reach the network's containers. For an encrypted network, the node drops VXLAN for the network's VNI that did not arrive under IPsec ([§9](#9-encryption)). | `os` |
| Overlay namespaces after a killed daemon | Not reclaimed at start. On Linux, the daemon is killed and then started again. Take an overlay network that is not programmed on the node again after the start. Its namespace stays in place. The namespace's bridge, VXLAN device and bind mount under the exec root also stay. They stay until the host reboots, or until another network that uses one of that network's VNIs is programmed on the node ([§11.1](#111-restart-and-reclamation)). | `none` |
| Encrypted overlay on Windows | Not implemented. Traffic between Windows nodes on an `encrypted` overlay is not encrypted. Traffic between Windows nodes and Linux nodes on such an overlay does not pass because the Linux peers drop cleartext VXLAN for the network's VNI. A Windows node's endpoint MTU on an `encrypted` network is the same as on a cleartext network ([§9](#9-encryption), [§10.3](#103-encrypted-overlay-networks)). | `none` |
| Data-path address on Windows | Not applied. A Windows node's tunnel endpoint is the address of its network adapter, whatever `DataPathAddr` or the advertise address says. | `none` |
| Data-path port on Windows | Not applied. Windows nodes do not use the cluster's data-path port. In a cluster with a non-default data-path port, Windows nodes cannot exchange overlay traffic with Linux nodes. | `none` |
| IPv6 data path on Windows | Not supported. Windows nodes cannot exchange overlay traffic with Linux nodes over an IPv6 underlay. | `none` |
| Interface names as `DataPathAddr` or `AdvertiseAddr` | Resolved one time. `POST /swarm/init` and `POST /swarm/join` replace an interface name with the interface's address when the request is made. The node keeps that address after the interface's address changes. It also keeps it after the daemon restarts, even when no interface on the node has that address ([§1](#1-cluster--node-configuration)). | `none` |
| Mixed IPv4 and IPv6 data-path addresses | Not refused. A node can join a Swarm with a data-path address of a different IP version from the data-path addresses of the other nodes. Overlay traffic between two nodes whose data-path addresses have different IP versions fails. Only the daemon log of each node reports the failure to program the other node as a peer ([§1](#1-cluster--node-configuration), [§11.6](#116-failure-reporting)). | `none` |
| `mtu` option on Windows | Ignored. On a network with `com.docker.network.driver.mtu` set, Windows nodes do not report the inner MTU that Linux nodes report. Traffic between Windows nodes and Linux nodes on that network can be blackholed ([§3](#3-observable-state-inside-a-container)). | `none` |
| Egress from `internal` overlays on Windows | Not prevented. A Windows container attached only to `internal` overlays has egress ([§8](#8-egress--the-gateway-network)). | `none` |
| Egress with both `internal` and non-internal overlays on Windows | Not guaranteed to leave through the non-internal network. A Windows container attached to an `internal` overlay and to a non-internal overlay has a gateway on each network. Its egress can leave through either network ([§8](#8-egress--the-gateway-network)). | `none` |
| SCTP on Windows | Not supported. Windows does not have SCTP. SCTP does not pass to or from containers on Windows nodes, directly or through a VIP ([§4](#4-east-west-reachability), [§6](#6-load-balancing-east-west-vip)). A task on a Windows node that publishes an SCTP port in `mode=host` fails with an error that contains `SCTP is unsupported on windows`. On a Windows node, a port published as SCTP in `mode=ingress` is published as TCP. That port then collides with a TCP port of the same number ([§7](#7-published-ports), [§11.4](#114-scale-and-exhaustion)). | `os` |
| Connections from a task to its own service's VIP in DSR mode | Not load-balanced. On Linux nodes, in DSR mode, each task of a service has the service's VIP as a local address. A connection from a task to the VIP of its own service reaches that task. The other tasks of the service do not get these connections. The task gets them also while it does not yet report healthy, and while it shuts down. Connections from other clients are load-balanced ([§6](#6-load-balancing-east-west-vip)). | `none` |
| VIP on an overlay network with more than one subnet | Not load-balanced across subnets. On Linux nodes, in NAT mode and in DSR mode, a backend task outside the subnet of the client node's load-balancer endpoint does not answer traffic to the VIP. A VIP outside that subnet does not reach any backend. The tasks' own addresses stay reachable across subnets ([§6](#6-load-balancing-east-west-vip)). | `none` |
| `dsr` option on Windows | Ignored. Windows nodes load-balance VIP traffic in NAT mode, whatever the network's `dsr` option says. A task on a Windows node does not observe the client's real source address, and does not observe the VIP ([§6](#6-load-balancing-east-west-vip)). | `none` |
| Isolation between tasks on the ingress network on Windows | Not enforced. Another task can reach a task on a Windows node through the ingress-network address of the task on the Windows node ([§7.2](#72-modeingress)). | `none` |
| Egress through the ingress link on Windows | Not prevented. A task on a Windows node can reach the outside world through its ingress-network link ([§7.2](#72-modeingress)). | `none` |
| Orphaned HNS networks on Windows | Not reclaimed. Every HNS overlay network that is on the node when the daemon starts stays in place. One of these networks is deleted only when a network with the same ID, or with an overlapping VNI, is programmed on the node. Thus the HNS network of a Swarm network that the node stopped using while the daemon was not running stays in place ([§11.1](#111-restart-and-reclamation)). The HNS endpoints that a killed daemon left on these networks also stay in place. | `none` |
| Resolver network preference on Windows | Not applied. Each network of a Windows container has its own embedded resolver. That resolver answers only from the records of its network. The network whose answer the container gets depends on which adapter's resolver the Windows DNS client asks ([§5](#5-service-discovery)). | `os` |
| Ingress port reservation on Windows | Not reserved. On Windows nodes, an ingress port is an HNS load-balancer policy, and its host port is not reserved. A `docker run -p` on a port that a service publishes is not refused. On a Windows node, a port is also published in ingress mode when it is already in use ([§7.2.1](#721-port-allocation)). | `none` |
| Rootless mode | Not supported, and nothing refuses it. `docker swarm init` and `docker network create --driver overlay` succeed. A service in the `vip` endpoint mode on a user-defined overlay network reports its tasks as running, but its VIP refuses connections. The daemon cannot program IPVS from a user namespace. A service that publishes a port through the routing mesh reports its task as running, but the port does not answer. A node cannot join a Swarm whose manager runs rootless. The node's connection to the manager is refused. | `os` |
| Network allocation failures are not reported to the caller | `POST /networks/create` for an overlay network that cannot be allocated responds 201. This occurs when a VNI in the network's `vxlanid_list` is in use, or when its address pool is exhausted. It also occurs when its requested subnet overlaps the subnet of another Swarm network. It also occurs when it has an IPv6 subnet under the default IPAM driver. Networks are allocated asynchronously. When a network cannot be allocated, the failure appears in the leader manager's log. Allocation is tried again later, for example after another network, service or task is removed. Nothing tells the caller about the failure. `POST /services/create` for a service on such a network also responds 201. The service's tasks never start, and nothing tells the caller why. On a manager, `GET /networks/{id}` reports the network with an empty `Driver` and `IPAM`, even when the request named them ([§2.3](#23-overlay-networks), [§11.4](#114-scale-and-exhaustion)). | `none` |
| Network status by full ID on a manager | Node-local. Take a manager that has a local attachment to an overlay network. On that manager, `GET /networks/{id}` with the network's full ID reports only that node's own endpoints in `Status.IPAM`. With the network's name or a partial ID, it reports the manager's allocation for the whole network ([§2.3](#23-overlay-networks)). | `none` |
| `dangling` filter for Swarm-scoped networks | Wrong. On a manager, `GET /networks` with the filter `dangling=true` lists every Swarm-scoped network, in use or not. A Swarm-scoped network with a local attachment is listed for `dangling=false` too. A network that services or containers use is meant to be listed only for `dangling=false` ([§2.2](#22-swarm-scoped-networks)). | `none` |
| Published-port conflicts accepted in a race | Because of a race condition, two or more services with colliding published ports may be accepted in error ([§7.2.1](#721-port-allocation)). | `none` |
| Auto-assigned published ports across an update | Reassigned. An update that adds or removes a published port, in either publish mode, reassigns each of the service's other auto-assigned ports. Clients then must discover the ports' numbers again ([§7.2.1](#721-port-allocation)). | `none` |
| Auto-assigned ports and `mode=host` ports | Not kept apart. An auto-assigned port can be a port that another service publishes in `mode=host`. `POST /services/create` then responds 201. The refusal of a port that both modes publish applies only to a chosen port. Take a node where a service's auto-assigned port is a port that another service publishes there in `mode=host`. On that node, one of two things occurs. Either the routing mesh does not serve the service's ports, or the `mode=host` task fails to start. Which one occurs depends on which of them binds first ([§7.2.1](#721-port-allocation)). | `none` |
| Flows to a stopping task from other nodes | Not drained reliably. A task leaves load balancing on its own node at once, and the task is stopped 2 s later. Another node may not learn of the withdrawal by that time. That node continues to send new flows to the task after the task stops, through the VIP and through published ports. Those flows fail until that node learns of the withdrawal ([§6.2](#62-health-and-drain), [§11.3](#113-churn)). | `none` |
| Ports above 65535 | Accepted. `POST /services/create` with an ingress-mode `PublishedPort` above 65535 responds 201. The service's tasks stay in the `new` state that `GET /tasks` reports. With a `mode=host` `PublishedPort` above 65535, the request also responds 201, and each of the service's tasks is rejected with `invalid port specification`. In `mode=host`, a `TargetPort` above 65535 is also accepted. The service's tasks run, but they do not publish the port ([§7.2.1](#721-port-allocation), [§7.3](#73-modehost)). | `none` |
| Node-local failures are not reported again without a new attempt | A node reports a failure to program cluster-scoped configuration only when something triggers a new attempt. An example of a trigger is a change to the service that the configuration is for, or to its tasks. Example: a node's data plane is wrong for an hour, and nothing triggers a new attempt. In that hour, the node says nothing after its first reports. An operator who did not see the first reports cannot discover the condition. | `none` |
| Recovery from a node-local failure is not reported | At the default log level, a successful attempt does not log anything. An operator sees the failures stop, and sees nothing else. No log entry or report tells the operator that the node's data plane is correct again. | `none` |
| Failure log entries do not name the service | On Linux nodes, a log entry for a load-balancer failure names the network, the VIP or a backend's address. Some of these entries contain only internal identifiers. Examples are a prefix of a container ID and the path of a network namespace. No entry names the service. An entry for a failure to program an IPsec security association prints `%!s(PANIC=String method: runtime error: invalid memory address or nil pointer dereference)` in place of the association. That entry does not name the peer ([§11.6](#116-failure-reporting)). | `none` |
| Recovery from a node-local failure is incomplete | On Linux nodes, the next attempt programs only the backend that triggered it. A backend whose programming failed does not get traffic through the node's load balancer until its task is replaced. This includes every backend that arrived while the failure continued. Take a failure that occurs after the node publishes the service's ingress ports, and a next attempt that follows. If the service is then deleted or stops publishing those ports, the host ports stay published until the daemon restarts ([§7](#7-published-ports), [§11.6](#116-failure-reporting)). | `none` |
| HTTP 500 for refused requests | Each request below is refused on purpose, but the request responds 500. 500 is the status for a fault in the daemon. The requests are: (1) `POST /swarm/init` with a data-path port outside the accepted range. (2) `POST /swarm/init` with a `SubnetSize` greater than 29, or with a non-zero `SubnetSize` less than the prefix length of one of its `DefaultAddrPool` prefixes. (3) `POST /swarm/init` and `POST /swarm/join` with `live-restore` true. (4) `POST /swarm/init` and `POST /swarm/join` with `firewall-backend` set to `nftables`, with `iptables` true, and without `features.swarm-nftables`. (5) `POST /containers/{id}/start` for a created or stopped container on a non-attachable Swarm-scoped network, and `POST /networks/{id}/connect` to such a network for a running container. (6) `POST /containers/{id}/start` for a created or stopped container that publishes a port that a service publishes in ingress mode. (7) On a node in a Swarm, `POST /containers/{id}/start` for a created or stopped container whose network was deleted after the container was created, or never existed. For some other invalid `POST /swarm/init` parameters, such as an invalid listen address, the request responds 400 ([§1](#1-cluster--node-configuration), [§2.2](#22-swarm-scoped-networks), [§7.2.1](#721-port-allocation), [§11.3](#113-churn)). | `none` |
| `SubnetSize` of 0 at init | Accepted. `POST /swarm/init` with `DefaultAddrPool` set and a `SubnetSize` of 0 responds 200. The leader manager then cannot create its network allocator. No Swarm-scoped network is allocated, the ingress network included. Only the leader manager's log reports this, in an entry that contains `failed to create allocator` ([§1](#1-cluster--node-configuration)). | `none` |
| HTTP 400 when the Swarm's state refuses a request | Misleading. A request that the Swarm's current state refuses responds 400, the status for a malformed request. An identical request succeeds once that state changes. The daemon maps the gRPC codes `FailedPrecondition` and `InvalidArgument` to 400. Examples are a service with a port in ingress mode in a Swarm without an ingress network, and `DELETE /networks/{id}` for a network that a service or task uses. Others are `DELETE /networks/{id}` for the ingress network while a service publishes a port in ingress mode, and a service that chooses a port that another service publishes ([§2.2](#22-swarm-scoped-networks), [§2.4](#24-the-ingress-network), [§7.2.1](#721-port-allocation)). | `none` |
| Error for an overlay network created on a worker | Misleading. On a worker, the error from `docker network create --driver overlay` says `This node is not a swarm manager. Use "docker swarm init" or "docker swarm join" to connect this node to swarm and try again.`. This error tells a node that is already in a Swarm to join a Swarm ([§2.2](#22-swarm-scoped-networks)). | `none` |
| Network and IPAM plugins on the leader | Required. A Swarm-scoped network whose driver or IPAM driver is a managed plugin can be created and allocated only while the leader manager has the plugin. After leadership moves to a manager without the plugin, the network still reports its allocation. But the tasks of services on it stay in the `new` state that `GET /tasks` reports. Only the leader manager's log reports the failure ([§2.2](#22-swarm-scoped-networks)). | `none` |
| Plugin names without a tag | Inconsistent. A managed plugin is found by its name without the tag when the request is checked, but its driver is registered under the full name. Thus `POST /networks/create` with a network driver named without the tag responds 500. An IPAM driver named without the tag is accepted, but the network is never allocated ([§2.2](#22-swarm-scoped-networks)). | `none` |
| Config-only networks for Swarm-scoped networks | Checked on one node. `POST /networks/create` for a Swarm-scoped network with `ConfigFrom` responds 201 only when the config-only network exists on the manager that handles the request. Each node that runs a task on the network uses its own config-only network of that name. Thus a manager that does not run tasks needs the config-only network only to allow the request. Task placement does not take into account which nodes have the config-only network. A task can be placed on a node that does not have it. That task is then rejected. On such a node, the attachment of a standalone container to the network fails after 20 seconds ([§2.2](#22-swarm-scoped-networks)). | `none` |
| Ingress networks without `Ingress` | Partly supported. Older daemons created the ingress network as an overlay network named `ingress` with the label `com.docker.swarm.internal`, without `Ingress`. `POST /networks/create` still accepts this form, and the network is treated as an ingress network. But a node sets up the routing mesh for it only while the node has a local attachment to it. A published port is not reachable at the address of any other node. When the network is deleted, the nodes keep their copies until the daemon restarts. A new ingress network then fails on those nodes when its subnet overlaps. The checks that `POST /networks/create` makes for `Ingress` do not apply to this form. Such a network can be `Attachable`. It can also be created while another ingress network exists, and it is then never allocated ([§2.4](#24-the-ingress-network)). | `none` |
| Local and Swarm-scoped networks with the same name | Not prevented on every node. A node can have a network that is not Swarm-scoped and that has the name of a Swarm-scoped network. A worker can create such a network, and a worker that has one can become a manager. `POST /networks/create` refuses the name only on the manager that handles the request ([§2.2](#22-swarm-scoped-networks)). On a manager with both networks, a request that names the network by name is ambiguous. On any node with both networks, the Swarm-scoped network is not usable. Its tasks are rejected there. A standalone container that names it by its ID cannot attach to it. One that names it by its name is attached to the other network. | `none` |
| Names that are a prefix of a Swarm-scoped network's ID | Resolved to the Swarm-scoped network on a manager. On a manager, `GET /networks/{id}` and `docker network inspect` with the name of a network that is not Swarm-scoped return a Swarm-scoped network whose ID starts with that name. On other nodes, and for networks that are not Swarm-scoped, a full-name match takes precedence over a partial ID ([§2.2](#22-swarm-scoped-networks), [network-common.md §1](network-common.md#1-network-api)). | `none` |
| `--config-from` on overlay networks | Not usable. `POST /networks/create` for an overlay network with `ConfigFrom` responds 201 only if the config-only network exists on the manager that handles the request. It responds 404 otherwise. Every attachment to the network then fails. The tasks of a service fail with `user-specified configurations are not supported if the network depends on a configuration network`. `docker run` exits non-zero after 20 seconds and prints an error that contains `attaching to network failed, make sure your network options are correct and check manager logs: context deadline exceeded` ([§2.2](#22-swarm-scoped-networks)). | `none` |
| Attachment failures for standalone containers time out | If the attachment of a standalone container to an attachable network fails, `POST /containers/{id}/start` waits 20 seconds. It then responds 500 with `attaching to network failed, make sure your network options are correct and check manager logs: context deadline exceeded`. This occurs in seven cases: (1) The network is `encrypted`, and the host's kernel does not offer the XFRM netlink interface. (2) The network is an overlay network created with `ConfigFrom`. (3) The network is deleted after the start has found it, but before the attachment task is created. (4) The container's static address is in use or is outside the network's subnets. (5) The network is not allocated. (6) The container's node has availability `drain` or `pause`. (7) The network is a Swarm-scoped network of a node-local driver created with `ConfigFrom`, and the container's node does not have the config-only network that `ConfigFrom` names. `POST /networks/{id}/connect` for a running container also waits 20 seconds in this case, and then responds 500 with the same message. In cases 1, 2 and 7, the reason appears in the attachment task only while the start waits. During that time, `GET /tasks` with the `runtime=attachment` filter lists the task as rejected. Then the task is deleted before the request responds. After that, the reason is only in the node's daemon log. In case 3, the attachment task is deleted, and no manager's daemon log reports the failure. In cases 4 and 5, the attachment task stays in the `new` state until it is deleted. `GET /tasks` reports that state. The reason then appears only in the leader manager's log, in an entry that contains `task allocation failure`. In case 6, the attachment task stays in the `pending` state until it is deleted. `GET /tasks` reports that state ([§2.2](#22-swarm-scoped-networks), [§2.3](#23-overlay-networks), [§9](#9-encryption), [§11.3](#113-churn), [§11.4](#114-scale-and-exhaustion)). | `none` |
| XFRM algorithm support is not checked | Not detected. Before a task or container joins an `encrypted` network, the daemon checks only that the kernel offers the XFRM netlink interface. On a host whose kernel does not offer `rfc4106(gcm(aes))`, the task starts, and the daemon logs a warning for each security association that it fails to add. The node's IPsec policies are in place, so its traffic on the network with other nodes does not pass ([§9](#9-encryption)). | `none` |
| ESP sequence numbers on busy node pairs | Exhausted before rekeying. An IPsec security association between two nodes has a 32-bit sequence number. The network encryption keys rotate every 12 hours, and the interval starts again when the leader manager changes. When a node sends 2³² packets to another node on one security association within an interval, the sequence number is exhausted. Traffic between the two nodes on `encrypted` networks then fails without an error that the operator can see. At about 1000 bytes per packet, this limits a node pair to about 100 MB/s on average in each direction. See [moby/moby#52005](https://github.com/moby/moby/issues/52005). | `none` |
| Partial blocks between nodes | Not covered. A firewall that blocks only the data path between two nodes (the data-path port, or ESP on an `encrypted` network) while gossip flows is not detected. The nodes stay peers, and the overlay traffic between them is dropped. A block in one direction only is not covered either. | `none` |
| Swarm under the nftables backend | Not asserted. With `features.swarm-nftables` set, a node in a Swarm can run the nftables backend, but the implementation is incomplete. No row of these specs is asserted on such a node, and a switch of a node in a Swarm to or from the nftables backend is not asserted either. moby/moby#53125 completes the implementation. | `none` |
| IPv6-only Swarm-scoped networks of node-local drivers | Refused. `POST /networks/create` for any Swarm-scoped network with `EnableIPv4` false responds 400 with `IPv4 cannot be disabled in a Swarm scoped network`. This holds whatever the network's driver is. Only `overlay` needs the refusal ([§2.2](#22-swarm-scoped-networks), [§2.3](#23-overlay-networks)). | `none` |
| Subnets and options on Swarm-scoped networks of node-local drivers | Ignored. `POST /networks/create` accepts a valid `IPAM.Config` and `Options` for a Swarm-scoped network of a node-local driver, but they are not kept. Each node creates the network from its own default address pools and the driver's defaults. A `ConfigFrom` that names a config-only network configures such a network on each node ([§2.2](#22-swarm-scoped-networks)). | `none` |
| Auxiliary addresses on overlay networks | Ignored. `POST /networks/create` accepts `AuxiliaryAddresses` in the `IPAM.Config` of an overlay network, but they are not kept. These addresses are not reserved, and tasks and VIPs can get them ([§2.3](#23-overlay-networks)). | `none` |
| Node-local attachable networks on Swarm leave | Disconnected. `POST /swarm/leave` disconnects every container on the node from each network that was created with `Attachable`. This includes node-local networks, such as user-defined bridge networks. The containers stay disconnected after the leave, and the node-local network stays. A warning that contains `is not a dynamic network` appears in the daemon log. Only Swarm-scoped networks are meant to be disconnected ([§11.1](#111-restart-and-reclamation)). | `none` |
| `mtu` on Swarm networks is not checked at create | Accepted. `POST /networks/create` with any value of `com.docker.network.driver.mtu` responds 201. If the value is not a non-negative integer, a Linux node rejects all tasks on the network that are assigned to that node. Their errors contain `failed to parse` or `invalid MTU value`. If the kernel cannot create the network's interfaces with the value, every such task fails at start. Windows nodes ignore the option ([§2.3](#23-overlay-networks)). | `none` |
| `ifname` is not checked before a task starts | Accepted. `POST /services/create` and `POST /services/{id}/update` accept any interface name in the driver options of a network attachment. Every task with an interface name that the OS refuses fails at start. Under the default restart policy, such a task is replaced indefinitely ([§2.2](#22-swarm-scoped-networks)). | `none` |
| Aliases in a service's `Hosts` | Dropped. `ContainerSpec.Hosts` takes entries in the form `<address> <name> <alias>...`, as in hosts(5). A task's `/etc/hosts` has a line that maps `<address>` to `<name>` only. The aliases are meant to map to `<address>` too ([network-common.md §4](network-common.md#4-etchosts)). | `none` |
| Services on the `host` network with other networks | Accepted. `POST /services/create` for a service attached to the `host` network and to an overlay network responds 201. `POST /services/create` for a service on `host` that publishes a port in ingress mode also responds 201. Every task of such a service fails at start. Under the default restart policy, such a task is replaced indefinitely ([§2.2](#22-swarm-scoped-networks)). | `none` |
| Unknown `Protocol` and `PublishMode` values | Accepted. `POST /services/create` with a published port whose `Protocol` or `PublishMode` the API reference does not list responds 201. Such a port is stored as `tcp` in ingress mode ([§7](#7-published-ports)). | `none` |

## Deliberately not asserted

These items are incidental to the implementation. A change to any of them does not break any row.

- The counters of Moby's own rules.
- Precedence over an operator's rule that the operator inserts at the top of the filter table's
  `INPUT` chain after an `encrypted` network is programmed. Such a rule can accept cleartext VXLAN
  for the network's VNI ([§12.2](#122-iptables)).
- The content of Moby's own chains and tables, and their names other than `DOCKER-USER`, except
  where [§12.3](#123-cross-generation-cleanup) pins them for cleanup and [bridge-networking.md
  §1.5](bridge-networking.md#15-rule-content) pins the bridge driver's rules. The content includes
  individual match/target lines and their order in a chain. A backend change replaces exactly
  these rules. A B row asserts their effect on packets, and never the content.
- Which packet-filtering technology is in use, except where
  [§12](#12-firewall-anchors--host-integration) pins a backend-specific anchor.
- The existence, name or topology of internal sandboxes (per-network load-balancer sandbox, ingress
  sandbox), network-namespace paths, and generated `veth` / bridge / VXLAN interface names. One
  exception is the name `ingress-sbox` in `Containers` of `GET /networks/docker_gwbridge`, which
  [network-common.md §1](network-common.md#1-network-api) pins. The other exception is a node's
  load-balancer endpoint on a network. `GET /networks/{id}` on that node lists this endpoint.
  [§2.3](#23-overlay-networks) asserts where its address falls, and
  [§6](#6-load-balancing-east-west-vip) asserts that a task observes it as the source address in
  NAT mode. The key and the name of this endpoint are not asserted.
- The load-balancer implementation and its scheduler, beyond the distribution property in
  [§6.1](#61-distribution).
- Internal refcounting, gossip timers, and reap and rejoin intervals. The surface for these is the
  convergence bounds in [§11.2](#112-convergence).
- The order in which the daemon allocates subnets from an address pool, addresses within a subnet,
  published ports from the auto-assignment range, and VNIs to networks created without
  `vxlanid_list`.
- The value of `LocalLBIndex`. `docker network inspect --verbose` shows this value, but the value is an
  internal firewall mark.
- The host-port reservation mechanics behind [§7.2.1](#721-port-allocation).
- Whether Windows nodes can use a VNI below 4096.
