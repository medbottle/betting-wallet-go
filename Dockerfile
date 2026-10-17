# syntax=docker/dockerfile:1

# Go version is pinned here and in go.mod (go 1.27.2).
FROM golang:1.27.2-alpine3.24 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/wallet ./cmd/wallet && \
    go build -ldflags="-s -w" -o /out/migrate ./cmd/migrate && \
    go build -ldflags="-s -w" -o /out/dlqredrive ./cmd/dlqredrive

# Extra, not required by the challenge (ARCHITECTURE §26): the same three
# binaries built with the race detector, for the multi-instance suite only
# (docker-compose.race.yml, MULTIINSTANCE_RACE=1). -race needs cgo and
# glibc, hence the Debian images. Built only when targeted; never deployed.
FROM golang:1.27.2-bookworm AS build-race
WORKDIR /src
ENV CGO_ENABLED=1 GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -race -o /out/wallet ./cmd/wallet && \
    go build -race -o /out/migrate ./cmd/migrate && \
    go build -race -o /out/dlqredrive ./cmd/dlqredrive

FROM debian:12.15-slim AS race
COPY --from=build-race /out/wallet /out/migrate /out/dlqredrive /app/
USER 65532:65532
EXPOSE 8081
ENTRYPOINT ["/app/wallet"]

# One image, three binaries (the default target, the last stage): the compose "migrate" service overrides the
# entrypoint with /app/migrate; /app/dlqredrive is the DLQ redrive tool.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/wallet /out/migrate /out/dlqredrive /app/
USER nonroot:nonroot
EXPOSE 8081
ENTRYPOINT ["/app/wallet"]
