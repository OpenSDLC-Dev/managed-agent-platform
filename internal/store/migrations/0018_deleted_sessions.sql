-- Tombstones for deleted sessions (plan 24 slice 3, review hardening). The
-- reaper's deleted tier destroys a sandbox and its checkpoint blob, so it must
-- run on affirmative evidence that THIS deployment deleted the session — a
-- missing sessions row alone also describes a holding that was never ours (a
-- second deployment sharing the Docker daemon or K8s namespace, a contract
-- suite's fixtures on a shared dev daemon). The environment kind rides along
-- because the session row is gone by the time the reaper asks, and only a
-- cloud session's sandbox is the platform's to destroy — a self_hosted one
-- belongs to the customer's BYOC worker even on a shared daemon.
-- deleteSession writes the tombstone in the same transaction that removes the
-- row; the rows are three small columns and are kept indefinitely.
CREATE TABLE deleted_sessions (
    id               text        PRIMARY KEY,
    environment_kind text        NOT NULL CHECK (environment_kind IN ('cloud', 'self_hosted')),
    deleted_at       timestamptz NOT NULL DEFAULT now()
);
