package wager

import (
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/medbottle/betting-wallet-go/internal/domain/money"
)

// maxIdentifierLen bounds identifiers and the idempotency key.
const maxIdentifierLen = 255

// RawRequest is an external operation as received, before validation. HTTP
// and SQS both fill it from their own envelopes. For SQS, IdempotencyKey is
// data.idempotencyKey; for HTTP it is the Idempotency-Key header.
type RawRequest struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           string
	Amount                         string
	Currency                       string
	ReferenceExternalTransactionID *string
}

// Request is a validated external operation. Both transports build the
// same Request, so both produce the same payload hash.
type Request struct {
	providerID            string
	externalTransactionID string
	idempotencyKey        string
	playerID              uuid.UUID
	walletID              uuid.UUID
	roundID               string
	gameID                string
	kind                  Kind
	money                 money.Money
	referenceExternalID   *string
}

// NewRequest validates raw input. The first failing check wins, in this
// order: idempotency key, identifiers and UUIDs, money, kind, amount for
// kind, references. Every error is a *ValidationError (category A).
func NewRequest(r RawRequest) (Request, error) {
	if r.IdempotencyKey == "" {
		return Request{}, invalid(CodeMissingIdempotencyKey, "idempotencyKey", "is required")
	}
	if !isIdempotencyKey(r.IdempotencyKey) {
		return Request{}, invalid(CodeInvalidRequest, "idempotencyKey", "must be 1-255 printable ASCII characters")
	}

	for _, f := range []struct{ name, value string }{
		{"providerId", r.ProviderID},
		{"externalTransactionId", r.ExternalTransactionID},
		{"roundId", r.RoundID},
		{"gameId", r.GameID},
	} {
		if !isIdentifier(f.value) {
			return Request{}, invalid(CodeInvalidRequest, f.name, "must be 1-255 printable ASCII characters without spaces")
		}
	}
	playerID, err := parseUUID(r.PlayerID)
	if err != nil {
		return Request{}, invalid(CodeInvalidRequest, "playerId", "%v", err)
	}
	walletID, err := parseUUID(r.WalletID)
	if err != nil {
		return Request{}, invalid(CodeInvalidRequest, "walletId", "%v", err)
	}
	if r.ReferenceExternalTransactionID != nil && !isIdentifier(*r.ReferenceExternalTransactionID) {
		return Request{}, invalid(CodeInvalidRequest, "referenceExternalTransactionId", "must be 1-255 printable ASCII characters without spaces")
	}

	m, err := money.Parse(r.Amount, r.Currency)
	switch {
	case errors.Is(err, money.ErrUnsupportedCurrency):
		return Request{}, invalid(CodeUnsupportedCurrency, "money.currency", "%v", err)
	case err != nil:
		return Request{}, invalid(CodeInvalidMoney, "money", "%v", err)
	}

	kind := Kind(r.Kind)
	if !kind.External() {
		return Request{}, invalid(CodeUnsupportedKind, "kind", "%q is not accepted from providers", r.Kind)
	}
	if kind == KindLoss && !m.IsZero() {
		return Request{}, invalid(CodeInvalidAmountForKind, "money.amount", "LOSS requires 0.00")
	}
	if kind != KindLoss && !m.IsPositive() {
		return Request{}, invalid(CodeInvalidAmountForKind, "money.amount", "%s requires an amount greater than zero", kind)
	}

	var ref *string
	if r.ReferenceExternalTransactionID != nil {
		v := *r.ReferenceExternalTransactionID
		ref = &v
	}
	switch {
	case kind.RequiresReference() && ref == nil:
		return Request{}, invalid(CodeMissingReference, "referenceExternalTransactionId", "is required for %s", kind)
	case !kind.AllowsReference() && ref != nil:
		return Request{}, invalid(CodeReferenceNotAllowed, "referenceExternalTransactionId", "is not allowed for %s", kind)
	}

	return Request{
		providerID:            r.ProviderID,
		externalTransactionID: r.ExternalTransactionID,
		idempotencyKey:        r.IdempotencyKey,
		playerID:              playerID,
		walletID:              walletID,
		roundID:               r.RoundID,
		gameID:                r.GameID,
		kind:                  kind,
		money:                 m,
		referenceExternalID:   ref,
	}, nil
}

func (r Request) ProviderID() string            { return r.providerID }
func (r Request) ExternalTransactionID() string { return r.externalTransactionID }
func (r Request) IdempotencyKey() string        { return r.idempotencyKey }
func (r Request) PlayerID() uuid.UUID           { return r.playerID }
func (r Request) WalletID() uuid.UUID           { return r.walletID }
func (r Request) RoundID() string               { return r.roundID }
func (r Request) GameID() string                { return r.gameID }
func (r Request) Kind() Kind                    { return r.kind }
func (r Request) Money() money.Money            { return r.money }

// ReferenceExternalTransactionID returns the reference, if any.
func (r Request) ReferenceExternalTransactionID() (string, bool) {
	if r.referenceExternalID == nil {
		return "", false
	}
	return *r.referenceExternalID, true
}

// valid reports whether r was built by NewRequest.
func (r Request) valid() bool { return r.kind.External() && r.money.Valid() }

// isIdentifier accepts 1-255 printable ASCII characters without spaces
// (0x21-0x7E).
func isIdentifier(s string) bool {
	if len(s) == 0 || len(s) > maxIdentifierLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '!' || s[i] > '~' {
			return false
		}
	}
	return true
}

// isIdempotencyKey accepts 1-255 printable ASCII characters, space
// included (0x20-0x7E), stored exactly as received.
func isIdempotencyKey(s string) bool {
	if len(s) == 0 || len(s) > maxIdentifierLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// parseUUID accepts only the 36-character hyphenated form, in either case.
// Braces, urn: prefixes, the 32-character form and the nil UUID are
// rejected. The result prints in lowercase.
func parseUUID(s string) (uuid.UUID, error) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return uuid.Nil, errors.New("must be a UUID in 8-4-4-4-12 form")
	}
	id, err := uuid.Parse(strings.ToLower(s))
	if err != nil {
		return uuid.Nil, errors.New("must be a UUID in 8-4-4-4-12 form")
	}
	if id == uuid.Nil {
		return uuid.Nil, errors.New("must not be the nil UUID")
	}
	return id, nil
}
