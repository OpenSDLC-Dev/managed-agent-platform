#!/usr/bin/env python3
"""Exercise env-power.sh against a fake `gcloud`, credential-free and free of charge.

Why this exists: every claim this script makes is a claim about a SEQUENCE of
gcloud calls, and neither shellcheck nor a careful read can check a sequence.
The two that would cost real money to get wrong:

  - `start` must restore the sizes `stop` saved. `variables.tf` defaults to two
    nodes a pool and staging runs one, so a bug that restores a plausible
    constant instead of the saved value doubles the bill the script exists to
    cut, and looks completely healthy from the outside. The scenarios below give
    the pools DIFFERENT sizes for exactly this reason: one uniform number cannot
    satisfy them both.
  - the two orderings. Down is nodes-then-database so pods drain before their
    database goes; up is database-then-nodes so pods do not land in front of a
    Postgres that is still starting. Both are invisible to any static check and
    both are one refactor away from inverting.

The fake models gcloud's argv and the state it mutates: node pool sizes, cluster
resource labels, and the Cloud SQL activation state. It dies on any invocation it
does not recognise, which is what makes it notice a script that has started
calling something new rather than quietly returning an empty string.

Faults are injected by dropping marker files in the state directory, so a
scenario can fail one specific call — the interesting failures here are partial
ones, where some pools are already at zero and the saved sizes must survive.

Run: make gcp-power-test
"""

import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).parent.resolve()
SCRIPT = HERE / "env-power.sh"

FAKE_GCLOUD = r'''#!/usr/bin/env python3
import os, sys, json, pathlib

S = pathlib.Path(os.environ["FAKE_STATE"])
DB = S / "state.json"
a = sys.argv[1:]

flags = {}
pos = []
for x in a:
    if x.startswith("--"):
        k, _, v = x.partition("=")
        flags[k] = v
    else:
        pos.append(x)


def die(msg, code=1):
    sys.stderr.write(msg + "\n")
    sys.exit(code)


def fault(tag):
    return (S / ("fault." + tag)).exists()


def load():
    return json.loads(DB.read_text())


def save(d):
    DB.write_text(json.dumps(d))


with (S / "calls.log").open("a") as f:
    f.write(" ".join(a) + "\n")

# Every call this script makes must carry --project. Ambient `gcloud config set
# project` is exactly the machine-local state the script promises not to depend
# on, so a missing one is a hard error here rather than a silent success.
if "--project" not in flags:
    die("fake gcloud: call without --project: " + " ".join(a), 2)
d = load()
proj = flags["--project"]
if proj != d["project"]:
    die("ERROR: (gcloud) project [%s] not found or permission denied." % proj)


def projection():
    fmt = flags.get("--format", "")
    return fmt[len("value("):-1] if fmt.startswith("value(") else fmt


def cluster(name):
    return d["clusters"].get(name)


if pos[:3] == ["container", "clusters", "list"]:
    # `--filter=name=X`; a name that matches nothing prints nothing and exits 0,
    # which is how the script tells "wrong prefix" from "no permission".
    want = flags.get("--filter", "").split("=", 1)[-1]
    c = cluster(want)
    if c:
        print(c["location"])
    sys.exit(0)

if pos[:3] == ["container", "clusters", "describe"]:
    c = cluster(pos[3]) or die("ERROR: (gcloud) NOT_FOUND: cluster %s" % pos[3])
    p = projection()
    if p.startswith("resourceLabels."):
        # A key that is not present prints an empty line, exit 0 -- verified
        # against the live cluster, and the whole basis for "refuse rather than
        # guess" being reachable at all.
        print(c["labels"].get(p[len("resourceLabels."):], ""))
    elif p == "resourceLabels":
        print(";".join("%s=%s" % kv for kv in sorted(c["labels"].items())))
    else:
        die("fake gcloud: unhandled clusters describe projection: " + p, 2)
    sys.exit(0)

if pos[:3] == ["container", "clusters", "update"]:
    c = cluster(pos[3]) or die("ERROR: (gcloud) NOT_FOUND: cluster %s" % pos[3])
    if "--update-labels" in flags and "--remove-labels" in flags:
        die("ERROR: (gcloud.container.clusters.update) argument --update-labels: "
            "At most one of --update-labels | --remove-labels may be specified.", 2)
    if fault("update-labels"):
        die("ERROR: (gcloud.container.clusters.update) UNAVAILABLE: backend error")
    if "--update-labels" in flags:
        # Merge, not replace: --update-labels adds and overwrites the keys named
        # and leaves every other one alone. Replacing here would drop
        # goog-terraform-provisioned, and would also silently make a resumed stop
        # forget the pool it had already parked.
        for kv in flags["--update-labels"].split(","):
            k, _, v = kv.partition("=")
            c["labels"][k] = v
    elif "--remove-labels" in flags:
        for k in flags["--remove-labels"].split(","):
            c["labels"].pop(k, None)
    else:
        die("fake gcloud: clusters update with no label flag: " + " ".join(a), 2)
    save(d)
    sys.exit(0)

if pos[:3] == ["container", "clusters", "resize"]:
    c = cluster(pos[3]) or die("ERROR: (gcloud) NOT_FOUND: cluster %s" % pos[3])
    pool = flags["--node-pool"]
    if pool not in c["pools"]:
        die("ERROR: (gcloud) NOT_FOUND: node pool %s" % pool)
    if fault("resize." + pool):
        die("ERROR: (gcloud.container.clusters.resize) UNAVAILABLE: backend error")
    c["pools"][pool] = int(flags["--num-nodes"])
    save(d)
    sys.exit(0)

if pos[:3] == ["container", "node-pools", "list"]:
    c = cluster(flags["--cluster"]) or die("ERROR: (gcloud) NOT_FOUND")
    for name in c["pools"]:
        print(name)
    sys.exit(0)

if pos[:3] == ["container", "node-pools", "describe"]:
    c = cluster(flags["--cluster"]) or die("ERROR: (gcloud) NOT_FOUND")
    print(c["pools"][pos[3]])
    sys.exit(0)

if pos[:3] == ["sql", "instances", "describe"]:
    inst = d["sql"].get(pos[3]) or die("ERROR: (gcloud) NOT_FOUND: instance %s" % pos[3])
    inst["describes"] += 1
    # A started instance does not answer immediately. `flip_after` is how many
    # describes it takes to come up, so the wait loop is exercised rather than
    # short-circuited on the first poll.
    if inst["state"] == "PENDING" and inst["describes"] >= inst["flip_after"]:
        inst["state"] = "RUNNABLE"
    save(d)
    print(inst["state"])
    sys.exit(0)

if pos[:3] == ["sql", "instances", "patch"]:
    inst = d["sql"].get(pos[3]) or die("ERROR: (gcloud) NOT_FOUND: instance %s" % pos[3])
    policy = flags["--activation-policy"]
    if policy == "NEVER":
        inst["state"] = "STOPPED"
    elif policy == "ALWAYS":
        inst["state"] = "PENDING" if inst["flip_after"] else "RUNNABLE"
        inst["describes"] = 0
    else:
        die("fake gcloud: unhandled activation policy: " + policy, 2)
    save(d)
    sys.exit(0)

die("fake gcloud: unhandled invocation: " + " ".join(a), 2)
'''

failures = []


class Run:
    def __init__(self, state, proc):
        self.state, self.proc = state, proc
        self.out = proc.stdout + proc.stderr
        self.code = proc.returncode

    @property
    def db(self):
        return json.loads((self.state / "state.json").read_text())

    def pools(self, cluster="map-staging"):
        return self.db["clusters"][cluster]["pools"]

    def labels(self, cluster="map-staging"):
        return self.db["clusters"][cluster]["labels"]

    def sql(self, inst="map-staging"):
        return self.db["sql"][inst]["state"]

    @property
    def calls(self):
        return (self.state / "calls.log").read_text().splitlines()

    def first(self, needle):
        """Index of the first call containing `needle`, or -1.

        Order is the whole point of several assertions below, and comparing
        indices is the only way to state it.
        """
        for i, ln in enumerate(self.calls):
            if needle in ln:
                return i
        return -1

    def last(self, needle):
        """Index of the last call containing `needle`, or -1.

        Needed where `first` is too weak to say anything: the wait loop's first
        poll happens before the database is even started, so only the LAST one
        marks the moment it reported ready.
        """
        for i in range(len(self.calls) - 1, -1, -1):
            if needle in self.calls[i]:
                return i
        return -1


def new_state(tmp, name, pools=None, sql="RUNNABLE", flip_after=0,
              labels=None, project="p", cluster="map-staging"):
    state = pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix=name + "."))
    db = {
        "project": project,
        "clusters": {
            cluster: {
                "location": "us-central1-a",
                # Terraform stamps this one and does not declare resourceLabels,
                # so it is what a merge has to leave alone.
                "labels": dict(labels or {"goog-terraform-provisioned": "true"}),
                "pools": dict(pools or {"platform": 3, "sandbox": 1}),
            }
        },
        "sql": {cluster: {"state": sql, "flip_after": flip_after, "describes": 0}},
    }
    (state / "state.json").write_text(json.dumps(db))
    return state


def run(tmp, state, action, faults=(), env_extra=None, project="p"):
    for stale in state.glob("fault.*"):
        stale.unlink()
    for f in faults:
        (state / ("fault." + f)).touch()
    (state / "calls.log").write_text("")
    env = dict(os.environ)
    env["PATH"] = str(pathlib.Path(tmp) / "bin") + os.pathsep + env["PATH"]
    env["FAKE_STATE"] = str(state)
    if project is not None:
        env["PROJECT"] = project
    else:
        env.pop("PROJECT", None)
    env.pop("NAME_PREFIX", None)
    env.pop("NODES", None)
    # Seconds, not minutes: the wait path has to be reachable in a test.
    env["SQL_POLL"] = "1"
    env["SQL_TIMEOUT"] = "3"
    for k, v in (env_extra or {}).items():
        env[k] = v
    return Run(state, subprocess.run(["bash", str(SCRIPT), action], env=env,
                                     text=True, capture_output=True, timeout=120))


def check(label, cond, detail=""):
    if cond:
        print("  ok   %s" % label)
    else:
        print("  FAIL %s%s" % (label, ("\n       " + str(detail).replace("\n", "\n       ")) if detail else ""))
        failures.append(label)


def main():
    if not SCRIPT.exists():
        print("env-power.sh not found next to this test", file=sys.stderr)
        return 1
    tmp = tempfile.mkdtemp(prefix="env-power-test.")
    try:
        binp = pathlib.Path(tmp) / "bin"
        binp.mkdir()
        (binp / "gcloud").write_text(FAKE_GCLOUD)
        (binp / "gcloud").chmod(0o755)

        print("a stop parks both pools and the database, in that order")
        st = new_state(tmp, "stop")
        r = run(tmp, st, "stop")
        check("exits 0", r.code == 0, r.out)
        check("both pools are at zero", r.pools() == {"platform": 0, "sandbox": 0}, r.pools())
        check("the database is stopped", r.sql() == "STOPPED", r.sql())
        # The ordering claim in the script's header, as an assertion. A pod that
        # loses its database mid-query is a worse way to stop than a drained one.
        check("every resize precedes the database patch",
              r.first("clusters resize") < r.first("sql instances patch"),
              r.calls)
        check("the sizes are saved BEFORE the first resize",
              r.first("--update-labels") < r.first("clusters resize"), r.calls)

        print("the saved sizes are the real ones, and survive the merge")
        check("platform's size is recorded", r.labels().get("power-saved-platform") == "3", r.labels())
        check("sandbox's size is recorded", r.labels().get("power-saved-sandbox") == "1", r.labels())
        # --labels would have replaced; --update-labels merges. Getting this
        # wrong would strip a label Terraform put there.
        check("terraform's own label is untouched",
              r.labels().get("goog-terraform-provisioned") == "true", r.labels())

        print("a start restores exactly what was parked, database first")
        r = run(tmp, st, "start")
        check("exits 0", r.code == 0, r.out)
        # The flagship assertion: two different sizes, both restored. Any
        # constant -- 1, 2, or variables.tf's default -- fails this.
        check("both pools are back at their own sizes",
              r.pools() == {"platform": 3, "sandbox": 1}, r.pools())
        check("the database is running", r.sql() == "RUNNABLE", r.sql())
        check("the database is started before any pool is resized",
              r.first("sql instances patch") < r.first("clusters resize"), r.calls)
        check("the saved sizes are cleared once restored",
              not any(k.startswith("power-saved-") for k in r.labels()), r.labels())

        print("start waits for the database instead of trusting the patch")
        st = new_state(tmp, "wait", pools={"platform": 0, "sandbox": 0},
                       sql="STOPPED", flip_after=3,
                       labels={"power-saved-platform": "3", "power-saved-sandbox": "1"})
        r = run(tmp, st, "start", env_extra={"SQL_TIMEOUT": "30"})
        check("exits 0", r.code == 0, r.out)
        check("it polled more than once", len([c for c in r.calls if "sql instances describe" in c]) >= 3, r.calls)
        # Measured against the LAST poll, not the first: the first one happens
        # before the instance is even started, so it proves nothing.
        check("no pool was resized before the database reported ready",
              r.first("clusters resize") > r.last("sql instances describe"), r.calls)
        check("both pools are back", r.pools() == {"platform": 3, "sandbox": 1}, r.pools())

        print("a database that never comes up leaves the pools at zero")
        # Resizing into a dead Postgres buys nothing but a CrashLoopBackOff and
        # a node bill, so giving up has to mean giving up on the whole start.
        st = new_state(tmp, "nevercomesup", pools={"platform": 0, "sandbox": 0},
                       sql="STOPPED", flip_after=9999,
                       labels={"power-saved-platform": "3", "power-saved-sandbox": "1"})
        r = run(tmp, st, "start")
        check("refuses", r.code != 0, r.out)
        check("resizes nothing", r.first("clusters resize") == -1, r.calls)
        check("the pools stay at zero", r.pools() == {"platform": 0, "sandbox": 0}, r.pools())
        check("the saved sizes are still there for the next attempt",
              r.labels().get("power-saved-platform") == "3", r.labels())

        print("start refuses to guess when the saved sizes are gone")
        st = new_state(tmp, "noguess", pools={"platform": 0, "sandbox": 0}, sql="STOPPED")
        r = run(tmp, st, "start")
        check("refuses", r.code != 0, r.out)
        check("says why", "refusing to guess" in r.out, r.out)
        check("names the way out", "NODES=" in r.out, r.out)
        check("leaves the pools at zero", r.pools() == {"platform": 0, "sandbox": 0}, r.pools())

        print("...and takes an explicit size when given one")
        r = run(tmp, st, "start", env_extra={"NODES": "2"})
        check("exits 0", r.code == 0, r.out)
        check("uses the size it was given", r.pools() == {"platform": 2, "sandbox": 2}, r.pools())

        print("a stop interrupted halfway keeps the sizes it had already saved")
        # The failure that would quietly destroy the saved state: the re-run sees
        # platform already at zero and must not record THAT as its parked size.
        st = new_state(tmp, "halfway")
        r = run(tmp, st, "stop", faults=["resize.sandbox"])
        check("the interrupted run reports failure", r.code != 0, r.out)
        check("platform was parked", r.pools()["platform"] == 0, r.pools())
        check("sandbox was not", r.pools()["sandbox"] == 1, r.pools())
        check("the database was never reached", r.sql() == "RUNNABLE", r.sql())
        r2 = run(tmp, st, "stop")
        check("the re-run finishes the job", r2.code == 0, r2.out)
        check("both pools are parked", r2.pools() == {"platform": 0, "sandbox": 0}, r2.pools())
        check("platform's ORIGINAL size survived, not the zero it was re-read at",
              r2.labels().get("power-saved-platform") == "3", r2.labels())
        r3 = run(tmp, st, "start")
        check("and a start brings both back to where they began",
              r3.pools() == {"platform": 3, "sandbox": 1}, r3.pools())

        print("a failed size save parks nothing")
        # If the sizes cannot be recorded, zeroing the pools would strand them:
        # start would have nothing to restore from and would refuse.
        st = new_state(tmp, "savefails")
        r = run(tmp, st, "stop", faults=["update-labels"])
        check("refuses", r.code != 0, r.out)
        check("resizes nothing", r.first("clusters resize") == -1, r.calls)
        check("the pools are untouched", r.pools() == {"platform": 3, "sandbox": 1}, r.pools())

        print("stopping what is already parked is a no-op, not a re-save")
        st = new_state(tmp, "already", pools={"platform": 0, "sandbox": 0}, sql="STOPPED",
                       labels={"power-saved-platform": "3", "power-saved-sandbox": "1"})
        r = run(tmp, st, "stop")
        check("exits 0", r.code == 0, r.out)
        check("says so", "already parked" in r.out, r.out)
        check("writes no labels", r.first("--update-labels") == -1, r.calls)
        check("the saved sizes are the original ones",
              r.labels().get("power-saved-platform") == "3", r.labels())

        print("starting what is already running changes nothing")
        st = new_state(tmp, "alreadyup")
        r = run(tmp, st, "start")
        check("exits 0", r.code == 0, r.out)
        check("resizes nothing", r.first("clusters resize") == -1, r.calls)
        check("the pools keep their sizes", r.pools() == {"platform": 3, "sandbox": 1}, r.pools())

        print("status reports without changing anything")
        st = new_state(tmp, "status", pools={"platform": 0, "sandbox": 0}, sql="STOPPED",
                       labels={"power-saved-platform": "3", "power-saved-sandbox": "1"})
        r = run(tmp, st, "status")
        check("exits 0", r.code == 0, r.out)
        check("names both pools", "platform" in r.out and "sandbox" in r.out, r.out)
        check("reports the database state", "STOPPED" in r.out, r.out)
        check("shows what they were parked from", "parked from 3" in r.out, r.out)
        check("issues no write", not any(w in c for c in r.calls
                                         for w in ("resize", "update", "patch")), r.calls)

        print("pools are discovered, not hard-coded to platform and sandbox")
        # main.tf declares two today. A third must be parked without this script
        # being edited -- and, just as important, must be RESTORED.
        st = new_state(tmp, "threepools", pools={"platform": 3, "sandbox": 1, "gpu": 5})
        r = run(tmp, st, "stop")
        check("all three are parked", r.pools() == {"platform": 0, "sandbox": 0, "gpu": 0}, r.pools())
        check("the third one's size is saved", r.labels().get("power-saved-gpu") == "5", r.labels())
        r2 = run(tmp, st, "start")
        check("and all three come back", r2.pools() == {"platform": 3, "sandbox": 1, "gpu": 5}, r2.pools())

        print("nothing is hard-coded: the prefix names every resource")
        # Requirement from the ask this script answers -- it has to run against
        # any deployment, on any machine, with no edits.
        st = new_state(tmp, "prefix", cluster="acme-staging")
        r = run(tmp, st, "stop", env_extra={"NAME_PREFIX": "acme"})
        check("exits 0", r.code == 0, r.out)
        check("parks the acme cluster", r.pools("acme-staging") == {"platform": 0, "sandbox": 0},
              r.pools("acme-staging"))
        check("never mentions map-staging", not any("map-staging" in c for c in r.calls), r.calls)

        print("a cluster that is not there is named as such")
        st = new_state(tmp, "nocluster", cluster="other-staging")
        r = run(tmp, st, "stop")
        check("refuses", r.code != 0, r.out)
        check("names the prefix as the likely cause", "NAME_PREFIX" in r.out, r.out)
        check("touches nothing", r.first("resize") == -1 and r.first("patch") == -1, r.calls)

        print("the arguments are checked before anything else is")
        st = new_state(tmp, "args")
        r = run(tmp, st, "stop", project=None)
        check("a missing PROJECT refuses", r.code == 2, r.out)
        check("says what to set", "PROJECT is required" in r.out, r.out)
        check("makes no call at all", not (st / "calls.log").read_text().strip(),
              (st / "calls.log").read_text())
        r = run(tmp, st, "bounce")
        check("an unknown action refuses", r.code == 2, r.out)
        check("prints usage", "usage:" in r.out, r.out)

        print()
        if failures:
            print("FAILED: %d check(s)" % len(failures))
            for f in failures:
                print("  - %s" % f)
            return 1
        print("ok: env-power.sh parks and revives correctly across every simulated state")
        return 0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
