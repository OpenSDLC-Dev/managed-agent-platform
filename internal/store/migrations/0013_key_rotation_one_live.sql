-- One live credential per rotation slot (#72). EnsureAPIKey and
-- EnsureEnvironmentKey each revoke the incumbent and register the replacement in
-- one transaction, but under READ COMMITTED concurrent mints cannot see each
-- other's uncommitted insert, so each revokes nothing and all of them commit —
-- leaving a name, or an environment's work queue, with several live credentials
-- that nobody who minted one knows about. Ordering the two statements fixes the
-- rotation paths; the index makes the state unrepresentable, so the invariant
-- also holds for the operator issuance surface (#43) and any hand-run statement.
-- The session_gate_tokens_one_live precedent (0012).

-- Shut out credential writers for the rest of this transaction. The migrator's
-- advisory lock only serializes other migrators, so without this a not-yet-
-- upgraded replica could mint a duplicate between the repair below and the index
-- build — and because every migration runs inside the migrator's single
-- transaction, that failing index build aborts the whole startup migration, not
-- just this statement, leaving upgraded replicas unable to boot. SHARE is what
-- CREATE INDEX takes anyway; taking it up front only moves it earlier. Plain
-- SELECT and SELECT FOR UPDATE are unaffected, so authentication keeps working.
LOCK TABLE api_keys, environment_keys IN SHARE MODE;

-- An existing database may already hold the duplicates the index forbids: the
-- race has been reachable since 0001. Collapse them, keeping the newest live row
-- per slot — the one a mint would have left live had the race not happened. Note
-- for the upgrade: the rows this revokes are credentials that were authenticating
-- until now, and they stop working immediately.
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
--
-- api_keys' slot is bare `name`, not (org_id, workspace_id, project_id, name):
-- that promotes EnsureAPIKey's existing revoke-by-name semantics to a constraint
-- rather than inventing a scope it does not implement. When the reserved tenancy
-- columns become real scoping (principle 5), this index is rebuilt with them.
-- environment_keys needs no such choice — environment_id is globally unique.
CREATE UNIQUE INDEX api_keys_one_live ON api_keys (name) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX environment_keys_one_live ON environment_keys (environment_id) WHERE revoked_at IS NULL;
