package wager

import (
	"time"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/event"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// eventFactory builds the events of one operation with shared metadata.
type eventFactory struct {
	tx        *WagerTransaction
	newID     func() uuid.UUID
	causation *string
	now       time.Time
}

func (f eventFactory) meta() event.Meta {
	return event.Meta{
		EventID:       f.newID(),
		AggregateID:   f.tx.walletID,
		CorrelationID: f.tx.correlationID,
		CausationID:   f.causation,
		OccurredAt:    f.now,
	}
}

func (f eventFactory) processed() (event.Envelope, error) {
	t := f.tx
	res := *t.result
	d := event.WagerTransactionProcessedData{
		TransactionID:         t.id,
		WalletID:              t.walletID,
		Origin:                string(t.origin),
		Kind:                  string(t.kind),
		ProviderID:            t.providerID,
		ExternalTransactionID: t.externalTransactionID,
		RoundID:               t.roundID,
		GameID:                t.gameID,
		Money:                 t.money,
		Balance:               res.Balance,
		WalletVersion:         res.WalletVersion,
	}
	if t.referenceID != nil {
		id := *t.referenceID
		d.ReferenceTransactionID = &id
	}
	return event.NewWagerTransactionProcessed(f.meta(), d)
}

func (f eventFactory) rejected() (event.Envelope, error) {
	t := f.tx
	res := *t.result
	return event.NewWagerTransactionRejected(f.meta(), event.WagerTransactionRejectedData{
		TransactionID:         t.id,
		WalletID:              t.walletID,
		Kind:                  string(t.kind),
		ProviderID:            t.providerID,
		ExternalTransactionID: t.externalTransactionID,
		FailureCode:           string(t.failureCode),
		Money:                 t.money,
		Balance:               res.Balance,
		WalletVersion:         res.WalletVersion,
	})
}

func (f eventFactory) balanceChanged(e wallet.LedgerEntry) (event.Envelope, error) {
	return event.NewWalletBalanceChanged(f.meta(), event.WalletBalanceChangedData{
		WalletID:      e.WalletID(),
		TransactionID: e.TransactionID(),
		Direction:     string(e.Direction()),
		Money:         e.Amount(),
		BalanceBefore: e.BalanceBefore(),
		BalanceAfter:  e.BalanceAfter(),
		WalletVersion: e.WalletVersion(),
	})
}

func (f eventFactory) pendingReference() (event.Envelope, error) {
	t := f.tx
	return event.NewWagerTransactionPendingReference(f.meta(), event.WagerTransactionPendingReferenceData{
		TransactionID:                  t.id,
		WalletID:                       t.walletID,
		Kind:                           string(t.kind),
		ProviderID:                     t.providerID,
		ExternalTransactionID:          t.externalTransactionID,
		ReferenceExternalTransactionID: *t.referenceExternalID,
		ReferenceDeadlineAt:            event.FormatTime(*t.deadline),
		Money:                          t.money,
	})
}

// completedEvents returns the events of a completed operation:
// WagerTransactionProcessed (plus WalletBalanceChanged when money moved) or
// WagerTransactionRejected.
func (f eventFactory) completedEvents(entry *wallet.LedgerEntry) ([]event.Envelope, error) {
	if f.tx.status == StatusRejected {
		e, err := f.rejected()
		if err != nil {
			return nil, err
		}
		return []event.Envelope{e}, nil
	}
	p, err := f.processed()
	if err != nil {
		return nil, err
	}
	out := []event.Envelope{p}
	if entry != nil {
		b, err := f.balanceChanged(*entry)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}
