package wager

// CheckReplay applies the idempotency rules of ARCHITECTURE §6 to an
// incoming request and the existing transaction found for it, by
// (provider, idempotencyKey) or else by (provider, externalTransactionId):
//
//   - same key, same payload hash: nil, the caller replays the persisted
//     result;
//   - same key, different payload: IDEMPOTENCY_KEY_CONFLICT;
//   - different key, same external id: EXTERNAL_ID_CONFLICT, even with an
//     identical payload.
//
// Conflicts are *ConflictError. An existing transaction that matches
// neither key nor external id, or belongs to another provider, means the
// caller looked up the wrong row and yields ErrInvalidTransaction.
func CheckReplay(existing *WagerTransaction, req Request) error {
	if existing == nil || existing.origin != OriginExternal || !req.valid() {
		return invalidTx("replay check needs an external transaction and a validated request")
	}
	if existing.providerID != req.providerID {
		return invalidTx("replay lookup crossed providers")
	}
	if existing.idempotencyKey == req.idempotencyKey {
		if existing.hashVersion != HashVersion {
			return invalidTx("stored hash version %d is not comparable with %d", existing.hashVersion, HashVersion)
		}
		if existing.payloadHash != req.PayloadHash() {
			return &ConflictError{Code: CodeIdempotencyKeyConflict, Msg: "idempotency key reused with a different payload"}
		}
		return nil
	}
	if existing.externalTransactionID == req.externalTransactionID {
		return &ConflictError{Code: CodeExternalIDConflict, Msg: "external transaction already submitted with another idempotency key"}
	}
	return invalidTx("existing transaction matches neither key nor external id")
}
