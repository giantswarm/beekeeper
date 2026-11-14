-- The inbox between the webhook receiver and the leader (plan §4, §5). seq is the arrival order the
-- leader drains in; received_at comes from the receiver's clock.
CREATE TABLE inbox (
    delivery_id  text PRIMARY KEY,
    seq          bigint GENERATED ALWAYS AS IDENTITY,
    event        text NOT NULL,
    action       text NOT NULL DEFAULT '',
    received_at  timestamptz NOT NULL,
    payload      jsonb NOT NULL,
    processed_at timestamptz
);
CREATE INDEX inbox_unprocessed ON inbox (seq) WHERE processed_at IS NULL;
CREATE INDEX inbox_processed ON inbox (processed_at) WHERE processed_at IS NOT NULL;

-- Wakes the leader's LISTEN on every stored delivery (an ON CONFLICT DO NOTHING duplicate inserts no row,
-- so it wakes nobody).
CREATE FUNCTION inbox_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('inbox', NEW.delivery_id);
    RETURN NULL;
END
$$;
CREATE TRIGGER inbox_notify AFTER INSERT ON inbox FOR EACH ROW EXECUTE FUNCTION inbox_notify();

-- Append-only history (contract §5.2). Every timestamp is the engine clock's, written by the engine.
CREATE TABLE events (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at     timestamptz NOT NULL,
    team   text NOT NULL DEFAULT '',
    item   text NOT NULL DEFAULT '',
    change text NOT NULL DEFAULT '',
    type   text NOT NULL,
    source text NOT NULL DEFAULT '',
    data   jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX events_item_at ON events (item, at);
CREATE INDEX events_change_at ON events (change, at);
CREATE INDEX events_type_at ON events (type, at);

-- Decisions, recorded once per (point, subject, inputs_hash); replay re-runs them from inputs.
CREATE TABLE decisions (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at             timestamptz NOT NULL,
    point          text NOT NULL,
    subject        text NOT NULL,
    other          text NOT NULL DEFAULT '',
    inputs         jsonb NOT NULL DEFAULT '{}',
    inputs_hash    text NOT NULL,
    rule           text NOT NULL DEFAULT '',
    jev            text NOT NULL DEFAULT '',
    jev_confidence double precision NOT NULL DEFAULT 0,
    jev_error      text NOT NULL DEFAULT '',
    chosen         text NOT NULL DEFAULT '',
    mode           text NOT NULL DEFAULT '',
    outcome        text NOT NULL DEFAULT '',
    team           text NOT NULL DEFAULT '',
    CONSTRAINT decisions_key UNIQUE (point, subject, inputs_hash)
);
CREATE INDEX decisions_point_at ON decisions (point, at);

-- Observed durations for the deadline arithmetic: kind ci (key: repo) or stage (key: team/phase).
CREATE TABLE durations (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind  text NOT NULL,
    key   text NOT NULL,
    value interval NOT NULL,
    at    timestamptz NOT NULL
);
CREATE INDEX durations_kind_key_at ON durations (kind, key, at);
