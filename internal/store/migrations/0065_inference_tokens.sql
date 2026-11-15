-- A model-turn token belongs to a claim, not a changing lease expiry.
-- The trigger covers old binaries' claims too: reclaiming with an old brain
-- must not revive the previous claimant's token during a rolling upgrade.
ALTER TABLE work_items ADD COLUMN inference_generation bigint NOT NULL DEFAULT 0;
CREATE FUNCTION advance_inference_generation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- A renewal can start before expiry, then wait on a row lock past it.
    -- Judge after that wait so an expired token cannot regain authority.
    IF NEW.kind='model_turn' AND NEW.state='active'
       AND (OLD.state<>'active' OR OLD.lease_expires_at<=clock_timestamp()) THEN
        NEW.inference_generation := OLD.inference_generation+1;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER advance_inference_generation BEFORE UPDATE OF state,lease_expires_at ON work_items
    FOR EACH ROW EXECUTE FUNCTION advance_inference_generation();

-- Only the work item is a foreign key. Minting while holding that item must
-- never acquire a session/principal row lock (#643). Authentication joins
-- the session and current principal instead. One hash per item bounds churn.
CREATE TABLE inference_tokens (
    work_id text PRIMARY KEY REFERENCES work_items(id) ON DELETE CASCADE,
    claim_generation bigint NOT NULL,
    token_hash text NOT NULL UNIQUE,
    principal_id text,
    api_key_id text,
    CHECK (num_nonnulls(principal_id,api_key_id)=1),
    CHECK (principal_id IS NULL OR principal_id<>''),
    CHECK (api_key_id IS NULL OR api_key_id<>'')
);
