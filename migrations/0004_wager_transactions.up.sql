-- Financial operations, internal (OPENING) and external (BET, WIN, LOSS,
-- REFUND, ROLLBACK). CHALLENGE §6.3, ARCHITECTURE §5-§8 and schema sketch.
CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    origin                            TEXT        NOT NULL
        CONSTRAINT wager_transactions_origin_valid CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    kind                              TEXT        NOT NULL
        CONSTRAINT wager_transactions_kind_valid CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    status                            TEXT        NOT NULL
        CONSTRAINT wager_transactions_status_valid CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    wallet_id                         UUID        NOT NULL,
    player_id                         UUID        NOT NULL,
    currency                          CHAR(3)     NOT NULL,
    amount_minor                      BIGINT      NOT NULL
        CONSTRAINT wager_transactions_amount_non_negative CHECK (amount_minor >= 0),

    -- External metadata; NULL for INTERNAL rows.
    provider_id                       TEXT
        CONSTRAINT wager_transactions_provider_fk REFERENCES providers (provider_id),
    external_transaction_id           TEXT,
    idempotency_key                   TEXT
        CONSTRAINT wager_transactions_idempotency_key_format CHECK (idempotency_key ~ '^[ -~]{1,255}$'),
    payload_hash                      BYTEA
        CONSTRAINT wager_transactions_payload_hash_sha256 CHECK (octet_length(payload_hash) = 32),
    hash_version                      SMALLINT
        CONSTRAINT wager_transactions_hash_version_positive CHECK (hash_version >= 1),
    round_id                          TEXT,
    game_id                           TEXT,
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID
        CONSTRAINT wager_transactions_reference_fk REFERENCES wager_transactions (id),

    -- Outcome.
    failure_code                      TEXT,
    result_balance_minor              BIGINT
        CONSTRAINT wager_transactions_result_balance_non_negative CHECK (result_balance_minor >= 0),
    result_wallet_version             BIGINT
        CONSTRAINT wager_transactions_result_version_positive CHECK (result_wallet_version >= 1),

    -- Reference worker scheduling (ARCHITECTURE §8).
    attempts                          INTEGER     NOT NULL DEFAULT 0
        CONSTRAINT wager_transactions_attempts_non_negative CHECK (attempts >= 0),
    next_attempt_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    reference_deadline_at             TIMESTAMPTZ,

    correlation_id                    TEXT        NOT NULL,
    created_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at                      TIMESTAMPTZ,

    -- Wallet, player and currency always agree with the wallet row.
    CONSTRAINT wager_transactions_wallet_fk
        FOREIGN KEY (wallet_id, player_id, currency) REFERENCES wallets (id, player_id, currency),
    -- Target for the ledger's composite foreign key.
    CONSTRAINT wager_transactions_id_wallet_currency_key UNIQUE (id, wallet_id, currency),

    -- Financial uniqueness and idempotency scope (ARCHITECTURE §6). NULLs
    -- are distinct, so INTERNAL rows never collide.
    CONSTRAINT wager_transactions_provider_external_id_key UNIQUE (provider_id, external_transaction_id),
    CONSTRAINT wager_transactions_provider_idempotency_key_key UNIQUE (provider_id, idempotency_key),

    -- INTERNAL rows are OPENINGs created already PROCESSED, without
    -- external metadata; EXTERNAL rows are never OPENING and carry it all.
    CONSTRAINT wager_transactions_origin_shape CHECK (
        (origin = 'INTERNAL' AND kind = 'OPENING' AND status = 'PROCESSED'
            AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL AND hash_version IS NULL
            AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL)
        OR
        (origin = 'EXTERNAL' AND kind <> 'OPENING'
            AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL AND hash_version IS NOT NULL
            AND round_id IS NOT NULL AND game_id IS NOT NULL)
    ),

    -- LOSS moves no money; every other kind moves a positive amount.
    CONSTRAINT wager_transactions_amount_by_kind CHECK ((kind = 'LOSS') = (amount_minor = 0)),

    -- REFUND/ROLLBACK require a reference; only WIN/REFUND/ROLLBACK may
    -- carry one; a resolved reference implies the external one.
    CONSTRAINT wager_transactions_reference_by_kind CHECK (
        CASE
            WHEN kind IN ('REFUND', 'ROLLBACK') THEN reference_external_transaction_id IS NOT NULL
            WHEN kind = 'WIN' THEN true
            ELSE reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL
        END
    ),
    CONSTRAINT wager_transactions_reference_resolved CHECK (
        reference_transaction_id IS NULL OR reference_external_transaction_id IS NOT NULL
    ),
    CONSTRAINT wager_transactions_reference_not_self CHECK (reference_transaction_id <> id),

    -- Only the stable codes of ARCHITECTURE §12: category B for REJECTED,
    -- category D for FAILED, none otherwise.
    CONSTRAINT wager_transactions_failure_code CHECK (
        CASE status
            WHEN 'REJECTED' THEN failure_code IS NOT NULL AND failure_code IN (
                'INSUFFICIENT_FUNDS', 'REVERSAL_INSUFFICIENT_FUNDS', 'REFERENCE_NOT_FOUND',
                'REFERENCE_UNRESOLVED_TIMEOUT', 'REFERENCE_NOT_PROCESSED', 'REFERENCE_KIND_NOT_ALLOWED',
                'REFERENCE_MISMATCH', 'REFERENCE_AMOUNT_MISMATCH', 'ALREADY_REVERSED', 'AMOUNT_OVERFLOW')
            WHEN 'FAILED' THEN failure_code IS NOT NULL AND failure_code = 'INTERNAL_INVARIANT_VIOLATION'
            ELSE failure_code IS NULL
        END
    ),

    -- Replays return the persisted result (ARCHITECTURE §6).
    CONSTRAINT wager_transactions_result CHECK (
        CASE
            WHEN status IN ('PROCESSED', 'REJECTED') THEN result_balance_minor IS NOT NULL AND result_wallet_version IS NOT NULL
            WHEN status IN ('PENDING', 'PENDING_REFERENCE') THEN result_balance_minor IS NULL AND result_wallet_version IS NULL
            ELSE true
        END
    ),
    CONSTRAINT wager_transactions_completed_at CHECK (
        (completed_at IS NOT NULL) = (status IN ('PROCESSED', 'REJECTED', 'FAILED'))
    ),
    CONSTRAINT wager_transactions_reference_deadline CHECK (
        status <> 'PENDING_REFERENCE' OR reference_deadline_at IS NOT NULL
    )
);

-- At most one initial credit per wallet.
CREATE UNIQUE INDEX wager_transactions_one_opening_per_wallet
    ON wager_transactions (wallet_id) WHERE kind = 'OPENING';

-- At most one successful reversal of any kind per referenced operation
-- (ARCHITECTURE §7).
CREATE UNIQUE INDEX wager_transactions_one_reversal_per_reference
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- Reference worker claim and nudge (ARCHITECTURE §8).
CREATE INDEX wager_transactions_due
    ON wager_transactions (next_attempt_at) WHERE status IN ('PENDING', 'PENDING_REFERENCE');
CREATE INDEX wager_transactions_pending_by_reference
    ON wager_transactions (provider_id, reference_external_transaction_id) WHERE status = 'PENDING_REFERENCE';

-- Terminal rows never change; business columns never change; the state
-- machine of ARCHITECTURE §5 never moves back to PENDING; a resolved
-- reference is never re-pointed; rows are never removed.
CREATE FUNCTION wager_transactions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = format('wager_transactions: %s is not allowed', TG_OP);
    END IF;

    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = format('wager_transactions: row %s is terminal (%s)', OLD.id, OLD.status);
    END IF;

    IF (NEW.id, NEW.origin, NEW.kind, NEW.wallet_id, NEW.player_id, NEW.currency, NEW.amount_minor,
        NEW.provider_id, NEW.external_transaction_id, NEW.idempotency_key, NEW.payload_hash,
        NEW.hash_version, NEW.round_id, NEW.game_id, NEW.reference_external_transaction_id,
        NEW.correlation_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.origin, OLD.kind, OLD.wallet_id, OLD.player_id, OLD.currency, OLD.amount_minor,
        OLD.provider_id, OLD.external_transaction_id, OLD.idempotency_key, OLD.payload_hash,
        OLD.hash_version, OLD.round_id, OLD.game_id, OLD.reference_external_transaction_id,
        OLD.correlation_id, OLD.created_at) THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'wager_transactions: business columns are immutable';
    END IF;

    IF OLD.status = 'PENDING_REFERENCE' AND NEW.status = 'PENDING' THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'wager_transactions: invalid transition PENDING_REFERENCE -> PENDING';
    END IF;

    IF OLD.reference_transaction_id IS NOT NULL
       AND NEW.reference_transaction_id IS DISTINCT FROM OLD.reference_transaction_id THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'wager_transactions: a resolved reference cannot change';
    END IF;

    RETURN NEW;
END
$$;

CREATE TRIGGER wager_transactions_guard_row
    BEFORE UPDATE OR DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();

CREATE TRIGGER wager_transactions_guard_truncate
    BEFORE TRUNCATE ON wager_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION wager_transactions_guard();

GRANT SELECT, INSERT, UPDATE ON wager_transactions TO wallet_app;
