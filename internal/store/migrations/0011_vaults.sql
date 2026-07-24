-- Vaults + vault credentials (docs/plan/12_vaults-credentials.md slice 2, #50):
-- the /v1/vaults management surface. Secret material never lands here in the
-- clear — it is sealed through internal/secrets (ciphertext + the key id that
-- produced it); everything else is the wire-visible resource state.

CREATE TABLE vaults (
    id           text PRIMARY KEY,
    -- Reserved multi-tenant scope columns, single-tenant defaults (CLAUDE.md
    -- principle 5), as on every resource table.
    org_id       text NOT NULL DEFAULT 'default',
    workspace_id text NOT NULL DEFAULT 'default',
    project_id   text NOT NULL DEFAULT 'default',
    display_name text NOT NULL,
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    archived_at  timestamptz
);

CREATE TABLE vault_credentials (
    id           text PRIMARY KEY,
    vault_id     text NOT NULL REFERENCES vaults(id) ON DELETE CASCADE,
    -- The one nullable top-level wire field on the credential resource.
    display_name text,
    auth_type    text NOT NULL CHECK (auth_type IN ('mcp_oauth', 'static_bearer', 'environment_variable')),
    -- The non-secret auth surface exactly as rendered on the wire (the union
    -- under "type"); write-only fields never enter this document.
    auth         jsonb NOT NULL,
    -- The variant's write-only secret fields, marshaled as one JSON object and
    -- sealed through internal/secrets. NULL after archive (the docs' "secrets
    -- are purged; records are retained") or when a variant carries no secret.
    secret_ciphertext bytea,
    secret_key_id     text,
    -- The uniqueness anchor among ACTIVE credentials of a vault, namespaced by
    -- its wire field: 'url:<mcp_server_url>' (mcp_oauth and static_bearer
    -- share the field, so they share the namespace) or 'name:<secret_name>'.
    cred_key     text NOT NULL,
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    archived_at  timestamptz
);

-- A duplicate active key is a 409; archiving frees the key for re-use.
CREATE UNIQUE INDEX vault_credentials_active_key
    ON vault_credentials (vault_id, cred_key) WHERE archived_at IS NULL;
-- List + per-vault cap checks scan by vault.
CREATE INDEX vault_credentials_vault_idx ON vault_credentials (vault_id);
