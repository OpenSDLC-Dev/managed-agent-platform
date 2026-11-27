-- A service restart upgrades every writer together. New organization members
-- receive workspace grants exclusively through workspace membership management.
DROP TRIGGER initialize_legacy_workspace_grant ON principals;
DROP FUNCTION initialize_legacy_workspace_grant();
DROP FUNCTION backfill_default_workspace_grants();
DROP FUNCTION legacy_default_role(text, boolean);
ALTER TABLE principals DROP COLUMN workspace_grants_initialized;

-- The registry constrains org_id to this installation's single organization.
-- The scope is permanent, including after archival, when reports still use it.
CREATE FUNCTION prevent_resource_scope_move() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'resource workspace is immutable' USING ERRCODE = '23514';
END $$;

DO $$
DECLARE
    rel text;
BEGIN
    FOREACH rel IN ARRAY ARRAY['agents','environments','sessions','deployments','dreams',
        'vaults','memory_stores','files','skills','session_threads','events','work_items',
        'environment_keys','api_keys']
    LOOP
        IF rel NOT IN ('api_keys','environment_keys') THEN
            EXECUTE format('ALTER TABLE %I ADD FOREIGN KEY (org_id,workspace_id) REFERENCES workspaces(org_id,id)', rel);
        END IF;
        EXECUTE format('ALTER TABLE %I ADD CHECK (project_id = %L)', rel, 'default');
        EXECUTE format('CREATE TRIGGER prevent_scope_move BEFORE UPDATE OF workspace_id ON %I
                         FOR EACH ROW WHEN (NEW.workspace_id IS DISTINCT FROM OLD.workspace_id)
                         EXECUTE FUNCTION prevent_resource_scope_move()', rel);
    END LOOP;
END $$;

ALTER TABLE skills ADD CHECK (source <> 'anthropic' OR workspace_id = 'default');
ALTER TABLE agents ADD UNIQUE (workspace_id,id);
ALTER TABLE environments ADD UNIQUE (workspace_id,id);
ALTER TABLE sessions ADD UNIQUE (workspace_id,id);
ALTER TABLE deployments ADD UNIQUE (workspace_id,id);

ALTER TABLE sessions
    DROP CONSTRAINT sessions_environment_id_fkey,
    DROP CONSTRAINT sessions_deployment_id_fkey,
    ADD FOREIGN KEY (workspace_id,deployment_id) REFERENCES deployments(workspace_id,id),
    ADD FOREIGN KEY (workspace_id,environment_id) REFERENCES environments(workspace_id,id),
    ADD FOREIGN KEY (workspace_id,agent_id) REFERENCES agents(workspace_id,id);
ALTER TABLE deployments
    DROP CONSTRAINT deployments_environment_id_fkey,
    ADD FOREIGN KEY (workspace_id,environment_id) REFERENCES environments(workspace_id,id),
    ADD FOREIGN KEY (workspace_id,agent_id) REFERENCES agents(workspace_id,id);
ALTER TABLE environment_keys
    DROP CONSTRAINT environment_keys_environment_id_fkey,
    ADD FOREIGN KEY (workspace_id,environment_id) REFERENCES environments(workspace_id,id) ON DELETE CASCADE;
ALTER TABLE session_threads
    DROP CONSTRAINT session_threads_session_id_fkey,
    ADD FOREIGN KEY (workspace_id,session_id) REFERENCES sessions(workspace_id,id) ON DELETE CASCADE;
ALTER TABLE events
    DROP CONSTRAINT events_session_id_fkey,
    ADD FOREIGN KEY (workspace_id,session_id) REFERENCES sessions(workspace_id,id) ON DELETE CASCADE;
ALTER TABLE work_items
    DROP CONSTRAINT work_items_environment_id_fkey,
    DROP CONSTRAINT work_items_session_id_fkey,
    ADD FOREIGN KEY (workspace_id,environment_id) REFERENCES environments(workspace_id,id) ON DELETE CASCADE,
    ADD FOREIGN KEY (workspace_id,session_id) REFERENCES sessions(workspace_id,id) ON DELETE CASCADE;
ALTER TABLE dreams
    DROP CONSTRAINT dreams_session_id_fkey,
    ADD FOREIGN KEY (workspace_id,session_id) REFERENCES sessions(workspace_id,id) ON DELETE SET NULL (session_id);

-- Organization removal invalidates the current generation in every workspace,
-- including work the subject initiated under an earlier inherited grant.
DROP TRIGGER retire_principal_runs ON principals;
DROP FUNCTION on_default_principal_removal();
CREATE FUNCTION on_principal_removal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM revoke_workspace_runs(NEW.id,w.id) FROM workspaces w WHERE w.org_id = NEW.org_id;
    RETURN NULL;
END $$;
CREATE TRIGGER retire_principal_runs AFTER UPDATE OF status ON principals
    FOR EACH ROW WHEN (OLD.status = 'active' AND NEW.status <> 'active')
    EXECUTE FUNCTION on_principal_removal();

-- Writers share the workspace row with other writers and hold it through commit.
-- An archive or grant mutation takes the incompatible lock. Re-read permissions
-- after acquiring it so a waiter observes the winning administration change.
CREATE FUNCTION lock_workspace_write(space text, actor_id text, key_id text) RETURNS boolean
LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    IF actor_id <> '' THEN
        PERFORM 1 FROM principals WHERE id = actor_id AND status = 'active' FOR SHARE;
        IF NOT FOUND THEN RETURN false; END IF;
    END IF;
    PERFORM 1 FROM workspaces WHERE id = space AND archived_at IS NULL FOR SHARE;
    IF NOT FOUND THEN RETURN false; END IF;
    IF key_id <> '' THEN
        PERFORM 1 FROM api_keys WHERE id = key_id AND workspace_id = space AND status = 'active'
            AND (expires_at IS NULL OR expires_at > clock_timestamp()) FOR SHARE;
        IF NOT FOUND THEN RETURN false; END IF;
    END IF;
    RETURN actor_id = '' OR coalesce(workspace_role(actor_id, space) IN ('developer','limited_developer','admin'), false);
END $$;

CREATE FUNCTION lock_workspace_membership_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    space text;
    archived timestamptz;
BEGIN
    IF TG_OP = 'DELETE' THEN space := OLD.workspace_id; ELSE space := NEW.workspace_id; END IF;
    IF TG_OP = 'UPDATE' AND (NEW.workspace_id,NEW.principal_id) IS DISTINCT FROM (OLD.workspace_id,OLD.principal_id) THEN
        RAISE EXCEPTION 'workspace membership identity is immutable' USING ERRCODE = '23514';
    END IF;
    SELECT archived_at INTO archived FROM workspaces WHERE id = space FOR UPDATE;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    IF archived IS NOT NULL THEN
        RAISE EXCEPTION 'workspace is archived' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER lock_workspace_grant BEFORE INSERT OR UPDATE OR DELETE ON workspace_memberships
    FOR EACH ROW EXECUTE FUNCTION lock_workspace_membership_change();
