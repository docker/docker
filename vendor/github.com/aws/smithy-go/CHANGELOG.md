# Release (2026-10-27)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.28.7
  * **Bug Fix**: Fix a panic or `EOF` error when deserializing a JSON union that contains a member not modeled by the client and whose value is not `null`.
  * **Bug Fix**: Log the underlying error when dumping an HTTP request or response for logging fails

# Release (2026-10-21)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.28.6
  * **Bug Fix**: Enforce event stream header key value length limit instead of truncating
  * **Bug Fix**: Retain the value in a list with the zero value when encountering a null entry on a dense list. This matches how legacy code-generated JSON deserializers behaved.

# Release (2026-10-09)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.28.5
  * **Bug Fix**: Retain the key with the zero value when encountering a null-value key in a dense map. This matches how legacy code-generated JSON deserializers behaved.

# Release (2026-10-07)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.28.4
  * **Bug Fix**: Revert #698 that fixed JoinPath with trailing slash. This has a side effect on certain S3 API paths in the downstream SDK that need more consideration.

# Release (2026-10-06)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.28.3
  * **Bug Fix**: Accept either casing of `message`/`Message` on modeled JSON error structures in schema-based deserialization, and emit the `smithy.api#error` trait in generated schemas
  * **Bug Fix**: Include the underlying error when logging failures to discard or close an HTTP response body

# Release (2026-09-18)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.28.2
  * **Bug Fix**: Avoid allocating a slice in ValidateEndpointHost by indexing into the hostname instead of using strings.Split.
  * **Bug Fix**: Fix event stream decode panic on unknown header value type.
  * **Bug Fix**: Fix schema-serde CBOR decode panic on malformed string/blob length.
  * **Bug Fix**: JoinPath keeps a trailing slash when the added path is a slash.

# Release (2026-08-26)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.28.1
  * **Bug Fix**: Fix broken AddLogger middleware since its insert point was removed.

# Release (2026-08-25.2)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.28.0
  * **Feature**: Set `Content-Length` inline when the request body is set via `SetStream`. The `ComputeContentLength` middleware is now deprecated.

# Release (2026-08-25)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.10
  * **Bug Fix**: Fix a data race on the underlying writer when an event stream is closed while an event write is in flight.
  * **Bug Fix**: Restore draining the HTTP response body in `CloseResponseBody` to avoid issues with TCP connection reuse.

# Release (2026-08-21)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.9
  * **Bug Fix**: Fix a generic event stream exception not carrying the error code and message from its payload.
  * **Bug Fix**: Fix an event stream not being closed when its connection is lost, which would cause a caller writing to the stream to block indefinitely.
  * **Bug Fix**: Fix deserialization of an empty list producing a nil slice instead of an empty one.
  * **Bug Fix**: Restore draining the HTTP response body in `CloseResponseBody` to avoid issues with TCP connection reuse.

# Release (2026-08-14)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.8
  * **Bug Fix**: Restore draining the HTTP response body in `CloseResponseBody` to avoid issues with TCP connection reuse.

# Release (2026-08-07)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.7
  * **Bug Fix**: Don't serialize unset JSON documents as `nil` in structure members.
  * **Bug Fix**: Fix a deserialization panic around collection members in recursive shape configs.

# Release (2026-07-31)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.6
  * **Bug Fix**: Fix failure to deserialize any `@httpPayload` struct with a non-string member.
  * **Bug Fix**: Fix failure to serialize any `@httpPayload` struct with a nested struct.

# Release (2026-07-27)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.5
  * **Bug Fix**: Fix a performance issue in awsQuery with large response payloads.

# Release (2026-07-16)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go/aws-http-auth`: [v1.2.1](aws-http-auth/CHANGELOG.md#v121-2026-07-16)
  * **Bug Fix**: Use r.URL.Host when r.Host is unset.
* `github.com/aws/smithy-go/aws-http-auth-schemes`: [v1.0.0](aws-http-auth-schemes/CHANGELOG.md#v100-2026-07-16)
  * **Release**: Module `github.com/aws/smithy-go/aws-http-auth-schemes` adds generic smithy-go client support for AWS Sigv4 and Sigv4a.

# Release (2026-06-26)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.3
  * **Bug Fix**: Fix bug in JSON doc encoder and endpoint host label format validation

# Release (2026-06-05)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.2
  * **Bug Fix**: Fix incorrect serialization of unions in CBOR-based protocols.

# Release (2026-06-04)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.1
  * **Bug Fix**: Fixed a deserialization failure in all protocols when encountering a union with explicit null members.
  * **Bug Fix**: Fixed a panic when deserializing nested unions in JSON- and CBOR-based protocols.

# Release (2026-06-02)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.27.0
  * **Feature**: Add APIs for schema-based serialization.
  * **Feature**: Add support for all current AWS and Smithy protocols.
  * **Bug Fix**: Enforce max nesting depth of 128 on CBOR payloads.
* `github.com/aws/smithy-go/aws-http-auth`: [v1.2.0](aws-http-auth/CHANGELOG.md#v120-2026-06-02)
  * **Feature**: Add event stream signer.

# Release (2026-05-27)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.26.0
  * **Feature**: Add StringSlice to endpoint rulesfn.

# Release (2026-04-23)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.25.1
  * **Bug Fix**: Fixed a memory leak in the LRU cache implementation used by some AWS services.

# Release (2026-04-15)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.25.0
  * **Feature**: Add support for endpointBdd trait

# Release (2026-04-02)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.24.3
  * **Bug Fix**: Add additional sigv4 configuration.
* `github.com/aws/smithy-go/aws-http-auth`: [v1.1.3](aws-http-auth/CHANGELOG.md#v113-2026-04-02)
  * **Bug Fix**: Add additional sigv4 configuration.

# Release (2026-02-27)

## General Highlights
* **Dependency Update**: Bump minimum go version to 1.24.

# Release (2026-02-20)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.24.1
  * **Feature**: Add new middleware functions to get event stream output from middleware

# Release (2025-12-01)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.24.0
  * **Feature**: Improve allocation footprint of the middleware stack. This should convey a ~10% reduction in allocations per SDK request.

# Release (2025-11-03)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.23.2
  * **Bug Fix**: Adjust the initial sizes of each middleware phase to avoid some unnecessary reallocation.
  * **Bug Fix**: Avoid unnecessary allocation overhead from the metrics system when not in use.

# Release (2025-10-15)

## General Highlights
* **Dependency Update**: Bump minimum go version to 1.23.
* **Dependency Update**: Updated to the latest SDK module versions

# Release (2025-09-18)

## Module Highlights
* `github.com/aws/smithy-go/aws-http-auth`: [v1.1.0](aws-http-auth/CHANGELOG.md#v110-2025-09-18)
  * **Feature**: Added support for SIG4/SIGV4A querystring authentication.

# Release (2025-08-27)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.23.0
  * **Feature**: Sort map keys in JSON Document types.

# Release (2025-07-24)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.22.5
  * **Feature**: Add HTTP interceptors.

# Release (2025-06-16)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.22.4
  * **Bug Fix**: Fix CBOR serd empty check for string and enum fields
  * **Bug Fix**: Fix HTTP metrics data race.
  * **Bug Fix**: Replace usages of deprecated ioutil package.

# Release (2025-02-17)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.22.3
 *  **Dependency Update**: Bump minimum Go version to 1.22 per our language support policy.

# Release (2025-01-21)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.22.2
  * **Bug Fix**: Fix HTTP metrics data race.
  * **Bug Fix**: Replace usages of deprecated ioutil package.

# Release (2024-11-15)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.22.1
  * **Bug Fix**: Fix failure to replace URI path segments when their names overlap.

# Release (2024-10-03)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.22.0
  * **Feature**: Add HTTP client metrics.

# Release (2024-09-25)

## Module Highlights
* `github.com/aws/smithy-go/aws-http-auth`: [v1.0.0](aws-http-auth/CHANGELOG.md#v100-2024-09-25)
  * **Release**: Initial release of module aws-http-auth, which implements generically consumable SigV4 and SigV4a request signing.

# Release (2024-09-19)

## General Highlights
* **Dependency Update**: Updated to the latest SDK module versions

## Module Highlights
* `github.com/aws/smithy-go`: v1.21.0
  * **Feature**: Add tracing and metrics APIs, and builtin instrumentation for both, in generated clients.
* `github.com/aws/smithy-go/metrics/smithyotelmetrics`: [v1.0.0](metrics/smithyotelmetrics/CHANGELOG.md#v100-2024-09-19)
  * **Release**: Initial release of `smithyotelmetrics` module, which is used to adapt an OpenTelemetry SDK meter provider to be used with Smithy clients.
* `github.com/aws/smithy-go/tracing/smithyoteltracing`: [v1.0.0](tracing/smithyoteltracing/CHANGELOG.md#v100-2024-09-19)
  * **Release**: Initial release of `smithyoteltracing` module, which is used to adapt an OpenTelemetry SDK tracer provider to be used with Smithy clients.

# Release (2024-08-14)

## Module Highlights
* `github.com/aws/smithy-go`: v1.20.4
  * **Dependency Update**: Bump minimum Go version to 1.21.

# Release (2024-06-27)

## Module Highlights
* `github.com/aws/smithy-go`: v1.20.3
  * **Bug Fix**: Fix encoding/cbor test overflow on x86.

# Release (2024-03-29)

* No change notes available for this release.

# Release (2024-02-21)

## Module Highlights
* `github.com/aws/smithy-go`: v1.20.1
  * **Bug Fix**: Remove runtime dependency on go-cmp.

# Release (2024-02-13)

## Module Highlights
* `github.com/aws/smithy-go`: v1.20.0
  * **Feature**: Add codegen definition for sigv4a trait.
  * **Feature**: Bump minimum Go version to 1.20 per our language support policy.

# Release (2023-12-07)

## Module Highlights
* `github.com/aws/smithy-go`: v1.19.0
  * **Feature**: Support modeled request compression.

# Release (2023-11-30)

* No change notes available for this release.

# Release (2023-11-29)

## Module Highlights
* `github.com/aws/smithy-go`: v1.18.0
  * **Feature**: Expose Options() method on generated service clients.

# Release (2023-11-15)

## Module Highlights
* `github.com/aws/smithy-go`: v1.17.0
  * **Feature**: Support identity/auth components of client reference architecture.

# Release (2023-10-31)

## Module Highlights
* `github.com/aws/smithy-go`: v1.16.0
  * **Feature**: **LANG**: Bump minimum go version to 1.19.

# Release (2023-10-06)

## Module Highlights
* `github.com/aws/smithy-go`: v1.15.0
  * **Feature**: Add `http.WithHeaderComment` middleware.

# Release (2023-08-18)

* No change notes available for this release.

# Release (2023-08-07)

## Module Highlights
* `github.com/aws/smithy-go`: v1.14.1
  * **Bug Fix**: Prevent duplicated error returns in EndpointResolverV2 default implementation.

# Release (2023-07-31)

## General Highlights
* **Feature**: Adds support for smithy-modeled endpoint resolution.

# Release (2022-12-02)

* No change notes available for this release.

# Release (2022-10-24)

## Module Highlights
* `github.com/aws/smithy-go`: v1.13.4
  * **Bug Fix**: fixed document type checking for encoding nested types

# Release (2022-09-14)

* No change notes available for this release.

# Release (v1.13.2)

* No change notes available for this release.

# Release (v1.13.1)

* No change notes available for this release.

# Release (v1.13.0)

## Module Highlights
* `github.com/aws/smithy-go`: v1.13.0
  * **Feature**: Adds support for the Smithy httpBearerAuth authentication trait to smithy-go. This allows the SDK to support the bearer authentication flow for API operations decorated with httpBearerAuth. An API client will need to be provided with its own bearer.TokenProvider implementation or use the bearer.StaticTokenProvider implementation.

# Release (v1.12.1)

## Module Highlights
* `github.com/aws/smithy-go`: v1.12.1
  * **Bug Fix**: Fixes a bug where JSON object keys were not escaped.

# Release (v1.12.0)

## Module Highlights
* `github.com/aws/smithy-go`: v1.12.0
  * **Feature**: `transport/http`: Add utility for setting context metadata when operation serializer automatically assigns content-type default value.

# Release (v1.11.3)

## Module Highlights
* `github.com/aws/smithy-go`: v1.11.3
  * **Dependency Update**: Updates smithy-go unit test dependency go-cmp to 0.5.8.

# Release (v1.11.2)

* No change notes available for this release.

# Release (v1.11.1)

## Module Highlights
* `github.com/aws/smithy-go`: v1.11.1
  * **Bug Fix**: Updates the smithy-go HTTP Request to correctly handle building the request to an http.Request. Related to [aws/aws-sdk-go-v2#1583](https://github.com/aws/aws-sdk-go-v2/issues/1583)

# Release (v1.11.0)

## Module Highlights
* `github.com/aws/smithy-go`: v1.11.0
  * **Feature**: Updates deserialization of header list to supported quoted strings

# Release (v1.10.0)

## Module Highlights
* `github.com/aws/smithy-go`: v1.10.0
  * **Feature**: Add `ptr.Duration`, `ptr.ToDuration`, `ptr.DurationSlice`, `ptr.ToDurationSlice`, `ptr.DurationMap`, and `ptr.ToDurationMap` functions for the `time.Duration` type.

# Release (v1.9.1)

## Module Highlights
* `github.com/aws/smithy-go`: v1.9.1
  * **Documentation**: Fixes various typos in Go package documentation.

# Release (v1.9.0)

## Module Highlights
* `github.com/aws/smithy-go`: v1.9.0
  * **Feature**: sync: OnceErr, can be used to concurrently record a signal when an error has occurred.
  * **Bug Fix**: `transport/http`: CloseResponseBody and ErrorCloseResponseBody middleware have been updated to ensure that the body is fully drained before closing.

# Release v1.8.1

### Smithy Go Module
* **Bug Fix**: Fixed an issue that would cause the HTTP Content-Length to be set to 0 if the stream body was not set.
  * Fixes [aws/aws-sdk-go-v2#1418](https://github.com/aws/aws-sdk-go-v2/issues/1418)

# Release v1.8.0

### Smithy Go Module

* `time`: Add support for parsing additional DateTime timestamp format ([#324](https://github.com/aws/smithy-go/pull/324))
  * Adds support for parsing DateTime timestamp formatted time similar to RFC 3339, but without the `Z` character, nor UTC offset.
  * Fixes [#1387](https://github.com/aws/aws-sdk-go-v2/issues/1387)

# Release v1.7.0

### Smithy Go Module
* `ptr`:  Handle error for deferred file close call ([#314](https://github.com/aws/smithy-go/pull/314))
  * Handle error for defer close call
* `middleware`: Add Clone to Metadata ([#318](https://github.com/aws/smithy-go/pull/318))
  * Adds a new Clone method to the middleware Metadata type. This provides a shallow clone of the entries in the Metadata.
* `document`: Add new package for document shape serialization support ([#310](https://github.com/aws/smithy-go/pull/310))

### Codegen
* Add Smithy Document Shape Support ([#310](https://github.com/aws/smithy-go/pull/310))
  * Adds support for Smithy Document shapes and supporting types for protocols to implement support

# Release v1.6.0 (2021-07-15)

### Smithy Go Module
* `encoding/httpbinding`: Support has been added for encoding `float32` and `float64` values that are `NaN`, `Infinity`, or `-Infinity`. ([#316](https://github.com/aws/smithy-go/pull/316))

### Codegen
* Adds support for handling `float32` and `float64` `NaN` values in HTTP Protocol Unit Tests. ([#316](https://github.com/aws/smithy-go/pull/316))
* Adds support protocol generator implementations to override the error code string returned by `ErrorCode` methods on generated error types. ([#315](https://github.com/aws/smithy-go/pull/315))

# Release v1.5.0 (2021-06-25)

### Smithy Go module
* `time`: Update time parsing to not be as strict for HTTPDate and DateTime ([#307](https://github.com/aws/smithy-go/pull/307))
  * Fixes [#302](https://github.com/aws/smithy-go/issues/302) by changing time to UTC before formatting so no local offset time is lost.

### Codegen
* Adds support for integrating client members via plugins ([#301](https://github.com/aws/smithy-go/pull/301))
* Fix serialization of enum types marked with payload trait ([#296](https://github.com/aws/smithy-go/pull/296))
* Update generation of API client modules to include a manifest of files generated ([#283](https://github.com/aws/smithy-go/pull/283))
* Update Group Java group ID for smithy-go generator ([#298](https://github.com/aws/smithy-go/pull/298))
* Support the delegation of determining the errors that can occur for an operation ([#304](https://github.com/aws/smithy-go/pull/304))
* Support for marking and documenting deprecated client config fields. ([#303](https://github.com/aws/smithy-go/pull/303))

# Release v1.4.0 (2021-05-06)

### Smithy Go module
* `encoding/xml`: Fix escaping of Next Line and Line Start in XML Encoder ([#267](https://github.com/aws/smithy-go/pull/267))

### Codegen
* Add support for Smithy 1.7 ([#289](https://github.com/aws/smithy-go/pull/289))
* Add support for httpQueryParams location
* Add support for model renaming conflict resolution with service closure

# Release v1.3.1 (2021-04-08)

### Smithy Go module
* `transport/http`: Loosen endpoint hostname validation to allow specifying port numbers. ([#279](https://github.com/aws/smithy-go/pull/279))
* `io`: Fix RingBuffer panics due to out of bounds index. ([#282](https://github.com/aws/smithy-go/pull/282))

# Release v1.3.0 (2021-04-01)

### Smithy Go module
* `transport/http`: Add utility to safely join string to url path, and url raw query.

### Codegen
* Update HttpBindingProtocolGenerator to use http/transport JoinPath and JoinQuery utility.

# Release v1.2.0 (2021-03-12)

### Smithy Go module
* Fix support for parsing shortened year format in HTTP Date header.
* Fix GitHub APIDiff action workflow to get gorelease tool correctly.
* Fix codegen artifact unit test for Go 1.16

### Codegen
* Fix generating paginator nil parameter handling before usage.
* Fix Serialize unboxed members decorated as required.
* Add ability to define resolvers at both client construction and operation invocation.
* Support for extending paginators with custom runtime trait
