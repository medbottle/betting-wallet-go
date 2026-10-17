package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/app/authz"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

// lookupCounter is a WageringService that knows one transaction and counts
// the lookups by id.
type lookupCounter struct {
	tx      *wager.WagerTransaction
	lookups atomic.Int64
}

func (s *lookupCounter) Submit(context.Context, wagering.SubmitCommand) (wagering.SubmitResult, error) {
	return wagering.SubmitResult{}, app.ErrTransient
}

func (s *lookupCounter) Get(_ context.Context, id uuid.UUID) (*wager.WagerTransaction, error) {
	s.lookups.Add(1)
	if id == s.tx.ID() {
		return s.tx, nil
	}
	return nil, app.ErrNotFound
}

func (s *lookupCounter) GetByExternalID(context.Context, string, string) (*wager.WagerTransaction, error) {
	return nil, app.ErrNotFound
}

func providerATransaction(t *testing.T) *wager.WagerTransaction {
	t.Helper()
	req, err := wager.NewRequest(wager.RawRequest{
		ProviderID: "provider-a", ExternalTransactionID: "tx-1", IdempotencyKey: "provider-a:tx-1",
		PlayerID: uuid.NewString(), WalletID: uuid.NewString(), RoundID: "round-1", GameID: "game-1",
		Kind: "BET", Amount: "1.00", Currency: "BRL",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := wager.NewExternal(req, uuid.New(), "corr-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// TestGetTransactionRefusesBeforeTheLookup: CHALLENGE §13, no data exposed
// on unauthorized access. A caller with neither the provider nor the
// wallet-internal role gets 403 before any lookup, so the answer is the
// same whether the transaction exists or not (ARCHITECTURE §11, §26 I1).
func TestGetTransactionRefusesBeforeTheLookup(t *testing.T) {
	svc := &lookupCounter{tx: providerATransaction(t)}
	principals := map[string]authz.Principal{}
	for token, claims := range map[string]struct {
		client, provider string
		roles            []string
	}{
		"no-role":    {"some-client", "", nil},
		"provider-a": {"provider-a", "provider-a", []string{"provider"}},
		"provider-b": {"provider-b", "provider-b", []string{"provider"}},
	} {
		p, err := authz.NewPrincipal(claims.client, claims.provider, claims.roles)
		if err != nil {
			t.Fatal(err)
		}
		principals[token] = p
	}
	router := NewRouter(Deps{Verifier: tokenTable(principals), Wagering: svc, HandlerTimeout: time.Second})
	get := func(token, id string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/wagering/transactions/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		router.ServeHTTP(rec, req)
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Error.Code
	}

	for _, id := range []string{svc.tx.ID().String(), uuid.NewString(), "not-a-uuid"} {
		if status, code := get("no-role", id); status != http.StatusForbidden || code != codeForbidden {
			t.Errorf("no role, %s: %d %s; want 403 FORBIDDEN", id, status, code)
		}
	}
	if n := svc.lookups.Load(); n != 0 {
		t.Fatalf("%d lookups for a caller with no role", n)
	}

	// Providers still look up, and another provider's transaction stays
	// hidden as 404 like a missing one.
	if status, _ := get("provider-a", svc.tx.ID().String()); status != http.StatusOK {
		t.Errorf("owner: %d", status)
	}
	for _, id := range []string{svc.tx.ID().String(), uuid.NewString()} {
		if status, code := get("provider-b", id); status != http.StatusNotFound || code != codeTransactionNotFound {
			t.Errorf("provider-b, %s: %d %s; want 404", id, status, code)
		}
	}
}

// tokenTable verifies a bearer token by looking its principal up.
type tokenTable map[string]authz.Principal

func (tt tokenTable) Verify(_ context.Context, raw string) (authz.Principal, error) {
	p, ok := tt[raw]
	if !ok {
		return authz.Principal{}, errors.New("unknown token")
	}
	return p, nil
}
