-- The model gateway's configuration (docs/plan/59_model-gateway.md): which
-- vendor accounts it calls, with which keys, for which models, under which
-- names, and which platform API keys may call them. In a schema of its own so
-- that nothing the gateway owns is mistaken for the platform's, and nothing in
-- the platform reads it; the gateway reads the platform only through api_keys.
-- Its queries live in internal/modelgateway/store.
--
-- providers, deployments and aliases are top-level and reserve the scope
-- columns, as the platform's top-level tables do (store's package comment);
-- credentials and alias_targets inherit scope through their parent, and
-- key_policies through the api key it grants.
CREATE SCHEMA modelgateway;

-- One vendor account behind fixed endpoints. profile names a compiled-in
-- vendor profile (internal/modelgateway/profile); each protocol's base URL is
-- either one of the profile's hosts or a custom one. The endpoints are fixed at
-- creation: thinking provenance and an embedding index key on a deployment, and
-- a deployment's provider must keep meaning one account behind one endpoint.
CREATE TABLE modelgateway.providers (
    id                 text PRIMARY KEY,
    org_id             text NOT NULL DEFAULT 'default',
    workspace_id       text NOT NULL DEFAULT 'default',
    project_id         text NOT NULL DEFAULT 'default',
    name               text NOT NULL CHECK (name <> ''),
    profile            text NOT NULL,
    anthropic_base_url text,
    openai_base_url    text,
    headers            jsonb NOT NULL DEFAULT '{}',
    stall_timeout_ms   integer CHECK (stall_timeout_ms > 0),
    enabled            boolean NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (anthropic_base_url IS NOT NULL OR openai_base_url IS NOT NULL)
);

-- A provider's key, sealed by internal/secrets as the vault credentials are.
-- Never returned: last_four is all the API shows of it.
CREATE TABLE modelgateway.credentials (
    id          text PRIMARY KEY,
    provider_id text NOT NULL REFERENCES modelgateway.providers (id) ON DELETE CASCADE,
    kind        text NOT NULL DEFAULT 'api_key' CHECK (kind IN ('api_key')),
    ciphertext  bytea NOT NULL,
    key_id      text NOT NULL,
    last_four   text NOT NULL DEFAULT '',
    protocols   text[] NOT NULL
        CHECK (cardinality(protocols) > 0 AND protocols <@ ARRAY['anthropic', 'openai']),
    weight      integer NOT NULL DEFAULT 1 CHECK (weight > 0),
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX credentials_provider_idx ON modelgateway.credentials (provider_id);

-- One upstream model on one provider. provider_id, upstream_model and kind
-- never change, so a deployment id names one model on one account. Prices are
-- per million tokens, entered by the operator; a missing one costs nothing.
-- No cascade from providers: a provider with deployments cannot be deleted.
CREATE TABLE modelgateway.deployments (
    id                text PRIMARY KEY,
    org_id            text NOT NULL DEFAULT 'default',
    workspace_id      text NOT NULL DEFAULT 'default',
    project_id        text NOT NULL DEFAULT 'default',
    provider_id       text NOT NULL REFERENCES modelgateway.providers (id),
    upstream_model    text NOT NULL CHECK (upstream_model <> ''),
    kind              text NOT NULL CHECK (kind IN ('chat', 'embedding', 'rerank')),
    display_name      text NOT NULL DEFAULT '',
    capabilities      jsonb NOT NULL DEFAULT '{}',
    price_input       numeric CHECK (price_input >= 0),
    price_output      numeric CHECK (price_output >= 0),
    price_cache_write numeric CHECK (price_cache_write >= 0),
    price_cache_read  numeric CHECK (price_cache_read >= 0),
    enabled           boolean NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deployments_provider_idx ON modelgateway.deployments (provider_id);

-- The model name a caller sends; '*' is the optional wildcard. kind is the
-- kind of every target, fixed at creation.
CREATE TABLE modelgateway.aliases (
    name         text PRIMARY KEY CHECK (name <> ''),
    org_id       text NOT NULL DEFAULT 'default',
    workspace_id text NOT NULL DEFAULT 'default',
    project_id   text NOT NULL DEFAULT 'default',
    display_name text NOT NULL DEFAULT '',
    kind         text NOT NULL CHECK (kind IN ('chat', 'embedding', 'rerank')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- An alias's targets: groups are tried in ascending priority, and within a
-- group a deployment is chosen by weight. No cascade from deployments: a
-- deployment an alias routes to cannot be deleted.
CREATE TABLE modelgateway.alias_targets (
    alias         text NOT NULL REFERENCES modelgateway.aliases (name) ON DELETE CASCADE,
    deployment_id text NOT NULL REFERENCES modelgateway.deployments (id),
    priority      integer NOT NULL DEFAULT 0 CHECK (priority >= 0),
    weight        integer NOT NULL DEFAULT 1 CHECK (weight > 0),
    PRIMARY KEY (alias, deployment_id)
);
CREATE INDEX alias_targets_deployment_idx ON modelgateway.alias_targets (deployment_id);

-- The grant that lets a platform API key call models: an alias allow-list
-- (NULL is every alias) and per-minute request and token limits (NULL is no
-- limit). A key without a row calls nothing, the bootstrap and brain keys
-- excepted by their configured values. Issuance, status and expiry stay
-- api_keys'.
CREATE TABLE modelgateway.key_policies (
    api_key_id text PRIMARY KEY REFERENCES api_keys (id) ON DELETE CASCADE,
    aliases    text[],
    rpm        integer CHECK (rpm > 0),
    tpm        bigint CHECK (tpm > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
