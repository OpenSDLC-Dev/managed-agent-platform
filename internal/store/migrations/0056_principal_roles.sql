-- A principal's role is stored (docs/plan/69_principal-bound-auth.md decision
-- 4). This supersedes 0022's "roles are deliberately NOT a column". The
-- reference stores an organization role on each member, and an admin assigns
-- it in the console, so the role has to live here. 0022's fear was a stale
-- second authority beside the IdP. The rule the identity lane applies at every
-- sign-in (internal/principal's Resolve) answers it:
--   * a person a mapping matches takes the mapped role, and role_source says
--     'idp';
--   * a person whose role came from the mapping, and whom it no longer
--     matches, goes back to user, and role_source says 'console';
--   * a person no mapping has matched keeps the role the console set.
-- Every existing row gets user and 'console'. A mapped person regains their
-- role at their next request.
--
-- 0022's "an operator may DELETE a stale row at any time" no longer holds. A
-- deactivated principal's row is what keeps the person removed: delete it and
-- their next sign-in creates them afresh, as an active user. So the triggers
-- below refuse every DELETE and TRUNCATE on principals. A retention job
-- written to the DELETE docs/self-hosted-security.md §8 used to recommend then
-- fails loudly instead of undoing a removal, and §8's scrub, an UPDATE that
-- clears the personal fields, takes its place. The triggers are statement
-- level, so a DELETE that matches no row is refused too: a job that would
-- delete is a job to change.
--
-- install_bootstrap's one row records that the install has had an admin, and
-- a setup code is printed only while it is absent (plan 69 decision 1). The
-- row is never deleted, so a later state with no admin never reopens that
-- door.
--
-- A replica on an earlier build still reads roles from the token on every
-- request and ignores these columns, so while it runs it would admit a
-- deactivated person. Nothing in this build deactivates anyone, so the
-- overlap admits nobody it should refuse. That build's INSERT names none of
-- the new columns and gets their defaults, and its conflict update leaves
-- them as they are.
--
-- The ALTER takes principals ACCESS EXCLUSIVE, and every human request writes
-- principals. The bound migrate.go lays before every migration
-- (migrateLockWait) keeps the wait for that lock to 2s, and migrate.go retries
-- the give-up (55P03). Validating the new CHECKs reads the whole table under
-- that lock, which is cheap: it holds one row per person who has signed in.

ALTER TABLE principals
    ADD COLUMN kind text NOT NULL DEFAULT 'user'
        CHECK (kind IN ('user', 'service_account')),
    ADD COLUMN role text NOT NULL DEFAULT 'user'
        CHECK (role IN ('user', 'developer', 'admin')),
    ADD COLUMN role_source text NOT NULL DEFAULT 'console'
        CHECK (role_source IN ('console', 'idp')),
    ADD COLUMN status text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'deactivated')),
    ADD COLUMN description text NOT NULL DEFAULT '',
    -- A service account is developer or admin, as in the reference.
    ADD CHECK (kind <> 'service_account' OR role IN ('developer', 'admin'));

CREATE FUNCTION principals_refuse_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'principals rows are never deleted (plan 69): scrub personal fields instead, as docs/self-hosted-security.md §8 describes'
        USING ERRCODE = 'restrict_violation';
END $$;

CREATE TRIGGER principals_refuse_delete BEFORE DELETE ON principals
    FOR EACH STATEMENT EXECUTE FUNCTION principals_refuse_delete();

CREATE TRIGGER principals_refuse_truncate BEFORE TRUNCATE ON principals
    FOR EACH STATEMENT EXECUTE FUNCTION principals_refuse_delete();

-- The setup codes the replicas printed at start-up, by hash. Any unexpired
-- one redeems, and redeeming one deletes them all.
CREATE TABLE setup_codes (
    code_hash  text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE TABLE install_bootstrap (
    singleton      boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    first_admin_at timestamptz NOT NULL DEFAULT now()
);

-- Every admin grant closes setup permanently, including the IdP mapping and
-- SQL recovery. Neither a later demotion nor deactivation clears this flag.
CREATE FUNCTION principals_record_first_admin() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO install_bootstrap (singleton) VALUES (true) ON CONFLICT DO NOTHING;
    RETURN NEW;
END $$;

CREATE TRIGGER principals_first_admin_insert AFTER INSERT ON principals
    FOR EACH ROW WHEN (NEW.role = 'admin')
    EXECUTE FUNCTION principals_record_first_admin();

CREATE TRIGGER principals_first_admin_promotion AFTER UPDATE OF role ON principals
    FOR EACH ROW WHEN (OLD.role IS DISTINCT FROM NEW.role AND NEW.role = 'admin')
    EXECUTE FUNCTION principals_record_first_admin();
