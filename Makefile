.PHONY: build test test-race vet fmt fmt-check integration integration-race multiinstance migrate-up migrate-down migrate-version up down

GO ?= go

build:
	$(GO) build -o bin/ ./cmd/...

test:
	$(GO) test ./...

# -race needs cgo (a C toolchain such as gcc).
test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...
	$(GO) vet -tags=multiinstance ./...

fmt:
	gofmt -w .

# Fails if any file needs formatting.
fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

# Needs a running Docker daemon (testcontainers).
integration:
	$(GO) test -tags=integration -count=1 -v ./test/integration/...

integration-race:
	$(GO) test -tags=integration -race -count=1 -v ./test/integration/...

# Starts its own three-instance compose stack; the dev stack must be down.
multiinstance:
	$(GO) test -tags=multiinstance -count=1 -v -timeout 40m ./test/multiinstance/...

# Needs MIGRATE_DATABASE_URL (see .env.example).
migrate-up:
	$(GO) run ./cmd/migrate up

migrate-down:
	$(GO) run ./cmd/migrate down

migrate-version:
	$(GO) run ./cmd/migrate version

up:
	docker compose up --build

down:
	docker compose down -v
