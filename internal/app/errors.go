package app

import "errors"

// Sentinel errors returned by the ports. Adapters wrap the underlying error
// with these, so callers classify with errors.Is and logs keep the cause.
var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("app: not found")
	// ErrWalletAlreadyExists means the player already has a wallet in that
	// currency (WALLET_ALREADY_EXISTS, category C).
	ErrWalletAlreadyExists = errors.New("app: wallet already exists")
	// ErrTransient means the operation may succeed if retried: rolled back,
	// nothing persisted (category E: HTTP 503, SQS redelivery; ARCHITECTURE
	// §4/§5).
	ErrTransient = errors.New("app: transient failure")
	// ErrInvariantViolation means the database refused a change that the
	// domain should have prevented, or returned data that breaks a domain
	// invariant (category D, INTERNAL_INVARIANT_VIOLATION; ARCHITECTURE §5).
	ErrInvariantViolation = errors.New("app: invariant violation")
)
