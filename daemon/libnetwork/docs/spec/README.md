Networking test-surface specifications
======================================

Each document here specifies a part of Moby's networking as it is observable from outside the
daemon. A document states:

- what that part is expected to do
- what a failing test means
- the vantage point from which to observe it
- what the verdict of a test depends on

| Spec | Scope |
| --- | --- |
| [network-common.md](network-common.md) | What every network driver must do, and the Engine API requests for networks and endpoints that do not depend on the driver. A stub. |
| [swarm-networking.md](swarm-networking.md) | The overlay network driver and the Swarm service mesh |
| [ipvlan-networking.md](ipvlan-networking.md) | The ipvlan network driver. A stub. |
| [bridge-networking.md](bridge-networking.md) | The bridge network driver, and the parts of the daemon's firewall integration that are not specific to Swarm. A stub. |
| [container-dns.md](container-dns.md) | Name resolution inside a container on a user-defined network, whatever the network's driver. A stub. |

A spec describes behavior only. It should change when the behavior of the daemon changes. It should
also change when a regression reveals an expectation that, until then, was only an assumption.

## Who the specs serve

The specs serve two kinds of test. Functional tests serve the maintainer's story: *"as a Moby
maintainer, catch regressions in code changes."* Acceptance tests serve the packager's story: *"as a
packager, verify a candidate release works on every platform in my support matrix."*
[Depends on](#depends-on) tells which rows have a verdict that can differ between platforms.

The daemon can have facilities built in for testing, such as a failpoint or a timer override. A
packaged build can ship such a facility, enabled only by an environment variable whose name starts
with `DOCKER_TEST_`, as the daemon's existing test hooks are.

### Nodes

A Linux node can be a system container: a container that boots the platform's own init system and
userland, with the daemon's package installed, on the platform's kernel. Examples are a
systemd-nspawn machine and a privileged container that runs systemd. The expectations hold on such
nodes as on hosts, when the nodes share a Linux bridge as the physical network between them and the
host's own firewall does not filter the traffic on that bridge. A Windows node is a Windows host or
virtual machine.

## Expectations

A spec is a series of numbered sections. Each section has a table of expectations. A row gives the
state that the expectation applies to, the expectation, its tier, the vantage point from which to
observe it, and what its verdict depends on. After the numbered sections, a spec can list its known
divergences & non-goals, and what it deliberately does not assert.

The "Applies to" cell gives the state before the row's action. Examples are objects that exist,
daemon settings, the platform, and what happened to an object before the action. It never names the
action itself. A request, a command, an update or a restart that the row is about goes in the
"Expectation" cell. When the cell is blank, the row applies whenever its section does.

An expectation states the outcome of an action at the same level of abstraction as the action. The
outcome of an Engine API request is the API's response: its status and body. The outcome of a
`docker` command is what the command prints and its exit status. An HTTP status is not visible at
the level of a `docker` command. A row about a `docker` command names `cli` among what it depends
on. It usually has a partner row about the API request that the command makes.

An outcome covers only the direct response to the action. A side effect is not part of the outcome.
Examples of side effects are a network created or removed, traffic on the wire, and an entry in the
daemon's log. A spec states a side effect where it can be observed, whatever the action was. An
expectation that an action succeeds also states the side effect that shows the action took effect. A
daemon that reports success and does nothing fails such an expectation.

Unless the row names a range, an expectation about an Engine API request holds at every API version
that the daemon accepts by default. These versions go from the daemon's default minimum version to
the current version.

A row names a daemon setting by its `daemon.json` key, such as `firewall-backend` or
`features.swarm-nftables`. The matching `dockerd` flag, such as `--firewall-backend`, sets the same
setting. A row holds whichever of the two sets it. A row that names a `dockerd` flag, such as
`--mtu`, holds only when that flag sets the setting. Any other flag that a row names, such as
`--dns`, is a flag of a `docker` command.

Unless a row, its section or its spec says otherwise, an expectation holds on a node in its baseline
state. For a daemon installed from a package or an installer, the baseline is a clean install of a
supported OS, a fresh install of the package, and the setup that the product's installation
documentation prescribes. The package and that documentation are part of the system under test. For
a daemon that is not installed from a package, such as one built in CI, the baseline is a host that
meets the host requirements of these specs, with no other configuration. The host requirements are
the daemon's own requirements, whatever the distribution ([Host requirements](#host-requirements)).

The iptables backend means that the daemon's `firewall-backend` is `iptables`, the default. The
nftables backend means that it is `nftables`.

A row's state describes changes from the baseline. A row holds with each daemon setting that it does
not name as the baseline leaves it: as the package installs it, or at its default for a daemon that
is not installed from a package. It holds at another value only where the row or its section says
so. The exception is the firewall backend: a row that does not name a backend holds under both, at
the same tier, because the nftables backend behaves like the iptables backend except where a spec
says otherwise. Until moby/moby#53125, no row of these specs is asserted on a node in a Swarm under
the nftables backend
([swarm-networking.md §13](swarm-networking.md#13-known-divergences--non-goals)).

A driver's rows hold in every scope that the driver supports, local and Swarm, unless a row or its
spec says otherwise. A row about a standalone container on a Swarm-scoped network holds where
[swarm-networking.md §2.2](swarm-networking.md#22-swarm-scoped-networks) lets the container attach
to the network. A row about a network holds whatever options the network has, unless the row or its
section names them. The options are those of the network that the row names. The options of a
Swarm-scoped network of a node-local driver are those of the config-only network that its
`ConfigFrom` names on the node. Without `ConfigFrom`, the network has none of the `Options` given at
create. A spec can say that a section's rows hold in several cases, such as the two ends on the same
node or on different nodes, or each pairing of the nodes' platforms. A row then holds in every
combination of those cases, the network's options and the daemon settings under which the row holds.

Unless a row or its section says otherwise, the rule has these limits:

- The daemon does not publish ports on an `internal` network, and does not forward traffic between
  such a network and addresses outside it. A row about a published port, or about traffic between a
  container and an address outside the container's networks, holds where the network that carries
  the traffic is not `internal`. A row about a query that the embedded resolver forwards upstream
  holds for a querying container that has external access, as [container-dns.md](container-dns.md)
  defines it.
- A row about the addresses that a network allocates, or about its `Status.IPAM`, holds for a
  network that uses the default IPAM driver.
- A row does not cover a network that a known divergence says is not allocated, or that a container
  cannot attach to.

The release under test is *N*. The previous release, *N−1*, is each of two releases: the previous
patch release of *N*, and the latest patch release of the previous minor release line. A row about
*N−1* holds for each of them. A build from a release branch counts as the next patch release of that
branch. A build from the main branch, such as a pull request, counts as the next minor release, so
its *N−1* is the latest patch release of the latest minor release line. For an x.y.0 release, the
two releases are the same. A row about *N−1* holds whether *N−1* runs from its package or from its
released binaries. Apart from what the package's upgrade path decides, a row that names `upgrade`
also holds when the daemon's binaries are replaced in place of a package upgrade. A row about *N−1*
asserts the behavior of *N*. When a known defect in an *N−1* release breaks such a row, the row does
not cover the combination that the defect breaks, until *N−1* is a release without the defect. A
defect is known when *N* has its fix and that *N−1* release does not.

A request that a node's daemon carries out itself, such as a request about a container or its
endpoints, whatever the scope of the network, or about a network that is not Swarm-scoped, takes
effect on that node once the request responds, unless the row names a ceiling. The exception is a
node's loss of its local attachment to a Swarm-scoped network after its last container on that
network stops or disconnects. That loss is a removal in a Swarm, timed from when the request
responds. The effects of `POST /swarm/init`, `POST /swarm/join` and `POST /swarm/leave` on the node
that sent the request hold within the leave-and-rejoin ceiling, timed from when the request
responds. Any other effect in a Swarm holds within the ceiling of the change's convergence class in
[swarm-networking.md §11.2](swarm-networking.md#112-convergence). This includes every effect of a
Swarm request, such as a request about a service, a Swarm-scoped network or a node, on every node,
the manager that handled the request included. The effect of a removal is in the reassignment class.
So is the effect of an addition, such as a new task, service or endpoint, or a task's change to
healthy. So is the effect of a change that is neither, such as a change of a service's endpoint
mode, networks, published ports or aliases. The exceptions are the addresses, VIPs, published ports
and address counts that the API reports, which are in the allocation class. A task's change to
unhealthy, the exit of its container, or a change of its desired state away from `running` takes
effect on every node, the task's own node included, within the allocation ceiling. The ceiling
starts when the task's node emits the container's `health_status` or `die` event, or when `GET
/tasks` first reports the new `DesiredState`. What holds once no container of a service is running
also holds within the allocation ceiling, from the last of those `die` events. The effect of a
change of leader holds within the allocation ceiling too, from the change. The effect of a firewalld
reload holds within 30 seconds, the smallest of those ceilings. Some rows send a request that
succeeds only once an earlier effect in the row holds. Such a row holds when one attempt at the
request, before the ceiling of that effect has passed, gets the expected response. A request that is
refused has no effect to wait for. A row about what a refused request leaves unchanged holds once
the request responds. Any other row that says that something does not happen holds once its ceiling
has passed. A timer override does not shorten a ceiling. A row that says that something does not
happen, without an action, holds once a row that says that something does happen in the same setup
holds.

Traffic reaches a destination when the destination receives it. A response from the destination
shows that the traffic reached it. Traffic cannot reach a destination when the destination does not
receive it within 5 seconds of the send, whatever the sender gets back. This also holds for a row
that says that traffic does not reach a destination, or that a destination is not reachable. A
destination does not answer when traffic to it gets a refusal or gets no response within 5 seconds.
A refusal is a TCP reset or an ICMP unreachable error. A row that names the error holds only for
that error.

### Tier

Every expectation carries a tier. The tier says what a failing test means and how to respond to it.

| Tier | A failure means |
| --- | --- |
| B (behavioral) | *You broke this.* User-visible semantics. A change needs justification. |
| C (characterization) | *You changed this. Was that intended?* Internal but load-bearing. A deliberate change is fine. An accidental change is not. |
| (omitted) | Incidental. The spec does not assert it at all. Each spec lists such behaviors under *Deliberately not asserted*. |

A row in a spec's known divergences & non-goals is not an expectation.
A numbered C row pins a divergence that is worth a change detector. The "Depends on" cell of a
divergence names the facility that decides on which platforms it shows.

A behavior can carry both tiers, as a B row and a C row. The inner MTU in
[swarm-networking.md §3](swarm-networking.md#3-observable-state-inside-a-container) is the model
case: it is B as a property of the path and C as a pinned value.

### Observed from

A black-box expectation means something only from a stated vantage point. Some rows hold from one
vantage point and not from another.

| Code | Vantage point |
| --- | --- |
| `task` | Inside a Swarm service task's container |
| `ctr` | Inside a standalone container, one that is not a Swarm service task. A spec can narrow which networks the container is attached to. |
| `node` | The host network namespace of a node running the daemon |
| `underlay` | The physical network between nodes. Its traffic can be observed at any host on it, including at a node's own network interfaces. Traffic on it can come from any host on it other than the node under test, such as another node, or a container outside the node under test that is attached to the physical network. |
| `client` | A host other than the node under test that can reach one of the node's addresses. It can be another node of the same Swarm. |
| `api:mgr` | Engine API / CLI against a Swarm manager |
| `api:wkr` | Engine API / CLI against a Swarm worker |
| `api:active` | Engine API / CLI against a Swarm node, manager or worker, whose availability is `active`. Only such a node can run a Swarm service task or attach a standalone container to a Swarm-scoped network. |
| `api:solo` | Engine API / CLI against a daemon that is not in a Swarm |
| `api` | Engine API / CLI against a daemon, whether or not it is in a Swarm |
| `log` | The daemon log on a node. It is the only channel for a whole class of partial failure. See [swarm-networking.md §11.6](swarm-networking.md#116-failure-reporting) |

In a row, `a` → `b` names two vantage points: traffic sent from `a` is observed at `b`.

Where a row refers to the node's address, it holds at each address assigned to an interface in the
node's host network namespace that the client can reach. A row that says "any address of the node"
holds at every one of them. Two limits apply at an IPv6 link-local address of the node. A row about
a published port does not hold there when the daemon option `userland-proxy` is false
([bridge-networking.md §3.2](bridge-networking.md#32-port-publishing)). A row about the source
address that a container observes does not hold there. A row about a published port holds at an
address of the node only when the endpoint that the daemon publishes the port on has an address of
that family. With `userland-proxy` true, a port published on an endpoint without an IPv6 address is
also reachable at the node's IPv6 addresses. A row about the source address that a container or task
observes does not hold there.

A B row about the daemon log refers to the entry with the text that the row's C partner gives. A
deliberate change to that text breaks both rows. A row that says that the log does not report
something holds when no entry with that text appears.

### Depends on

A row names the facilities its verdict depends on, or `none`. A code can have a platform in
parentheses after it, such as `os` (Windows). Such a code applies on nodes of that platform only. On
the other nodes, the row names `none`.

| Code | Facility | What varies |
| --- | --- | --- |
| `os` | What the operating system vendor supplies: the kernel and first-party userland | On Linux: the kernel's netfilter, IPVS, XFRM, VXLAN and SCTP support, nftables and iptables userland, the iptables-nft shim, firewalld, systemd-resolved or NetworkManager, and sysctl defaults. On Windows: the Host Networking Service, the Virtual Filtering Platform and the DNS client. Which features each OS release offers, and how they behave. |
| `upgrade` | A package's upgrade path from the previous release | Its service units and maintainer scripts, which stop, start or restart the daemon across the upgrade |
| `cli` | The `docker` CLI a packager ships with the daemon | Its version, how it turns commands and flags into API requests, and how it prints the responses |

What a code means for a row's scope:

> A row that names a facility holds on every platform in a packager's support matrix, whatever its
> tier, with the facility that the platform supplies. Its verdict can differ from one platform to
> another. The verdict of a row that names `none` does not differ between platforms that run the same
> daemon build.

A row names a facility if the facility decides the verdict when the real facility is in use. A row
whose state fixes how a facility behaves, such as a kernel that does not offer XFRM, names `none`
when the daemon alone decides the verdict in that state.

### Platforms

Unless a section or a row narrows it, an expectation holds on Linux and Windows nodes alike. Unless
a section or a row narrows it, an expectation also holds for traffic between nodes of any mix of the
two. A section built on a Linux-only mechanism says so in its introduction. A row that depends on a
Linux-only daemon setting, such as the userland proxy or the firewall backend, holds on Linux nodes.
The spec lists a platform on which an expectation does not hold yet as a known divergence.

A row about traffic between nodes holds for each pairing of the two nodes' platforms: Linux to
Linux, Linux to Windows, Windows to Linux and Windows to Windows.

### Host requirements

A spec can list host requirements in a section of that name. A host requirement is something that a
node must provide for the spec's expectations to apply. A row applies only on a host that meets the
requirements it relies on, so its "Applies to" cell does not repeat them. A row whose "Applies to"
names a host that lacks a requirement states how the daemon behaves on such a host. Such a row also
holds where the lack is emulated, for example by a Linux security module policy that denies the
facility. The lists give the minimum that a node must provide. Together, the lists of all the specs
are the baseline for a daemon that is not installed from a package. A package and its installation
documentation meet them on each platform that the package supports.

Each kind of requirement has a table of its own:

- Kernel facilities: features of the Linux kernel that the daemon uses, such as VXLAN devices or
  IPVS. The "Kernel options" column gives the options that provide each facility. They are
  the options of the mainline Linux kernel, without their `CONFIG_` prefix. A kernel can offer a
  facility as a module or built in.
- Programs: commands that the daemon runs, such as `iptables` or `nft`, and what each must accept.
  A spec can add the kernel options that a program needs. The daemon does not check them.
- Host configuration: state of the host that the daemon relies on and does not set, such as a
  sysctl that must have a given value, or traffic that the host's firewall must accept.
- Windows facilities: services of the Windows operating system that the daemon uses, such as the
  Host Networking Service.

The "Needed when" column gives the daemon settings and network options under which the daemon needs
the requirement. "Always" means that the daemon needs it whatever its settings. The "Needed for"
column names the sections whose rows rely on it.

Each requirement names the row that pins it, usually a C row. A pinning row states how the daemon
behaves on a host that lacks the requirement, such as an error that names the missing facility, or
it states host state that an operator can observe, such as a sysctl. When the daemon stops needing
the requirement, that row fails, and the change has to update the requirement too. A pinning row
does not catch a new requirement. A new requirement shows when other rows fail on a host that lacks
it. A requirement without such a row says "Not pinned". A spec does not pin a requirement with a row
about the daemon's internal plumbing, such as which kind of interface it creates or the rules it
programs in a private network namespace.