package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/medbottle/betting-wallet-go/internal/app/wallets"
	"github.com/medbottle/betting-wallet-go/internal/domain/event"
	"github.com/medbottle/betting-wallet-go/internal/domain/money"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
	"github.com/medbottle/betting-wallet-go/internal/domain/wallet"
)

// maxBodyBytes caps request bodies (ARCHITECTURE §20, Q9).
const maxBodyBytes = 64 << 10

// decodeJSON reads exactly one JSON object into dst. Malformed JSON,
// unknown fields, trailing data and oversized bodies are INVALID_REQUEST.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return invalidRequest("body", "request body exceeds 64 KiB")
		}
		return invalidRequest("body", "malformed JSON: "+err.Error())
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidRequest("body", "unexpected data after the JSON object")
	}
	return nil
}

func invalidRequest(field, msg string) error {
	return &wager.ValidationError{Code: wager.CodeInvalidRequest, Field: field, Msg: msg}
}

// moneyDTO keeps amount and currency raw so a JSON number is rejected
// before it can be decoded through a float (ARCHITECTURE §2).
type moneyDTO struct {
	Amount   json.RawMessage `json:"amount"`
	Currency json.RawMessage `json:"currency"`
}

// strings returns amount and currency, which must both be JSON strings.
// Anything else is INVALID_MONEY.
func (m *moneyDTO) strings(field string) (amount, currency string, err error) {
	if m == nil {
		return "", "", &wager.ValidationError{Code: wager.CodeInvalidMoney, Field: field, Msg: "is required"}
	}
	if amount, err = jsonString(m.Amount); err != nil {
		return "", "", &wager.ValidationError{Code: wager.CodeInvalidMoney, Field: field + ".amount", Msg: "must be a decimal string such as \"25.00\""}
	}
	if currency, err = jsonString(m.Currency); err != nil {
		return "", "", &wager.ValidationError{Code: wager.CodeInvalidMoney, Field: field + ".currency", Msg: "must be a string such as \"BRL\""}
	}
	return amount, currency, nil
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

// submitRequest is the body of POST /wagering/transactions (CHALLENGE §9).
type submitRequest struct {
	ProviderID                     string    `json:"providerId"`
	ExternalTransactionID          string    `json:"externalTransactionId"`
	PlayerID                       string    `json:"playerId"`
	WalletID                       string    `json:"walletId"`
	RoundID                        string    `json:"roundId"`
	GameID                         string    `json:"gameId"`
	Kind                           string    `json:"kind"`
	Money                          *moneyDTO `json:"money"`
	ReferenceExternalTransactionID *string   `json:"referenceExternalTransactionId"`
}

// outcomeResponse is the outcome body of ARCHITECTURE §13.
type outcomeResponse struct {
	TransactionID    string       `json:"transactionId"`
	Status           string       `json:"status"`
	Balance          *money.Money `json:"balance,omitempty"`
	FailureCode      string       `json:"failureCode,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}

func newOutcome(t *wager.WagerTransaction, replay bool) outcomeResponse {
	o := outcomeResponse{TransactionID: t.ID().String(), Status: string(t.Status()), FailureCode: string(t.FailureCode()), IdempotentReplay: replay}
	if res, ok := t.Result(); ok {
		b := res.Balance
		o.Balance = &b
	}
	return o
}

// transactionResponse is the transaction resource of the GET endpoints
// (ARCHITECTURE §20, Q7): status, failure code and the persisted result,
// so pending operations can be followed and outcomes inspected
// (CHALLENGE §9). Fields that do not apply are omitted.
type transactionResponse struct {
	TransactionID                  string       `json:"transactionId"`
	Origin                         string       `json:"origin"`
	Kind                           string       `json:"kind"`
	Status                         string       `json:"status"`
	WalletID                       string       `json:"walletId"`
	PlayerID                       string       `json:"playerId"`
	Money                          money.Money  `json:"money"`
	ProviderID                     string       `json:"providerId,omitempty"`
	ExternalTransactionID          string       `json:"externalTransactionId,omitempty"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string       `json:"referenceTransactionId,omitempty"`
	FailureCode                    string       `json:"failureCode,omitempty"`
	Balance                        *money.Money `json:"balance,omitempty"`
	WalletVersion                  *int64       `json:"walletVersion,omitempty"`
	ReferenceDeadlineAt            string       `json:"referenceDeadlineAt,omitempty"`
	CreatedAt                      string       `json:"createdAt"`
	UpdatedAt                      string       `json:"updatedAt"`
	CompletedAt                    string       `json:"completedAt,omitempty"`
}

func newTransactionResponse(t *wager.WagerTransaction) transactionResponse {
	r := transactionResponse{
		TransactionID: t.ID().String(), Origin: string(t.Origin()), Kind: string(t.Kind()), Status: string(t.Status()),
		WalletID: t.WalletID().String(), PlayerID: t.PlayerID().String(), Money: t.Money(),
		ProviderID: t.ProviderID(), ExternalTransactionID: t.ExternalTransactionID(), RoundID: t.RoundID(), GameID: t.GameID(),
		FailureCode: string(t.FailureCode()), CreatedAt: formatTime(t.CreatedAt()), UpdatedAt: formatTime(t.UpdatedAt()),
	}
	if ref, ok := t.ReferenceExternalTransactionID(); ok {
		r.ReferenceExternalTransactionID = ref
	}
	if id, ok := t.ReferenceTransactionID(); ok {
		r.ReferenceTransactionID = id.String()
	}
	if res, ok := t.Result(); ok {
		b, v := res.Balance, res.WalletVersion
		r.Balance, r.WalletVersion = &b, &v
	}
	if d, ok := t.ReferenceDeadline(); ok && t.Status() == wager.StatusPendingReference {
		r.ReferenceDeadlineAt = formatTime(d)
	}
	if c, ok := t.CompletedAt(); ok {
		r.CompletedAt = formatTime(c)
	}
	return r
}

// openWalletRequest is the body of POST /wallets (CHALLENGE §9).
type openWalletRequest struct {
	PlayerID       string    `json:"playerId"`
	InitialBalance *moneyDTO `json:"initialBalance"`
}

// walletResponse is the wallet resource of CHALLENGE §9.
type walletResponse struct {
	ID       string      `json:"id"`
	PlayerID string      `json:"playerId"`
	Balance  money.Money `json:"balance"`
	Version  int64       `json:"version"`
}

func newWalletResponse(w *wallet.Wallet) walletResponse {
	return walletResponse{ID: w.ID().String(), PlayerID: w.PlayerID().String(), Balance: w.Balance(), Version: w.Version()}
}

// ledgerResponse is one ledger page (ARCHITECTURE §20, Q7); entry fields
// follow CHALLENGE §6.4.
type ledgerResponse struct {
	WalletID   string        `json:"walletId"`
	Entries    []ledgerEntry `json:"entries"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type ledgerEntry struct {
	ID            string      `json:"id"`
	TransactionID string      `json:"transactionId"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	WalletVersion int64       `json:"walletVersion"`
	CreatedAt     string      `json:"createdAt"`
}

func newLedgerResponse(walletID string, page wallets.LedgerPage) ledgerResponse {
	out := ledgerResponse{WalletID: walletID, Entries: make([]ledgerEntry, 0, len(page.Entries)), NextCursor: page.NextCursor}
	for _, e := range page.Entries {
		out.Entries = append(out.Entries, ledgerEntry{
			ID: e.ID().String(), TransactionID: e.TransactionID().String(), Direction: string(e.Direction()),
			Money: e.Amount(), BalanceBefore: e.BalanceBefore(), BalanceAfter: e.BalanceAfter(),
			WalletVersion: e.WalletVersion(), CreatedAt: formatTime(e.CreatedAt()),
		})
	}
	return out
}

// reconciliationResponse is the reconciliation body of CHALLENGE §9.
type reconciliationResponse struct {
	WalletID          string      `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
}

// formatTime uses the same RFC 3339 UTC millisecond form as the events.
func formatTime(t time.Time) string { return event.FormatTime(t) }
