package wager

import (
	"errors"
	"strings"
	"testing"
)

func TestNewRequestValid(t *testing.T) {
	for _, tc := range []struct{ kind, amount string }{
		{"BET", "25.00"}, {"WIN", "10.00"}, {"LOSS", "0.00"}, {"REFUND", "25.00"}, {"ROLLBACK", "25.00"},
	} {
		req, err := NewRequest(raw(tc.kind, tc.amount))
		if err != nil {
			t.Errorf("%s: %v", tc.kind, err)
			continue
		}
		if string(req.Kind()) != tc.kind || req.Money().String() != tc.amount || req.ProviderID() != "provider-a" ||
			req.PlayerID().String() != playerStr || req.WalletID().String() != walletStr {
			t.Errorf("%s: unexpected request %+v", tc.kind, req)
		}
	}
	win := raw("WIN", "10.00")
	win.ReferenceExternalTransactionID = strp("bet-1")
	if req, err := NewRequest(win); err != nil {
		t.Errorf("WIN with reference: %v", err)
	} else if ref, ok := req.ReferenceExternalTransactionID(); !ok || ref != "bet-1" {
		t.Errorf("reference = %q, %v", ref, ok)
	}
}

func TestNewRequestNormalisesUUIDCase(t *testing.T) {
	r := raw("BET", "25.00")
	r.PlayerID = strings.ToUpper(playerStr)
	req := mustRequest(t, r)
	if req.PlayerID().String() != playerStr {
		t.Errorf("playerId = %s, want lowercase", req.PlayerID())
	}
}

func TestNewRequestRejects(t *testing.T) {
	type mut func(*RawRequest)
	cases := []struct {
		name  string
		kind  string
		mut   mut
		code  FailureCode
		field string
	}{
		// Idempotency key.
		{"missing key", "BET", func(r *RawRequest) { r.IdempotencyKey = "" }, CodeMissingIdempotencyKey, "idempotencyKey"},
		{"key too long", "BET", func(r *RawRequest) { r.IdempotencyKey = strings.Repeat("k", 256) }, CodeInvalidRequest, "idempotencyKey"},
		{"key with newline", "BET", func(r *RawRequest) { r.IdempotencyKey = "a\nb" }, CodeInvalidRequest, "idempotencyKey"},
		{"key non-ascii", "BET", func(r *RawRequest) { r.IdempotencyKey = "chave-é" }, CodeInvalidRequest, "idempotencyKey"},
		// Identifiers.
		{"empty provider", "BET", func(r *RawRequest) { r.ProviderID = "" }, CodeInvalidRequest, "providerId"},
		{"external id with space", "BET", func(r *RawRequest) { r.ExternalTransactionID = "tx 1" }, CodeInvalidRequest, "externalTransactionId"},
		{"external id padded", "BET", func(r *RawRequest) { r.ExternalTransactionID = " tx-1" }, CodeInvalidRequest, "externalTransactionId"},
		{"round too long", "BET", func(r *RawRequest) { r.RoundID = strings.Repeat("r", 256) }, CodeInvalidRequest, "roundId"},
		{"game non-ascii", "BET", func(r *RawRequest) { r.GameID = "jogo-é" }, CodeInvalidRequest, "gameId"},
		{"reference empty", "REFUND", func(r *RawRequest) { r.ReferenceExternalTransactionID = strp("") }, CodeInvalidRequest, "referenceExternalTransactionId"},
		// UUIDs.
		{"player not uuid", "BET", func(r *RawRequest) { r.PlayerID = "player-1" }, CodeInvalidRequest, "playerId"},
		{"player braces", "BET", func(r *RawRequest) { r.PlayerID = "{" + playerStr + "}" }, CodeInvalidRequest, "playerId"},
		{"player urn", "BET", func(r *RawRequest) { r.PlayerID = "urn:uuid:" + playerStr }, CodeInvalidRequest, "playerId"},
		{"player no hyphens", "BET", func(r *RawRequest) { r.PlayerID = strings.ReplaceAll(playerStr, "-", "") }, CodeInvalidRequest, "playerId"},
		{"player nil", "BET", func(r *RawRequest) { r.PlayerID = "00000000-0000-0000-0000-000000000000" }, CodeInvalidRequest, "playerId"},
		{"wallet not uuid", "BET", func(r *RawRequest) { r.WalletID = "0192f291-27dd-7d3f-8071-5f8685deefZZ" }, CodeInvalidRequest, "walletId"},
		// Money.
		{"amount scale", "BET", func(r *RawRequest) { r.Amount = "25.5" }, CodeInvalidMoney, "money"},
		{"amount negative", "BET", func(r *RawRequest) { r.Amount = "-25.00" }, CodeInvalidMoney, "money"},
		{"amount scientific", "BET", func(r *RawRequest) { r.Amount = "2.5e1" }, CodeInvalidMoney, "money"},
		{"amount NaN", "BET", func(r *RawRequest) { r.Amount = "NaN" }, CodeInvalidMoney, "money"},
		{"amount Infinity", "BET", func(r *RawRequest) { r.Amount = "Infinity" }, CodeInvalidMoney, "money"},
		{"amount empty", "BET", func(r *RawRequest) { r.Amount = "" }, CodeInvalidMoney, "money"},
		{"currency lowercase", "BET", func(r *RawRequest) { r.Currency = "brl" }, CodeInvalidMoney, "money"},
		{"currency unsupported", "BET", func(r *RawRequest) { r.Currency = "EUR" }, CodeUnsupportedCurrency, "money.currency"},
		// Kind.
		{"opening", "OPENING", nil, CodeUnsupportedKind, "kind"},
		{"lowercase kind", "bet", nil, CodeUnsupportedKind, "kind"},
		{"unknown kind", "JACKPOT", nil, CodeUnsupportedKind, "kind"},
		{"empty kind", "", nil, CodeUnsupportedKind, "kind"},
		// Zero-amount policy per kind.
		{"loss with amount", "LOSS", func(r *RawRequest) { r.Amount = "1.00" }, CodeInvalidAmountForKind, "money.amount"},
		{"bet zero", "BET", func(r *RawRequest) { r.Amount = "0.00" }, CodeInvalidAmountForKind, "money.amount"},
		{"win zero", "WIN", func(r *RawRequest) { r.Amount = "0.00" }, CodeInvalidAmountForKind, "money.amount"},
		{"refund zero", "REFUND", func(r *RawRequest) { r.Amount = "0.00" }, CodeInvalidAmountForKind, "money.amount"},
		{"rollback zero", "ROLLBACK", func(r *RawRequest) { r.Amount = "0.00" }, CodeInvalidAmountForKind, "money.amount"},
		// References.
		{"refund without reference", "REFUND", func(r *RawRequest) { r.ReferenceExternalTransactionID = nil }, CodeMissingReference, "referenceExternalTransactionId"},
		{"rollback without reference", "ROLLBACK", func(r *RawRequest) { r.ReferenceExternalTransactionID = nil }, CodeMissingReference, "referenceExternalTransactionId"},
		{"bet with reference", "BET", func(r *RawRequest) { r.ReferenceExternalTransactionID = strp("x") }, CodeReferenceNotAllowed, "referenceExternalTransactionId"},
		{"loss with reference", "LOSS", func(r *RawRequest) { r.ReferenceExternalTransactionID = strp("x") }, CodeReferenceNotAllowed, "referenceExternalTransactionId"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amount := "25.00"
			if tc.kind == "LOSS" {
				amount = "0.00"
			}
			r := raw(tc.kind, amount)
			if tc.mut != nil {
				tc.mut(&r)
			}
			_, err := NewRequest(r)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if ve.Code != tc.code || ve.Field != tc.field {
				t.Errorf("got %s on %q, want %s on %q (%v)", ve.Code, ve.Field, tc.code, tc.field, err)
			}
			if ve.Code.Category() != CategoryInput {
				t.Errorf("%s is not category A", ve.Code)
			}
		})
	}
}

func TestNewRequestPrecedence(t *testing.T) {
	// Missing key wins over everything else.
	r := raw("OPENING", "-1")
	r.IdempotencyKey = ""
	r.PlayerID = "x"
	if _, err := NewRequest(r); !hasCode(err, CodeMissingIdempotencyKey) {
		t.Errorf("err = %v", err)
	}
	// Format errors win over money and kind.
	r = raw("OPENING", "-1")
	r.GameID = ""
	if _, err := NewRequest(r); !hasCode(err, CodeInvalidRequest) {
		t.Errorf("err = %v", err)
	}
	// Money wins over kind.
	if _, err := NewRequest(raw("OPENING", "-1")); !hasCode(err, CodeInvalidMoney) {
		t.Errorf("err = %v", err)
	}
}

func hasCode(err error, code FailureCode) bool {
	var ve *ValidationError
	return errors.As(err, &ve) && ve.Code == code
}
