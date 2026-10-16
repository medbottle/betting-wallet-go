-- Wallet aggregate root (CHALLENGE §6.2, ARCHITECTURE §4 and schema sketch).
CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL
        CONSTRAINT wallets_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    balance_minor BIGINT      NOT NULL
        CONSTRAINT wallets_balance_non_negative CHECK (balance_minor >= 0),
    version       BIGINT      NOT NULL
        CONSTRAINT wallets_version_positive CHECK (version >= 1),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One wallet per (player, currency).
    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency),
    -- Targets for composite foreign keys that pin currency and player.
    CONSTRAINT wallets_id_currency_key UNIQUE (id, currency),
    CONSTRAINT wallets_id_player_currency_key UNIQUE (id, player_id, currency)
);

-- Identity columns are immutable and wallets are never removed; the
-- non-locking pre-read in ARCHITECTURE §4 relies on this. The version moves
-- by exactly one with every balance change and never otherwise.
CREATE FUNCTION wallets_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = format('wallets: %s is not allowed', TG_OP);
    END IF;

    IF (NEW.id, NEW.player_id, NEW.currency, NEW.created_at)
       IS DISTINCT FROM (OLD.id, OLD.player_id, OLD.currency, OLD.created_at) THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'wallets: id, player_id, currency and created_at are immutable';
    END IF;

    IF NEW.balance_minor <> OLD.balance_minor THEN
        IF NEW.version <> OLD.version + 1 THEN
            RAISE EXCEPTION USING
                ERRCODE = 'integrity_constraint_violation',
                MESSAGE = 'wallets: version must increase by exactly 1 when the balance changes';
        END IF;
    ELSIF NEW.version <> OLD.version THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'wallets: version may change only together with the balance';
    END IF;

    RETURN NEW;
END
$$;

CREATE TRIGGER wallets_guard_row
    BEFORE UPDATE OR DELETE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard();

CREATE TRIGGER wallets_guard_truncate
    BEFORE TRUNCATE ON wallets
    FOR EACH STATEMENT EXECUTE FUNCTION wallets_guard();

GRANT SELECT, INSERT, UPDATE ON wallets TO wallet_app;
