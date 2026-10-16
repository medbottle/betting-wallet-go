package wager

// FailureCode is a stable, documented code (ARCHITECTURE §12). New codes
// require updating ARCHITECTURE.md and, for categories B and D, the
// wager_transactions_failure_code CHECK in the schema.
type FailureCode string

// Category A: correctable input. Not persisted; resubmittable with the
// same idempotency key.
const (
	CodeInvalidRequest        FailureCode = "INVALID_REQUEST"
	CodeMissingIdempotencyKey FailureCode = "MISSING_IDEMPOTENCY_KEY"
	CodeInvalidMoney          FailureCode = "INVALID_MONEY"
	CodeUnsupportedCurrency   FailureCode = "UNSUPPORTED_CURRENCY"
	CodeUnsupportedKind       FailureCode = "UNSUPPORTED_KIND"
	CodeInvalidAmountForKind  FailureCode = "INVALID_AMOUNT_FOR_KIND"
	CodeMissingReference      FailureCode = "MISSING_REFERENCE"
	CodeReferenceNotAllowed   FailureCode = "REFERENCE_NOT_ALLOWED"
	CodePlayerWalletMismatch  FailureCode = "PLAYER_WALLET_MISMATCH"
	CodeCurrencyMismatch      FailureCode = "CURRENCY_MISMATCH"
	CodeWalletNotFound        FailureCode = "WALLET_NOT_FOUND"
)

// Category B: business rejection. Definitive, persisted as REJECTED.
const (
	CodeInsufficientFunds          FailureCode = "INSUFFICIENT_FUNDS"
	CodeReversalInsufficientFunds  FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	CodeReferenceNotFound          FailureCode = "REFERENCE_NOT_FOUND"
	CodeReferenceUnresolvedTimeout FailureCode = "REFERENCE_UNRESOLVED_TIMEOUT"
	CodeReferenceNotProcessed      FailureCode = "REFERENCE_NOT_PROCESSED"
	CodeReferenceKindNotAllowed    FailureCode = "REFERENCE_KIND_NOT_ALLOWED"
	CodeReferenceMismatch          FailureCode = "REFERENCE_MISMATCH"
	CodeReferenceAmountMismatch    FailureCode = "REFERENCE_AMOUNT_MISMATCH"
	CodeAlreadyReversed            FailureCode = "ALREADY_REVERSED"
	CodeAmountOverflow             FailureCode = "AMOUNT_OVERFLOW"
)

// Category C: conflict. Not persisted as a transaction.
const (
	CodeIdempotencyKeyConflict FailureCode = "IDEMPOTENCY_KEY_CONFLICT"
	CodeExternalIDConflict     FailureCode = "EXTERNAL_ID_CONFLICT"
	CodeWalletAlreadyExists    FailureCode = "WALLET_ALREADY_EXISTS"
)

// Category D: permanent failure, persisted as FAILED.
const CodeInternalInvariantViolation FailureCode = "INTERNAL_INVARIANT_VIOLATION"

// Category E: transient, not persisted, retryable.
const CodeTemporarilyUnavailable FailureCode = "TEMPORARILY_UNAVAILABLE"

// Category groups failure codes by how they are handled.
type Category string

const (
	CategoryInput     Category = "A_INPUT"
	CategoryRejection Category = "B_REJECTION"
	CategoryConflict  Category = "C_CONFLICT"
	CategoryPermanent Category = "D_PERMANENT"
	CategoryTransient Category = "E_TRANSIENT"
)

var categories = map[FailureCode]Category{
	CodeInvalidRequest:        CategoryInput,
	CodeMissingIdempotencyKey: CategoryInput,
	CodeInvalidMoney:          CategoryInput,
	CodeUnsupportedCurrency:   CategoryInput,
	CodeUnsupportedKind:       CategoryInput,
	CodeInvalidAmountForKind:  CategoryInput,
	CodeMissingReference:      CategoryInput,
	CodeReferenceNotAllowed:   CategoryInput,
	CodePlayerWalletMismatch:  CategoryInput,
	CodeCurrencyMismatch:      CategoryInput,
	CodeWalletNotFound:        CategoryInput,

	CodeInsufficientFunds:          CategoryRejection,
	CodeReversalInsufficientFunds:  CategoryRejection,
	CodeReferenceNotFound:          CategoryRejection,
	CodeReferenceUnresolvedTimeout: CategoryRejection,
	CodeReferenceNotProcessed:      CategoryRejection,
	CodeReferenceKindNotAllowed:    CategoryRejection,
	CodeReferenceMismatch:          CategoryRejection,
	CodeReferenceAmountMismatch:    CategoryRejection,
	CodeAlreadyReversed:            CategoryRejection,
	CodeAmountOverflow:             CategoryRejection,

	CodeIdempotencyKeyConflict: CategoryConflict,
	CodeExternalIDConflict:     CategoryConflict,
	CodeWalletAlreadyExists:    CategoryConflict,

	CodeInternalInvariantViolation: CategoryPermanent,

	CodeTemporarilyUnavailable: CategoryTransient,
}

// Category returns the code's category, or "" for an unknown code.
func (c FailureCode) Category() Category { return categories[c] }

// Valid reports whether c is a documented code.
func (c FailureCode) Valid() bool { return c.Category() != "" }

// IsRejection reports whether c is a category B business rejection.
func (c FailureCode) IsRejection() bool { return c.Category() == CategoryRejection }

// CodesIn returns every code of category cat, in no particular order.
func CodesIn(cat Category) []FailureCode {
	var out []FailureCode
	for code, c := range categories {
		if c == cat {
			out = append(out, code)
		}
	}
	return out
}
