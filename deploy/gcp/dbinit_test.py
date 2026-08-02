#!/usr/bin/env python3
"""Run dbinit.sql against a real PostgreSQL 16 with TLS on. Credential-free, free of charge.

`terraform validate` cannot execute SQL and shellcheck cannot execute psql, so
without this the only thing that ever runs deploy/gcp/dbinit.sql is a billable
GKE cluster talking to a billable Cloud SQL instance — which is a slow, expensive
and infrequent way to discover a typo in a \\gexec.

What this covers that a read cannot:

  - the file parses and runs end to end, including the psql meta-commands
    (\\getenv, \\gset, \\if, \\gexec) whose behaviour is not obvious from reading;
  - a second run is a no-op rather than an error, which is the case that happens
    after every rebuild of environment/;
  - a changed password is actually APPLIED on that second run, which is the
    whole rotation procedure — proven by authenticating as the platform role
    with the new password and failing to with the old one;
  - and, most of all, that the assertions can FAIL. An assertion that cannot go
    red is a comment, so every state the assertions guard and the DDL does not
    repair is set up deliberately below and the run is required to go red for
    that reason. The repaired ones -- LOGIN, INHERIT, CREATEDB, CREATEROLE,
    database ownership -- are unreachable by the time the assertion runs, and
    are covered by the repair cases instead.

What it cannot cover: Cloud SQL's own behaviour. `cloudsqlsuperuser` does not
exist on stock PostgreSQL, so the membership assertion is exercised here against
a role of that name created for the purpose — which proves the check works, not
that Cloud SQL grants what it is documented to grant. Only the real acceptance
run settles that.

The certificate is generated on the host and copied in with `docker cp` rather
than bind-mounted: the server refuses a key that is not mode 600 owned by the
database user, and a bind mount carries the host's ownership. It is generated on
the host rather than in the container because postgres:16-alpine ships no
openssl binary — the server links the library, the image does not install the
command.
"""

import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import time
import uuid

HERE = pathlib.Path(__file__).resolve().parent
DBINIT = HERE / "dbinit.sql"

IMAGE = os.environ.get("DBINIT_TEST_IMAGE", "postgres:16-alpine")
ADMIN_PASSWORD = "adminpw"
DB_NAME = "map"
DB_USER = "map"
APP_ROLE = "map_app"

failures = []


def check(label, ok, detail=""):
    if ok:
        print("  ok   %s" % label)
    else:
        print("  FAIL %s" % label)
        if detail:
            print("       %s" % detail.rstrip()[:2000])
        failures.append(label)


def run(*args, **kwargs):
    return subprocess.run(args, capture_output=True, text=True, **kwargs)


class Postgres:
    """A throwaway PostgreSQL 16 with TLS enabled."""

    def __init__(self):
        self.name = "map-dbinit-test-%s" % uuid.uuid4().hex[:12]
        self._tmpdir = tempfile.mkdtemp(prefix="map-dbinit-test-")
        # Who dbinit.sql runs as. The container's `postgres` is a true
        # PostgreSQL SUPERUSER, which Cloud SQL's administrator is NOT --
        # see as_cloud_sql_administrator().
        self.admin_user = "postgres"
        self.admin_password = ADMIN_PASSWORD

    def __enter__(self):
        # __exit__ is NOT called when __enter__ raises, so setup failures have to
        # clean up after themselves -- otherwise every failed run leaves a
        # container behind, which is exactly what happened while writing this.
        #
        # `docker run` is INSIDE the guard, not before it: a run that fails after
        # the daemon created the container -- a failed start, or the CLI
        # interrupted between creation and the reply -- leaves the named
        # container behind while returning non-zero. `docker rm -f` on a name
        # that does not exist is a harmless no-op, so covering the case costs
        # nothing and not covering it leaks under exactly the conditions nobody
        # reproduces on demand. The temp directory goes the same way.
        try:
            r = run("docker", "run", "-d", "--name", self.name,
                    "-e", "POSTGRES_PASSWORD=" + ADMIN_PASSWORD,
                    IMAGE)
            if r.returncode != 0:
                raise RuntimeError("could not start %s: %s" % (IMAGE, r.stderr))
            self._wait_ready()
            self._enable_tls()
            self._create_database()
        except BaseException:
            run("docker", "rm", "-f", self.name)
            shutil.rmtree(self._tmpdir, ignore_errors=True)
            raise
        return self

    def __exit__(self, *exc):
        # Whatever a run spawns, it owns -- including on the failure paths.
        run("docker", "rm", "-f", self.name)
        shutil.rmtree(self._tmpdir, ignore_errors=True)
        return False

    def _wait_ready(self):
        for _ in range(120):
            if run("docker", "exec", self.name,
                   "pg_isready", "-U", "postgres").returncode == 0:
                return
            time.sleep(0.5)
        raise RuntimeError("postgres never became ready")

    def _enable_tls(self):
        # Generated on the HOST: postgres:16-alpine ships no openssl binary (the
        # server links OpenSSL, it does not install the CLI). Copied in with
        # `docker cp` rather than bind-mounted, because the server refuses a key
        # that is not mode 600 owned by the database user, and a bind mount
        # carries the host's ownership no matter what is done to it inside.
        tmp = pathlib.Path(self._tmpdir)
        r = run("openssl", "req", "-new", "-x509", "-days", "1", "-nodes", "-text",
                "-out", str(tmp / "server.crt"), "-keyout", str(tmp / "server.key"),
                "-subj", "/CN=localhost")
        if r.returncode != 0:
            raise RuntimeError("could not create a certificate: %s" % r.stderr)
        data = "/var/lib/postgresql/data"
        for f in ("server.crt", "server.key"):
            c = run("docker", "cp", str(tmp / f), "%s:%s/%s" % (self.name, data, f))
            if c.returncode != 0:
                raise RuntimeError("could not copy %s in: %s" % (f, c.stderr))
        fix = run("docker", "exec", "-u", "root", self.name, "sh", "-c",
                  "chown postgres:postgres %s/server.crt %s/server.key && "
                  "chmod 600 %s/server.key" % (data, data, data))
        if fix.returncode != 0:
            raise RuntimeError("could not fix certificate ownership: %s" % fix.stderr)

        # Make password authentication REAL. initdb writes
        # `host all all 127.0.0.1/32 trust` into pg_hba.conf, and first match
        # wins -- so every TCP connection this test makes was authenticating
        # without checking the password at all, and the rotation assertions were
        # passing vacuously: the OLD password kept working because no password
        # was ever verified. `local ... trust` is kept so the image's own health
        # probes over the unix socket are unaffected.
        #
        # `hba` lands in printf's FORMAT argument, not in a `%s` operand, and
        # that is what turns the two `\n` above into real newlines. Moving it to
        # `printf '%s' "$hba"` would stop interpreting them and write the whole
        # file as one unparseable line.
        hba = ("local all all trust\\n"
               "host all all all scram-sha-256\\n")
        r = run("docker", "exec", "-u", "postgres", self.name, "sh", "-c",
                "printf '%s' > %s/pg_hba.conf" % (hba, data))
        if r.returncode != 0:
            raise RuntimeError("could not rewrite pg_hba.conf: %s" % r.stderr)
        # `ssl` is SIGHUP-changeable, so a reload is enough -- no restart, and no
        # race against the server coming back up.
        #
        # Two separate invocations, not one -c with both statements: psql wraps
        # a multi-statement -c in an implicit transaction, and ALTER SYSTEM
        # refuses to run inside one.
        for stmt in ("ALTER SYSTEM SET ssl = on;", "SELECT pg_reload_conf();"):
            r = self.psql_admin(stmt, sslmode="disable")
            if r.returncode != 0:
                raise RuntimeError("could not enable ssl (%s): %s"
                                   % (stmt, r.stderr + r.stdout))
        for _ in range(60):
            probe = self.psql_admin("SELECT 1;", sslmode="require")
            if probe.returncode == 0:
                return
            time.sleep(0.25)
        raise RuntimeError("TLS never came up: %s" % (probe.stderr + probe.stdout))

    def _create_database(self):
        r = self.psql_admin("CREATE DATABASE %s;" % DB_NAME, sslmode="require",
                            database="postgres")
        if r.returncode != 0:
            raise RuntimeError("could not create the database: %s" % r.stderr)

    def as_cloud_sql_administrator(self):
        """Re-shape this instance to look like Cloud SQL, and run as its admin.

        The difference that matters: Cloud SQL's `postgres` user is NOT a
        PostgreSQL superuser. It is an ordinary role holding `cloudsqlsuperuser`,
        which carries CREATEDB and CREATEROLE -- the attributes that matter here
        -- but not SUPERUSER, and a superuser bypasses privilege checks that an
        ordinary role must actually satisfy. So
        every run against the container's own `postgres` proves the SQL works
        under privileges the real administrator does not have.

        The statement this exists for is `ALTER DATABASE ... OWNER TO`.
        PostgreSQL requires the caller to own the database AND be able to SET ROLE
        to the new owner AND hold CREATEDB. A superuser satisfies all three
        vacuously. Here none of them is vacuous: ownership arrives through
        membership in cloudsqlsuperuser (which owns the database, as on Cloud
        SQL), and membership in the new owner arrives only because CREATE ROLE
        grants the creator ADMIN OPTION.
        """
        stmts = [
            "CREATE ROLE cloudsqlsuperuser CREATEDB CREATEROLE;",
            # Parenthesised because it is ONE statement wrapped across two
            # lines, not two list entries with a comma missing -- which is what
            # bare adjacent string literals inside a list look like to a linter,
            # and to a reader skimming for the comma.
            ("CREATE ROLE sqladmin LOGIN PASSWORD 'sqladminpw' "
             "IN ROLE cloudsqlsuperuser CREATEDB CREATEROLE;"),
            "ALTER DATABASE %s OWNER TO cloudsqlsuperuser;" % DB_NAME,
        ]
        for s in stmts:
            r = self.psql_admin(s)
            if r.returncode != 0:
                raise RuntimeError("could not shape the instance (%s): %s"
                                   % (s, r.stderr))
        self.admin_user = "sqladmin"
        self.admin_password = "sqladminpw"
        return self

    def psql_admin(self, sql, sslmode="require", database="postgres"):
        """Connect as the container's own superuser, NOT as self.admin_user.

        Deliberate, and load-bearing in both directions. The setup in
        as_cloud_sql_administrator has to run as a superuser, because the
        `sqladmin` role it models does not exist yet; and the verification
        queries after a run must not be limited by the privileges being
        modelled, or a missing privilege would read as a passing assertion.
        Only dbinit() connects as self.admin_user -- that connection is the
        one under test.
        """
        return run("docker", "exec",
                   "-e", "PGPASSWORD=" + ADMIN_PASSWORD,
                   "-e", "PGSSLMODE=" + sslmode,
                   self.name,
                   "psql", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1",
                   "-U", "postgres", "-d", database, "-Atc", sql)

    def psql_as(self, user, password, sql, database=DB_NAME):
        return run("docker", "exec",
                   "-e", "PGPASSWORD=" + password,
                   "-e", "PGSSLMODE=require",
                   self.name,
                   "psql", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1",
                   "-U", user, "-d", database, "-Atc", sql)

    def dbinit(self, password, sslmode="require", db_user=DB_USER):
        """Run dbinit.sql exactly as the Job's container command does."""
        run("docker", "cp", str(DBINIT), "%s:/tmp/dbinit.sql" % self.name)
        return run("docker", "exec",
                   "-e", "PGHOST=127.0.0.1",
                   "-e", "PGPORT=5432",
                   "-e", "PGDATABASE=" + DB_NAME,
                   "-e", "PGUSER=" + self.admin_user,
                   "-e", "PGPASSWORD=" + self.admin_password,
                   "-e", "PGSSLMODE=" + sslmode,
                   "-e", "MAP_DB_PASSWORD=" + password,
                   self.name,
                   "psql", "--no-psqlrc",
                   "-v", "db_user=" + db_user,
                   "-v", "app_role=" + APP_ROLE,
                   "-v", "db_name=" + DB_NAME,
                   "-f", "/tmp/dbinit.sql")

    def dbinit_without_password(self):
        run("docker", "cp", str(DBINIT), "%s:/tmp/dbinit.sql" % self.name)
        return run("docker", "exec",
                   "-e", "PGHOST=127.0.0.1",
                   "-e", "PGDATABASE=" + DB_NAME,
                   "-e", "PGUSER=" + self.admin_user,
                   "-e", "PGPASSWORD=" + self.admin_password,
                   "-e", "PGSSLMODE=require",
                   self.name,
                   "psql", "--no-psqlrc",
                   "-v", "db_user=" + DB_USER,
                   "-v", "app_role=" + APP_ROLE,
                   "-v", "db_name=" + DB_NAME,
                   "-f", "/tmp/dbinit.sql")


def role_attrs(pg, user=DB_USER):
    r = pg.psql_admin(
        "SELECT rolsuper, rolcreatedb, rolcreaterole, rolbypassrls "
        "FROM pg_roles WHERE rolname = '%s';" % user)
    return r.stdout.strip()


def main():
    if not DBINIT.is_file():
        print("dbinit.sql not found next to this test", file=sys.stderr)
        return 1
    if shutil.which("docker") is None:
        print("docker is required and not on PATH", file=sys.stderr)
        return 1

    print("a clean run creates the role and proves what it created")
    with Postgres() as pg:
        r = pg.dbinit("firstpassword")
        check("exits 0", r.returncode == 0, r.stderr + r.stdout)
        check("says what it established", "ok:" in (r.stdout + r.stderr),
              r.stdout + r.stderr)
        check("the role exists with no privilege attributes",
              role_attrs(pg) == "f|f|f|f", role_attrs(pg))
        check("owns the platform database",
              pg.psql_admin(
                  "SELECT r.rolname FROM pg_database d JOIN pg_roles r "
                  "ON r.oid = d.datdba WHERE d.datname = '%s';" % DB_NAME
              ).stdout.strip() == DB_USER)
        check("is a member of its custom role",
              pg.psql_admin("SELECT pg_has_role('%s', '%s', 'member');"
                            % (DB_USER, APP_ROLE)).stdout.strip() == "t")
        # The record the Job's log is supposed to leave behind, matched on the
        # exact field rather than on a stray `t` somewhere in the output.
        log = r.stdout + r.stderr
        check("records that the session was encrypted", "encrypted=t" in log, log)
        check("records that it is outside cloudsqlsuperuser",
              "in_cloudsqlsuperuser=f" in log, log)
        check("the platform role can authenticate",
              pg.psql_as(DB_USER, "firstpassword", "SELECT 1;").returncode == 0)

        print("a second run is a no-op AND applies a rotated password")
        r2 = pg.dbinit("secondpassword")
        check("exits 0", r2.returncode == 0, r2.stderr + r2.stdout)
        check("the new password works",
              pg.psql_as(DB_USER, "secondpassword", "SELECT 1;").returncode == 0)
        check("the old password does not",
              pg.psql_as(DB_USER, "firstpassword", "SELECT 1;").returncode != 0)
        check("attributes are still all false", role_attrs(pg) == "f|f|f|f",
              role_attrs(pg))

        print("a privilege granted by hand is corrected, not merely reported")
        pg.psql_admin("ALTER ROLE %s CREATEDB CREATEROLE;" % DB_USER)
        check("drifted", role_attrs(pg) == "f|t|t|f", role_attrs(pg))
        r3 = pg.dbinit("secondpassword")
        check("exits 0", r3.returncode == 0, r3.stderr + r3.stdout)
        check("attributes are back to false", role_attrs(pg) == "f|f|f|f",
              role_attrs(pg))

    # The case the container's own `postgres` cannot exercise, because it is a
    # true superuser and Cloud SQL's administrator is not. Everything above runs
    # under privileges the real administrator does not have; this block is the
    # one that establishes the file works under the privileges it will actually
    # be given.
    print("it works under a NON-superuser administrator, the way Cloud SQL is")
    with Postgres() as pg:
        pg.as_cloud_sql_administrator()
        check("the administrator really is not a superuser",
              pg.psql_admin("SELECT rolsuper FROM pg_roles "
                            "WHERE rolname = 'sqladmin';").stdout.strip() == "f")
        r = pg.dbinit("pw")
        check("exits 0", r.returncode == 0, r.stderr + r.stdout)
        # ALTER DATABASE ... OWNER TO is the statement at risk: PostgreSQL wants
        # the caller to own the database, to be able to SET ROLE to the new
        # owner, and to hold CREATEDB -- all three of which a superuser gets for
        # free and this administrator has to genuinely satisfy.
        check("the platform role owns the database anyway",
              pg.psql_admin(
                  "SELECT r.rolname FROM pg_database d JOIN pg_roles r "
                  "ON r.oid = d.datdba WHERE d.datname = '%s';" % DB_NAME
              ).stdout.strip() == DB_USER)
        check("and is still outside cloudsqlsuperuser",
              pg.psql_admin("SELECT pg_has_role('%s', 'cloudsqlsuperuser', 'member');"
                            % DB_USER).stdout.strip() == "f")
        check("re-running under that administrator is still a no-op",
              pg.dbinit("pw2").returncode == 0)
        check("the rotated password works",
              pg.psql_as(DB_USER, "pw2", "SELECT 1;").returncode == 0)

    # NOLOGIN is the drift every other assertion is blind to: the role still
    # exists, still has no CREATEDB, still owns the database and still is not a
    # member of cloudsqlsuperuser. Only a check for it stops a run reporting
    # success over a credential the platform cannot authenticate with.
    print("a role stripped of LOGIN is repaired, and would otherwise pass everything")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        pg.psql_admin("ALTER ROLE %s NOLOGIN NOINHERIT;" % DB_USER)
        check("the role really cannot log in now",
              pg.psql_as(DB_USER, "pw", "SELECT 1;").returncode != 0)
        check("and every containment assertion still holds, which is the point",
              role_attrs(pg) == "f|f|f|f", role_attrs(pg))
        r = pg.dbinit("pw")
        check("exits 0", r.returncode == 0, r.stderr + r.stdout)
        check("LOGIN is back", pg.psql_as(DB_USER, "pw", "SELECT 1;").returncode == 0)
        check("INHERIT is back",
              pg.psql_admin("SELECT rolinherit FROM pg_roles WHERE rolname = '%s';"
                            % DB_USER).stdout.strip() == "t")

    print("an empty password is refused rather than stored")
    with Postgres() as pg:
        r = pg.dbinit("")
        check("refuses", r.returncode != 0)
        check("says which variable", "MAP_DB_PASSWORD" in (r.stderr + r.stdout),
              r.stderr + r.stdout)
        check("creates no role at all", role_attrs(pg) == "", role_attrs(pg))

    print("an UNSET password is refused too (\\getenv leaves the variable unset)")
    with Postgres() as pg:
        r = pg.dbinit_without_password()
        check("refuses", r.returncode != 0)
        check("says which variable", "MAP_DB_PASSWORD" in (r.stderr + r.stdout),
              r.stderr + r.stdout)
        check("creates no role at all", role_attrs(pg) == "", role_attrs(pg))

    # ---------------------------------------------------------------------
    # The assertions must be able to go red. Each case below sets up a state
    # the DDL does NOT correct, so reaching the assertion block is the only
    # possible outcome -- if the file still exits 0, that assertion is dead.
    # ---------------------------------------------------------------------

    # Containment applied to the platform's role alone is not containment. The
    # role is created IN ROLE the group, membership carries SET by default, so
    # a privileged GROUP role hands the platform those privileges one SET ROLE
    # away — while every assertion about the platform's own attributes stays
    # false. This is the case where a pre-existing group role is the attack.
    print("a PRE-EXISTING privileged group role is narrowed, not inherited")
    with Postgres() as pg:
        pg.psql_admin("CREATE ROLE %s CREATEDB CREATEROLE;" % APP_ROLE)
        check("the group role starts out privileged",
              pg.psql_admin("SELECT rolcreatedb, rolcreaterole FROM pg_roles "
                            "WHERE rolname = '%s';" % APP_ROLE).stdout.strip() == "t|t")
        r = pg.dbinit("pw")
        check("exits 0", r.returncode == 0, r.stderr + r.stdout)
        check("the group role was narrowed",
              pg.psql_admin("SELECT rolcreatedb, rolcreaterole, rolcanlogin FROM pg_roles "
                            "WHERE rolname = '%s';" % APP_ROLE).stdout.strip() == "f|f|f")
        # The property that actually matters: what the platform can reach
        # THROUGH the group, not what the group's row says.
        check("so the platform cannot reach CREATEDB through it",
              pg.psql_admin("SELECT pg_has_role('%s', '%s', 'member');"
                            % (DB_USER, APP_ROLE)).stdout.strip() == "t"
              and pg.psql_admin("SELECT rolcreatedb FROM pg_roles WHERE rolname = '%s';"
                                % APP_ROLE).stdout.strip() == "f")

    # The boundary of the narrowing above, under the privileges that actually
    # apply. Altering a role needs ADMIN OPTION on it, which the administrator
    # holds only for roles it created; a group role belonging to somebody else
    # cannot be narrowed at all. What matters is that this is a LOUD failure
    # before any password is set -- not a run that proceeds under a group whose
    # privileges it could not strip.
    print("a group role owned by SOMEONE ELSE stops the run rather than passing")
    with Postgres() as pg:
        pg.psql_admin("CREATE ROLE %s CREATEDB;" % APP_ROLE)  # created by the superuser
        pg.as_cloud_sql_administrator()
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("says it may not alter the role",
              "permission denied to alter role" in (r.stderr + r.stdout),
              r.stderr + r.stdout)
        check("and sets no password on the way out",
              pg.psql_as(DB_USER, "pw", "SELECT 1;").returncode != 0)

    print("the privileged-group-role assertion FIRES when the group cannot be narrowed")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        # Re-privilege the group AFTER the corrective ALTER would have run, by
        # granting it a superuser attribute the script's own ALTER cannot strip
        # -- which is exactly the state the assertion, rather than the repair,
        # has to catch.
        pg.psql_admin("ALTER ROLE %s SUPERUSER;" % APP_ROLE)
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("names the group role", "group role" in (r.stderr + r.stdout),
              r.stderr + r.stdout)

    # SUPERUSER above is one disjunct of that assertion; BYPASSRLS is another,
    # and it is the one the group's corrective ALTER cannot strip either
    # (`NOLOGIN NOCREATEDB NOCREATEROLE` names none of the three superuser-only
    # attributes). Without this case, deleting `g.rolbypassrls` from the
    # condition leaves the whole suite green.
    print("the group-role assertion FIRES on BYPASSRLS too, not only SUPERUSER")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        pg.psql_admin("ALTER ROLE %s BYPASSRLS;" % APP_ROLE)
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("reports bypassrls on the group role",
              "bypassrls=t" in (r.stderr + r.stdout), r.stderr + r.stdout)

    print("the cloudsqlsuperuser membership assertion FIRES when it should")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        pg.psql_admin("CREATE ROLE cloudsqlsuperuser SUPERUSER;")
        pg.psql_admin("GRANT cloudsqlsuperuser TO %s;" % DB_USER)
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("names cloudsqlsuperuser",
              "cloudsqlsuperuser" in (r.stderr + r.stdout), r.stderr + r.stdout)

    print("the owns-nothing-else assertion FIRES when it should")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        pg.psql_admin("CREATE DATABASE somethingelse OWNER %s;" % DB_USER)
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("says it owns another database",
              "owns a database other than" in (r.stderr + r.stdout),
              r.stderr + r.stdout)

    # The escalation this file exists to prevent, run in the direction that
    # actually matters. Everything above asks "did the platform role end up
    # contained?"; this asks "can the CONTAINED role climb back out on the next
    # run?" -- which is the question a compromised platform credential poses.
    #
    # After a first run the platform role owns the database and `public`, which
    # is exactly enough to set a database-level search_path and plant a
    # public.format(text,text). Every DDL statement in dbinit.sql is built with
    # format() and run with \gexec, so an unpinned search_path would resolve the
    # planted function and execute its return value as the ADMINISTRATOR.
    # Verified before the fix: format('CREATE ROLE %I','someRole') returned
    # `CREATE ROLE pwned LOGIN SUPERUSER`.
    print("a hostile search_path cannot make the admin run the platform's SQL")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        pg.psql_admin("ALTER DATABASE %s SET search_path = public, pg_catalog;" % DB_NAME)
        plant = pg.psql_as(DB_USER, "pw",
                           "CREATE FUNCTION public.format(text, text) RETURNS text "
                           "AS $$ SELECT 'CREATE ROLE pwned LOGIN SUPERUSER' $$ "
                           "LANGUAGE sql;")
        check("the platform role really can plant the shadow function",
              plant.returncode == 0, plant.stderr)
        r = pg.dbinit("pw")
        check("the run still succeeds", r.returncode == 0, r.stderr + r.stdout)
        pwned = pg.psql_admin(
            "SELECT count(*) FROM pg_roles WHERE rolname = 'pwned';")
        check("and no role the attacker named was created",
              pwned.stdout.strip() == "0", pwned.stdout + pwned.stderr)

    # A conditional statement whose condition is a scalar subquery does nothing
    # when the subquery is NULL -- so with no `public` schema the ALTER SCHEMA
    # is skipped, silently, and the platform's unqualified migrations then have
    # nowhere to create their tables. That has to fail HERE, not at startup.
    print("a missing public schema fails the run rather than passing it")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        drop = pg.psql_admin("DROP SCHEMA public CASCADE;", database=DB_NAME)
        check("the schema is gone", drop.returncode == 0, drop.stderr)
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("says the migrations have no schema",
              "no public schema" in (r.stderr + r.stdout), r.stderr + r.stdout)

    print("the session-encrypted assertion FIRES on a plaintext connection")
    with Postgres() as pg:
        r = pg.dbinit("pw", sslmode="disable")
        check("fails", r.returncode != 0, r.stdout)
        check("says the session is not encrypted",
              "not encrypted" in (r.stderr + r.stdout), r.stderr + r.stdout)

    # The three attributes the corrective ALTER deliberately does NOT name.
    # `ALTER ROLE ... LOGIN INHERIT NOCREATEDB NOCREATEROLE` cannot revoke
    # SUPERUSER, BYPASSRLS or REPLICATION, because PostgreSQL lets only a
    # superuser change those -- even to turn them off -- and Cloud SQL's
    # administrator is not one. So all three are asserted and not repaired,
    # and the three cases below are what make "asserted" mean something:
    # without them the assertions guard a reachable state that nothing ever
    # demonstrates.
    print("the SUPERUSER assertion FIRES on a role it cannot revoke it from")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        pg.psql_admin("ALTER ROLE %s SUPERUSER;" % DB_USER)
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("says SUPERUSER", "is SUPERUSER" in (r.stderr + r.stdout),
              r.stderr + r.stdout)

    print("the BYPASSRLS assertion FIRES on a role it cannot revoke it from")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        pg.psql_admin("ALTER ROLE %s BYPASSRLS;" % DB_USER)
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("says BYPASSRLS", "has BYPASSRLS" in (r.stderr + r.stdout),
              r.stderr + r.stdout)

    print("the REPLICATION assertion FIRES on a role it cannot revoke it from")
    with Postgres() as pg:
        check("a clean run first succeeds", pg.dbinit("pw").returncode == 0)
        pg.psql_admin("ALTER ROLE %s REPLICATION;" % DB_USER)
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("says REPLICATION", "has REPLICATION" in (r.stderr + r.stdout),
              r.stderr + r.stdout)

    # A login role that already exists skips the guarded `CREATE ROLE ... IN
    # ROLE`, and nothing later grants the membership -- so a role created by
    # hand before the first run would authenticate fine and hold none of the
    # group's grants. That is a silent partial install, which is exactly what
    # this assertion is for.
    print("the member-of-its-own-role assertion FIRES on a hand-made role")
    with Postgres() as pg:
        pg.psql_admin("CREATE ROLE %s LOGIN PASSWORD 'preexisting';" % DB_USER)
        r = pg.dbinit("pw")
        check("fails", r.returncode != 0, r.stdout)
        check("says it is not a member of its own role",
              "is not a member of its own role" in (r.stderr + r.stdout),
              r.stderr + r.stdout)

    print()
    if failures:
        print("FAILED: %d check(s)" % len(failures))
        for f in failures:
            print("  - %s" % f)
        return 1
    print("ok: dbinit.sql establishes and proves the privilege set it claims")
    return 0


if __name__ == "__main__":
    sys.exit(main())
