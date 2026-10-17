package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/medbottle/betting-wallet-go/internal/app/authz"
	"github.com/medbottle/betting-wallet-go/internal/app/wagering"
	"github.com/medbottle/betting-wallet-go/internal/app/wallets"
	"github.com/medbottle/betting-wallet-go/internal/domain/wager"
)

// principal returns the caller set by RequireAuth. Business routes are
// only registered behind it, so a missing principal is a wiring bug and is
// refused.
func principal(r *http.Request) (authz.Principal, error) {
	p, ok := authz.FromContext(r.Context())
	if !ok {
		return authz.Principal{}, authz.ErrForbidden
	}
	return p, nil
}

// submit is POST /wagering/transactions. Order: parse the body, check the
// providerId against the token (403 with no effects), then hand the raw
// request to the use case, which validates it (ARCHITECTURE §6/§11/§13).
func (h handlers) submit(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	var body submitRequest
	if err := decodeJSON(w, r, &body); err != nil {
		h.writeAppError(w, r, err)
		return
	}
	if err := p.CanSubmitTransaction(body.ProviderID); err != nil {
		h.writeAppError(w, r, err)
		return
	}
	amount, currency, err := body.Money.strings("money")
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	res, err := h.wagering.Submit(r.Context(), wagering.SubmitCommand{
		Raw: wager.RawRequest{
			ProviderID:                     body.ProviderID,
			ExternalTransactionID:          body.ExternalTransactionID,
			IdempotencyKey:                 r.Header.Get("Idempotency-Key"),
			PlayerID:                       body.PlayerID,
			WalletID:                       body.WalletID,
			RoundID:                        body.RoundID,
			GameID:                         body.GameID,
			Kind:                           body.Kind,
			Amount:                         amount,
			Currency:                       currency,
			ReferenceExternalTransactionID: body.ReferenceExternalTransactionID,
		},
		CorrelationID: correlationID(r),
	})
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	t := res.Transaction
	status := http.StatusOK
	switch t.Status() {
	case wager.StatusRejected:
		status = http.StatusUnprocessableEntity
	case wager.StatusFailed:
		status = http.StatusInternalServerError
	case wager.StatusPendingReference, wager.StatusPending:
		status = http.StatusAccepted
		w.Header().Set("Location", "/providers/"+url.PathEscape(t.ProviderID())+"/wagering/transactions/"+url.PathEscape(t.ExternalTransactionID()))
	}
	writeJSON(w, status, newOutcome(t, res.Replay))
}

// getTransaction is GET /wagering/transactions/{transactionId}: 200 with
// the resource whatever its status; 404 when absent or another provider's.
// A caller with neither role is refused (403) before the lookup, whether
// the transaction exists or not.
func (h handlers) getTransaction(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err == nil {
		err = p.CanLookUpTransactions()
	}
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	id, err := wager.ParseID("transactionId", r.PathValue("transactionId"))
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	t, err := h.wagering.Get(r.Context(), id)
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	if err := p.CanReadTransaction(t.ProviderID()); err != nil {
		h.writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newTransactionResponse(t))
}

// getProviderTransaction is GET /providers/{providerId}/wagering/
// transactions/{externalTransactionId}. Another provider's path is
// refused before any lookup.
func (h handlers) getProviderTransaction(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	providerID := r.PathValue("providerId")
	if err := p.CanReadProviderTransaction(providerID); err != nil {
		h.writeAppError(w, r, err)
		return
	}
	t, err := h.wagering.GetByExternalID(r.Context(), providerID, r.PathValue("externalTransactionId"))
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newTransactionResponse(t))
}

// walletCaller checks the internal-only permission of the wallet routes.
func (h handlers) walletCaller(w http.ResponseWriter, r *http.Request) bool {
	p, err := principal(r)
	if err == nil {
		err = p.CanUseWalletOperations()
	}
	if err != nil {
		h.writeAppError(w, r, err)
		return false
	}
	return true
}

// openWallet is POST /wallets: 201, 409 WALLET_ALREADY_EXISTS or 400.
func (h handlers) openWallet(w http.ResponseWriter, r *http.Request) {
	if !h.walletCaller(w, r) {
		return
	}
	var body openWalletRequest
	if err := decodeJSON(w, r, &body); err != nil {
		h.writeAppError(w, r, err)
		return
	}
	amount, currency, err := body.InitialBalance.strings("initialBalance")
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	wl, err := h.wallets.Open(r.Context(), wallets.OpenCommand{
		PlayerID: body.PlayerID, Amount: amount, Currency: currency, CorrelationID: correlationID(r),
	})
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	w.Header().Set("Location", "/wallets/"+wl.ID().String())
	writeJSON(w, http.StatusCreated, newWalletResponse(wl))
}

// getWallet is GET /wallets/{walletId}.
func (h handlers) getWallet(w http.ResponseWriter, r *http.Request) {
	if !h.walletCaller(w, r) {
		return
	}
	id, err := wager.ParseID("walletId", r.PathValue("walletId"))
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	wl, err := h.wallets.Get(r.Context(), id)
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newWalletResponse(wl))
}

// listLedger is GET /wallets/{walletId}/ledger?cursor=...&limit=50.
func (h handlers) listLedger(w http.ResponseWriter, r *http.Request) {
	if !h.walletCaller(w, r) {
		return
	}
	id, err := wager.ParseID("walletId", r.PathValue("walletId"))
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 {
			h.writeAppError(w, r, invalidRequest("limit", "must be a positive integer"))
			return
		}
	}
	page, err := h.wallets.Ledger(r.Context(), id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newLedgerResponse(id.String(), page))
}

// reconcile is POST /wallets/{walletId}/reconciliation: 200 even when
// inconsistent.
func (h handlers) reconcile(w http.ResponseWriter, r *http.Request) {
	if !h.walletCaller(w, r) {
		return
	}
	id, err := wager.ParseID("walletId", r.PathValue("walletId"))
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	rec, err := h.wallets.Reconcile(r.Context(), id, correlationID(r))
	if err != nil {
		h.writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reconciliationResponse{
		WalletID: rec.WalletID.String(), StoredBalance: rec.StoredBalance, CalculatedBalance: rec.CalculatedBalance,
		Difference: rec.Difference, Consistent: rec.Consistent, CheckedEntries: rec.CheckedEntries,
	})
}
