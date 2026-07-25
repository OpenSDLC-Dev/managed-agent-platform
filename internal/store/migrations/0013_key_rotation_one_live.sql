-- One live credential per rotation slot (#72). EnsureAPIKey and
-- EnsureEnvironmentKey each revoke the incumbent and register the replacement in
-- one transaction, but under READ COMMITTED concurrent mints cannot see each
-- other's uncommitted insert, so each revokes nothing and all of them commit —
-- leaving a name, or an environment's work queue, with several live credentials
-- that nobody who minted one knows about. Ordering the two statements fixes the
-- rotation paths; the index makes the state unrepresentable, so the invariant
-- also holds for the operator issuance surface (#43) and any hand-run statement.
-- The session_gate_tokens_one_live precedent (0012).

-- An existing database may already hold the duplicates the index forbids: the
-- race has been reachable since 0001. Every migration runs inside the migrator's
-- single transaction, so an unrepaired duplicate would fail the whole startup
-- migration rather than just this statement — collapse them first, keeping the
-- newest live row per slot, which is the one a mint would have left live had the
-- race not happened.
UPDATE api_keys SET revoked_at = now()
 WHERE revoked_at IS NULL
   AND id NOT IN (SELECT DISTINCT ON (name) id
                    FROM api_keys WHERE revoked_at IS NULL
                   ORDER BY name, created_at DESC, id DESC);

UPDATE environment_keys SET revoked_at = now()
 WHERE revoked_at IS NULL
   AND id NOT IN (SELECT DISTINCT ON (environment_id) id
                    FROM environment_keys WHERE revoked_at IS NULL
                   ORDER BY environment_id, created_at DESC, id DESC);

-- At most one live key per logical name / per environment. No companion
-- non-partial index (unlike session_gate_tokens_session_idx): these tables hold
-- one row per credential *rotation* — an operator-paced event, not a per-session
-- one — so the environment_keys cascade has no history to seq-scan at scale.
CREATE UNIQUE INDEX api_keys_one_live ON api_keys (name) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX environment_keys_one_live ON environment_keys (environment_id) WHERE revoked_at IS NULL;
