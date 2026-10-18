# Teste de carga

Diferencial opcional da seção 14 do desafio: comando reproduzível, ambiente, metodologia, throughput, p50/p95/p99, erros, conflitos e atraso da outbox. Não há meta mínima de RPS; os números abaixo descrevem uma máquina de desenvolvimento, não um ambiente de produção.

## Comando

Com a pilha de três instâncias de pé (o nome de projeto é livre; as portas do host são fixas, então só uma pilha por vez):

```sh
docker compose -p wager-wallet-load -f docker-compose.yml -f docker-compose.multi.yml up -d --build --wait
```

```sh
go run ./cmd/loadtest -duration 60s
```

```sh
docker compose -p wager-wallet-load -f docker-compose.yml -f docker-compose.multi.yml down -v
```

O programa sai com código 1 se alguma carteira não reconciliar. Parâmetros (`go run ./cmd/loadtest -h`), com os padrões usados nas execuções abaixo:

| Flag | Padrão | Significado |
| --- | --- | --- |
| `-wallets` | `200` | carteiras abertas antes da carga |
| `-hot` / `-hot-percent` | `4` / `30` | carteiras compartilhadas e a porcentagem de rodadas dirigidas a elas |
| `-hot-balance-cents` / `-balance-cents` | `5000` / `100000` | saldo inicial das carteiras compartilhadas (R$ 50,00, baixo de propósito) e das demais (R$ 1.000,00) |
| `-workers` | `48` | goroutines concorrentes |
| `-duration` | `60s` | duração da carga |
| `-drain-timeout` | `10m` | espera máxima pela drenagem da outbox depois da carga |
| `-apps` / `-metrics` | `:8081-:8083` / `:9090-:9092` | instâncias e endpoints do Prometheus |

## Ambiente

| Item | Valor |
| --- | --- |
| Host | Windows 10 Home 10.0.19045, AMD Ryzen 5 5500 (6 núcleos, 12 threads), 15,9 GB de RAM |
| Docker | Docker Desktop, engine 29.8.2, WSL2 (kernel 6.18.40.1-microsoft-standard-WSL2), 12 CPUs e 7,7 GiB para a VM |
| Pilha | `docker-compose.yml` + `docker-compose.multi.yml` sem alteração: três instâncias da aplicação, cada uma com todos os papéis (HTTP, consumidor, relay da outbox, worker de referências), um Postgres, Keycloak e MiniStack com `AUTH=true` |
| Gerador | `cmd/loadtest` com Go 1.27.2, rodando no próprio host; ele disputa CPU com a pilha |
| Data | 2026-10-18, pilha recém-criada (`down -v` antes de cada execução) |

## Metodologia

1. Obtém tokens `client_credentials` do Keycloak: `wallet-internal` para abrir e reconciliar carteiras e `provider-a` para as operações ([GUIA §7](GUIA.md#7-keycloak-e-identidades-de-teste)).
2. Abre as carteiras em rodízio pelas três instâncias.
3. Lê as métricas das três instâncias (linha de base) e passa a amostrar `outbox_lag_seconds` a cada segundo.
4. Por `-duration`, cada worker repete uma rodada: escolhe uma carteira (30% das vezes uma das 4 compartilhadas, senão uma das 196 demais), envia uma `BET` de R$ 1,00 a R$ 19,99 para uma instância aleatória e, se ela foi processada, envia para outra instância aleatória:
   - 40%: `WIN` do dobro do valor, referenciando a aposta;
   - 20%: `REFUND` do mesmo valor, referenciando a aposta;
   - 5%: a mesma `BET` com a mesma chave (replay idêntico, 200 com `idempotentReplay`);
   - 5%: a mesma chave com outro valor (409 `IDEMPOTENCY_KEY_CONFLICT`);
   - 30%: nada.

   As carteiras compartilhadas recebem cerca de 30% das rodadas em 4 linhas, o que gera disputa pelo lock da carteira entre instâncias e esgota o saldo (422 `INSUFFICIENT_FUNDS`).
5. Ao fim da duração, as requisições ainda em andamento são canceladas e contadas como `CANCELLED_AT_END`, fora das latências; elas podem ter sido gravadas ou não, o que a reconciliação cobre.
6. Lê as métricas de novo, espera a outbox esvaziar (`outbox_unpublished = 0`) e mede o tempo.
7. Reconcilia cada carteira (`POST /wallets/{id}/reconciliation`) e exige `consistent=true` em todas.

Os valores são centavos inteiros formatados como string; o programa só usa ponto flutuante para amostras do Prometheus e para imprimir throughput e latência (ARCHITECTURE §17). Os contadores são somados nas três instâncias como diferença entre o início e o fim da carga. `outbox_lag_seconds` e `outbox_unpublished` são globais (a mesma tabela, o mesmo valor em toda instância), então vale o máximo.

## Resultados

### Execução A: configuração padrão

`go run ./cmd/loadtest -duration 60s -drain-timeout 20m`

48.895 requisições concluídas em 60,0 s: **814,7 req/s**.

| Operação | n | p50 (ms) | p95 (ms) | p99 (ms) | máx. (ms) |
| --- | ---: | ---: | ---: | ---: | ---: |
| Todas | 48.895 | 52,1 | 116,2 | 191,2 | 894,1 |
| `BET` | 32.508 | 53,7 | 123,2 | 205,0 | 682,3 |
| `WIN` | 9.445 | 52,2 | 105,5 | 158,8 | 894,1 |
| `REFUND` | 4.590 | 52,2 | 106,3 | 172,0 | 721,9 |
| replay idêntico | 1.180 | 30,0 | 79,2 | 109,1 | 170,7 |
| chave com outro corpo | 1.172 | 28,5 | 74,2 | 99,9 | 173,1 |

| Operação | Status | Código | Quantidade |
| --- | --- | --- | ---: |
| `BET` | 200 | — | 23.463 |
| `BET` | 422 | `INSUFFICIENT_FUNDS` | 9.045 |
| `WIN` | 200 | — | 9.445 |
| `REFUND` | 200 | — | 4.590 |
| replay idêntico | 200 | — (`idempotentReplay`) | 1.180 |
| chave com outro corpo | 409 | `IDEMPOTENCY_KEY_CONFLICT` | 1.172 |
| várias | — | `CANCELLED_AT_END` | 48 |

Nenhum 5xx, 401 ou erro de transporte durante a carga.

| Métrica | Valor |
| --- | --- |
| `db_concurrency_conflicts_total` (todas as instâncias, todos os `sqlstate`) | 0 |
| `retries_total` | 0 |
| `outbox_lag_seconds`, máximo durante a carga | 51,9 s |
| `outbox_unpublished` ao fim da carga | 73.042 |
| Drenagem da outbox depois da carga | 14 min 56 s (84.462 eventos no total, todos publicados) |
| Reconciliação | 200 de 200 carteiras com `consistent=true` |

Nesta execução, a reconciliação do programa recebeu 401 em todas as carteiras: o token interno expirou durante os 15 minutos de drenagem. O programa passou a buscar um token novo antes de reconciliar, e as 200 carteiras dessa mesma base foram reconciliadas em seguida com esse token (200 de 200 `consistent=true`; menor saldo 0). A execução B já roda com a correção.

### Execução B: relay com 8 goroutines por instância

O mesmo comando, com `-f deploy/loadtest/relay-concurrency.yml` acrescentado ao `up` (`OUTBOX_RELAY_CONCURRENCY=8` nas três instâncias):

```sh
docker compose -p wager-wallet-load -f docker-compose.yml -f docker-compose.multi.yml -f deploy/loadtest/relay-concurrency.yml up -d --build --wait
```

| Medida | A (padrão) | B (relay 8) |
| --- | ---: | ---: |
| Throughput | 814,7 req/s | 643,2 req/s |
| p50 / p95 / p99, todas (ms) | 52,1 / 116,2 / 191,2 | 59,2 / 170,8 / 308,2 |
| 422 `INSUFFICIENT_FUNDS` | 9.045 | 6.610 |
| 409 `IDEMPOTENCY_KEY_CONFLICT` | 1.172 | 951 |
| `db_concurrency_conflicts_total` | 0 | 0 |
| `outbox_lag_seconds` máximo na carga | 51,9 s | 51,3 s |
| `outbox_unpublished` ao fim | 73.042 | 56.133 |
| Drenagem | 14 min 56 s (≈ 82 eventos/s) | 11 min 42 s (≈ 80 eventos/s) |
| Reconciliação | 200/200 | 200/200 |

## Leitura dos resultados

- **Integridade**: as duas execuções terminaram com todas as carteiras reconciliadas e nenhum saldo negativo, com operações da mesma carteira chegando por três processos ao mesmo tempo.
- **Conflitos**: `db_concurrency_conflicts_total` ficou em 0. As operações de uma carteira compartilhada esperam o lock `FOR NO KEY UPDATE` da linha (ARCHITECTURE §4) e não chegam ao `lock_timeout`; a disputa aparece como latência na cauda (p99 e máximo), não como rollback. Os 409 vêm só da reutilização deliberada de chave; os ids externos do teste são únicos, então não há outro 409.
- **422**: as quatro carteiras compartilhadas começam com R$ 50,00 e esgotam o saldo em segundos; daí em diante, uma aposta só passa depois de um `WIN` ou `REFUND` na mesma carteira.
- **Outbox**: o relay publica cerca de 80 eventos/s somando as três instâncias, enquanto a carga gera mais de 1.000 eventos/s, então o atraso cresce durante toda a carga (cerca de 52 s ao fim de 60 s) e a fila esvazia depois: ao fim da drenagem não resta evento não publicado. Oito goroutines por instância não mudaram a taxa de drenagem, então o limite não é a concorrência do relay, e sim o caminho de publicação até o emulador (MiniStack num container local). Este teste não isola a causa; com SQS real, a taxa precisa ser medida de novo antes de dimensionar produção.
- **Throughput entre A e B**: a diferença (814,7 e 643,2 req/s) está dentro da variação de uma máquina compartilhada com o gerador e o Docker Desktop; não deve ser lida como efeito do relay.
