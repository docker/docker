# Engine API

[![PkgGoDev](https://pkg.go.dev/badge/github.com/moby/moby/api)](https://pkg.go.dev/github.com/moby/moby/api)
![GitHub License](https://img.shields.io/github/license/moby/moby)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/moby/moby/badge)](https://scorecard.dev/viewer/?uri=github.com/moby/moby)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/10989/badge)](https://www.bestpractices.dev/projects/10989)


The Engine API is an HTTP API used by the command-line client to communicate with the daemon. It can also be used by third-party software to control the daemon.

It consists of various components in this repository:

- `api/openapi.yaml` An OpenAPI 3.2.0 definition of the API.
- `api/swagger.yaml` A generated Swagger 2.0 version for existing tooling.
- `api/types/` Types shared by both the client and server, representing various objects, options, responses, etc. Most are written manually, but some are automatically generated from the API definition. See [#27919](https://github.com/moby/moby/issues/27919) for progress on this.
- `client/` The Go client used by the command-line client. It can also be used by third-party Go programs.
- `daemon/` The daemon, which serves the API.

## OpenAPI definition

The API is defined by the [OpenAPI 3.2.0](https://spec.openapis.org/oas/v3.2.0) definition in `api/openapi.yaml`. This definition can be used to:

1. Automatically generate documentation.
2. Automatically generate the Go server and client. (A work-in-progress.)
3. Provide a machine readable version of the API for introspecting what it can do, automatically generating clients for other languages, etc.

## Updating the API documentation

Edit `api/openapi.yaml` when changing the API.
The API documentation is generated from this file.
Documentation for each API version can be found in the [docs directory](docs/README.md), which also provides a [CHANGELOG.md](docs/CHANGELOG.md). 

The file is split into two main sections:

- `components.schemas`, which defines re-usable objects used in requests and responses
- `paths`, which defines the API endpoints (and some inline objects which don't need to be reusable)

Find the endpoint under `paths`.
Reusable schemas are referenced with `$ref: "#/components/schemas/Name"`.

Request bodies use `requestBody.content`, and responses use `responses.<status>.content`, keyed by media type.
Non-body parameters use `schema`.
For array query parameters, specify `style: form` and `explode: true` for repeated keys or `explode: false` for comma-separated values.
An empty media object leaves the response schema unspecified; it does not mean there is no body.

### Swagger compatibility

OpenAPI is the source of truth.
We generate Swagger for existing tooling, including go-swagger.
For this migration, we keep the existing Swagger shape and generated Go models unchanged.
Long term, Swagger conversion should be best effort, not a limit on new API features.

The converter still rejects type unions, `anyOf`, `oneOf`, `nullable`, and `itemSchema`.
Using these features will need converter changes.
Keep `x-nullable` and other Go generator extensions: they control the generated Go code, not API validation.

Media-level request examples override schema examples and become the Swagger body schema's `example`.
The converter currently requires matching schemas and effective examples across request media types.
Response examples can differ by media type.

After editing `openapi.yaml`, run `make -C api swagger-spec` from the repository root and commit `swagger.yaml` too.
Do not edit `swagger.yaml` directly.
Regeneration keeps multiline JSON examples but may reformat other content when the specification changes.

Swagger lists all response media types in the operation's `produces` field, not per response.
Bodyless operations use `x-consumes` to keep their existing Swagger `consumes` values.

Run `make -C api validate-swagger` to check YAML style, validate both specifications, and test the converter against the checked-in Swagger.

## Generating Go models

Run `make swagger-gen` from the repository root to regenerate `api/swagger.yaml` and generate Go models with go-swagger.
Run `make -C api validate-swagger-gen` to compare regenerated models with the checked-in files.

## Viewing the API documentation

Run `make swagger-docs` to preview the documentation at `http://localhost:9000`.
Check that your changes render correctly; styling may differ from the published docs.

Published docs use the versioned `docs/v*.yaml` specifications vendored into [docker/docs](https://github.com/docker/docs).
