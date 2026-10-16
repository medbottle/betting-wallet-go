package wager

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/money"
)

func pendingTx(t *testing.T, kind string) *WagerTransaction {
	t.Helper()
	amount := "25.00"
	if kind == "LOSS" {
		amount = "0.00"
	}
	tx, err := NewExternal(mustRequest(t, raw(kind, amount)), uuid.New(), "corr-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func result(t *testing.T) Result { return Result{Balance: brl(t, "975.00"), WalletVersion: 2} }

func TestNewExternal(t *testing.T) {
	req := mustRequest(t, raw("REFUND", "25.00"))
	id := uuid.New()
	tx, err := NewExternal(req, id, "corr-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := tx.ReferenceExternalTransactionID()
	if tx.ID() != id || tx.Status() != StatusPending || tx.Origin() != OriginExternal || tx.Kind() != KindRefund ||
		tx.PayloadHash() != req.PayloadHash() || tx.HashVersion() != HashVersion || ref != "bet-1" ||
		!tx.NextAttemptAt().Equal(t0) || tx.Attempts() != 0 {
		t.Errorf("unexpected transaction %+v", tx.Snapshot())
	}
	if _, ok := tx.Result(); ok {
		t.Error("pending transaction has a result")
	}

	if _, err := NewExternal(Request{}, id, "c", t0); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("zero request: %v", err)
	}
	if _, err := NewExternal(req, uuid.Nil, "c", t0); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("nil id: %v", err)
	}
	if _, err := NewExternal(req, id, "", t0); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("empty correlation: %v", err)
	}
	if _, err := NewExternal(req, id, "c", time.Time{}); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("zero time: %v", err)
	}
}

// TestStateMachine checks every transition from every state.
func TestStateMachine(t *testing.T) {
	type transition struct {
		name string
		do   func(tx *WagerTransaction) error
	}
	later := t0.Add(time.Minute)
	transitions := []transition{
		{"MarkProcessed", func(tx *WagerTransaction) error { return tx.MarkProcessed(result(t), later) }},
		{"Reject", func(tx *WagerTransaction) error { return tx.Reject(CodeInsufficientFunds, result(t), later) }},
		{"Fail", func(tx *WagerTransaction) error { return tx.Fail(CodeInternalInvariantViolation, later) }},
		{"AwaitReference", func(tx *WagerTransaction) error { return tx.AwaitReference(later.Add(time.Hour), later, later) }},
		{"Reschedule", func(tx *WagerTransaction) error { return tx.Reschedule(later, later) }},
	}
	// allowed[from][transition]
	allowed := map[Status]map[string]Status{
		StatusPending: {
			"MarkProcessed": StatusProcessed, "Reject": StatusRejected, "Fail": StatusFailed, "AwaitReference": StatusPendingReference,
		},
		StatusPendingReference: {
			"MarkProcessed": StatusProcessed, "Reject": StatusRejected, "Fail": StatusFailed, "Reschedule": StatusPendingReference,
		},
		StatusProcessed: {},
		StatusRejected:  {},
		StatusFailed:    {},
	}
	reach := map[Status]func() *WagerTransaction{
		StatusPending: func() *WagerTransaction { return pendingTx(t, "REFUND") },
		StatusPendingReference: func() *WagerTransaction {
			tx := pendingTx(t, "REFUND")
			if err := tx.AwaitReference(t0.Add(time.Hour), t0, t0); err != nil {
				t.Fatal(err)
			}
			return tx
		},
		StatusProcessed: func() *WagerTransaction {
			tx := pendingTx(t, "REFUND")
			_ = tx.MarkProcessed(result(t), t0)
			return tx
		},
		StatusRejected: func() *WagerTransaction {
			tx := pendingTx(t, "REFUND")
			_ = tx.Reject(CodeAlreadyReversed, result(t), t0)
			return tx
		},
		StatusFailed: func() *WagerTransaction {
			tx := pendingTx(t, "REFUND")
			_ = tx.Fail(CodeInternalInvariantViolation, t0)
			return tx
		},
	}
	for from, mk := range reach {
		for _, tr := range transitions {
			tx := mk()
			if tx.Status() != from {
				t.Fatalf("setup reached %s, want %s", tx.Status(), from)
			}
			before := tx.Snapshot()
			err := tr.do(tx)
			want, ok := allowed[from][tr.name]
			if ok {
				if err != nil || tx.Status() != want {
					t.Errorf("%s from %s: err=%v status=%s, want %s", tr.name, from, err, tx.Status(), want)
				}
				continue
			}
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s from %s: err = %v, want ErrInvalidTransition", tr.name, from, err)
			}
			if after := tx.Snapshot(); !snapshotsEqual(before, after) {
				t.Errorf("%s from %s changed the transaction", tr.name, from)
			}
		}
	}
}

func snapshotsEqual(a, b Snapshot) bool {
	return a.Status == b.Status && a.Attempts == b.Attempts && a.UpdatedAt.Equal(b.UpdatedAt) &&
		(a.CompletedAt == nil) == (b.CompletedAt == nil) && (a.FailureCode == nil) == (b.FailureCode == nil)
}

func TestTransitionEffects(t *testing.T) {
	tx := pendingTx(t, "REFUND")
	deadline, next := t0.Add(10*time.Minute), t0.Add(time.Second)
	if err := tx.AwaitReference(deadline, next, t0); err != nil {
		t.Fatal(err)
	}
	if d, ok := tx.ReferenceDeadline(); !ok || !d.Equal(deadline) || !tx.NextAttemptAt().Equal(next) {
		t.Errorf("deadline=%v next=%v", d, tx.NextAttemptAt())
	}
	if err := tx.Reschedule(t0.Add(3*time.Second), t0.Add(time.Second)); err != nil || tx.Attempts() != 1 {
		t.Errorf("reschedule: %v attempts=%d", err, tx.Attempts())
	}
	done := t0.Add(5 * time.Second)
	if err := tx.Reject(CodeReferenceNotFound, result(t), done); err != nil {
		t.Fatal(err)
	}
	if c, ok := tx.CompletedAt(); !ok || !c.Equal(done) || tx.FailureCode() != CodeReferenceNotFound {
		t.Errorf("completed=%v code=%s", c, tx.FailureCode())
	}
	if r, ok := tx.Result(); !ok || r.Balance.String() != "975.00" || r.WalletVersion != 2 {
		t.Errorf("result = %+v", r)
	}
}

func TestTransitionGuards(t *testing.T) {
	usd, _ := money.Parse("1.00", "USD")
	neg, _ := money.FromMinor(-1, money.BRL)
	cases := map[string]func(tx *WagerTransaction) error{
		"reject with category A":    func(tx *WagerTransaction) error { return tx.Reject(CodeInvalidMoney, result(t), t0) },
		"reject with category D":    func(tx *WagerTransaction) error { return tx.Reject(CodeInternalInvariantViolation, result(t), t0) },
		"reject with unknown code":  func(tx *WagerTransaction) error { return tx.Reject("NOPE", result(t), t0) },
		"fail with category B":      func(tx *WagerTransaction) error { return tx.Fail(CodeInsufficientFunds, t0) },
		"result in other currency":  func(tx *WagerTransaction) error { return tx.MarkProcessed(Result{Balance: usd, WalletVersion: 2}, t0) },
		"negative result":           func(tx *WagerTransaction) error { return tx.MarkProcessed(Result{Balance: neg, WalletVersion: 2}, t0) },
		"result version 0":          func(tx *WagerTransaction) error { return tx.MarkProcessed(Result{Balance: brl(t, "1.00")}, t0) },
		"zero time":                 func(tx *WagerTransaction) error { return tx.MarkProcessed(result(t), time.Time{}) },
		"await without deadline":    func(tx *WagerTransaction) error { return tx.AwaitReference(time.Time{}, t0, t0) },
		"await without next":        func(tx *WagerTransaction) error { return tx.AwaitReference(t0, time.Time{}, t0) },
		"uninitialised transaction": func(*WagerTransaction) error { return (&WagerTransaction{}).MarkProcessed(result(t), t0) },
	}
	for name, do := range cases {
		tx := pendingTx(t, "REFUND")
		if err := do(tx); !errors.Is(err, ErrInvalidTransaction) {
			t.Errorf("%s: err = %v, want ErrInvalidTransaction", name, err)
		}
		if name != "uninitialised transaction" && tx.Status() != StatusPending {
			t.Errorf("%s: status changed to %s", name, tx.Status())
		}
	}
	// A BET has no reference to wait for.
	if err := pendingTx(t, "BET").AwaitReference(t0.Add(time.Hour), t0, t0); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("await on BET: %v", err)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	tx := pendingTx(t, "REFUND")
	if err := tx.AwaitReference(t0.Add(time.Hour), t0.Add(time.Second), t0); err != nil {
		t.Fatal(err)
	}
	if err := tx.resolveReference(uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkProcessed(result(t), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	back, err := Rehydrate(tx.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	a, b := tx.Snapshot(), back.Snapshot()
	if a.ID != b.ID || a.Status != b.Status || *a.PayloadHash != *b.PayloadHash || *a.ReferenceTransactionID != *b.ReferenceTransactionID ||
		!a.ResultBalance.Equal(*b.ResultBalance) || !a.CompletedAt.Equal(*b.CompletedAt) || *a.IdempotencyKey != *b.IdempotencyKey {
		t.Errorf("round trip mismatch:\n%+v\n%+v", a, b)
	}
	// Rehydrated terminal transactions stay frozen.
	if err := back.Reschedule(t0, t0); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("rehydrated terminal accepted a transition: %v", err)
	}
}

func TestRehydrateRejects(t *testing.T) {
	good := func() Snapshot {
		tx := pendingTx(t, "BET")
		_ = tx.MarkProcessed(result(t), t0)
		return tx.Snapshot()
	}
	code := func(c FailureCode) *FailureCode { return &c }
	usd, _ := money.Parse("1.00", "USD")
	cases := map[string]func(s *Snapshot){
		"nil id":                 func(s *Snapshot) { s.ID = uuid.Nil },
		"invalid status":         func(s *Snapshot) { s.Status = "DONE" },
		"external opening":       func(s *Snapshot) { s.Kind = KindOpening },
		"internal with metadata": func(s *Snapshot) { s.Origin = OriginInternal; s.Kind = KindOpening },
		"external without key":   func(s *Snapshot) { s.IdempotencyKey = nil },
		"bad key":                func(s *Snapshot) { s.IdempotencyKey = strp("a\tb") },
		"loss with amount":       func(s *Snapshot) { s.Kind = KindLoss },
		"bet with reference":     func(s *Snapshot) { s.ReferenceExternalTransactionID = strp("x") },
		"processed with code":    func(s *Snapshot) { s.FailureCode = code(CodeInsufficientFunds) },
		"rejected with A code":   func(s *Snapshot) { s.Status = StatusRejected; s.FailureCode = code(CodeInvalidMoney) },
		"failed with B code": func(s *Snapshot) {
			s.Status = StatusFailed
			s.FailureCode = code(CodeInsufficientFunds)
			s.ResultBalance = nil
			s.ResultWalletVersion = nil
		},
		"processed without result": func(s *Snapshot) { s.ResultBalance = nil },
		"result other currency":    func(s *Snapshot) { s.ResultBalance = &usd },
		"terminal without done":    func(s *Snapshot) { s.CompletedAt = nil },
		"pending with result":      func(s *Snapshot) { s.Status = StatusPending; s.CompletedAt = nil },
		"pending ref no deadline": func(s *Snapshot) {
			s.Status = StatusPendingReference
			s.CompletedAt = nil
			s.ResultBalance = nil
			s.ResultWalletVersion = nil
		},
		"no correlation":    func(s *Snapshot) { s.CorrelationID = "" },
		"negative attempts": func(s *Snapshot) { s.Attempts = -1 },
	}
	for name, mut := range cases {
		s := good()
		mut(&s)
		if _, err := Rehydrate(s); !errors.Is(err, ErrInvalidTransaction) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestFailureCodes(t *testing.T) {
	if len(CodesIn(CategoryInput)) != 11 || len(CodesIn(CategoryRejection)) != 10 || len(CodesIn(CategoryConflict)) != 3 ||
		len(CodesIn(CategoryPermanent)) != 1 || len(CodesIn(CategoryTransient)) != 1 {
		t.Error("category sizes differ from ARCHITECTURE §12")
	}
	if FailureCode("NOPE").Valid() || !CodeAlreadyReversed.IsRejection() || CodeInvalidMoney.IsRejection() {
		t.Error("classification")
	}
}
