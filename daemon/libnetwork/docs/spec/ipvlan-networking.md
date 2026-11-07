ipvlan networking surface area
==============================

Scope: the ipvlan network driver, as observable from outside the daemon. Every expectation here
holds on Linux nodes. A row about `POST /networks/create` or `docker network create` holds for a
network that is not Swarm-scoped.

This spec is a stub. It holds expectations that belong to the ipvlan driver. They come from reviews
of the specs' coverage. It is not yet a complete account of the ipvlan driver.
[README.md](README.md) gives its conventions for tiers, vantage points and dependencies.
[network-common.md](network-common.md) covers what every network driver must do, including the
rows for a driver that does not accept an endpoint MAC address.

## Host requirements

The conventions for this section are in [README.md](README.md#host-requirements). The ipvlan driver
also needs the requirements in [network-common.md](network-common.md#host-requirements). The
daemon needs Linux 4.2 or later for an ipvlan network. It checks the kernel version, and does not
probe for ipvlan links. The check guards against defects in the L2 mode of ipvlan before Linux 4.2,
which a probe cannot detect.

### Kernel facilities

| Facility | Needed when | Kernel options | Needed for | Pinned by |
| --- | --- | --- | --- | --- |
| ipvlan links, in Linux 4.2 or later | Always | `IPVLAN` | Every section | The [§2](#2-networks-and-interfaces) rows about a kernel older than 4.2 and about a host without ipvlan links |
| Dummy links | A network without a `parent` | `DUMMY` | Every section | Not pinned. The error for a missing dummy link does not name the facility. |
| VLAN links | A network with a `parent` of the form `<interface>.<VLAN ID>` | `VLAN_8021Q` | Every section | The [§2](#2-networks-and-interfaces) row about a host without VLAN links |

## 1. Endpoint options

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| An ipvlan network | `POST /containers/{id}/start` for a created or stopped container whose endpoint on that network sets `MacAddress`, created at API version 1.44 or later, responds 500. `POST /networks/{id}/connect` to that network for a running container, with `MacAddress` set, at API version 1.54 or later, also responds 500. Both messages contain `ipvlan interfaces do not support custom mac address assignment`. See [§3](#3-known-divergences--non-goals). | C | `api` | `none` |

## 2. Networks and interfaces

| Applies to | Expectation | Tier | Observed from | Depends on |
| --- | --- | --- | --- | --- |
| A Linux host whose kernel version is older than 4.2 | `POST /networks/create` for an ipvlan network responds 500, with a message that contains `kernel version failed to meet the minimum ipvlan kernel requirement`. See [Host requirements](#host-requirements). | C | `api` | `none` |
| A Linux host whose kernel does not offer ipvlan links, and a created or stopped container with an endpoint on an ipvlan network | `POST /containers/{id}/start` for the container responds 500, with a message that contains `failed to create the ipvlan port`. See [Host requirements](#host-requirements). | C | `api` | `none` |
| A Linux host whose kernel does not offer VLAN links | `POST /networks/create` for an ipvlan network whose `parent` is `<interface>.<VLAN ID>`, for an existing interface `<interface>` that does not yet have that VLAN sub-interface, responds 500, with a message that contains `vlan link`. See [Host requirements](#host-requirements). | C | `api` | `none` |

## 3. Known divergences & non-goals

| Topic | Current behavior | Depends on |
| --- | --- | --- |
| HTTP 500 for refused requests | The daemon refuses the requests below on purpose, but they respond 500. 500 is the status for a fault in the daemon. The first request is `POST /containers/{id}/start` for a container whose endpoint on an ipvlan network sets `MacAddress`. The second is `POST /networks/{id}/connect` to an ipvlan network for a running container, with `MacAddress` set, at API version 1.54 or later ([§1](#1-endpoint-options)). | `none` |
