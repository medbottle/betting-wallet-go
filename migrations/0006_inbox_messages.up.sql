-- SQS consumer inbox (CHALLENGE §6.5, ARCHITECTURE §9). The row is
-- inserted first in the business transaction and completed (outcome,
-- processed_at) before the same transaction commits.
CREATE TABLE inbox_messages (
    consumer_name  TEXT        NOT NULL
        CONSTRAINT inbox_messages_consumer_name_not_empty CHECK (consumer_name <> ''),
    message_id     TEXT        NOT NULL
        CONSTRAINT inbox_messages_message_id_not_empty CHECK (message_id <> ''),
    payload_hash   BYTEA       NOT NULL
        CONSTRAINT inbox_messages_payload_hash_sha256 CHECK (octet_length(payload_hash) = 32),
    transaction_id UUID
        CONSTRAINT inbox_messages_transaction_fk REFERENCES wager_transactions (id),
    -- Status of the linked transaction when handling completed.
    outcome        TEXT
        CONSTRAINT inbox_messages_outcome_valid CHECK (outcome IN ('PROCESSED', 'REJECTED', 'PENDING_REFERENCE', 'FAILED')),
    received_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at   TIMESTAMPTZ,

    CONSTRAINT inbox_messages_pkey PRIMARY KEY (consumer_name, message_id),
    CONSTRAINT inbox_messages_completion CHECK ((outcome IS NULL) = (processed_at IS NULL)),
    CONSTRAINT inbox_messages_completion_has_transaction CHECK (processed_at IS NULL OR transaction_id IS NOT NULL)
);

-- A completed message never changes; the identity and hash never change;
-- rows are never removed.
CREATE FUNCTION inbox_messages_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = format('inbox_messages: %s is not allowed', TG_OP);
    END IF;

    IF OLD.processed_at IS NOT NULL THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'inbox_messages: a completed message cannot change';
    END IF;

    IF (NEW.consumer_name, NEW.message_id, NEW.payload_hash, NEW.received_at)
       IS DISTINCT FROM (OLD.consumer_name, OLD.message_id, OLD.payload_hash, OLD.received_at) THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'inbox_messages: consumer_name, message_id, payload_hash and received_at are immutable';
    END IF;

    RETURN NEW;
END
$$;

CREATE TRIGGER inbox_messages_guard_row
    BEFORE UPDATE OR DELETE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION inbox_messages_guard();

CREATE TRIGGER inbox_messages_guard_truncate
    BEFORE TRUNCATE ON inbox_messages
    FOR EACH STATEMENT EXECUTE FUNCTION inbox_messages_guard();

GRANT SELECT, INSERT, UPDATE ON inbox_messages TO wallet_app;
