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
)

// ResolveIterationTimeout bounds one worker iteration: its context and its
// statement_timeout (ARCHITECTURE §14).
const ResolveIterationTimeout = 10 * time.Second

// ResolveNext is one reference-worker iteration (ARCHITECTURE §8), in one
// transaction:
//
//	claim the oldest due PENDING/PENDING_REFERENCE row (FOR UPDATE SKIP LOCKED)
//	-> lock its wallet -> re-read the reference with a fresh statement
//	-> apply: process, reject (deadline passed) or reschedule with backoff
//	-> persist balance, ledger, row, outbox -> nudge dependents -> commit
//
// The row lock is the claim: if the process dies, the transaction aborts
// and any instance can claim the row again. didWork is false when nothing
// was due. A transient or unclassified error rolls back and is returned;
// the row stays due. An invariant violation (the same category D as in
// Submit) rolls back and the row is recorded as FAILED in a separate
// transaction (ARCHITECTURE §5), so a broken row cannot be retried forever.
func (s *Service) ResolveNext(ctx context.Context) (didWork bool, err error) {
	start := time.Now()
	outcome := "error"
	defer func() {
		if didWork {
			s.metrics.ProcessingDuration(app.TransportWorker, outcome, time.Since(start))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, ResolveIterationTimeout)
	defer cancel()
	opts := app.TxOptions{StatementTimeout: ResolveIterationTimeout}

	var claimed uuid.UUID
	var res *wager.WagerTransaction
	err = s.tx.InTx(ctx, opts, func(ctx context.Context, r app.Repos) error {
		t, found, err := r.Transactions().ClaimDue(ctx)
		if err != nil || !found {
			return err
		}
		claimed = t.ID()
		w, err := r.Wallets().GetForUpdate(ctx, t.WalletID())
		if err != nil {
			return err
		}
		ref, err := lookupReferenceOf(ctx, r, t)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		version := w.Version()
		out, err := wager.Apply(wager.ApplyParams{
			Transaction:  t,
			Wallet:       w,
			Reference:    ref,
			ReferenceTTL: s.cfg.ReferenceTTL,
			// This is attempt attempts+1; if it fails, the next one backs off
			// accordingly.
			NextAttemptAt: s.cfg.Backoff.Next(now, t.Attempts()+1),
			Now:           now,
			NewID:         s.ids.NewID,
		})
		if err != nil {
			return err
		}
		if err := persist(ctx, r, t, w, version, out); err != nil {
			return err
		}
		res = t
		return nil
	})
	switch {
	case err == nil && claimed == uuid.Nil:
		return false, nil
	case err == nil:
		s.logResolved(res)
		outcome = string(res.Status())
		if res.Status() == wager.StatusPendingReference {
			s.metrics.Retry(app.RetryReferenceWorker)
		} else {
			s.metrics.TransactionOutcome(app.TransportWorker, res.Status(), res.FailureCode())
		}
		return true, nil
	case claimed == uuid.Nil, ctx.Err() != nil, !isInvariantViolation(err):
		// Transient or unclassified: rolled back, the row stays due and any
		// instance retries it (ARCHITECTURE §27, H-1). Only an invariant
		// violation, which no retry can fix, is recorded as FAILED.
		return claimed != uuid.Nil, err
	default:
		if ferr := s.failClaimed(ctx, claimed, err); ferr != nil {
			return true, ferr
		}
		outcome = string(wager.StatusFailed)
		return true, nil
	}
}

// lookupReferenceOf is lookupReference for a persisted transaction.
func lookupReferenceOf(ctx context.Context, r app.Repos, t *wager.WagerTransaction) (wager.ReferenceState, error) {
	refExt, ok := t.ReferenceExternalTransactionID()
	return findReference(ctx, r, t.ProviderID(), refExt, ok)
}

// failClaimed records FAILED/INTERNAL_INVARIANT_VIOLATION for a row whose
// resolution hit a permanent error, locking the row and then its wallet
// like every state change (ARCHITECTURE §4/§5).
func (s *Service) failClaimed(ctx context.Context, id uuid.UUID, cause error) error {
	s.logger.Error("reference resolution failed permanently, recording FAILED",
		slog.String("transactionId", id.String()), slog.String("error", cause.Error()))
	failed := false
	err := s.tx.InTx(ctx, app.TxOptions{StatementTimeout: ResolveIterationTimeout}, func(ctx context.Context, r app.Repos) error {
		t, err := r.Transactions().GetForUpdate(ctx, id)
		if errors.Is(err, app.ErrNotFound) {
			return nil // already finished by someone else
		}
		if err != nil {
			return err
		}
		if _, err := r.Wallets().GetForUpdate(ctx, t.WalletID()); err != nil {
			return err
		}
		if err := t.Fail(wager.CodeInternalInvariantViolation, s.clock.Now()); err != nil {
			return err
		}
		failed = true
		return r.Transactions().Update(ctx, t)
	})
	if err != nil {
		return fmt.Errorf("recording FAILED for %s: %w (original: %w)", id, err, cause)
	}
	if failed {
		s.metrics.TransactionOutcome(app.TransportWorker, wager.StatusFailed, wager.CodeInternalInvariantViolation)
	}
	return nil
}

func (s *Service) logResolved(t *wager.WagerTransaction) {
	attrs := []any{
		slog.String("transactionId", t.ID().String()), slog.String("walletId", t.WalletID().String()),
		slog.String("providerId", t.ProviderID()), slog.String("correlationId", t.CorrelationID()),
		slog.String("status", string(t.Status())), slog.Int("attempts", t.Attempts()),
	}
	if t.FailureCode() != "" {
		attrs = append(attrs, slog.String("failureCode", string(t.FailureCode())))
	}
	if t.Status() == wager.StatusPendingReference {
		s.logger.Debug("reference still unavailable, rescheduled", attrs...)
		return
	}
	s.logger.Info("pending operation resolved", attrs...)
}
