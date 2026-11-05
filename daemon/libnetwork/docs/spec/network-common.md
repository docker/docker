Common networking surface area
==============================

Scope: the behavior that every network driver must have, and the Engine API requests for networks
and endpoints that do not depend on the network's driver. This spec describes them as observable
from outside the daemon.

This spec is a stub. It holds expectations that every network driver shares. They come from the work
on the other specs and from reviews of the specs' coverage. It is not yet a complete account.
[README.md](README.md) gives its conventions for tiers, vantage points and dependencies.
[swarm-networking.md](swarm-networking.md) and [bridge-networking.md](bridge-networking.md) cover
what is specific to their drivers. [container-dns.md](container-dns.md) covers name resolution.

In this spec, a predefined network is a network that `GET /networks` with the filter `type=builtin`
lists. The rows here also hold for Swarm-scoped networks and on nodes in a Swarm, unless a row
narrows them. [swarm-networking.md §2](swarm-networking.md#2-network-lifecycle--api-surface) covers
the requests that are specific to Swarm-scoped networks. Where a row of swarm-networking.md says
something different about the same request or state, that row holds.

## Host requirements

The kernel facilities are requirements of each Linux node. The host configuration is a requirement
of each node. The conventions for this section are in [README.md](README.md#host-requirements). The
specs of the drivers list what each driver needs:
[bridge-networking.md](bridge-networking.md#host-requirements),
[swarm-networking.md](swarm-networking.md#host-requirements) and
[ipvlan-networking.md](ipvlan-networking.md#host-requirements).

### Kernel facilities

| Facility | Needed when | Kernel options | Needed for | Pinned by |
| --- | --- | --- | --- | --- |
| Network namespaces | Always | `NET_NS` | Every section | The [§6](#6-daemon-restart) rows about network namespaces |
| macvlan links | A `macvlan` network | `MACVLAN` | [§2](#2-endpoint-interfaces) | The [§2](#2-endpoint-interfaces) row about a host without macvlan links |
| Dummy links | A `macvlan` network without a `parent` | `DUMMY` | [§2](#2-endpoint-interfaces) | Not pinned. The error for a missing dummy link does not name the facility. |
| VLAN links | A `macvlan` network with a `parent` of the form `<interface>.<VLAN ID>` | `VLAN_8021Q` | [§2](#2-endpoint-interfaces) | The [§2](#2-endpoint-interfaces) row about a host without VLAN links |

### Host configuration

| Configuration | Needed when | Needed for | Pinned by |
| --- | --- | --- | --- |
| The host's firewall passes the traffic that a row involves, to and from the node, its containers and its published ports. The rules and the firewalld configuration that the daemon programs are not part of the host's firewall. | Always, except in a row whose state names firewalld | Every row about traffic, in every spec | Not pinned |

## 1. Network API

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| An existing network that is not Swarm-scoped and not predefined | `POST /networks/create` with the network's name responds 409. `GET /networks` then lists only one network with that name. | B | `api` | `none` |
| An existing network that is not Swarm-scoped and not predefined | `docker network create` with the network's name exits non-zero and prints the daemon's error. `docker network ls` then lists only one network with that name. | B | `api` | `cli` |
| A Linux node that is not in a Swarm | `POST /networks/create` with the name of a predefined network, `default` or `container` responds 403. `POST /networks/create` with a name that starts with `container:` also responds 403. `GET /networks` then does not list a new network with that name. | B | `api:solo` | `none` |
| A Linux manager | `POST /networks/create` with the name `bridge` or `host` responds 409. It responds 403 when the name is `none`, `default` or `container`, or starts with `container:`. This holds for the default driver and for `overlay`. | C | `api:mgr` | `none` |
| Linux nodes | `docker network create` with the name of a predefined network exits non-zero and prints the daemon's error. | B | `api` | `cli` |
| Linux nodes | `DELETE /networks/{id}` for a predefined network responds 403. `GET /networks` then still lists the network. | B | `api` | `none` |
| Linux nodes | `docker network rm` for a predefined network exits non-zero and prints the daemon's error. `docker network ls` then still lists the network. | B | `api` | `cli` |
| A network that is not Swarm-scoped, with a running container attached to it | `DELETE /networks/{id}` for the network responds 403. `GET /networks` then still lists the network. | B | `api` | `none` |
| A network that is not Swarm-scoped, with a running container attached to it | `docker network rm` for the network exits non-zero and prints the daemon's error. `docker network ls` then still lists the network. | B | `api` | `cli` |
| A running container attached to a network | `POST /networks/{id}/connect` to that network, for the container, responds with a 4xx or 5xx status. The container's addresses on that network do not change. | B | `api` | `none` |
| A running container attached to a network that is not Swarm-scoped | `POST /networks/{id}/connect` to that network, for the container, responds 403. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| A running container attached to a network | `docker network connect` naming that network and the container exits non-zero and prints the daemon's error. | B | `api` | `cli` |
| A created or stopped container whose `NetworkSettings.Networks` lists a network that is not Swarm-scoped | `POST /networks/{id}/connect` to that network, for the container, responds 200. The container's endpoint settings for the network do not change. | C | `api` | `none` |
| A node that is not in a Swarm, with a created or stopped container | `POST /networks/{id}/connect` for the container, with an `{id}` that is not the name or an ID prefix of any network, responds 200. `POST /containers/{id}/start` for the container then responds 404 while no network has that name. See [§7](#7-known-divergences--non-goals). | C | `api:solo` | `none` |
| A created or stopped container | `POST /networks/{id}/connect` for the container, with an `{id}` that is not the name or an ID prefix of any network, responds 200. A network with that name is then created. `POST /containers/{id}/start` for the container then responds 204, and the container has an address on that network. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| A network that is not Swarm-scoped, and a container that is not attached to it | `POST /containers/create` with a static address in `IPAMConfig` that is outside every subnet of that network responds 400, and does not create a container. `POST /networks/{id}/connect` to that network, for the container, with that address, responds 400, and does not attach the container. | B | `api` | `none` |
| Linux nodes with a container that is not attached to the network `bridge` | `POST /containers/create` with a static address in `IPAMConfig` for the network `bridge` responds 400, and does not create a container. `POST /networks/{id}/connect` to the network `bridge`, for the container, with a static address in `IPAMConfig`, responds 400, and does not attach the container. | B | `api` | `none` |
| | `docker run` and `docker network connect` with `--ip` outside every subnet of a network that is not Swarm-scoped exit non-zero and print the daemon's error. On Linux nodes, they do the same with `--ip` for the network `bridge`. | B | `api` | `cli` |
| | At API version 1.43 or earlier, `POST /containers/create` with more than one network in `NetworkingConfig.EndpointsConfig` responds 400, and does not create a container. | B | `api` | `none` |
| A container, and a network that it is not attached to | `POST /networks/{id}/disconnect` for the container from that network responds with a 4xx or 5xx status. | B | `api` | `none` |
| A container, and a network that it is not attached to | `POST /networks/{id}/disconnect` for the container from that network responds 500, with a message that contains `is not connected to`. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| A container, and a network that it is not attached to | `docker network disconnect` naming that network and the container exits non-zero and prints the daemon's error. | B | `api` | `cli` |
| A running container with an endpoint on a network, from its start or from a later connect | `POST /networks/{id}/disconnect` for the container responds 200. `GET /containers/{id}/json` then does not list the network in `NetworkSettings.Networks`, and the container still runs. | B | `api` | `none` |
| A running container with an endpoint on a network, from its start or from a later connect | After `POST /networks/{id}/disconnect` for the container responds 200, the container does not have its interface on that network. | B | `ctr` | `os` |
| A running container with an endpoint on a network, from its start or from a later connect | `docker network disconnect` for the container exits zero. `docker inspect` for the container then does not list the network in `NetworkSettings.Networks`. | B | `api` | `cli` |
| A created or stopped container whose `NetworkSettings.Networks` lists a network | `POST /networks/{id}/disconnect` for the container from that network responds 200. `GET /containers/{id}/json` then does not list the network in `NetworkSettings.Networks`. | B | `api` | `none` |
| A created or stopped container whose `NetworkSettings.Networks` lists a network | `docker network disconnect` naming that network and the container exits zero. `docker inspect` for the container then does not list the network in `NetworkSettings.Networks`. | B | `api` | `cli` |
| A running container, and a network whose driver does not accept an endpoint MAC address ([§5](#5-driver-properties)) | `POST /networks/{id}/connect` to that network, for the container, with `MacAddress` set, at API version 1.54 or later, fails. `GET /containers/{id}/json` then lists the network in `NetworkSettings.Networks`, and `GET /networks/{id}` does not list the container. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| | `GET /networks/{id}` responds 200 with the network when `{id}` is the network's full ID or its name. It does the same when `{id}` is a prefix of the network's ID that the ID of no other network starts with, and that is not the name of another network. | B | `api` | `none` |
| | `docker network inspect` prints the network when it is given the network's full ID or its name. It does the same when it is given a prefix of the network's ID that the ID of no other network starts with, and that is not the name of another network. | B | `api` | `cli` |
| | `GET /networks/{id}` responds 404 when `{id}` is not the full ID, the name or a prefix of the ID of any network. | B | `api` | `none` |
| Two or more networks whose IDs start with a prefix that is not a network name | `GET /networks/{id}` with that prefix as `{id}` responds with a 4xx or 5xx status. | B | `api` | `none` |
| Two or more networks whose IDs start with a prefix that is not a network name | `GET /networks/{id}` with that prefix as `{id}` responds 400, with a message that contains `is ambiguous`. | C | `api` | `none` |
| A network whose name is a prefix of the ID of another network that is not Swarm-scoped | `GET /networks/{id}` with that name as `{id}` returns the network with that name. | C | `api` | `none` |
| | `GET /networks` with the filter `type=custom` lists the networks that the filter `type=builtin` does not list. | B | `api` | `none` |
| Linux nodes | `GET /networks` with the filter `type=builtin` lists `bridge`, `host` and `none`. | C | `api` | `none` |
| | `GET /networks` with a filter term that it does not support responds 400. `GET /networks` with a value that a filter term does not accept also responds 400. | B | `api` | `none` |
| | `docker network ls --filter` exits non-zero and prints the daemon's error for a filter term that the daemon does not support. It does the same for a value that a filter term does not accept. | B | `api` | `cli` |
| | The `type` filter term of `GET /networks` accepts the values `builtin` and `custom`. | C | `api` | `none` |
| | `GET /networks` with both `type=builtin` and `type=custom` lists a subset of the networks. The subset can differ from one request to the next. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| A network that is not Swarm-scoped and not predefined, with no running container attached to it, other than `docker_gwbridge` on a Linux node in a Swarm that has an ingress network | `GET /networks` with the filter `dangling=true` lists the network. | B | `api` | `none` |
| A network that is not Swarm-scoped, and that is predefined or has a running container attached to it | `GET /networks` with the filter `dangling=false` lists the network. | B | `api` | `none` |
| A node that is not in a Swarm | `POST /networks/prune` responds 200. It deletes each network that is not Swarm-scoped, not predefined and not config-only, and that no running container is attached to. `NetworksDeleted` in the response lists the names of the networks that it deletes. | B | `api:solo` | `none` |
| A node that is not in a Swarm | `docker network prune --force` exits zero, and prints the names of the networks that it deletes. `docker network ls` then does not list those networks. | B | `api:solo` | `cli` |
| A node that is not in a Swarm, and a `docker_gwbridge` network that no running container is attached to | `POST /networks/prune` deletes `docker_gwbridge`. See [§7](#7-known-divergences--non-goals). | C | `api:solo` | `none` |
| A Linux node in a Swarm that has an ingress network | `POST /networks/prune` does not delete `docker_gwbridge`. `GET /networks/docker_gwbridge` lists `ingress-sbox` in `Containers`. | C | `api:mgr`, `api:wkr` | `none` |
| | `POST /networks/create` with `ConfigOnly` true responds 201. `GET /networks/{id}` then reports `ConfigOnly` true. | B | `api` | `none` |
| A network that is not Swarm-scoped, created with `ConfigFrom` naming a config-only network | `GET /networks/{id}` for the network reports the config-only network's `IPAM` configuration. [swarm-networking.md §2.2](swarm-networking.md#22-swarm-scoped-networks) covers a Swarm-scoped network. | B | `api` | `none` |
| A network created with `ConfigFrom` naming a config-only network | The network's containers get addresses in the subnets of the config-only network's `IPAM` configuration. | B | `ctr` | `none` |
| | `POST /networks/create` with `ConfigFrom` naming a network that does not exist responds with a 4xx or 5xx status. `GET /networks` then does not list a network with the request's `Name`. | B | `api` | `none` |
| | `POST /networks/create` with `ConfigFrom` naming a network that does not exist responds 404, with a message that contains `configuration network "<name>" does not exist`. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| | `docker network create --config-from` naming a network that does not exist exits non-zero and prints the daemon's error. | B | `api` | `cli` |
| A config-only network, and a network that is not Swarm-scoped whose `ConfigFrom` names it | `DELETE /networks/{id}` for the config-only network responds 403. `GET /networks` then still lists the config-only network. [swarm-networking.md §2.2](swarm-networking.md#22-swarm-scoped-networks) covers a Swarm-scoped network. | B | `api` | `none` |
| A config-only network, and a network that is not Swarm-scoped whose `ConfigFrom` names it | `docker network rm` for the config-only network exits non-zero and prints the daemon's error. | B | `api` | `cli` |
| A config-only network | `POST /containers/create` for a container on that network responds 201. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| A config-only network, and a created or stopped container that is not attached to it | `POST /networks/{id}/connect` to that network, for the container, responds 200. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| A created container on a config-only network | `POST /containers/{id}/start` for the container responds with a 4xx or 5xx status. The container stays in the created state. | B | `api` | `none` |
| A created container on a config-only network | `POST /containers/{id}/start` for the container responds 403. The container stays in the created state. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| A config-only network | `docker run -d --network` naming that network exits non-zero and prints the daemon's error. | B | `api` | `cli` |
| A running container, and a config-only network | `POST /networks/{id}/connect` to that network, for the container, responds with a 4xx or 5xx status. `GET /networks/{id}` then does not list the container. | B | `api` | `none` |
| A running container, and a config-only network | `POST /networks/{id}/connect` to that network, for the container, responds 403, with a message that contains `cannot create endpoint on configuration-only network`. | C | `api` | `none` |
| A running container, and a config-only network | After `POST /networks/{id}/connect` to that network, for the container, fails, `GET /containers/{id}/json` lists the network in `NetworkSettings.Networks`. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| A running container, and a config-only network | `docker network connect` naming that network and the container exits non-zero and prints the daemon's error. | B | `api` | `cli` |

## 2. Endpoint interfaces

The rows in this section hold for an endpoint of any network driver that adds an interface to the
container ([§5](#5-driver-properties)). An endpoint requests an interface name when its
`com.docker.network.endpoint.ifname` is set to that name.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| An endpoint that requests an interface name | The interface has that name. See [§7](#7-known-divergences--non-goals). | B | `task`, `ctr` | `os` |
| Linux nodes with an endpoint that does not request an interface name, on a network of a built-in driver without `com.docker.network.container_iface_prefix` | The interface name is `eth<N>`. | C | `task`, `ctr` | `os` |
| Linux nodes with a created or stopped container with an endpoint whose `com.docker.network.endpoint.ifname` the OS refuses | `POST /containers/{id}/start` for the container responds with a 4xx or 5xx status. | B | `api` | `none` |
| Linux nodes with a running container | `POST /networks/{id}/connect` for the container, with an endpoint whose `com.docker.network.endpoint.ifname` the OS refuses, responds with a 4xx or 5xx status. | B | `api` | `none` |
| Linux nodes with a created or stopped container with an endpoint whose `com.docker.network.endpoint.ifname` the OS refuses | `POST /containers/{id}/start` for the container responds 500, with a message that contains `error renaming interface` and the requested name. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| Linux nodes with a running container | `POST /networks/{id}/connect` for the container, with an endpoint whose `com.docker.network.endpoint.ifname` the OS refuses, responds 500. The message contains `error renaming interface` and the requested name. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| Linux nodes | `docker run -d` with a `com.docker.network.endpoint.ifname` that the OS refuses exits non-zero and prints the daemon's error. | B | `api` | `cli` |
| Linux nodes with a running container | `docker network connect` for the container, with a `com.docker.network.endpoint.ifname` that the OS refuses, exits non-zero and prints the daemon's error. | B | `api` | `cli` |
| Linux nodes | At API version 1.44 or later, `POST /containers/create` for a container with two endpoints that request the same interface name responds 201. So does a request for a container with an endpoint that requests `lo`. | C | `api` | `none` |
| Linux nodes with a created or stopped container with two endpoints that request the same interface name | `POST /containers/{id}/start` for the container responds with a 4xx or 5xx status. | B | `api` | `none` |
| Linux nodes with a created or stopped container with an endpoint that requests `lo` | `POST /containers/{id}/start` for the container responds with a 4xx or 5xx status. | B | `api` | `none` |
| Linux nodes with a running container | `POST /networks/{id}/connect` for the container, with an endpoint that requests `lo`, responds with a 4xx or 5xx status. It does the same with an endpoint that requests the name of one of the container's interfaces. | B | `api` | `none` |
| Linux nodes with a created or stopped container with two endpoints that request the same interface name, or with one that requests `lo` | `POST /containers/{id}/start` for the container responds 500, with a message that names the interface. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| Linux nodes with a running container | `POST /networks/{id}/connect` for the container, with an endpoint that requests `lo` or the name of one of its interfaces, responds 500. The message names the interface. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| Linux nodes | `docker run -d` exits non-zero and prints the daemon's error when two of the container's endpoints request the same interface name. It does the same when an endpoint requests `lo`. | B | `api` | `cli` |
| Linux nodes with a running container | `docker network connect` for the container exits non-zero and prints the daemon's error when the endpoint requests `lo` or the name of one of its interfaces. | B | `api` | `cli` |
| A running standalone container whose endpoint on a network has an `IPv4Address` or `IPv6Address` in `IPAMConfig` | Its interface on that network has that address. | B | `ctr` | `os` |
| A running standalone container whose endpoint on a network has an `IPv4Address` or `IPv6Address` in `IPAMConfig` | `GET /containers/{id}/json` reports the address as `IPAddress` or `GlobalIPv6Address` in `NetworkSettings.Networks.<network>`. | B | `api` | `none` |
| A running container created at API version 1.44 or later with an endpoint `MacAddress` on a network whose driver accepts an endpoint MAC address ([§5](#5-driver-properties)) | Its interface on that network has that MAC address. | B | `ctr` | `os` |
| A running container, and a network whose driver accepts an endpoint MAC address ([§5](#5-driver-properties)) | After `POST /networks/{id}/connect` with `MacAddress` set, at API version 1.54 or later, responds 200, the container's interface on the network has that MAC address. | B | `ctr` | `os` |
| A running container, and a network whose driver does not accept an endpoint MAC address ([§5](#5-driver-properties)) | `POST /networks/{id}/connect` to that network, for the container, with `MacAddress` set, at API version 1.54 or later, responds with a 4xx or 5xx status. `GET /networks/{id}` then does not list the container. | B | `api` | `none` |
| A network whose driver does not accept an endpoint MAC address ([§5](#5-driver-properties)) | At API version 1.44 or later, `POST /containers/create` for a container whose endpoint on that network sets `MacAddress` responds 201. `POST /containers/{id}/start` for the container then responds with a 4xx or 5xx status, and the container stays in the created state. | C | `api` | `none` |
| | `docker run -d` with `--mac-address` and with `--network` naming a network whose driver does not accept an endpoint MAC address ([§5](#5-driver-properties)) exits non-zero and prints the daemon's error. | B | `api` | `cli` |
| A Linux host whose kernel does not offer macvlan links, and a created or stopped container with an endpoint on a `macvlan` network | `POST /containers/{id}/start` for the container responds 500, with a message that contains `failed to create the macvlan port`. See [Host requirements](#host-requirements). | C | `api` | `none` |
| A Linux host whose kernel does not offer VLAN links | `POST /networks/create` for a `macvlan` network whose `parent` is `<interface>.<VLAN ID>`, for an existing interface `<interface>` that does not yet have that VLAN sub-interface, responds 500, with a message that contains `vlan link`. See [Host requirements](#host-requirements). | C | `api` | `none` |

## 3. Default gateway

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A container attached only to `internal` networks | The container does not have a default route. See [§7](#7-known-divergences--non-goals). | B | `task`, `ctr` | `os` |
| Linux nodes with a container with endpoints on two or more networks that are not `internal`, one with a higher `GwPriority` than the others | For each IP version that the endpoint gives the container a gateway or default route for, the container's default route goes through that endpoint. | B | `ctr` | `os` |
| Linux nodes with a container with endpoints on two or more networks that are not `internal`, all with the same `GwPriority` | For each IP version, the container's default route is through the first endpoint that gives a gateway or default route for that version, in this order: (1) endpoints that give one for both IPv4 and IPv6, then endpoints that give one for a single version; (2) within each group, network names in byte order. | C | `ctr` | `none` |
| | At API version 1.47 or earlier, `POST /containers/create` ignores `GwPriority` in `EndpointsConfig`. | B | `api`, `ctr` | `none` |
| | `POST /networks/{id}/connect` applies `GwPriority` at every API version. | C | `api`, `ctr` | `none` |

## 4. `/etc/hosts`

This section holds on Linux nodes.

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A container that does not use the host's network | For each of the container's addresses, `/etc/hosts` has a line that maps the address to the container's hostname. The exceptions are addresses on the ingress network and on `docker_gwbridge`. IPv6 addresses are also exceptions while IPv6 is disabled in the container, so that its loopback interface does not have an IPv6 address, for example with `net.ipv6.conf.all.disable_ipv6` set to 1. | B | `task`, `ctr` | `none` |
| A container whose `Hostname` contains a dot, or that has a `Domainname` | Each line for the container's addresses gives two names. The first is the full name: the hostname, then a dot and the domain name if the container has one. The second is the part of the hostname before its first dot. | B | `ctr` | `none` |
| A running container | After `POST /networks/{id}/connect` for the container responds 200, `/etc/hosts` has the lines for the container's addresses on that network. | B | `ctr` | `none` |
| A running container with an endpoint on a network, from its start or from a later connect | After `POST /networks/{id}/disconnect` for the container responds 200, `/etc/hosts` does not have the lines for the container's addresses on that network. | B | `ctr` | `none` |
| A daemon with `host-gateway-ips` set | An entry `<name>:host-gateway` in `HostConfig.ExtraHosts` adds a line to `/etc/hosts` for each address in `host-gateway-ips`. Each line maps the address to `<name>`. | B | `ctr` | `none` |
| A daemon without `host-gateway-ips` | An entry `<name>:host-gateway` in `HostConfig.ExtraHosts` adds a line to `/etc/hosts` for each gateway address of the default bridge. This holds whatever networks the container is attached to. | C | `ctr` | `none` |
| A daemon without `host-gateway-ips` and with `bridge` set to `none`, and a created container with an entry `<name>:host-gateway` in `HostConfig.ExtraHosts` | `POST /containers/{id}/start` for the container responds 500, with a message that contains `unable to derive the IP value for host-gateway`. The container stays in the created state. See [§7](#7-known-divergences--non-goals). | C | `api` | `none` |
| A daemon without `host-gateway-ips` and with `bridge` set to `none` | `docker run` with `--add-host <name>:host-gateway` exits non-zero and prints the daemon's error. | C | `api` | `cli` |

## 5. Driver properties

Rows in this spec name a property of a network driver, not the driver itself, where the set of
drivers with the property can change. This table gives the properties of the built-in drivers. It is
tier C: it pins which drivers have each property today. A plugin driver has the properties that it
implements. A cell is observed from `ctr`, in a container on a network of that driver, and depends
on `os`. The `l2tunnel` and `ics` cells hold on platforms that can create those networks.

| Driver | Adds an interface to the container | Accepts an endpoint MAC address |
| --- | --- | --- |
| `bridge` | Yes | Yes |
| `overlay` | Yes | Yes |
| `macvlan` | Yes | Yes |
| `ipvlan` | Yes | No |
| `host` | No | Not applicable |
| `null` (the `none` network) | No | Not applicable |
| Windows: `nat`, `transparent`, `l2bridge`, `l2tunnel`, `ics` | Yes | Yes |

## 6. Daemon restart

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| Linux nodes | After `SIGKILL` and restart, every network namespace that the daemon created before the kill is removed, other than an overlay network's namespace and the namespace of a container that live-restore keeps running. This includes the namespaces of load-balancer and ingress sandboxes, and of containers that the daemon does not keep running. An overlay network's namespace is the one that holds the network's VXLAN device. A namespace that the daemon creates after the restart is a different namespace, even at the same path. The row is checked as soon as the daemon's API answers again, without a ceiling, on a node in a Swarm too. See [swarm-networking.md §13](swarm-networking.md#13-known-divergences--non-goals). | B | `node` | `os` |
| Linux nodes with a running container, whose daemon has `live-restore` true | After `SIGKILL` and restart, the container keeps its network namespace and its interfaces. | B | `node`, `ctr` | `os` |

## 7. Known divergences & non-goals

| Topic | Current behavior | Depends on |
| --- | --- | --- |
| HTTP 500 for refused requests | Each request below is refused on purpose, but it responds 500. 500 is the status for a fault in the daemon. The requests are: (1) `POST /networks/{id}/disconnect` for a container that is not attached to the network. (2) `POST /containers/{id}/start` and `POST /networks/{id}/connect` for an endpoint with a `com.docker.network.endpoint.ifname` that the OS refuses, or that another endpoint of the container already uses. (3) `POST /containers/{id}/start` for a created or stopped container with a `host-gateway` entry in `HostConfig.ExtraHosts`, when the daemon does not have an address for `host-gateway`. (4) On a node in a Swarm, `POST /containers/{id}/start` for a created or stopped container that `POST /networks/{id}/connect` connected to a network that does not exist ([§1](#1-network-api), [swarm-networking.md §2.2](swarm-networking.md#22-swarm-scoped-networks), [§2](#2-endpoint-interfaces), [§4](#4-etchosts)). | `none` |
| Networks that a created or stopped container cannot use | Accepted. For a created or stopped container, `POST /networks/{id}/connect` to a network that does not exist, or to a config-only network, responds 200. `POST /containers/create` for a container on a config-only network also responds 201. Only `POST /containers/{id}/start` refuses the network. For a network that does not exist, it responds 404 with `network <name> not found` on a node that is not in a Swarm. On a node in a Swarm, it responds 500 with a message that contains `network <name> not found`. For a config-only network, it responds 403 with `cannot create endpoint on configuration-only network`. If a network with the missing name is created before the start, the start attaches the container to it ([§1](#1-network-api)). | `none` |
| Connecting a running container to a network it is already attached to | Refused with two different statuses. The request responds 403 on a network that is not Swarm-scoped, and 409 on an attachable Swarm-scoped network ([§1](#1-network-api), [swarm-networking.md §2.2](swarm-networking.md#22-swarm-scoped-networks)). | `none` |
| HTTP 404 for a missing `ConfigFrom` network | `POST /networks/create` with `ConfigFrom` naming a network that does not exist responds 404. 404 is the status for a request whose own resource does not exist. Here the request is valid, and its body names the missing network ([§1](#1-network-api)). | `none` |
| Several values for the `type` filter | Not combined. With both `type=builtin` and `type=custom` given, `GET /networks` tests each network against one of the two values, chosen at random. The response lists a different subset of the networks each time. As with other filter terms, a network is meant to match when it matches either value ([§1](#1-network-api)). | `none` |
| Default route on `internal` overlays on Windows | A Windows container attached only to `internal` overlays has a default route ([§3](#3-default-gateway)). | `none` |
| `ifname` option on Windows | Ignored. A Windows container's interface name does not follow `com.docker.network.endpoint.ifname` ([§2](#2-endpoint-interfaces)). | `none` |
| Interface left behind after a failed connect | If `POST /networks/{id}/connect` for a running container fails at the rename, the interface stays in the container under its generated `veth` name. The interface is down and does not have addresses. Its host-side peer stays attached. The interface and the peer stay until the container's network namespace is destroyed. A failed connect is meant to leave nothing in place ([§2](#2-endpoint-interfaces)). | `none` |
| Interface names the kernel rewrites | Accepted without an error. For a name that contains `%d`, the kernel gives the interface the first free name that matches, such as `eth0`. A later disconnect responds 200, but the interface stays in the container. The interface stays up and keeps its address ([§2](#2-endpoint-interfaces)). | `none` |
| Interface-name collisions depend on attach order | Take a container with two endpoints. One endpoint asks for `eth0`. The name of the other endpoint is generated. The container starts or fails, depending on the order in which its endpoints are attached. This order is random. The endpoint that asks for the name is meant to get it, whatever the order ([§2](#2-endpoint-interfaces)). | `none` |
| Network listed after a failed connect | Left in place. `POST /networks/{id}/connect` for a running container can fail when the daemon creates the endpoint. For example, it fails when the network's driver does not accept the endpoint, or when the network is config-only. After such a failure, `GET /containers/{id}/json` lists the network in `NetworkSettings.Networks`, but the container does not have an endpoint on it. A failed connect is meant to leave nothing in place ([§1](#1-network-api)). | `none` |
| `docker_gwbridge` outside a Swarm | Pruned. On a node that is not in a Swarm, `docker_gwbridge` is not a predefined network. `POST /networks/prune` deletes it while no running container is attached to it. This includes a `docker_gwbridge` that an operator creates before the node joins a Swarm, to configure the gateway network ([§1](#1-network-api), [swarm-networking.md §8.1](swarm-networking.md#81-gateway-network)). | `none` |
