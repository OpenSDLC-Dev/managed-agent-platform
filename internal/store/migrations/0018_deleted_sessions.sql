-- Tombstones for deleted sessions (plan 24 slice 3, review hardening). The
-- reaper's deleted tier destroys a sandbox and its checkpoint blob, so it must
-- run on affirmative evidence that THIS deployment deleted the session — a
-- missing sessions row alone also describes a holding that was never ours (a
-- second deployment sharing the Docker daemon or K8s namespace, a contract
-- suite's fixtures on a shared dev daemon). deleteSession writes the tombstone
-- in the same transaction that removes the row; the rows are two small columns
-- and are kept indefinitely.
CREATE TABLE deleted_sessions (
    id         text        PRIMARY KEY,
    deleted_at timestamptz NOT NULL DEFAULT now()
);
