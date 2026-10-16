-- Transactional outbox (CHALLENGE §6.5/§11, ARCHITECTURE §9). Rows are
-- written in the business transaction and published after commit by the
-- relay, which claims them with a short lease.
CREATE TABLE outbox_events (
    event_id        UUID        PRIMARY KEY,
    aggregate_type  TEXT        NOT NULL
        CONSTRAINT outbox_events_aggregate_type_not_empty CHECK (aggregate_type <> ''),
    aggregate_id    UUID        NOT NULL,
    event_type      TEXT        NOT NULL
        CONSTRAINT outbox_events_event_type_valid CHECK (event_type IN (
            'WagerTransactionProcessed', 'WagerTransactionRejected',
            'WalletBalanceChanged', 'WagerTransactionPendingReference')),
    event_version   INTEGER     NOT NULL
        CONSTRAINT outbox_events_event_version_positive CHECK (event_version >= 1),
    correlation_id  TEXT        NOT NULL,
    causation_id    TEXT,
    occurred_at     TIMESTAMPTZ NOT NULL,
    -- Immutable snapshot of the full envelope.
    payload         JSONB       NOT NULL
        CONSTRAINT outbox_events_payload_is_object CHECK (jsonb_typeof(payload) = 'object'),

    -- Relay state.
    attempts        INTEGER     NOT NULL DEFAULT 0
        CONSTRAINT outbox_events_attempts_non_negative CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_by       TEXT,
    locked_until    TIMESTAMPTZ,
    last_error      TEXT,
    published_at    TIMESTAMPTZ,
    parked_at       TIMESTAMPTZ,

    CONSTRAINT outbox_events_published_or_parked CHECK (published_at IS NULL OR parked_at IS NULL)
);

-- Relay claim query (ARCHITECTURE §9).
CREATE INDEX outbox_events_due
    ON outbox_events (next_attempt_at) WHERE published_at IS NULL AND parked_at IS NULL;

-- The event itself is immutable (only relay state changes), a publication
-- is never undone, and rows are never removed.
CREATE FUNCTION outbox_events_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = format('outbox_events: %s is not allowed', TG_OP);
    END IF;

    IF (NEW.event_id, NEW.aggregate_type, NEW.aggregate_id, NEW.event_type, NEW.event_version,
        NEW.correlation_id, NEW.causation_id, NEW.occurred_at, NEW.payload)
       IS DISTINCT FROM
       (OLD.event_id, OLD.aggregate_type, OLD.aggregate_id, OLD.event_type, OLD.event_version,
        OLD.correlation_id, OLD.causation_id, OLD.occurred_at, OLD.payload) THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'outbox_events: event columns are immutable';
    END IF;

    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION USING
            ERRCODE = 'integrity_constraint_violation',
            MESSAGE = 'outbox_events: published_at cannot change once set';
    END IF;

    RETURN NEW;
END
$$;

CREATE TRIGGER outbox_events_guard_row
    BEFORE UPDATE OR DELETE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard();

CREATE TRIGGER outbox_events_guard_truncate
    BEFORE TRUNCATE ON outbox_events
    FOR EACH STATEMENT EXECUTE FUNCTION outbox_events_guard();

GRANT SELECT, INSERT, UPDATE ON outbox_events TO wallet_app;
