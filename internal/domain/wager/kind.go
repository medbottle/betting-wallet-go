package wager

// Kind is the type of a financial operation. The zero value is invalid.
type Kind string

const (
	KindOpening  Kind = "OPENING"
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return true
	}
	return false
}

// External reports whether k may arrive over HTTP or SQS. OPENING is
// reserved for internal wallet opening.
func (k Kind) External() bool { return k.Valid() && k != KindOpening }

// RequiresReference reports whether k must reference another operation.
func (k Kind) RequiresReference() bool { return k == KindRefund || k == KindRollback }

// AllowsReference reports whether k may reference another operation.
func (k Kind) AllowsReference() bool { return k == KindWin || k.RequiresReference() }

// IsReversal reports whether k reverses another operation.
func (k Kind) IsReversal() bool { return k.RequiresReference() }

// Status is the lifecycle state of a transaction (ARCHITECTURE §5).
type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return true
	}
	return false
}

// Terminal reports whether s admits no further transitions.
func (s Status) Terminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

// Origin distinguishes internal (OPENING) from external operations.
type Origin string

const (
	OriginInternal Origin = "INTERNAL"
	OriginExternal Origin = "EXTERNAL"
)

// Valid reports whether o is a known origin.
func (o Origin) Valid() bool { return o == OriginInternal || o == OriginExternal }
