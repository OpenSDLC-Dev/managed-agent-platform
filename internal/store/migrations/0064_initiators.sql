-- Plan 69: the authority work acts for is separate from its audit creator.
-- No foreign keys: a creator removed before principal retention was fixed
-- remains a legacy initiator until slice 7. A claim reads these columns
-- without locking the session while it holds a work item (#643).
ALTER TABLE sessions
    ADD COLUMN initiator_principal_id text,
    ADD COLUMN initiator_api_key_id text,
    ADD CHECK (initiator_principal_id IS NULL OR initiator_api_key_id IS NULL);
ALTER TABLE deployments
    ADD COLUMN initiator_principal_id text,
    ADD COLUMN initiator_api_key_id text,
    ADD CHECK (initiator_principal_id IS NULL OR initiator_api_key_id IS NULL);
ALTER TABLE dreams
    ADD COLUMN initiator_principal_id text,
    ADD COLUMN initiator_api_key_id text,
    ADD CHECK (initiator_principal_id IS NULL OR initiator_api_key_id IS NULL);

-- Do not filter by status or role here. Attribution survives deactivation,
-- key expiry and demotion; admission checks membership separately.
CREATE FUNCTION resolve_initiator(creator text)
RETURNS TABLE (principal_id text, api_key_id text)
LANGUAGE sql STABLE AS $$
    SELECT coalesce(p.id,k.owner_principal_id),
           CASE WHEN coalesce(p.id,k.owner_principal_id) IS NULL THEN nullif(creator,'') END
      FROM (SELECT 1) seed
      LEFT JOIN principals p ON p.id=creator
      LEFT JOIN api_keys k ON k.id=creator
$$;

CREATE FUNCTION record_creator_initiator() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.initiator_principal_id IS NULL AND NEW.initiator_api_key_id IS NULL THEN
        SELECT principal_id,api_key_id INTO NEW.initiator_principal_id,NEW.initiator_api_key_id
          FROM resolve_initiator(NEW.created_by);
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER record_creator_initiator BEFORE INSERT ON deployments
    FOR EACH ROW EXECUTE FUNCTION record_creator_initiator();
CREATE TRIGGER record_creator_initiator BEFORE INSERT ON dreams
    FOR EACH ROW EXECUTE FUNCTION record_creator_initiator();

UPDATE deployments SET (initiator_principal_id,initiator_api_key_id)=
    (SELECT principal_id,api_key_id FROM resolve_initiator(created_by));
UPDATE dreams SET (initiator_principal_id,initiator_api_key_id)=
    (SELECT principal_id,api_key_id FROM resolve_initiator(created_by));

-- This also resolves a rolling upgrade's old writer. In particular, an old
-- dream runner links its session only after inserting it. The claim uses
-- this view without repairing the session under its work-item lock.
-- Deployment fallback uses its original creator, never a later replacement:
-- an unresolved historical session must not acquire authority on a repoint.
-- The insert trigger below snapshots configured initiators for future runs.
CREATE VIEW session_initiators AS
SELECT s.id AS session_id, chosen.principal_id, chosen.api_key_id
  FROM sessions s
  LEFT JOIN LATERAL resolve_initiator(s.created_by) actor ON true
  LEFT JOIN deployments d ON d.id=s.deployment_id
  LEFT JOIN LATERAL resolve_initiator(d.created_by) da ON true
  LEFT JOIN dreams dr ON dr.session_id=s.id
  LEFT JOIN LATERAL resolve_initiator(dr.created_by) dra ON true
  LEFT JOIN LATERAL (
      SELECT v.principal_id,v.api_key_id
        FROM (VALUES
            (0,s.initiator_principal_id,s.initiator_api_key_id),
            (1,actor.principal_id,actor.api_key_id),
            (3,da.principal_id,da.api_key_id),
            (4,dr.initiator_principal_id,dr.initiator_api_key_id),
            (5,dra.principal_id,dra.api_key_id)
        ) v(priority,principal_id,api_key_id)
       WHERE v.principal_id IS NOT NULL OR v.api_key_id IS NOT NULL
       ORDER BY v.priority LIMIT 1
  ) chosen ON true;

UPDATE sessions s SET initiator_principal_id=i.principal_id,initiator_api_key_id=i.api_key_id
  FROM session_initiators i WHERE i.session_id=s.id;

CREATE FUNCTION record_session_initiator() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.initiator_principal_id IS NOT NULL OR NEW.initiator_api_key_id IS NOT NULL THEN
        RETURN NEW;
    END IF;
    SELECT principal_id,api_key_id INTO NEW.initiator_principal_id,NEW.initiator_api_key_id
      FROM resolve_initiator(NEW.created_by);
    IF NEW.initiator_principal_id IS NULL AND NEW.initiator_api_key_id IS NULL
       AND NEW.deployment_id IS NOT NULL THEN
        SELECT d.initiator_principal_id,d.initiator_api_key_id
          INTO NEW.initiator_principal_id,NEW.initiator_api_key_id
          FROM deployments d WHERE d.id=NEW.deployment_id;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER record_session_initiator BEFORE INSERT ON sessions
    FOR EACH ROW EXECUTE FUNCTION record_session_initiator();
