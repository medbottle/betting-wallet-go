package wager

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/event"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

const ttl = 10 * time.Minute

// world is a wallet with its transactions, for rule tests.
type world struct {
	t      *testing.T
	wallet *wallet.Wallet
	newID  func() uuid.UUID
	n      int
}

func newWorld(t *testing.T, balance string) *world {
	t.Helper()
	w, err := wallet.Rehydrate(wallet.Snapshot{
		ID: uuid.MustParse(walletStr), PlayerID: uuid.MustParse(playerStr),
		Balance: brl(t, balance), Version: 1, CreatedAt: t0, UpdatedAt: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, wallet: w, newID: sequence()}
}

// tx builds a PENDING external operation with a unique external id.
func (w *world) tx(kind, amount, ref string) *WagerTransaction {
	w.t.Helper()
	w.n++
	r := raw(kind, amount)
	r.ExternalTransactionID = fmt.Sprintf("%s-%d", strings.ToLower(kind), w.n)
	r.IdempotencyKey = "provider-a:" + r.ExternalTransactionID
	r.ReferenceExternalTransactionID = nil
	if ref != "" {
		r.ReferenceExternalTransactionID = strp(ref)
	}
	tx, err := NewExternal(mustRequest(w.t, r), w.newID(), "corr-"+r.ExternalTransactionID, t0)
	if err != nil {
		w.t.Fatal(err)
	}
	return tx
}

func (w *world) params(tx, ref *WagerTransaction, reversed bool, now time.Time) ApplyParams {
	return ApplyParams{
		Transaction:   tx,
		Wallet:        w.wallet,
		Reference:     ReferenceState{Transaction: ref, Reversed: reversed},
		ReferenceTTL:  ttl,
		NextAttemptAt: now.Add(time.Second),
		Now:           now,
		NewID:         w.newID,
	}
}

func (w *world) applyAt(tx, ref *WagerTransaction, reversed bool, now time.Time) Outcome {
	w.t.Helper()
	out, err := Apply(w.params(tx, ref, reversed, now))
	if err != nil {
		w.t.Fatalf("Apply(%s %s): %v", tx.Kind(), tx.ExternalTransactionID(), err)
	}
	if out.Status != tx.Status() {
		w.t.Fatalf("outcome status %s != transaction status %s", out.Status, tx.Status())
	}
	return out
}

func (w *world) apply(tx, ref *WagerTransaction, reversed bool) Outcome {
	w.t.Helper()
	return w.applyAt(tx, ref, reversed, t0)
}

// processed applies tx and asserts PROCESSED.
func (w *world) processed(tx, ref *WagerTransaction) *WagerTransaction {
	w.t.Helper()
	if out := w.apply(tx, ref, false); out.Status != StatusProcessed {
		w.t.Fatalf("%s: %s %s, want PROCESSED", tx.ExternalTransactionID(), out.Status, out.FailureCode)
	}
	return tx
}

func (w *world) balance() string { return w.wallet.Balance().String() }

func expectRejected(t *testing.T, out Outcome, code FailureCode) {
	t.Helper()
	if out.Status != StatusRejected || out.FailureCode != code {
		t.Fatalf("outcome %s %s, want REJECTED %s", out.Status, out.FailureCode, code)
	}
	if out.Entry != nil {
		t.Fatal("a rejection must not produce a ledger entry")
	}
	if types := eventTypes(out); len(types) != 1 || types[0] != event.TypeWagerTransactionRejected {
		t.Fatalf("events = %v, want [WagerTransactionRejected]", types)
	}
}

func eventTypes(out Outcome) []event.Type {
	var ts []event.Type
	for _, e := range out.Events {
		ts = append(ts, e.Type())
	}
	return ts
}

func TestApplyBet(t *testing.T) {
	w := newWorld(t, "100.00")
	bet := w.tx("BET", "25.00", "")
	out := w.apply(bet, nil, false)

	if out.Status != StatusProcessed || w.balance() != "75.00" || w.wallet.Version() != 2 {
		t.Fatalf("status %s balance %s v%d", out.Status, w.balance(), w.wallet.Version())
	}
	if out.Entry == nil || out.Entry.Direction() != wallet.Debit || out.Entry.TransactionID() != bet.ID() || out.Entry.WalletVersion() != 2 {
		t.Fatalf("entry = %+v", out.Entry)
	}
	if r, _ := bet.Result(); r.Balance.String() != "75.00" || r.WalletVersion != 2 {
		t.Errorf("result = %+v", r)
	}
	types := eventTypes(out)
	if len(types) != 2 || types[0] != event.TypeWagerTransactionProcessed || types[1] != event.TypeWalletBalanceChanged {
		t.Fatalf("events = %v", types)
	}
	for _, e := range out.Events {
		if e.AggregateID() != w.wallet.ID() || e.CorrelationID() != bet.CorrelationID() || !e.OccurredAt().Equal(t0) || e.Version() != 1 {
			t.Errorf("event meta %+v", e)
		}
	}
	if out.Events[0].EventID() == out.Events[1].EventID() {
		t.Error("event ids must be distinct")
	}
	bc := out.Events[1].Data().(event.WalletBalanceChangedData)
	if bc.Direction != "DEBIT" || bc.BalanceBefore.String() != "100.00" || bc.BalanceAfter.String() != "75.00" || bc.WalletVersion != 2 || bc.Money.String() != "25.00" {
		t.Errorf("balance changed data = %+v", bc)
	}
}

// TestTwoBetsOnOneHundred is CHALLENGE §8 played sequentially, the order the
// wallet lock imposes: one bet processed, one rejected, 20.00 left, one
// debit.
func TestTwoBetsOnOneHundred(t *testing.T) {
	w := newWorld(t, "100.00")
	first, second := w.tx("BET", "80.00", ""), w.tx("BET", "80.00", "")

	a := w.apply(first, nil, false)
	b := w.apply(second, nil, false)

	if a.Status != StatusProcessed || a.Entry == nil {
		t.Fatalf("first bet: %s", a.Status)
	}
	expectRejected(t, b, CodeInsufficientFunds)
	if w.balance() != "20.00" || w.wallet.Version() != 2 {
		t.Fatalf("balance %s v%d, want 20.00 v2", w.balance(), w.wallet.Version())
	}
	if r, _ := second.Result(); r.Balance.String() != "20.00" || r.WalletVersion != 2 {
		t.Errorf("rejected result must record the observed balance, got %+v", r)
	}
	// Replaying either transaction is refused: they are terminal.
	if _, err := Apply(w.params(first, nil, false, t0)); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("re-applying a processed bet: %v", err)
	}
}

func TestApplyWinAndLoss(t *testing.T) {
	w := newWorld(t, "100.00")
	win := w.processed(w.tx("WIN", "10.00", ""), nil)
	if w.balance() != "110.00" || w.wallet.Version() != 2 {
		t.Fatalf("after win: %s v%d", w.balance(), w.wallet.Version())
	}
	if _, ok := win.ReferenceTransactionID(); ok {
		t.Error("win without reference resolved one")
	}

	loss := w.tx("LOSS", "0.00", "")
	out := w.apply(loss, nil, false)
	if out.Status != StatusProcessed || out.Entry != nil || w.wallet.Version() != 2 || w.balance() != "110.00" {
		t.Fatalf("loss: %s entry=%v v%d", out.Status, out.Entry, w.wallet.Version())
	}
	if types := eventTypes(out); len(types) != 1 || types[0] != event.TypeWagerTransactionProcessed {
		t.Fatalf("loss events = %v, want only WagerTransactionProcessed", types)
	}
	if r, _ := loss.Result(); r.Balance.String() != "110.00" || r.WalletVersion != 2 {
		t.Errorf("loss result = %+v", r)
	}
}

func TestApplyWinWithReference(t *testing.T) {
	w := newWorld(t, "100.00")
	bet := w.processed(w.tx("BET", "25.00", ""), nil)
	win := w.processed(w.tx("WIN", "60.00", bet.ExternalTransactionID()), bet)
	if id, ok := win.ReferenceTransactionID(); !ok || id != bet.ID() {
		t.Errorf("reference id = %s, %v", id, ok)
	}
	if w.balance() != "135.00" {
		t.Errorf("balance %s", w.balance())
	}
	// Several WINs may reference the same BET (ARCHITECTURE premise).
	w.processed(w.tx("WIN", "1.00", bet.ExternalTransactionID()), bet)
}

func TestApplyReversals(t *testing.T) {
	cases := []struct {
		name      string
		refKind   string
		kind      string
		wantDir   wallet.Direction
		wantAfter string
	}{
		{"refund of bet", "BET", "REFUND", wallet.Credit, "100.00"},
		{"rollback of bet", "BET", "ROLLBACK", wallet.Credit, "100.00"},
		{"rollback of win", "WIN", "ROLLBACK", wallet.Debit, "100.00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, "100.00")
			ref := w.processed(w.tx(tc.refKind, "25.00", ""), nil)
			out := w.apply(w.tx(tc.kind, "25.00", ref.ExternalTransactionID()), ref, false)
			if out.Status != StatusProcessed || out.Entry == nil || out.Entry.Direction() != tc.wantDir || w.balance() != tc.wantAfter {
				t.Fatalf("%s %s entry=%+v balance %s", out.Status, out.FailureCode, out.Entry, w.balance())
			}
		})
	}

	t.Run("rollback of refund", func(t *testing.T) {
		w := newWorld(t, "100.00")
		bet := w.processed(w.tx("BET", "25.00", ""), nil)
		refund := w.processed(w.tx("REFUND", "25.00", bet.ExternalTransactionID()), bet)
		out := w.apply(w.tx("ROLLBACK", "25.00", refund.ExternalTransactionID()), refund, false)
		if out.Status != StatusProcessed || out.Entry.Direction() != wallet.Debit || w.balance() != "75.00" {
			t.Fatalf("%s balance %s", out.Status, w.balance())
		}
	})
}

func TestApplyReferenceRejections(t *testing.T) {
	type setup func(w *world) (tx, ref *WagerTransaction, reversed bool)
	otherWallet := func(w *world, ref *WagerTransaction) *WagerTransaction {
		s := ref.Snapshot()
		s.WalletID = uuid.New()
		r, err := Rehydrate(s)
		if err != nil {
			w.t.Fatal(err)
		}
		return r
	}
	withSnapshot := func(w *world, ref *WagerTransaction, mut func(*Snapshot)) *WagerTransaction {
		s := ref.Snapshot()
		mut(&s)
		r, err := Rehydrate(s)
		if err != nil {
			w.t.Fatal(err)
		}
		return r
	}
	cases := []struct {
		name  string
		setup setup
		code  FailureCode
	}{
		{"refund of rejected bet", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.tx("BET", "25.00", "")
			_ = bet.Reject(CodeInsufficientFunds, Result{Balance: w.wallet.Balance(), WalletVersion: 1}, t0)
			return w.tx("REFUND", "25.00", bet.ExternalTransactionID()), bet, false
		}, CodeReferenceNotProcessed},
		{"rollback of failed bet", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.tx("BET", "25.00", "")
			_ = bet.Fail(CodeInternalInvariantViolation, t0)
			return w.tx("ROLLBACK", "25.00", bet.ExternalTransactionID()), bet, false
		}, CodeReferenceNotProcessed},
		{"win of rejected bet", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.tx("BET", "25.00", "")
			_ = bet.Reject(CodeInsufficientFunds, Result{Balance: w.wallet.Balance(), WalletVersion: 1}, t0)
			return w.tx("WIN", "5.00", bet.ExternalTransactionID()), bet, false
		}, CodeReferenceNotProcessed},
		{"refund of win", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			win := w.processed(w.tx("WIN", "25.00", ""), nil)
			return w.tx("REFUND", "25.00", win.ExternalTransactionID()), win, false
		}, CodeReferenceKindNotAllowed},
		{"rollback of loss", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			loss := w.processed(w.tx("LOSS", "0.00", ""), nil)
			return w.tx("ROLLBACK", "25.00", loss.ExternalTransactionID()), loss, false
		}, CodeReferenceKindNotAllowed},
		{"rollback of rollback", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			rb := w.processed(w.tx("ROLLBACK", "25.00", bet.ExternalTransactionID()), bet)
			return w.tx("ROLLBACK", "25.00", rb.ExternalTransactionID()), rb, false
		}, CodeReferenceKindNotAllowed},
		{"win of win", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			win := w.processed(w.tx("WIN", "25.00", ""), nil)
			return w.tx("WIN", "5.00", win.ExternalTransactionID()), win, false
		}, CodeReferenceKindNotAllowed},
		{"reference on another wallet", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("REFUND", "25.00", bet.ExternalTransactionID()), otherWallet(w, bet), false
		}, CodeReferenceMismatch},
		{"reference in another round", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("REFUND", "25.00", bet.ExternalTransactionID()), withSnapshot(w, bet, func(s *Snapshot) { s.RoundID = strp("round-1") }), false
		}, CodeReferenceMismatch},
		{"reference of another provider", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("WIN", "5.00", bet.ExternalTransactionID()), withSnapshot(w, bet, func(s *Snapshot) { s.ProviderID = strp("provider-b") }), false
		}, CodeReferenceMismatch},
		{"reference of another player", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("REFUND", "25.00", bet.ExternalTransactionID()), withSnapshot(w, bet, func(s *Snapshot) { s.PlayerID = uuid.New() }), false
		}, CodeReferenceMismatch},
		{"reference in another currency", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("WIN", "5.00", bet.ExternalTransactionID()), withSnapshot(w, bet, func(s *Snapshot) {
				s.Money, _ = money.Parse("25.00", "USD")
				usd, _ := money.Parse("75.00", "USD")
				s.ResultBalance = &usd
			}), false
		}, CodeReferenceMismatch},
		{"partial refund", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("REFUND", "10.00", bet.ExternalTransactionID()), bet, false
		}, CodeReferenceAmountMismatch},
		{"rollback for more", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("ROLLBACK", "30.00", bet.ExternalTransactionID()), bet, false
		}, CodeReferenceAmountMismatch},
		{"refund of reversed bet", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("REFUND", "25.00", bet.ExternalTransactionID()), bet, true
		}, CodeAlreadyReversed},
		{"rollback of refunded bet", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("ROLLBACK", "25.00", bet.ExternalTransactionID()), bet, true
		}, CodeAlreadyReversed},
		// Precedence: status > kind > match > amount > reversed > funds.
		{"rejected win referenced by refund: status first", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			win := w.tx("WIN", "25.00", "")
			_ = win.Reject(CodeAmountOverflow, Result{Balance: w.wallet.Balance(), WalletVersion: 1}, t0)
			return w.tx("REFUND", "25.00", win.ExternalTransactionID()), win, true
		}, CodeReferenceNotProcessed},
		{"wrong kind and wrong round: kind first", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			win := w.processed(w.tx("WIN", "25.00", ""), nil)
			return w.tx("REFUND", "25.00", win.ExternalTransactionID()), withSnapshot(w, win, func(s *Snapshot) { s.RoundID = strp("round-1") }), false
		}, CodeReferenceKindNotAllowed},
		{"wrong amount and reversed: amount first", func(w *world) (*WagerTransaction, *WagerTransaction, bool) {
			bet := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.tx("REFUND", "10.00", bet.ExternalTransactionID()), bet, true
		}, CodeReferenceAmountMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, "100.00")
			tx, ref, reversed := tc.setup(w)
			before, version := w.balance(), w.wallet.Version()
			out := w.apply(tx, ref, reversed)
			expectRejected(t, out, tc.code)
			if w.balance() != before || w.wallet.Version() != version {
				t.Errorf("wallet changed: %s v%d", w.balance(), w.wallet.Version())
			}
			if id, ok := tx.ReferenceTransactionID(); !ok || id != ref.ID() {
				t.Errorf("found reference must be recorded for audit, got %s %v", id, ok)
			}
		})
	}
}

func TestReversalInsufficientFunds(t *testing.T) {
	w := newWorld(t, "100.00")
	win := w.processed(w.tx("WIN", "50.00", ""), nil) // 150.00
	w.processed(w.tx("BET", "140.00", ""), nil)       // 10.00
	out := w.apply(w.tx("ROLLBACK", "50.00", win.ExternalTransactionID()), win, false)
	expectRejected(t, out, CodeReversalInsufficientFunds)
	if CodeReversalInsufficientFunds == CodeInsufficientFunds {
		t.Fatal("reversal and bet shortfalls must have distinct codes")
	}
	if w.balance() != "10.00" {
		t.Errorf("balance %s", w.balance())
	}
}

func TestCreditOverflowIsRejected(t *testing.T) {
	max, _ := money.FromMinor(math.MaxInt64, money.BRL)
	w := newWorld(t, "0.00")
	ww, err := wallet.Rehydrate(wallet.Snapshot{ID: w.wallet.ID(), PlayerID: w.wallet.PlayerID(), Balance: max, Version: 9, CreatedAt: t0, UpdatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	w.wallet = ww
	expectRejected(t, w.apply(w.tx("WIN", "0.01", ""), nil, false), CodeAmountOverflow)
}

func TestPendingReferenceLifecycle(t *testing.T) {
	w := newWorld(t, "100.00")
	refund := w.tx("REFUND", "25.00", "bet-late")

	// First evaluation: the BET has not arrived.
	out := w.apply(refund, nil, false)
	if out.Status != StatusPendingReference || out.Entry != nil {
		t.Fatalf("first: %s", out.Status)
	}
	if types := eventTypes(out); len(types) != 1 || types[0] != event.TypeWagerTransactionPendingReference {
		t.Fatalf("first events = %v", types)
	}
	deadline, _ := refund.ReferenceDeadline()
	if !deadline.Equal(t0.Add(ttl)) {
		t.Errorf("deadline = %v, want created_at + TTL", deadline)
	}
	pr := out.Events[0].Data().(event.WagerTransactionPendingReferenceData)
	if pr.ReferenceExternalTransactionID != "bet-late" || pr.ReferenceDeadlineAt != "2026-09-08T12:10:00.000Z" {
		t.Errorf("pending reference data = %+v", pr)
	}

	// Worker retry, still missing: rescheduled silently.
	out = w.applyAt(refund, nil, false, t0.Add(time.Minute))
	if out.Status != StatusPendingReference || len(out.Events) != 0 || refund.Attempts() != 1 {
		t.Fatalf("retry: %s events=%d attempts=%d", out.Status, len(out.Events), refund.Attempts())
	}
	if !refund.NextAttemptAt().Equal(t0.Add(time.Minute + time.Second)) {
		t.Errorf("next attempt = %v", refund.NextAttemptAt())
	}

	// The BET arrives and is processed; the refund resolves.
	bet := w.tx("BET", "25.00", "")
	betSnap := bet.Snapshot()
	betSnap.ExternalTransactionID = strp("bet-late")
	betSnap.IdempotencyKey = strp("provider-a:bet-late")
	bet, _ = Rehydrate(betSnap)
	w.processed(bet, nil)
	out = w.applyAt(refund, bet, false, t0.Add(2*time.Minute))
	if out.Status != StatusProcessed || w.balance() != "100.00" {
		t.Fatalf("resolved: %s balance %s", out.Status, w.balance())
	}
}

func TestReferenceWaitsWhileReferencePending(t *testing.T) {
	w := newWorld(t, "100.00")
	// A ROLLBACK of a REFUND that is itself still waiting.
	refund := w.tx("REFUND", "25.00", "bet-x")
	w.apply(refund, nil, false)
	rollback := w.tx("ROLLBACK", "25.00", refund.ExternalTransactionID())
	if out := w.apply(rollback, refund, false); out.Status != StatusPendingReference {
		t.Fatalf("status %s, want PENDING_REFERENCE", out.Status)
	}
	if _, ok := rollback.ReferenceTransactionID(); ok {
		t.Error("an unresolved reference must not be recorded")
	}
}

func TestReferenceDeadline(t *testing.T) {
	expired := t0.Add(ttl)

	t.Run("never arrived", func(t *testing.T) {
		w := newWorld(t, "100.00")
		refund := w.tx("REFUND", "25.00", "bet-never")
		w.apply(refund, nil, false)
		expectRejected(t, w.applyAt(refund, nil, false, expired), CodeReferenceNotFound)
	})
	t.Run("still pending", func(t *testing.T) {
		w := newWorld(t, "100.00")
		refund := w.tx("REFUND", "25.00", "bet-x")
		w.apply(refund, nil, false)
		rollback := w.tx("ROLLBACK", "25.00", refund.ExternalTransactionID())
		w.apply(rollback, refund, false)
		expectRejected(t, w.applyAt(rollback, refund, false, expired), CodeReferenceUnresolvedTimeout)
	})
	t.Run("resolvable after deadline is applied", func(t *testing.T) {
		w := newWorld(t, "100.00")
		bet := w.processed(w.tx("BET", "25.00", ""), nil)
		refund := w.tx("REFUND", "25.00", bet.ExternalTransactionID())
		w.apply(refund, nil, false) // waits: lookup raced the commit
		out := w.applyAt(refund, bet, false, expired.Add(time.Hour))
		if out.Status != StatusProcessed {
			t.Fatalf("status %s, want PROCESSED", out.Status)
		}
	})
	t.Run("stale PENDING past its deadline", func(t *testing.T) {
		w := newWorld(t, "100.00")
		refund := w.tx("REFUND", "25.00", "bet-never")
		expectRejected(t, w.applyAt(refund, nil, false, expired), CodeReferenceNotFound)
	})
}

func TestApplyErrors(t *testing.T) {
	w := newWorld(t, "100.00")
	bet := w.tx("BET", "25.00", "")

	cases := map[string]struct {
		p    func() ApplyParams
		want error
	}{
		"nil transaction": {func() ApplyParams { p := w.params(bet, nil, false, t0); p.Transaction = nil; return p }, ErrInvalidTransaction},
		"nil wallet":      {func() ApplyParams { p := w.params(bet, nil, false, t0); p.Wallet = nil; return p }, ErrInvalidTransaction},
		"nil id source":   {func() ApplyParams { p := w.params(bet, nil, false, t0); p.NewID = nil; return p }, ErrInvalidTransaction},
		"zero time":       {func() ApplyParams { p := w.params(bet, nil, false, t0); p.Now = time.Time{}; return p }, ErrInvalidTransaction},
		"wrong wallet": {func() ApplyParams {
			other, _ := wallet.Rehydrate(wallet.Snapshot{ID: uuid.New(), PlayerID: uuid.MustParse(playerStr), Balance: brl(t, "1.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0})
			p := w.params(bet, nil, false, t0)
			p.Wallet = other
			return p
		}, ErrInvalidTransaction},
		"reference lookup mismatch": {func() ApplyParams {
			other := w.processed(w.tx("BET", "25.00", ""), nil)
			return w.params(w.tx("REFUND", "25.00", "not-that-one"), other, false, t0)
		}, ErrInvalidTransaction},
		"no ttl while waiting": {func() ApplyParams {
			p := w.params(w.tx("REFUND", "25.00", "bet-y"), nil, false, t0)
			p.ReferenceTTL = 0
			return p
		}, ErrInvalidTransaction},
		"no next attempt while waiting": {func() ApplyParams {
			p := w.params(w.tx("REFUND", "25.00", "bet-y"), nil, false, t0)
			p.NextAttemptAt = time.Time{}
			return p
		}, ErrInvalidTransaction},
	}
	for name, c := range cases {
		if _, err := Apply(c.p()); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if bet.Status() != StatusPending || w.wallet.Version() != 2 {
		// Version 2 comes from the processed BET in "reference lookup mismatch".
		t.Errorf("state changed: %s v%d", bet.Status(), w.wallet.Version())
	}
}

func TestApplyInputMismatches(t *testing.T) {
	w := newWorld(t, "100.00")
	bet := w.tx("BET", "25.00", "")

	stranger, _ := wallet.Rehydrate(wallet.Snapshot{ID: w.wallet.ID(), PlayerID: uuid.New(), Balance: brl(t, "100.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0})
	p := w.params(bet, nil, false, t0)
	p.Wallet = stranger
	if _, err := Apply(p); !hasCode(err, CodePlayerWalletMismatch) {
		t.Errorf("player mismatch: %v", err)
	}

	usd, _ := money.Parse("100.00", "USD")
	usdWallet, _ := wallet.Rehydrate(wallet.Snapshot{ID: w.wallet.ID(), PlayerID: w.wallet.PlayerID(), Balance: usd, Version: 1, CreatedAt: t0, UpdatedAt: t0})
	p.Wallet = usdWallet
	if _, err := Apply(p); !hasCode(err, CodeCurrencyMismatch) {
		t.Errorf("currency mismatch: %v", err)
	}
	// A LOSS still requires the wallet's currency.
	p = w.params(w.tx("LOSS", "0.00", ""), nil, false, t0)
	p.Wallet = usdWallet
	if _, err := Apply(p); !hasCode(err, CodeCurrencyMismatch) {
		t.Errorf("loss currency mismatch: %v", err)
	}
	if bet.Status() != StatusPending {
		t.Error("input errors must not change the transaction")
	}
}

func TestCausationPropagates(t *testing.T) {
	w := newWorld(t, "100.00")
	p := w.params(w.tx("BET", "25.00", ""), nil, false, t0)
	p.CausationID = strp("msg-123")
	out, err := Apply(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range out.Events {
		if c, ok := e.CausationID(); !ok || c != "msg-123" {
			t.Errorf("%s causation = %q, %v", e.Type(), c, ok)
		}
	}
}

func TestOpenWallet(t *testing.T) {
	ids := sequence()
	walletID, playerID := uuid.New(), uuid.New()
	res, err := OpenWallet(OpenWalletParams{
		WalletID: walletID, PlayerID: playerID, InitialBalance: brl(t, "1000.00"),
		CorrelationID: "corr-open", Now: t0, NewID: ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Wallet.Version() != 1 || res.Wallet.Balance().String() != "1000.00" {
		t.Errorf("wallet v%d %s", res.Wallet.Version(), res.Wallet.Balance())
	}
	op := res.Opening
	if op == nil || op.Origin() != OriginInternal || op.Kind() != KindOpening || op.Status() != StatusProcessed ||
		op.ProviderID() != "" || op.IdempotencyKey() != "" || op.WalletID() != walletID || op.PlayerID() != playerID {
		t.Fatalf("opening = %+v", op)
	}
	if r, _ := op.Result(); r.Balance.String() != "1000.00" || r.WalletVersion != 1 {
		t.Errorf("opening result %+v", r)
	}
	if _, err := Rehydrate(op.Snapshot()); err != nil {
		t.Errorf("opening snapshot must satisfy the schema rules: %v", err)
	}
	if res.Entry == nil || res.Entry.TransactionID() != op.ID() || res.Entry.WalletVersion() != 1 || res.Entry.BalanceBefore().String() != "0.00" {
		t.Fatalf("entry = %+v", res.Entry)
	}
	types := []event.Type{res.Events[0].Type(), res.Events[1].Type()}
	if len(res.Events) != 2 || types[0] != event.TypeWagerTransactionProcessed || types[1] != event.TypeWalletBalanceChanged {
		t.Fatalf("events = %v", types)
	}
	// Internal events omit the external metadata.
	var payload map[string]any
	if err := json.Unmarshal(res.Events[0].Payload(), &payload); err != nil {
		t.Fatal(err)
	}
	data := payload["data"].(map[string]any)
	for _, k := range []string{"providerId", "externalTransactionId", "roundId", "gameId"} {
		if _, ok := data[k]; ok {
			t.Errorf("internal event carries %s", k)
		}
	}
	if data["origin"] != "INTERNAL" || data["kind"] != "OPENING" || data["walletVersion"] != float64(1) {
		t.Errorf("data = %v", data)
	}
}

func TestOpenWalletZeroBalance(t *testing.T) {
	res, err := OpenWallet(OpenWalletParams{
		WalletID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"),
		CorrelationID: "c", Now: t0, NewID: sequence(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Opening != nil || res.Entry != nil || len(res.Events) != 0 || res.Wallet.Version() != 1 {
		t.Errorf("zero opening produced %+v", res)
	}
}

func TestOpenWalletRejects(t *testing.T) {
	neg, _ := money.FromMinor(-1, money.BRL)
	base := OpenWalletParams{WalletID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "1.00"), CorrelationID: "c", Now: t0, NewID: sequence()}

	p := base
	p.InitialBalance = neg
	if _, err := OpenWallet(p); !hasCode(err, CodeInvalidMoney) {
		t.Errorf("negative: %v", err)
	}
	p = base
	p.InitialBalance = money.Money{}
	if _, err := OpenWallet(p); !hasCode(err, CodeInvalidMoney) {
		t.Errorf("missing: %v", err)
	}
	p = base
	p.NewID = nil
	if _, err := OpenWallet(p); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("nil id source: %v", err)
	}
	p = base
	p.WalletID = uuid.Nil
	if _, err := OpenWallet(p); !errors.Is(err, wallet.ErrInvalidWallet) {
		t.Errorf("nil wallet id: %v", err)
	}
}
