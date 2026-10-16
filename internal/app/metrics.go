package app

import (
	"time"

	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

// Transport labels where an operation entered (or, for the worker, was
// resumed).
type Transport string

const (
	TransportHTTP   Transport = "http"
	TransportSQS    Transport = "sqs"
	TransportWorker Transport = "worker"
)

// DuplicateKind tells a replay by idempotency key from a redelivered inbox
// message.
type DuplicateKind string

const (
	DuplicateIdempotentReplay DuplicateKind = "idempotent_replay"
	DuplicateInboxMessage     DuplicateKind = "inbox_duplicate"
)

// Retry components (ARCHITECTURE §25).
const (
	RetryConsumer        = "consumer"
	RetryReferenceWorker = "reference_worker"
	RetryOutbox          = "outbox"
)

// Metrics records the measurements of CHALLENGE §12 (ARCHITECTURE §25).
// Implementations must be safe for concurrent use and never block. Labels
// are low-cardinality: no wallet, transaction or provider ids.
type Metrics interface {
	// TransactionOutcome counts an operation that reached a new state
	// (replays are counted by Duplicate, not here).
	TransactionOutcome(transport Transport, status wager.Status, code wager.FailureCode)
	Duplicate(transport Transport, kind DuplicateKind)
	// Conflict counts category C conflicts (ARCHITECTURE §12).
	Conflict(code wager.FailureCode)
	// DBConflict counts transactions rolled back by a concurrency or
	// timeout SQLSTATE (55P03, 40P01, 40001, 57014).
	DBConflict(sqlstate string)
	Retry(component string)
	DeadLettered(code wager.FailureCode)
	OutboxParked()
	// ProcessingDuration observes one submission or worker iteration;
	// outcome is the resulting status, or "error".
	ProcessingDuration(transport Transport, outcome string, d time.Duration)
	ReconciliationDivergence()
}

// NopMetrics discards every measurement.
type NopMetrics struct{}

var _ Metrics = NopMetrics{}

func (NopMetrics) TransactionOutcome(Transport, wager.Status, wager.FailureCode) {}
func (NopMetrics) Duplicate(Transport, DuplicateKind)                            {}
func (NopMetrics) Conflict(wager.FailureCode)                                    {}
func (NopMetrics) DBConflict(string)                                             {}
func (NopMetrics) Retry(string)                                                  {}
func (NopMetrics) DeadLettered(wager.FailureCode)                                {}
func (NopMetrics) OutboxParked()                                                 {}
func (NopMetrics) ProcessingDuration(Transport, string, time.Duration)           {}
func (NopMetrics) ReconciliationDivergence()                                     {}

// OrNop returns m, or NopMetrics when m is nil.
func OrNop(m Metrics) Metrics {
	if m == nil {
		return NopMetrics{}
	}
	return m
}
