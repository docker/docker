# v1.12.1 (2026-10-30)

* **Bug Fix**: Fix malformed JSON when serializing nil lists or maps in union members, map values, and list elements.

# v1.12.0 (2026-10-27)

* **Feature**: Enable schema-based (de)serialization for this service.
* **Dependency Update**: Update to smithy-go v1.28.7.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.11.0 (2026-10-26)

* **Feature**: Adds support for the OAuth 2.0 Token Exchange grant (RFC 8693), enabling applications to exchange identity provider issued JSON Web Tokens (JWTs) for short-lived OAuth access tokens to access supported AWS services without managing separate AWS credentials.

# v1.10.5 (2026-10-22)

* **Bug Fix**: Update smithy-go to v1.28.6
* **Dependency Update**: Updated to the latest SDK module versions

# v1.10.4 (2026-10-09.2)

* **Dependency Update**: Update to smithy-go v1.28.5.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.10.3 (2026-10-08)

* **Dependency Update**: Update to smithy-go v1.28.4.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.10.2 (2026-10-06)

* No change notes available for this release.

# v1.10.1 (2026-09-24)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.10.0 (2026-09-09)

* **Feature**: Stop registering the `retry.MetricsHeader` middleware in generated clients. The `Amz-Sdk-Request` header is now set by the retry middleware itself.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.9.0 (2026-09-04)

* **Feature**: Stop registering the `spanRetryLoop` middleware in generated clients. The retry loop's tracing span is now opened by the retry middleware itself.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.8.0 (2026-08-31.2)

* **Feature**: Stop registering the `SetCredentialSourceMiddleware` middleware in generated clients. Credential source user agent features are now set when the client's middleware stack is constructed.

# v1.7.1 (2026-08-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.7.0 (2026-08-27)

* **Feature**: Support connection read timeouts in the SDK. This is currently available on an opt-in basis by setting env `AWS_ENABLE_DEFAULT_SOCKET_TIMEOUT_2026=true`.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.6.0 (2026-08-26)

* **Feature**: Stop registering the `ComputeContentLength` middleware in generated clients. `Content-Length` is now set when the request body is set via `SetStream`.
* **Dependency Update**: Update to smithy-go v1.28.0.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.8 (2026-08-25)

* **Dependency Update**: Update to smithy-go v1.27.10.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.7 (2026-08-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.6 (2026-08-14)

* **Dependency Update**: Update to smithy-go v1.27.8.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.5 (2026-08-10)

* **Dependency Update**: Update to smithy-go v1.27.7.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.4 (2026-08-05)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.3 (2026-07-31.2)

* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.27.6 to fix various serde issues in HTTP binding services.

# v1.5.2 (2026-07-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.1 (2026-07-28)

* **Dependency Update**: Update to smithy-go v1.27.5.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.0 (2026-07-21)

* **Feature**: Add an option to clients to disable clock skew
* **Dependency Update**: Updated to the latest SDK module versions

# v1.4.1 (2026-07-13)

* No change notes available for this release.

# v1.4.0 (2026-07-08.2)

* **Feature**: Adds support for OAuth 2.0 token operations in AWS Sign-In, CreateOAuth2TokenWithIAM (client credentials flow), IntrospectOAuth2TokenWithIAM (token inspection), and RevokeOAuth2TokenWithIAM (token revocation).

# v1.3.0 (2026-07-06)

* **Feature**: Add request serialization snapshot tests.

# v1.2.2 (2026-07-01)

* **Bug Fix**: Bump smithy-go to 1.27.3, fix JSON encorder for document.Number, endpoint host label format validation and CBOR union serialization on new serde
* **Dependency Update**: Updated to the latest SDK module versions

# v1.2.1 (2026-06-29)

* No change notes available for this release.

# v1.2.0 (2026-06-10)

* **Feature**: AWS Sign-In now allows customers to control access to the AWS Management Console using resource-based policies. With this release customers can restrict console access based on network perimeters such as VPC IDs, VPC endpoints, and IP addresses.

# v1.1.5 (2026-06-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.1.4 (2026-06-04)

* **Dependency Update**: Update to smithy-go v1.27.1 to fix several union-related deserialization bugs in schema-serde-enabled services.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.1.3 (2026-06-03)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.1.2 (2026-06-02)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.1.1 (2026-05-29)

* **Dependency Update**: Update to smithy-go v1.26.0.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.1.0 (2026-05-28)

* **Feature**: Adding new BDD representation of endpoint ruleset
* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.11 (2026-04-29)

* **Dependency Update**: Update to smithy-go v1.25.1.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.10 (2026-04-17)

* **Dependency Update**: Bump smithy-go to 1.25.0 to support endpointBdd trait
* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.9 (2026-03-26)

* **Bug Fix**: Fix a bug where a recorded clock skew could persist on the client even if the client and server clock ended up realigning.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.8 (2026-03-13)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.7 (2026-03-03)

* **Dependency Update**: Bump minimum Go version to 1.24
* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.6 (2026-02-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.5 (2026-01-09)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.4 (2025-12-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.3 (2025-12-02)

* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.24.0. Notably this version of the library reduces the allocation footprint of the middleware system. We observe a ~10% reduction in allocations per SDK call with this change.

# v1.0.2 (2025-11-25)

* **Bug Fix**: Add error check for endpoint param binding during auth scheme resolution to fix panic reported in #3234

# v1.0.1 (2025-11-19.2)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.0.0 (2025-11-19)

* **Release**: New AWS service client module
* **Feature**: AWS Sign-In manages authentication for AWS services. This service provides secure authentication flows for accessing AWS resources from the console and developer tools. This release adds the CreateOAuth2Token API, which can be used to fetch OAuth2 access tokens and refresh tokens from Sign-In.

