package sqsconsumer

import (
	"errors"
	"strings"
	"testing"

	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

const validBody = `{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}`

func TestParseEnvelopeChallengeExample(t *testing.T) {
	p, id, err := parseEnvelope([]byte(validBody))
	if err != nil {
		t.Fatal(err)
	}
	want := wager.RawRequest{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123", IdempotencyKey: "provider-a:transaction-123",
		PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37",
		RoundID: "round-987", GameID: "fortune-chimp", Kind: "BET", Amount: "25.00", Currency: "BRL",
	}
	if id != "msg-123" || p.MessageID != "msg-123" || p.Raw != want {
		t.Fatalf("got %q %+v", id, p)
	}
	if _, err := wager.NewRequest(p.Raw); err != nil {
		t.Fatalf("the CHALLENGE example must be a valid request: %v", err)
	}
}

func TestParseEnvelopeReference(t *testing.T) {
	body := strings.Replace(validBody, `"kind": "BET"`, `"kind": "REFUND", "referenceExternalTransactionId": "transaction-100"`, 1)
	p, _, err := parseEnvelope([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if p.Raw.ReferenceExternalTransactionID == nil || *p.Raw.ReferenceExternalTransactionID != "transaction-100" {
		t.Fatalf("reference = %v", p.Raw.ReferenceExternalTransactionID)
	}
}

func TestParseEnvelopeRejects(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		code  wager.FailureCode
		envID string
	}{
		{"not json", `not json`, wager.CodeInvalidRequest, ""},
		{"array", `[]`, wager.CodeInvalidRequest, ""},
		{"trailing data", validBody + `{}`, wager.CodeInvalidRequest, "msg-123"},
		{"unknown envelope field", strings.Replace(validBody, `"type"`, `"extra": 1, "type"`, 1), wager.CodeInvalidRequest, "msg-123"},
		{"unknown data field", strings.Replace(validBody, `"kind"`, `"extra": 1, "kind"`, 1), wager.CodeInvalidRequest, "msg-123"},
		{"missing messageId", strings.Replace(validBody, `"messageId": "msg-123",`, ``, 1), wager.CodeInvalidRequest, ""},
		{"messageId with space", strings.Replace(validBody, `"msg-123"`, `"msg 123"`, 1), wager.CodeInvalidRequest, ""},
		{"messageId too long", strings.Replace(validBody, `"msg-123"`, `"`+strings.Repeat("m", 256)+`"`, 1), wager.CodeInvalidRequest, ""},
		{"wrong type", strings.Replace(validBody, `"WagerTransactionRequested"`, `"WagerTransactionProcessed"`, 1), wager.CodeInvalidRequest, "msg-123"},
		{"bad occurredAt", strings.Replace(validBody, `"2026-09-08T12:00:00.000Z"`, `"08/09/2026"`, 1), wager.CodeInvalidRequest, "msg-123"},
		{"missing data", `{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z"}`, wager.CodeInvalidRequest, "msg-123"},
		{"amount as number", strings.Replace(validBody, `"amount": "25.00"`, `"amount": 25.00`, 1), wager.CodeInvalidMoney, "msg-123"},
		{"currency as number", strings.Replace(validBody, `"currency": "BRL"`, `"currency": 986`, 1), wager.CodeInvalidMoney, "msg-123"},
		{"missing money", strings.Replace(validBody, `"money": { "amount": "25.00", "currency": "BRL" }`, `"money": null`, 1), wager.CodeInvalidMoney, "msg-123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, id, err := parseEnvelope([]byte(tc.body))
			var ve *wager.ValidationError
			if !errors.As(err, &ve) || ve.Code != tc.code {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
			if id != tc.envID {
				t.Fatalf("envelope id = %q, want %q", id, tc.envID)
			}
		})
	}
}
