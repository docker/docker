# Changes

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
