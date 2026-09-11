# syntax=docker/dockerfile:1

# The build context is THIS directory. The module depends on nothing else in the
# repository, so the whole component builds from its own directory:
#
#   docker build -t subscription-mediator:dev .
#
# Or just `make build`, which runs `docker compose build` and gets this right.

FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o /out/subscription-mediator ./cmd/subscription-mediator

FROM alpine:3.21

# ca-certificates for an HTTPS gateway; wget (busybox) backs the healthcheck.
RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=build /out/subscription-mediator /app/subscription-mediator

# config.toml is bind-mounted at run time; the image ships without one so it
# cannot start with a stale or half-filled copy baked in.
#
# Mount a volume at /app/data to keep the mappings and the pending event queue
# across restarts. Without it a restart loses the queue, and the API Portal
# never redelivers.
RUN mkdir -p /app/data

EXPOSE 8085
ENTRYPOINT ["/app/subscription-mediator"]
CMD ["-config", "/app/config.toml"]
