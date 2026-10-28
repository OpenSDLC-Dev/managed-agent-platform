-- Private member/service-account management metadata (plan 69 slice 2).
-- Audit references retain internal principal_ IDs; HTTP renders kind aliases.
-- Deactivation preserves the issuer/subject tombstone and its reserved name.
ALTER TABLE principals
    ADD COLUMN created_by text REFERENCES principals(id),
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN updated_by text REFERENCES principals(id),
    ADD COLUMN deactivated_at timestamptz,
    ADD COLUMN deactivated_by text REFERENCES principals(id);
