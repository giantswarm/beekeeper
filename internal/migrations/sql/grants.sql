-- Privileges for the roles the CloudNativePG operator creates (plan §5). Runs after every migration
-- (GRANT is idempotent), and only for the roles that exist, so a database without them migrates too.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'engine') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON inbox, events, decisions, durations TO engine;
        GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO engine;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'receiver') THEN
        -- Only INSERT on inbox: the receiver can't read deliveries back, or touch history.
        GRANT INSERT ON inbox TO receiver;
    END IF;
END
$$;
