-- Named, individually revocable environment keys (#43, plan 30). Issuance moves
-- off rotate-on-mint — EnsureEnvironmentKey made a supplied value the *one* live
-- credential for an environment, revoking whatever it replaced — and onto the
-- model the reference console implements: the operator generates one key per
-- host, names it so the hosts can be told apart, and revokes that one key without
-- disturbing the others. "Generate one per host so you can revoke access
-- individually" is the reference's own copy, and it cannot coexist with a
-- one-live-credential-per-environment invariant.

-- Rows minted before this are grandfathered, not backfilled: they get no name
-- (the console renders "—") and no expiry. Dating an existing key's expiry from
-- its created_at would retro-expire credentials that are authenticating right
-- now — a migration must not revoke a worker's key on its way past.
ALTER TABLE environment_keys ADD COLUMN name text NOT NULL DEFAULT '';
ALTER TABLE environment_keys ADD COLUMN expires_at timestamptz;

-- 0013 made a second live credential per environment unrepresentable. That was
-- the right invariant under rotate-on-mint and is the wrong one now: several live
-- keys per environment is the feature. Dropping it does not reopen the race 0013
-- closed (#72) — that race was two mints silently sharing one slot neither knew
-- was shared, and per-key issuance has no shared slot. key_hash's UNIQUE
-- constraint still binds one key value to one environment for life. The api_keys
-- half of 0013 stands: management keys keep rotation-by-restart semantics, so
-- api_keys_one_live is deliberately left alone.
DROP INDEX IF EXISTS environment_keys_one_live;

-- Dropping it also drops the only index on environment_id: 0001 created none, and
-- 0013's partial unique index had been serving both the per-environment lookup and
-- the ON DELETE CASCADE from environments. 0013 declined a companion non-partial
-- index because the table then held one row per credential *rotation* — an
-- operator-paced event. It now holds one row per host plus its revocation history,
-- so take the companion index after all, following session_gate_tokens_session_idx
-- (0012).
CREATE INDEX environment_keys_environment_idx ON environment_keys (environment_id);
