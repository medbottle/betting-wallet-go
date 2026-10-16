# wager-wallet-service

Distributed processing of bets and wallet movements in Go (Uber Fx, PostgreSQL,
SQS, Keycloak). Spec: [docs/CHALLENGE.md](docs/CHALLENGE.md). Design decisions:
[ARCHITECTURE.md](ARCHITECTURE.md).

> **Status: slice 1 (skeleton and infrastructure).** Only the health endpoints
> and the migration tooling exist. There is no domain logic, business API, SQS
> consumer or real schema yet.

## Prerequisites

| Tool | Version | Used for |
| --- | --- | --- |
| Go | 1.27.2 (pinned in `go.mod` and `Dockerfile`) | build, unit tests |
| Docker + Docker Compose v2 | recent | local stack, integration tests |
| C toolchain (gcc) | any | only for `go test -race` (cgo) |
| make | optional | shortcuts below; every target is a plain command |

## Local stack

```sh
docker compose up --build
```

This starts the services in dependency order:

| Service | Image | Host port | Ready when |
| --- | --- | --- | --- |
| `postgres` | `postgres:18.6-alpine3.24` | 5432 | `pg_isready` over TCP on db `wallet` |
| `keycloak` | `quay.io/keycloak/keycloak:26.7.5` | 8080 | `/health/ready` on management port 9000 |
| `ministack` | `ministackorg/ministack:1.5.27` | 4566 | gateway healthy and queues created |
| `migrate` | built from `Dockerfile` | — | one-shot `migrate up`, exits 0 |
| `app` | built from `Dockerfile` | 8081 | after all of the above |

```sh
curl -i http://localhost:8081/health/live
curl -i http://localhost:8081/health/ready
```

Stop and wipe the data: `docker compose down -v`.

Every compose variable has a local default. To override one, run
`cp .env.example .env` and edit it.

### Queues

On startup, `deploy/ministack/init-queues.sh` (run by MiniStack from
`/etc/localstack/init/ready.d`) creates:

| Queue | Visibility | Retention | Redrive |
| --- | --- | --- | --- |
| `wager-transactions.fifo` | 60s | 4 days | to `wager-transactions-dlq.fifo` after 10 receives |
| `wager-transactions-dlq.fifo` | 60s | 14 days | — |
| `wallet-events.fifo` | 30s | 4 days | to `wallet-events-dlq.fifo` after 10 receives |
| `wallet-events-dlq.fifo` | 30s | 14 days | — |

All queues are FIFO and have content-based deduplication turned off. Producers
always send an explicit `MessageDeduplicationId`.

To list them from the host (any AWS CLI, with dummy credentials):

```sh
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test aws --endpoint-url http://localhost:4566 --region us-east-1 sqs list-queues
```

### Keycloak

Keycloak only starts in this slice, with admin `admin`/`admin` at
http://localhost:8080. The `wagering` realm, its clients and the test identities
are provisioned in the auth slice.

## Environment variables

`.env.example` documents every variable with its local value.

| Variable | Default | Used by | Meaning |
| --- | --- | --- | --- |
| `DATABASE_URL` | — (required) | app | Postgres URL for the `wallet_app` role |
| `MIGRATE_DATABASE_URL` | — (required) | migrate | Postgres URL for the `wallet_owner` role |
| `DB_MAX_CONNS` | `10` | app | pgxpool size |
| `DB_STARTUP_TIMEOUT` | `10s` | app | bound on the startup ping and schema check |
| `HTTP_ADDR` | `:8081` | app | listen address |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | app | header read timeout |
| `HTTP_HANDLER_TIMEOUT` | `10s` | app | per-request handler timeout |
| `READINESS_INTERVAL` | `2s` | app | readiness loop period |
| `READINESS_CHECK_TIMEOUT` | `1s` | app | timeout for each readiness check |
| `READINESS_DRAIN_DELAY` | `2s` | app | how long `/health/ready` reports draining before shutdown |
| `LOG_LEVEL` | `info` | app | `debug`, `info`, `warn`, `error` |
| `RUN_HTTP` | `true` | app | serve HTTP |
| `RUN_CONSUMER` | `true` | app | SQS consumer (not implemented yet) |
| `RUN_OUTBOX` | `true` | app | outbox relay (not implemented yet) |
| `RUN_REFERENCE_WORKER` | `true` | app | PENDING_REFERENCE worker (not implemented yet) |

Invalid configuration makes the app exit at startup and lists every problem it
found.

## Database migrations

Migrations live in `migrations/` as `NNNN_name.up.sql` / `NNNN_name.down.sql`.
They are embedded in the binaries and applied with
[golang-migrate](https://github.com/golang-migrate/migrate) through
`cmd/migrate`. Migrations always run as the schema owner role `wallet_owner`.
The app connects as `wallet_app` and refuses to start if the schema is older
than the latest embedded migration, or if a migration is dirty.

Apply all pending migrations:

```sh
export MIGRATE_DATABASE_URL='postgres://wallet_owner:wallet_owner_local@localhost:5432/wallet?sslmode=disable'
go run ./cmd/migrate up
```

Roll back the last migration (or the last N):

```sh
go run ./cmd/migrate down
go run ./cmd/migrate down 2
```

Show the current version:

```sh
go run ./cmd/migrate version
```

The same commands are available inside the compose network:

```sh
docker compose run --rm migrate up
docker compose run --rm migrate down
docker compose run --rm migrate version
```

If a migration fails halfway, `version` reports `dirty: true` and the app will
not start. Fix the cause, repair the schema by hand, then reset the recorded
version with the upstream golang-migrate CLI (`migrate -path migrations
-database "$MIGRATE_DATABASE_URL" force N`). `cmd/migrate` does not wrap
`force` on purpose. Rolling back is safe only while
no business data depends on the migration being reverted.

The database roles are created by `deploy/postgres/init/01-roles.sh` when the
Postgres data directory is first initialised. The same script is used by the
integration tests.

## Tests

| Command | What runs | Needs |
| --- | --- | --- |
| `go test ./...` | unit tests, Fx graph validation, architecture rules | Go |
| `go test -race ./...` | the same with the race detector | Go + gcc (cgo) |
| `go vet ./...` | vet (add `-tags=integration` to vet integration code too) | Go |
| `gofmt -l .` | lists unformatted files (should print nothing) | Go |
| `go test -tags=integration -count=1 ./test/integration/...` | real Postgres via testcontainers: migrations up/down, Fx start/stop with goroutine-leak check | Docker |
| `go test -tags=integration -race -count=1 ./test/integration/...` | the same with the race detector | Docker + gcc |

Build tags:

- `integration`: tests under `test/integration` that start real containers.
- `multiinstance` (planned): three-process tests on the compose stack.

The architecture test (`test/architecture`) runs with the plain `go test ./...`.
It fails if `internal/domain` depends on fx, net/http, the AWS SDK, pgx or
database/sql, if `internal/app` depends on `internal/adapters`, or if any
package other than `internal/bootstrap` and `cmd/*` imports fx.

### Make shortcuts

`make build test test-race vet fmt fmt-check integration integration-race
migrate-up migrate-down migrate-version up down`
