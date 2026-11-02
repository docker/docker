# 🚀 Releases

This document outlines the Moby project’s release process, including cadence and versioning.

## 📌 Versioning

Releases of Moby modules will be versioned using the dotted triples [Semantic Version](http://semver.org/)
format. For the purposes of this document, we will refer to the respective
components of this triple as `<major>.<minor>.<patch>-[suffix[N]]`. The version
number may have additional information, such as alpha, beta and release candidate
qualifications along with a numeric identifier. Such releases will be considered
"pre-releases".

> [!NOTE]
> v17.03-v20.10 releases were following [CalVer](https://calver.org/).
> v2.0.0 introduces the `github.com/moby/moby/v2` go module after the
> previous v28 release of the `github.com/docker/docker` pseudo-module.

## ⏳ Release cadence

The project will release as quickly as possible while ensuring stability and
timely feature delivery. Maintainers curate a feature roadmap and set milestones
for each release.

Release dates assigned to milestones are commitments, and any changes will be
announced to the community via an
[announcement discussion](https://github.com/moby/moby/discussions/categories/announcements).
To meet deadlines, some features may be deferred to future releases.

If your issue or feature is missing from the roadmap, please open a GitHub issue
or comment to request its inclusion.

## 🎯 Major and minor releases

Major and minor releases of moby will be made from main. Releases will be marked
with GPG signed tags and posted at https://github.com/moby/moby/releases. The
tag will be of the format `v<major>.<minor>.<patch>-[suffix[N]]`.

After a minor release of the main module, a branch will be created, with the format
`release/<major>.<minor>` from the minor tag. All further patch releases will be
done from that branch. For example, after the release of `v2.1.0`, a branch
`release/2.1` would be created from that tag. All future patch releases will be
done against that branch.

## 🛠️ Patch releases

Patch releases are made directly from release branches and will be done as needed
by the release branch owners.

> [!IMPORTANT] 
> Security releases are also "patch releases", but follow a
> different procedure. Security releases may be developed in a private repository,
> released and tested under embargo before they become publicly available.

## 🎬 Pre-releases

Pre-releases, such as alphas, betas and release candidates will be conducted
from their source branch. For major and minor releases, these releases will be
done from `master`. For patch releases, it is uncommon to have pre-releases but
they may have an `rc` based on the discretion of the release branch owners.

## 🏗️ Modules

The Moby project is a collection of modules, each with its own versioning but with
the same minor release cadence.

- `github.com/moby/moby/v2`: The main module, which contains the core components of Moby.
  This module is used to build the main daemon is not intended for direct use by clients.
- `github.com/moby/moby/api`: The API module, which contains the API types and documentation.
- `github.com/moby/moby/client`: The client module, which contains the client library for
  interacting with the Moby API.

## Support Horizon

Support horizons will be defined corresponding to a release branch, identified
by `<major>.<minor>`. Release branches will be in one of several states:

- __*Future*__: An upcoming scheduled release.
- __*Alpha*__: The next scheduled release on the main branch under active development.
- __*Beta*__: The next scheduled release on the main branch under testing. Begins 2-4 weeks before a final release.
- __*RC*__: The next scheduled release on the main branch under final testing and stabilization. Begins 1 week before a final release. For new releases where the source branch is main, the main branch will be in a feature freeze during this phase.
- __*Active*__: The release is a stable branch which is currently supported and accepting patches.
- __*Extended*__: The release branch is only accepting security patches.
- __*End of Life*__: The release branch is no longer supported and no new patches will be accepted.

Releases are supported until at least the next _minor_ release. Any minor release
may be supported longer if a maintainer elects to be the owner. The owners of a
release branch may decide whether to support it as an active release accepting a wider
range of bug fixes or as an extended security release which only receive security
related fixes.

### Current Releases

| Main module | API     | Client | Status   | Release date | End of life | Owner(s) |
|-------------|---------|--------|----------|--------------|-------------|----------|
| v2.0        | v0.1.52 | v0.1.0 | Alpha    | 2025-08-dd   |             |          |
| v2.1        | v1.x    | v1.x   | *Future* |              |             |          |

### Pre-modules releases branches

| Release Branch | Status      | End of Life  | Owner(s)         |
|----------------|-------------|--------------|------------------|
| 28.x           | Active      | v2.0 release | @moby-committers |
| 27.x           | End of Life |              |                  |
| 26.1           | End of Life |              |                  |
| 26.0           | End of Life |              |                  |
| 25.0           | Active      | See [mcr25]  | @corhere         |
| 24.0           | End of Life |              |                  |
| 23.0           | Extended    | See [mcr23]  | @corhere         |

[mcr25]: https://docs.mirantis.com/mcr/25.0/compat-matrix/maintenance-lifecycle.html
[mcr23]: https://docs.mirantis.com/mcr/23.0/compat-matrix/maintenance-lifecycle.html

## API Stability

The following table provides an overview of the components covered by
Moby versions and the stability guarantees provided to users and importers.

| Component        | Status        | Stabilized Version | Links |
|------------------|---------------|--------------------|-------|
| HTTP API         | Stable        | 1.0                | [Docker Engine API](https://docs.docker.com/reference/api/engine/) |
| API Go module    | *In Progress* | 1.x                | [`github.com/moby/moby/api`](https://pkg.go.dev/github.com/moby/moby/api) |
| Client Go module | *In Progress* | 1.0                | [`github.com/moby/moby/client`](https://pkg.go.dev/github.com/moby/moby/client) |
| Daemon Config    | Stable        | 1.0                | [Daemon configuration file](https://docs.docker.com/reference/cli/dockerd/#daemon-configuration-file) |
| Main Go Module   | Unstable      | Unsupported        | [`github.com/moby/moby/v2`](https://pkg.go.dev/github.com/moby/moby/v2) |

> NOTE: While the daemon's HTTP API and configuration file are considered stable, the Go code within the main module
> is not considered stable. The `github.com/moby/moby/api` and `githhub.com/moby/moby/client` modules are intended
> to be the stable modules which are supported for import by other projects. It is recommended to use these stable
> modules directly or another stable Go library for working with the daemon, importing the `github.com/moby/moby/v2`
> module directly (or indirectly) is not recommended and may have incompatible changes between minor releases.
