# API Module Makefile
# This Makefile provides targets for the swagger generation and validation
# which are specific to the API module.

DOCKER ?= docker
BUILDX ?= $(DOCKER) buildx

API_DIR := $(CURDIR)
PROJECT_PATH := /go/src/github.com/moby/moby

DOCKER_MOUNT := -v "$(API_DIR):$(PROJECT_PATH)/api"

DOCKER_IMAGE := docker-api-dev

DOCKER_WORKDIR := -w $(PROJECT_PATH)/api

DOCKER_FLAGS := $(DOCKER) run --rm $(DOCKER_MOUNT) $(DOCKER_WORKDIR)
DOCKER_RUN := $(DOCKER_FLAGS) "$(DOCKER_IMAGE)"

DOCKER_CONTAINER_NAME := $(if $(CONTAINER_NAME),--name $(CONTAINER_NAME),)

DOCKER_BUILD_ARGS += --build-arg=GO_VERSION
DOCKER_BUILD_ARGS += --build-arg=SWAGGER_VERSION

BUILD_CMD := $(BUILDX) build

SWAGGER_DOCS_PORT ?= 9000

.DEFAULT_GOAL := help

.PHONY: build
build:
	$(BUILD_CMD) $(DOCKER_BUILD_ARGS) \
		--target dev \
		--load \
		-t "$(DOCKER_IMAGE)" \
		-f Dockerfile \
		.

.PHONY: swagger-gen
swagger-gen: build ## generate swagger.yaml and API types
	$(DOCKER_RUN) ./scripts/generate-swagger-api.sh

.PHONY: swagger-spec
swagger-spec: build ## generate the Swagger 2.0 compatibility specification
	$(DOCKER_RUN) ./scripts/generate-swagger-spec.sh

.PHONY: swagger-docs
swagger-docs: ## preview the API documentation
	@echo "API docs preview will be running at http://localhost:$(SWAGGER_DOCS_PORT)"
	@docker run --rm \
		-v ./:/usr/share/nginx/html/swagger/ \
		-e 'REDOC_OPTIONS=hide-hostname="true" lazy-rendering' \
		-e SPEC_URL="swagger/openapi.yaml" \
		-p $(SWAGGER_DOCS_PORT):80 \
		redocly/redoc:v2.5.3

.PHONY: validate-swagger
validate-swagger: build ## validate OpenAPI and the generated Swagger specification
	$(DOCKER_RUN) ./scripts/validate-swagger.sh

.PHONY: validate-swagger-gen
validate-swagger-gen: build ## validate generated types are up-to-date
	$(DOCKER_RUN) ./scripts/validate-swagger-gen.sh

.PHONY: help
help: ## display this help message
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_-]+:.*?## / {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
