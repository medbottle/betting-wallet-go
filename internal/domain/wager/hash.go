package wager

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
)

// HashVersion identifies the canonical form below (ARCHITECTURE §6). A
// change to fields or normalisation requires a new version.
const HashVersion int16 = 1

// PayloadHash is a SHA-256 digest of a canonical JSON document.
type PayloadHash [32]byte

// canonicalMoney and canonicalPayload declare their fields in lexicographic
// order of the JSON keys, which encoding/json preserves, so the encoding is
// the canonical form: sorted keys, no insignificant whitespace, UTF-8, no
// HTML escaping. A test re-derives the order from a sorted map.
type canonicalMoney struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type canonicalPayload struct {
	ExternalTransactionID          string         `json:"externalTransactionId"`
	GameID                         string         `json:"gameId"`
	Kind                           string         `json:"kind"`
	Money                          canonicalMoney `json:"money"`
	PlayerID                       string         `json:"playerId"`
	ProviderID                     string         `json:"providerId"`
	ReferenceExternalTransactionID *string        `json:"referenceExternalTransactionId,omitempty"`
	RoundID                        string         `json:"roundId"`
	WalletID                       string         `json:"walletId"`
}

// canonicalMessage is canonicalPayload plus idempotencyKey, for the inbox
// hash (ARCHITECTURE §9).
type canonicalMessage struct {
	ExternalTransactionID          string         `json:"externalTransactionId"`
	GameID                         string         `json:"gameId"`
	IdempotencyKey                 string         `json:"idempotencyKey"`
	Kind                           string         `json:"kind"`
	Money                          canonicalMoney `json:"money"`
	PlayerID                       string         `json:"playerId"`
	ProviderID                     string         `json:"providerId"`
	ReferenceExternalTransactionID *string        `json:"referenceExternalTransactionId,omitempty"`
	RoundID                        string         `json:"roundId"`
	WalletID                       string         `json:"walletId"`
}

func (r Request) canonicalPayload() canonicalPayload {
	return canonicalPayload{
		ExternalTransactionID:          r.externalTransactionID,
		GameID:                         r.gameID,
		Kind:                           string(r.kind),
		Money:                          canonicalMoney{Amount: r.money.String(), Currency: r.money.Currency().String()},
		PlayerID:                       r.playerID.String(),
		ProviderID:                     r.providerID,
		ReferenceExternalTransactionID: r.referenceExternalID,
		RoundID:                        r.roundID,
		WalletID:                       r.walletID.String(),
	}
}

// errUninitialisedRequest guards the exported hash methods: a zero Request
// must not hash to a value that could be stored or compared.
func errUninitialisedRequest() error {
	return invalidTx("request was not built by NewRequest")
}

// CanonicalPayload returns the canonical JSON hashed by PayloadHash. It
// excludes the idempotency key and all transport metadata.
func (r Request) CanonicalPayload() ([]byte, error) {
	if !r.valid() {
		return nil, errUninitialisedRequest()
	}
	return r.canonicalPayloadJSON(), nil
}

// PayloadHash returns the idempotency hash of the business fields.
func (r Request) PayloadHash() (PayloadHash, error) {
	if !r.valid() {
		return PayloadHash{}, errUninitialisedRequest()
	}
	return r.payloadHash(), nil
}

// CanonicalMessage returns the canonical JSON hashed by MessageHash.
func (r Request) CanonicalMessage() ([]byte, error) {
	if !r.valid() {
		return nil, errUninitialisedRequest()
	}
	return r.canonicalMessageJSON(), nil
}

// MessageHash returns the inbox hash: the business fields plus the
// idempotency key, so a messageId reused for another operation or another
// key is detected.
func (r Request) MessageHash() (PayloadHash, error) {
	if !r.valid() {
		return PayloadHash{}, errUninitialisedRequest()
	}
	return r.messageHash(), nil
}

// The unexported forms assume r is valid; callers check first.

func (r Request) canonicalPayloadJSON() []byte { return canonicalJSON(r.canonicalPayload()) }

func (r Request) payloadHash() PayloadHash { return sha256.Sum256(r.canonicalPayloadJSON()) }

func (r Request) messageHash() PayloadHash { return sha256.Sum256(r.canonicalMessageJSON()) }

func (r Request) canonicalMessageJSON() []byte {
	p := r.canonicalPayload()
	return canonicalJSON(canonicalMessage{
		ExternalTransactionID:          p.ExternalTransactionID,
		GameID:                         p.GameID,
		IdempotencyKey:                 r.idempotencyKey,
		Kind:                           p.Kind,
		Money:                          p.Money,
		PlayerID:                       p.PlayerID,
		ProviderID:                     p.ProviderID,
		ReferenceExternalTransactionID: p.ReferenceExternalTransactionID,
		RoundID:                        p.RoundID,
		WalletID:                       p.WalletID,
	})
}

func canonicalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoding plain strings and nested structs of strings cannot fail.
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
