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
-- The password rides along on the same statement because it must be applied on
-- every run for rotation to work. NOTE: PostgreSQL writes ALTER ROLE to the
-- server log with the password in clear when log_statement is `ddl` or `all`.
-- Cloud SQL's default is `none`; if you have turned it up, turn it down for the
-- duration of this run.
SELECT format(
    'ALTER ROLE %I NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION PASSWORD %L',
    :'db_user', :'db_password')
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
    r    record;
    owns text;
BEGIN
    SELECT * INTO r FROM pg_roles WHERE rolname = u;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'FAILED: role % does not exist', u;
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

    -- OWNERSHIP: the platform database, and no other.
    SELECT r2.rolname INTO owns
    FROM pg_database db JOIN pg_roles r2 ON r2.oid = db.datdba
    WHERE db.datname = d;
    IF owns IS DISTINCT FROM u THEN
        RAISE EXCEPTION 'FAILED: database % is owned by %, not by %', d, coalesce(owns, '<none>'), u;
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
        'dbinit: role=% superuser=% createdb=% createrole=% bypassrls=% in_cloudsqlsuperuser=% owns=% encrypted=%',
        u, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolbypassrls,
        (to_regrole('cloudsqlsuperuser') IS NOT NULL
         AND pg_has_role(u, 'cloudsqlsuperuser', 'member')),
        coalesce((SELECT string_agg(db.datname, ',')
                  FROM pg_database db WHERE db.datdba = r.oid), '<none>'),
        COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false);

    RAISE NOTICE 'ok: % is a non-superuser, outside cloudsqlsuperuser, owning only %', u, d;
END
$assert$;
