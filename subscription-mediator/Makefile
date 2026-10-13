# subscription-mediator
#
# Targets WSO2 API Portal 1.0.0, WSO2 API Platform Gateway 1.2.0 and
# WSO2 API Manager 4.7.0.
#
#   make build                      build the Docker image for this machine
#   make build-and-push-multiarch   build linux/amd64 + linux/arm64 and push
#   make test                       run the test suite
#   make help                       list the targets
#
# The service is started with Docker Compose, not from here:
#
#   edit configs/config.toml   # the webhook secret and the gateway's address
#   make build
#   docker compose up -d
#   docker compose logs -f
#
# Publishing a multi-arch image:
#
#   docker login ghcr.io
#   make build-and-push-multiarch DOCKER_REGISTRY=ghcr.io/<org> VERSION=1.0.0

# Image version. Defaults to the short commit; set VERSION=x.y.z for a release.
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")

# Registry the image is pushed to, e.g. ghcr.io/<org> or docker.io/<user>.
# Optional for `make build`; required for `make build-and-push-multiarch`.
DOCKER_REGISTRY ?=
IMAGE_NAME := $(if $(DOCKER_REGISTRY),$(DOCKER_REGISTRY)/,)subscription-mediator

# The tag docker-compose.yaml runs. `make build` always applies it, so
# `docker compose up` starts the image just built rather than a stale one.
COMPOSE_IMAGE := subscription-mediator:dev

# Extra ':latest' tag applied on multi-arch pushes. Set PUSH_LATEST=false to
# publish a version tag without moving :latest.
LATEST_TAG_ARG := $(if $(filter false,$(PUSH_LATEST)),,-t $(IMAGE_NAME):latest)

# Multi-arch builds need a docker-container buildx builder; the default docker
# driver cannot export a multi-platform image. Created on first use.
PLATFORMS ?= linux/amd64,linux/arm64
BUILDX_BUILDER ?= subscription-mediator-builder

.PHONY: help build build-and-push-multiarch test

help: ## Show this help message
	@echo 'Usage: make [target] [VERSION=x.y.z] [DOCKER_REGISTRY=registry/org]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-26s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the Docker image for this machine's platform
	@echo "Building Docker image $(IMAGE_NAME):$(VERSION)..."
	docker buildx build \
		-t $(IMAGE_NAME):$(VERSION) \
		-t $(COMPOSE_IMAGE) \
		--load \
		.
	@echo "Built $(IMAGE_NAME):$(VERSION) (also tagged $(COMPOSE_IMAGE) for docker compose)."

build-and-push-multiarch: ## Build and push a multi-arch image (linux/amd64, linux/arm64)
	@if [ -z "$(DOCKER_REGISTRY)" ]; then \
		echo "Error: DOCKER_REGISTRY is required, e.g." >&2; \
		echo "  make build-and-push-multiarch DOCKER_REGISTRY=ghcr.io/<org> VERSION=1.0.0" >&2; \
		exit 1; \
	fi
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container >/dev/null
	@echo "Building and pushing $(IMAGE_NAME):$(VERSION) for $(PLATFORMS)..."
	docker buildx build \
		--builder $(BUILDX_BUILDER) \
		--platform $(PLATFORMS) \
		-t $(IMAGE_NAME):$(VERSION) \
		$(LATEST_TAG_ARG) \
		--push \
		.

test: ## Run the test suite
	go test ./...
