package wager

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCanonicalPayloadGolden(t *testing.T) {
	req := mustRequest(t, raw("BET", "25.00"))
	want := `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET",` +
		`"money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	if got := string(req.canonicalPayloadJSON()); got != want {
		t.Fatalf("canonical payload\n got %s\nwant %s", got, want)
	}
	if strings.Contains(string(req.canonicalPayloadJSON()), "idempotency") {
		t.Fatal("payload hash must exclude the idempotency key")
	}
	// Pinned digest: a change here breaks idempotency for stored rows and
	// requires a new HashVersion. Cross-checked with sha256sum over the
	// golden string above.
	const wantHex = "629836932b79106b99523d06a1e7fa80689b0ea1e1c47aa3f0a5a2c87d0c4344"
	h := req.payloadHash()
	if got := hex.EncodeToString(h[:]); got != wantHex {
		t.Fatalf("payload hash = %s, want %s", got, wantHex)
	}
}

func TestCanonicalMessageGolden(t *testing.T) {
	r := raw("REFUND", "25.00")
	req := mustRequest(t, r)
	want := `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","idempotencyKey":"provider-a:transaction-123",` +
		`"kind":"REFUND","money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","referenceExternalTransactionId":"bet-1","roundId":"round-987",` +
		`"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	if got := string(req.canonicalMessageJSON()); got != want {
		t.Fatalf("canonical message\n got %s\nwant %s", got, want)
	}
	if req.messageHash() == req.payloadHash() {
		t.Fatal("message hash must differ from payload hash")
	}
}

// TestCanonicalKeysSorted re-encodes through a map, which encoding/json
// writes with sorted keys, and expects identical bytes.
func TestCanonicalKeysSorted(t *testing.T) {
	for _, kind := range []string{"BET", "REFUND"} {
		req := mustRequest(t, raw(kind, "25.00"))
		for _, doc := range [][]byte{req.canonicalPayloadJSON(), req.canonicalMessageJSON()} {
			var m map[string]any
			if err := json.Unmarshal(doc, &m); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(m); err != nil {
				t.Fatal(err)
			}
			if got := bytes.TrimSpace(buf.Bytes()); !bytes.Equal(got, doc) {
				t.Errorf("not canonical:\n got %s\nwant %s", doc, got)
			}
		}
	}
}

func TestCanonicalNoHTMLEscaping(t *testing.T) {
	r := raw("BET", "25.00")
	r.GameID = "a<b>&c"
	if got := string(mustRequest(t, r).canonicalPayloadJSON()); !strings.Contains(got, `"gameId":"a<b>&c"`) {
		t.Errorf("HTML escaping applied: %s", got)
	}
}

// TestHashEquivalence: HTTP (key from header) and SQS (key from
// data.idempotencyKey) fill the same RawRequest and must hash the same;
// UUID case does not matter; every business field matters.
func TestHashEquivalence(t *testing.T) {
	httpReq := raw("BET", "25.00")
	sqsReq := raw("BET", "25.00")
	sqsReq.PlayerID = strings.ToUpper(sqsReq.PlayerID)
	sqsReq.WalletID = strings.ToUpper(sqsReq.WalletID)
	a, b := mustRequest(t, httpReq), mustRequest(t, sqsReq)
	if a.payloadHash() != b.payloadHash() || a.messageHash() != b.messageHash() {
		t.Fatal("equivalent requests must hash the same")
	}

	otherKey := httpReq
	otherKey.IdempotencyKey = "another-key"
	if mustRequest(t, otherKey).payloadHash() != a.payloadHash() {
		t.Error("the idempotency key must not affect the payload hash")
	}
	if mustRequest(t, otherKey).messageHash() == a.messageHash() {
		t.Error("the idempotency key must affect the message hash")
	}

	changes := map[string]func(*RawRequest){
		"provider":    func(r *RawRequest) { r.ProviderID = "provider-b" },
		"external id": func(r *RawRequest) { r.ExternalTransactionID = "transaction-124" },
		"player":      func(r *RawRequest) { r.PlayerID = uuid.NewString() },
		"wallet":      func(r *RawRequest) { r.WalletID = uuid.NewString() },
		"round":       func(r *RawRequest) { r.RoundID = "round-988" },
		"game":        func(r *RawRequest) { r.GameID = "other-game" },
		"kind":        func(r *RawRequest) { r.Kind = "WIN" },
		"amount":      func(r *RawRequest) { r.Amount = "25.01" },
		"currency":    func(r *RawRequest) { r.Currency = "USD" },
		"reference":   func(r *RawRequest) { r.Kind = "WIN"; r.ReferenceExternalTransactionID = strp("bet-1") },
	}
	base := raw("BET", "25.00")
	for name, change := range changes {
		r := base
		change(&r)
		if mustRequest(t, r).payloadHash() == a.payloadHash() {
			t.Errorf("changing %s did not change the payload hash", name)
		}
	}

	// An absent reference is omitted, not encoded as null or "".
	win := raw("WIN", "10.00")
	if strings.Contains(string(mustRequest(t, win).canonicalPayloadJSON()), "reference") {
		t.Error("absent reference must be omitted")
	}
}

func TestCheckReplay(t *testing.T) {
	newTx := func(r RawRequest) *WagerTransaction {
		tx, err := NewExternal(mustRequest(t, r), uuid.New(), "corr", t0)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}
	stored := newTx(raw("BET", "25.00"))

	// Same key, same payload: replay.
	if err := CheckReplay(stored, mustRequest(t, raw("BET", "25.00"))); err != nil {
		t.Errorf("replay: %v", err)
	}
	// Same key, different payload: IDEMPOTENCY_KEY_CONFLICT.
	changed := raw("BET", "30.00")
	if err := CheckReplay(stored, mustRequest(t, changed)); !hasConflict(err, CodeIdempotencyKeyConflict) {
		t.Errorf("same key, other payload: %v", err)
	}
	// Same external id, other key, identical payload: EXTERNAL_ID_CONFLICT.
	otherKey := raw("BET", "25.00")
	otherKey.IdempotencyKey = "provider-a:retry-2"
	if err := CheckReplay(stored, mustRequest(t, otherKey)); !hasConflict(err, CodeExternalIDConflict) {
		t.Errorf("same external id, other key: %v", err)
	}
	// Wrong lookups are programming errors.
	unrelated := raw("BET", "25.00")
	unrelated.IdempotencyKey, unrelated.ExternalTransactionID = "k2", "t2"
	if err := CheckReplay(stored, mustRequest(t, unrelated)); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("unrelated: %v", err)
	}
	otherProvider := raw("BET", "25.00")
	otherProvider.ProviderID = "provider-b"
	if err := CheckReplay(stored, mustRequest(t, otherProvider)); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("cross provider: %v", err)
	}
	if err := CheckReplay(nil, mustRequest(t, raw("BET", "25.00"))); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("nil existing: %v", err)
	}
	if err := CheckReplay(stored, Request{}); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("zero request: %v", err)
	}
}

func hasConflict(err error, code FailureCode) bool {
	var ce *ConflictError
	return errors.As(err, &ce) && ce.Code == code && ce.Code.Category() == CategoryConflict
}

// TestExportedHashesRejectZeroRequest: the exported forms agree with the
// internal ones for a valid request and refuse an uninitialised one.
func TestExportedHashesRejectZeroRequest(t *testing.T) {
	req := mustRequest(t, raw("BET", "25.00"))
	if h, err := req.PayloadHash(); err != nil || h != req.payloadHash() {
		t.Errorf("PayloadHash = %x, %v", h, err)
	}
	if h, err := req.MessageHash(); err != nil || h != req.messageHash() {
		t.Errorf("MessageHash = %x, %v", h, err)
	}
	if b, err := req.CanonicalPayload(); err != nil || !bytes.Equal(b, req.canonicalPayloadJSON()) {
		t.Errorf("CanonicalPayload = %s, %v", b, err)
	}
	if b, err := req.CanonicalMessage(); err != nil || !bytes.Equal(b, req.canonicalMessageJSON()) {
		t.Errorf("CanonicalMessage = %s, %v", b, err)
	}

	// A partially filled Request (only kind and money set) is still not a
	// validated one.
	partial := Request{kind: KindBet, money: req.money}
	if _, err := partial.PayloadHash(); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("partial PayloadHash: %v", err)
	}

	var zero Request
	if _, err := zero.PayloadHash(); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("zero PayloadHash: %v", err)
	}
	if _, err := zero.MessageHash(); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("zero MessageHash: %v", err)
	}
	if _, err := zero.CanonicalPayload(); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("zero CanonicalPayload: %v", err)
	}
	if _, err := zero.CanonicalMessage(); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("zero CanonicalMessage: %v", err)
	}
}
