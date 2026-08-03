-- Creates the platform's database role OUTSIDE cloudsqlsuperuser, and proves it.
--
-- Run by deploy/gcp/dbinit.sh as a Kubernetes Job, connected to the platform
-- database as the Cloud SQL built-in administrator. See that script for why it
-- runs from inside the cluster: the instance has no public address.
--
-- WHY THIS FILE EXISTS AT ALL. Cloud SQL grants cloudsqlsuperuser — CREATEDB
-- and CREATEROLE — to every built-in user created through its Admin API, which
-- is the path `gcloud sql users create` and Terraform's google_sql_user take.
-- Google documents one way out: name a custom database role at creation
-- (--database-roles / databaseRoles / database_roles) and the cloudsqlsuperuser
-- grant is suppressed. That role must already exist, and creating a PostgreSQL
-- role takes a SQL session — so something has to run SQL first no matter which
-- path is chosen. Once something does, plain CREATE ROLE reaches the same end
-- more directly: it never goes through the Admin API's user path, so there is
-- no grant to suppress. This file takes that route and then ASSERTS the result,
-- because the argument above is reasoning and the assertions are evidence.
--
-- NOTHING HERE IS INTERPOLATED BY THE SHELL. Every name and the password
-- arrive as psql variables — the names via -v, the password via \getenv from
-- the pod's environment, which is sourced from a Kubernetes Secret. They reach
-- SQL through format()'s %I (identifier) and %L (literal), which quote
-- server-side. A name is therefore never concatenated into a statement, and the
-- password never appears in a process argument list, in a ConfigMap, or in this
-- file.
--
-- IDEMPOTENT. Re-running is the normal case: after a rebuild of environment/,
-- and after bootstrap.sh rotates the password. Creation is guarded on
-- existence; the ALTERs are unconditional, which is what makes a re-run apply a
-- rotated password rather than skip it.

\set ON_ERROR_STOP on

-- PINNED BEFORE ANYTHING ELSE, and this is a privilege boundary rather than
-- hygiene. Every statement below builds its DDL with format() and runs it with
-- \gexec, and function resolution follows search_path. After the first run the
-- platform's role owns this database and its `public` schema, which is exactly
-- enough to (a) `ALTER DATABASE ... SET search_path = public, pg_catalog` and
-- (b) `CREATE FUNCTION public.format(text, text)` returning any DDL it likes.
-- The next run of this file — the rotation path, executed by the Cloud SQL
-- ADMINISTRATOR — would then resolve the planted format(), and \gexec would
-- execute the attacker's string with the administrator's privileges. The
-- assertions at the end would not notice: they inspect the roles this file
-- names, not roles it was tricked into creating. A compromised platform
-- credential would escalate to the administrator on the next rotation, which
-- inverts the containment this whole file exists to establish.
--
-- pg_catalog first, so the built-ins cannot be shadowed. pg_temp named
-- EXPLICITLY and LAST, because left unnamed it is searched FIRST and it is
-- writable. Naming it is not what makes the temp table below reachable —
-- PostgreSQL finds the session temp schema whether or not it is on the path,
-- so omitting it entirely would also work. Last is simply the safer of the two
-- placements, and the only one that says so out loud.
--
-- The %I/%L quoting below defends against hostile NAMES. It does nothing about
-- a hostile function resolution, which is why both are needed.
SET search_path = pg_catalog, pg_temp;

-- Defaulted BEFORE \getenv, which leaves the psql variable alone when the
-- environment variable is absent. Without this line an unset MAP_DB_PASSWORD
-- reaches the next statement as the nine literal characters `:'db_password'`
-- and fails as a syntax error — a confusing way to report a missing secret, and
-- one that never reaches the guard written for exactly that case.
\set db_password ''
\getenv db_password MAP_DB_PASSWORD

-- A missing password would otherwise reach ALTER ROLE as an empty string and
-- set an empty password, which authenticates nothing and looks like a working
-- deploy right up until the platform starts. \getenv leaves the variable unset
-- when the environment variable is absent, so the test is written to cover both
-- unset and empty.
--
-- The failure is a RAISE rather than a \echo because only an error stops psql:
-- with ON_ERROR_STOP on, this exits non-zero and the Job fails. An \echo would
-- print the complaint and then carry on to set the empty password.
SELECT coalesce(:'db_password', '') <> '' AS have_password \gset
\if :have_password
\else
DO $missing$ BEGIN
    RAISE EXCEPTION 'MAP_DB_PASSWORD is empty or unset — refusing to set an empty password';
END $missing$;
\endif

-- ---------------------------------------------------------------------------
-- The custom role. NOLOGIN (the default): it is a group to hold whatever
-- database-wide grants the platform's role should share, and a thing to name in
-- --database-roles if a future user IS created through the Admin API.
-- ---------------------------------------------------------------------------

SELECT format('CREATE ROLE %I', :'app_role')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'app_role')
\gexec

-- Narrowed on EVERY run, not only when it was just created — and this is not
-- symmetry for its own sake. Containment applied to the platform's role alone
-- is not containment: the role is created IN ROLE this one, membership carries
-- the SET option by default, so `SET ROLE <app_role>` hands over whatever this
-- role holds. A pre-existing group role with CREATEDB would therefore give the
-- platform CREATEDB while every assertion about the platform's OWN attributes
-- stayed false. NOLOGIN because nothing should ever authenticate as the group.
--
-- ONE case this cannot repair, and it fails loudly rather than quietly: altering
-- a role needs ADMIN OPTION on it, which the administrator holds only for roles
-- it created itself (PostgreSQL 16 grants it implicitly at CREATE ROLE). A group
-- role pre-created by some OTHER role therefore stops the run here with
-- `permission denied to alter role`, before any password is set and before any
-- assertion can report success. That is the correct outcome — the alternative
-- would be proceeding under a group whose privileges this session cannot see the
-- end of — but it means "narrowed on every run" holds for the roles this
-- administrator owns, not universally. Fix such a role by hand, or drop it and
-- let this file create it.
SELECT format('ALTER ROLE %I NOLOGIN NOCREATEDB NOCREATEROLE', :'app_role')
\gexec

-- ---------------------------------------------------------------------------
-- The platform's own role, created under it.
-- ---------------------------------------------------------------------------

SELECT format('CREATE ROLE %I LOGIN IN ROLE %I', :'db_user', :'app_role')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'db_user')
\gexec

-- Stated rather than inherited. These are the defaults for a role created by
-- CREATE ROLE, so on a first run this changes nothing — its job is the SECOND
-- run, against a role that already exists and may have been granted something
-- by hand since. Asserting below without correcting here would turn a drift
-- into a failed deploy rather than a fixed one.
--
-- CREATEDB and CREATEROLE ONLY, and the omissions are not an oversight.
-- PostgreSQL lets only a SUPERUSER change the SUPERUSER, REPLICATION and
-- BYPASSRLS attributes — even to turn them OFF — and the administrator this
-- runs as is not one: Cloud SQL's `postgres` holds `cloudsqlsuperuser`, which
-- carries CREATEDB and CREATEROLE — the attributes that matter here — but is
-- not SUPERUSER itself. Naming NOSUPERUSER here fails the whole statement with
-- `permission denied to alter role / Only roles with the SUPERUSER attribute
-- may change the SUPERUSER attribute`, so a run
-- that tried to be thorough would not run at all. Those three are still
-- ASSERTED below; what changes is only that a drift in them is reported rather
-- than silently repaired, which is the honest outcome for a privilege this
-- session genuinely cannot revoke.
--
-- The password rides along on the same statement because it must be applied on
-- every run for rotation to work. NOTE: PostgreSQL writes ALTER ROLE to the
-- server log with the password in clear when log_statement is `ddl` or `all`.
-- Cloud SQL's default is `none`; if you have turned it up, turn it down for the
-- duration of this run.
-- LOGIN and INHERIT are restored here, not only set at CREATE. They are the two
-- properties whose absence is invisible to every other check in this file: a
-- role stripped of LOGIN still exists, still has no CREATEDB, still owns the
-- database and still is not a member of cloudsqlsuperuser, so the assertions
-- below would all pass and the platform would fail at startup with
-- `role "..." is not permitted to log in`. Without INHERIT it would authenticate
-- and then not have the privileges of its own group role.
SELECT format(
    'ALTER ROLE %I LOGIN INHERIT NOCREATEDB NOCREATEROLE PASSWORD %L',
    :'db_user', :'db_password')
\gexec

-- The grant that makes the ownership transfer below legal.
--
-- `ALTER DATABASE ... OWNER TO` requires the caller to be able to SET ROLE to
-- the new owner. A superuser can always, which is why this line looks
-- unnecessary and is not: the administrator this actually runs as is NOT a
-- superuser — Cloud SQL's `postgres` holds `cloudsqlsuperuser`, not SUPERUSER — and
-- in PostgreSQL 16 the membership CREATE ROLE implicitly grants its creator
-- does not carry the SET option. Without this the next statement fails with
-- `must be able to SET ROLE "<role>"`, on the real instance and never on a
-- workstation where the test happens to connect as a superuser.
--
-- The direction is the safe one and worth being explicit about: this makes the
-- ADMINISTRATOR a member of the platform's role, not the reverse. The platform's
-- role gains nothing, and in particular does not become a member of
-- cloudsqlsuperuser — which the assertions below re-check.
--
-- Unconditional because GRANT is idempotent. If the role pre-existed and this
-- administrator has no ADMIN OPTION on it, this fails loudly here rather than
-- producing a confusing ownership error one statement later.
SELECT format('GRANT %I TO CURRENT_USER', :'db_user')
\gexec

-- Ownership of the platform's own database, and of the schema the migrations
-- create their objects in. In PostgreSQL 15+ `public` is owned by
-- pg_database_owner, so transferring the database usually carries the schema
-- with it — usually, because a managed provider is free to have created it
-- differently, and "usually" is not something to leave a deploy resting on.
SELECT format('ALTER DATABASE %I OWNER TO %I', :'db_name', :'db_user')
\gexec

SELECT format('ALTER SCHEMA public OWNER TO %I', :'db_user')
WHERE (SELECT r.rolname FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner
       WHERE n.nspname = 'public') <> 'pg_database_owner'
\gexec

-- ---------------------------------------------------------------------------
-- The assertions. Each one RAISEs, so psql's exit status is the verdict and the
-- Job fails rather than reporting success over a privilege that was never
-- narrowed.
--
-- The names come from a temp table rather than from psql variables because psql
-- does not substitute inside a dollar-quoted body — a :'db_user' written in
-- there would reach the server as those nine literal characters.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _dbinit_names(k text PRIMARY KEY, v text);
INSERT INTO _dbinit_names VALUES
    ('db_user', :'db_user'),
    ('app_role', :'app_role'),
    ('db_name', :'db_name');

DO $assert$
DECLARE
    u    text := (SELECT v FROM _dbinit_names WHERE k = 'db_user');
    a    text := (SELECT v FROM _dbinit_names WHERE k = 'app_role');
    d    text := (SELECT v FROM _dbinit_names WHERE k = 'db_name');
    r     record;
    g     record;
    owns  text;
    extra text;
BEGIN
    SELECT * INTO r FROM pg_roles WHERE rolname = u;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'FAILED: role % does not exist', u;
    END IF;

    -- The two properties that are about the role being USABLE rather than about
    -- it being contained. Every other assertion here is happy with a role that
    -- cannot log in, so without these a run could report success over a
    -- credential the platform then fails to authenticate with.
    IF NOT r.rolcanlogin THEN
        RAISE EXCEPTION 'FAILED: % cannot log in', u;
    END IF;
    IF NOT r.rolinherit THEN
        RAISE EXCEPTION 'FAILED: % does not inherit the privileges of its roles', u;
    END IF;

    -- Role ATTRIBUTES. Necessary and, on their own, nowhere near sufficient —
    -- see the membership check below, which is the one that actually settles
    -- the cloudsqlsuperuser question.
    IF r.rolsuper THEN
        RAISE EXCEPTION 'FAILED: % is SUPERUSER', u;
    END IF;
    IF r.rolcreatedb THEN
        RAISE EXCEPTION 'FAILED: % has CREATEDB', u;
    END IF;
    IF r.rolcreaterole THEN
        RAISE EXCEPTION 'FAILED: % has CREATEROLE', u;
    END IF;
    IF r.rolbypassrls THEN
        RAISE EXCEPTION 'FAILED: % has BYPASSRLS', u;
    END IF;
    -- REPLICATION belongs with SUPERUSER and BYPASSRLS, not with the two
    -- above: it is the third attribute only a real superuser may change, so it
    -- is asserted and never repaired. It is worth asserting on its own merits
    -- rather than for symmetry — a replication connection streams the WAL,
    -- which carries every database on the instance, so it reads around the
    -- ownership containment the rest of this block establishes.
    IF r.rolreplication THEN
        RAISE EXCEPTION 'FAILED: % has REPLICATION', u;
    END IF;

    -- MEMBERSHIP, which the attributes above say nothing about: a role with
    -- every attribute false is still a superuser in effect if it is a member of
    -- one. This is the assertion the whole file exists for.
    --
    -- to_regrole returns NULL rather than raising when the role is absent, so
    -- this stays runnable against a plain PostgreSQL where cloudsqlsuperuser
    -- does not exist — and a NULL there is a genuine "not a member", not a
    -- skipped check.
    IF to_regrole('cloudsqlsuperuser') IS NOT NULL
       AND pg_has_role(u, 'cloudsqlsuperuser', 'member') THEN
        RAISE EXCEPTION 'FAILED: % is a member of cloudsqlsuperuser', u;
    END IF;

    IF NOT pg_has_role(u, a, 'member') THEN
        RAISE EXCEPTION 'FAILED: % is not a member of its own role %', u, a;
    END IF;

    -- The membership set is EXACTLY {itself, its group, pg_database_owner}, and
    -- this is the general form of the cloudsqlsuperuser check above rather than
    -- a second copy of it. That check names one role; this one closes the set,
    -- because cloudsqlsuperuser is not the only membership that would undo the
    -- containment. `pg_read_all_data` is the sharp example: a single GRANT
    -- gives SELECT on every table in every database the role can connect to,
    -- while every attribute assertion above stays false and the ownership
    -- assertions below stay true. A rerun would have printed ok.
    --
    -- pg_database_owner MUST be excluded: owning the database makes the role an
    -- implicit member of it, so including it would fail every clean run.
    SELECT string_agg(r2.rolname, ', ' ORDER BY r2.rolname) INTO extra
    FROM pg_roles r2
    WHERE r2.rolname NOT IN (u, a, 'pg_database_owner')
      AND pg_has_role(u, r2.oid, 'MEMBER');
    IF extra IS NOT NULL THEN
        RAISE EXCEPTION 'FAILED: % holds membership beyond % — also a member of: %', u, a, extra;
    END IF;

    -- And the group role's OWN attributes, which the checks above are blind to.
    -- Membership carries the SET option by default, so anything this role holds
    -- is one `SET ROLE` away for the platform — a group role with CREATEDB
    -- gives the platform CREATEDB while every assertion about the platform's
    -- own attributes stays false.
    --
    -- The NOT FOUND guard is not defence against a state reachable today: the
    -- role is created earlier in this same script. It is defence against the
    -- assertion going QUIET if that ever stops being true — every field of a
    -- missing row is NULL, the condition below evaluates to NULL rather than
    -- true, and the check the file exists for would be skipped in silence.
    SELECT * INTO g FROM pg_roles WHERE rolname = a;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'FAILED: group role % does not exist', a;
    END IF;
    IF g.rolsuper OR g.rolcreatedb OR g.rolcreaterole OR g.rolbypassrls OR g.rolcanlogin THEN
        RAISE EXCEPTION
            'FAILED: group role % is privileged (superuser=% createdb=% createrole=% bypassrls=% login=%) and % can SET ROLE to it',
            a, g.rolsuper, g.rolcreatedb, g.rolcreaterole, g.rolbypassrls, g.rolcanlogin, u;
    END IF;

    -- OWNERSHIP: the platform database, and no other.
    SELECT r2.rolname INTO owns
    FROM pg_database db JOIN pg_roles r2 ON r2.oid = db.datdba
    WHERE db.datname = d;
    IF owns IS DISTINCT FROM u THEN
        RAISE EXCEPTION 'FAILED: database % is owned by %, not by %', d, coalesce(owns, '<none>'), u;
    END IF;

    -- The SCHEMA, asserted separately from the database and not implied by it.
    -- The ALTER SCHEMA above is conditional, and its condition is a scalar
    -- subquery: with no `public` schema at all that subquery is NULL, the
    -- comparison is NULL rather than true, and the statement is skipped. That
    -- is a silent success over a database the migrations cannot use —
    -- internal/store/migrations creates unqualified objects, so it needs a
    -- schema to create them in. Asserted here so the failure lands in this Job
    -- rather than in the platform's first startup.
    IF NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'public') THEN
        RAISE EXCEPTION 'FAILED: database % has no public schema for the migrations to use', d;
    END IF;
    SELECT r2.rolname INTO owns
    FROM pg_namespace n JOIN pg_roles r2 ON r2.oid = n.nspowner
    WHERE n.nspname = 'public';
    IF owns IS DISTINCT FROM u AND owns IS DISTINCT FROM 'pg_database_owner' THEN
        RAISE EXCEPTION 'FAILED: schema public is owned by %, not by % or pg_database_owner',
            coalesce(owns, '<none>'), u;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_database db JOIN pg_roles r2 ON r2.oid = db.datdba
               WHERE r2.rolname = u AND db.datname <> d) THEN
        RAISE EXCEPTION 'FAILED: % owns a database other than %', u, d;
    END IF;

    -- The connection this session is running over. Asserted here rather than
    -- inferred from a DSN parameter: sslmode states what the client asked for,
    -- pg_stat_ssl states what the server got.
    IF NOT COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false) THEN
        RAISE EXCEPTION 'FAILED: this session is not encrypted';
    END IF;

    -- The record, emitted from inside the block that just proved every field.
    --
    -- It is NOT a separate SELECT over pg_roles, and that is deliberate: an
    -- unguarded pg_has_role(..., 'cloudsqlsuperuser', ...) out here RAISES on
    -- any PostgreSQL where that role does not exist, which turned a passing run
    -- into an error at the very last statement — after every assertion had
    -- already succeeded. Reusing what the assertions read keeps the record and
    -- the verdict from being able to disagree, and keeps the file runnable
    -- against stock PostgreSQL so it can be tested without a Cloud SQL bill.
    RAISE NOTICE
        'dbinit: role=% superuser=% createdb=% createrole=% bypassrls=% replication=% in_cloudsqlsuperuser=% owns=% encrypted=%',
        u, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolbypassrls,
        r.rolreplication,
        (to_regrole('cloudsqlsuperuser') IS NOT NULL
         AND pg_has_role(u, 'cloudsqlsuperuser', 'member')),
        coalesce((SELECT string_agg(db.datname, ',')
                  FROM pg_database db WHERE db.datdba = r.oid), '<none>'),
        COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false);

    RAISE NOTICE 'ok: % is a non-superuser, outside cloudsqlsuperuser, owning only %', u, d;
END
$assert$;
