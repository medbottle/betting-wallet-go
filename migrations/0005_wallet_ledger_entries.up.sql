-- Append-only wallet ledger (CHALLENGE §6.4, ARCHITECTURE schema sketch).
CREATE TABLE wallet_ledger_entries (
    id                   UUID        PRIMARY KEY,
    wallet_id            UUID        NOT NULL,
    currency             CHAR(3)     NOT NULL,
    transaction_id       UUID        NOT NULL,
    direction            TEXT        NOT NULL
        CONSTRAINT wallet_ledger_entries_direction_valid CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor         BIGINT      NOT NULL
        CONSTRAINT wallet_ledger_entries_amount_positive CHECK (amount_minor > 0),
    balance_before_minor BIGINT      NOT NULL
        CONSTRAINT wallet_ledger_entries_balance_before_non_negative CHECK (balance_before_minor >= 0),
    balance_after_minor  BIGINT      NOT NULL
        CONSTRAINT wallet_ledger_entries_balance_after_non_negative CHECK (balance_after_minor >= 0),
    -- Wallet version after this entry; the OPENING credit is version 1.
    wallet_version       BIGINT      NOT NULL
        CONSTRAINT wallet_ledger_entries_wallet_version_positive CHECK (wallet_version >= 1),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT wallet_ledger_entries_wallet_fk
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),
    -- The entry's transaction belongs to the same wallet and currency.
    CONSTRAINT wallet_ledger_entries_transaction_fk
        FOREIGN KEY (transaction_id, wallet_id, currency) REFERENCES wager_transactions (id, wallet_id, currency),

    CONSTRAINT wallet_ledger_entries_balance_arithmetic CHECK (
        balance_after_minor = balance_before_minor
            + CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END
    ),

    -- One entry per transaction per wallet.
    CONSTRAINT wallet_ledger_entries_wallet_transaction_key UNIQUE (wallet_id, transaction_id),
    -- One entry per wallet version; also the stable pagination cursor.
    CONSTRAINT wallet_ledger_entries_wallet_version_key UNIQUE (wallet_id, wallet_version)
);

-- Corrections are new entries; existing ones never change or disappear,
-- not even for the owner role.
CREATE FUNCTION wallet_ledger_entries_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION USING
        ERRCODE = 'integrity_constraint_violation',
        MESSAGE = format('wallet_ledger_entries: %s is not allowed, the ledger is append-only', TG_OP);
END
$$;

CREATE TRIGGER wallet_ledger_entries_guard_row
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_guard();

CREATE TRIGGER wallet_ledger_entries_guard_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION wallet_ledger_entries_guard();

GRANT SELECT, INSERT ON wallet_ledger_entries TO wallet_app;
