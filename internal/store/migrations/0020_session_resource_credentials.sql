-- Session resource credentials (docs/plan/25_git-repo-mounting.md, decision 2 —
-- slice 1 of the git half of #55): the write-only authorization_token of a
-- github_repository session resource. The rendered resource stored in
-- sessions.resources is token-free by schema (session GET echoes that array
-- verbatim), so the secret lives here instead, sealed through internal/secrets
-- exactly as vault_credentials seals its material (0011): ciphertext + the key
-- id that produced it, never the clear value.
--
-- resource_id is the sesrsc_ id of the jsonb array element this row backs.
-- The session FK cascades: repo resources are immutable post-create (no add,
-- no delete — plan 25 decision 3), so the only way a credential row dies is
-- with its session (the #266 lesson: no orphaned secrets).

CREATE TABLE session_resource_credentials (
    resource_id      text PRIMARY KEY,
    session_id       text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    token_ciphertext bytea NOT NULL,
    token_key_id     text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- The executor's materialization pass loads a session's clone tokens by
-- session; the cascade delete walks the same key.
CREATE INDEX session_resource_credentials_session_idx
    ON session_resource_credentials (session_id);
