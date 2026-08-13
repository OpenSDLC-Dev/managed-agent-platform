-- Named, expiring, individually manageable management keys (#378, plan 32).
-- api_keys has held one live credential per logical name since 0001, rotated by
-- restarting cmd/controlplane with a new CONTROLPLANE_API_KEY. That is the right
-- model for an env-var-managed credential and the wrong one for keys an admin
-- issues from a console: the reference lets several live keys share a name, gives
-- each an expiry, and retires one without touching the others.
--
-- 0021 made exactly this move for environment_keys and said of this table: "The
-- api_keys half of 0013 stands: management keys keep rotation-by-restart
-- semantics, so api_keys_one_live is deliberately left alone." That was correct
-- while nothing could issue a management key. Plan 32 makes something that can,
-- so the invariant is not dropped but *narrowed* — see the index at the bottom.

-- status is the single answer to "may this key authenticate", replacing
-- revoked_at. Three values, matching the reference's own settable enum (its 400
-- reads: "status: Input should be 'active', 'inactive' or 'archived'"): active
-- authenticates; inactive is a reversible disable; archived is what the
-- reference's "Delete" does, and it is not a row deletion — an archived key stays
-- readable so an operator can still see that it existed.
--
-- A fourth state the reference reports, `expired`, is deliberately NOT stored: it
-- is computed from expires_at at read time. Storing it would need a sweeper to
-- keep true, and a key that expires while the sweeper is down would keep
-- authenticating — the failure mode a credential store must not have.
ALTER TABLE api_keys ADD COLUMN status text NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'inactive', 'archived'));

-- Every revoked row becomes archived: revocation was one-way, and archived is the
-- one-way state. This runs before revoked_at is dropped, so no information is
-- lost that anything reads. The revocation *timestamp* is not preserved — nothing
-- queries it, the reference exposes no equivalent field, and keeping a column that
-- no longer decides anything would leave two answers to "is this key live".
UPDATE api_keys SET status = 'archived' WHERE revoked_at IS NOT NULL;
ALTER TABLE api_keys DROP COLUMN revoked_at;

-- Nullable, and absent means never expires. The reference's console omits the
-- field entirely for its "Never" choice rather than sending null, and reports
-- expires_at: null on the way back; both spellings arrive here as NULL.
--
-- Existing rows are grandfathered rather than backfilled, for the reason 0021
-- gives: dating an expiry from created_at would retro-expire the credential the
-- deployment is authenticating with right now, and a migration must not lock an
-- operator out on its way past.
ALTER TABLE api_keys ADD COLUMN expires_at timestamptz;

-- The masked value a listing shows. It cannot be derived — key_hash is a SHA-256
-- and the plaintext is never stored — so it is kept beside the hash, and it is
-- the ONLY part of a key that survives issuance. Empty for rows that predate this
-- migration: their plaintext is gone, and inventing a hint would be a lie about
-- which key a row is.
ALTER TABLE api_keys ADD COLUMN partial_key_hint text NOT NULL DEFAULT '';

-- Who issued this key: a principal_ id when a human did it over SSO, an apikey_
-- id when a machine credential did, NULL when the process seeded it from
-- CONTROLPLANE_API_KEY. No foreign key, deliberately: a principal may be removed
-- from the IdP while the keys they issued keep working, which is the rule the
-- reference states outright ("API keys are owned by workspaces and remain active
-- even after the creator is removed"). A cascade here would delete credentials
-- that a fleet is still authenticating with.
ALTER TABLE api_keys ADD COLUMN created_by text;

-- The invariant, narrowed rather than dropped.
--
-- EnsureAPIKey needs at most one live key per logical name, or replicas booting
-- with different values for the same name would each register one and the name
-- would silently carry two live credentials (the race 0013 closed, #72). Console-
-- issued keys need the opposite: the reference allows duplicate names, verified
-- live on 2026-08-13 by creating two keys named the same and watching both come
-- back 200 and both stay active.
--
-- The two are told apart by created_by rather than by a literal name: a row
-- nobody issued is env-var-managed, and those are exactly the rows EnsureAPIKey
-- owns. Keying the index on the *semantics* instead of on the string 'bootstrap'
-- keeps the schema from depending on a constant that lives in cmd/controlplane,
-- and keeps the guarantee for every name EnsureAPIKey is ever called with.
DROP INDEX IF EXISTS api_keys_one_live;
CREATE UNIQUE INDEX api_keys_one_live_unissued ON api_keys (name)
    WHERE status = 'active' AND created_by IS NULL;

-- No index for the listing here on purpose. Nothing sorts or filters this table
-- by created_at yet -- the two reads in the tree are keyed on name and key_hash --
-- and the listing that would want one lands in slice 2. A migration is immutable
-- once merged, so an index taken a slice early is a permanent bet on a query that
-- does not exist.
