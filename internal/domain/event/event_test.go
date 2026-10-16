package event

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/money"
)

var (
	eventID  = uuid.MustParse("0192f2a0-0000-7000-8000-000000000001")
	walletID = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	txID     = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func meta() Meta {
	// A non-UTC zone checks the UTC normalisation.
	saoPaulo := time.FixedZone("BRT", -3*3600)
	return Meta{
		EventID:       eventID,
		AggregateID:   walletID,
		CorrelationID: "corr-1",
		OccurredAt:    time.Date(2026, 9, 8, 9, 0, 0, 123456789, saoPaulo),
	}
}

func TestWalletBalanceChangedPayload(t *testing.T) {
	e, err := NewWalletBalanceChanged(meta(), WalletBalanceChangedData{
		WalletID: walletID, TransactionID: txID, Direction: "DEBIT",
		Money: brl(t, "25.00"), BalanceBefore: brl(t, "1000.00"), BalanceAfter: brl(t, "975.00"), WalletVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"eventId":"0192f2a0-0000-7000-8000-000000000001","eventType":"WalletBalanceChanged",` +
		`"aggregateId":"0192f291-27dd-7d3f-8071-5f8685deef37","correlationId":"corr-1",` +
		`"occurredAt":"2026-09-08T12:00:00.123Z","version":1,"data":{` +
		`"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","transactionId":"0192f298-345e-7e38-af88-e43f851a819d",` +
		`"direction":"DEBIT","money":{"amount":"25.00","currency":"BRL"},` +
		`"balanceBefore":{"amount":"1000.00","currency":"BRL"},"balanceAfter":{"amount":"975.00","currency":"BRL"},` +
		`"walletVersion":2}}`
	if got := string(e.Payload()); got != want {
		t.Fatalf("payload\n got %s\nwant %s", got, want)
	}
	if e.Type() != TypeWalletBalanceChanged || e.Version() != 1 || e.AggregateType() != "Wallet" || e.OccurredAt().Location() != time.UTC {
		t.Errorf("envelope %+v", e)
	}
	if _, ok := e.CausationID(); ok {
		t.Error("causation should be absent")
	}
}

func TestPayloadIsImmutable(t *testing.T) {
	e, err := NewWagerTransactionProcessed(meta(), WagerTransactionProcessedData{
		TransactionID: txID, WalletID: walletID, Origin: "EXTERNAL", Kind: "LOSS", ProviderID: "provider-a",
		ExternalTransactionID: "t-1", RoundID: "r", GameID: "g", Money: brl(t, "0.00"), Balance: brl(t, "975.00"), WalletVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := e.Payload()
	p[0] = 'X'
	if e.Payload()[0] != '{' {
		t.Fatal("Payload must return a copy")
	}
}

func TestEnvelopeFields(t *testing.T) {
	m := meta()
	m.CausationID = ptr("msg-123")
	e, err := NewWagerTransactionRejected(m, WagerTransactionRejectedData{
		TransactionID: txID, WalletID: walletID, Kind: "BET", ProviderID: "provider-a", ExternalTransactionID: "t-2",
		FailureCode: "INSUFFICIENT_FUNDS", Money: brl(t, "80.00"), Balance: brl(t, "20.00"), WalletVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(e.Payload(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"eventId", "eventType", "aggregateId", "correlationId", "causationId", "occurredAt", "version", "data"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("envelope lacks %s", k)
		}
	}
	data := doc["data"].(map[string]any)
	if data["failureCode"] != "INSUFFICIENT_FUNDS" || data["balance"].(map[string]any)["amount"] != "20.00" {
		t.Errorf("data = %v", data)
	}
	// Money is always a decimal string, never a JSON number.
	if _, isString := data["money"].(map[string]any)["amount"].(string); !isString {
		t.Error("money amount must be a string")
	}
}

func TestPendingReferencePayload(t *testing.T) {
	e, err := NewWagerTransactionPendingReference(meta(), WagerTransactionPendingReferenceData{
		TransactionID: txID, WalletID: walletID, Kind: "REFUND", ProviderID: "provider-a", ExternalTransactionID: "t-3",
		ReferenceExternalTransactionID: "bet-1", ReferenceDeadlineAt: FormatTime(time.Date(2026, 9, 8, 12, 10, 0, 0, time.UTC)),
		Money: brl(t, "25.00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Type() != TypeWagerTransactionPendingReference {
		t.Errorf("type = %s", e.Type())
	}
	d := e.Data().(WagerTransactionPendingReferenceData)
	if d.ReferenceDeadlineAt != "2026-09-08T12:10:00.000Z" {
		t.Errorf("deadline = %s", d.ReferenceDeadlineAt)
	}
}

func TestConstructorsReject(t *testing.T) {
	good := WalletBalanceChangedData{
		WalletID: walletID, TransactionID: txID, Direction: "CREDIT",
		Money: brl(t, "1.00"), BalanceBefore: brl(t, "0.00"), BalanceAfter: brl(t, "1.00"), WalletVersion: 1,
	}
	metaCases := map[string]func(*Meta){
		"nil event id":    func(m *Meta) { m.EventID = uuid.Nil },
		"nil aggregate":   func(m *Meta) { m.AggregateID = uuid.Nil },
		"no correlation":  func(m *Meta) { m.CorrelationID = "" },
		"zero time":       func(m *Meta) { m.OccurredAt = time.Time{} },
		"empty causation": func(m *Meta) { m.CausationID = ptr("") },
	}
	for name, mut := range metaCases {
		m := meta()
		mut(&m)
		if _, err := NewWalletBalanceChanged(m, good); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	dataCases := map[string]func(*WalletBalanceChangedData){
		"aggregate is not the wallet": func(d *WalletBalanceChangedData) { d.WalletID = uuid.New() },
		"nil transaction":             func(d *WalletBalanceChangedData) { d.TransactionID = uuid.Nil },
		"uninitialised money":         func(d *WalletBalanceChangedData) { d.Money = money.Money{} },
		"version 0":                   func(d *WalletBalanceChangedData) { d.WalletVersion = 0 },
	}
	for name, mut := range dataCases {
		d := good
		mut(&d)
		if _, err := NewWalletBalanceChanged(meta(), d); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := NewWagerTransactionRejected(meta(), WagerTransactionRejectedData{TransactionID: txID, WalletID: walletID}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("rejected without code: %v", err)
	}
}

func ptr(s string) *string { return &s }

func TestEnvelopeValid(t *testing.T) {
	e, err := NewWalletBalanceChanged(meta(), WalletBalanceChangedData{
		WalletID: walletID, TransactionID: txID, Direction: "CREDIT",
		Money: brl(t, "1.00"), BalanceBefore: brl(t, "0.00"), BalanceAfter: brl(t, "1.00"), WalletVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Valid() {
		t.Error("constructed envelope must be valid")
	}
	var zero Envelope
	if zero.Valid() {
		t.Error("zero envelope must not be valid")
	}
}
