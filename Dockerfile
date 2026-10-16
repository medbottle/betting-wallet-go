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

# One image, three binaries: the compose "migrate" service overrides the
# entrypoint with /app/migrate; /app/dlqredrive is the DLQ redrive tool.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/wallet /out/migrate /out/dlqredrive /app/
USER nonroot:nonroot
EXPOSE 8081
ENTRYPOINT ["/app/wallet"]
