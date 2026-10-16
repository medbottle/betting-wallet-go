package wager

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/money"
)

// WagerTransaction is a financial operation and its lifecycle. State is
// encapsulated; it changes only through the transition methods, which
// enforce the state machine of ARCHITECTURE §5.
type WagerTransaction struct {
	id       uuid.UUID
	origin   Origin
	kind     Kind
	status   Status
	walletID uuid.UUID
	playerID uuid.UUID
	money    money.Money

	// External metadata; zero for INTERNAL.
	providerID            string
	externalTransactionID string
	idempotencyKey        string
	payloadHash           PayloadHash
	hashVersion           int16
	roundID               string
	gameID                string
	referenceExternalID   *string
	referenceID           *uuid.UUID

	failureCode   FailureCode
	result        *Result
	attempts      int
	nextAttemptAt time.Time
	deadline      *time.Time

	correlationID string
	createdAt     time.Time
	updatedAt     time.Time
	completedAt   *time.Time
}

// Result is the financial outcome observed when the operation completed,
// returned unchanged on every replay.
type Result struct {
	Balance       money.Money
	WalletVersion int64
}

func (r Result) validFor(c money.Currency) error {
	if !r.Balance.Valid() || r.Balance.IsNegative() || r.Balance.Currency() != c {
		return invalidTx("result balance must be a non-negative %s amount", c)
	}
	if r.WalletVersion < 1 {
		return invalidTx("result wallet version must be >= 1")
	}
	return nil
}

// NewExternal creates a PENDING transaction from a validated request.
func NewExternal(req Request, id uuid.UUID, correlationID string, now time.Time) (*WagerTransaction, error) {
	if !req.valid() {
		return nil, invalidTx("request was not built by NewRequest")
	}
	if id == uuid.Nil || correlationID == "" || now.IsZero() {
		return nil, invalidTx("id, correlation id and time must be set")
	}
	now = now.UTC()
	return &WagerTransaction{
		id:                    id,
		origin:                OriginExternal,
		kind:                  req.kind,
		status:                StatusPending,
		walletID:              req.walletID,
		playerID:              req.playerID,
		money:                 req.money,
		providerID:            req.providerID,
		externalTransactionID: req.externalTransactionID,
		idempotencyKey:        req.idempotencyKey,
		payloadHash:           req.payloadHash(),
		hashVersion:           HashVersion,
		roundID:               req.roundID,
		gameID:                req.gameID,
		referenceExternalID:   req.referenceExternalID,
		nextAttemptAt:         now,
		correlationID:         correlationID,
		createdAt:             now,
		updatedAt:             now,
	}, nil
}

// newOpening creates the internal OPENING credit. It starts PENDING like
// every transaction and is completed by OpenWallet in memory, so it is
// persisted as PROCESSED.
func newOpening(id, walletID, playerID uuid.UUID, amount money.Money, correlationID string, now time.Time) (*WagerTransaction, error) {
	if id == uuid.Nil || walletID == uuid.Nil || playerID == uuid.Nil || correlationID == "" || now.IsZero() {
		return nil, invalidTx("opening needs ids, correlation id and time")
	}
	if !amount.IsPositive() {
		return nil, invalidTx("opening amount must be positive")
	}
	now = now.UTC()
	return &WagerTransaction{
		id:            id,
		origin:        OriginInternal,
		kind:          KindOpening,
		status:        StatusPending,
		walletID:      walletID,
		playerID:      playerID,
		money:         amount,
		nextAttemptAt: now,
		correlationID: correlationID,
		createdAt:     now,
		updatedAt:     now,
	}, nil
}

// MarkProcessed completes the operation successfully.
func (t *WagerTransaction) MarkProcessed(res Result, now time.Time) error {
	if err := t.transitionFrom(now, StatusPending, StatusPendingReference); err != nil {
		return err
	}
	if err := res.validFor(t.money.Currency()); err != nil {
		return err
	}
	t.complete(StatusProcessed, "", &res, now)
	return nil
}

// Reject completes the operation with a category B business rejection and
// the wallet state observed at that moment.
func (t *WagerTransaction) Reject(code FailureCode, res Result, now time.Time) error {
	if err := t.transitionFrom(now, StatusPending, StatusPendingReference); err != nil {
		return err
	}
	if !code.IsRejection() {
		return invalidTx("%q is not a business rejection code", code)
	}
	if err := res.validFor(t.money.Currency()); err != nil {
		return err
	}
	t.complete(StatusRejected, code, &res, now)
	return nil
}

// Fail records a permanent failure (category D) for audit.
func (t *WagerTransaction) Fail(code FailureCode, now time.Time) error {
	if err := t.transitionFrom(now, StatusPending, StatusPendingReference); err != nil {
		return err
	}
	if code.Category() != CategoryPermanent {
		return invalidTx("%q is not a permanent failure code", code)
	}
	t.complete(StatusFailed, code, nil, now)
	return nil
}

// AwaitReference parks a PENDING operation until its reference can be
// resolved, at the latest until deadline.
func (t *WagerTransaction) AwaitReference(deadline, nextAttemptAt, now time.Time) error {
	if err := t.transitionFrom(now, StatusPending); err != nil {
		return err
	}
	if t.referenceExternalID == nil {
		return invalidTx("only operations with a reference can wait for it")
	}
	if deadline.IsZero() || nextAttemptAt.IsZero() {
		return invalidTx("deadline and next attempt must be set")
	}
	d := deadline.UTC()
	t.status = StatusPendingReference
	t.deadline = &d
	t.nextAttemptAt = nextAttemptAt.UTC()
	t.updatedAt = now.UTC()
	return nil
}

// Reschedule records another unsuccessful resolution attempt.
func (t *WagerTransaction) Reschedule(nextAttemptAt, now time.Time) error {
	if err := t.transitionFrom(now, StatusPendingReference); err != nil {
		return err
	}
	if nextAttemptAt.IsZero() {
		return invalidTx("next attempt must be set")
	}
	t.attempts++
	t.nextAttemptAt = nextAttemptAt.UTC()
	t.updatedAt = now.UTC()
	return nil
}

// resolveReference records the internal id of the referenced operation.
// Once set it never changes.
func (t *WagerTransaction) resolveReference(id uuid.UUID) error {
	if t.referenceExternalID == nil || id == uuid.Nil || id == t.id {
		return invalidTx("cannot resolve reference %s", id)
	}
	if t.referenceID != nil && *t.referenceID != id {
		return invalidTx("reference already resolved to %s", *t.referenceID)
	}
	t.referenceID = &id
	return nil
}

func (t *WagerTransaction) transitionFrom(now time.Time, from ...Status) error {
	if t == nil || t.id == uuid.Nil {
		return invalidTx("uninitialised transaction")
	}
	if now.IsZero() {
		return invalidTx("time must be set")
	}
	for _, s := range from {
		if t.status == s {
			return nil
		}
	}
	return fmt.Errorf("%w: from %s", ErrInvalidTransition, t.status)
}

func (t *WagerTransaction) complete(s Status, code FailureCode, res *Result, now time.Time) {
	now = now.UTC()
	t.status = s
	t.failureCode = code
	t.result = res
	t.updatedAt = now
	t.completedAt = &now
}

// Getters.

func (t *WagerTransaction) ID() uuid.UUID                 { return t.id }
func (t *WagerTransaction) Origin() Origin                { return t.origin }
func (t *WagerTransaction) Kind() Kind                    { return t.kind }
func (t *WagerTransaction) Status() Status                { return t.status }
func (t *WagerTransaction) WalletID() uuid.UUID           { return t.walletID }
func (t *WagerTransaction) PlayerID() uuid.UUID           { return t.playerID }
func (t *WagerTransaction) Money() money.Money            { return t.money }
func (t *WagerTransaction) ProviderID() string            { return t.providerID }
func (t *WagerTransaction) ExternalTransactionID() string { return t.externalTransactionID }
func (t *WagerTransaction) IdempotencyKey() string        { return t.idempotencyKey }
func (t *WagerTransaction) PayloadHash() PayloadHash      { return t.payloadHash }
func (t *WagerTransaction) HashVersion() int16            { return t.hashVersion }
func (t *WagerTransaction) RoundID() string               { return t.roundID }
func (t *WagerTransaction) GameID() string                { return t.gameID }
func (t *WagerTransaction) FailureCode() FailureCode      { return t.failureCode }
func (t *WagerTransaction) Attempts() int                 { return t.attempts }
func (t *WagerTransaction) NextAttemptAt() time.Time      { return t.nextAttemptAt }
func (t *WagerTransaction) CorrelationID() string         { return t.correlationID }
func (t *WagerTransaction) CreatedAt() time.Time          { return t.createdAt }
func (t *WagerTransaction) UpdatedAt() time.Time          { return t.updatedAt }

// ReferenceExternalTransactionID returns the external reference, if any.
func (t *WagerTransaction) ReferenceExternalTransactionID() (string, bool) {
	if t.referenceExternalID == nil {
		return "", false
	}
	return *t.referenceExternalID, true
}

// ReferenceTransactionID returns the resolved internal reference, if any.
func (t *WagerTransaction) ReferenceTransactionID() (uuid.UUID, bool) {
	if t.referenceID == nil {
		return uuid.Nil, false
	}
	return *t.referenceID, true
}

// Result returns the persisted outcome of a PROCESSED or REJECTED
// operation.
func (t *WagerTransaction) Result() (Result, bool) {
	if t.result == nil {
		return Result{}, false
	}
	return *t.result, true
}

// ReferenceDeadline returns the deadline of a PENDING_REFERENCE operation.
func (t *WagerTransaction) ReferenceDeadline() (time.Time, bool) {
	if t.deadline == nil {
		return time.Time{}, false
	}
	return *t.deadline, true
}

// CompletedAt returns when a terminal operation completed.
func (t *WagerTransaction) CompletedAt() (time.Time, bool) {
	if t.completedAt == nil {
		return time.Time{}, false
	}
	return *t.completedAt, true
}

// Snapshot is the persisted state of a transaction, field for field with
// the wager_transactions table. Optional columns are pointers.
type Snapshot struct {
	ID                             uuid.UUID
	Origin                         Origin
	Kind                           Kind
	Status                         Status
	WalletID                       uuid.UUID
	PlayerID                       uuid.UUID
	Money                          money.Money
	ProviderID                     *string
	ExternalTransactionID          *string
	IdempotencyKey                 *string
	PayloadHash                    *PayloadHash
	HashVersion                    *int16
	RoundID                        *string
	GameID                         *string
	ReferenceExternalTransactionID *string
	ReferenceTransactionID         *uuid.UUID
	FailureCode                    *FailureCode
	ResultBalance                  *money.Money
	ResultWalletVersion            *int64
	Attempts                       int
	NextAttemptAt                  time.Time
	ReferenceDeadlineAt            *time.Time
	CorrelationID                  string
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
	CompletedAt                    *time.Time
}

// Snapshot returns the state for persistence.
func (t *WagerTransaction) Snapshot() Snapshot {
	s := Snapshot{
		ID:                             t.id,
		Origin:                         t.origin,
		Kind:                           t.kind,
		Status:                         t.status,
		WalletID:                       t.walletID,
		PlayerID:                       t.playerID,
		Money:                          t.money,
		ReferenceExternalTransactionID: copyPtr(t.referenceExternalID),
		ReferenceTransactionID:         copyPtr(t.referenceID),
		Attempts:                       t.attempts,
		NextAttemptAt:                  t.nextAttemptAt,
		ReferenceDeadlineAt:            copyPtr(t.deadline),
		CorrelationID:                  t.correlationID,
		CreatedAt:                      t.createdAt,
		UpdatedAt:                      t.updatedAt,
		CompletedAt:                    copyPtr(t.completedAt),
	}
	if t.origin == OriginExternal {
		hash, version := t.payloadHash, t.hashVersion
		s.ProviderID = ptr(t.providerID)
		s.ExternalTransactionID = ptr(t.externalTransactionID)
		s.IdempotencyKey = ptr(t.idempotencyKey)
		s.PayloadHash = &hash
		s.HashVersion = &version
		s.RoundID = ptr(t.roundID)
		s.GameID = ptr(t.gameID)
	}
	if t.failureCode != "" {
		s.FailureCode = ptr(t.failureCode)
	}
	if t.result != nil {
		s.ResultBalance = ptr(t.result.Balance)
		s.ResultWalletVersion = ptr(t.result.WalletVersion)
	}
	return s
}

// Rehydrate restores a persisted transaction after checking the same rules
// the schema enforces (ARCHITECTURE §16). It performs no transition and
// produces no events.
func Rehydrate(s Snapshot) (*WagerTransaction, error) {
	if err := validateSnapshot(s); err != nil {
		return nil, err
	}
	t := &WagerTransaction{
		id:            s.ID,
		origin:        s.Origin,
		kind:          s.Kind,
		status:        s.Status,
		walletID:      s.WalletID,
		playerID:      s.PlayerID,
		money:         s.Money,
		attempts:      s.Attempts,
		nextAttemptAt: s.NextAttemptAt.UTC(),
		correlationID: s.CorrelationID,
		createdAt:     s.CreatedAt.UTC(),
		updatedAt:     s.UpdatedAt.UTC(),

		referenceExternalID: copyPtr(s.ReferenceExternalTransactionID),
		referenceID:         copyPtr(s.ReferenceTransactionID),
		deadline:            utcPtr(s.ReferenceDeadlineAt),
		completedAt:         utcPtr(s.CompletedAt),
	}
	if s.Origin == OriginExternal {
		t.providerID = *s.ProviderID
		t.externalTransactionID = *s.ExternalTransactionID
		t.idempotencyKey = *s.IdempotencyKey
		t.payloadHash = *s.PayloadHash
		t.hashVersion = *s.HashVersion
		t.roundID = *s.RoundID
		t.gameID = *s.GameID
	}
	if s.FailureCode != nil {
		t.failureCode = *s.FailureCode
	}
	if s.ResultBalance != nil {
		t.result = &Result{Balance: *s.ResultBalance, WalletVersion: *s.ResultWalletVersion}
	}
	return t, nil
}

func validateSnapshot(s Snapshot) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if s.ID == uuid.Nil || s.WalletID == uuid.Nil || s.PlayerID == uuid.Nil {
		add("ids must be set")
	}
	if !s.Origin.Valid() || !s.Kind.Valid() || !s.Status.Valid() {
		add("origin %q, kind %q or status %q is invalid", s.Origin, s.Kind, s.Status)
	}
	if !s.Money.Valid() || s.Money.IsNegative() {
		add("amount must be valid and non-negative")
	} else if (s.Kind == KindLoss) != s.Money.IsZero() {
		add("LOSS must be zero and other kinds positive")
	}

	external := []any{s.ProviderID, s.ExternalTransactionID, s.IdempotencyKey, s.PayloadHash, s.HashVersion, s.RoundID, s.GameID}
	switch s.Origin {
	case OriginInternal:
		if s.Kind != KindOpening || s.Status != StatusProcessed {
			add("INTERNAL rows are PROCESSED OPENINGs")
		}
		for _, f := range external {
			if !isNilPtr(f) {
				add("INTERNAL rows carry no external metadata")
				break
			}
		}
		if s.ReferenceExternalTransactionID != nil || s.ReferenceTransactionID != nil {
			add("INTERNAL rows carry no reference")
		}
	case OriginExternal:
		if !s.Kind.External() {
			add("EXTERNAL rows cannot be OPENING")
		}
		for _, f := range external {
			if isNilPtr(f) {
				add("EXTERNAL rows carry all external metadata")
				break
			}
		}
		if s.IdempotencyKey != nil && !isIdempotencyKey(*s.IdempotencyKey) {
			add("idempotency key format")
		}
		if s.HashVersion != nil && *s.HashVersion < 1 {
			add("hash version must be >= 1")
		}
	}

	if s.Kind.RequiresReference() && s.ReferenceExternalTransactionID == nil {
		add("%s requires a reference", s.Kind)
	}
	if !s.Kind.AllowsReference() && (s.ReferenceExternalTransactionID != nil || s.ReferenceTransactionID != nil) {
		add("%s cannot carry a reference", s.Kind)
	}
	if s.ReferenceTransactionID != nil && (s.ReferenceExternalTransactionID == nil || *s.ReferenceTransactionID == s.ID) {
		add("resolved reference needs an external reference and cannot be itself")
	}

	switch s.Status {
	case StatusRejected:
		if s.FailureCode == nil || !s.FailureCode.IsRejection() {
			add("REJECTED needs a category B failure code")
		}
	case StatusFailed:
		if s.FailureCode == nil || s.FailureCode.Category() != CategoryPermanent {
			add("FAILED needs a category D failure code")
		}
	default:
		if s.FailureCode != nil {
			add("%s cannot carry a failure code", s.Status)
		}
	}

	hasResult := s.ResultBalance != nil || s.ResultWalletVersion != nil
	switch s.Status {
	case StatusProcessed, StatusRejected:
		if s.ResultBalance == nil || s.ResultWalletVersion == nil {
			add("%s needs a result", s.Status)
		} else if err := (Result{Balance: *s.ResultBalance, WalletVersion: *s.ResultWalletVersion}).validFor(s.Money.Currency()); err != nil {
			errs = append(errs, err)
		}
	case StatusPending, StatusPendingReference:
		if hasResult {
			add("%s cannot carry a result", s.Status)
		}
	}
	if (s.CompletedAt != nil) != s.Status.Terminal() {
		add("completed_at is set exactly for terminal statuses")
	}
	if s.Status == StatusPendingReference && s.ReferenceDeadlineAt == nil {
		add("PENDING_REFERENCE needs a deadline")
	}
	if s.Attempts < 0 {
		add("attempts must be >= 0")
	}
	if s.CorrelationID == "" || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.NextAttemptAt.IsZero() {
		add("correlation id and timestamps must be set")
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTransaction, err)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func copyPtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func utcPtr(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	v := p.UTC()
	return &v
}

func isNilPtr(v any) bool {
	switch p := v.(type) {
	case *string:
		return p == nil
	case *PayloadHash:
		return p == nil
	case *int16:
		return p == nil
	}
	return v == nil
}
