-- Deployments and their run history (docs/plan/37_scheduled-deployments.md
-- slice 1, #51): the /v1/deployments management surface. A deployment binds an
-- agent to an environment, credentials and initial events, plus an optional
-- 5-field POSIX cron schedule that starts sessions on its own. Every attempt
-- is one immutable row in deployment_runs.
--
-- sessions.deployment_id is slice 3's own migration; nothing here references it.

CREATE TABLE deployments (
    id             text PRIMARY KEY,
    -- Reserved multi-tenant scope columns, single-tenant defaults (CLAUDE.md
    -- principle 5), as on every resource table. They are also what makes
    -- workspace_archived_error and organization_disabled_error admissible
    -- values below rather than nonsense: the scope exists, nothing enforces it
    -- yet, so nothing produces those two.
    org_id         text NOT NULL DEFAULT 'default',
    workspace_id   text NOT NULL DEFAULT 'default',
    project_id     text NOT NULL DEFAULT 'default',
    name           text NOT NULL,
    -- "Description of what the deployment does" — required on the wire, so
    -- empty string when unset, never null.
    description    text NOT NULL DEFAULT '',
    -- "A resolved agent reference with a concrete version": the version is
    -- pinned at write time, sessions' own discipline (0001).
    agent_id       text NOT NULL,
    agent_version  integer NOT NULL,
    environment_id text NOT NULL REFERENCES environments(id),
    vault_ids      text[] NOT NULL DEFAULT '{}',
    initial_events jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- Echoes the input minus write-only credentials; a github_repository
    -- token is sealed by the cipher before the write and stored as ciphertext.
    resources      jsonb NOT NULL DEFAULT '[]'::jsonb,
    metadata       jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- The schedule. Null means manual-only: "Presence enables scheduled
    -- execution; null means manual-only". Expression and timezone travel
    -- together or not at all.
    schedule_expression text,
    schedule_timezone   text,
    -- Written from the database clock at create and at every unpause, and read
    -- by the scheduler's candidate scan as a floor. It is what makes the
    -- reference's published "Unpause resumes the schedule from the next
    -- scheduled occurrence. Missed triggers are not backfilled" implementable:
    -- no run row advances the derived watermark while a deployment is paused,
    -- so without this column the first tick after an unpause would fire the
    -- occurrence that fell due during it (plan 37 §4.2). A plain restart
    -- writes nothing here, which is why catch-up still works across one.
    schedule_resumed_at timestamptz NOT NULL DEFAULT now(),

    -- status and paused_reason are rendered from these three, never stored.
    -- An archived deployment reports status "active" with archived_at set, so
    -- archive leaves them exactly as it found them.
    paused_at         timestamptz,
    paused_kind       text,
    paused_error_type text,

    -- Audit only: which API key or principal created the deployment. Never on
    -- the wire, never used for isolation — sessions.created_by's rule (0001).
    -- It matters more here than usual: a session a schedule fires has a NULL
    -- created_by, because no human asked for it, so this row is the only place
    -- the audit trail survives.
    created_by  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- Set once and never cleared: archiving is one-way, and this platform
    -- serves no unarchive and no DELETE /v1/deployments.
    archived_at timestamptz,

    FOREIGN KEY (agent_id, agent_version) REFERENCES agent_versions (agent_id, version),

    CONSTRAINT deployments_schedule_pair CHECK (
        (schedule_expression IS NULL) = (schedule_timezone IS NULL)
    ),
    -- Total rather than clever: every reachable combination is spelled out, so
    -- a NULL cannot make the constraint vacuously true.
    CONSTRAINT deployments_paused_shape CHECK (
        (paused_at IS NULL     AND paused_kind IS NULL     AND paused_error_type IS NULL)
        OR (paused_at IS NOT NULL AND paused_kind = 'manual' AND paused_error_type IS NULL)
        OR (paused_at IS NOT NULL AND paused_kind = 'error'  AND paused_error_type IS NOT NULL)
    ),
    -- All fourteen members of the reference's paused-reason union. Seven of
    -- them are produced by no path in this platform and are admitted anyway,
    -- so a future emitter needs no migration.
    CONSTRAINT deployments_paused_error_type CHECK (
        paused_error_type IS NULL OR paused_error_type IN (
            'agent_archived_error',
            'environment_archived_error',
            'environment_not_found_error',
            'file_not_found_error',
            'mcp_egress_blocked_error',
            'memory_store_archived_error',
            'organization_disabled_error',
            'self_hosted_resources_unsupported_error',
            'session_resource_not_found_error',
            'skill_not_found_error',
            'unknown_error',
            'vault_archived_error',
            'vault_not_found_error',
            'workspace_archived_error'
        )
    )
);

-- The list is newest-first, keyset-paged on (created_at, id).
CREATE INDEX deployments_created_idx ON deployments (created_at DESC, id DESC);
-- Every mutating path resolves the environment, and DELETE /v1/environments has
-- to name the deployments that block it.
CREATE INDEX deployments_environment_idx ON deployments (environment_id);
-- Archiving an agent is refused while a live deployment pins it, so that check
-- runs on every archive.
CREATE INDEX deployments_agent_idx ON deployments (agent_id);

-- One immutable record per attempt to start a session from a deployment.
--
-- The row does three jobs at once, and the third is the one to remember when
-- writing a pruning job: it is the history a client reads, it is the claim that
-- makes an occurrence fire at most once across replicas, AND it is the
-- watermark the scheduler derives instead of storing. Deleting a row newer than
-- the catch-up window un-claims its occurrence, and the next tick will fire it
-- again.
CREATE TABLE deployment_runs (
    id            text PRIMARY KEY,
    deployment_id text NOT NULL REFERENCES deployments(id),
    trigger_type  text NOT NULL,
    -- The exact cron match, in UTC, for a scheduled run; null for a manual one.
    --
    -- timestamptz, not timestamp, and the reason is a correctness one rather
    -- than a style one: on a fall-back day a wall clock occurs twice and both
    -- occurrences fire. Stored as a local wall clock the second 01:30 collides
    -- with the first on the unique index below and is silently swallowed — a
    -- wrong answer that looks exactly like a right one. Two distinct UTC
    -- instants are two rows.
    scheduled_at  timestamptz,
    -- The version actually fired, snapshotted like the deployment's own.
    agent_id      text NOT NULL,
    agent_version integer NOT NULL,
    -- The session this attempt created, or null when it failed before one
    -- existed. A committed row has exactly one of session_id and error_type
    -- set; that invariant is enforced by construction rather than by a CHECK,
    -- because the claim is inserted with both null inside the same transaction
    -- that then sets one (plan 37 §4.4).
    session_id    text REFERENCES sessions(id) ON DELETE SET NULL,
    error_type    text,
    error_message text,
    -- The instant the fire began — the row is inserted as the claim, before the
    -- session exists — and therefore the value a deployment's last_run_at
    -- reports for "the most recent scheduled run actually started".
    created_at    timestamptz NOT NULL DEFAULT now(),

    FOREIGN KEY (agent_id, agent_version) REFERENCES agent_versions (agent_id, version),

    CONSTRAINT deployment_runs_trigger_type CHECK (trigger_type IN ('schedule', 'manual')),
    -- A scheduled run always names its occurrence; a manual one never does.
    CONSTRAINT deployment_runs_scheduled_at CHECK (
        (trigger_type = 'schedule') = (scheduled_at IS NOT NULL)
    ),
    CONSTRAINT deployment_runs_error_pair CHECK (
        (error_type IS NULL) = (error_message IS NULL)
    ),
    -- All sixteen members of the run-error union: the fourteen a pause can
    -- carry, plus the two the pausing union omits.
    CONSTRAINT deployment_runs_error_type CHECK (
        error_type IS NULL OR error_type IN (
            'agent_archived_error',
            'environment_archived_error',
            'environment_not_found_error',
            'file_not_found_error',
            'mcp_egress_blocked_error',
            'memory_store_archived_error',
            'organization_disabled_error',
            'self_hosted_resources_unsupported_error',
            'session_creation_rejected_error',
            'session_rate_limited_error',
            'session_resource_not_found_error',
            'skill_not_found_error',
            'unknown_error',
            'vault_archived_error',
            'vault_not_found_error',
            'workspace_archived_error'
        )
    )
);

-- The occurrence claim, and the reference's own published idempotency key:
-- "At most one run is recorded per (deployment_id, scheduled_at) pair". Partial
-- because a manual run has no scheduled_at, and while Postgres admits unlimited
-- NULLs in a plain unique index, the partial form spells the intent.
--
-- Not CONCURRENTLY, and it cannot be: migrations run inside one transaction.
--
-- The predicate is also load-bearing at read time. Postgres will not use a
-- partial index unless a query's own WHERE implies its predicate, so every
-- query that wants this index has to write `scheduled_at IS NOT NULL` out even
-- where the rest of its filter already makes it redundant.
CREATE UNIQUE INDEX deployment_runs_occurrence_idx
    ON deployment_runs (deployment_id, scheduled_at)
    WHERE scheduled_at IS NOT NULL;

-- The runs list is newest-first, keyset-paged, and usually scoped to one
-- deployment.
CREATE INDEX deployment_runs_created_idx ON deployment_runs (created_at DESC, id DESC);
