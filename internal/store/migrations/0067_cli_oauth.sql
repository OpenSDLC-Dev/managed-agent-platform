CREATE TABLE cli_oauth_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id text NOT NULL REFERENCES principals(id),
    client_id text NOT NULL,
    scopes text[] NOT NULL,
    code_hash text UNIQUE,
    challenge text NOT NULL,
    state_hash text NOT NULL,
    redirect_uri text NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked boolean NOT NULL DEFAULT false
);
CREATE INDEX cli_oauth_grants_principal_idx ON cli_oauth_grants(principal_id);
CREATE INDEX cli_oauth_grants_expiry_idx ON cli_oauth_grants(expires_at);

-- Keep spent refresh hashes for the remaining grant lifetime so a replay can
-- retire the whole grant. The access and refresh values themselves never persist.
CREATE TABLE cli_oauth_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    grant_id uuid NOT NULL REFERENCES cli_oauth_grants(id) ON DELETE CASCADE,
    access_hash text NOT NULL UNIQUE,
    refresh_hash text NOT NULL UNIQUE,
    access_expires_at timestamptz NOT NULL,
    spent boolean NOT NULL DEFAULT false
);
CREATE INDEX cli_oauth_tokens_grant_idx ON cli_oauth_tokens(grant_id);

CREATE FUNCTION revoke_cli_oauth_on_removal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE cli_oauth_grants SET revoked = true
    WHERE principal_id = NEW.id AND NOT revoked;
    RETURN NEW;
END $$;

CREATE TRIGGER revoke_cli_oauth_on_removal AFTER UPDATE OF status ON principals
    FOR EACH ROW WHEN (OLD.status <> NEW.status AND NEW.status = 'deactivated')
    EXECUTE FUNCTION revoke_cli_oauth_on_removal();
