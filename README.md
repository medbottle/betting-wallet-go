# betting-wallet-go

Distributed processing of bets and wallet movements in Go (Uber Fx, PostgreSQL,
SQS, Keycloak). Spec: [docs/CHALLENGE.md](docs/CHALLENGE.md). Design decisions:
[ARCHITECTURE.md](ARCHITECTURE.md).

> **Status: slice 10 (metrics, multi-instance tests, failure simulations).**
> The authenticated HTTP API, the SQS consumer (with inbox, DLQ and the
> `dlqredrive` tool), the reference worker and the outbox relay run on the
> full schema, domain and persistence adapter; committed events are
> published to `wallet-events.fifo`; Prometheus metrics are served on
> `:9090`; the CHALLENGE §13 scenarios run against three instances.
> Interpretations, limitations and unfinished work: ARCHITECTURE.md,
> "Interpretações adotadas", "Premissas e limitações" and "Trabalho não
> concluído".

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

Each role uses its own broker credentials: the consumer, the outbox
publisher, the `dlqredrive` operator and each provider's producer. Their
least-privilege IAM policies, and the queue policy that lets only the
producers and the operator send to `wager-transactions.fifo`, are in
[deploy/aws/iam-policies.json](deploy/aws/iam-policies.json) for a real AWS
account. MiniStack
enforces IAM for SQS only when started with `AUTH=true`, which this project
does not use: in that mode the emulator generates the access keys, so they
would have to be provisioned for every role and read by the app and the
tests. In the default mode used here it accepts any credentials and
evaluates no policy, so locally the separation is configured but not
enforced, and the policies file stays the AWS definition (ARCHITECTURE.md
§10, assumption A12).

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

### Inbound operations (SQS)

Providers can also submit operations through `wager-transactions.fifo`. The
consumer runs the same use case as `POST /wagering/transactions`, with the
same idempotency guarantees, plus an inbox keyed by the envelope's
`messageId` (ARCHITECTURE §9, §10, §23).

Message body (CHALLENGE §10); `data` takes the same fields as the HTTP body
plus `idempotencyKey`. Unknown fields are refused, and amounts must be
strings:

```json
{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}
```

The provider a message acts for comes from the sender's broker identity,
never from `data.providerId` alone (ARCHITECTURE §10, "Identidade do
produtor"). The consumer reads the `SenderId` SQS reports and maps it to a
provider with `SQS_PROVIDER_SENDERS`; `data.providerId` must name that
provider. In AWS the sender id is the producer's IAM user or role id.
MiniStack reports the caller's account as `SenderId` and treats a 12-digit
access key as that account, so locally each producer sends with its own
12-digit key:

| Producer | Access key / secret (local test values) | Acts for |
| --- | --- | --- |
| provider-a | `111111111111` / `provider-a-broker-local-secret` | `provider-a` |
| provider-b | `222222222222` / `provider-b-broker-local-secret` | `provider-b` |

MiniStack does not authenticate keys in the mode used here, so this shows
the consumer's sender-to-provider check, not broker authentication, which is
IAM's job in AWS.

| Item | Value |
| --- | --- |
| Sender | a key mapped to no provider goes to the DLQ as `FORBIDDEN`; a `data.providerId` of another provider as `PROVIDER_MISMATCH`; nothing is persisted |
| `MessageGroupId` | the `walletId`: per-wallet order, different wallets in parallel |
| `MessageDeduplicationId` | the envelope `messageId` (best effort; the inbox and the database constraints are the guarantee) |
| Visibility timeout / handler deadline | 60s, set on every receive / 15s; after a group's first message, the next starts only while its deadline plus 5s still fits the visibility, otherwise it and the rest of the group are released for redelivery |
| Attempts | transient failures retry with visibility `min(2^receiveCount, 60)`s; after 10 receives the redrive policy moves the message to `wager-transactions-dlq.fifo` |
| Readiness | polling pauses while `/health/ready` fails, so an outage does not use up attempts |
| Invalid or permanent | sent to the DLQ at once with `failureClass=PERMANENT`, `failureCode`, `originalMessageId`, `envelopeMessageId` and `originalSenderId` (the sender it was attributed to), then deleted; nothing is persisted |
| Business rejection | committed as `REJECTED`, message deleted |
| Duplicate `messageId` | same payload: deleted without effect; different payload: DLQ (`INVALID_REQUEST`) |

The message is deleted only after its transaction committed. Follow the
outcome with `GET /providers/{providerId}/wagering/transactions/{externalId}`.

Send one from the host as provider-a's producer:

```sh
AWS_ACCESS_KEY_ID=111111111111 AWS_SECRET_ACCESS_KEY=provider-a-broker-local-secret aws --endpoint-url http://localhost:4566 --region us-east-1 sqs send-message --queue-url http://localhost:4566/000000000000/wager-transactions.fifo --message-group-id <walletId> --message-deduplication-id msg-123 --message-body file://message.json
```

#### DLQ redrive procedure

1. Confirm the cause is resolved: `/health/ready` answers 200.
2. Messages with `failureClass=PERMANENT` need a fix by the producer; they are
   not resent as they are. Inspect them with `receive-message` on
   `wager-transactions-dlq.fifo` (`--message-attribute-names All`).
3. Resend the others (transient attempts exhausted) with `dlqredrive`, which
   keeps the body and `MessageGroupId`, uses
   `MessageDeduplicationId=<MessageId>:redrive:<unixSeconds>`, names the
   original producer in the `originalSenderId` attribute (the consumer
   accepts it only from the operator's identity, `SQS_OPERATOR_SENDER_ID`),
   deletes each message from the DLQ after the send, and skips the permanent
   ones. Locally the operator's 12-digit key is its own MiniStack account, so
   it finds the queues in their owner's account with
   `SQS_QUEUE_OWNER_ACCOUNT_ID`:

   ```sh
   docker compose run --rm --no-deps --entrypoint /app/dlqredrive -e SQS_OPERATOR_ACCESS_KEY_ID=333333333333 -e SQS_OPERATOR_SECRET_ACCESS_KEY=wallet-operator-local-secret -e SQS_QUEUE_OWNER_ACCOUNT_ID=000000000000 app
   ```

   It prints `redriven=N skipped_permanent=M`. Skipped messages stay in
   flight for the DLQ visibility timeout (60s); because of FIFO order, later
   messages of the same group may need a second run after that.
4. Redriving is safe: the envelope `messageId` does not change, so the inbox
   and idempotency absorb anything already processed.

### Metrics

Prometheus text format on a separate listener (`METRICS_ADDR`, default
`:9090`; compose publishes it on the host):

```sh
curl -s http://localhost:9090/metrics | grep -E '^(wager_|outbox_|sqs_|retries_|db_|reconciliation_|http_request)'
```

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `wager_transactions_total` | counter | `transport`, `status`, `failure_code` | operations that reached a new state (http, sqs, worker) |
| `wager_duplicates_total` | counter | `transport`, `kind` | replays by idempotency key and inbox duplicates |
| `wager_conflicts_total` | counter | `code` | idempotency, external id and wallet conflicts |
| `db_concurrency_conflicts_total` | counter | `sqlstate` | rollbacks by lock timeout, deadlock, serialization failure or statement timeout |
| `retries_total` | counter | `component` | consumer, reference worker and outbox retries |
| `sqs_dlq_messages_total` | counter | `failure_code` | messages sent to the DLQ explicitly |
| `outbox_parked_total` | counter | — | events parked after exhausting attempts |
| `outbox_unpublished` / `outbox_lag_seconds` | gauge | — | outbox backlog and age of the oldest unpublished event |
| `wager_processing_duration_seconds` | histogram | `transport`, `outcome` | duration of a submission or worker iteration |
| `http_request_duration_seconds` | histogram | `route`, `code` | HTTP latency by route pattern |
| `reconciliation_divergences_total` | counter | — | reconciliations with `consistent=false` |

Alert on `wager_transactions_total{status="FAILED"}`, `outbox_parked_total`,
`outbox_lag_seconds`, `sqs_dlq_messages_total` and
`reconciliation_divergences_total`. Labels never carry wallet, transaction or
provider ids. Details: ARCHITECTURE.md §25.

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
missing, invalid or expired. If the token is signed with a key the service
has not cached and Keycloak cannot be reached to fetch it, the answer is
503 `TEMPORARILY_UNAVAILABLE` (retryable, `Retry-After: 2`) instead
(ARCHITECTURE §24). The providerId a provider may act for comes only
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
| `DB_STATEMENT_TIMEOUT` | `10s` | app | default `statement_timeout` of every transaction; a cancelled statement is transient (ARCHITECTURE §24) |
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
| `SQS_INBOUND_QUEUE` / `SQS_INBOUND_DLQ` | `wager-transactions.fifo` / `wager-transactions-dlq.fifo` | app, dlqredrive | inbound queue and its DLQ |
| `SQS_CONSUMER_ACCESS_KEY_ID` / `SQS_CONSUMER_SECRET_ACCESS_KEY` | — (required) | app | consumer-only credentials (ARCHITECTURE §10) |
| `SQS_PROVIDER_SENDERS` | — (required with `RUN_CONSUMER`) | app | `senderId=providerId` pairs, comma separated: the producer identities the consumer trusts (ARCHITECTURE §10) |
| `SQS_OPERATOR_SENDER_ID` | — (required with `RUN_CONSUMER`; required by dlqredrive) | app, dlqredrive | the redrive operator's sender id, the only one whose `originalSenderId` is trusted |
| `SQS_OPERATOR_ACCESS_KEY_ID` / `SQS_OPERATOR_SECRET_ACCESS_KEY` | — (required) | dlqredrive | operator credentials for the redrive tool (ARCHITECTURE §23) |
| `SQS_QUEUE_OWNER_ACCOUNT_ID` | — (the operator's account) | dlqredrive | account that owns the queues, when the operator's is another one (locally `000000000000`) |
| `CONSUMER_WAIT_SECONDS` | `20` | app | long-poll wait (0-20) |
| `CONSUMER_MAX_MESSAGES` | `10` | app | messages per receive (1-10) |
| `CONSUMER_HANDLER_TIMEOUT` | `15s` | app | deadline of one message |
| `CONSUMER_VISIBILITY_TIMEOUT` | `60s` | app | visibility set on every receive; whole seconds, at least the handler deadline + 5s |
| `CONSUMER_BACKOFF_MAX` | `60s` | app | longest visibility after a transient failure |
| `CONSUMER_PAUSE_INTERVAL` | `1s` | app | readiness recheck while polling is paused |
| `OUTBOX_LEASE` | `30s` | app | claim lease (ARCHITECTURE §9) |
| `OUTBOX_BATCH_SIZE` | `10` | app | events per `SendMessageBatch` (max 10) |
| `OUTBOX_PUBLISH_TIMEOUT` | `5s` | app | timeout of one publish call |
| `OUTBOX_BACKOFF_BASE` / `OUTBOX_BACKOFF_MAX` | `1s` / `5m` | app | retry backoff |
| `OUTBOX_MAX_ATTEMPTS` | `20` | app | attempts before an event is parked |
| `OUTBOX_POLL_INTERVAL` | `1s` | app | idle wait of the relay |
| `OUTBOX_RELAY_CONCURRENCY` | `1` | app | relay goroutines per instance |
| `HTTP_ADDR` | `:8081` | app | listen address |
| `METRICS_ADDR` | `:9090` | app | Prometheus listener; empty disables it (ARCHITECTURE §25) |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | app | header read timeout |
| `HTTP_HANDLER_TIMEOUT` | `10s` | app | per-request handler timeout |
| `READINESS_INTERVAL` | `2s` | app | readiness loop period |
| `READINESS_CHECK_TIMEOUT` | `1s` | app | timeout for each readiness check |
| `READINESS_DRAIN_DELAY` | `2s` | app | how long `/health/ready` reports draining before shutdown |
| `LOG_LEVEL` | `info` | app | `debug`, `info`, `warn`, `error` |
| `RUN_HTTP` | `true` | app | serve HTTP |
| `RUN_CONSUMER` | `true` | app | SQS consumer |
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
| `go test -tags=integration -count=1 ./test/integration/...` | real Postgres via testcontainers: migrations up/down, Fx start/stop with goroutine-leak check, every schema constraint, trigger and grant, the repositories, wallet locking and transaction atomicity, token verification against a real Keycloak (started once per run), the HTTP API end to end including the concurrency scenarios of CHALLENGE §13, the reference worker (late references, expiry, contention, abandoned claims, restart), the outbox relay against a real MiniStack (publishing, contention, crash recovery, backoff, parking), the SQS consumer against a real MiniStack (inbox, duplicates, DLQ, HTTP/SQS crossover, crash before delete, transient retries and redrive, readiness pause, graceful stop) | Docker |
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
- `multiinstance`: three-process tests on the compose stack (below).

### Multi-instance tests and failure simulations

`test/multiinstance` runs the CHALLENGE §13 scenarios against three
independent app processes (`app`, `app-2`, `app-3` from
`docker-compose.multi.yml`, on ports 8081-8083, metrics on 9090-9092) sharing
Postgres, Keycloak and MiniStack, and simulates the failures of CHALLENGE §3:

| Test | What it shows |
| --- | --- |
| `TestSameBet50TimesAcrossInstances` | the same BET 50 times over three HTTP APIs and SQS: one debit |
| `TestTwoBetsOf80AcrossInstances` | 100.00 vs 2 x 80.00 through different instances (five rounds): one processed, one `INSUFFICIENT_FUNDS`, 20.00 left |
| `TestDistinctWalletsInParallel` | other wallets progress on every instance while one wallet is locked |
| `TestHTTPAndSQSCrossoverAcrossInstances` | the same operation over SQS and three HTTP APIs at once: one effect |
| `TestConsumerKilledUnderLoad` | SIGKILL of a consuming instance: redeliveries are inbox duplicates, one effect each |
| `TestOutboxRelaysKilledUnderLoad` | SIGKILL of a relaying instance under load: every committed event reaches `wallet-events.fifo` |
| `TestReferenceResolvedAfterInstanceKill` | pending references held by a killed instance are resolved or expired by another |
| `TestRestartAllInstances` | all three killed and restarted: replays, pending references and queued messages survive |
| `TestPostgresOutage` | Postgres stopped: readiness 503, HTTP 503 retryable, consumers paused, no DLQ; recovery |
| `TestSQSOutage` | MiniStack paused: HTTP keeps committing, events wait in the outbox and are all published after recovery |

Each test ends with the invariant checks: nothing left pending, reconciliation
consistent with the expected balance, one ledger entry per money-moving
processed operation, outbox drained with nothing parked, and every committed
`eventId` received on `wallet-events.fifo`.

Prepare: Docker running, the dev stack stopped (`docker compose down`), ports
8080-8083, 4566, 5432 and 9090-9092 free. The suite builds and starts the
stack itself under the project `wager-wallet-multi` and removes it at the end
(`MULTIINSTANCE_KEEP_STACK=1` keeps it):

```sh
go test -tags=multiinstance -count=1 -v -timeout 40m ./test/multiinstance/
```

It takes several minutes: messages held by a killed consumer return only
after the 60s visibility timeout, and the outages last about 15s each. The
same stack can be started by hand to explore:

```sh
docker compose -p wager-wallet-multi -f docker-compose.yml -f docker-compose.multi.yml up -d --build
```

The suite drives the stack from the host with `docker compose`, so it runs
without `-race` here (no local C toolchain, and the Go container used for
`-race` has no Docker CLI). The processes under test are the image's
binaries, which are not race-instrumented in any case; `-race` covers the
unit and integration suites.

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
