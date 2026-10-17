-- Every balance change commits with its ledger entry (CHALLENGE §5.3 and
-- §6.2, ARCHITECTURE §16). A deferred constraint trigger checks, at commit,
-- that each change of a wallet's balance from OLD to NEW has the entry
-- (wallet_id, wallet_version) = (NEW.id, NEW.version) going from
-- OLD.balance_minor to NEW.balance_minor, and that a wallet inserted with a
-- positive balance has its opening credit at its version, from 0. With the
-- version rule of 0003 and the arithmetic CHECK of 0005, the ledger thus
-- rebuilds every stored balance. Deferred, so the wallet and the entry may
-- be written in either order within the transaction.
CREATE FUNCTION wallets_ledger_link() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    before_minor BIGINT := 0;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.balance_minor = OLD.balance_minor THEN
            RETURN NULL;
        END IF;
        before_minor := OLD.balance_minor;
    ELSIF NEW.balance_minor = 0 THEN
        -- A zero opening balance creates no OPENING and no entry (CHALLENGE §9).
        RETURN NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM wallet_ledger_entries
        WHERE wallet_id = NEW.id
          AND wallet_version = NEW.version
          AND balance_before_minor = before_minor
          AND balance_after_minor = NEW.balance_minor
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            CONSTRAINT = 'wallets_ledger_link',
            MESSAGE = format('wallets: balance of %s changed from %s to %s at version %s without its ledger entry',
                             NEW.id, before_minor, NEW.balance_minor, NEW.version);
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER wallets_ledger_link
    AFTER INSERT OR UPDATE OF balance_minor ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallets_ledger_link();
