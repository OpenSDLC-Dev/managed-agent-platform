#!/usr/bin/env python3
"""Exercise bootstrap.sh against a fake `gcloud`, credential-free and free of charge.

Why this exists: `make gcp-lint` is shellcheck, and shellcheck cannot know that
`gcloud secrets versions describe` rejects `--filter`. It exited 0 on a version of
this script that aborted on its very first call in every project — the documented
three-step deploy path was dead and every static check was green. Reviewing harder
is not a fix for that; the script has to be RUN.

The fake models the parts of gcloud whose behaviour the script actually depends on:

  - argv strictness. `--filter` is one of gcloud's list-command flags and a
    `describe` command rejects it at parse time, exit 2, before contacting the API.
    That single rule is what makes the regression above reproducible here.
  - `--data-file=-` stores stdin BYTE FOR BYTE, so payloads are captured verbatim
    and asserted on. A trailing newline in the database password is invisible
    until it reaches a DSN parser.

Faults are injected by dropping marker files in the state directory, which lets a
scenario fail a specific call rather than the whole binary — including the case
that matters most and cannot be produced by an exit code alone: a call that
COMMITS server-side and then reports failure.

Run: make gcp-bootstrap-test
"""

import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).parent.resolve()
BOOTSTRAP = HERE / "bootstrap.sh"
SECRETS = ["map-db-password", "map-db-admin-password"]

FAKE_GCLOUD = r'''#!/usr/bin/env python3
import os, sys, json, pathlib
S = pathlib.Path(os.environ["FAKE_STATE"])
a = sys.argv[1:]
flags = {x.split("=", 1)[0] for x in a if x.startswith("--")}


def die(msg, code=2):
    sys.stderr.write(msg + "\n")
    sys.exit(code)


def fault(tag):
    return (S / ("fault." + tag)).exists()


def log(line):
    with (S / "calls.log").open("a") as f:
        f.write(line + "\n")


log(" ".join(a))

if a[:3] == ["secrets", "versions", "describe"]:
    # Real gcloud registers --filter only on ListCommand, so a describe rejects
    # it at argv parsing -- before auth, in every project.
    if "--filter" in flags:
        die("ERROR: (gcloud.secrets.versions.describe) unrecognized arguments: "
            "--filter=state=ENABLED (did you mean '--flatten'?)")
    name = [x.split("=", 1)[1] for x in a if x.startswith("--secret=")][0]
    if fault("describe"):
        die("ERROR: (gcloud.secrets.versions.describe) UNAVAILABLE: backend error", 1)
    f = S / ("ver." + name)
    if not f.exists():
        die("ERROR: (gcloud.secrets.versions.describe) NOT_FOUND: Secret Version "
            "[projects/p/secrets/%s/versions/latest] not found." % name, 1)
    if fault("warn"):
        # gcloud writes warnings to stderr on SUCCESS as well as failure --
        # configured service-account impersonation prints one on every call.
        sys.stderr.write("WARNING: This command is using service account impersonation. "
                         "All API calls will be executed as [x@y.iam.gserviceaccount.com].\n")
    print(f.read_text().strip())
    sys.exit(0)

if a[:2] == ["secrets", "describe"]:
    sys.exit(0 if (S / ("container." + a[2])).exists() else 1)

if a[:3] == ["secrets", "versions", "add"]:
    name = a[3]
    (S / ("payload." + name)).write_bytes(sys.stdin.buffer.read())
    (S / ("ver." + name)).write_text("ENABLED")
    if fault("add." + name):
        # The write COMMITTED above, and only then reports failure: a dropped
        # connection reading the response, or a signal landing just after.
        die("ERROR: (gcloud.secrets.versions.add) connection reset reading response", 1)
    sys.exit(0)

die("fake gcloud: unhandled invocation: " + " ".join(a))
'''

failures = []


class Run:
    def __init__(self, state, proc):
        self.state, self.proc = state, proc
        self.out = proc.stdout + proc.stderr
        self.code = proc.returncode

    def payload(self, name):
        p = self.state / ("payload." + name)
        return p.read_bytes() if p.exists() else None


def run(tmp, name, faults=(), containers=SECRETS, versions=(), reuse=None):
    state = reuse if reuse else pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix=name + "."))
    if not reuse:
        for c in containers:
            (state / ("container." + c)).touch()
        for v in versions:
            (state / ("ver." + v)).write_text("ENABLED")
    # Faults are per-run, not per-state: a scenario that reuses a previous run's
    # state is usually asking "does the NEXT run recover?", which it cannot answer
    # if the previous run's injected failure is still armed.
    for stale in state.glob("fault.*"):
        stale.unlink()
    for f in faults:
        (state / ("fault." + f)).touch()
    env = dict(os.environ)
    env["PATH"] = str(pathlib.Path(tmp) / "bin") + os.pathsep + env["PATH"]
    env["FAKE_STATE"] = str(state)
    env["PROJECT"] = "p"
    env.pop("NAME_PREFIX", None)
    return Run(state, subprocess.run(["bash", str(BOOTSTRAP)], env=env, text=True,
                                     capture_output=True, timeout=120))


def check(label, cond, detail=""):
    if cond:
        print("  ok   %s" % label)
    else:
        print("  FAIL %s%s" % (label, ("\n       " + detail.replace("\n", "\n       ")) if detail else ""))
        failures.append(label)


def main():
    if not BOOTSTRAP.exists():
        print("bootstrap.sh not found next to this test", file=sys.stderr)
        return 1
    tmp = tempfile.mkdtemp(prefix="bootstrap-test.")
    try:
        binp = pathlib.Path(tmp) / "bin"
        binp.mkdir()
        (binp / "gcloud").write_text(FAKE_GCLOUD)
        (binp / "gcloud").chmod(0o755)
        # A generator that fails producing nothing. Under a naive
        # `openssl | gcloud --data-file=-` pipeline the right-hand side still
        # runs, reads EOF, and stores an enabled ZERO-BYTE password that every
        # later run skips as "already done".
        # Resolved before the fake bin goes on PATH, and by lookup rather than a
        # fixed path: the wrapper cannot call bare `openssl` without recursing
        # into itself, and `/usr/bin/openssl` does not exist everywhere (Homebrew
        # puts it under /opt/homebrew/bin, and container images vary).
        real_openssl = shutil.which("openssl")
        if real_openssl is None:
            print("openssl is not on PATH; bootstrap.sh requires it", file=sys.stderr)
            return 1
        (binp / "openssl").write_text(
            '#!/usr/bin/env bash\n'
            'if [[ -e "$FAKE_STATE/fault.openssl" ]]; then exit 1; fi\n'
            'exec %s "$@"\n' % real_openssl
        )
        (binp / "openssl").chmod(0o755)

        print("a clean run stores every secret")
        r = run(tmp, "clean")
        check("exits 0", r.code == 0, r.out)
        check("writes every secret", all(r.payload(s) is not None for s in SECRETS))
        # The regression that shipped: --filter on a describe. The fake rejects it
        # exactly as gcloud does, so the clean run above cannot pass if it returns.
        check("never passes --filter to a describe",
              "versions describe" in (r.state / "calls.log").read_text()
              and not any("describe" in ln and "--filter" in ln
                          for ln in (r.state / "calls.log").read_text().splitlines()))

        print("the database password is stored without a trailing newline")
        db = r.payload("map-db-password")
        check("is 64 hex characters exactly", db is not None and len(db) == 64, repr(db))
        check("does not end in a newline", db is not None and not db.endswith(b"\n"), repr(db))

        # The two database credentials must not be the same value. They are
        # generated by one shared function, which is exactly the shape that
        # produces this bug: hoist the generation out of the loop, or reuse a
        # variable across both calls, and both secrets get one password. Nothing
        # downstream would notice — the platform would authenticate, the
        # administrator would authenticate — and the whole point of the split,
        # that the platform's credential is not the superuser's, would be gone
        # while every other check stayed green.
        print("the administrator's password is a DIFFERENT credential")
        admin = r.payload("map-db-admin-password")
        check("is 64 hex characters exactly", admin is not None and len(admin) == 64, repr(admin))
        check("is not the platform's password", admin != db)

        print("re-running is a no-op rather than an overwrite")
        before = {s: r.payload(s) for s in SECRETS}
        r2 = run(tmp, "rerun", reuse=r.state)
        check("exits 0", r2.code == 0, r2.out)
        check("says it left them alone", "left alone" in r2.out, r2.out)
        check("changes no payload", all(r2.payload(s) == before[s] for s in SECRETS))

        print("a missing foundation is named as such")
        r = run(tmp, "nofoundation", containers=[])
        check("refuses", r.code != 0)
        check("names gcp-foundation-apply", "gcp-foundation-apply" in r.out, r.out)

        print("an unreadable secret aborts instead of writing a second version")
        r = run(tmp, "unreadable", faults=["describe"])
        check("refuses", r.code != 0)
        check("writes nothing", all(r.payload(s) is None for s in SECRETS))

        print("a failed generator never lets an empty password be stored")
        r = run(tmp, "opensslfails", faults=["openssl"])
        check("refuses", r.code != 0)
        check("stores no password at all", r.payload("map-db-password") is None,
              repr(r.payload("map-db-password")))
        r2 = run(tmp, "opensslfails-rerun", reuse=r.state)
        check("and the next run still creates a real one",
              r2.payload("map-db-password") is not None and len(r2.payload("map-db-password")) == 64,
              repr(r2.payload("map-db-password")))

        # The fault the fake exists for, and the one an exit code alone cannot
        # produce: the write COMMITS server-side and only then reports failure.
        # The danger is a rerun treating the reported failure as "nothing was
        # written" and adding a SECOND version — the live system would keep
        # using the first, so the password an operator reads back is not the
        # password the platform holds. Idempotency-by-skipping is what stops it,
        # and this is the only scenario that puts a payload in place by a route
        # the script does not know succeeded. #240 deleted the scenarios that
        # used to arm this fault; the password path still needs it.
        print("a versions-add that commits and then reports failure is not rewritten")
        r = run(tmp, "commitfail", faults=["add.map-db-password"])
        check("the run exits nonzero", r.code != 0, r.out)
        committed = r.payload("map-db-password")
        check("the payload was stored anyway",
              committed is not None and len(committed) == 64, repr(committed))
        check("the admin secret was never reached", r.payload("map-db-admin-password") is None)
        r2 = run(tmp, "commitfail-rerun", reuse=r.state)
        check("the rerun exits 0", r2.code == 0, r2.out)
        check("the rerun leaves the committed password alone",
              r2.payload("map-db-password") == committed)
        check("and finishes the admin secret",
              r2.payload("map-db-admin-password") is not None)

        print("a warning on a SUCCESSFUL call does not read as a missing secret")
        # gcloud merges nothing, but this script merges stderr so it can classify
        # errors -- so a success-path warning lands in the same capture as the
        # state. Reading that as "no version" would write a second version over a
        # live secret.
        r = run(tmp, "warn-clean", faults=["warn"])
        check("clean run still succeeds", r.code == 0, r.out)
        r2 = run(tmp, "warn-rerun", faults=["warn"], reuse=r.state)
        check("re-run still skips rather than rewriting", "left alone" in r2.out, r2.out)

        print()
        if failures:
            print("FAILED: %d check(s)" % len(failures))
            for f in failures:
                print("  - %s" % f)
            return 1
        print("ok: bootstrap.sh behaves correctly across every simulated state")
        return 0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
