Container DNS surface area
==========================

Scope: name resolution inside a container on a user-defined network, whatever the network's
driver. This covers the `/etc/resolv.conf` that the daemon writes for the container and the
embedded DNS resolver behind it, as observable from outside the daemon.

This is a stub. It holds expectations that do not depend on any one driver. They come from the
work on the [Swarm networking surface area](swarm-networking.md) and from reviews of the specs'
coverage. It is not yet a complete account. Its conventions for tiers, vantage points and
dependencies are in [README.md](README.md).

A container sends its DNS queries to the embedded DNS resolver, which is a DNS server that the
daemon runs for the container. The rows state what the embedded resolver answers, and what it
forwards to upstream nameservers. A name resolves to an address when the embedded resolver answers a
DNS query for the name with that address. A container has external access when its default route is
on a network that is not `internal`, and replies to its egress traffic reach it. On a bridge network
with `com.docker.network.bridge.enable_ip_masquerade` set to `false`, replies can fail to reach it
([bridge-networking.md §3.3](bridge-networking.md#33-egress)). The host's `resolv.conf` is the
`/etc/resolv.conf` that the daemon reads, even on a host that runs systemd-resolved.

On Linux nodes, the containers that follow do not use the embedded resolver, and nothing here
applies to them, other than the rows whose state names the default bridge:

- A container attached only to the default bridge network.
- A container that uses the host's network.

## Host requirements

These requirements hold on Linux nodes. The conventions for this section are in
[README.md](README.md#host-requirements). The embedded resolver needs NAT rules in the container's
network namespace under the active firewall backend. It needs them whether or not the daemon's
`iptables` setting is true.

### Kernel facilities

| Facility | Needed when | Kernel options | Needed for | Pinned by |
| --- | --- | --- | --- | --- |
| nftables with the NAT expression | The nftables backend | `NF_TABLES`, `NF_TABLES_IPV4`, `NFT_NAT`. `NFT_CHAIN_NAT_IPV4` on kernels that have it. | Every section | The [bridge-networking.md](bridge-networking.md#host-requirements) nftables facility and its pins |

### Programs

| Program | Needed when | Needed for | Pinned by |
| --- | --- | --- | --- |
| `iptables`, accepting the nat table and the `DNAT` and `SNAT` targets | The iptables backend | Every section | The [§4](#4-port-53-in-the-container) rows about a host without the `iptables` command |
| `nft` | The nftables backend | Every section | The [bridge-networking.md](bridge-networking.md#14-firewall-backend-and-firewalld) row about a host without the `nft` command |

The daemon does not depend on which implementation of `iptables` the host has. The kernel options
that the program needs depend on that implementation:

| Program | Implementation | Kernel options |
| --- | --- | --- |
| `iptables` | Either | `NETFILTER`, `NETFILTER_XTABLES`, `NF_CONNTRACK`, `NF_NAT`, `NETFILTER_XT_NAT` |
| `iptables` | iptables-legacy | `IP_NF_IPTABLES`, `IP_NF_NAT`. `IP_NF_IPTABLES_LEGACY` from Linux 6.9, and `NETFILTER_XTABLES_LEGACY` from Linux 6.17. |
| `iptables` | iptables-nft | `NF_TABLES`, `NF_TABLES_IPV4`, `NFT_COMPAT`, `NFT_NAT`. Before Linux 5.1, `NFT_CHAIN_NAT_IPV4` in place of `NFT_NAT`. |

### Host configuration

| Configuration | Needed when | Needed for | Pinned by |
| --- | --- | --- | --- |
| External network access, and nameservers in the host's `resolv.conf` that resolve public names | Always | The [§3](#3-forwarding) row about public DNS names | Not pinned |

## 1. `resolv.conf`

This section holds on Linux nodes.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | The `/etc/resolv.conf` nameserver is `127.0.0.11`. | C | `ctr` | `none` |
| A container whose DNS options include a valid `ndots` | `/etc/resolv.conf` has the `ndots` value from the container's DNS options. | B | `ctr` | `none` |
| A container whose DNS options are empty, and a host `resolv.conf` with a valid `ndots` | `/etc/resolv.conf` has the `ndots` value from the host's `resolv.conf`. | B | `ctr` | `none` |
| A container whose DNS options include any option | `/etc/resolv.conf` has none of the options in the host's `resolv.conf`, `ndots` included. | C | `ctr` | `none` |
| A container whose options in effect do not include a valid `ndots` | `/etc/resolv.conf` contains `options ndots:0`. This applies when the options do not set `ndots`, and when all of the `ndots` values in them are invalid. | C | `ctr` | `none` |
| A container created with search domains | `/etc/resolv.conf` has the search domains that the container was created with. | B | `ctr` | `none` |
| A container created without search domains, and a daemon with `dns-search` set | `/etc/resolv.conf` has the daemon's `dns-search`. | B | `ctr` | `none` |
| A container created without search domains, and a daemon without `dns-search` | `/etc/resolv.conf` has the search list from the host's `resolv.conf`. | B | `ctr` | `none` |
| | `/etc/resolv.conf` does not contain a search domain of `.` given with `--dns-search` or in the daemon's `dns-search`. | B | `ctr` | `none` |
| A container whose only search domain, given with `--dns-search` or in the daemon's `dns-search`, is `.` | `/etc/resolv.conf` does not have a `search` line, even if the host's `resolv.conf` has one. | B | `ctr` | `none` |
| | Apart from `ndots`, `/etc/resolv.conf` lists the options in effect as they were given. | B | `ctr` | `none` |
| A running container | After a process in the container changes the container's `/etc/resolv.conf`, the file keeps the process's content. A connect or disconnect of the container does not change the file. | B | `ctr` | `none` |
| A host whose `/etc/resolv.conf` names a nameserver other than 127.0.0.53, and a running container attached only to the default bridge whose `/etc/resolv.conf` has not changed since the daemon wrote it | After `POST /networks/{id}/connect` to a user-defined network responds 200, the container resolves the names of the containers on that network. It resolves other names through the nameservers in the host's `resolv.conf`, loopback nameservers included. See [§5](#5-known-divergences--non-goals). | B | `ctr` | `none` |
| A running container attached only to the default bridge, whose `/etc/resolv.conf` has not changed since the daemon wrote it | After `POST /networks/{id}/connect` to a user-defined network responds 200, the `/etc/resolv.conf` nameserver is `127.0.0.11`. | C | `ctr` | `none` |
| A running container that started with an endpoint on a user-defined network | The container's `/etc/resolv.conf` keeps the content that it had at start. This holds after later connects and disconnects. | C | `ctr` | `none` |
| A running container that started attached only to the default bridge, and that a later connect attached to a user-defined network | The container's `/etc/resolv.conf` keeps the content that it had after that connect. This holds after later connects and disconnects. | C | `ctr` | `none` |
| A running container that has had an endpoint on a user-defined network at any time since it started | After the container disconnects from every user-defined network, the embedded resolver continues to answer the container's queries. See [§5](#5-known-divergences--non-goals). | C | `ctr` | `none` |

A container's DNS options are the options that it was created with. If it was created without DNS
options, they are the daemon's `dns-opts`. The options in effect are the container's DNS options
when it has any. Otherwise, they are the options in the host's `resolv.conf`. A valid `ndots` is an
integer of 0 or more. `/etc/resolv.conf` keeps a valid `ndots` above 15 as given, although glibc
treats any `ndots` above 15 as 15.

## 2. Names and answers

In this section, the querying container is the container that sends the DNS query. The embedded
resolver has records for a name on a network when an endpoint on that network has that name. An
endpoint's names are its container's name, short ID and hostname, and its network aliases. In
`<name>.<network>`, `<name>` stands for a name and `<network>` for the name of a network. Both are
placeholders, not literal text.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A container's name resolves to its address on a network it shares with the querying container. See [§5](#5-known-divergences--non-goals). | B | `ctr` | `none` |
| | A container's name and its network aliases resolve whatever the case of the query: a container named `Web` resolves as `web` and as `WEB`. | B | `ctr` | `none` |
| | A network alias given to a container's endpoint resolves to the container's address on that network, for containers on that network only. | B | `ctr` | `none` |
| Several containers on a network whose endpoints have the same network alias, where no other network of the querying container has an endpoint with that alias | The alias resolves to the addresses of all of those containers on that network. | B | `ctr` | `none` |
| | A container's short ID (the first 12 hex digits of its ID) and its hostname resolve as its name does. | C | `ctr` | `none` |
| A container on a user-defined network | `GET /containers/{id}/json` lists in `NetworkSettings.Networks.<network>.DNSNames` the container's name, and then the aliases of its endpoint on that network. The name and each alias resolve on that network. | B | `api`, `ctr` | `none` |
| A container on a user-defined network | `NetworkSettings.Networks.<network>.DNSNames` in `GET /containers/{id}/json` lists the container's short ID and then its hostname after the aliases. It does not list a name more than once. | C | `api` | `none` |
| A running container whose hostname is not its name | After `POST /containers/{id}/rename` for the container responds 204, `NetworkSettings.Networks.<network>.DNSNames` in `GET /containers/{id}/json` has the new name for each user-defined network. It does not have the old name. Its other entries do not change. | C | `api` | `none` |
| Linux nodes with a container attached to the default bridge | `NetworkSettings.Networks.bridge.DNSNames` in `GET /containers/{id}/json` is empty. | C | `api` | `none` |
| Linux nodes with a running container attached to the default bridge | After `POST /containers/{id}/rename` for the container responds 204, `NetworkSettings.Networks.bridge.DNSNames` lists the new name, the short ID and, when it differs from the short ID, the hostname. See [§5](#5-known-divergences--non-goals). | C | `api` | `none` |
| | A name resolves when an endpoint with that name exists on any network that the querying container is attached to. | B | `ctr` | `os` (Windows) |
| | A query for a name does not get the address of an endpoint on a network that the querying container is not attached to. | B | `ctr` | `none` |
| A created or stopped standalone container | After `POST /containers/{id}/start` for the container responds 204, its name and its aliases on each of its networks resolve for the other containers on that network. This holds whether or not the container has a healthcheck. | B | `ctr` | `none` |
| A running standalone container | After `POST /networks/{id}/connect` for the container responds 200, its name and its aliases on that network resolve for the other containers on the network. This holds whether or not the container has a healthcheck. | B | `ctr` | `none` |
| A running container with an endpoint on a user-defined network | After `POST /networks/{id}/disconnect` for the container from that network responds 200, the other containers on that network do not resolve its name or aliases to its address on that network. | B | `ctr` | `none` |
| A running container with endpoints on user-defined networks | After `POST /containers/{id}/stop` for the container responds 204, the other containers on each of those networks do not resolve its name or aliases to its address on that network. | B | `ctr` | `none` |
| A node that is not in a Swarm, and a running container whose hostname is not its name | After `POST /containers/{id}/rename` for the container responds 204, its new name resolves and its old name does not, for containers on each of its networks. | B | `ctr` | `none` |
| A node in a Swarm, and a running container whose hostname is not its name, with an endpoint on a Swarm-scoped network of a multi-host driver, such as `overlay` | After `POST /containers/{id}/rename` for the container responds 204, its new name resolves and its old name does not, for containers on that network on its node. See [§5](#5-known-divergences--non-goals). [swarm-networking.md §5](swarm-networking.md#5-service-discovery) covers containers on other nodes. | B | `ctr` | `none` |
| | The embedded resolver answers an A or AAAA query for a name that it has records for from those records. This holds for a single-label name too, whatever `ndots` the options in effect set ([§1](#1-resolvconf)). | B | `ctr` | `none` |
| A container on a network with IPv6 that it shares with the querying container, other than an overlay network, where no other network of the querying container has an endpoint with that container's name | An AAAA query for that name returns the container's IPv6 address on the shared network. | B | `ctr` | `none` |
| Records in the embedded resolver for a name that are all A records, or all AAAA records | A query of the other type for that name gets an empty `NOERROR` response. The embedded resolver does not forward this query upstream. | B | `ctr` | `none` |
| | An MX query for a name that the embedded resolver has A or AAAA records for gets an empty `NOERROR` response. The embedded resolver does not forward this query upstream. | B | `ctr` | `none` |
| | The embedded resolver forwards upstream a query of a type other than A, AAAA, MX, PTR and SRV. This holds for a name that it has records for too. | C | `ctr` | `none` |
| A Linux node that is not in a Swarm | Several of the querying container's networks can have records for a name. The embedded resolver answers an A or AAAA query for that name from the records of the first network only. The order is: (1) Endpoints with a higher `GwPriority` first. (2) Networks that are not `internal` first. (3) Endpoints with a gateway or default route for more IP versions first. (4) Network names in byte order. When that network does not have an address of the family asked for, the query gets an empty `NOERROR` response. This holds even if another network has one. | C | `ctr` | `none` |
| | Over UDP, an answer from the embedded resolver's own records lists all of the name's addresses when it fits in a size limit. The limit is the larger of 512 bytes and the query's EDNS0 buffer size. When the answer does not fit, the embedded resolver truncates it and sets the TC bit. Over TCP, the answer lists all of the name's addresses. | B | `ctr` | `none` |
| | In answers from the embedded resolver's own records, the order of a name's addresses varies from one query to the next. | C | `ctr` | `none` |
| | The records in an answer from the embedded resolver's own records have a TTL of 600 seconds. This holds for A, AAAA and PTR answers. | C | `ctr` | `none` |
| | Reverse (PTR) lookups resolve for the IPv4 and IPv6 addresses of containers on the querying container's networks. | B | `ctr` | `os` (Windows) |
| | A forward lookup of the name that a reverse lookup returns for a container's address returns that address. | B | `ctr` | `none` |
| | The answer to a reverse lookup of a container's address is `<name>.<network>.`, where `<name>` is the container's name and `<network>` is the network's name. It is never an alias, the short ID or the hostname. | C | `ctr` | `os` (Windows) |
| A querying container attached to the network `<network>`, and records for `<name>` on that network | A query for `<name>.<network>` resolves to the addresses of `<name>` on that network only. See [§5](#5-known-divergences--non-goals). | B | `ctr` | `none` |
| | In a `<name>.<network>` query, `<network>` matches a network only when its case is the same as the case of the network's name. `<name>` matches whatever its case. | C | `ctr` | `none` |
| A network whose name is a valid DNS label and that the querying container is not attached to, where no network of the querying container has an endpoint named `<name>.<network>`, `<network>` being that network's name | The embedded resolver forwards a query for `<name>.<network>` upstream. | C | `ctr` | `none` |
| A querying container attached to the network `<network>`, where an endpoint is named `<name>`, and an endpoint named `<name>.<network>` on one of the querying container's networks | `<name>.<network>` resolves to the address of the endpoint with the whole name. It does not resolve to the address of `<name>` on the network `<network>`. | C | `ctr` | `none` |
| Linux nodes with a container with `<name>:<alias>` in the `Links` of its endpoint settings for a user-defined network | The alias resolves to the named container's address on that network. It resolves only for queries from the container that has the link. | B | `ctr` | `none` |
| Linux nodes | An alias in the `Links` of any of the querying container's endpoints takes precedence over the names and network aliases on the container's networks. The alias resolves to the linked container even when another container on one of those networks has that name. | C | `ctr` | `none` |

## 3. Forwarding

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A container that has external access | Public DNS names resolve. | B | `ctr` | `os` |
| Linux nodes with a container attached only to `internal` networks | The embedded resolver does not forward the container's queries to any nameserver that the container cannot reach from its own network namespace. It also does not forward them to the nameservers in the host's `resolv.conf`. | B | `ctr` | `none` |
| Linux nodes with a container attached only to `internal` networks and created without `--dns` nameservers, whose daemon does not have a `dns` setting | A query for a name that the embedded resolver does not have a record for gets SERVFAIL. When the options in effect set `ndots`, a single-label A or AAAA query gets an empty `NOERROR` response instead ([§1](#1-resolvconf), [§5](#5-known-divergences--non-goals)). | C | `ctr` | `none` |
| A container that has external access and was created with `--dns` nameservers | The embedded resolver forwards to the nameservers set with `--dns`, in place of the host's nameservers. | B | `ctr` | `os` |
| A daemon with a `dns` setting, and a container that has external access and was created without `--dns` nameservers | The embedded resolver forwards to the nameservers in the daemon's `dns` setting, in place of the host's nameservers. | B | `ctr` | `os` |
| A container attached only to `internal` networks and created with `--dns` set to the address of a nameserver in another container on one of those networks | The container created with `--dns` resolves names through that nameserver. | B | `ctr` | `os` |
| A daemon with a `dns` setting that holds the address of a nameserver in a container on an `internal` network, and another container attached only to `internal` networks, that network among them, and created without `--dns` nameservers | The container created without `--dns` nameservers resolves names through that nameserver. | B | `ctr` | `os` |
| Linux nodes with a container that started with an endpoint on a user-defined network, has external access and was created without `--dns` nameservers, whose daemon does not have a `dns` setting | The embedded resolver forwards to the nameservers in the host's `resolv.conf`, loopback nameservers included. It queries them from the host's network namespace. | B | `ctr` | `os` |
| Linux nodes | The embedded resolver queries nameservers set with `--dns` or in the daemon's `dns` setting from the container's network namespace. | B | `ctr` | `os` |
| | The embedded resolver forwards a query to the next nameserver when the current nameserver does not respond, or responds SERVFAIL or REFUSED. When each nameserver that it forwards to does not respond, or responds SERVFAIL or REFUSED, the client gets SERVFAIL. | B | `ctr` | `none` |
| | Over UDP, when no nameserver that the embedded resolver forwards a query to responds, the client gets SERVFAIL within 4 seconds times the number of those nameservers. | C | `ctr` | `none` |
| | The embedded resolver returns a nameserver's response other than SERVFAIL or REFUSED, NXDOMAIN included, to the client. It does not ask the next nameserver. | B | `ctr` | `none` |
| | The embedded resolver forwards to at most the first three nameservers. | C | `ctr` | `none` |
| Linux nodes | The embedded resolver does not forward to a nameserver `127.0.0.11` set with `--dns` or in the daemon's `dns` setting. That nameserver does not count toward the three nameservers. | C | `ctr` | `none` |
| Linux nodes with a container whose only nameserver, set with `--dns` or in the daemon's `dns` setting, is `127.0.0.11` | A query for a name that the embedded resolver does not have a record for gets SERVFAIL. When the options in effect set `ndots`, a single-label A or AAAA query gets an empty `NOERROR` response instead ([§1](#1-resolvconf), [§5](#5-known-divergences--non-goals)). | C | `ctr` | `none` |
| | The embedded resolver forwards upstream an A or AAAA query for a multi-label name that does not match any container, service or alias on the querying container's networks. This does not hold for `<name>.<network>` when the querying container is attached to a network named `<network>` and the embedded resolver has records for `<name>` on it ([§2](#2-names-and-answers)). | B | `ctr` | `none` |
| A container on a Windows node, or a container on a Linux node whose options in effect, per [§1](#1-resolvconf), do not set `ndots` | The embedded resolver forwards a single-label A or AAAA query upstream when it does not have a record for the name. | B | `ctr` | `none` |
| A container on a Linux node whose options in effect, per [§1](#1-resolvconf), set `ndots` to 1 or more | A single-label A or AAAA query gets an empty `NOERROR` response when the embedded resolver does not have a record for the name. The embedded resolver does not forward the query upstream. See [§5](#5-known-divergences--non-goals). | C | `ctr` | `none` |

The row for an `ndots` of 1 or more is tier C because the behavior itself is in question.
[moby/moby#53157](https://github.com/moby/moby/issues/53157) proposes to put container names in a
domain on the search list. That change would make this behavior unnecessary. An `ndots` of 0, or an
invalid `ndots`, has the same effect. That effect is a bug for these values. See
[§5](#5-known-divergences--non-goals).

## 4. Port 53 in the container

This section holds on Linux nodes.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| | A process in the container can bind UDP and TCP port 53 on `127.0.0.11` without an error. No packet reaches that socket. | C | `ctr` | `os` |
| A daemon with the iptables backend and `iptables` false, on a host without the `iptables` command, and a running container with an endpoint on a user-defined network | A DNS query from the container to `127.0.0.11` does not get an answer. See [Host requirements](#host-requirements). | C | `ctr` | `os` |
| A daemon with the iptables backend and `iptables` false, on a host without the `iptables` command, and a running container with an endpoint on a user-defined network | The daemon log contains `setting up DNAT/SNAT rules failed`. See [Host requirements](#host-requirements). | C | `log` | `none` |

## 5. Known divergences & non-goals

| Topic | Current behavior | Depends on |
| --- | --- | --- |
| Single-label forwarding with an `ndots` of 0 or an invalid `ndots` | Not forwarded. This applies when the options in effect set `ndots` to 0 or to an invalid value. A single-label A or AAAA query then gets an empty `NOERROR` response when the embedded resolver does not have a record for the name. This is the same as for an `ndots` of 1 or more ([§3](#3-forwarding)). Only an `ndots` of 1 or more is meant to have that effect ([moby/moby#53104](https://github.com/moby/moby/issues/53104)). The effect shows in Docker-in-Docker. The inner daemon reads the outer container's `resolv.conf` as the `resolv.conf` of its host. The inner daemon finds the `ndots:0` that the outer daemon added. The containers of the inner daemon cannot resolve names that only the outer resolver knows. | `none` |
| Single-label queries after a live restore | Forwarded. After the daemon restarts with `live-restore` true, the embedded resolver forwards upstream a single-label A or AAAA query that it does not have a record for, from a container that kept running. This occurs whatever `ndots` the options in effect set ([§3](#3-forwarding)). | `none` |
| Renaming a container on a Swarm node | Not applied. On a node in a Swarm, `POST /containers/{id}/rename` changes the container's records only on Swarm-scoped networks of multi-host drivers, such as overlay. It does not change them on other networks, such as user-defined bridge networks and Swarm-scoped networks of node-local drivers. The container's new name does not resolve there. Its old name continues to resolve there after the rename. The old name continues to resolve there after the container is removed ([§2](#2-names-and-answers)). | `none` |
| Records after a node leaves a Swarm | Lost. Take a node in a Swarm, whether it created the Swarm with `POST /swarm/init` or joined it, that leaves it with `POST /swarm/leave`. After the leave, the names of the containers that were running on the node's user-defined networks before the leave stop resolving there. A container that starts after the leave gets records that resolve ([§2](#2-names-and-answers)). | `none` |
| `resolv.conf` after the embedded resolver starts | Not updated. After the embedded resolver starts for a container, `/etc/resolv.conf` does not change again while the container runs. A container that disconnects from every user-defined network keeps `nameserver 127.0.0.11` and the embedded resolver. A container started on the default bridge alone gets the host's nameservers ([§1](#1-resolvconf)). | `none` |
| Upstream nameservers after a connect from the default bridge | On a Linux node whose `/etc/resolv.conf` names only 127.0.0.53, take a container that started attached only to the default bridge and later connects to a user-defined network. It forwards to the nameservers in `/run/systemd/resolve/resolv.conf`, not to 127.0.0.53 ([§1](#1-resolvconf), [§3](#3-forwarding)). | `os` |
| `DNSNames` on the default bridge after a rename | Filled in. On Linux nodes, `GET /containers/{id}/json` does not report `DNSNames` for the default bridge, which does not have name resolution. After a rename of the running container, it reports the new name, the short ID and the hostname there. These names do not resolve for a container attached to the default bridge alone. On a node that is not in a Swarm, a container attached to the default bridge and a user-defined network resolves the new name and the short ID to the renamed container's address on the default bridge ([§2](#2-names-and-answers)). | `none` |
| Reverse-lookup round trip on Windows | Fails. A reverse lookup returns `<name>.<network>`. The embedded resolver on a Windows node does not answer names in that form. It forwards a query for such a name upstream ([§2](#2-names-and-answers)). | `none` |
| Nameservers on `internal` networks on Windows | Not applied. A Windows endpoint on an `internal` network does not get the nameservers set with `--dns` or in the daemon's `dns` setting. A Windows container attached only to `internal` networks cannot resolve names through a nameserver on one of them ([§3](#3-forwarding)). | `none` |

## Deliberately not asserted

- The comments the daemon writes into `resolv.conf`, such as its header and its record of where
  `ndots` came from.
- The ports that the embedded resolver listens on.
- The firewall rules that the daemon programs in a container's network namespace: whether they
  exist, their content and names, and the hooks and priorities of their base chains.
