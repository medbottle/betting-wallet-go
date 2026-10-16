//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/adapters/httpapi"
	"github.com/medbottle/betting-wallet-go/internal/adapters/postgres"
	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/app/wallets"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
	"github.com/medbottle/betting-wallet-go/internal/platform/clock"
	"github.com/medbottle/betting-wallet-go/internal/platform/idgen"
	"github.com/medbottle/betting-wallet-go/internal/platform/metrics"
)

// apiEnv is the real HTTP stack: router, middleware, use cases and the
// pgx adapter on a migrated Postgres, tokens from a real Keycloak.
type apiEnv struct {
	repo     repoEnv
	srv      *httptest.Server
	tokens   map[string]string
	wagering *wagering.Service
	logger   *slog.Logger
	metrics  *metrics.Registry
}

type apiOption func(*apiConfig)

type apiConfig struct {
	runnerFrom func(repo repoEnv) app.TxRunner
	reference  wagering.Config
}

// withRunner replaces the transaction runner (a short lock timeout, a
// fault injector).
func withRunner(r func(repo repoEnv) app.TxRunner) apiOption {
	return func(c *apiConfig) { c.runnerFrom = r }
}

// withReference sets the reference TTL and backoff (short values make the
// worker tests fast; ARCHITECTURE §8 uses a 2s TTL in tests).
func withReference(ttl, base, max time.Duration) apiOption {
	return func(c *apiConfig) {
		c.reference = wagering.Config{ReferenceTTL: ttl, Backoff: wagering.Backoff{Base: base, Max: max}}
	}
}

func newAPIEnv(t *testing.T, opts ...apiOption) apiEnv {
	t.Helper()
	repo := newRepoEnv(t)
	kc := startKeycloak(t)
	cfg := apiConfig{reference: wagering.Config{ReferenceTTL: refTTL, Backoff: wagering.Backoff{Base: time.Second, Max: time.Minute}}}
	for _, o := range opts {
		o(&cfg)
	}
	var runner app.TxRunner = repo.runner
	if cfg.runnerFrom != nil {
		runner = cfg.runnerFrom(repo)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	reader := postgres.NewReader(repo.db.app)
	ids := idgen.UUIDv7{}
	reg := metrics.New()
	wag := wagering.NewService(runner, reader, reader, reader, clock.System{}, ids, cfg.reference, logger, reg)
	wal := wallets.NewService(runner, reader, reader, clock.System{}, ids, logger, reg)
	router := httpapi.NewRouter(httpapi.Deps{
		Verifier:       newKeycloakVerifier(t, kc, kc.Issuer, "wallet-api", nil),
		Wagering:       wag,
		Wallets:        wal,
		NewID:          ids.NewID,
		Logger:         logger,
		HandlerTimeout: 15 * time.Second,
		Observer:       reg,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	tokens := map[string]string{}
	for id := range clientSecrets {
		tokens[id] = mustToken(t, kc, id)
	}
	return apiEnv{repo: repo, srv: srv, tokens: tokens, wagering: wag, logger: logger, metrics: reg}
}

// response is a decoded API answer.
type response struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

func (r response) errorCode() string {
	e, _ := r.Body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func (r response) str(key string) string {
	v, _ := r.Body[key].(string)
	return v
}

func (r response) amount(key string) string {
	m, _ := r.Body[key].(map[string]any)
	v, _ := m["amount"].(string)
	return v
}

// call sends a request. body may be nil, a string (sent verbatim) or a
// value encoded as JSON. headers are pairs.
func (e apiEnv) call(t *testing.T, method, path, client string, body any, headers ...string) response {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if client != "" {
		req.Header.Set("Authorization", "Bearer "+e.tokens[client])
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := response{Status: resp.StatusCode, Header: resp.Header, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

// apiWallet is a wallet opened through the API.
type apiWallet struct {
	ID, PlayerID string
}

func (e apiEnv) openWallet(t *testing.T, balance string) apiWallet {
	t.Helper()
	player := uuid.NewString()
	r := e.call(t, http.MethodPost, "/wallets", "wallet-internal", map[string]any{
		"playerId": player, "initialBalance": map[string]string{"amount": balance, "currency": "BRL"},
	})
	if r.Status != http.StatusCreated {
		t.Fatalf("open wallet: %d %s", r.Status, r.Raw)
	}
	return apiWallet{ID: r.str("id"), PlayerID: player}
}

// op builds a submission body for w.
func op(w apiWallet, provider, kind, amount, externalID string, ref ...string) map[string]any {
	b := map[string]any{
		"providerId": provider, "externalTransactionId": externalID, "playerId": w.PlayerID, "walletId": w.ID,
		"roundId": "round-1", "gameId": "fortune-chimp", "kind": kind,
		"money": map[string]string{"amount": amount, "currency": "BRL"},
	}
	if len(ref) > 0 {
		b["referenceExternalTransactionId"] = ref[0]
	}
	return b
}

// submit posts body with Idempotency-Key provider:externalId.
func (e apiEnv) submit(t *testing.T, client string, body map[string]any) response {
	t.Helper()
	key := fmt.Sprintf("%s:%s", body["providerId"], body["externalTransactionId"])
	return e.call(t, http.MethodPost, "/wagering/transactions", client, body, "Idempotency-Key", key)
}

// assertConsistent checks stored balance against the ledger through the
// reconciliation endpoint (CHALLENGE §13: verify at the end).
func (e apiEnv) assertConsistent(t *testing.T, w apiWallet, wantBalance string) {
	t.Helper()
	r := e.call(t, http.MethodPost, "/wallets/"+w.ID+"/reconciliation", "wallet-internal", nil)
	if r.Status != http.StatusOK || r.Body["consistent"] != true || r.amount("storedBalance") != wantBalance || r.amount("calculatedBalance") != wantBalance {
		t.Fatalf("reconciliation of %s: %d %s (want balance %s)", w.ID, r.Status, r.Raw, wantBalance)
	}
}

// count runs a count query as the owner.
func (e apiEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.repo.db.owner.QueryRow(ctxT(t), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// faultyRunner injects one invariant violation at the next ledger insert,
// the only way to reach the FAILED path: the database correctly prevents
// it otherwise (ARCHITECTURE §5).
type faultyRunner struct {
	inner app.TxRunner
	armed *atomic.Bool
}

func (f faultyRunner) InTx(ctx context.Context, opts app.TxOptions, fn func(ctx context.Context, r app.Repos) error) error {
	return f.inner.InTx(ctx, opts, func(ctx context.Context, r app.Repos) error {
		return fn(ctx, faultyRepos{Repos: r, armed: f.armed})
	})
}

type faultyRepos struct {
	app.Repos
	armed *atomic.Bool
}

func (r faultyRepos) Ledger() app.LedgerRepository {
	return faultyLedger{LedgerRepository: r.Repos.Ledger(), armed: r.armed}
}

type faultyLedger struct {
	app.LedgerRepository
	armed *atomic.Bool
}

func (l faultyLedger) Insert(ctx context.Context, e wallet.LedgerEntry) error {
	if l.armed.CompareAndSwap(true, false) {
		return fmt.Errorf("%w: injected ledger fault", app.ErrInvariantViolation)
	}
	return l.LedgerRepository.Insert(ctx, e)
}
