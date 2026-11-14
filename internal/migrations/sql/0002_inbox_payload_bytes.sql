-- Webhook payloads are stored as the bytes GitHub sent (review 1, finding 6a). jsonb refused a payload
-- with a \u0000 escape (SQLSTATE 22P05: it can't hold NUL in text), so the receiver answered 503 to that
-- delivery every time GitHub retried it. Nothing queries into payloads: the leader decodes them in Go.
ALTER TABLE inbox ALTER COLUMN payload TYPE bytea USING convert_to(payload::text, 'UTF8');
