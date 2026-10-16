-- Registry of game providers (ARCHITECTURE §11). Managed by operations;
-- the application only reads it.
CREATE TABLE providers (
    provider_id TEXT    PRIMARY KEY
        CONSTRAINT providers_id_format CHECK (provider_id ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
    active      BOOLEAN NOT NULL DEFAULT true
);

CREATE FUNCTION providers_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION USING
        ERRCODE = 'integrity_constraint_violation',
        MESSAGE = format('providers: %s is not allowed', TG_OP);
END
$$;

CREATE TRIGGER providers_guard_truncate
    BEFORE TRUNCATE ON providers
    FOR EACH STATEMENT EXECUTE FUNCTION providers_guard();

GRANT SELECT ON providers TO wallet_app;

INSERT INTO providers (provider_id) VALUES ('provider-a'), ('provider-b');
