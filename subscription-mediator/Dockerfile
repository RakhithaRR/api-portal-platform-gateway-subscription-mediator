# syntax=docker/dockerfile:1

# The build context is THIS directory. The module depends on nothing else in the
# repository, so the whole component builds from its own directory:
#
#   docker build -t subscription-mediator:dev .
#
# Or just `make build`, which also applies the tag docker-compose.yaml runs.

# The build stage runs on the builder's own platform and cross-compiles for the
# target one, so a multi-arch build (`make build-and-push-multiarch`) compiles
# natively instead of under QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.9-alpine AS build
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" \
    -o /out/subscription-mediator ./cmd/subscription-mediator

FROM alpine:3.24

# ca-certificates for an HTTPS gateway; wget (busybox) backs the healthcheck.
#
# The service runs as an unprivileged user with a fixed numeric UID/GID, so
# Kubernetes `runAsNonRoot` can verify it (10001:10001).
RUN apk add --no-cache ca-certificates \
 && addgroup -S -g 10001 mediator \
 && adduser -S -D -H -u 10001 -G mediator -h /app -s /sbin/nologin mediator

WORKDIR /app
COPY --from=build /out/subscription-mediator /app/subscription-mediator

# config.toml is bind-mounted at run time; the image ships without one so it
# cannot start with a stale or half-filled copy baked in.
#
# The service keeps no state and writes nothing to disk; the binary stays
# root-owned and read-only to the service user.
USER 10001:10001

EXPOSE 8085
ENTRYPOINT ["/app/subscription-mediator"]
CMD ["-config", "/app/config.toml"]
