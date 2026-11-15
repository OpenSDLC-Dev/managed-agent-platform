-- Expand before contract: old gateway replicas keep writing usage_daily by
-- key. Keep that compatibility rollup until the last old writer is retired.
-- The new canonical rollup groups owned calls by principal, across keys.
-- Historical rows retain their original key attribution; created_by never
-- supplies ownership. No FK can erase usage when a credential is removed.
ALTER TABLE modelgateway.usage ADD COLUMN principal_id text;
ALTER TABLE modelgateway.usage_daily ADD COLUMN principal_id text;
CREATE INDEX usage_principal_idx ON modelgateway.usage (principal_id, id)
    WHERE principal_id IS NOT NULL;

CREATE TABLE modelgateway.usage_actor_daily (
    day date NOT NULL,
    actor_id text NOT NULL,
    principal_id text,
    api_key_id text NOT NULL,
    alias text NOT NULL,
    deployment_id text NOT NULL,
    requests bigint NOT NULL DEFAULT 0,
    errors bigint NOT NULL DEFAULT 0,
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    cache_write_tokens bigint NOT NULL DEFAULT 0,
    cache_read_tokens bigint NOT NULL DEFAULT 0,
    cost numeric NOT NULL DEFAULT 0,
    PRIMARY KEY (day, actor_id, alias, deployment_id)
);

INSERT INTO modelgateway.usage_actor_daily
    SELECT day, api_key_id, NULL, api_key_id, alias, deployment_id, requests,
           errors, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost
    FROM modelgateway.usage_daily;

-- Both writer generations feed the canonical rollup in the ledger transaction.
-- Backfill from the old rollup, not the retained ledger: older ledger rows may
-- already have been swept. Never replay them into the backfill.
CREATE FUNCTION modelgateway.rollup_usage_actor() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO modelgateway.usage_actor_daily AS d
        (day, actor_id, principal_id, api_key_id, alias, deployment_id, requests,
         errors, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost)
    VALUES ((NEW.created_at AT TIME ZONE 'UTC')::date,
            coalesce(NEW.principal_id, NEW.api_key_id), NEW.principal_id,
            CASE WHEN NEW.principal_id IS NULL THEN NEW.api_key_id ELSE '' END,
            NEW.alias, NEW.deployment_id, 1, (NEW.error_type IS NOT NULL)::integer,
            coalesce(NEW.input_tokens,0), coalesce(NEW.output_tokens,0),
            coalesce(NEW.cache_write_tokens,0), coalesce(NEW.cache_read_tokens,0), coalesce(NEW.cost,0))
    ON CONFLICT (day, actor_id, alias, deployment_id) DO UPDATE SET
        requests = d.requests + EXCLUDED.requests,
        errors = d.errors + EXCLUDED.errors,
        input_tokens = d.input_tokens + EXCLUDED.input_tokens,
        output_tokens = d.output_tokens + EXCLUDED.output_tokens,
        cache_write_tokens = d.cache_write_tokens + EXCLUDED.cache_write_tokens,
        cache_read_tokens = d.cache_read_tokens + EXCLUDED.cache_read_tokens,
        cost = d.cost + EXCLUDED.cost;
    RETURN NEW;
END $$;
CREATE TRIGGER usage_actor_rollup AFTER INSERT ON modelgateway.usage
    FOR EACH ROW EXECUTE FUNCTION modelgateway.rollup_usage_actor();
