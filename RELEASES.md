# Versioning and Releases

This document details the versioning and release plan for Moby.
This document is a living document, if you want to see a change to releases or
versioning, propose the change by modifying this document.

Since Moby is a critical piece of infrastructure software with a lot of
history, one change that wil not be accepted is one that modifies the backwards
compatability guarantees for existing releases.

The core principal here being that we do not make breaking changes to a released
API. New functionality can be added by bumping the API version, but not at the
expense of changing the old behavior.


## Versioning Scheme

Moby generally follows semantic versioning with `<Major>.<Minor>.<Patch>`.

The major version is bumped to signify a breaking change. In general this will
not ever be bumped since we do not accept breaking API changes.

Minor version is bumped when there is a release which includes new features.

Patch version is bumped for bug-fix only releases.

This repo includes a lot of history, including tags from Docker-CE releases.
These will need to stay in place so as not to break people targetting these old
tags. To find out latest release information see the [support](#support-horizon)
section of this document.

## Releases

There is one release of Moby, `1.x`. New API's can be added, but must not break
existing functionality.

There will be two feature releases per year, roughly 6 months apart, the exact
dates will be determined based on quality rather than timelines.

Each release cycle will be divided in approximately half:

#### Merge Windows

The first half of the cycle is considered the "merge window". This is the only
period in which major code changes, minor changes unrelated to specific bug
fixes, or features will be reviewed or merged.

#### Testing/Release

The second half of the release cycle is for testing and handling the actual
release itself. During this phase, no changes will be merged except those
related to the release itself or bug fixes for the release. Contributors should
not expect timely reviews to PR's  during this time period except for changes
related to the upcoming release.

During this phase, the master branch is effectively locked down until the final
release is cut, at which point a release branch is created and the merge window
for the next release is opened.

*Note*: While bug fixes may be merged during the testing phase, please do not
wait until the testing phase to submit bug fixes. This creates uneccessary
burden on maintainers and is generally frowned upon.

#### Bug fix releases

Releases for bug fixes will be made on a monthly bases  *if* bug fixes are
available. There may be one-off releases may be made for show-stopper bug fixes
or security issues.

### Next Release

The activity for the next release will be tracked in the
[milestones](https://github.com/moby/moby/milestones). If a milestone is missing
or if you think an issue or PR should be included in a milestone, please reach
out to the maintainers.

### Support Horizon

Each release will be supported for a period of 2x the release cycle,
effectively ensuring that at any given time there are two supported versions.
This means we will accept bug reports and bug fixes up until the end of life
date. With the current release cycle of 6 months, this means that each release
will be supported for 1 year.

| Version | Status | Start | End of Life |
|---------|--------|-------|-------------|
| TBD | Pre | TBD | TBD + 1 year |

TODO: Figure out release versioning with the mess of pre-existing versions from
older docker releases and then docker-ce releases.

### Backporting

Backports are community driven. As maintainers, we will try to make sure that
critical bug fixes make it into all active releases, but generally once a merge
window has opened our focus will be on shaping up the next release.

If there are fixes that need to be backported, please let us know ine one of
three ways:

1. Open an issue.
2. Open a PR with the cherry-picked change from master
3. Open a PR with a ported fix

Any supported release can accept backports. If you are cherry-picking a commit,
please use `git cherry-pick -x -s <commit>` to ensure the original commit is
referenced in the commit message and the cherry-pick has a DCO sign-off in
addition to the original commit.

Please fix the bug in master before submitting a backport.

### Public API stability

| Component | Status | Stablized Version |
|-----------|--------|-------------------|
| HTTP API  | Stable | 1.0 |
| Prometheus Metrics | Unstable | _future_ |
| Go Client API | Unstable | _future_ |

#### HTTP API

The primary product of Moby is the HTTP API. The HTTP API will not have any
backwards incompatible changes.
Any breaking changes to the HTTP API are considered bugs and should be fixed in
a patch release.

#### Prometheus Metrics

`dockerd` emits Prometheus-style metrics which relate to the metrics of the
daemon itself (nothing to do with container metrics). These are not considered
stable.

#### Go Client API

While we generally try not to make breaking changes to the Go client, we do not
consider the Go client API stable.
Breaking changes to this API should be detectable at compile time, and it is
recommended to vendor in the specific versions you need.


#### Deprecations

Individual subsystems may implement things based on some value, for example a
generic driver option, or even a driver itself. These must remain stable, but
things may be deprecated. Deprecations only signal to users that they should
migrate off of some feature because it will be removed in the future.

Deprecations must be communicated in [DEPRECATED.md](DEPRECATED.md)
Deprecated features must not be removed for a period of at least 2 release cycles.

Deprecations are not taken lightly and must have a clear migration process.

#### Error/HTTP status codes

Error codes  are not well documented (or may be incorrectly documented), and
there has been no formal verification of these error codes.

Even though error codes (which are not HTTP status 500) should not generally
change, these are not considered stable until documentation has been verified
against reality.

#### Not covered

Unless explicitly calledout in this list it does not provide any stability
guarantees and may change on any release.
A non-exhaustive list includes:

* File system layout
* Storage formats
* Supporting tools like docker-proxy or containerd support.
* Graphdriver support

Between upgrades of subsequent, minor versions, we may migrate these formats.
Any outside processes relying on details of these file system layouts may break
in that process. Container root file systems will be maintained on upgrade.

In general if you think a component is missing from the public API stability
guarantees, make a PR to add it to the list.

#### Exceptions

We may make exceptions in the interest of security patches. If a break is
required, it will be communicated clearly and the solution will be considered
against total impact.
