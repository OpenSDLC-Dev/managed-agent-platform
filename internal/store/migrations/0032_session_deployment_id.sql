-- Slice 3 of scheduled deployments (docs/plan/37_scheduled-deployments.md,
-- #51): the session side of a deployment fire, and the durable success marker
-- the run table was missing (#520).

-- "Deployment ID when the session was created from a deployment reference.
-- Null otherwise." Written only by a fire — POST /run now, the scheduler in
-- slice 4 — never by POST /v1/sessions, whose create params carry no
-- deployment field (docs/DIVERGENCES.md, the fired-only entry). No ON DELETE
-- action: nothing serves DELETE /v1/deployments, so the referenced row can
-- only ever be archived, which breaks no reference.
ALTER TABLE sessions ADD COLUMN deployment_id text REFERENCES deployments(id);

-- The sessions list's deployment_id filter, newest-first and keyset-paged like
-- the list itself, so the index carries the list's ordering under the filter
-- column. Partial: almost every session is deployment-less, and a filter query
-- says `deployment_id = $n`, which implies the predicate (0031's rule: a query
-- that wants a partial index must imply its WHERE).
CREATE INDEX sessions_deployment_idx
    ON sessions (deployment_id, created_at DESC, id DESC)
    WHERE deployment_id IS NOT NULL;

-- The durable success marker (#520). session_id is ON DELETE SET NULL, so
-- deleting a session would otherwise leave a committed run with neither arm
-- set and pull a schedule's last_run_at backwards — "the most recent scheduled
-- run actually started" must stay true about a run that did start. Read
-- queries and the wire's success arm key off this instant, never off the
-- session link, which is now what it reads as: a link that may go stale.
ALTER TABLE deployment_runs ADD COLUMN succeeded_at timestamptz;

-- A run settles exactly once: success and failure are mutually exclusive.
-- (succeeded_at alone cannot be CHECKed against session_id — the session link
-- is designed to outlive it going null.)
ALTER TABLE deployment_runs ADD CONSTRAINT deployment_runs_settled_once
    CHECK (succeeded_at IS NULL OR error_type IS NULL);
