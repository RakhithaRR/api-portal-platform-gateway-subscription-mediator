# subscription-mediator
#
# Targets WSO2 API Portal 1.0.0, WSO2 API Platform Gateway 1.2.0 and
# WSO2 API Manager 4.7.0.
#
#   make build    build the Docker image
#   make test     run the test suite
#
# The service is started with Docker Compose, not from here:
#
#   edit configs/config.toml   # the webhook secret and the gateway's address
#   make build
#   docker compose up -d
#   docker compose logs -f

.PHONY: build test

# Builds through Compose so the image tag, build args and build context are the
# same ones `docker compose up` runs. A plain `docker build` would need the tag
# kept in sync by hand, and a mismatch leaves compose starting a stale image.
build:
	docker compose build

test:
	go test ./...
