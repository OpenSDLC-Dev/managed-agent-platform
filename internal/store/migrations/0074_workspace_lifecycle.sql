-- Archive and credential/worker retirement commit under the same workspace lock.
ALTER TABLE workspaces ADD COLUMN display_color text NOT NULL DEFAULT '#6C5BB9'
    CHECK (display_color ~ '^#[0-9A-Fa-f]{6}$');

CREATE FUNCTION retire_archived_workspace() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.archived_at IS NOT NULL THEN
        RAISE EXCEPTION 'workspace archival is permanent';
    END IF;
    UPDATE api_keys SET status = 'archived' WHERE workspace_id = NEW.id AND status <> 'archived';
    UPDATE environment_keys SET revoked_at = NEW.archived_at WHERE workspace_id = NEW.id AND revoked_at IS NULL;
    UPDATE deployments SET paused_at = NEW.archived_at, paused_kind = 'error',
        paused_error_type = 'workspace_archived_error', updated_at = NEW.archived_at
        WHERE workspace_id = NEW.id AND archived_at IS NULL AND paused_at IS NULL;
    UPDATE work_items SET state = 'stopped', stopped_at = NEW.archived_at, updated_at = NEW.archived_at,
        stop_requested_at = coalesce(stop_requested_at, NEW.archived_at), lease_expires_at = NULL
        WHERE workspace_id = NEW.id AND state <> 'stopped';
    RETURN NULL;
END $$;
CREATE TRIGGER retire_archived_workspace AFTER UPDATE OF archived_at ON workspaces
    FOR EACH ROW WHEN (NEW.archived_at IS DISTINCT FROM OLD.archived_at)
    EXECUTE FUNCTION retire_archived_workspace();
