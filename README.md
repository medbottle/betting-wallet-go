# betting-wallet-go

Serviço de carteiras para provedores de jogos, em Go. Processa apostas, ganhos, perdas, reembolsos e estornos por uma API HTTP e por uma fila SQS, com as mesmas garantias nos dois caminhos.

**Stack:** Go 1.27 · Uber Fx · PostgreSQL · AWS SQS (MiniStack localmente) · Keycloak · Docker Compose

## Garantias

- **Dinheiro exato**: valores em unidades mínimas inteiras com moeda, nunca em ponto flutuante.
- **Ledger confiável**: toda mudança de saldo tem um lançamento append-only, imposto pelo banco, e a reconciliação reconstrói o saldo a partir dele.
- **Concorrência segura**: um lock por carteira; a mesma carteira nunca tem gasto duplo, e carteiras diferentes avançam em paralelo, em qualquer número de instâncias.
- **Idempotência que sobrevive a reinícios**: reenvios e entregas duplicadas devolvem o resultado original, sem mover dinheiro duas vezes.
- **Mensageria confiável**: inbox no SQS, outbox transacional (eventos só depois do commit), retries, DLQ e ferramenta de redrive.
- **Autenticação em tudo**: OAuth 2.0 `client_credentials` na API, credenciais e políticas do broker no SQS; cada provedor só vê as próprias transações.

## Início rápido

Pré-requisitos: Docker com Docker Compose v2, `curl` e `jq` (e Go 1.27.2 para os testes). Os comandos são para bash: Linux, macOS ou, no Windows, Git Bash ou WSL.

```sh
docker compose up --build
```

Isso sobe Postgres, Keycloak, MiniStack, as migrations e a aplicação em `http://localhost:8081`. Para conferir:

```sh
curl -i http://localhost:8081/health/ready
```

Abrir uma carteira e fazer uma aposta:

```sh
INTERNAL=$(curl -s -d grant_type=client_credentials -d client_id=wallet-internal -d client_secret=wallet-internal-local-secret http://localhost:8080/realms/wagering/protocol/openid-connect/token | jq -r .access_token)
PROVIDER=$(curl -s -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-local-secret http://localhost:8080/realms/wagering/protocol/openid-connect/token | jq -r .access_token)

WALLET=$(curl -s -X POST http://localhost:8081/wallets -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}' | jq -r .id)

curl -s -X POST http://localhost:8081/wagering/transactions -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' -H 'Idempotency-Key: provider-a:transaction-123' -d '{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"'"$WALLET"'","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
```

Para parar e apagar os dados: `docker compose down -v`.

## Configuração

O Compose já traz todos os valores locais; para mudar algum, copie `.env.example` para `.env`. As principais variáveis:

| Variável | Para quê |
| --- | --- |
| `DATABASE_URL` / `MIGRATE_DATABASE_URL` | Postgres da aplicação (papel `wallet_app`) e das migrations (papel `wallet_owner`) |
| `OIDC_ISSUER`, `OIDC_JWKS_URL`, `OIDC_AUDIENCE` | validação dos tokens do Keycloak |
| `SQS_ENDPOINT`, `SQS_CREDENTIALS_FILE` | endpoint do MiniStack e arquivo com as chaves que ele gera |
| `SQS_PROVIDER_SENDERS`, `SQS_OPERATOR_SENDER_ID` | identidades do broker em que o consumidor confia |
| `RUN_HTTP`, `RUN_CONSUMER`, `RUN_OUTBOX`, `RUN_REFERENCE_WORKER` | quais papéis a instância executa |

Todas as variáveis, com padrões e valores de exemplo, estão no [.env.example](.env.example) e na tabela do [guia](docs/GUIA.md#10-variáveis-de-ambiente). O script `deploy/ministack/init-queues.sh` cria as filas SQS (`wager-transactions.fifo`, a DLQ dela e `wallet-events.fifo`) e as identidades do broker no start do MiniStack, e o Keycloak importa o realm `wagering` com as identidades de teste (detalhes no [guia](docs/GUIA.md#3-filas-e-identidades-do-broker)):

| Client (`client_credentials`) | Segredo (valor local de teste) | Papel |
| --- | --- | --- |
| `provider-a` | `provider-a-local-secret` | `provider` (age por `provider-a`) |
| `provider-b` | `provider-b-local-secret` | `provider` (age por `provider-b`) |
| `wallet-internal` | `wallet-internal-local-secret` | `wallet-internal` (operações de carteira) |

Os fluxos autenticados usam um token desses clients como `Authorization: Bearer <token>`, como no início rápido; mais exemplos no [guia, seção 8](docs/GUIA.md#8-exemplos-de-chamadas-da-api).

## Migrations

O `docker compose up` aplica as migrations antes de subir a aplicação. Para aplicá-las ou revertê-las à mão:

```sh
docker compose run --rm migrate up        # aplica as pendentes
docker compose run --rm migrate down      # reverte a última (down N reverte N)
docker compose run --rm migrate version   # versão atual
```

## Testes

```sh
go test ./...                                              # unitários, grafo do Fx, regras de arquitetura
go test -race ./...                                        # o mesmo com o detector de corrida (precisa de gcc)
go vet ./...
gofmt -l .                                                 # não deve imprimir nada
go test -tags=integration -count=1 ./test/integration/...  # Postgres, Keycloak e MiniStack reais (Docker)
go test -tags=multiinstance -count=1 -v -timeout 40m ./test/multiinstance/  # três instâncias e simulações de falha
```

Sem gcc (por exemplo, no Windows), rode o `-race` num container Linux: os comandos, a preparação das dependências dos testes, as build tags (`integration`, `multiinstance`) e as simulações de falha estão no [guia, seção 11](docs/GUIA.md#11-testes).

## Documentação

| Documento | Conteúdo |
| --- | --- |
| [docs/GUIA.md](docs/GUIA.md) | execução e operação: ambiente local, filas e identidades do broker, SQS, métricas, Keycloak, exemplos da API, migrations, variáveis de ambiente, testes e limitações conhecidas |
| [ARCHITECTURE.md](ARCHITECTURE.md) | decisões de arquitetura, com um resumo de uma página no início |
| [docs/CHALLENGE.md](docs/CHALLENGE.md) | especificação do desafio |
| [.env.example](.env.example) | todas as variáveis de ambiente com valores locais de exemplo |

## Estrutura

```
cmd/            binários: wallet (aplicação), migrate, dlqredrive
internal/       domain, app (casos de uso), adapters (HTTP, Postgres, SQS, OIDC), platform, bootstrap (Fx)
migrations/     migrations SQL versionadas
deploy/         Keycloak, MiniStack, Postgres e políticas da AWS
test/           testes de integração, de múltiplas instâncias e de arquitetura
```
