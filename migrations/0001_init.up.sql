-- Slice 1 placeholder: proves the migration pipeline (up and down) works
-- end to end. The real schema starts in 0002.
CREATE TABLE schema_info (
    id             SMALLINT    PRIMARY KEY CHECK (id = 1),
    initialized_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO schema_info (id) VALUES (1);
