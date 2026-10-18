# Guia de execução e operação

Como executar, configurar, operar e testar o `betting-wallet-go`. A visão geral está no [README](../README.md); as decisões de arquitetura, em [ARCHITECTURE.md](../ARCHITECTURE.md); a especificação, em [CHALLENGE.md](CHALLENGE.md).

## Sumário

1. [Pré-requisitos](#1-pré-requisitos)
2. [Ambiente local](#2-ambiente-local)
3. [Filas e identidades do broker](#3-filas-e-identidades-do-broker)
4. [Eventos de saída](#4-eventos-de-saída)
5. [Operações de entrada pelo SQS](#5-operações-de-entrada-pelo-sqs)
6. [Métricas](#6-métricas)
7. [Keycloak e identidades de teste](#7-keycloak-e-identidades-de-teste)
8. [Exemplos de chamadas da API](#8-exemplos-de-chamadas-da-api)
9. [Migrations](#9-migrations)
10. [Variáveis de ambiente](#10-variáveis-de-ambiente)
11. [Testes](#11-testes)
12. [Limitações conhecidas](#12-limitações-conhecidas)

## 1. Pré-requisitos

| Ferramenta | Versão | Uso |
| --- | --- | --- |
| Go | 1.27.2 (fixada no `go.mod` e no `Dockerfile`) | build, testes unitários |
| Docker + Docker Compose v2 | recente | ambiente local, testes de integração |
| Compilador C (gcc) | qualquer | só para `go test -race` (cgo) |
| make | opcional | atalhos; todo alvo é um comando simples |
| curl e jq | qualquer | os exemplos autenticados da API (`jq` extrai o token) |
| AWS CLI v2 | opcional | os exemplos de SQS a partir do host; sem ele, rode-os dentro do container do MiniStack ([seção 3](#3-filas-e-identidades-do-broker)) |

**Shell e plataforma.** Todos os comandos deste guia são para um shell POSIX (bash): Linux, macOS ou, no Windows, Git Bash ou WSL. PowerShell e `cmd` não os executam como estão escritos (aspas do bash, `$(...)`, continuação de linha com `\`). No Git Bash, prefixe com `MSYS_NO_PATHCONV=1` os comandos `docker` que passam um caminho de container (`-w /src`, `--entrypoint /app/dlqredrive`), senão o MSYS reescreve o caminho. O comando do detector de corrida ([seção 11](#11-testes)) também tem uma forma para PowerShell. `docker compose up --build`, `go test ./...` e `go vet ./...` funcionam em qualquer shell.

## 2. Ambiente local

```sh
docker compose up --build
```

Os serviços sobem na ordem das dependências:

| Serviço | Imagem | Porta no host | Pronto quando |
| --- | --- | --- | --- |
| `postgres` | `postgres:18.6-alpine3.24` | 5432 | `pg_isready` por TCP no banco `wallet` |
| `keycloak` | `quay.io/keycloak/keycloak:26.7.5` | 8080 | `/health/ready` na porta de gerência 9000 |
| `ministack` | `ministackorg/ministack:1.5.27` | 4566 | gateway saudável, filas e identidades criadas |
| `migrate` | construída a partir do `Dockerfile` | — | `migrate up` único, termina com 0 |
| `app` | construída a partir do `Dockerfile` | 8081 | depois de todos os anteriores |

```sh
curl -i http://localhost:8081/health/live
curl -i http://localhost:8081/health/ready
```

Para parar e apagar os dados: `docker compose down -v`.

O Compose lê o `.env` só para preencher as variáveis que o `docker-compose.yml` interpola, cada uma com um padrão local: `POSTGRES_PASSWORD`, `WALLET_OWNER_PASSWORD`, `WALLET_APP_PASSWORD`, `KEYCLOAK_ADMIN`, `KEYCLOAK_ADMIN_PASSWORD`, `AWS_REGION`, `SQS_PROVIDER_SENDERS`, `SQS_OPERATOR_SENDER_ID` e `LOG_LEVEL`. As chaves do broker não estão entre elas: o MiniStack as gera no start ([seção 3](#3-filas-e-identidades-do-broker)). Para mudar uma, rode `cp .env.example .env` e edite. As demais variáveis do `.env.example` (timeouts, `REFERENCE_*`, `OUTBOX_*`, `CONSUMER_*` etc.) não chegam aos containers: defina-as no `environment` do serviço `app` no `docker-compose.yml`, ou exporte-as ao rodar os binários no host.

## 3. Filas e identidades do broker

No start, `deploy/ministack/init-queues.sh` (executado pelo MiniStack a partir de `/etc/localstack/init/ready.d`) cria:

| Fila | Visibility | Retenção | Redrive |
| --- | --- | --- | --- |
| `wager-transactions.fifo` | 60s | 4 dias | para `wager-transactions-dlq.fifo` após 10 recebimentos |
| `wager-transactions-dlq.fifo` | 60s | 14 dias | — |
| `wallet-events.fifo` | 30s | 4 dias | para `wallet-events-dlq.fifo` após 10 recebimentos |
| `wallet-events-dlq.fifo` | 30s | 14 dias | — |

Todas são FIFO, com deduplicação por conteúdo desligada. Os produtores sempre enviam um `MessageDeduplicationId` explícito.

### Identidades e credenciais

O MiniStack roda com `AUTH=true`: confere a assinatura de cada requisição e avalia as políticas de identidade e de fila (ARCHITECTURE §28). Depois das filas, o script de init cria a identidade de cada papel, com a sua política de menor privilégio:

| Identidade | Como age | Pode |
| --- | --- | --- |
| `wallet-consumer` (usuário IAM) | chave própria | receber, remover e mudar a visibilidade em `wager-transactions.fifo`; enviar à DLQ dela |
| `wallet-publisher` (usuário IAM) | chave própria | enviar a `wallet-events.fifo` |
| `provider-a-producer`, `provider-b-producer` (usuários IAM) | assumem a role `wager-producer` na conta `111111111111` / `222222222222` | enviar a `wager-transactions.fifo` |
| `wallet-operator` (usuário IAM) | assume a role `wager-operator` na conta `333333333333` | ler e remover na DLQ de entrada; enviar a `wager-transactions.fifo` |

A política da fila `wager-transactions.fifo` só permite envio às duas roles de produtor e à role do operador. Como o MiniStack gera as chaves de acesso, o script as grava em `/var/lib/wager-credentials/sqs-credentials.json`, no volume `sqs-credentials`, que a aplicação lê por `SQS_CREDENTIALS_FILE`. As mesmas políticas para uma conta AWS real estão em [deploy/aws/iam-policies.json](../deploy/aws/iam-policies.json).

As chaves mudam a cada start do MiniStack, porque ele guarda o estado em memória (como as filas): reinicie a aplicação depois de reiniciar o `ministack`. Para rodar os binários no host, copie o arquivo e aponte `SQS_CREDENTIALS_FILE` para ele. O diretório `deploy/local/` ignora tudo exceto o próprio `.gitignore`, então as chaves copiadas nunca entram no git:

```sh
docker compose cp ministack:/var/lib/wager-credentials/sqs-credentials.json ./deploy/local/sqs-credentials.json
```

`test` / `test` é a chave raiz da conta dona das filas, a administradora do emulador. Use-a só para inspecionar filas, como abaixo; ela não age por nenhum provedor. Para listar as filas a partir do host (qualquer AWS CLI):

```sh
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test aws --endpoint-url http://localhost:4566 --region us-east-1 sqs list-queues
```

Sem um AWS CLI local, rode qualquer comando `aws` deste guia dentro do container do emulador, que traz um. Passe as credenciais com `-e` e o corpo da mensagem inline, em vez de `file://`:

```sh
docker compose exec -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=test ministack aws --endpoint-url http://localhost:4566 --region us-east-1 sqs list-queues
```

## 4. Eventos de saída

O relay da outbox publica as linhas commitadas em `wallet-events.fifo`: `MessageGroupId` é o id da carteira, `MessageDeduplicationId` é o `eventId`, o corpo é o envelope do evento, e `eventType`/`eventVersion` vão como atributos da mensagem. A entrega é at-least-once: deduplique por `eventId` e ordene por carteira pelo `walletVersion`. Contrato completo: ARCHITECTURE §22.

Enquanto a checagem de readiness do SQS falha, o relay não reivindica nada, então uma queda não consome tentativas. Um evento que ainda falhe 20 vezes (`OUTBOX_MAX_ATTEMPTS`) é estacionado e contado em `outbox_parked_total`; depois de resolver a causa, devolva os eventos estacionados à fila como dono do schema:

```sh
docker compose exec postgres psql -U wallet_owner -d wallet -c "UPDATE outbox_events SET parked_at = NULL, attempts = 0, next_attempt_at = now() WHERE parked_at IS NOT NULL"
```

Eles mantêm o `eventId`, então os consumidores deduplicam qualquer entrega anterior.

Para ler os eventos a partir do host:

```sh
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test aws --endpoint-url http://localhost:4566 --region us-east-1 sqs receive-message --queue-url http://localhost:4566/000000000000/wallet-events.fifo --max-number-of-messages 10 --attribute-names All --message-attribute-names All
```

`/health/ready` reporta tanto `postgres` quanto `sqs`.

## 5. Operações de entrada pelo SQS

Os provedores também podem enviar operações por `wager-transactions.fifo`. O consumidor executa o mesmo caso de uso de `POST /wagering/transactions`, com as mesmas garantias de idempotência, mais uma inbox indexada pelo `messageId` do envelope (ARCHITECTURE §9, §10, §23).

Corpo da mensagem (CHALLENGE §10): `data` leva os mesmos campos do corpo HTTP mais `idempotencyKey`. Campos desconhecidos são recusados, e os valores precisam ser strings:

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

O provedor pelo qual a mensagem age vem da identidade do remetente no broker, nunca só de `data.providerId` (ARCHITECTURE §10, "Identidade do produtor"). O consumidor lê o `SenderId` informado pelo SQS e o mapeia para um provedor com `SQS_PROVIDER_SENDERS`; `data.providerId` precisa nomear esse provedor. Na AWS o sender id é o id do usuário ou da role IAM do produtor. O MiniStack informa a conta do remetente como `SenderId`, então localmente cada produtor envia por uma role na própria conta:

| Produtor | Envia como | `SenderId` | Age por |
| --- | --- | --- | --- |
| provider-a | role `wager-producer` em `111111111111` | `111111111111` | `provider-a` |
| provider-b | role `wager-producer` em `222222222222` | `222222222222` | `provider-b` |

Um segredo errado, uma chave desconhecida ou a chave raiz de uma conta (`111111111111` com qualquer segredo) são recusados pelo broker antes que o consumidor veja qualquer coisa. Fica uma lacuna do emulador: o MiniStack nunca confere o segredo da chave raiz de uma conta, então quem sabe disso ainda pode mudar o IAM dessa conta e agir pela role dela. Na AWS as credenciais raiz são secretas (ARCHITECTURE §28).

| Item | Valor |
| --- | --- |
| Remetente | uma chave sem provedor mapeado vai para a DLQ como `FORBIDDEN`; um `data.providerId` de outro provedor, como `PROVIDER_MISMATCH`; nada é gravado |
| `MessageGroupId` | o `walletId`: ordem por carteira, carteiras diferentes em paralelo |
| `MessageDeduplicationId` | o `messageId` do envelope (best effort; a inbox e as constraints do banco são a garantia) |
| Visibility timeout / deadline do handler | 60s, enviado em todo recebimento / 15s; depois da primeira mensagem de um grupo, a seguinte só começa se o deadline mais 5s ainda couber na visibilidade; senão ela e o resto do grupo são liberados para reentrega |
| Tentativas | falhas transitórias são repetidas com visibilidade de `min(2^receiveCount, 60)`s; após 10 recebimentos, o redrive da fila move a mensagem para `wager-transactions-dlq.fifo` |
| Readiness | o polling pausa enquanto `/health/ready` falha, então uma queda não consome tentativas |
| Inválida ou permanente | enviada à DLQ na hora com `failureClass=PERMANENT`, `failureCode`, `originalMessageId`, `envelopeMessageId` e `originalSenderId` (o remetente a quem foi atribuída), depois removida; nada é gravado |
| Rejeição de negócio | commitada como `REJECTED`, mensagem removida |
| `messageId` duplicado | mesmo payload: removida sem efeito; payload diferente: DLQ (`INVALID_REQUEST`) |

A mensagem só é removida depois que a transação dela commitou. Acompanhe o resultado com `GET /providers/{providerId}/wagering/transactions/{externalId}`.

Para enviar uma como o produtor do provider-a, use `deploy/ministack/send-as.sh`, montado no container do emulador. Ele lê a chave do produtor do arquivo de credenciais, assume a role do provider-a e envia com ela:

```sh
docker compose exec ministack sh /opt/wager/send-as.sh provider-a <walletId> msg-123 '<JSON da mensagem>'
```

Os argumentos são o provedor, o `MessageGroupId`, o `MessageDeduplicationId` e o corpo (ou `@/caminho/do/arquivo` dentro do container).

### Procedimento de redrive da DLQ

1. Confirme que a causa foi resolvida: `/health/ready` responde 200.
2. Mensagens com `failureClass=PERMANENT` exigem correção pelo produtor; não são reenviadas como estão. Inspecione-as com `receive-message` em `wager-transactions-dlq.fifo` (`--message-attribute-names All`).
3. Reenvie as demais (tentativas transitórias esgotadas) com o `dlqredrive`. Ele mantém o corpo e o `MessageGroupId`, usa `MessageDeduplicationId=<MessageId>:redrive:<unixSeconds>`, nomeia o produtor original no atributo `originalSenderId` (que o consumidor só aceita da identidade do operador, `SQS_OPERATOR_SENDER_ID`), remove cada mensagem da DLQ depois do envio e pula as permanentes. Ele pega a chave e a role do operador no arquivo de credenciais (`SQS_CREDENTIALS_FILE`, definido no serviço `app`) e age pela role, na conta `333333333333`, então acha as filas na conta dona com `SQS_QUEUE_OWNER_ACCOUNT_ID`:

   ```sh
   docker compose run --rm --no-deps --entrypoint /app/dlqredrive -e SQS_QUEUE_OWNER_ACCOUNT_ID=000000000000 app
   ```

   Ele imprime `redriven=N skipped_permanent=M`. As mensagens puladas ficam em voo durante o visibility timeout da DLQ (60s); por causa da ordem FIFO, mensagens posteriores do mesmo grupo podem precisar de uma segunda execução depois disso.
4. O redrive é seguro: o `messageId` do envelope não muda, então a inbox e a idempotência absorvem o que já foi processado.

## 6. Métricas

Formato texto do Prometheus num listener separado (`METRICS_ADDR`, padrão `:9090`; o compose o publica no host):

```sh
curl -s http://localhost:9090/metrics | grep -E '^(wager_|outbox_|sqs_|retries_|db_|reconciliation_|http_request)'
```

| Métrica | Tipo | Rótulos | Significado |
| --- | --- | --- | --- |
| `wager_transactions_total` | counter | `transport`, `status`, `failure_code` | operações que chegaram a um novo estado (http, sqs, worker) |
| `wager_duplicates_total` | counter | `transport`, `kind` | replays por chave de idempotência e duplicatas da inbox |
| `wager_conflicts_total` | counter | `code` | conflitos de idempotência, de ID externo e de carteira |
| `db_concurrency_conflicts_total` | counter | `sqlstate` | rollbacks por lock timeout, deadlock, falha de serialização ou statement timeout |
| `retries_total` | counter | `component` | retries do consumidor, do worker de referências e da outbox |
| `sqs_dlq_messages_total` | counter | `failure_code` | mensagens enviadas à DLQ explicitamente |
| `outbox_parked_total` | counter | — | eventos estacionados após esgotar as tentativas |
| `outbox_unpublished` / `outbox_lag_seconds` | gauge | — | backlog da outbox e idade do evento não publicado mais antigo |
| `wager_processing_duration_seconds` | histogram | `transport`, `outcome` | duração de um envio ou de uma iteração do worker |
| `http_request_duration_seconds` | histogram | `route`, `code` | latência HTTP por padrão de rota |
| `reconciliation_divergences_total` | counter | — | reconciliações com `consistent=false` |

Alerte em `wager_transactions_total{status="FAILED"}`, `outbox_parked_total`, `outbox_lag_seconds`, `sqs_dlq_messages_total` e `reconciliation_divergences_total`. Os rótulos nunca levam ids de carteira, transação ou provedor. Detalhes: ARCHITECTURE §25.

### Prometheus e Grafana (opcional)

`docker-compose.observability.yml` acrescenta um Prometheus (`prom/prometheus:v3.5.0`, coleta a cada 5 s `app:9090`, `app-2:9090` e `app-3:9090` pela rede do compose) e um Grafana (`grafana/grafana:12.1.1`) com o datasource e o painel "Wager wallet" provisionados de `deploy/observability/`. Nada muda na aplicação; o override só lê os listeners de métricas.

```sh
docker compose -f docker-compose.yml -f docker-compose.multi.yml -f docker-compose.observability.yml up -d --build
```

Grafana em http://localhost:3000 (leitura anônima; `admin`/`admin` para editar) e Prometheus em http://localhost:9095. Sem `docker-compose.multi.yml`, só `app` existe e os alvos `app-2` e `app-3` aparecem como `down`. O painel mostra instâncias no ar, `wager_transactions_total` por status e por `failure_code`, p50/p95/p99 de `http_request_duration_seconds` (também p99 por rota) e de `wager_processing_duration_seconds` por transporte, `db_concurrency_conflicts_total` por `sqlstate`, `retries_total` por componente, `sqs_dlq_messages_total`, `outbox_lag_seconds`, `outbox_unpublished` e `reconciliation_divergences_total`. Contadores com rótulos só existem depois do primeiro incremento, então "No data" em conflitos, retries ou DLQ significa zero ocorrências. Para gerar tráfego, use o teste de carga ([docs/CARGA.md](CARGA.md)). Derrube com o mesmo `-f` e `down -v`.

## 7. Keycloak e identidades de teste

O Keycloak (console de administração `admin`/`admin` em http://localhost:8080) importa o realm `wagering` de `deploy/keycloak/realm-wagering.json` no start. Todo token tem `iss=http://localhost:8080/realms/wagering` e `aud=wallet-api`.

| Client (`client_credentials`) | Segredo (valor local de teste) | Papel | Claim `provider_id` |
| --- | --- | --- | --- |
| `provider-a` | `provider-a-local-secret` | `provider` | `provider-a` |
| `provider-b` | `provider-b-local-secret` | `provider` | `provider-b` |
| `wallet-internal` | `wallet-internal-local-secret` | `wallet-internal` | — |

Para obter um token:

```sh
curl -s -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-local-secret http://localhost:8080/realms/wagering/protocol/openid-connect/token
```

Envie-o como `Authorization: Bearer <access_token>`. Os endpoints de negócio respondem 401 com `WWW-Authenticate: Bearer error="invalid_token"` quando o token falta, é inválido ou expirou. Se o token foi assinado com uma chave que o serviço ainda não tem em cache e o Keycloak está inacessível para buscá-la, a resposta é 503 `TEMPORARILY_UNAVAILABLE` (retryable, `Retry-After: 2`) (ARCHITECTURE §24). O providerId pelo qual um provedor pode agir vem só da claim `provider_id`. `/health/*` não exige token.

## 8. Exemplos de chamadas da API

Contratos e códigos de status: ARCHITECTURE §13 e §20. Toda rota de negócio exige um bearer token ([seção 7](#7-keycloak-e-identidades-de-teste)); `X-Correlation-Id` é opcional e devolvido na resposta.

```sh
INTERNAL=$(curl -s -d grant_type=client_credentials -d client_id=wallet-internal -d client_secret=wallet-internal-local-secret http://localhost:8080/realms/wagering/protocol/openid-connect/token | jq -r .access_token)
PROVIDER=$(curl -s -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-local-secret http://localhost:8080/realms/wagering/protocol/openid-connect/token | jq -r .access_token)
```

Abrir uma carteira (serviço interno; 201):

```sh
curl -s -X POST http://localhost:8081/wallets -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'
```

Enviar uma aposta (provedor; substitua `<walletId>`). Reenviar a mesma requisição devolve o resultado gravado com `"idempotentReplay": true`:

```sh
curl -s -X POST http://localhost:8081/wagering/transactions -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' -H 'Idempotency-Key: provider-a:transaction-123' -d '{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<walletId>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
```

Reembolsar essa aposta. Uma reversão nomeia a operação original em `referenceExternalTransactionId`, e o valor precisa ser igual ao do original:

```sh
curl -s -X POST http://localhost:8081/wagering/transactions -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' -H 'Idempotency-Key: provider-a:transaction-124' -d '{"providerId":"provider-a","externalTransactionId":"transaction-124","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<walletId>","roundId":"round-987","gameId":"fortune-chimp","kind":"REFUND","money":{"amount":"25.00","currency":"BRL"},"referenceExternalTransactionId":"transaction-123"}'
```

Leituras e reconciliação. Uma transação pode ser lida pelo id interno ou pelo id externo do provedor; as leituras de carteira são do serviço interno:

```sh
curl -s http://localhost:8081/providers/provider-a/wagering/transactions/transaction-123 -H "Authorization: Bearer $PROVIDER"
```

```sh
curl -s http://localhost:8081/wagering/transactions/<transactionId> -H "Authorization: Bearer $PROVIDER"
```

```sh
curl -s http://localhost:8081/wallets/<walletId> -H "Authorization: Bearer $INTERNAL"
```

```sh
curl -s "http://localhost:8081/wallets/<walletId>/ledger?limit=50" -H "Authorization: Bearer $INTERNAL"
```

```sh
curl -s -X POST http://localhost:8081/wallets/<walletId>/reconciliation -H "Authorization: Bearer $INTERNAL"
```

## 9. Migrations

As migrations ficam em `migrations/` como `NNNN_nome.up.sql` / `NNNN_nome.down.sql`, uma por tabela, e depois a 0008, que acrescenta um trigger a `wallets`:

| Versão | Cria |
| --- | --- |
| 0001 | `schema_info` (verificação do pipeline) |
| 0002 | `providers`, com `provider-a` e `provider-b` |
| 0003 | `wallets` |
| 0004 | `wager_transactions` |
| 0005 | `wallet_ledger_entries` (append-only) |
| 0006 | `inbox_messages` |
| 0007 | `outbox_events` |
| 0008 | `wallets_ledger_link`: trigger de restrição adiado que só commita uma mudança de saldo junto com o seu lançamento |

A migration de cada tabela traz as constraints, os triggers de proteção e os grants para `wallet_app`; a 0008 acrescenta um trigger a `wallets` (ARCHITECTURE §16).

Elas ficam embutidas nos binários e são aplicadas com o [golang-migrate](https://github.com/golang-migrate/migrate) pelo `cmd/migrate`, sempre como o papel dono do schema, `wallet_owner`. A aplicação conecta como `wallet_app` e se recusa a subir se o schema estiver mais antigo que a última migration embutida, ou se uma migration estiver suja.

Aplicar todas as migrations pendentes:

```sh
export MIGRATE_DATABASE_URL='postgres://wallet_owner:wallet_owner_local@localhost:5432/wallet?sslmode=disable'
go run ./cmd/migrate up
```

Reverter a última migration (ou as últimas N):

```sh
go run ./cmd/migrate down
go run ./cmd/migrate down 2
```

Ver a versão atual:

```sh
go run ./cmd/migrate version
```

Os mesmos comandos dentro da rede do compose:

```sh
docker compose run --rm migrate up
docker compose run --rm migrate down
docker compose run --rm migrate version
```

O `migrate` é dependência do `app`, então `docker compose up` e `docker compose start app` rodam `migrate up` de novo antes de a aplicação subir. Para subir a aplicação contra um schema revertido (por exemplo, para vê-la se recusar a subir), use `docker start wager-wallet-app-1`.

Se uma migration falhar no meio, `version` reporta `dirty: true` e a aplicação não sobe. Corrija a causa, repare o schema à mão e depois redefina a versão registrada com o CLI original do golang-migrate (`migrate -path migrations -database "$MIGRATE_DATABASE_URL" force N`). O `cmd/migrate` não expõe `force` de propósito. Reverter só é seguro enquanto nenhum dado de negócio depender da migration revertida.

Os papéis do banco são criados por `deploy/postgres/init/01-roles.sh` quando o diretório de dados do Postgres é inicializado pela primeira vez. Os testes de integração usam o mesmo script.

## 10. Variáveis de ambiente

O `.env.example` documenta cada variável com o seu valor local.

| Variável | Padrão | Usada por | Significado |
| --- | --- | --- | --- |
| `DATABASE_URL` | — (obrigatória) | app | URL do Postgres para o papel `wallet_app` |
| `MIGRATE_DATABASE_URL` | — (obrigatória) | migrate | URL do Postgres para o papel `wallet_owner` |
| `DB_MAX_CONNS` | `10` | app | tamanho do pgxpool |
| `DB_STARTUP_TIMEOUT` | `10s` | app | limite do ping e da checagem de schema no start |
| `DB_LOCK_TIMEOUT` | `5s` | app | `lock_timeout` padrão das transações de negócio (ARCHITECTURE §4) |
| `DB_STATEMENT_TIMEOUT` | `10s` | app | `statement_timeout` padrão de toda transação; um comando cancelado é transitório (ARCHITECTURE §24) |
| `OIDC_ISSUER` | — (obrigatória) | app | `iss` esperado, exato |
| `OIDC_JWKS_URL` | — (obrigatória) | app | endpoint do JWKS (no compose: `http://keycloak:8080/...`) |
| `OIDC_AUDIENCE` | `wallet-api` | app | valor de `aud` exigido |
| `OIDC_JWKS_MIN_REFRESH_INTERVAL` | `30s` | app | intervalo mínimo entre buscas do JWKS por `kid` desconhecido |
| `REFERENCE_TTL` | `10m` | app | quanto tempo uma operação espera pela referência (ARCHITECTURE §8) |
| `REFERENCE_BACKOFF_BASE` | `1s` | app | primeiro atraso de retry de uma referência ausente |
| `REFERENCE_BACKOFF_MAX` | `60s` | app | maior atraso de retry |
| `REFERENCE_WORKER_CONCURRENCY` | `4` | app | goroutines do worker de referências por instância |
| `REFERENCE_WORKER_POLL_INTERVAL` | `1s` | app | espera ociosa antes de o worker procurar operações vencidas de novo |
| `SQS_ENDPOINT` | — (AWS) | app | endpoint alternativo do broker (compose: `http://ministack:4566`) |
| `AWS_REGION` | `us-east-1` | app | região do broker |
| `SQS_EVENTS_QUEUE` | `wallet-events.fifo` | app | fila dos eventos de saída |
| `SQS_CREDENTIALS_FILE` | — | app, dlqredrive | arquivo de credenciais do broker (compose: o que o script de init do MiniStack grava); preenche as chaves abaixo que não estiverem definidas (ARCHITECTURE §28) |
| `SQS_PUBLISHER_ACCESS_KEY_ID` / `SQS_PUBLISHER_SECRET_ACCESS_KEY` | — (obrigatórias, ou do arquivo) | app | credenciais exclusivas do publisher (ARCHITECTURE §10) |
| `SQS_INBOUND_QUEUE` / `SQS_INBOUND_DLQ` | `wager-transactions.fifo` / `wager-transactions-dlq.fifo` | app, dlqredrive | fila de entrada e sua DLQ |
| `SQS_CONSUMER_ACCESS_KEY_ID` / `SQS_CONSUMER_SECRET_ACCESS_KEY` | — (obrigatórias, ou do arquivo) | app | credenciais exclusivas do consumidor (ARCHITECTURE §10) |
| `SQS_PROVIDER_SENDERS` | — (obrigatória com `RUN_CONSUMER`) | app | pares `senderId=providerId`, separados por vírgula: as identidades de produtor em que o consumidor confia (ARCHITECTURE §10) |
| `SQS_OPERATOR_SENDER_ID` | — (obrigatória com `RUN_CONSUMER`; exigida pelo dlqredrive) | app, dlqredrive | sender id do operador de redrive, o único cujo `originalSenderId` é aceito |
| `SQS_OPERATOR_ACCESS_KEY_ID` / `SQS_OPERATOR_SECRET_ACCESS_KEY` | — (obrigatórias, ou do arquivo) | dlqredrive | credenciais do operador para a ferramenta de redrive (ARCHITECTURE §23) |
| `SQS_OPERATOR_ROLE_ARN` | — (do arquivo; vazia age como a própria chave) | dlqredrive | role que a chave do operador assume (localmente `arn:aws:iam::333333333333:role/wager-operator`, ARCHITECTURE §28) |
| `SQS_QUEUE_OWNER_ACCOUNT_ID` | — (a conta do operador) | dlqredrive | conta dona das filas, quando a do operador é outra (localmente `000000000000`) |
| `CONSUMER_WAIT_SECONDS` | `20` | app | espera do long poll (0-20) |
| `CONSUMER_MAX_MESSAGES` | `10` | app | mensagens por recebimento (1-10) |
| `CONSUMER_HANDLER_TIMEOUT` | `15s` | app | deadline de uma mensagem |
| `CONSUMER_VISIBILITY_TIMEOUT` | `60s` | app | visibilidade enviada em todo recebimento; segundos inteiros, no mínimo o deadline do handler + 5s |
| `CONSUMER_BACKOFF_MAX` | `60s` | app | maior visibilidade após uma falha transitória |
| `CONSUMER_PAUSE_INTERVAL` | `1s` | app | rechecagem da readiness enquanto o polling está pausado |
| `OUTBOX_LEASE` | `30s` | app | lease do claim (ARCHITECTURE §9) |
| `OUTBOX_BATCH_SIZE` | `10` | app | eventos por `SendMessageBatch` (máx. 10) |
| `OUTBOX_PUBLISH_TIMEOUT` | `5s` | app | timeout de uma chamada de publicação |
| `OUTBOX_BACKOFF_BASE` / `OUTBOX_BACKOFF_MAX` | `1s` / `5m` | app | backoff de retry |
| `OUTBOX_MAX_ATTEMPTS` | `20` | app | tentativas antes de um evento ser estacionado |
| `OUTBOX_POLL_INTERVAL` | `1s` | app | espera ociosa do relay |
| `OUTBOX_RELAY_CONCURRENCY` | `1` | app | goroutines do relay por instância |
| `HTTP_ADDR` | `:8081` | app | endereço de escuta |
| `METRICS_ADDR` | `:9090` | app | listener do Prometheus; vazio o desliga (ARCHITECTURE §25) |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | app | timeout de leitura dos headers |
| `HTTP_HANDLER_TIMEOUT` | `10s` | app | timeout por requisição |
| `READINESS_INTERVAL` | `2s` | app | período do loop de readiness |
| `READINESS_CHECK_TIMEOUT` | `1s` | app | timeout de cada checagem de readiness |
| `READINESS_DRAIN_DELAY` | `2s` | app | quanto tempo `/health/ready` reporta draining antes do shutdown |
| `LOG_LEVEL` | `info` | app | `debug`, `info`, `warn`, `error` |
| `RUN_HTTP` | `true` | app | serve HTTP |
| `RUN_CONSUMER` | `true` | app | consumidor SQS |
| `RUN_OUTBOX` | `true` | app | relay da outbox |
| `RUN_REFERENCE_WORKER` | `true` | app | worker de `PENDING_REFERENCE` |

Uma configuração inválida faz a aplicação sair no start, listando todos os problemas encontrados.

## 11. Testes

| Comando | O que roda | Precisa de |
| --- | --- | --- |
| `go test ./...` | testes unitários, validação do grafo do Fx, regras de arquitetura | Go |
| `go test -race ./...` | o mesmo com o detector de corrida | Go + gcc (cgo) |
| `go vet ./...` | vet (acrescente `-tags=integration` para checar também o código de integração) | Go |
| `gofmt -l .` | lista arquivos sem formatação (não deve imprimir nada) | Go |
| `go test -tags=integration -count=1 ./test/integration/...` | Postgres real via testcontainers: migrations up/down, start/stop do Fx com checagem de vazamento de goroutines, cada constraint, trigger e grant do schema, os repositórios, lock de carteira e atomicidade das transações, verificação de tokens contra um Keycloak real (iniciado uma vez por execução), a API HTTP de ponta a ponta incluindo os cenários de concorrência do CHALLENGE §13, o worker de referências (referências atrasadas, expiração, disputa, claims abandonados, reinício, falhas transitórias), o relay da outbox contra um MiniStack real (publicação, disputa, recuperação de queda, backoff, estacionamento), o consumidor SQS contra um MiniStack real com IAM aplicado (`AUTH=true`: credenciais e políticas de cada identidade, a falsificação pela chave raiz recusada, identidade do remetente e vínculo ao provedor, inbox, duplicatas, DLQ, cruzamento HTTP/SQS, queda antes do delete, retries transitórios e redrive, pausa de readiness, parada graciosa) | Docker |
| `go test -tags=integration -race -count=1 ./test/integration/...` | o mesmo com o detector de corrida | Docker + gcc |

Sem um compilador C local (por exemplo, no Windows sem gcc), rode o detector de corrida num container Go Linux. O socket do Docker permite que o testcontainers suba os containers irmãos:

```sh
docker run --rm -v "$PWD:/src" -w /src \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal \
  --add-host=host.docker.internal:host-gateway \
  golang:1.27.2-bookworm go test -race -tags=integration -count=1 ./...
```

Esse comando é para um shell POSIX (Linux, macOS, WSL). No Windows, use uma linha só. No PowerShell, `${PWD}` precisa das chaves:

```powershell
docker run --rm -v "${PWD}:/src" -w /src -v /var/run/docker.sock:/var/run/docker.sock -e TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal --add-host=host.docker.internal:host-gateway golang:1.27.2-bookworm go test -race -tags=integration -count=1 ./...
```

No Git Bash, impeça o MSYS de reescrever os caminhos do container (`-w /src` viraria `C:/Program Files/Git/src`) e passe o caminho do Windows:

```sh
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W):/src" -w /src -v /var/run/docker.sock:/var/run/docker.sock -e TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal --add-host=host.docker.internal:host-gateway golang:1.27.2-bookworm go test -race -tags=integration -count=1 ./...
```

Build tags:

- `integration`: testes em `test/integration` que sobem containers reais.
- `multiinstance`: testes com três processos sobre a pilha do compose (abaixo).

### Múltiplas instâncias e simulações de falha

`test/multiinstance` roda os cenários do CHALLENGE §13 contra três processos independentes da aplicação (`app`, `app-2`, `app-3` do `docker-compose.multi.yml`, nas portas 8081-8083 e métricas em 9090-9092) que compartilham Postgres, Keycloak e MiniStack, e simula as falhas do CHALLENGE §3:

| Teste | O que mostra |
| --- | --- |
| `TestSameBet50TimesAcrossInstances` | a mesma BET 50 vezes pelas três APIs HTTP e pelo SQS: um débito |
| `TestTwoBetsOf80AcrossInstances` | 100.00 contra 2 x 80.00 por instâncias diferentes (cinco rodadas): uma processada, uma `INSUFFICIENT_FUNDS`, 20.00 restante |
| `TestDistinctWalletsInParallel` | outras carteiras avançam em todas as instâncias enquanto uma carteira está travada |
| `TestHTTPAndSQSCrossoverAcrossInstances` | a mesma operação pelo SQS e pelas três APIs HTTP ao mesmo tempo: um efeito |
| `TestConsumerKilledUnderLoad` | SIGKILL de uma instância consumindo: as reentregas são duplicatas da inbox, um efeito cada |
| `TestOutboxRelaysKilledUnderLoad` | SIGKILL de uma instância publicando sob carga: todo evento commitado chega a `wallet-events.fifo` |
| `TestReferenceResolvedAfterInstanceKill` | referências pendentes de uma instância morta são resolvidas ou expiradas por outra |
| `TestRestartAllInstances` | as três mortas e reiniciadas: replays, referências pendentes e mensagens enfileiradas sobrevivem |
| `TestPostgresOutage` | Postgres parado: readiness 503, HTTP 503 retryable, consumidores pausados, nada na DLQ; recuperação |
| `TestSQSOutage` | MiniStack pausado: o HTTP continua commitando, os relays pausam, os eventos esperam na outbox sem consumir tentativas e são todos publicados após a volta |

Cada teste termina com as checagens de invariantes: nada pendente, reconciliação consistente com o saldo esperado, um lançamento por operação processada que move dinheiro, outbox esvaziada sem nada estacionado, e cada `eventId` commitado recebido em `wallet-events.fifo`.

Preparação: Docker rodando, a pilha de desenvolvimento parada (`docker compose down`), portas 8080-8083, 4566, 5432 e 9090-9092 livres. A suíte constrói e sobe a pilha sozinha, no projeto `wager-wallet-multi`, e a remove no fim (`MULTIINSTANCE_KEEP_STACK=1` a mantém):

```sh
go test -tags=multiinstance -count=1 -v -timeout 40m ./test/multiinstance/
```

Leva alguns minutos: as mensagens retidas por um consumidor morto só voltam depois do visibility timeout de 60s, e as quedas duram cerca de 15s cada. A mesma pilha pode ser iniciada à mão para explorar:

```sh
docker compose -p wager-wallet-multi -f docker-compose.yml -f docker-compose.multi.yml up -d --build
```

Para rodar a suíte com o detector de corrida, use um container Linux com gcc e o CLI do Docker ([test/multiinstance/race.Dockerfile](../test/multiinstance/race.Dockerfile)). Ele controla a mesma pilha pelo socket do Docker e alcança as portas publicadas por `host.docker.internal`. O repositório é montado no próprio caminho, para os bind mounts relativos do compose (scripts de init, realm) resolverem no host do Docker:

```sh
docker build -t wager-wallet-race-harness - < test/multiinstance/race.Dockerfile
```

```sh
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$PWD:$PWD" -w "$PWD" -e MULTIINSTANCE_HOST=host.docker.internal --add-host=host.docker.internal:host-gateway wager-wallet-race-harness go test -race -tags=multiinstance -count=1 -v -timeout 40m ./test/multiinstance/
```

`make multiinstance-race` roda os dois. No Docker Desktop para Windows, troque `$PWD` dos dois lados da montagem e no `-w` pelo caminho que a VM do Docker conhece para o checkout: `/run/desktop/mnt/host/c/...` para `C:\...`. O detector instrumenta o processo de teste: os clientes HTTP, SQS e Postgres e as goroutines dos cenários concorrentes. Os três processos da aplicação são os binários da imagem, compilados sem `-race` (ARCHITECTURE §25, N7).

Extra, não exigido pelo desafio: os três processos da aplicação também podem rodar binários compilados com `-race` (alvo `race` do Dockerfile, [docker-compose.race.yml](../docker-compose.race.yml)). Defina `MULTIINSTANCE_RACE=1`, por exemplo acrescentando `-e MULTIINSTANCE_RACE=1` ao comando do container acima. Também funciona a partir do host, já que o Docker constrói as imagens. A suíte então confere que o binário da aplicação foi compilado com `-race` e falha com qualquer `WARNING: DATA RACE` nos logs de `app`, `app-2` e `app-3` (ARCHITECTURE §26). Processos com `-race` são mais lentos e usam mais memória.

O teste de arquitetura (`test/architecture`) roda com o `go test ./...` simples. Ele falha se:

- `internal/domain` depender de fx, net/http, do AWS SDK, de pgx ou de database/sql, ou de qualquer coisa fora da biblioteca padrão, de google/uuid e de si mesmo;
- algum código fora dos testes em `internal/`, `cmd/` ou `migrations/` usar `float32`/`float64`, funções de float do `strconv` ou `panic`, exceto os segundos e contagens do Prometheus em `internal/platform/metrics` e o re-panic do TxRunner depois do rollback, que estão numa allow-list (uma entrada sem uso também falha);
- `internal/app` depender de `internal/adapters`;
- algum pacote além de `internal/bootstrap` e `cmd/*` importar fx.

O teste de fuzz do parsing de Money roda o seu corpus inicial no `go test`; para fuzzing mais longo:

```sh
go test -run=^$ -fuzz=FuzzParseRoundTrip -fuzztime=1m ./internal/domain/money/
```

### Atalhos do make

`make build test test-race vet fmt fmt-check integration integration-race multiinstance multiinstance-race migrate-up migrate-down migrate-version up down`

## 12. Limitações conhecidas

A lista completa está no ARCHITECTURE.md, em "Premissas e limitações". As que aparecem ao rodar a pilha localmente:

- **O MiniStack não autentica as chaves raiz das contas.** Mesmo com `AUTH=true`, o emulador nunca confere o segredo da chave raiz de uma conta (`test`, ou um id de conta de 12 dígitos). Enviar direto à fila de entrada com o id da conta de um provedor e qualquer segredo é recusado pela política da fila, mas quem sabe disso pode usar a chave raiz dessa conta para mudar o IAM dela (por exemplo a política de confiança da role) e então enviar pela role do provedor. Localmente, portanto, a identidade do provedor é garantida pelo broker contra credenciais comuns, não contra esse comportamento do emulador; na AWS as credenciais raiz são secretas e o IAM autentica a role (ARCHITECTURE §28). O consumidor confere de todo modo que `data.providerId` é o provedor do remetente.
- **As chaves do broker mudam quando o MiniStack reinicia.** Ele guarda o estado (filas, usuários IAM, chaves) em memória, então reiniciar o `ministack` perde as filas e gera chaves novas: reinicie a aplicação depois.
- **As quedas do SQS são simuladas com `docker compose pause`**, não com restart, pelo mesmo motivo (ARCHITECTURE §25).
- **As políticas do broker usadas localmente são mais amplas que as da AWS.** O MiniStack confere uma role de outra conta contra o ARN da fila na conta de quem chama, então as políticas locais usam curinga de conta e os nomes das filas de teste; `deploy/aws/iam-policies.json` tem os ARNs exatos para a AWS.
