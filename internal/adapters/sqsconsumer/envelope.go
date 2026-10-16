package sqsconsumer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

// MessageType is the only envelope type this consumer accepts.
const MessageType = "WagerTransactionRequested"

// envelope is the message body of CHALLENGE §10.
type envelope struct {
	MessageID  string   `json:"messageId"`
	Type       string   `json:"type"`
	OccurredAt string   `json:"occurredAt"`
	Data       *dataDTO `json:"data"`
}

type dataDTO struct {
	ProviderID                     string    `json:"providerId"`
	ExternalTransactionID          string    `json:"externalTransactionId"`
	IdempotencyKey                 string    `json:"idempotencyKey"`
	PlayerID                       string    `json:"playerId"`
	WalletID                       string    `json:"walletId"`
	RoundID                        string    `json:"roundId"`
	GameID                         string    `json:"gameId"`
	Kind                           string    `json:"kind"`
	Money                          *moneyDTO `json:"money"`
	ReferenceExternalTransactionID *string   `json:"referenceExternalTransactionId"`
}

// moneyDTO keeps amount and currency raw so a JSON number is rejected
// before it can be decoded through a float (ARCHITECTURE §2).
type moneyDTO struct {
	Amount   json.RawMessage `json:"amount"`
	Currency json.RawMessage `json:"currency"`
}

// parsed is a structurally valid envelope. Business validation happens in
// the use case, exactly as for HTTP.
type parsed struct {
	MessageID string
	Raw       wager.RawRequest
}

// parseEnvelope validates the envelope strictly (ARCHITECTURE §23, G7):
// one JSON object, no unknown fields, messageId of 1-255 printable ASCII
// characters without spaces, type WagerTransactionRequested, occurredAt in
// RFC 3339, data present and money as strings. Failures are
// *wager.ValidationError (INVALID_REQUEST or INVALID_MONEY). messageID is
// returned whenever it was readable and valid, even if a later check
// fails, for the DLQ attributes.
func parseEnvelope(body []byte) (p parsed, messageID string, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return parsed{}, peekMessageID(body), invalid("body", "malformed envelope: "+err.Error())
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if !validMessageID(env.MessageID) {
			env.MessageID = ""
		}
		return parsed{}, env.MessageID, invalid("body", "unexpected data after the JSON object")
	}
	if !validMessageID(env.MessageID) {
		return parsed{}, "", invalid("messageId", "must be 1-255 printable ASCII characters without spaces")
	}
	if env.Type != MessageType {
		return parsed{}, env.MessageID, invalid("type", "must be "+MessageType)
	}
	if _, err := time.Parse(time.RFC3339, env.OccurredAt); err != nil {
		return parsed{}, env.MessageID, invalid("occurredAt", "must be an RFC 3339 timestamp")
	}
	if env.Data == nil {
		return parsed{}, env.MessageID, invalid("data", "is required")
	}
	d := env.Data
	if d.Money == nil {
		return parsed{}, env.MessageID, &wager.ValidationError{Code: wager.CodeInvalidMoney, Field: "data.money", Msg: "is required"}
	}
	amount, err := jsonString(d.Money.Amount)
	if err != nil {
		return parsed{}, env.MessageID, &wager.ValidationError{Code: wager.CodeInvalidMoney, Field: "data.money.amount", Msg: "must be a decimal string such as \"25.00\""}
	}
	currency, err := jsonString(d.Money.Currency)
	if err != nil {
		return parsed{}, env.MessageID, &wager.ValidationError{Code: wager.CodeInvalidMoney, Field: "data.money.currency", Msg: "must be a string such as \"BRL\""}
	}
	return parsed{
		MessageID: env.MessageID,
		Raw: wager.RawRequest{
			ProviderID:                     d.ProviderID,
			ExternalTransactionID:          d.ExternalTransactionID,
			IdempotencyKey:                 d.IdempotencyKey,
			PlayerID:                       d.PlayerID,
			WalletID:                       d.WalletID,
			RoundID:                        d.RoundID,
			GameID:                         d.GameID,
			Kind:                           d.Kind,
			Amount:                         amount,
			Currency:                       currency,
			ReferenceExternalTransactionID: d.ReferenceExternalTransactionID,
		},
	}, env.MessageID, nil
}

// peekMessageID reads messageId from a body that failed strict decoding,
// for the DLQ attribute only.
func peekMessageID(body []byte) string {
	var v struct {
		MessageID string `json:"messageId"`
	}
	if json.Unmarshal(body, &v) != nil || !validMessageID(v.MessageID) {
		return ""
	}
	return v.MessageID
}

func validMessageID(s string) bool {
	if len(s) < 1 || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func jsonString(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", errors.New("not a JSON string")
	}
	var s string
	err := json.Unmarshal(raw, &s)
	return s, err
}

func invalid(field, msg string) error {
	return &wager.ValidationError{Code: wager.CodeInvalidRequest, Field: field, Msg: msg}
}
