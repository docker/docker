# Moby threat model

This document defines the security scope and untrusted inputs of Moby, the open-source container engine.
Its daemon, `dockerd`, usually runs as root, manages containers through containerd and runc, and exposes the Docker Engine REST API.

Engine API access, over REST or gRPC, grants the daemon user's privileges, so API clients, host root, Swarm managers, and installed plugins are trusted.
Bind-mounting `docker.sock` or `containerd.sock` into an untrusted container is unsafe: access to these sockets can grant the corresponding daemon's privileges, even through a read-only bind mount.
Multi-tenant setups are possible, but API clients aren't isolated from each other; an AuthZ plugin can restrict trusted clients, and only bypassing it is a vulnerability.
Container processes, image content and build inputs, registries, network traffic, Swarm workers beyond their own tasks, and unprivileged host users are untrusted.
A bug is a vulnerability when one of these can use it to escape or weaken isolation, gain privileges, reach the host or other containers, leak secrets, or seriously disrupt the daemon.
Bugs in containerd, runc, and the kernel go upstream, unless Moby's use of them causes the issue.

## Assets

- Host: the whole system the daemon runs on, including its filesystem, processes, devices, network, and availability.
- Daemon control plane: the API socket or named pipe, TCP API endpoints, and the containerd socket.
- Daemon-managed resources, which the daemon is expected to access, unlike the rest of the host:
  - Container storage: the data root, images, container filesystems, volumes, and build cache.
  - Image integrity: content must match its digests.
  - Registry credentials.
- Other containers.
- Swarm cluster state, secrets, and configs.

## Actors

Trusted, because they already have the privileges an attack would give them:

- Host root and the daemon's user.
- API clients, including local users in the `docker` group and clients restricted by an AuthZ plugin.
- Swarm managers.
- Installed plugins and drivers, within the privileges granted at install time.

Untrusted:

- Remote hosts on the network, excluding Swarm cluster members.
- Container processes, including rootless containers and Swarm workloads.
- Image and build input authors.
- Registries, including mirrors and insecure registries.
  A registry is trusted only with the credentials given for it, and can send them to the token server it names.
- Swarm workers: trusted with read-only knowledge of tasks scheduled onto them and their dependencies (secrets and configs), but not with the rest of the cluster.
- Local unprivileged host users.
- Daemons and API endpoints that a program using the Go client connects to.

## Trust boundaries and untrusted input

Engine API access, over REST or gRPC, is privileged: it grants the daemon user's privileges, root by default, or an unprivileged user in rootless mode.
Multi-tenant setups come with security trade-offs: API clients aren't isolated from each other.
The daemon controls API access with TLS client certificates (`--tlsverify`), socket or named pipe permissions, and the active authorization (AuthZ) plugin.
A bug is not a vulnerability if the attacker needs API access, unless it bypasses these checks or gains more privileges than the daemon's user has.

Image content and build inputs are attacker-controlled, even when a trusted client passes them in: layers, image config (labels, environment, `USER`, entrypoint, `VOLUME`, `EXPOSE`, `HEALTHCHECK`, `ONBUILD`), Dockerfiles, and build contexts.
They can control the workload inside the container.
Image directives can have their documented effects, but they must not grant privileges or choose host resources the client didn't allow.
For example, `VOLUME` can create volumes and copy image files into them, but must not make the daemon modify host paths outside those volumes.

We treat these as untrusted:

- Container processes, within the isolation described in [Container isolation](#container-isolation).
  Escapes that affect the host or other containers are in scope, including symlink and TOCTOU races in container filesystem operations: `docker cp`, archive and export endpoints, volume and bind mount setup, and `docker commit`.
- Container and image files the daemon reads, such as `/etc/passwd` and `/etc/group` when resolving `USER`, `--user`, `--group-add`, and `--chown`.
- Images and registries: manifests, configs, layer tarballs, and registry HTTP responses from `docker pull`, and image tarballs for `docker load`.
  Layer extraction that writes outside its target is in scope, and so is copying image content into `VOLUME` volumes.
  Accepting content that doesn't match its digest is in scope.
- Build inputs: Dockerfiles, build contexts, remote Git and URL contexts, `ADD` URLs, and BuildKit frontends.
  Escaping a build container or writing outside the build target is in scope.
- Traffic from containers and remote hosts: the embedded DNS resolver, userland proxy, and iptables and nftables rules for isolation and port publishing.
  Bypasses include reaching unpublished ports from blocked networks, crossing isolated networks, or reaching localhost-bound ports from remote hosts.
  Allowed traffic is not a vulnerability, for example between containers on a bridge with inter-container communication enabled.
- Container logs parsed by logging drivers.
- Swarm nodes without valid join tokens or certificates.
- Swarm workers: workers receive only the task data they are authorized to access, but are trusted participants in NetworkDB gossip.
  NetworkDB does not protect against malicious authenticated gossip participants.
  Reading secrets, configs, or task information of other nodes, or gaining manager privileges, is in scope.
  A worker is still a full daemon with a Swarm agent: its Swarm permissions are limited, but the rest of this document applies to it unchanged.
- Local unprivileged host users: they must not traverse or read the data root (`/var/lib/docker`), `/run/docker`, or container filesystems, run programs from them, or connect to sockets they have no permission for.

## Container isolation

By default, a container is isolated from the host and from other containers.
It has its own filesystem view and its own mount, PID, network, IPC, and UTS namespaces.
It gets a reduced capability set, a minimal set of devices, masked and read-only `/proc` and `/sys` paths, and AppArmor or SELinux confinement where enabled.
It gets the default seccomp profile when the kernel supports seccomp, unless daemon configuration overrides it, including by disabling seccomp.
It can't see or change host files, processes, devices, or network interfaces beyond what the client gives it.
Clients can weaken this on purpose, for example by bind-mounting host paths, starting privileged containers, adding capabilities, or sharing host namespaces.
Bind-mounting any host path changes the container’s isolation boundary, even with read-only access; Moby passes the requested mount options to the kernel, which is responsible for enforcing them.
Isolation weaker than what the client asked for is in scope, even without a working escape.
Without extra options, that means the applicable defaults above.
For example: required seccomp or AppArmor profiles missing, capabilities not dropped, masked or read-only paths skipped, a wrong SELinux label, or no-new-privileges dropped.

Rootless containers must stay isolated too: escaping to the daemon's host user is in scope, even without gaining host root.
With user-namespace remapping (`--userns-remap`), escaping to an unmapped host user, such as root, is in scope.

Under [Microsoft's security servicing criteria](https://www.microsoft.com/en-us/msrc/windows-security-servicing-criteria), process-isolated Windows Server containers are not a security boundary against hostile tenants; Hyper-V isolated containers are covered by the virtual machine boundary.
Normal Windows security boundaries still apply to unprivileged container users, including protection against elevation to administrator.
Bugs in Moby's own Windows code, such as path handling in `docker cp` or layer extraction, or permissions on the API named pipe, are in scope.

## Authorization plugins

An AuthZ plugin can restrict trusted clients, but what it restricts is up to the plugin.
The daemon guarantees that:

- every HTTP API request (not gRPC calls) goes through the active AuthZ plugin,
- it doesn't bypass the plugin's decision,
- it doesn't process a request differently from what it shows the plugin in a way that undermines the decision.

Breaking these guarantees is a vulnerability.
A plugin interpreting a request incorrectly is not, by itself, a vulnerability in Moby.
For example, the plugin may parse JSON differently from Go; implementing its policy correctly is up to the plugin.
A client using requests the plugin allows to gain privileges on the host, such as bind-mounting a host path or starting a privileged container, is not a vulnerability either.

AuthZ plugins can inspect only JSON request and response bodies.
For connection upgrades, plugins check the initial HTTP requests, not the later stream.
They cannot reliably filter streamed or partly buffered response bodies.
See the [AuthZ architecture and limitations](https://docs.docker.com/engine/extend/plugins_authorization/#basic-architecture).

## Components

Highest priority:

- Container isolation: OCI spec generation, seccomp, AppArmor, capabilities, devices, mounts, and exec.
- Container filesystem operations: archive handling, layers and images, and pulls.
- API server, routing, and request validation.
- Networking.

Lower priority, but still in scope:

- Bugs in the Go client or API types that a malicious daemon or API endpoint can exploit to compromise the calling program or its host.
- Swarm mode.
- Plugin installation and privileges: a plugin must not get more privileges than it declared and the user granted.

Vendored modules from the moby GitHub organization, such as SwarmKit, `go-archive`, `sys`, and `profiles`, are in scope.
Their issues get the priority of the Moby component that uses the affected code.
BuildKit is the exception: report BuildKit issues to BuildKit directly, under its own threat model.
Here, only the Moby-specific BuildKit integration is in scope, such as the executor's AppArmor, user-namespace, and network setup, entitlement handling, and how Moby exposes BuildKit through the Engine API.

Out of scope:

- Other vendored code (for example containerd and `golang.org/x/...` modules).
  Report bugs upstream, unless they're caused by how Moby uses the dependency.
- Bugs in containerd, runc, crun, or the Linux kernel, and the shared-kernel container model itself; stronger isolation needs a VM-based runtime.
  Moby is responsible only for issues caused by its OCI specs or use of these components.
- Malicious or compromised plugins abusing their granted privileges.
  Managed and legacy plugins, AuthZ plugins, logging, volume, and network drivers, and NRI plugins are trusted within those privileges.
  A plugin gaining more privileges than it declared and the user granted is still in scope.
- Attacks that need host root, a Swarm manager, or write access to `daemon.json`, the data root, daemon binaries, or other files writable only by the daemon's user.
- Tests and test helpers, development tooling, and contributed utilities (except rootless scripts).
- [Deprecated components](https://docs.docker.com/engine/deprecated/), except the classic builder.
- Bugs in the Windows kernel or Hyper-V.
  Report these to Microsoft; bugs caused by how Moby uses Windows isolation are still in scope.
- Expected effects of settings documented as insecure, such as `--insecure-registry`, TCP API access without TLS, `--privileged`, `--security-opt seccomp=unconfined`, or bind-mounting `docker.sock` or `containerd.sock` into a container.
  Access to these sockets can grant the corresponding daemon's privileges; a read-only bind mount does not make the API read-only.
  Bypassing the restrictions that remain is still in scope: disabling seccomp doesn't disable mount isolation, capability restrictions, or AppArmor.

## Vulnerabilities and bugs

A bug is a vulnerability only when one of the untrusted inputs above can use it to:

- escape or weaken isolation requested by the client or set by the defaults,
- gain privileges beyond those it was given,
- access the host or other containers without permission,
- leak credentials or other secrets,
- crash the daemon, or cause hangs or resource exhaustion that seriously disrupt its ability to serve requests.

These are also vulnerabilities: bypassing API access checks or the active AuthZ plugin (see [Authorization plugins](#authorization-plugins)), an API client gaining more privileges than the daemon's user has, and a plugin getting more privileges than it declared and the user granted.
Everything else is a regular bug.
Most documentation mistakes are regular bugs, but report incorrect security advice privately if you can show a security impact.
Report regular bugs in public issues as described in `CONTRIBUTING.md`, and report privately when in doubt.

Don't report these as vulnerabilities:

- Findings limited to unit tests, direct calls to internal functions, or unreachable code.
- Denial-of-service reports based only on recovered request-handler panics that don't affect the daemon's ability to serve requests.
- Weaker isolation from privileges the client requested, such as host namespaces or `CAP_SYS_ADMIN`.
- Containers without resource limits using up host CPU, memory, disk, or PIDs; setting limits is up to the client.
  Failing to enforce a requested resource limit is in scope unless the required host features (for example, relevant cgroup controllers) are unavailable to the daemon.
- Malicious binaries, vulnerable libraries, or secrets in images; only images that trick the daemon are in scope.
- Missing isolation between API clients, with or without an AuthZ plugin.
  Images, layers, build cache, volumes, and networks are shared, so an image pulled with one client's credentials is available to all clients.
- Wrong cache matches from tampered `--cache-from` sources; cache sources must be trusted by the user.
- Build steps or frontends reading secrets or SSH agent sockets the client passed to the build.
- Panics or errors in the Go client or API types caused by invalid arguments from the calling program.
- Secrets in registry or plugin errors passed through by the daemon.
  Credentials the daemon itself writes to logs or errors are in scope.
- Outdated dependencies without a reachable, demonstrated impact on Moby.

## Reporting

Report privately to security@docker.com as described in `SECURITY.md`, not in public issues.
Show the security impact and a reachable path in a running `dockerd` (API request, CLI command, container process, image, or network traffic).
For issues in the Go client or API types, show a path from a malicious daemon or API endpoint to a program using the Go client.
For issues in rootless scripts, show a reachable path through the affected script; a running daemon is not required.
