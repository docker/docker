# v1.7.25 (2026-10-27)

* **Dependency Update**: Update to smithy-go v1.28.7.

# v1.7.24 (2026-10-22)

* **Bug Fix**: Update smithy-go to v1.28.6

# v1.7.23 (2026-10-21)

* **Bug Fix**: Enforce event stream header key value length limit instead of truncating

# v1.7.22 (2026-10-09.2)

* **Dependency Update**: Update to smithy-go v1.28.5.

# v1.7.21 (2026-10-08)

* **Dependency Update**: Update to smithy-go v1.28.4.

# v1.7.20 (2026-08-26)

* **Dependency Update**: Update to smithy-go v1.28.0.

# v1.7.19 (2026-08-25)

* **Dependency Update**: Update to smithy-go v1.27.10.

# v1.7.18 (2026-08-14)

* **Dependency Update**: Update to smithy-go v1.27.8.

# v1.7.17 (2026-08-10)

* **Dependency Update**: Update to smithy-go v1.27.7.

# v1.7.16 (2026-07-31.2)

* **Dependency Update**: Upgrade to smithy-go v1.27.6 to fix various serde issues in HTTP binding services.

# v1.7.15 (2026-07-28)

* **Dependency Update**: Update to smithy-go v1.27.5.

# v1.7.14 (2026-07-01)

* No change notes available for this release.

# v1.7.13 (2026-06-04)

* **Dependency Update**: Update to smithy-go v1.27.1 to fix several union-related deserialization bugs in schema-serde-enabled services.

# v1.7.12 (2026-06-03)

* No change notes available for this release.

# v1.7.11 (2026-05-29)

* **Dependency Update**: Update to smithy-go v1.26.0.

# v1.7.10 (2026-04-29)

* **Dependency Update**: Update to smithy-go v1.25.1.

# v1.7.9 (2026-04-17)

* **Dependency Update**: Bump smithy-go to 1.25.0 to support endpointBdd trait

# v1.7.8 (2026-03-23)

* No change notes available for this release.

# v1.7.7 (2026-03-13)

* **Bug Fix**: Replace usages of the old ioutil/ package throughout the SDK.

# v1.7.6 (2026-03-03)

* **Bug Fix**: Modernize non codegen files with go fix
* **Dependency Update**: Bump minimum Go version to 1.24

# v1.7.5 (2026-02-23)

* No change notes available for this release.

# v1.7.4 (2025-12-02)

* **Dependency Update**: Upgrade to smithy-go v1.24.0. Notably this version of the library reduces the allocation footprint of the middleware system. We observe a ~10% reduction in allocations per SDK call with this change.

# v1.7.3 (2025-11-04)

* **Dependency Update**: Upgrade to smithy-go v1.23.2 which should convey some passive reduction of overall allocations, especially when not using the metrics system.

# v1.7.2 (2025-10-16)

* **Dependency Update**: Bump minimum Go version to 1.23.

# v1.7.1 (2025-08-27)

* **Dependency Update**: Update to smithy-go v1.23.0.

# v1.7.0 (2025-07-28)

* **Feature**: Add support for HTTP interceptors.

# v1.6.11 (2025-06-17)

* **Dependency Update**: Update to smithy-go v1.22.4.

# v1.6.10 (2025-02-18)

* **Bug Fix**: Bump go version to 1.22

# v1.6.9 (2025-02-14)

* **Bug Fix**: Remove max limit on event stream messages

# v1.6.8 (2025-01-24)

* **Dependency Update**: Upgrade to smithy-go v1.22.2.

# v1.6.7 (2024-11-18)

* **Dependency Update**: Update to smithy-go v1.22.1.

# v1.6.6 (2024-10-04)

* No change notes available for this release.

# v1.6.5 (2024-09-20)

* No change notes available for this release.

# v1.6.4 (2024-08-15)

* **Dependency Update**: Bump minimum Go version to 1.21.

# v1.6.3 (2024-06-28)

* No change notes available for this release.

# v1.6.2 (2024-03-29)

* No change notes available for this release.

# v1.6.1 (2024-02-21)

* No change notes available for this release.

# v1.6.0 (2024-02-13)

* **Feature**: Bump minimum Go version to 1.20 per our language support policy.

# v1.5.4 (2023-12-07)

* No change notes available for this release.

# v1.5.3 (2023-11-30)

* No change notes available for this release.

# v1.5.2 (2023-11-29)

* No change notes available for this release.

# v1.5.1 (2023-11-15)

* No change notes available for this release.

# v1.5.0 (2023-10-31)

* **Feature**: **BREAKING CHANGE**: Bump minimum go version to 1.19 per the revised [go version support policy](https://aws.amazon.com/blogs/developer/aws-sdk-for-go-aligns-with-go-release-policy-on-supported-runtimes/).

# v1.4.14 (2023-10-06)

* No change notes available for this release.

# v1.4.13 (2023-08-18)

* No change notes available for this release.

# v1.4.12 (2023-08-07)

* No change notes available for this release.

# v1.4.11 (2023-07-31)

* No change notes available for this release.

# v1.4.10 (2022-12-02)

* No change notes available for this release.

# v1.4.9 (2022-10-24)

* No change notes available for this release.

# v1.4.8 (2022-09-14)

* No change notes available for this release.

# v1.4.7 (2022-09-02)

* No change notes available for this release.

# v1.4.6 (2022-08-31)

* No change notes available for this release.

# v1.4.5 (2022-08-29)

* No change notes available for this release.

# v1.4.4 (2022-08-09)

* No change notes available for this release.

# v1.4.3 (2022-06-29)

* No change notes available for this release.

# v1.4.2 (2022-06-07)

* No change notes available for this release.

# v1.4.1 (2022-03-24)

* No change notes available for this release.

# v1.4.0 (2022-03-08)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version

# v1.3.0 (2022-02-24)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version

# v1.2.0 (2022-01-14)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version

# v1.1.0 (2022-01-07)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version

# v1.0.0 (2021-11-06)

* **Announcement**: Support has been added for AWS EventStream APIs for Kinesis, S3, and Transcribe Streaming. Support for the Lex Runtime V2 EventStream API will be added in a future release.
* **Release**: Protocol support has been added for AWS event stream.
* **Feature**: Updated `github.com/aws/smithy-go` to latest version

