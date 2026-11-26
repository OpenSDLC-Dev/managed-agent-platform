-- The browser slug and the storage key identify the same row. The CLI has
-- already issued Default's public ID. Neither identifier may be recycled.
CREATE TABLE workspaces (
    id text PRIMARY KEY,
    public_id text NOT NULL UNIQUE CHECK (public_id ~ '^wrkspc_[A-Za-z0-9]{26}$'),
    org_id text NOT NULL DEFAULT 'default' CHECK (org_id = 'default'),
    name text NOT NULL,
    archived_at timestamptz,
    UNIQUE (org_id, id),
    CHECK ((id = 'default') = (public_id = 'wrkspc_00000000000000000000000001')),
    CHECK (id <> 'default' OR (name = 'Default' AND archived_at IS NULL))
);
INSERT INTO workspaces (id, public_id, name)
VALUES ('default', 'wrkspc_00000000000000000000000001', 'Default');

CREATE FUNCTION protect_workspace_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.id, NEW.public_id) = (OLD.id, OLD.public_id) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'workspace identifiers are permanent; archive the workspace instead';
END $$;
CREATE TRIGGER protect_workspace_identity BEFORE UPDATE OR DELETE ON workspaces
    FOR EACH ROW EXECUTE FUNCTION protect_workspace_identity();
CREATE TRIGGER protect_workspace_registry BEFORE TRUNCATE ON workspaces
    FOR EACH STATEMENT EXECUTE FUNCTION protect_workspace_identity();

CREATE TABLE workspace_memberships (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    principal_id text NOT NULL REFERENCES principals(id),
    role text NOT NULL CHECK (role IN ('user','limited_developer','developer','admin')),
    PRIMARY KEY (workspace_id, principal_id)
);
ALTER TABLE principals DROP CONSTRAINT principals_role_check,
    ADD CHECK (role IN ('user','developer','billing','admin')),
    ADD COLUMN workspace_grants_initialized boolean NOT NULL DEFAULT false;

-- Current organization authority, explicit assignment and workspace liveness
-- are read in one snapshot. Billing never gains resource permissions by rank.
CREATE FUNCTION workspace_role(subject_id text, space_id text) RETURNS text
LANGUAGE sql STABLE AS $$
    SELECT CASE WHEN p.role = 'admin' THEN 'admin'
                WHEN p.role = 'billing' AND m.role IS DISTINCT FROM 'admin' THEN 'billing'
                ELSE m.role END
    FROM principals p JOIN workspaces w ON w.org_id = p.org_id
    LEFT JOIN workspace_memberships m ON (m.workspace_id,m.principal_id) = (w.id,p.id)
    WHERE p.id = subject_id AND p.status = 'active' AND w.id = space_id AND w.archived_at IS NULL
$$;

CREATE FUNCTION legacy_default_role(org_role text, playground boolean) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN org_role = 'developer' THEN 'developer' WHEN playground THEN 'user' END
$$;

-- Mark even principals who receive no grant. Replaying the backfill can then
-- neither replace a later assignment nor restore a membership that was deleted.
CREATE FUNCTION backfill_default_workspace_grants() RETURNS void LANGUAGE sql AS $$
    WITH pending AS (
        UPDATE principals SET workspace_grants_initialized = true
        WHERE NOT workspace_grants_initialized
        RETURNING id, org_id, role, status, playground_access
    ), grants AS (
        SELECT id, legacy_default_role(role, playground_access) AS role
        FROM pending WHERE status = 'active' AND org_id = 'default'
    )
    INSERT INTO workspace_memberships (workspace_id, principal_id, role)
    SELECT 'default', id, role FROM grants WHERE role IS NOT NULL
    ON CONFLICT DO NOTHING
$$;
SELECT backfill_default_workspace_grants();

-- A previous binary omits the marker on first sign-in or account creation.
-- An aware writer explicitly initializes an empty set of grants.
CREATE FUNCTION initialize_legacy_workspace_grant() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    granted text := legacy_default_role(NEW.role, NEW.playground_access);
BEGIN
    IF NEW.org_id = 'default' AND NEW.status = 'active' AND granted IS NOT NULL THEN
        INSERT INTO workspace_memberships VALUES ('default', NEW.id, granted);
    END IF;
    UPDATE principals SET workspace_grants_initialized = true WHERE id = NEW.id;
    RETURN NULL;
END $$;
CREATE TRIGGER initialize_legacy_workspace_grant AFTER INSERT ON principals
    FOR EACH ROW WHEN (NOT NEW.workspace_grants_initialized)
    EXECUTE FUNCTION initialize_legacy_workspace_grant();

-- The existing Playground grant is the Default User assignment. The trigger
-- participates in the principal writer's transaction, including on old replicas.
CREATE FUNCTION sync_default_playground_grant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.playground_access AND NEW.status = 'active' AND NEW.org_id = 'default' THEN
        INSERT INTO workspace_memberships VALUES ('default', NEW.id, 'user') ON CONFLICT DO NOTHING;
    ELSIF NOT NEW.playground_access THEN
        DELETE FROM workspace_memberships WHERE workspace_id = 'default' AND principal_id = NEW.id AND role = 'user';
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER sync_default_playground_grant AFTER UPDATE OF playground_access ON principals
    FOR EACH ROW WHEN (NEW.playground_access <> OLD.playground_access)
    EXECUTE FUNCTION sync_default_playground_grant();

-- Audit the reserved triples before adding registry references. Report counts
-- and scopes; no resource bytes or credentials enter migration diagnostics.
DO $$
DECLARE
    tbl regclass;
    scopes text;
    unexpected text[] := '{}';
BEGIN
    FOREACH tbl IN ARRAY ARRAY['agents','environments','sessions','events','work_items','api_keys',
        'environment_keys','skills','files','vaults','principals','session_threads','memory_stores',
        'deployments','dreams','modelgateway.providers','modelgateway.deployments','modelgateway.aliases']::regclass[] LOOP
        EXECUTE format('SELECT string_agg(format(''%%s/%%s/%%s (%%s rows)'',org_id,workspace_id,project_id,n), '', '')
                        FROM (SELECT org_id,workspace_id,project_id,count(*) n FROM %s
                              WHERE (org_id,workspace_id,project_id) <> (''default'',''default'',''default'')
                              GROUP BY org_id,workspace_id,project_id) x', tbl) INTO scopes;
        IF scopes IS NOT NULL THEN
            unexpected := array_append(unexpected, format('%s: %s', tbl, scopes));
        END IF;
    END LOOP;
    IF cardinality(unexpected) > 0 THEN
        RAISE EXCEPTION 'reconcile historical workspace scopes before upgrading: %', array_to_string(unexpected, '; ');
    END IF;
END $$;

ALTER TABLE api_keys ADD FOREIGN KEY (org_id, workspace_id) REFERENCES workspaces(org_id, id);
ALTER TABLE environment_keys ADD FOREIGN KEY (org_id, workspace_id) REFERENCES workspaces(org_id, id);
ALTER TABLE cli_oauth_grants ADD COLUMN workspace_id text NOT NULL DEFAULT 'default' REFERENCES workspaces(id);
ALTER TABLE modelgateway.usage ADD COLUMN workspace_id text NOT NULL DEFAULT 'default' REFERENCES workspaces(id);
ALTER TABLE modelgateway.usage_daily ADD COLUMN workspace_id text NOT NULL DEFAULT 'default' REFERENCES workspaces(id);
ALTER TABLE modelgateway.usage_actor_daily ADD COLUMN workspace_id text NOT NULL DEFAULT 'default' REFERENCES workspaces(id);

-- Existing work survives an organization-role change, even for a User who
-- never had Playground. Explicit withdrawal advances the subject's generation.
-- Re-granting access cannot make work from the retired generation live again.
CREATE TABLE workspace_run_revocations (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    principal_id text NOT NULL REFERENCES principals(id),
    generation bigint NOT NULL,
    PRIMARY KEY (workspace_id, principal_id)
);
CREATE FUNCTION workspace_run_generation(subject_id text, space_id text) RETURNS bigint
LANGUAGE sql STABLE AS $$
    SELECT coalesce((SELECT generation FROM workspace_run_revocations
        WHERE workspace_id=space_id AND principal_id=subject_id), 0)
$$;
CREATE FUNCTION revoke_workspace_runs(subject_id text, space_id text) RETURNS void LANGUAGE sql AS $$
    INSERT INTO workspace_run_revocations AS v VALUES (space_id,subject_id,1)
    ON CONFLICT (workspace_id,principal_id) DO UPDATE SET generation = v.generation + 1
$$;
CREATE FUNCTION on_workspace_grant_removal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM revoke_workspace_runs(OLD.principal_id, OLD.workspace_id);
    RETURN NULL;
END $$;
CREATE TRIGGER retire_workspace_runs AFTER DELETE ON workspace_memberships
    FOR EACH ROW EXECUTE FUNCTION on_workspace_grant_removal();

CREATE FUNCTION on_default_principal_removal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM revoke_workspace_runs(NEW.id, 'default');
    RETURN NULL;
END $$;
CREATE TRIGGER retire_principal_runs AFTER UPDATE OF status ON principals
    FOR EACH ROW WHEN (OLD.status='active' AND NEW.status<>'active')
    EXECUTE FUNCTION on_default_principal_removal();

CREATE FUNCTION workspace_can_continue(subject_id text, space_id text, issued_generation bigint)
RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT FROM principals p JOIN workspaces w ON w.org_id=p.org_id
        WHERE p.id=subject_id AND p.status='active' AND w.id=space_id AND w.archived_at IS NULL
        AND issued_generation=workspace_run_generation(subject_id,space_id))
$$;

-- A constant default stamps pre-upgrade rows without rewriting a populated
-- sessions table. New rows must qualify through their live grant or parent.
ALTER TABLE sessions ADD COLUMN workspace_grant_generation bigint DEFAULT 0;
ALTER TABLE dreams ADD COLUMN workspace_grant_generation bigint DEFAULT 0;
ALTER TABLE deployments ADD COLUMN workspace_grant_generation bigint DEFAULT 0;
ALTER TABLE sessions ALTER COLUMN workspace_grant_generation DROP DEFAULT;
ALTER TABLE dreams ALTER COLUMN workspace_grant_generation DROP DEFAULT;
ALTER TABLE deployments ALTER COLUMN workspace_grant_generation DROP DEFAULT;

-- Freeze the effective subject of older sessions that still resolve through
-- a creator or parent. An unresolved or inactive subject receives no stamp.
UPDATE sessions s SET initiator_principal_id=i.principal_id
FROM session_initiators i WHERE i.session_id=s.id AND s.initiator_principal_id IS NULL
AND i.principal_id IS NOT NULL;
UPDATE sessions s SET workspace_grant_generation=NULL WHERE NOT EXISTS
    (SELECT FROM principals p WHERE p.id=s.initiator_principal_id AND p.status='active' AND p.org_id=s.org_id);
UPDATE dreams d SET workspace_grant_generation=NULL WHERE NOT EXISTS
    (SELECT FROM principals p WHERE p.id=d.initiator_principal_id AND p.status='active' AND p.org_id=d.org_id);
UPDATE deployments d SET workspace_grant_generation=NULL WHERE NOT EXISTS
    (SELECT FROM principals p WHERE p.id=d.initiator_principal_id AND p.status='active' AND p.org_id=d.org_id);

CREATE FUNCTION stamp_workspace_work() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME='sessions' THEN
        IF NEW.initiator_principal_id IS NULL AND NEW.initiator_api_key_id IS NULL AND NEW.created_by IS NOT NULL THEN
            SELECT principal_id,api_key_id INTO NEW.initiator_principal_id,NEW.initiator_api_key_id
            FROM resolve_initiator(NEW.created_by);
        END IF;
        -- Manual runs have a creator and are authorized as new work.
        IF NEW.deployment_id IS NOT NULL AND NEW.created_by IS NULL THEN
            SELECT d.workspace_grant_generation INTO NEW.workspace_grant_generation FROM deployments d
            WHERE d.id=NEW.deployment_id AND d.workspace_id=NEW.workspace_id
              AND d.initiator_principal_id=NEW.initiator_principal_id;
            RETURN NEW;
        END IF;
    END IF;
    NEW.workspace_grant_generation := NULL;
    IF workspace_role(NEW.initiator_principal_id,NEW.workspace_id) IN ('developer','limited_developer','admin') THEN
        NEW.workspace_grant_generation := workspace_run_generation(NEW.initiator_principal_id,NEW.workspace_id);
    END IF;
    RETURN NEW;
END $$;
-- PostgreSQL orders same-event triggers by name; record_*_initiator runs first.
CREATE TRIGGER stamp_workspace_work BEFORE INSERT OR UPDATE OF initiator_principal_id ON sessions
    FOR EACH ROW EXECUTE FUNCTION stamp_workspace_work();
CREATE TRIGGER stamp_workspace_work BEFORE INSERT OR UPDATE OF initiator_principal_id ON dreams
    FOR EACH ROW EXECUTE FUNCTION stamp_workspace_work();
CREATE TRIGGER stamp_workspace_work BEFORE INSERT OR UPDATE OF initiator_principal_id ON deployments
    FOR EACH ROW EXECUTE FUNCTION stamp_workspace_work();

-- Both generations of the dream runner attach the child in the transaction
-- that creates it. Carry the parent's original eligibility across that link.
CREATE FUNCTION inherit_dream_continuation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE sessions s SET workspace_grant_generation=NEW.workspace_grant_generation
    FROM session_initiators i WHERE s.id=NEW.session_id AND i.session_id=s.id
      AND s.workspace_id=NEW.workspace_id AND i.principal_id=NEW.initiator_principal_id;
    RETURN NULL;
END $$;
CREATE TRIGGER inherit_dream_continuation AFTER UPDATE OF session_id ON dreams
    FOR EACH ROW WHEN (NEW.session_id IS NOT NULL AND NEW.session_id IS DISTINCT FROM OLD.session_id)
    EXECUTE FUNCTION inherit_dream_continuation();
