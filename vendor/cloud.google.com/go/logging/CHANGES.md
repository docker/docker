# Changes

## [1.21.1](https://github.com/googleapis/google-cloud-go/compare/logging/v1.21.0...logging/v1.21.1) (2026-10-28)


### Bug Fixes

* Adds additional validation for REST RPC requests ([#20753](https://github.com/googleapis/google-cloud-go/issues/20753)) ([7827181](https://github.com/googleapis/google-cloud-go/commit/7827181e7cb653fb69645c538b162b2c7490b0bb))

## [1.21.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.20.0...logging/v1.21.0) (2026-10-22)


### Features

* Update API sources and regenerate ([#20728](https://github.com/googleapis/google-cloud-go/issues/20728)) ([ab1da9d](https://github.com/googleapis/google-cloud-go/commit/ab1da9d200416594ae18fa7a2944961f1c92c928))

## [1.20.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.19.1...logging/v1.20.0) (2026-09-23)


### Features

* **all:** Update supported go versions ([#20520](https://github.com/googleapis/google-cloud-go/issues/20520)) ([90413d3](https://github.com/googleapis/google-cloud-go/commit/90413d3e9d381ef4b92e3c496d98b023c5b2d8cc))

## [1.19.1](https://github.com/googleapis/google-cloud-go/compare/logging/v1.19.0...logging/v1.19.1) (2026-08-07)


### Bug Fixes

* **logging:** Add HTTP client Timeout to fix metadata probe hang ([#19979](https://github.com/googleapis/google-cloud-go/issues/19979)) ([a47a42b](https://github.com/googleapis/google-cloud-go/commit/a47a42b853e6afcdf492ad7e40268f03614165c6))

## [1.19.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.18.0...logging/v1.19.0) (2026-07-13)


### Features

* **o11y:** Regenerate clients for LRO tracing ([#20107](https://github.com/googleapis/google-cloud-go/issues/20107)) ([779074e](https://github.com/googleapis/google-cloud-go/commit/779074edd267a26520bae459307660953129eb07))

## [1.18.0](https://github.com/googleapis/google-cloud-go/releases/tag/logging%2Fv1.18.0) (2026-05-07)

## [1.17.0](https://github.com/googleapis/google-cloud-go/releases/tag/logging%2Fv1.17.0) (2026-04-30)

## [1.16.0](https://github.com/googleapis/google-cloud-go/releases/tag/logging%2Fv1.16.0) (2026-04-13)

## [1.15.0](https://github.com/googleapis/google-cloud-go/releases/tag/logging%2Fv1.15.0) (2026-04-09)

## [1.14.0](https://github.com/googleapis/google-cloud-go/releases/tag/logging%2Fv1.14.0) (2026-04-02)

## [1.13.2](https://github.com/googleapis/google-cloud-go/releases/tag/logging%2Fv1.13.2) (2026-02-04)


### Features

* **logging:** Enable AllowHardBoundTokens for Go logging library ([#13615](https://github.com/googleapis/google-cloud-go/pull/13615)) ([18dec3a](https://github.com/googleapis/google-cloud-go/commit/18dec3a31f1d24dfa1dc58eeb532991336772dac))

## [1.13.1](https://github.com/googleapis/google-cloud-go/compare/logging/v1.13.0...logging/v1.13.1) (2025-10-28)


### Bug Fixes

* **logging:** Upgrade gRPC service registration func ([486bd40](https://github.com/googleapis/google-cloud-go/commit/486bd406336fe91eae7c9040f2496ebfc8aee342))
* **logging:** Upgrade gRPC service registration func ([#13139](https://github.com/googleapis/google-cloud-go/issues/13139)) ([39af2f9](https://github.com/googleapis/google-cloud-go/commit/39af2f9683b76ab290cc66b009ddc366fe63986b))

## [1.13.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.12.0...logging/v1.13.0) (2025-01-02)


### Features

* **logging:** Change go gapic transport to grpc+rest in logging ([#11289](https://github.com/googleapis/google-cloud-go/issues/11289)) ([a5f250b](https://github.com/googleapis/google-cloud-go/commit/a5f250baf8085bdb07807869a7c4a3a0ca3f535d))


### Bug Fixes

* **logging:** Update golang.org/x/net to v0.33.0 ([e9b0b69](https://github.com/googleapis/google-cloud-go/commit/e9b0b69644ea5b276cacff0a707e8a5e87efafc9))
* **logging:** Update google.golang.org/api to v0.203.0 ([8bb87d5](https://github.com/googleapis/google-cloud-go/commit/8bb87d56af1cba736e0fe243979723e747e5e11e))
* **logging:** WARNING: On approximately Dec 1, 2024, an update to Protobuf will change service registration function signatures to use an interface instead of a concrete type in generated .pb.go files. This change is expected to affect very few if any users of this client library. For more information, see https://togithub.com/googleapis/google-cloud-go/issues/11020. ([8bb87d5](https://github.com/googleapis/google-cloud-go/commit/8bb87d56af1cba736e0fe243979723e747e5e11e))

## [1.12.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.11.0...logging/v1.12.0) (2024-10-16)


### Features

* **logging:** Add support for Go 1.23 iterators ([84461c0](https://github.com/googleapis/google-cloud-go/commit/84461c0ba464ec2f951987ba60030e37c8a8fc18))


### Bug Fixes

* **logging:** Bump dependencies ([2ddeb15](https://github.com/googleapis/google-cloud-go/commit/2ddeb1544a53188a7592046b98913982f1b0cf04))
* **logging:** Fixed input validation for X-Cloud-Trace-Context; encoded spanID from XCTC header into hex string. ([#10979](https://github.com/googleapis/google-cloud-go/issues/10979)) ([a157558](https://github.com/googleapis/google-cloud-go/commit/a157558fd92adb1e6f608d5764316652e06dcd02))
* **logging:** Update google.golang.org/api to v0.191.0 ([5b32644](https://github.com/googleapis/google-cloud-go/commit/5b32644eb82eb6bd6021f80b4fad471c60fb9d73))

## [1.11.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.10.0...logging/v1.11.0) (2024-07-24)


### Features

* **logging:** OpenTelemetry trace/span ID integration for Go logging library ([#10030](https://github.com/googleapis/google-cloud-go/issues/10030)) ([c6711b8](https://github.com/googleapis/google-cloud-go/commit/c6711b83cb6f9f35032e69a40632b7268fcdbd0a))


### Bug Fixes

* **logging:** Bump google.golang.org/api@v0.187.0 ([8fa9e39](https://github.com/googleapis/google-cloud-go/commit/8fa9e398e512fd8533fd49060371e61b5725a85b))
* **logging:** Bump google.golang.org/grpc@v1.64.1 ([8ecc4e9](https://github.com/googleapis/google-cloud-go/commit/8ecc4e9622e5bbe9b90384d5848ab816027226c5))
* **logging:** Skip automatic resource detection if a CommonResource ([#10441](https://github.com/googleapis/google-cloud-go/issues/10441)) ([fc4c910](https://github.com/googleapis/google-cloud-go/commit/fc4c91099443385d3052e1d6cf1020c7918c0e5a))
* **logging:** Update dependencies ([257c40b](https://github.com/googleapis/google-cloud-go/commit/257c40bd6d7e59730017cf32bda8823d7a232758))


### Documentation

* **logging:** Documentation for automatic trace/span ID extraction ([#10536](https://github.com/googleapis/google-cloud-go/issues/10536)) ([8cf89a3](https://github.com/googleapis/google-cloud-go/commit/8cf89a340ad75cc1c39e8a9b876b47af069aa273))

## [1.10.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.9.0...logging/v1.10.0) (2024-05-15)


### Features

* **logging/logadmin:** Allow logging PageSize to override ([#9409](https://github.com/googleapis/google-cloud-go/issues/9409)) ([5ca0271](https://github.com/googleapis/google-cloud-go/commit/5ca0271f4354d51a968cf5819322d1c093944d1c))


### Bug Fixes

* **logging:** Bump x/net to v0.24.0 ([ba31ed5](https://github.com/googleapis/google-cloud-go/commit/ba31ed5fda2c9664f2e1cf972469295e63deb5b4))
* **logging:** Enable universe domain resolution options ([fd1d569](https://github.com/googleapis/google-cloud-go/commit/fd1d56930fa8a747be35a224611f4797b8aeb698))
* **logging:** Set default value for BundleByteLimit to 9.5 MiB to avoid payload size limits. ([#9662](https://github.com/googleapis/google-cloud-go/issues/9662)) ([d5815da](https://github.com/googleapis/google-cloud-go/commit/d5815da84dfb3fedd67bce4c7a24e2f0ab235811))
* **logging:** Update protobuf dep to v1.33.0 ([30b038d](https://github.com/googleapis/google-cloud-go/commit/30b038d8cac0b8cd5dd4761c87f3f298760dd33a))

## [1.9.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.8.1...logging/v1.9.0) (2023-12-12)


### Features

* **logging:** Add Cloud Run job monitored resource ([#8631](https://github.com/googleapis/google-cloud-go/issues/8631)) ([de66868](https://github.com/googleapis/google-cloud-go/commit/de66868905c83cc77d7781202264e4c6daafb519))
* **logging:** Automatic project detection in logging.NewClient() ([#9006](https://github.com/googleapis/google-cloud-go/issues/9006)) ([bc13e6a](https://github.com/googleapis/google-cloud-go/commit/bc13e6acd5df2c46fe43de64cc0a6220e7086b9c))


### Bug Fixes

* **logging:** Added marshalling methods for proto fields in structuredLogEntry ([#8979](https://github.com/googleapis/google-cloud-go/issues/8979)) ([aa385f9](https://github.com/googleapis/google-cloud-go/commit/aa385f97d07230af0bb47a0775cf0e2db368a0b7))
* **logging:** Bump google.golang.org/api to v0.149.0 ([8d2ab9f](https://github.com/googleapis/google-cloud-go/commit/8d2ab9f320a86c1c0fab90513fc05861561d0880))
* **logging:** Update golang.org/x/net to v0.17.0 ([174da47](https://github.com/googleapis/google-cloud-go/commit/174da47254fefb12921bbfc65b7829a453af6f5d))
* **logging:** Update grpc-go to v1.56.3 ([343cea8](https://github.com/googleapis/google-cloud-go/commit/343cea8c43b1e31ae21ad50ad31d3b0b60143f8c))
* **logging:** Update grpc-go to v1.59.0 ([81a97b0](https://github.com/googleapis/google-cloud-go/commit/81a97b06cb28b25432e4ece595c55a9857e960b7))
* **logging:** Use instance/attributes/cluster-location for location on GKE ([#9094](https://github.com/googleapis/google-cloud-go/issues/9094)) ([c85b9d4](https://github.com/googleapis/google-cloud-go/commit/c85b9d4ee4b936c551562d9b83bcaab09297f369))

## [1.8.1](https://github.com/googleapis/google-cloud-go/compare/logging/v1.8.0...logging/v1.8.1) (2023-08-14)


### Bug Fixes

* **logging:** Init default retryer ([#8415](https://github.com/googleapis/google-cloud-go/issues/8415)) ([c980708](https://github.com/googleapis/google-cloud-go/commit/c980708c5f69f69c21632250a96f4f2c2e87f697))

## [1.8.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.7.0...logging/v1.8.0) (2023-08-09)


### Features

* **logging:** Log Analytics features of the Cloud Logging API feat: Add ConfigServiceV2.CreateBucketAsync method for creating Log Buckets asynchronously feat: Add ConfigServiceV2.UpdateBucketAsync method for creating Log Buckets asynchronously feat: Add ConfigServiceV2.CreateLink method for creating linked datasets for Log Analytics Buckets feat: Add ConfigServiceV2.DeleteLink method for deleting linked datasets feat: Add ConfigServiceV2.ListLinks method for listing linked datasets feat: Add ConfigServiceV2.GetLink methods for describing linked datasets feat: Add LogBucket.analytics_enabled field that specifies whether Log Bucket's Analytics features are enabled feat: Add LogBucket.index_configs field that contains a list of Log Bucket's indexed fields and related configuration data docs: Documentation for the Log Analytics features of the Cloud Logging API ([31c3766](https://github.com/googleapis/google-cloud-go/commit/31c3766c9c4cab411669c14fc1a30bd6d2e3f2dd))
* **logging:** Update all direct dependencies ([b340d03](https://github.com/googleapis/google-cloud-go/commit/b340d030f2b52a4ce48846ce63984b28583abde6))


### Bug Fixes

* **logging/logadmin:** Fix paging example filter ([#8224](https://github.com/googleapis/google-cloud-go/issues/8224)) ([710c627](https://github.com/googleapis/google-cloud-go/commit/710c627b2cf46b8b2e83ff02e020700b3281e498))
* **logging:** REST query UpdateMask bug ([df52820](https://github.com/googleapis/google-cloud-go/commit/df52820b0e7721954809a8aa8700b93c5662dc9b))
* **logging:** Update grpc to v1.55.0 ([1147ce0](https://github.com/googleapis/google-cloud-go/commit/1147ce02a990276ca4f8ab7a1ab65c14da4450ef))
* **logging:** Use fieldmask directly instead of field_mask genproto alias ([#8031](https://github.com/googleapis/google-cloud-go/issues/8031)) ([13d9483](https://github.com/googleapis/google-cloud-go/commit/13d9483ddcfef20ea6dcdb3db5f4560c11c15c09))

## [1.7.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.6.1...logging/v1.7.0) (2023-02-27)


### Features

* **logging:** Add (*Logger). StandardLoggerFromTemplate() method. ([#7261](https://github.com/googleapis/google-cloud-go/issues/7261)) ([533ecbb](https://github.com/googleapis/google-cloud-go/commit/533ecbb19a2833e667ad139a6604fd40dfb43cdc))
* **logging:** Add REST client ([06a54a1](https://github.com/googleapis/google-cloud-go/commit/06a54a16a5866cce966547c51e203b9e09a25bc0))
* **logging:** Rewrite signatures and type in terms of new location ([620e6d8](https://github.com/googleapis/google-cloud-go/commit/620e6d828ad8641663ae351bfccfe46281e817ad))


### Bug Fixes

* **logging:** Correctly populate SourceLocation when logging via (*Logger).StandardLogger ([#7320](https://github.com/googleapis/google-cloud-go/issues/7320)) ([1a0bd13](https://github.com/googleapis/google-cloud-go/commit/1a0bd13b88569826f4ee6528e9cdb59fd26914fa))
* **logging:** Fix typo in README.md ([#7297](https://github.com/googleapis/google-cloud-go/issues/7297)) ([82aa2ee](https://github.com/googleapis/google-cloud-go/commit/82aa2ee9381f793bd731f1b6789fc18e4b671bd7))

## [1.6.1](https://github.com/googleapis/google-cloud-go/compare/logging/v1.6.0...logging/v1.6.1) (2022-12-02)


### Bug Fixes

* **logging:** downgrade some dependencies ([7540152](https://github.com/googleapis/google-cloud-go/commit/754015236d5af7c82a75da218b71a87b9ead6eb5))

## [1.6.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.5.0...logging/v1.6.0) (2022-11-29)


### Features

* **logging:** start generating proto stubs ([0eb700d](https://github.com/googleapis/google-cloud-go/commit/0eb700d17c4cac56f59038f0f3ae5a65257a3d38))


### Bug Fixes

* **logging:** Fix stdout log http request format ([#7083](https://github.com/googleapis/google-cloud-go/issues/7083)) ([2894e66](https://github.com/googleapis/google-cloud-go/commit/2894e66be7ff7536f725ede453d1834586a361bd))

## [1.5.0](https://github.com/googleapis/google-cloud-go/compare/logging/v1.4.2...logging/v1.5.0) (2022-06-25)


### Features

* **logging:** add better version metadata to calls ([d1ad921](https://github.com/googleapis/google-cloud-go/commit/d1ad921d0322e7ce728ca9d255a3cf0437d26add))
* **logging:** set versionClient to module version ([55f0d92](https://github.com/googleapis/google-cloud-go/commit/55f0d92bf112f14b024b4ab0076c9875a17423c9))
* **logging:** support structured logging functionality ([#6029](https://github.com/googleapis/google-cloud-go/issues/6029)) ([56f4cdd](https://github.com/googleapis/google-cloud-go/commit/56f4cdd066cc9eaeece2c6fb466d58c3e7c41563))
* **logging:** Update Logging API with latest changes ([5af548b](https://github.com/googleapis/google-cloud-go/commit/5af548bee4ffde279727b2e1ad9b072925106a74))


### Bug Fixes

* **logging:** remove instance_name resource label ([#5461](https://github.com/googleapis/google-cloud-go/issues/5461)) ([115385f](https://github.com/googleapis/google-cloud-go/commit/115385f066ee54cf35a093749bc2673a17b3fa08))

### [1.4.2](https://www.github.com/googleapis/google-cloud-go/compare/logging/v1.4.1...logging/v1.4.2) (2021-05-20)


### Bug Fixes

* **logging:** correctly detect GKE resource ([#4092](https://www.github.com/googleapis/google-cloud-go/issues/4092)) ([a2538e1](https://www.github.com/googleapis/google-cloud-go/commit/a2538e16123c21da62036b56df8c104360f1c2d6))

### [1.4.1](https://www.github.com/googleapis/google-cloud-go/compare/logging/v1.4.0...logging/v1.4.1) (2021-05-03)


### Bug Fixes

* **logging:** allow nil or custom zones in resource detection ([#3997](https://www.github.com/googleapis/google-cloud-go/issues/3997)) ([aded90b](https://www.github.com/googleapis/google-cloud-go/commit/aded90b92de3fa3bed079af1aa4879d00572e8ae))
* **logging:** appengine zone label ([#3998](https://www.github.com/googleapis/google-cloud-go/issues/3998)) ([394a586](https://www.github.com/googleapis/google-cloud-go/commit/394a586bac04953e92a6496a7ca3b61bd64155ab))

## [1.4.0](https://www.github.com/googleapis/google-cloud-go/compare/logging/v1.2.0...logging/v1.4.0) (2021-04-15)


### Features

* **logging:** cloud run and functions resource autodetection ([#3909](https://www.github.com/googleapis/google-cloud-go/issues/3909)) ([1204de8](https://www.github.com/googleapis/google-cloud-go/commit/1204de85e58334bf93fecdcb0ab8b581449c2745))
* **logging:** make toLogEntry function public ([#3863](https://www.github.com/googleapis/google-cloud-go/issues/3863)) ([71828c2](https://www.github.com/googleapis/google-cloud-go/commit/71828c28d424c34da6d0392651739a364cd57e79))


### Bug Fixes

* **logging:** Entries has a 24H default filter ([#3120](https://www.github.com/googleapis/google-cloud-go/issues/3120)) ([b32eb82](https://www.github.com/googleapis/google-cloud-go/commit/b32eb822d17838bde91c610a5a9d392d325a592d))

## v1.3.0

- Updates to various dependencies.

## [1.2.0](https://www.github.com/googleapis/google-cloud-go/compare/logging/v1.1.2...v1.2.0) (2021-01-25)


### Features

* **logging:** add localIP and Cache fields to HTTPRequest conversion from proto ([#3600](https://www.github.com/googleapis/google-cloud-go/issues/3600)) ([f93027b](https://www.github.com/googleapis/google-cloud-go/commit/f93027b47735e7c181989666e0826bea57ec51e1))

### [1.1.2](https://www.github.com/googleapis/google-cloud-go/compare/logging/v1.1.1...v1.1.2) (2020-11-09)


### Bug Fixes

* **logging:** allow X-Cloud-Trace-Context fields to be optional ([#3062](https://www.github.com/googleapis/google-cloud-go/issues/3062)) ([7ff03cf](https://www.github.com/googleapis/google-cloud-go/commit/7ff03cf9a544e753de5b034e18339ecf517d2193))
* **logging:** do not panic in library code ([#3076](https://www.github.com/googleapis/google-cloud-go/issues/3076)) ([529be97](https://www.github.com/googleapis/google-cloud-go/commit/529be977f766443f49cb8914e17ba07c93841e84)), closes [#1862](https://www.github.com/googleapis/google-cloud-go/issues/1862)

## v1.1.1

- Rebrand "Stackdriver Logging" to "Cloud Logging".

## v1.1.0

- Support unmarshalling stringified Severity.
- Add exported SetGoogleClientInfo wrappers to manual file.
- Support no payload.
- Update "Grouping Logs by Request" docs.
- Add auto-detection of monitored resources on GAE Standard.

## v1.0.0

This is the first tag to carve out logging as its own module. See:
https://github.com/golang/go/wiki/Modules#is-it-possible-to-add-a-module-to-a-multi-module-repository.
