// Package wagering holds SubmitWagerTransaction, the use case HTTP and SQS
// share (ARCHITECTURE §1/§4), and the transaction queries. Permission
// checks belong to the transport, which runs them before calling in.
package wagering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/app"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// Config holds the reference-resolution settings (ARCHITECTURE §8).
type Config struct {
	ReferenceTTL time.Duration
	Backoff      Backoff
}

// Service implements the wagering use cases.
type Service struct {
	tx           app.TxRunner
	wallets      app.WalletReader
	providers    app.ProviderReader
	transactions app.TransactionReader
	clock        app.Clock
	ids          app.IDGenerator
	cfg          Config
	logger       *slog.Logger
	metrics      app.Metrics
}

// NewService wires the use cases to their ports. A nil metrics discards
// the measurements.
func NewService(tx app.TxRunner, wallets app.WalletReader, providers app.ProviderReader, transactions app.TransactionReader,
	clock app.Clock, ids app.IDGenerator, cfg Config, logger *slog.Logger, metrics app.Metrics) *Service {
	return &Service{tx: tx, wallets: wallets, providers: providers, transactions: transactions, clock: clock, ids: ids, cfg: cfg,
		logger: logger, metrics: app.OrNop(metrics)}
}

// SubmitCommand is one external operation as received by a transport.
type SubmitCommand struct {
	Raw           wager.RawRequest
	CorrelationID string
	// CausationID is the SQS messageId for consumer submissions.
	CausationID *string
	// Inbox, set by the SQS consumer, records the message in the inbox in
	// the same transaction (ARCHITECTURE §9). Nil for HTTP.
	Inbox *InboxEntry
}

// InboxEntry identifies one consumed message.
type InboxEntry struct {
	ConsumerName string
	MessageID    string
	ReceivedAt   time.Time
}

// SubmitResult is the persisted transaction: the outcome of this call, or
// of the original call on a replay.
type SubmitResult struct {
	Transaction *wager.WagerTransaction
	Replay      bool
	// Duplicate reports a redelivered inbox message that was already
	// handled: nothing was done (ARCHITECTURE §9).
	Duplicate bool
}

// Submit runs the synchronous flow of ARCHITECTURE §4:
//
//	validate -> provider registry -> wallet pre-read (no transaction)
//	BEGIN
//	  insert inbox row (SQS only; a redelivery is a duplicate, §9)
//	  insert own row (ON CONFLICT DO NOTHING; a duplicate replays or conflicts, §6)
//	  lock wallet -> read reference -> apply -> persist balance, ledger, row, outbox -> nudge
//	  complete inbox row
//	COMMIT
//
// Errors: *wager.ValidationError (category A, nothing persisted),
// *wager.ConflictError (category C), app.ErrProviderNotAllowed,
// app.ErrInboxHashMismatch, app.ErrTransient (rolled back, retry). An invariant violation rolls back
// and is recorded as FAILED in a separate transaction (§5); the FAILED
// transaction is then the result.
func (s *Service) Submit(ctx context.Context, cmd SubmitCommand) (res SubmitResult, err error) {
	start := time.Now()
	defer func() { s.observe(cmd, res, err, time.Since(start)) }()
	return s.submit(ctx, cmd)
}

// observe records the outcome of one submission (ARCHITECTURE §25): a new
// state, a replay or duplicate, or a conflict, and the duration.
func (s *Service) observe(cmd SubmitCommand, res SubmitResult, err error, d time.Duration) {
	transport := app.TransportHTTP
	if cmd.Inbox != nil {
		transport = app.TransportSQS
	}
	outcome := "error"
	var ce *wager.ConflictError
	switch {
	case err == nil && res.Duplicate:
		outcome = "replay"
		s.metrics.Duplicate(transport, app.DuplicateInboxMessage)
	case err == nil && res.Replay:
		outcome = "replay"
		s.metrics.Duplicate(transport, app.DuplicateIdempotentReplay)
	case err == nil:
		outcome = string(res.Transaction.Status())
		s.metrics.TransactionOutcome(transport, res.Transaction.Status(), res.Transaction.FailureCode())
	case errors.As(err, &ce):
		s.metrics.Conflict(ce.Code)
	}
	s.metrics.ProcessingDuration(transport, outcome, d)
}

func (s *Service) submit(ctx context.Context, cmd SubmitCommand) (SubmitResult, error) {
	req, err := wager.NewRequest(cmd.Raw)
	if err != nil {
		return SubmitResult{}, err
	}
	active, err := s.providers.IsActive(ctx, req.ProviderID())
	if err != nil {
		return SubmitResult{}, err
	}
	if !active {
		return SubmitResult{}, fmt.Errorf("%w: %s", app.ErrProviderNotAllowed, req.ProviderID())
	}
	if err := s.preRead(ctx, req); err != nil {
		return SubmitResult{}, err
	}

	now := s.clock.Now()
	t, err := wager.NewExternal(req, s.ids.NewID(), cmd.CorrelationID, now)
	if err != nil {
		return SubmitResult{}, err
	}
	var res SubmitResult
	err = s.tx.InTx(ctx, app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		if dup, ok, err := claimInbox(ctx, r, req, cmd); ok || err != nil {
			res = dup
			return err
		}
		inserted, err := r.Transactions().InsertPending(ctx, t)
		if err != nil {
			return err
		}
		if !inserted {
			existing, err := replay(ctx, r, req)
			if err != nil {
				return err
			}
			res = SubmitResult{Transaction: existing, Replay: true}
			return completeInbox(ctx, r, cmd, existing, now)
		}
		if err := s.process(ctx, r, t, req, cmd, now); err != nil {
			return err
		}
		res = SubmitResult{Transaction: t}
		return completeInbox(ctx, r, cmd, t, now)
	})
	if errors.Is(err, app.ErrInvariantViolation) {
		return s.recordFailure(ctx, req, cmd, err)
	}
	if err != nil {
		return SubmitResult{}, err
	}
	s.logOutcome(res, cmd)
	return res, nil
}

// preRead is ARCHITECTURE §4 step 0: input errors before anything is
// written.
func (s *Service) preRead(ctx context.Context, req wager.Request) error {
	id, err := s.wallets.GetIdentity(ctx, req.WalletID())
	switch {
	case errors.Is(err, app.ErrNotFound):
		return &wager.ValidationError{Code: wager.CodeWalletNotFound, Field: "walletId", Msg: "wallet not found"}
	case err != nil:
		return err
	case id.PlayerID != req.PlayerID():
		return &wager.ValidationError{Code: wager.CodePlayerWalletMismatch, Field: "playerId", Msg: "player does not own the wallet"}
	case id.Currency != req.Money().Currency():
		return &wager.ValidationError{Code: wager.CodeCurrencyMismatch, Field: "money.currency", Msg: "wallet is " + id.Currency.String()}
	}
	return nil
}

// claimInbox inserts the inbox row first, before any lock (ARCHITECTURE
// §4 step 1). A messageId seen before is a duplicate when its hash matches
// (the original call's transaction is returned, nothing changes) and
// ErrInboxHashMismatch otherwise. ok reports a duplicate.
func claimInbox(ctx context.Context, r app.Repos, req wager.Request, cmd SubmitCommand) (dup SubmitResult, ok bool, err error) {
	if cmd.Inbox == nil {
		return SubmitResult{}, false, nil
	}
	hash, err := req.MessageHash()
	if err != nil {
		return SubmitResult{}, false, err
	}
	inserted, err := r.Inbox().Insert(ctx, app.InboxMessage{
		ConsumerName: cmd.Inbox.ConsumerName, MessageID: cmd.Inbox.MessageID, PayloadHash: hash, ReceivedAt: cmd.Inbox.ReceivedAt,
	})
	if err != nil || inserted {
		return SubmitResult{}, false, err
	}
	m, err := r.Inbox().Get(ctx, cmd.Inbox.ConsumerName, cmd.Inbox.MessageID)
	if err != nil {
		return SubmitResult{}, false, err
	}
	if m.PayloadHash != hash {
		return SubmitResult{}, false, fmt.Errorf("%w: %s", app.ErrInboxHashMismatch, cmd.Inbox.MessageID)
	}
	// Insert and completion share one transaction, and the insert waited
	// for any concurrent holder of the key, so a committed row is complete.
	if m.ProcessedAt == nil {
		return SubmitResult{}, false, fmt.Errorf("inbox message %s committed without completion", cmd.Inbox.MessageID)
	}
	existing, err := replay(ctx, r, req)
	if err != nil {
		return SubmitResult{}, false, err
	}
	return SubmitResult{Transaction: existing, Replay: true, Duplicate: true}, true, nil
}

// completeInbox records the outcome in the same commit as the domain
// changes. A PENDING_REFERENCE completes it: the worker takes over (§9).
func completeInbox(ctx context.Context, r app.Repos, cmd SubmitCommand, t *wager.WagerTransaction, now time.Time) error {
	if cmd.Inbox == nil {
		return nil
	}
	if t.Status() == wager.StatusPending {
		// Only a stale row left for the worker can be PENDING (ARCHITECTURE
		// §5); retry the message once the worker has moved it on.
		return fmt.Errorf("%w: transaction %s is still PENDING", app.ErrTransient, t.ID())
	}
	return r.Inbox().Complete(ctx, cmd.Inbox.ConsumerName, cmd.Inbox.MessageID, t.ID(), t.Status(), now)
}

// replay finds the row that made InsertPending a no-op, by key and then by
// external id, and applies the conflict rules (ARCHITECTURE §6).
func replay(ctx context.Context, r app.Repos, req wager.Request) (*wager.WagerTransaction, error) {
	existing, err := r.Transactions().FindByIdempotencyKey(ctx, req.ProviderID(), req.IdempotencyKey())
	if errors.Is(err, app.ErrNotFound) {
		existing, err = r.Transactions().FindByExternalID(ctx, req.ProviderID(), req.ExternalTransactionID())
	}
	if errors.Is(err, app.ErrNotFound) {
		return nil, fmt.Errorf("%w: insert conflicted but no matching row exists", app.ErrInvariantViolation)
	}
	if err != nil {
		return nil, err
	}
	if err := wager.CheckReplay(existing, req); err != nil {
		return nil, err
	}
	return existing, nil
}

// process is steps 2-4 for a freshly inserted row.
func (s *Service) process(ctx context.Context, r app.Repos, t *wager.WagerTransaction, req wager.Request, cmd SubmitCommand, now time.Time) error {
	w, err := r.Wallets().GetForUpdate(ctx, req.WalletID())
	if err != nil {
		return err
	}
	ref, err := lookupReference(ctx, r, req)
	if err != nil {
		return err
	}
	version := w.Version()
	out, err := wager.Apply(wager.ApplyParams{
		Transaction:   t,
		Wallet:        w,
		Reference:     ref,
		ReferenceTTL:  s.cfg.ReferenceTTL,
		NextAttemptAt: s.cfg.Backoff.Next(now, t.Attempts()),
		CausationID:   cmd.CausationID,
		Now:           now,
		NewID:         s.ids.NewID,
	})
	if err != nil {
		return err
	}
	return persist(ctx, r, t, w, version, out)
}

// lookupReference reads the reference with a fresh statement after the
// wallet lock, without locking it (ARCHITECTURE §4 step 3).
func lookupReference(ctx context.Context, r app.Repos, req wager.Request) (wager.ReferenceState, error) {
	refExt, ok := req.ReferenceExternalTransactionID()
	return findReference(ctx, r, req.ProviderID(), refExt, ok)
}

// findReference resolves (provider, refExt) and whether it already has a
// processed reversal. A missing reference is not an error.
func findReference(ctx context.Context, r app.Repos, providerID, refExt string, hasRef bool) (wager.ReferenceState, error) {
	if !hasRef {
		return wager.ReferenceState{}, nil
	}
	found, err := r.Transactions().FindByExternalID(ctx, providerID, refExt)
	if errors.Is(err, app.ErrNotFound) {
		return wager.ReferenceState{}, nil
	}
	if err != nil {
		return wager.ReferenceState{}, err
	}
	reversed, err := r.Transactions().HasProcessedReversal(ctx, found.ID())
	if err != nil {
		return wager.ReferenceState{}, err
	}
	return wager.ReferenceState{Transaction: found, Reversed: reversed}, nil
}

// persist writes what Apply produced, then nudges operations waiting on
// this one when it reached a terminal state (ARCHITECTURE §8).
func persist(ctx context.Context, r app.Repos, t *wager.WagerTransaction, w *wallet.Wallet, version int64, out wager.Outcome) error {
	if out.Entry != nil {
		if err := r.Wallets().UpdateBalance(ctx, w, version); err != nil {
			return err
		}
		if err := r.Ledger().Insert(ctx, *out.Entry); err != nil {
			return err
		}
	}
	if err := r.Transactions().Update(ctx, t); err != nil {
		return err
	}
	if err := r.Outbox().Insert(ctx, out.Events...); err != nil {
		return err
	}
	if t.Status().Terminal() {
		if _, err := r.Transactions().NudgeDependents(ctx, t.ProviderID(), t.ExternalTransactionID()); err != nil {
			return err
		}
	}
	return nil
}

// recordFailure persists FAILED/INTERNAL_INVARIANT_VIOLATION after the
// business transaction rolled back, in a separate transaction that takes
// the wallet lock like every state change (ARCHITECTURE §4/§5). If another
// call already persisted the operation, that one is replayed instead.
func (s *Service) recordFailure(ctx context.Context, req wager.Request, cmd SubmitCommand, cause error) (SubmitResult, error) {
	s.logger.Error("invariant violation, recording FAILED",
		slog.String("providerId", req.ProviderID()), slog.String("walletId", req.WalletID().String()),
		slog.String("correlationId", cmd.CorrelationID), slog.String("error", cause.Error()))
	now := s.clock.Now()
	t, err := wager.NewExternal(req, s.ids.NewID(), cmd.CorrelationID, now)
	if err != nil {
		return SubmitResult{}, err
	}
	var res SubmitResult
	err = s.tx.InTx(ctx, app.TxOptions{}, func(ctx context.Context, r app.Repos) error {
		if dup, ok, err := claimInbox(ctx, r, req, cmd); ok || err != nil {
			res = dup
			return err
		}
		inserted, err := r.Transactions().InsertPending(ctx, t)
		if err != nil {
			return err
		}
		if !inserted {
			existing, err := replay(ctx, r, req)
			if err != nil {
				return err
			}
			res = SubmitResult{Transaction: existing, Replay: true}
			return completeInbox(ctx, r, cmd, existing, now)
		}
		if _, err := r.Wallets().GetForUpdate(ctx, req.WalletID()); err != nil {
			return err
		}
		if err := t.Fail(wager.CodeInternalInvariantViolation, now); err != nil {
			return err
		}
		if err := r.Transactions().Update(ctx, t); err != nil {
			return err
		}
		res = SubmitResult{Transaction: t}
		return completeInbox(ctx, r, cmd, t, now)
	})
	if err != nil {
		return SubmitResult{}, fmt.Errorf("recording FAILED: %w (original: %w)", err, cause)
	}
	s.logOutcome(res, cmd)
	return res, nil
}

func (s *Service) logOutcome(res SubmitResult, cmd SubmitCommand) {
	t := res.Transaction
	attrs := []any{
		slog.String("transactionId", t.ID().String()), slog.String("walletId", t.WalletID().String()),
		slog.String("providerId", t.ProviderID()), slog.String("correlationId", cmd.CorrelationID),
		slog.String("status", string(t.Status())), slog.Bool("idempotentReplay", res.Replay),
	}
	if cmd.Inbox != nil {
		attrs = append(attrs, slog.Bool("duplicateMessage", res.Duplicate))
	}
	if t.FailureCode() != "" {
		attrs = append(attrs, slog.String("failureCode", string(t.FailureCode())))
	}
	if cmd.CausationID != nil {
		attrs = append(attrs, slog.String("messageId", *cmd.CausationID))
	}
	s.logger.Info("wager transaction submitted", attrs...)
}

// Get returns a transaction by internal id (ErrNotFound when absent).
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*wager.WagerTransaction, error) {
	return s.transactions.GetByID(ctx, id)
}

// GetByExternalID returns a provider's transaction (ErrNotFound when
// absent).
func (s *Service) GetByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wager.WagerTransaction, error) {
	return s.transactions.GetByExternalID(ctx, providerID, externalTransactionID)
}
