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
  - a GCS HMAC secret is returned exactly ONCE, at creation, so a key whose secret
    was not stored is permanently unusable. Every rollback assertion below is
    about that irreversibility.

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
SECRETS = ["map-db-password", "map-blob-access-key", "map-blob-secret-key"]

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

if a[:3] == ["storage", "hmac", "list"]:
    if fault("hmaclist") and list(S.glob("key.*")):
        die("ERROR: (gcloud.storage.hmac.list) UNAVAILABLE", 1)
    for k in sorted(p.name[4:] for p in S.glob("key.*")):
        print(k)
    sys.exit(0)

if a[:3] == ["storage", "hmac", "create"]:
    if fault("createfails"):
        # Fails without creating anything -- nothing to roll back.
        die("ERROR: (gcloud.storage.hmac.create) PERMISSION_DENIED", 1)
    kid = "GOOG" + str(len(list(S.glob("key.*"))) + 1) * 4
    (S / ("key." + kid)).write_text("active")
    if fault("createorphans"):
        # Commits server-side, then reports failure: the orphan window.
        die("ERROR: (gcloud.storage.hmac.create) connection reset reading response", 1)
    if fault("badjson"):
        print("<html>502 Bad Gateway</html>")
        sys.exit(0)
    print(json.dumps({"metadata": {"accessId": kid, "state": "ACTIVE"},
                      "secret": "s3cr3t/SECRET+value=="}))
    sys.exit(0)

if a[:3] == ["storage", "hmac", "update"] or a[:3] == ["storage", "hmac", "delete"]:
    if fault("deactivatefails") and a[2] == "update":
        die("ERROR: (gcloud.storage.hmac.update) UNAVAILABLE", 1)
    if a[2] == "delete":
        # A key must be INACTIVE before it can be deleted.
        if (S / ("key." + a[3])).read_text() != "inactive":
            die("ERROR: (gcloud.storage.hmac.delete) HMAC key must be INACTIVE to delete", 1)
        (S / ("key." + a[3])).unlink()
    else:
        (S / ("key." + a[3])).write_text("inactive")
    with (S / "deleted.log").open("a") as f:
        f.write(a[2] + " " + a[3] + "\n")
    sys.exit(0)

die("fake gcloud: unhandled invocation: " + " ".join(a))
'''

failures = []


class Run:
    def __init__(self, state, proc):
        self.state, self.proc = state, proc
        self.out = proc.stdout + proc.stderr
        self.code = proc.returncode

    def keys(self):
        return sorted(p.name[4:] for p in self.state.glob("key.*"))

    def payload(self, name):
        p = self.state / ("payload." + name)
        return p.read_bytes() if p.exists() else None

    def deletions(self):
        p = self.state / "deleted.log"
        return [ln for ln in p.read_text().splitlines() if ln.startswith("delete ")] if p.exists() else []


def run(tmp, name, faults=(), containers=SECRETS, keys=(), versions=(), reuse=None):
    state = reuse if reuse else pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix=name + "."))
    if not reuse:
        for c in containers:
            (state / ("container." + c)).touch()
        for k in keys:
            (state / ("key." + k)).write_text("active")
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
        (binp / "openssl").write_text(
            '#!/usr/bin/env bash\n'
            'if [[ -e "$FAKE_STATE/fault.openssl" ]]; then exit 1; fi\n'
            'exec /usr/bin/openssl "$@"\n'
        )
        (binp / "openssl").chmod(0o755)

        print("a clean run stores every secret")
        r = run(tmp, "clean")
        check("exits 0", r.code == 0, r.out)
        check("writes all three secrets", all(r.payload(s) is not None for s in SECRETS))
        check("creates exactly one HMAC key", len(r.keys()) == 1, str(r.keys()))
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

        print("re-running is a no-op rather than an overwrite")
        before = {s: r.payload(s) for s in SECRETS}
        r2 = run(tmp, "rerun", reuse=r.state)
        check("exits 0", r2.code == 0, r2.out)
        check("says it left them alone", "left alone" in r2.out, r2.out)
        check("changes no payload", all(r2.payload(s) == before[s] for s in SECRETS))
        check("creates no second key", len(r2.keys()) == 1, str(r2.keys()))

        print("a missing foundation is named as such")
        r = run(tmp, "nofoundation", containers=[])
        check("refuses", r.code != 0)
        check("names gcp-foundation-apply", "gcp-foundation-apply" in r.out, r.out)

        print("an unreadable secret aborts instead of writing a second version")
        r = run(tmp, "unreadable", faults=["describe"])
        check("refuses", r.code != 0)
        check("writes nothing", all(r.payload(s) is None for s in SECRETS))

        print("half an HMAC pair is refused rather than guessed at")
        r = run(tmp, "halfpair", versions=["map-blob-access-key"])
        check("refuses", r.code != 0)
        check("explains why", "cannot authenticate" in r.out, r.out)

        print("a write that commits and then reports failure keeps its key")
        r = run(tmp, "committhenfail", faults=["add.map-blob-secret-key"])
        check("deletes nothing", r.deletions() == [], str(r.deletions()))
        check("keeps the key", len(r.keys()) == 1, str(r.keys()))
        check("says so", "NOT rolling back" in r.out, r.out)
        r2 = run(tmp, "committhenfail-rerun", reuse=r.state)
        check("and the next run recovers cleanly", r2.code == 0 and "left alone" in r2.out, r2.out)

        print("an unparseable create response rolls the new key back")
        r = run(tmp, "badjson", faults=["badjson"])
        check("refuses", r.code != 0)
        check("deletes exactly one key", len(r.deletions()) == 1, str(r.deletions()))
        check("leaves no key behind", r.keys() == [], str(r.keys()))

        print("a key created in the window before the response is still rolled back")
        r = run(tmp, "orphanwindow", faults=["createorphans"])
        check("refuses", r.code != 0)
        check("deletes the orphan", len(r.deletions()) == 1, str(r.deletions()))
        check("leaves no key behind", r.keys() == [], str(r.keys()))

        print("a create that never took effect says nothing alarming")
        r = run(tmp, "createfails", faults=["createfails"])
        check("refuses", r.code != 0)
        check("deletes nothing", r.deletions() == [], str(r.deletions()))
        check("does not claim a rollback", "rolling back" not in r.out, r.out)
        check("does not demand manual cleanup", "COULD NOT identify" not in r.out, r.out)

        print("a rollback that cannot list refuses to guess")
        r = run(tmp, "listfails", faults=["badjson", "hmaclist"])
        check("refuses", r.code != 0)
        check("deletes nothing", r.deletions() == [], str(r.deletions()))
        check("prints manual recovery", "COULD NOT identify" in r.out, r.out)
        check("does not claim a deletion", "rolling back" not in r.out, r.out)

        print("a failed generator never lets an empty password be stored")
        r = run(tmp, "opensslfails", faults=["openssl"])
        check("refuses", r.code != 0)
        check("stores no password at all", r.payload("map-db-password") is None,
              repr(r.payload("map-db-password")))
        r2 = run(tmp, "opensslfails-rerun", reuse=r.state)
        check("and the next run still creates a real one",
              r2.payload("map-db-password") is not None and len(r2.payload("map-db-password")) == 64,
              repr(r2.payload("map-db-password")))

        print("a warning on a SUCCESSFUL call does not read as a missing secret")
        # gcloud merges nothing, but this script merges stderr so it can classify
        # errors -- so a success-path warning lands in the same capture as the
        # state. Reading that as "no version" would write a second version over a
        # live secret, and would make the rollback treat a stored pair as unstored.
        r = run(tmp, "warn-clean", faults=["warn"])
        check("clean run still succeeds", r.code == 0, r.out)
        r2 = run(tmp, "warn-rerun", faults=["warn"], reuse=r.state)
        check("re-run still skips rather than rewriting", "left alone" in r2.out, r2.out)
        check("writes no second key", len(r2.keys()) == 1, str(r2.keys()))

        print("a rollback that cannot deactivate says the key is still there")
        r = run(tmp, "deactivatefails", faults=["badjson", "deactivatefails"])
        check("refuses", r.code != 0)
        check("reports the failed rollback", "ROLLBACK FAILED" in r.out, r.out)
        check("names the key id", "GOOG" in r.out, r.out)
        check("leaves the key (honestly reported)", len(r.keys()) == 1, str(r.keys()))

        print("a pre-existing key created out of band is never deleted")
        r = run(tmp, "preexisting", faults=["badjson"], keys=["GOOGPREEXISTING"])
        check("keeps the pre-existing key", "GOOGPREEXISTING" in r.keys(), str(r.keys()))
        check("deletes only the new one", all("PREEXISTING" not in d for d in r.deletions()),
              str(r.deletions()))

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
