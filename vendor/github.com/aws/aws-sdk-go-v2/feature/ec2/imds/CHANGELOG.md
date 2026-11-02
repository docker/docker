# v1.20.5 (2026-10-27)

* **Dependency Update**: Update to smithy-go v1.28.7.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.4 (2026-10-22)

* **Bug Fix**: Update smithy-go to v1.28.6
* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.3 (2026-10-09.2)

* **Dependency Update**: Update to smithy-go v1.28.5.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.2 (2026-10-08)

* **Dependency Update**: Update to smithy-go v1.28.4.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.1 (2026-09-24)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.0 (2026-09-09)

* **Feature**: Stop registering the `retry.MetricsHeader` middleware in generated clients. The `Amz-Sdk-Request` header is now set by the retry middleware itself.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.19.2 (2026-09-04)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.19.1 (2026-08-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.19.0 (2026-08-27)

* **Feature**: Support connection read timeouts in the SDK. This is currently available on an opt-in basis by setting env `AWS_ENABLE_DEFAULT_SOCKET_TIMEOUT_2026=true`.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.40 (2026-08-26)

* **Dependency Update**: Update to smithy-go v1.28.0.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.39 (2026-08-25)

* **Dependency Update**: Update to smithy-go v1.27.10.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.38 (2026-08-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.37 (2026-08-14)

* **Dependency Update**: Update to smithy-go v1.27.8.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.36 (2026-08-10)

* **Dependency Update**: Update to smithy-go v1.27.7.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.35 (2026-08-05)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.34 (2026-07-31.2)

* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.27.6 to fix various serde issues in HTTP binding services.

# v1.18.33 (2026-07-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.32 (2026-07-28)

* **Dependency Update**: Update to smithy-go v1.27.5.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.31 (2026-07-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.30 (2026-07-01)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.29 (2026-06-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.28 (2026-06-04)

* **Dependency Update**: Update to smithy-go v1.27.1 to fix several union-related deserialization bugs in schema-serde-enabled services.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.27 (2026-06-03)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.26 (2026-06-02)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.25 (2026-05-29)

* **Dependency Update**: Update to smithy-go v1.26.0.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.24 (2026-05-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.23 (2026-04-29)

* **Dependency Update**: Update to smithy-go v1.25.1.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.22 (2026-04-17)

* **Dependency Update**: Bump smithy-go to 1.25.0 to support endpointBdd trait
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.21 (2026-03-26)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.20 (2026-03-13)

* **Bug Fix**: Replace usages of the old ioutil/ package throughout the SDK.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.19 (2026-03-03)

* **Bug Fix**: Modernize non codegen files with go fix
* **Dependency Update**: Bump minimum Go version to 1.24
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.18 (2026-02-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.17 (2026-01-09)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.16 (2025-12-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.15 (2025-12-02)

* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.24.0. Notably this version of the library reduces the allocation footprint of the middleware system. We observe a ~10% reduction in allocations per SDK call with this change.

# v1.18.14 (2025-11-19.2)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.13 (2025-11-04)

* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.23.2 which should convey some passive reduction of overall allocations, especially when not using the metrics system.

# v1.18.12 (2025-10-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.11 (2025-10-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.10 (2025-10-16)

* **Dependency Update**: Bump minimum Go version to 1.23.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.9 (2025-09-26)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.8 (2025-09-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.7 (2025-09-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.6 (2025-08-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.5 (2025-08-27)

* **Dependency Update**: Update to smithy-go v1.23.0.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.4 (2025-08-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.3 (2025-08-11)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.2 (2025-08-04)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.1 (2025-07-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.18.0 (2025-07-29)

* **Feature**: Add config switch `DisableDefaultMaxBackoff` that allows you to disable the default maximum backoff (1 second) for IMDS calls retry attempt

# v1.17.0 (2025-07-28)

* **Feature**: Add support for HTTP interceptors.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.33 (2025-07-19)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.32 (2025-06-17)

* **Dependency Update**: Update to smithy-go v1.22.4.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.31 (2025-06-10)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.30 (2025-02-27)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.29 (2025-02-18)

* **Bug Fix**: Bump go version to 1.22
* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.28 (2025-02-05)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.27 (2025-01-31)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.26 (2025-01-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.25 (2025-01-24)

* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.22.2.

# v1.16.24 (2025-01-15)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.23 (2025-01-09)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.22 (2024-12-19)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.21 (2024-12-02)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.20 (2024-11-18)

* **Dependency Update**: Update to smithy-go v1.22.1.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.19 (2024-11-06)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.18 (2024-10-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.17 (2024-10-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.16 (2024-10-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.15 (2024-10-04)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.14 (2024-09-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.13 (2024-09-03)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.12 (2024-08-15)

* **Dependency Update**: Bump minimum Go version to 1.21.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.11 (2024-07-10.2)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.10 (2024-07-10)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.9 (2024-06-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.8 (2024-06-19)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.7 (2024-06-18)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.6 (2024-06-17)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.5 (2024-06-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.4 (2024-06-03)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.3 (2024-05-16)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.2 (2024-05-15)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.1 (2024-03-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.16.0 (2024-03-21)

* **Feature**: Add config switch `DisableDefaultTimeout` that allows you to disable the default operation timeout (5 seconds) for IMDS calls.

# v1.15.4 (2024-03-18)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.3 (2024-03-07)

* **Bug Fix**: Remove dependency on go-cmp.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.2 (2024-02-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.1 (2024-02-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.0 (2024-02-13)

* **Feature**: Bump minimum Go version to 1.20 per our language support policy.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.11 (2024-01-04)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.10 (2023-12-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.9 (2023-12-01)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.8 (2023-11-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.7 (2023-11-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.6 (2023-11-28.2)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.5 (2023-11-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.4 (2023-11-15)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.3 (2023-11-09)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.2 (2023-11-02)

* No change notes available for this release.

# v1.14.1 (2023-11-01)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.0 (2023-10-31)

* **Feature**: **BREAKING CHANGE**: Bump minimum go version to 1.19 per the revised [go version support policy](https://aws.amazon.com/blogs/developer/aws-sdk-for-go-aligns-with-go-release-policy-on-supported-runtimes/).
* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.13 (2023-10-12)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.12 (2023-10-06)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.11 (2023-08-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.10 (2023-08-18)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.9 (2023-08-17)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.8 (2023-08-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.7 (2023-07-31)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.6 (2023-07-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.5 (2023-07-13)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.4 (2023-06-13)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.3 (2023-04-24)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.2 (2023-04-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.1 (2023-03-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.0 (2023-03-14)

* **Feature**: Add flag to disable IMDSv1 fallback

# v1.12.24 (2023-03-10)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.23 (2023-02-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.22 (2023-02-03)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.21 (2022-12-15)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.20 (2022-12-02)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.19 (2022-10-24)

* **Bug Fix**: Fixes an issue that prevented logging of the API request or responses when the respective log modes were enabled.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.18 (2022-10-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.17 (2022-09-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.16 (2022-09-14)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.15 (2022-09-02)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.14 (2022-08-31)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.13 (2022-08-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.12 (2022-08-11)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.11 (2022-08-09)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.10 (2022-08-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.9 (2022-08-01)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.8 (2022-07-05)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.7 (2022-06-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.6 (2022-06-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.5 (2022-05-17)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.4 (2022-04-25)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.3 (2022-03-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.2 (2022-03-24)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.1 (2022-03-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.0 (2022-03-08)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.11.0 (2022-02-24)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.10.0 (2022-01-14)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.9.0 (2022-01-07)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.8.2 (2021-12-02)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.8.1 (2021-11-19)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.8.0 (2021-11-06)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.7.0 (2021-10-21)

* **Feature**: Updated  to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.6.0 (2021-10-11)

* **Feature**: Respect passed in Context Deadline/Timeout. Updates the IMDS Client operations to not override the passed in Context's Deadline or Timeout options. If an Client operation is called with a Context with a Deadline or Timeout, the client will no longer override it with the client's default timeout.
* **Bug Fix**: Fix IMDS client's response handling and operation timeout race. Fixes #1253
* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.1 (2021-09-17)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.0 (2021-08-27)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.4.1 (2021-08-19)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.4.0 (2021-08-04)

* **Feature**: adds error handling for defered close calls
* **Dependency Update**: Updated `github.com/aws/smithy-go` to latest version.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.3.0 (2021-07-15)

* **Feature**: Support has been added for EC2 IPv6-enabled Instance Metadata Service Endpoints.
* **Dependency Update**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.2.0 (2021-06-25)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.1.1 (2021-05-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.1.0 (2021-05-14)

* **Feature**: Constant has been added to modules to enable runtime version inspection for reporting.
* **Dependency Update**: Updated to the latest SDK module versions

