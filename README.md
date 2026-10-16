# betting-wallet-go

Distributed processing of bets and wallet movements in Go (Uber Fx, PostgreSQL,
SQS, Keycloak). Spec: [docs/CHALLENGE.md](docs/CHALLENGE.md). Design decisions:
[ARCHITECTURE.md](ARCHITECTURE.md).

> **Status: slice 8 (outbox relay).** The authenticated HTTP API, the
> reference worker and the outbox relay run on the full schema, domain and
> persistence adapter: committed events are published to
> `wallet-events.fifo`. The SQS consumer and metrics are not built yet. Open
> points: [docs/OPEN-QUESTIONS.md](docs/OPEN-QUESTIONS.md); parked
> assumptions: [docs/ASSUMPTIONS.md](docs/ASSUMPTIONS.md).

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

### Outbound events

Committed outbox rows are published to `wallet-events.fifo` by the outbox
relay: `MessageGroupId` is the wallet id, `MessageDeduplicationId` the
`eventId`, the body is the event envelope and `eventType`/`eventVersion` are
message attributes. Delivery is at-least-once; deduplicate by `eventId` and
order per wallet by `walletVersion`. Full contract: ARCHITECTURE.md §22.

Read them from the host:

```sh
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test aws --endpoint-url http://localhost:4566 --region us-east-1 sqs receive-message --queue-url http://localhost:4566/000000000000/wallet-events.fifo --max-number-of-messages 10 --attribute-names All --message-attribute-names All
```

`/health/ready` reports both `postgres` and `sqs`.

### Keycloak and test identities

Keycloak (admin console `admin`/`admin` at http://localhost:8080) imports the
`wagering` realm from `deploy/keycloak/realm-wagering.json` on start. Every
token has `iss=http://localhost:8080/realms/wagering` and `aud=wallet-api`.

| Client (`client_credentials`) | Secret (local test value) | Role | `provider_id` claim |
| --- | --- | --- | --- |
| `provider-a` | `provider-a-local-secret` | `provider` | `provider-a` |
| `provider-b` | `provider-b-local-secret` | `provider` | `provider-b` |
| `wallet-internal` | `wallet-internal-local-secret` | `wallet-internal` | — |

Get a token:

```sh
curl -s -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-local-secret http://localhost:8080/realms/wagering/protocol/openid-connect/token
```

Send it as `Authorization: Bearer <access_token>`. Business endpoints answer
401 with `WWW-Authenticate: Bearer error="invalid_token"` when the token is
missing, invalid or expired. The providerId a provider may act for comes only
from the `provider_id` claim. `/health/*` needs no token.

## Environment variables

`.env.example` documents every variable with its local value.

| Variable | Default | Used by | Meaning |
| --- | --- | --- | --- |
| `DATABASE_URL` | — (required) | app | Postgres URL for the `wallet_app` role |
| `MIGRATE_DATABASE_URL` | — (required) | migrate | Postgres URL for the `wallet_owner` role |
| `DB_MAX_CONNS` | `10` | app | pgxpool size |
| `DB_STARTUP_TIMEOUT` | `10s` | app | bound on the startup ping and schema check |
| `DB_LOCK_TIMEOUT` | `5s` | app | default `lock_timeout` of business transactions (ARCHITECTURE §4) |
| `OIDC_ISSUER` | — (required) | app | exact expected `iss` |
| `OIDC_JWKS_URL` | — (required) | app | JWKS endpoint (inside compose: `http://keycloak:8080/...`) |
| `OIDC_AUDIENCE` | `wallet-api` | app | required `aud` value |
| `OIDC_JWKS_MIN_REFRESH_INTERVAL` | `30s` | app | minimum gap between JWKS fetches for unknown `kid`s |
| `REFERENCE_TTL` | `10m` | app | how long an operation waits for its reference (ARCHITECTURE §8) |
| `REFERENCE_BACKOFF_BASE` | `1s` | app | first retry delay for a missing reference |
| `REFERENCE_BACKOFF_MAX` | `60s` | app | longest retry delay |
| `REFERENCE_WORKER_CONCURRENCY` | `4` | app | reference worker goroutines per instance |
| `REFERENCE_WORKER_POLL_INTERVAL` | `1s` | app | idle wait before the worker looks for due operations again |
| `SQS_ENDPOINT` | — (AWS) | app | broker endpoint override (compose: `http://ministack:4566`) |
| `AWS_REGION` | `us-east-1` | app | broker region |
| `SQS_EVENTS_QUEUE` | `wallet-events.fifo` | app | outbound events queue |
| `SQS_PUBLISHER_ACCESS_KEY_ID` / `SQS_PUBLISHER_SECRET_ACCESS_KEY` | — (required) | app | publisher-only credentials (ARCHITECTURE §10) |
| `OUTBOX_LEASE` | `30s` | app | claim lease (ARCHITECTURE §9) |
| `OUTBOX_BATCH_SIZE` | `10` | app | events per `SendMessageBatch` (max 10) |
| `OUTBOX_PUBLISH_TIMEOUT` | `5s` | app | timeout of one publish call |
| `OUTBOX_BACKOFF_BASE` / `OUTBOX_BACKOFF_MAX` | `1s` / `5m` | app | retry backoff |
| `OUTBOX_MAX_ATTEMPTS` | `20` | app | attempts before an event is parked |
| `OUTBOX_POLL_INTERVAL` | `1s` | app | idle wait of the relay |
| `OUTBOX_RELAY_CONCURRENCY` | `1` | app | relay goroutines per instance |
| `HTTP_ADDR` | `:8081` | app | listen address |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | app | header read timeout |
| `HTTP_HANDLER_TIMEOUT` | `10s` | app | per-request handler timeout |
| `READINESS_INTERVAL` | `2s` | app | readiness loop period |
| `READINESS_CHECK_TIMEOUT` | `1s` | app | timeout for each readiness check |
| `READINESS_DRAIN_DELAY` | `2s` | app | how long `/health/ready` reports draining before shutdown |
| `LOG_LEVEL` | `info` | app | `debug`, `info`, `warn`, `error` |
| `RUN_HTTP` | `true` | app | serve HTTP |
| `RUN_CONSUMER` | `true` | app | SQS consumer (not implemented yet) |
| `RUN_OUTBOX` | `true` | app | outbox relay |
| `RUN_REFERENCE_WORKER` | `true` | app | PENDING_REFERENCE worker |

Invalid configuration makes the app exit at startup and lists every problem it
found.

## API examples

Contracts and status codes: ARCHITECTURE.md §13 and §20. Every business
route needs a bearer token (see above); `X-Correlation-Id` is optional and
echoed back.

```sh
INTERNAL=$(curl -s -d grant_type=client_credentials -d client_id=wallet-internal -d client_secret=wallet-internal-local-secret http://localhost:8080/realms/wagering/protocol/openid-connect/token | jq -r .access_token)
PROVIDER=$(curl -s -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-local-secret http://localhost:8080/realms/wagering/protocol/openid-connect/token | jq -r .access_token)
```

Open a wallet (internal service; 201):

```sh
curl -s -X POST http://localhost:8081/wallets -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'
```

Submit a bet (provider; replace `<walletId>`). Resending the same request
returns the stored outcome with `"idempotentReplay": true`:

```sh
curl -s -X POST http://localhost:8081/wagering/transactions -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' -H 'Idempotency-Key: provider-a:transaction-123' -d '{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<walletId>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
```

Reads and reconciliation:

```sh
curl -s http://localhost:8081/providers/provider-a/wagering/transactions/transaction-123 -H "Authorization: Bearer $PROVIDER"
```

```sh
curl -s "http://localhost:8081/wallets/<walletId>/ledger?limit=50" -H "Authorization: Bearer $INTERNAL"
```

```sh
curl -s -X POST http://localhost:8081/wallets/<walletId>/reconciliation -H "Authorization: Bearer $INTERNAL"
```

## Database migrations

Migrations live in `migrations/` as `NNNN_name.up.sql` / `NNNN_name.down.sql`,
one per table:

| Version | Creates |
| --- | --- |
| 0001 | `schema_info` (pipeline check) |
| 0002 | `providers`, seeded with `provider-a` and `provider-b` |
| 0003 | `wallets` |
| 0004 | `wager_transactions` |
| 0005 | `wallet_ledger_entries` (append-only) |
| 0006 | `inbox_messages` |
| 0007 | `outbox_events` |

Each migration carries its table's constraints, trigger guards and grants for
`wallet_app` (see ARCHITECTURE.md §16).

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

`migrate` is a dependency of `app`, so `docker compose up` and
`docker compose start app` run `migrate up` again before the app starts. To
start the app against a rolled-back schema (for example, to watch it refuse to
start), use `docker start wager-wallet-app-1`.

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
| `go test -tags=integration -count=1 ./test/integration/...` | real Postgres via testcontainers: migrations up/down, Fx start/stop with goroutine-leak check, every schema constraint, trigger and grant, the repositories, wallet locking and transaction atomicity, token verification against a real Keycloak (started once per run), the HTTP API end to end including the concurrency scenarios of CHALLENGE §13, the reference worker (late references, expiry, contention, abandoned claims, restart), the outbox relay against a real MiniStack (publishing, contention, crash recovery, backoff, parking) | Docker |
| `go test -tags=integration -race -count=1 ./test/integration/...` | the same with the race detector | Docker + gcc |

With no local C toolchain (for example, on Windows without gcc), run the race
detector in a Linux Go container. The Docker socket lets testcontainers start
its sibling containers:

```sh
docker run --rm -v "$PWD:/src" -w /src \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal \
  --add-host=host.docker.internal:host-gateway \
  golang:1.27.2-bookworm go test -race -tags=integration -count=1 ./...
```

Build tags:

- `integration`: tests under `test/integration` that start real containers.
- `multiinstance` (planned): three-process tests on the compose stack.

The architecture test (`test/architecture`) runs with the plain `go test ./...`.
It fails if:

- `internal/domain` depends on fx, net/http, the AWS SDK, pgx or database/sql,
  or on anything outside the standard library, google/uuid and itself;
- domain code (outside tests) uses `float32`/`float64`, `strconv` float
  functions or `panic`;
- `internal/app` depends on `internal/adapters`;
- any package other than `internal/bootstrap` and `cmd/*` imports fx.

The fuzz test for Money parsing runs its seed corpus under `go test`; to fuzz
for longer:

```sh
go test -run=^$ -fuzz=FuzzParseRoundTrip -fuzztime=1m ./internal/domain/money/
```

### Make shortcuts

`make build test test-race vet fmt fmt-check integration integration-race
migrate-up migrate-down migrate-version up down`
