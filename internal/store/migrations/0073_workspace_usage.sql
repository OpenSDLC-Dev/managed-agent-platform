-- Preserve totals for retained and swept requests when splitting the rollups.
ALTER TABLE modelgateway.usage_daily DROP CONSTRAINT usage_daily_pkey,
    ADD PRIMARY KEY (workspace_id, day, api_key_id, alias, deployment_id);
ALTER TABLE modelgateway.usage_actor_daily DROP CONSTRAINT usage_actor_daily_pkey,
    ADD PRIMARY KEY (workspace_id, day, actor_id, alias, deployment_id);
CREATE INDEX usage_workspace_cursor_idx ON modelgateway.usage (workspace_id, id);
ALTER TABLE workspaces ADD COLUMN created_at timestamptz NOT NULL DEFAULT statement_timestamp();

CREATE OR REPLACE FUNCTION modelgateway.rollup_usage_actor() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO modelgateway.usage_actor_daily AS d
        (workspace_id, day, actor_id, principal_id, api_key_id, alias, deployment_id, requests,
         errors, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost)
    VALUES (NEW.workspace_id, (NEW.created_at AT TIME ZONE 'UTC')::date,
            coalesce(NEW.principal_id, NEW.api_key_id), NEW.principal_id,
            CASE WHEN NEW.principal_id IS NULL THEN NEW.api_key_id ELSE '' END,
            NEW.alias, NEW.deployment_id, 1, (NEW.error_type IS NOT NULL)::integer,
            coalesce(NEW.input_tokens,0), coalesce(NEW.output_tokens,0),
            coalesce(NEW.cache_write_tokens,0), coalesce(NEW.cache_read_tokens,0), coalesce(NEW.cost,0))
    ON CONFLICT (workspace_id, day, actor_id, alias, deployment_id) DO UPDATE SET
        requests = d.requests + EXCLUDED.requests, errors = d.errors + EXCLUDED.errors,
        input_tokens = d.input_tokens + EXCLUDED.input_tokens, output_tokens = d.output_tokens + EXCLUDED.output_tokens,
        cache_write_tokens = d.cache_write_tokens + EXCLUDED.cache_write_tokens,
        cache_read_tokens = d.cache_read_tokens + EXCLUDED.cache_read_tokens, cost = d.cost + EXCLUDED.cost;
    RETURN NEW;
END $$;
