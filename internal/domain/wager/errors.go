package wager

import (
	"errors"
	"fmt"
)

// ErrInvalidTransition means a state change the state machine forbids,
// including any change to a terminal transaction.
var ErrInvalidTransition = errors.New("wager: invalid transition")

// ErrInvalidTransaction means a transaction would break its invariants
// (construction or rehydration), or an argument the caller must guarantee
// is missing. It signals a programming or data error, never a business
// rejection.
var ErrInvalidTransaction = errors.New("wager: invalid transaction")

// ValidationError is a category A input error: not persisted, correctable
// by the client.
type ValidationError struct {
	Code  FailureCode
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Field, e.Msg)
}

// RejectionError is a category B business rejection.
type RejectionError struct {
	Code FailureCode
}

func (e *RejectionError) Error() string { return string(e.Code) }

// ConflictError is a category C idempotency or uniqueness conflict.
type ConflictError struct {
	Code FailureCode
	Msg  string
}

func (e *ConflictError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Msg) }

func invalid(code FailureCode, field, format string, args ...any) error {
	return &ValidationError{Code: code, Field: field, Msg: fmt.Sprintf(format, args...)}
}

func invalidTx(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTransaction, fmt.Sprintf(format, args...))
}
