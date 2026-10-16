package wager

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/money"
)

var t0 = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

const (
	playerStr = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	walletStr = "0192f291-27dd-7d3f-8071-5f8685deef37"
)

// sequence returns a deterministic id source: 00000000-0000-7000-8000-<n>.
func sequence() func() uuid.UUID {
	n := 0
	return func() uuid.UUID {
		n++
		return uuid.MustParse(fmt.Sprintf("00000000-0000-7000-8000-%012d", n))
	}
}

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func strp(s string) *string { return &s }

// raw returns a valid RawRequest of kind with amount, as in CHALLENGE §9.
func raw(kind, amount string) RawRequest {
	r := RawRequest{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PlayerID:              playerStr,
		WalletID:              walletStr,
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Amount:                amount,
		Currency:              "BRL",
	}
	if kind == "REFUND" || kind == "ROLLBACK" {
		r.ReferenceExternalTransactionID = strp("bet-1")
	}
	return r
}

func mustRequest(t *testing.T, r RawRequest) Request {
	t.Helper()
	req, err := NewRequest(r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}
