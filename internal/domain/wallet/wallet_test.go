package wallet

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/money"
)

var t0 = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openWith(t *testing.T, balance string) *Wallet {
	t.Helper()
	w, _, err := Open(OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, balance),
		OpeningTransactionID: uuid.New(), OpeningEntryID: uuid.New(), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestOpenWithBalance(t *testing.T) {
	txID, entryID := uuid.New(), uuid.New()
	w, entry, err := Open(OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "1000.00"),
		OpeningTransactionID: txID, OpeningEntryID: entryID, Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 1 || w.Balance().String() != "1000.00" || w.Currency() != money.BRL {
		t.Errorf("wallet = v%d %s", w.Version(), w.Balance())
	}
	if entry == nil {
		t.Fatal("expected opening entry")
	}
	if entry.Direction() != Credit || entry.BalanceBefore().String() != "0.00" || entry.BalanceAfter().String() != "1000.00" ||
		entry.WalletVersion() != 1 || entry.TransactionID() != txID || entry.ID() != entryID || entry.WalletID() != w.ID() {
		t.Errorf("unexpected opening entry %+v", entry)
	}
}

func TestOpenWithZeroBalance(t *testing.T) {
	w, entry, err := Open(OpenParams{ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil || w.Version() != 1 || !w.Balance().IsZero() {
		t.Errorf("zero opening: entry=%v version=%d balance=%s", entry, w.Version(), w.Balance())
	}
}

func TestOpenRejects(t *testing.T) {
	neg, _ := money.FromMinor(-1, money.BRL)
	cases := map[string]OpenParams{
		"nil id":             {PlayerID: uuid.New(), InitialBalance: brl(t, "1.00"), OpeningTransactionID: uuid.New(), OpeningEntryID: uuid.New(), Now: t0},
		"nil player":         {ID: uuid.New(), InitialBalance: brl(t, "1.00"), OpeningTransactionID: uuid.New(), OpeningEntryID: uuid.New(), Now: t0},
		"zero money":         {ID: uuid.New(), PlayerID: uuid.New(), Now: t0},
		"negative balance":   {ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: neg, Now: t0},
		"zero time":          {ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "0.00")},
		"missing opening tx": {ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "1.00"), OpeningEntryID: uuid.New(), Now: t0},
	}
	for name, p := range cases {
		if _, _, err := Open(p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDebitAndCredit(t *testing.T) {
	w := openWith(t, "100.00")
	tx1, e1 := uuid.New(), uuid.New()
	d, err := w.Debit(tx1, e1, brl(t, "80.00"), t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().String() != "20.00" || w.Version() != 2 {
		t.Errorf("after debit: %s v%d", w.Balance(), w.Version())
	}
	if d.Direction() != Debit || d.BalanceBefore().String() != "100.00" || d.BalanceAfter().String() != "20.00" ||
		d.WalletVersion() != 2 || d.TransactionID() != tx1 || d.ID() != e1 {
		t.Errorf("unexpected debit entry %+v", d)
	}
	if !w.UpdatedAt().Equal(t0.Add(time.Second)) {
		t.Errorf("updatedAt = %v", w.UpdatedAt())
	}

	c, err := w.Credit(uuid.New(), uuid.New(), brl(t, "5.50"), t0.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().String() != "25.50" || w.Version() != 3 || c.WalletVersion() != 3 || c.BalanceAfter().String() != "25.50" {
		t.Errorf("after credit: %s v%d", w.Balance(), w.Version())
	}
}

func TestDebitToExactlyZero(t *testing.T) {
	w := openWith(t, "80.00")
	if _, err := w.Debit(uuid.New(), uuid.New(), brl(t, "80.00"), t0); err != nil {
		t.Fatal(err)
	}
	if !w.Balance().IsZero() || w.Version() != 2 {
		t.Errorf("balance %s v%d", w.Balance(), w.Version())
	}
}

func TestInsufficientFundsLeavesWalletUntouched(t *testing.T) {
	w := openWith(t, "20.00")
	_, err := w.Debit(uuid.New(), uuid.New(), brl(t, "80.00"), t0)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if w.Balance().String() != "20.00" || w.Version() != 1 {
		t.Errorf("wallet changed: %s v%d", w.Balance(), w.Version())
	}
}

func TestMovementRejects(t *testing.T) {
	usd, _ := money.Parse("1.00", "USD")
	cases := []struct {
		name   string
		amount money.Money
		want   error
	}{
		{"zero amount", brl(t, "0.00"), ErrInvalidAmount},
		{"negative amount", func() money.Money { m, _ := money.FromMinor(-100, money.BRL); return m }(), ErrInvalidAmount},
		{"uninitialised amount", money.Money{}, ErrInvalidAmount},
		{"other currency", usd, money.ErrCurrencyMismatch},
	}
	for _, c := range cases {
		for _, dir := range []Direction{Debit, Credit} {
			w := openWith(t, "100.00")
			_, err := w.move(dir, uuid.New(), uuid.New(), c.amount, t0)
			if !errors.Is(err, c.want) {
				t.Errorf("%s %s: err = %v, want %v", dir, c.name, err, c.want)
			}
			if w.Version() != 1 || w.Balance().String() != "100.00" {
				t.Errorf("%s %s: wallet changed", dir, c.name)
			}
		}
	}

	w := openWith(t, "100.00")
	if _, err := w.Debit(uuid.Nil, uuid.New(), brl(t, "1.00"), t0); !errors.Is(err, ErrInvalidLedgerEntry) {
		t.Errorf("nil transaction id: %v", err)
	}
	if _, err := w.Credit(uuid.New(), uuid.New(), brl(t, "1.00"), time.Time{}); !errors.Is(err, ErrInvalidWallet) {
		t.Errorf("zero time: %v", err)
	}
	if w.Version() != 1 {
		t.Error("wallet changed after rejected movements")
	}
}

func TestCreditOverflow(t *testing.T) {
	max, _ := money.FromMinor(math.MaxInt64, money.BRL)
	w, err := Rehydrate(Snapshot{ID: uuid.New(), PlayerID: uuid.New(), Balance: max, Version: 7, CreatedAt: t0, UpdatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Credit(uuid.New(), uuid.New(), brl(t, "0.01"), t0); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("err = %v, want ErrAmountOverflow", err)
	}
	if w.Version() != 7 {
		t.Error("wallet changed")
	}
}

func TestUninitialisedWallet(t *testing.T) {
	var w Wallet
	if _, err := w.Debit(uuid.New(), uuid.New(), brl(t, "1.00"), t0); !errors.Is(err, ErrInvalidWallet) {
		t.Errorf("zero wallet debit: %v", err)
	}
	var nilW *Wallet
	if _, err := nilW.Credit(uuid.New(), uuid.New(), brl(t, "1.00"), t0); !errors.Is(err, ErrInvalidWallet) {
		t.Errorf("nil wallet credit: %v", err)
	}
}

func TestRehydrate(t *testing.T) {
	good := Snapshot{ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, "975.00"), Version: 2, CreatedAt: t0, UpdatedAt: t0.Add(time.Minute)}
	w, err := Rehydrate(good)
	if err != nil {
		t.Fatal(err)
	}
	if w.Snapshot() != good {
		t.Errorf("snapshot round trip: %+v", w.Snapshot())
	}

	neg, _ := money.FromMinor(-1, money.BRL)
	bad := map[string]func(Snapshot) Snapshot{
		"nil id":           func(s Snapshot) Snapshot { s.ID = uuid.Nil; return s },
		"nil player":       func(s Snapshot) Snapshot { s.PlayerID = uuid.Nil; return s },
		"negative balance": func(s Snapshot) Snapshot { s.Balance = neg; return s },
		"zero money":       func(s Snapshot) Snapshot { s.Balance = money.Money{}; return s },
		"version 0":        func(s Snapshot) Snapshot { s.Version = 0; return s },
		"updated < create": func(s Snapshot) Snapshot { s.UpdatedAt = t0.Add(-time.Second); return s },
		"zero created":     func(s Snapshot) Snapshot { s.CreatedAt = time.Time{}; return s },
	}
	for name, mut := range bad {
		if _, err := Rehydrate(mut(good)); !errors.Is(err, ErrInvalidWallet) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestLedgerEntryInvariants(t *testing.T) {
	base := LedgerEntryParams{
		ID: uuid.New(), WalletID: uuid.New(), TransactionID: uuid.New(), Direction: Debit,
		Amount: brl(t, "25.00"), BalanceBefore: brl(t, "100.00"), BalanceAfter: brl(t, "75.00"),
		WalletVersion: 2, CreatedAt: t0,
	}
	if _, err := NewLedgerEntry(base); err != nil {
		t.Fatalf("valid entry: %v", err)
	}
	if _, err := RehydrateLedgerEntry(base); err != nil {
		t.Fatalf("rehydrate valid entry: %v", err)
	}
	usd, _ := money.Parse("75.00", "USD")
	bad := map[string]func(LedgerEntryParams) LedgerEntryParams{
		"wrong arithmetic":  func(p LedgerEntryParams) LedgerEntryParams { p.BalanceAfter = brl(t, "80.00"); return p },
		"wrong direction":   func(p LedgerEntryParams) LedgerEntryParams { p.Direction = Credit; return p },
		"invalid direction": func(p LedgerEntryParams) LedgerEntryParams { p.Direction = "SIDEWAYS"; return p },
		"zero amount":       func(p LedgerEntryParams) LedgerEntryParams { p.Amount = brl(t, "0.00"); return p },
		"currency mismatch": func(p LedgerEntryParams) LedgerEntryParams { p.BalanceAfter = usd; return p },
		"negative after": func(p LedgerEntryParams) LedgerEntryParams {
			p.BalanceBefore = brl(t, "10.00")
			p.BalanceAfter, _ = money.FromMinor(-1500, money.BRL)
			return p
		},
		"version 0":          func(p LedgerEntryParams) LedgerEntryParams { p.WalletVersion = 0; return p },
		"nil transaction id": func(p LedgerEntryParams) LedgerEntryParams { p.TransactionID = uuid.Nil; return p },
		"zero time":          func(p LedgerEntryParams) LedgerEntryParams { p.CreatedAt = time.Time{}; return p },
	}
	for name, mut := range bad {
		if _, err := NewLedgerEntry(mut(base)); !errors.Is(err, ErrInvalidLedgerEntry) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := ParseDirection("CREDIT"); err != nil {
		t.Error(err)
	}
	if _, err := ParseDirection("credit"); !errors.Is(err, ErrInvalidLedgerEntry) {
		t.Errorf("lowercase direction: %v", err)
	}
}

func TestLedgerEntryValid(t *testing.T) {
	w := openWith(t, "100.00")
	e, err := w.Debit(uuid.New(), uuid.New(), brl(t, "1.00"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Valid() {
		t.Error("constructed entry must be valid")
	}
	var zero LedgerEntry
	if zero.Valid() {
		t.Error("zero entry must not be valid")
	}
}
