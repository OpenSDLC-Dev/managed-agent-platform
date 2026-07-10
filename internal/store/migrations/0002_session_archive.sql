-- 0002_session_archive: sessions carry archived_at on the wire (nullable,
-- null while active), confirmed against the SDK's BetaManagedAgentsSession.
-- 0001 predates that confirmation; slice 2's archive endpoint needs it.

ALTER TABLE sessions ADD COLUMN archived_at timestamptz;
