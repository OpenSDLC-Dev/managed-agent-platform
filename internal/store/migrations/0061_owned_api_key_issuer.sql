-- Activate with the owned HTTP issuer. Old replicas may retain prepared INSERTs;
-- refuse their human-unowned writes without changing any historical row.
-- The prefix identifies the old human issuer lane, including dangling IDs. It
-- is not evidence of ownership: owner_principal_id must be explicitly supplied.
CREATE FUNCTION api_keys_require_human_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_principal_id IS NULL AND starts_with(NEW.created_by, 'principal_') THEN
        RAISE EXCEPTION 'human-issued API keys require an explicit owner'
            USING ERRCODE = '23514', CONSTRAINT = 'api_keys_human_issuer_requires_owner';
    END IF;
    RETURN NEW;
END $$;

-- INSERT only: legacy metadata changes, machine issuers and null preset issuers
-- remain valid. Migration 0060 still enforces credential identity and shape.
CREATE TRIGGER api_keys_human_issuer_requires_owner BEFORE INSERT ON api_keys
    FOR EACH ROW EXECUTE FUNCTION api_keys_require_human_owner();
