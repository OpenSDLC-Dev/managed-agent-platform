-- Per-session gate tokens (docs/plan/12_vaults-credentials.md slice 4, #50): the
-- scoped bearer credential a session's egress gate presents to the controlplane
-- internal gate-config endpoint to fetch its networking policy and decrypted
-- credentials. Only the hash is stored (the environment_keys precedent). A token
-- is valid for the life of its session — the gate container holds it and
-- re-fetches periodically — so there is no wall-clock expiry: validity ends when
-- the session is archived (the authenticate join guards archived_at), when the
-- row is revoked (a replacement gate re-mints, revoking its predecessor), or when
-- the session is deleted (cascade). Keeping the token session-long, not
-- wall-clock-short, avoids a controlplane outage longer than a TTL being
-- misread as a revocation.
CREATE TABLE session_gate_tokens (
    id          text PRIMARY KEY,
    session_id  text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    token_hash  text NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

-- At most one live token per session; revoke-on-re-mint keeps it to one.
CREATE UNIQUE INDEX session_gate_tokens_one_live
    ON session_gate_tokens (session_id) WHERE revoked_at IS NULL;
