-- The model gateway's ledger and limits (docs/plan/59_model-gateway.md,
-- "Configuration model" and "Routing, retries, limits"): one row per request
-- a key was admitted for, a rollup per UTC day kept after those rows are
-- swept, and the one-minute windows RPM and TPM count in. Metadata only — no
-- prompt or answer is kept. None references the configuration by foreign key:
-- a key, an alias or a deployment may go while its history stays.

-- deployment_id and credential_id are the attempt that answered, or the last
-- one made when none did. The token counts are the upstream's own, NULL when
-- it reported none, and cost is theirs at the deployment's prices when the
-- row was written, NULL with them. created_at is when the request ended;
-- latency runs from its arrival to then, and ttft to a stream's first event,
-- in microseconds, so a request served within a millisecond does not read as
-- none.
CREATE TABLE modelgateway.usage (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id         text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    api_key_id         text NOT NULL,
    model              text NOT NULL,
    alias              text NOT NULL,
    deployment_id      text NOT NULL,
    credential_id      text NOT NULL,
    session_id         text,
    protocol           text NOT NULL,
    endpoint           text NOT NULL,
    status             integer NOT NULL,
    error_type         text,
    input_tokens       bigint,
    output_tokens      bigint,
    cache_write_tokens bigint,
    cache_read_tokens  bigint,
    cost               numeric,
    latency_us         bigint NOT NULL,
    ttft_us            bigint
);
CREATE INDEX usage_created_idx ON modelgateway.usage (created_at);
CREATE INDEX usage_key_idx ON modelgateway.usage (api_key_id, id);
CREATE INDEX usage_alias_idx ON modelgateway.usage (alias, id);
CREATE INDEX usage_deployment_idx ON modelgateway.usage (deployment_id, id);
CREATE INDEX usage_session_idx ON modelgateway.usage (session_id, id) WHERE session_id IS NOT NULL;

-- The ledger summed per UTC day, key, configured alias and deployment,
-- written with each usage row and never swept.
CREATE TABLE modelgateway.usage_daily (
    day                date NOT NULL,
    api_key_id         text NOT NULL,
    alias              text NOT NULL,
    deployment_id      text NOT NULL,
    requests           bigint NOT NULL DEFAULT 0,
    errors             bigint NOT NULL DEFAULT 0,
    input_tokens       bigint NOT NULL DEFAULT 0,
    output_tokens      bigint NOT NULL DEFAULT 0,
    cache_write_tokens bigint NOT NULL DEFAULT 0,
    cache_read_tokens  bigint NOT NULL DEFAULT 0,
    cost               numeric NOT NULL DEFAULT 0,
    PRIMARY KEY (day, api_key_id, alias, deployment_id)
);

-- A limited key's requests admitted, and tokens completed, in each minute of
-- the database's clock. Only the current minute is read; older ones are
-- swept.
CREATE TABLE modelgateway.rate_windows (
    api_key_id text NOT NULL,
    minute     timestamptz NOT NULL,
    requests   integer NOT NULL DEFAULT 0,
    tokens     bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (api_key_id, minute)
);
