-- A mode switch must not authorize setup with the previous mode's code.
ALTER TABLE setup_codes ADD COLUMN purpose text NOT NULL DEFAULT 'identity'
    CHECK (purpose IN ('identity', 'password'));
