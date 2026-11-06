-- Plan 69 slice 3 storage preparation. Existing keys stay unowned: created_by
-- is the issuer's audit identifier, never evidence of ownership. Owned digests
-- live outside key_hash so even a legacy prepared authentication query cannot
-- grant them unowned authority. Keep key_hash's UNIQUE constraint for old seeds
-- using ON CONFLICT(key_hash); the expression index also spans both lanes.
ALTER TABLE api_keys
    ADD COLUMN owner_principal_id text REFERENCES principals(id),
    ADD COLUMN owned_key_hash text,
    ALTER COLUMN key_hash DROP NOT NULL;

ALTER TABLE api_keys ADD CONSTRAINT api_keys_credential_shape CHECK (
    (owner_principal_id IS NULL AND key_hash IS NOT NULL AND owned_key_hash IS NULL)
    OR
    (owner_principal_id IS NOT NULL AND key_hash IS NULL AND owned_key_hash IS NOT NULL
        AND created_by IS NOT NULL)
);

CREATE UNIQUE INDEX api_keys_authentication_hash_key
    ON api_keys (coalesce(key_hash, owned_key_hash));

-- A key's identity cannot move between owners or authentication lanes. Mutable
-- lifecycle/name metadata and old seed updates to unowned rows still work.
CREATE FUNCTION api_keys_refuse_credential_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.owner_principal_id, NEW.key_hash, NEW.owned_key_hash)
        IS DISTINCT FROM (OLD.owner_principal_id, OLD.key_hash, OLD.owned_key_hash) THEN
        RAISE EXCEPTION 'API key credential identity is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'api_keys_credential_immutable';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER api_keys_credential_immutable BEFORE UPDATE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION api_keys_refuse_credential_change();

-- Old member/SA removal SQL must archive owned keys in its own transaction.
-- Restoration does not restore keys, and removing an issuer affects no key
-- owned by someone else. The future owned HTTP issuer must share the explicit
-- READ COMMITTED principal-first mutation fence in api.beginPrincipalMutation.
-- This batch keeps legacy human INSERTs valid until that issuer replaces them.
CREATE FUNCTION principals_archive_owned_keys() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE api_keys SET status = 'archived'
        WHERE owner_principal_id = NEW.id AND status <> 'archived';
    RETURN NEW;
END $$;

CREATE TRIGGER principals_archive_owned_keys AFTER UPDATE OF status ON principals
    FOR EACH ROW WHEN (OLD.status = 'active' AND NEW.status = 'deactivated')
    EXECUTE FUNCTION principals_archive_owned_keys();

-- Reject old policy writers too, not just a new gateway's HTTP handler. Refuse
-- a target this statement cannot see before the FK can wait/recheck visibility.
-- KEY SHARE keeps a visible target present; credential immutability above keeps
-- its ownership fixed. Neither an uncommitted nor an owned target gets a grant.
CREATE FUNCTION modelgateway.key_policy_require_unowned() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    owner_id text;
BEGIN
    SELECT owner_principal_id INTO owner_id FROM api_keys
        WHERE id = NEW.api_key_id FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'API key policy target is not visible'
            USING ERRCODE = '23503', CONSTRAINT = 'modelgateway_key_policy_target';
    END IF;
    IF owner_id IS NOT NULL THEN
        RAISE EXCEPTION 'principal-owned API keys cannot have legacy model grants'
            USING ERRCODE = '23514', CONSTRAINT = 'modelgateway_key_policy_unowned';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER modelgateway_key_policy_unowned BEFORE INSERT OR UPDATE ON modelgateway.key_policies
    FOR EACH ROW EXECUTE FUNCTION modelgateway.key_policy_require_unowned();
