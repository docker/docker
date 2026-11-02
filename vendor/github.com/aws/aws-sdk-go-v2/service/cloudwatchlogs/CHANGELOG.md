# v1.90.3 (2026-10-27)

* **Dependency Update**: Update to smithy-go v1.28.7.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.90.2 (2026-10-22)

* **Bug Fix**: Update smithy-go to v1.28.6
* **Dependency Update**: Updated to the latest SDK module versions

# v1.90.1 (2026-10-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.90.0 (2026-10-19)

* **Feature**: Amazon CloudWatch Logs now supports filtering DescribeDeliverySources by resourceArns, DescribeDeliveryDestinations by names, and DescribeDeliveries by deliverySourceNames.
* **Bug Fix**: Allow lists with null values to deserialize into its zero value instead of failing

# v1.89.3 (2026-10-09.2)

* **Dependency Update**: Update to smithy-go v1.28.5.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.89.2 (2026-10-08)

* **Bug Fix**: Fix modeled errors returning an empty `ErrorMessage()` when the service sends the message under a differently-cased key (`Message` instead of `message`). This was a regression introduced with schema-based deserialization.
* **Dependency Update**: Update to smithy-go v1.28.4.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.89.1 (2026-10-06)

* No change notes available for this release.

# v1.89.0 (2026-09-30)

* **Feature**: Amazon CloudWatch Logs now supports an optional roleArn parameter on PutDeliveryDestination for X-Ray trace delivery destinations, specifying the IAM role to assume when delivering traces.

# v1.88.1 (2026-09-24)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.88.0 (2026-09-15)

* **Feature**: Enable schema-based (de)serialization for this service.

# v1.87.0 (2026-09-09)

* **Feature**: Stop registering the `retry.MetricsHeader` middleware in generated clients. The `Amz-Sdk-Request` header is now set by the retry middleware itself.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.86.0 (2026-09-04)

* **Feature**: Stop registering the `spanRetryLoop` middleware in generated clients. The retry loop's tracing span is now opened by the retry middleware itself.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.85.0 (2026-08-31.2)

* **Feature**: Stop registering the `SetCredentialSourceMiddleware` middleware in generated clients. Credential source user agent features are now set when the client's middleware stack is constructed.

# v1.84.1 (2026-08-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.84.0 (2026-08-27)

* **Feature**: Added resultCount to QueryStatistics in GetQueryResults. This field returns the total number of output rows in the final result set, helping customers programmatically determine whether a query produced results after all operations including post-aggregation filters.
* **Feature**: Support connection read timeouts in the SDK. This is currently available on an opt-in basis by setting env `AWS_ENABLE_DEFAULT_SOCKET_TIMEOUT_2026=true`.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.83.0 (2026-08-26)

* **Feature**: Stop registering the `ComputeContentLength` middleware in generated clients. `Content-Length` is now set when the request body is set via `SetStream`.
* **Dependency Update**: Update to smithy-go v1.28.0.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.82.4 (2026-08-25)

* **Dependency Update**: Update to smithy-go v1.27.10.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.82.3 (2026-08-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.82.2 (2026-08-14)

* **Dependency Update**: Update to smithy-go v1.27.8.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.82.1 (2026-08-10)

* **Dependency Update**: Update to smithy-go v1.27.7.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.82.0 (2026-08-06)

* **Feature**: This release adds index category support to the CloudWatch Logs DescribeFieldIndexes API. Customers can filter and identify DEFAULT, CUSTOM, AUTO, and INACTIVE field indexes.

# v1.81.1 (2026-08-05)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.81.0 (2026-07-31.2)

* **Feature**: Amazon CloudWatch Logs now lets you create and update lookup tables directly from CloudWatch Logs query results by passing a queryId, and configure a lookup table as a scheduled query destination so it refreshes automatically with the latest query results on each run.
* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.27.6 to fix various serde issues in HTTP binding services.

# v1.80.2 (2026-07-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.80.1 (2026-07-28)

* **Dependency Update**: Update to smithy-go v1.27.5.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.80.0 (2026-07-21)

* **Feature**: Add an option to clients to disable clock skew
* **Dependency Update**: Updated to the latest SDK module versions

# v1.79.1 (2026-07-13)

* No change notes available for this release.

# v1.79.0 (2026-07-06)

* **Feature**: Add request serialization snapshot tests.
* **Feature**: Added PutStorageTierPolicy and GetStorageTierPolicy APIs to Amazon CloudWatch Logs. Customers can now configure account-level Intelligent Tiering to automatically optimize log storage costs by moving infrequently accessed data to lower-cost storage tiers.

# v1.78.2 (2026-07-01)

* **Bug Fix**: Bump smithy-go to 1.27.3, fix JSON encorder for document.Number, endpoint host label format validation and CBOR union serialization on new serde
* **Dependency Update**: Updated to the latest SDK module versions

# v1.78.1 (2026-06-29)

* No change notes available for this release.

# v1.78.0 (2026-06-22)

* **Feature**: CloudWatch Logs Updates - New APIs introduced to support syslog ingestion to a log group. For more information, see CloudWatch Logs API documentation.

# v1.77.0 (2026-06-18)

* **Feature**: Added optional startFromHead parameter to FilterLogEvents enabling descending timestamp order (newest first) when set to false. Default true preserves existing ascending order. Reverse sorting requires a startTime on or after Jan 1, 2024.

# v1.76.0 (2026-06-15)

* **Feature**: Added endTimeOffset parameter to Scheduled Queries APIs (Create, Update, Get) enabling bounded time window configuration. Introduced scheduleType filter (CUSTOMER MANAGED, AWS MANAGED) for ListScheduledQueries and exposed it in Get and Update responses.

# v1.75.2 (2026-06-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.75.1 (2026-06-04)

* **Dependency Update**: Update to smithy-go v1.27.1 to fix several union-related deserialization bugs in schema-serde-enabled services.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.75.0 (2026-06-03.2)

* **Feature**: Adding new BDD representation of endpoint ruleset

# v1.74.4 (2026-06-03)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.74.3 (2026-06-02)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.74.2 (2026-05-29)

* **Dependency Update**: Update to smithy-go v1.26.0.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.74.1 (2026-05-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.74.0 (2026-05-15)

* **Feature**: Updating the max limit for start query api parameter.

# v1.73.0 (2026-05-04)

* **Feature**: Adding an additional optional deliverySourceConfiguration field to PutDeliverySource API. This enables customers to pass service-specific configurations through IngestionHub such as tracing enablement or sampling rates that will be propagated to the source resource.

# v1.72.0 (2026-05-01)

* **Feature**: Adds support for filtering log groups by tags in the ListLogGroups API via the new logGroupTags parameter.

# v1.71.1 (2026-04-29)

* **Dependency Update**: Update to smithy-go v1.25.1.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.71.0 (2026-04-27)

* **Feature**: Adds support for selecting all logs sources and types in a single association.

# v1.70.0 (2026-04-24)

* **Feature**: Adding nextToken and maxItems to the GetQueryResults API.

# v1.69.1 (2026-04-17)

* **Dependency Update**: Bump smithy-go to 1.25.0 to support endpointBdd trait
* **Dependency Update**: Updated to the latest SDK module versions

# v1.69.0 (2026-04-16)

* **Feature**: Endpoint update for CloudWatch Logs Streaming APIs.

# v1.68.0 (2026-04-03)

* **Feature**: Added queryDuration, bytesScanned, and userIdentity fields to the QueryInfo response object returned by DescribeQueries. Customers can now view detailed query cost information including who ran the query, how long it took, and the volume of data scanned.

# v1.67.0 (2026-04-02)

* **Feature**: We are pleased to announce that our logs transformation csv processor now has a destination field, allowing you to specify under which parent node parsed columns be placed under.

# v1.66.0 (2026-03-30)

* **Feature**: Adds Lookup Tables to CloudWatch Logs for log enrichment using CSV key-value data with KMS encryption support.

# v1.65.0 (2026-03-26)

* **Feature**: This release adds parameter support to saved queries in CloudWatch Logs Insights. Define reusable query templates with named placeholders, invoke them using start query. Available in Console, CLI and SDK
* **Bug Fix**: Fix a bug where a recorded clock skew could persist on the client even if the client and server clock ended up realigning.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.64.2 (2026-03-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.64.1 (2026-03-13)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.64.0 (2026-03-03)

* **Feature**: CloudWatch Logs updates- Added support for the PutBearerTokenAuthentication API to enable or disable bearer token authentication on a log group. For more information, see CloudWatch Logs API documentation.
* **Dependency Update**: Bump minimum Go version to 1.24
* **Dependency Update**: Updated to the latest SDK module versions

# v1.63.2 (2026-02-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.63.1 (2026-01-09)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.63.0 (2025-12-15)

* **Feature**: This release allows you to import your historical CloudTrail Lake data into CloudWatch with a few steps, enabling you to easily consolidate operational, security, and compliance data in one place.

# v1.62.2 (2025-12-09)

* No change notes available for this release.

# v1.62.1 (2025-12-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.62.0 (2025-12-02)

* **Feature**: CloudWatch Logs adds managed S3 Tables integration to access logs using other analytical tools, as well as facets and field indexing to simplify log analytics in CloudWatch Logs Insights.
* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.24.0. Notably this version of the library reduces the allocation footprint of the middleware system. We observe a ~10% reduction in allocations per SDK call with this change.

# v1.61.1 (2025-11-25)

* **Bug Fix**: Add error check for endpoint param binding during auth scheme resolution to fix panic reported in #3234

# v1.61.0 (2025-11-24)

* **Feature**: New CloudWatch Logs feature - LogGroup Deletion Protection, a capability that allows customers to safeguard their critical CloudWatch log groups from accidental or unintended deletion.

# v1.60.1 (2025-11-19.2)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.60.0 (2025-11-19)

* **Feature**: Adding support for ocsf version 1.5, add optional parameter MappingVersion

# v1.59.0 (2025-11-18)

* **Feature**: CloudWatch Logs updates: Added capability to setup a recurring schedule for log insights queries. Logs introduced Scheduled Queries (managed through Create/Update/Get/Delete/List/History Scheduled Query APIs). For more information, see CloudWatch Logs API documentation.

# v1.58.9 (2025-11-12)

* **Bug Fix**: Further reduce allocation overhead when the metrics system isn't in-use.
* **Bug Fix**: Reduce allocation overhead when the client doesn't have any HTTP interceptors configured.
* **Bug Fix**: Remove blank trace spans towards the beginning of the request that added no additional information. This conveys a slight reduction in overall allocations.

# v1.58.8 (2025-11-11)

* **Bug Fix**: Return validation error if input region is not a valid host label.

# v1.58.7 (2025-11-04)

* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.23.2 which should convey some passive reduction of overall allocations, especially when not using the metrics system.

# v1.58.6 (2025-10-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.58.5 (2025-10-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.58.4 (2025-10-22)

* No change notes available for this release.

# v1.58.3 (2025-10-16)

* **Dependency Update**: Bump minimum Go version to 1.23.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.58.2 (2025-09-26)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.58.1 (2025-09-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.58.0 (2025-09-16)

* **Feature**: Cloudwatch Logs added support for 2 new API parameters in metric and subscription filter APIs to filter log events based on system field values and emit system field values as dimensions and send them to customer destination as additional metadata.

# v1.57.4 (2025-09-10)

* No change notes available for this release.

# v1.57.3 (2025-09-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.57.2 (2025-08-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.57.1 (2025-08-27)

* **Dependency Update**: Update to smithy-go v1.23.0.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.57.0 (2025-08-25)

* **Feature**: Remove incorrect endpoint tests

# v1.56.2 (2025-08-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.56.1 (2025-08-20)

* **Bug Fix**: Remove unused deserialization code.

# v1.56.0 (2025-08-11)

* **Feature**: Add support for configuring per-service Options via callback on global config.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.55.0 (2025-08-04)

* **Feature**: Support configurable auth scheme preferences in service clients via AWS_AUTH_SCHEME_PREFERENCE in the environment, auth_scheme_preference in the config file, and through in-code settings on LoadDefaultConfig and client constructor methods.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.54.1 (2025-07-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.54.0 (2025-07-28)

* **Feature**: Add support for HTTP interceptors.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.53.1 (2025-07-19)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.53.0 (2025-07-18)

* **Feature**: CloudWatchLogs launches GetLogObject API with streaming support for efficient log data retrieval. Logs added support for new AccountPolicy type METRIC_EXTRACTION_POLICY. For more information, see CloudWatch Logs API documentation

# v1.52.0 (2025-07-16.2)

* **Feature**: CloudWatch Logs updates: Added X-Ray tracing for Amazon Bedrock Agent resources. Logs introduced Log Group level resource policies (managed through Put/Delete/Describe Resource Policy APIs). For more information, see CloudWatch Logs API documentation.

# v1.51.0 (2025-06-18)

* **Feature**: Added CloudWatch Logs Transformer support for converting CloudTrail, VPC Flow, EKS Audit, AWS WAF and Route53 Resolver logs to OCSF v1.1 format.

# v1.50.3 (2025-06-17)

* **Dependency Update**: Update to smithy-go v1.22.4.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.50.2 (2025-06-10)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.50.1 (2025-06-06)

* No change notes available for this release.

# v1.50.0 (2025-05-14)

* **Feature**: This release adds a new API "ListLogGroups" and an improvement in API "DescribeLogGroups"

# v1.49.0 (2025-05-09)

* **Feature**: We are pleased to announce limit increases to our grok processor logs transformation feature. Now you can define 20 Grok patterns in their configurations, with an expanded total pattern matching limit of 512 characters.

# v1.48.0 (2025-04-30)

* **Feature**: CloudWatch Logs supports "DELIVERY" log class. This log class is used only for delivering AWS Lambda logs to Amazon S3 or Amazon Data Firehose.

# v1.47.3 (2025-04-10)

* No change notes available for this release.

# v1.47.2 (2025-04-03)

* No change notes available for this release.

# v1.47.1 (2025-03-20)

* No change notes available for this release.

# v1.47.0 (2025-03-13)

* **Feature**: Updated CreateLogAnomalyDetector to accept only kms key arn

# v1.46.1 (2025-03-04.2)

* **Bug Fix**: Add assurance test for operation order.

# v1.46.0 (2025-02-27)

* **Feature**: Track credential providers via User-Agent Feature ids
* **Dependency Update**: Updated to the latest SDK module versions

# v1.45.14 (2025-02-18)

* **Bug Fix**: Bump go version to 1.22
* **Dependency Update**: Updated to the latest SDK module versions

# v1.45.13 (2025-02-14)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.45.12 (2025-02-05)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.45.11 (2025-02-04)

* No change notes available for this release.

# v1.45.10 (2025-01-31)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.45.9 (2025-01-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.45.8 (2025-01-24)

* **Dependency Update**: Updated to the latest SDK module versions
* **Dependency Update**: Upgrade to smithy-go v1.22.2.

# v1.45.7 (2025-01-21)

* **Documentation**: Documentation-only update to address doc errors

# v1.45.6 (2025-01-17)

* **Bug Fix**: Fix bug where credentials weren't refreshed during retry loop.

# v1.45.5 (2025-01-15)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.45.4 (2025-01-14)

* No change notes available for this release.

# v1.45.3 (2025-01-09)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.45.2 (2025-01-08)

* No change notes available for this release.

# v1.45.1 (2024-12-19)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.45.0 (2024-12-02)

* **Feature**: Adds PutIntegration, GetIntegration, ListIntegrations and DeleteIntegration APIs. Adds QueryLanguage support to StartQuery, GetQueryResults, DescribeQueries, DescribeQueryDefinitions, and PutQueryDefinition APIs.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.44.0 (2024-11-21)

* **Feature**: Adds "Create field indexes to improve query performance and reduce scan volume" and "Transform logs during ingestion". Updates documentation for "PutLogEvents with Entity".

# v1.43.3 (2024-11-18)

* **Dependency Update**: Update to smithy-go v1.22.1.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.43.2 (2024-11-07)

* **Bug Fix**: Adds case-insensitive handling of error message fields in service responses

# v1.43.1 (2024-11-06)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.43.0 (2024-10-29)

* **Feature**: Added support for new optional baseline parameter in the UpdateAnomaly API. For UpdateAnomaly requests with baseline set to True, The anomaly behavior is then treated as baseline behavior. However, more severe occurrences of this behavior will still be reported as anomalies.

# v1.42.1 (2024-10-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.42.0 (2024-10-25)

* **Feature**: Adding inferred token name for dynamic tokens in Anomalies.

# v1.41.2 (2024-10-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.41.1 (2024-10-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.41.0 (2024-10-04)

* **Feature**: Add support for HTTP client metrics.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.40.4 (2024-10-03)

* No change notes available for this release.

# v1.40.3 (2024-09-27)

* No change notes available for this release.

# v1.40.2 (2024-09-25)

* No change notes available for this release.

# v1.40.1 (2024-09-23)

* No change notes available for this release.

# v1.40.0 (2024-09-20)

* **Feature**: Add tracing and metrics support to service clients.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.39.1 (2024-09-17)

* **Bug Fix**: **BREAKFIX**: Only generate AccountIDEndpointMode config for services that use it. This is a compiler break, but removes no actual functionality, as no services currently use the account ID in endpoint resolution.

# v1.39.0 (2024-09-04)

* **Feature**: Update to support new APIs for delivery of logs from AWS services.

# v1.38.1 (2024-09-03)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.38.0 (2024-08-30)

* **Feature**: This release introduces a new optional parameter: Entity, in PutLogEvents request

# v1.37.5 (2024-08-22)

* No change notes available for this release.

# v1.37.4 (2024-08-15)

* **Dependency Update**: Bump minimum Go version to 1.21.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.37.3 (2024-07-10.2)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.37.2 (2024-07-10)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.37.1 (2024-06-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.37.0 (2024-06-26)

* **Feature**: Support list-of-string endpoint parameter.

# v1.36.1 (2024-06-19)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.36.0 (2024-06-18)

* **Feature**: Track usage of various AWS SDK features in user-agent string.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.35.8 (2024-06-17)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.35.7 (2024-06-07)

* **Bug Fix**: Add clock skew correction on all service clients
* **Dependency Update**: Updated to the latest SDK module versions

# v1.35.6 (2024-06-03)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.35.5 (2024-05-23)

* No change notes available for this release.

# v1.35.4 (2024-05-16)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.35.3 (2024-05-15)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.35.2 (2024-05-08)

* **Bug Fix**: GoDoc improvement

# v1.35.1 (2024-03-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.35.0 (2024-03-19)

* **Feature**: Update LogSamples field in Anomaly model to be a list of LogEvent

# v1.34.4 (2024-03-18)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.34.3 (2024-03-07)

* **Bug Fix**: Remove dependency on go-cmp.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.34.2 (2024-02-29)

* No change notes available for this release.

# v1.34.1 (2024-02-23)

* **Bug Fix**: Move all common, SDK-side middleware stack ops into the service client module to prevent cross-module compatibility issues in the future.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.34.0 (2024-02-22)

* **Feature**: Add middleware stack snapshot tests.

# v1.33.3 (2024-02-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.33.2 (2024-02-20)

* **Bug Fix**: When sourcing values for a service's `EndpointParameters`, the lack of a configured region (i.e. `options.Region == ""`) will now translate to a `nil` value for `EndpointParameters.Region` instead of a pointer to the empty string `""`. This will result in a much more explicit error when calling an operation instead of an obscure hostname lookup failure.

# v1.33.1 (2024-02-15)

* **Bug Fix**: Correct failure to determine the error type in awsJson services that could occur when errors were modeled with a non-string `code` field.

# v1.33.0 (2024-02-13)

* **Feature**: Bump minimum Go version to 1.20 per our language support policy.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.32.0 (2024-02-06)

* **Feature**: This release adds a new field, logGroupArn, to the response of the logs:DescribeLogGroups action.

# v1.31.0 (2024-01-10)

* **Feature**: Add support for account level subscription filter policies to PutAccountPolicy, DescribeAccountPolicies, and DeleteAccountPolicy APIs. Additionally, PutAccountPolicy has been modified with new optional "selectionCriteria" parameter for resource selection.

# v1.30.2 (2024-01-04)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.30.1 (2023-12-20)

* No change notes available for this release.

# v1.30.0 (2023-12-12)

* **Feature**: This release introduces the StartLiveTail API to tail ingested logs in near real time.

# v1.29.5 (2023-12-08)

* **Bug Fix**: Reinstate presence of default Retryer in functional options, but still respect max attempts set therein.

# v1.29.4 (2023-12-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.29.3 (2023-12-06)

* **Bug Fix**: Restore pre-refactor auth behavior where all operations could technically be performed anonymously.

# v1.29.2 (2023-12-01)

* **Bug Fix**: Correct wrapping of errors in authentication workflow.
* **Bug Fix**: Correctly recognize cache-wrapped instances of AnonymousCredentials at client construction.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.29.1 (2023-11-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.29.0 (2023-11-29)

* **Feature**: Expose Options() accessor on service clients.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.28.2 (2023-11-28.2)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.28.1 (2023-11-28)

* **Bug Fix**: Respect setting RetryMaxAttempts in functional options at client construction.

# v1.28.0 (2023-11-27)

* **Feature**: Added APIs to Create, Update, Get, List and Delete LogAnomalyDetectors and List and Update Anomalies in Detector. Added LogGroupClass attribute for LogGroups to classify loggroup as Standard loggroup with all capabilities or InfrequentAccess loggroup with limited capabilities.

# v1.27.2 (2023-11-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.27.1 (2023-11-15)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.27.0 (2023-11-09.2)

* **Feature**: Update to support new APIs for delivery of logs from AWS services.

# v1.26.1 (2023-11-09)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.26.0 (2023-11-01)

* **Feature**: Adds support for configured endpoints via environment variables and the AWS shared configuration file.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.25.0 (2023-10-31)

* **Feature**: **BREAKING CHANGE**: Bump minimum go version to 1.19 per the revised [go version support policy](https://aws.amazon.com/blogs/developer/aws-sdk-for-go-aligns-with-go-release-policy-on-supported-runtimes/).
* **Dependency Update**: Updated to the latest SDK module versions

# v1.24.2 (2023-10-12)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.24.1 (2023-10-06)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.24.0 (2023-09-20)

* **Feature**: Add ClientToken to QueryDefinition CFN Handler in CWL

# v1.23.5 (2023-08-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.23.4 (2023-08-18)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.23.3 (2023-08-17)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.23.2 (2023-08-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.23.1 (2023-08-01)

* No change notes available for this release.

# v1.23.0 (2023-07-31)

* **Feature**: Adds support for smithy-modeled endpoint resolution. A new rules-based endpoint resolution will be added to the SDK which will supercede and deprecate existing endpoint resolution. Specifically, EndpointResolver will be deprecated while BaseEndpoint and EndpointResolverV2 will take its place. For more information, please see the Endpoints section in our Developer Guide.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.22.2 (2023-07-28)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.22.1 (2023-07-13)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.22.0 (2023-07-07)

* **Feature**: Add CMK encryption support for CloudWatch Logs Insights query result data

# v1.21.2 (2023-06-15)

* No change notes available for this release.

# v1.21.1 (2023-06-13)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.21.0 (2023-06-07)

* **Feature**: This change adds support for account level data protection policies using 3 new APIs, PutAccountPolicy, DeleteAccountPolicy and DescribeAccountPolicy. DescribeLogGroup API has been modified to indicate if account level policy is applied to the LogGroup via "inheritedProperties" list in the response.

# v1.20.11 (2023-05-04)

* No change notes available for this release.

# v1.20.10 (2023-04-24)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.9 (2023-04-10)

* No change notes available for this release.

# v1.20.8 (2023-04-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.7 (2023-03-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.6 (2023-03-10)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.5 (2023-02-22)

* **Bug Fix**: Prevent nil pointer dereference when retrieving error codes.

# v1.20.4 (2023-02-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.3 (2023-02-15)

* **Announcement**: When receiving an error response in restJson-based services, an incorrect error type may have been returned based on the content of the response. This has been fixed via PR #2012 tracked in issue #1910.
* **Bug Fix**: Correct error type parsing for restJson services.

# v1.20.2 (2023-02-03)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.20.1 (2023-01-23)

* No change notes available for this release.

# v1.20.0 (2023-01-19)

* **Feature**: Bug fix - Removed the regex pattern validation from CoralModel to avoid potential security issue.

# v1.19.0 (2023-01-12)

* **Feature**: Bug fix: logGroupName is now not a required field in GetLogEvents, FilterLogEvents, GetLogGroupFields, and DescribeLogStreams APIs as logGroupIdentifier can be provided instead

# v1.18.0 (2023-01-05)

* **Feature**: Add `ErrorCodeOverride` field to all error structs (aws/smithy-go#401).

# v1.17.4 (2023-01-04)

* **Documentation**: Update to remove sequenceToken as a required field in PutLogEvents calls.

# v1.17.3 (2022-12-15)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.17.2 (2022-12-09)

* **Documentation**: Doc-only update for CloudWatch Logs, for Tagging Permissions clarifications

# v1.17.1 (2022-12-02)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.17.0 (2022-11-28)

* **Feature**: Updates to support CloudWatch Logs data protection and CloudWatch cross-account observability

# v1.16.4 (2022-11-22)

* No change notes available for this release.

# v1.16.3 (2022-11-16)

* No change notes available for this release.

# v1.16.2 (2022-11-10)

* No change notes available for this release.

# v1.16.1 (2022-11-04)

* **Documentation**: Doc-only update for bug fixes and support of export to buckets encrypted with SSE-KMS

# v1.16.0 (2022-10-31)

* **Feature**: SDK release to support tagging for destinations and log groups with TagResource. Also supports tag on create with PutDestination.

# v1.15.22 (2022-10-24)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.21 (2022-10-21)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.20 (2022-09-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.19 (2022-09-14)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.18 (2022-09-02)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.17 (2022-08-31)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.16 (2022-08-30)

* No change notes available for this release.

# v1.15.15 (2022-08-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.14 (2022-08-11)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.13 (2022-08-09)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.12 (2022-08-08)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.11 (2022-08-01)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.10 (2022-07-05)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.9 (2022-06-29)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.8 (2022-06-07)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.7 (2022-05-20)

* **Documentation**: Doc-only update to publish the new valid values for log retention

# v1.15.6 (2022-05-17)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.5 (2022-04-25)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.4 (2022-03-30)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.3 (2022-03-24)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.2 (2022-03-23)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.15.1 (2022-03-08.2)

* No change notes available for this release.

# v1.15.0 (2022-03-08)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.14.0 (2022-02-24)

* **Feature**: API client updated
* **Feature**: Adds RetryMaxAttempts and RetryMod to API client Options. This allows the API clients' default Retryer to be configured from the shared configuration files or environment variables. Adding a new Retry mode of `Adaptive`. `Adaptive` retry mode is an experimental mode, adding client rate limiting when throttles reponses are received from an API. See [retry.AdaptiveMode](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/aws/retry#AdaptiveMode) for more details, and configuration options.
* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.13.0 (2022-01-14)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.12.0 (2022-01-07)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.11.0 (2021-12-21)

* **Feature**: API Paginators now support specifying the initial starting token, and support stopping on empty string tokens.
* **Feature**: API client updated
* **Feature**: Updated to latest service endpoints

# v1.10.2 (2021-12-02)

* **Bug Fix**: Fixes a bug that prevented aws.EndpointResolverWithOptions from being used by the service client. ([#1514](https://github.com/aws/aws-sdk-go-v2/pull/1514))
* **Dependency Update**: Updated to the latest SDK module versions

# v1.10.1 (2021-11-19)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.10.0 (2021-11-12)

* **Feature**: Service clients now support custom endpoints that have an initial URI path defined.

# v1.9.0 (2021-11-06)

* **Feature**: The SDK now supports configuration of FIPS and DualStack endpoints using environment variables, shared configuration, or programmatically.
* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.8.0 (2021-10-21)

* **Feature**: API client updated
* **Feature**: Updated  to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.7.2 (2021-10-11)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.7.1 (2021-09-17)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.7.0 (2021-08-27)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.6.0 (2021-08-19)

* **Feature**: API client updated
* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.2 (2021-08-04)

* **Dependency Update**: Updated `github.com/aws/smithy-go` to latest version.
* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.1 (2021-07-15)

* **Dependency Update**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.5.0 (2021-06-25)

* **Feature**: Updated `github.com/aws/smithy-go` to latest version
* **Dependency Update**: Updated to the latest SDK module versions

# v1.4.0 (2021-05-25)

* **Feature**: API client updated

# v1.3.1 (2021-05-20)

* **Dependency Update**: Updated to the latest SDK module versions

# v1.3.0 (2021-05-14)

* **Feature**: Constant has been added to modules to enable runtime version inspection for reporting.
* **Dependency Update**: Updated to the latest SDK module versions

