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

THE FAKE MODELS FOUR THINGS THAT REAL GCLOUD ACTUALLY DOES, each of which the
script would otherwise get wrong in a way no static check could see:

  - `--update-labels` REPLACES the cluster's label set. It is not a merge,
    whatever the name suggests: `UpdateLabelsCommon` builds the SetLabelsRequest
    from the flag's pairs alone and reads the cluster only for its fingerprint
    (googlecloudsdk/api_lib/container/api_adapter.py). An earlier version of this
    script assumed a merge, and under that assumption a resumed stop deletes the
    size the interrupted attempt had already saved.
  - `--remove-labels` errors on a label that is not there, and on a cluster with
    no labels at all (`RemoveLabelsCommon`, same file). It is a read-modify-write
    and therefore safe for deletion — but only if handed keys that exist.
  - a `value(resourceLabels.<key>)` projection on a MISSING key prints an empty
    line and exits 0. Verified live. It is the whole basis for `start` being able
    to tell "no saved size" from "could not ask".
  - gcloud on Windows terminates its lines CRLF. A pool name read with the CR
    still attached produces `node pool "platform\r" not found` on every call
    after the listing — observed against the real cluster, not theorised.

Projections are enforced rather than ignored: a `describe` whose `--format` asks
for the wrong field is an error here, because a fake that prints the right answer
to the wrong question cannot catch the script asking it.

Faults are injected by dropping marker files in the state directory, so a
scenario can fail one specific call. The interesting failures are partial ones,
where some pools are already parked or already restored and the saved sizes have
to survive.

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


def out(line):
    # Windows gcloud terminates lines CRLF. Under the `crlf` fault this fake does
    # too, so the script's `tr -d '\r'` is exercised rather than assumed.
    sys.stdout.write(str(line) + ("\r\n" if fault("crlf") else "\n"))


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
if flags["--project"] != d["project"]:
    die("ERROR: (gcloud) project [%s] not found or permission denied."
        % flags["--project"])


def projection(expected):
    """The --format must ask for `expected`; anything else is the script's bug."""
    fmt = flags.get("--format", "")
    if fmt != "value(%s)" % expected:
        die("fake gcloud: %s expects --format=value(%s), got %r"
            % (" ".join(pos[:3]), expected, fmt), 2)


def cluster(name):
    c = d["clusters"].get(name)
    if c is None:
        die("ERROR: (gcloud) NOT_FOUND: cluster %s" % name)
    return c


def render_labels(labels):
    return ";".join("%s=%s" % kv for kv in sorted(labels.items()))


if pos[:3] == ["container", "clusters", "list"]:
    projection("location")
    if fault("list-denied"):
        die("ERROR: (gcloud.container.clusters.list) ResponseError: code=403, "
            "message=Required 'container.clusters.list' permission.")
    # `--filter=name=X`; a name that matches nothing prints nothing and exits 0,
    # which is how the script tells "wrong prefix" from "no permission".
    want = flags.get("--filter", "").split("=", 1)[-1]
    if want in d["clusters"]:
        out(d["clusters"][want]["location"])
    sys.exit(0)

if pos[:3] == ["container", "clusters", "describe"]:
    c = cluster(pos[3])
    fmt = flags.get("--format", "")
    if fmt == "value(resourceLabels)":
        out(render_labels(c["labels"]))
    elif fmt.startswith("value(resourceLabels."):
        key = fmt[len("value(resourceLabels."):-1]
        out(c["labels"].get(key, ""))
    else:
        die("fake gcloud: clusters describe: unhandled --format %r" % fmt, 2)
    sys.exit(0)

if pos[:3] == ["container", "clusters", "update"]:
    c = cluster(pos[3])
    if "--update-labels" in flags and "--remove-labels" in flags:
        die("ERROR: (gcloud.container.clusters.update) argument --update-labels: "
            "Exactly one of (--update-labels | --remove-labels) must be specified.", 2)
    if fault("update-labels"):
        die("ERROR: (gcloud.container.clusters.update) UNAVAILABLE: backend error")
    if "--update-labels" in flags:
        # REPLACE, not merge -- see this file's docstring. Anything the script
        # leaves out of the flag is deleted from the cluster.
        new = {}
        for kv in flags["--update-labels"].split(","):
            k, _, v = kv.partition("=")
            if not k or not v:
                die("ERROR: (gcloud.container.clusters.update) argument "
                    "--update-labels: Bad value [%s]" % kv, 2)
            new[k] = v
        c["labels"] = new
    elif "--remove-labels" in flags:
        if not c["labels"]:
            die("ERROR: (gcloud.container.clusters.update) No labels found on "
                "cluster [%s]." % pos[3])
        for k in flags["--remove-labels"].split(","):
            if k not in c["labels"]:
                die("ERROR: (gcloud.container.clusters.update) No label named "
                    "'%s' found on cluster [%s]." % (k, pos[3]))
            del c["labels"][k]
    else:
        die("fake gcloud: clusters update with no label flag: " + " ".join(a), 2)
    save(d)
    sys.exit(0)

if pos[:3] == ["container", "clusters", "resize"]:
    c = cluster(pos[3])
    pool = flags["--node-pool"]
    if pool not in c["pools"]:
        die("ERROR: (gcloud) NOT_FOUND: node pool \"%s\" not found" % pool)
    if fault("resize." + pool):
        die("ERROR: (gcloud.container.clusters.resize) UNAVAILABLE: backend error")
    c["pools"][pool] = int(flags["--num-nodes"])
    save(d)
    sys.exit(0)

if pos[:3] == ["container", "node-pools", "list"]:
    projection("name")
    c = cluster(flags["--cluster"])
    names = list(c["pools"])
    if fault("list-partial"):
        # A listing that prints some pools and THEN fails -- pagination dying
        # halfway. The danger is a caller that cannot see the exit status and
        # parks only what it managed to read.
        out(names[0])
        die("ERROR: (gcloud.container.node-pools.list) ResponseError: code=503")
    for name in names:
        out(name)
    sys.exit(0)

if pos[:3] == ["container", "node-pools", "describe"]:
    projection("initialNodeCount")
    c = cluster(flags["--cluster"])
    if pos[3] not in c["pools"]:
        die("ERROR: (gcloud) NOT_FOUND: node pool \"%s\" not found" % pos[3])
    out(c["pools"][pos[3]])
    sys.exit(0)

if pos[:3] == ["sql", "instances", "describe"]:
    projection("state")
    inst = d["sql"].get(pos[3])
    if inst is None:
        die("ERROR: (gcloud) NOT_FOUND: instance %s" % pos[3])
    inst["describes"] += 1
    # A started instance does not answer immediately. `flip_after` is how many
    # describes it takes to come up, so the wait loop is exercised rather than
    # short-circuited on the first poll.
    if inst["state"] == "PENDING" and inst["describes"] >= inst["flip_after"]:
        inst["state"] = "RUNNABLE"
    save(d)
    out(inst["state"])
    sys.exit(0)

if pos[:3] == ["sql", "instances", "patch"]:
    inst = d["sql"].get(pos[3])
    if inst is None:
        die("ERROR: (gcloud) NOT_FOUND: instance %s" % pos[3])
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

TF_LABEL = {"goog-terraform-provisioned": "true"}


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

    def saved(self, cluster="map-staging"):
        return {k[len("power-saved-"):]: v for k, v in self.labels(cluster).items()
                if k.startswith("power-saved-")}

    def sql(self, inst="map-staging"):
        return self.db["sql"][inst]["state"]

    @property
    def calls(self):
        return (self.state / "calls.log").read_text().splitlines()

    def first(self, needle):
        for i, ln in enumerate(self.calls):
            if needle in ln:
                return i
        return -1

    def last(self, needle):
        """Index of the last call containing `needle`, or -1.

        `first` is too weak for most ordering claims here: the wait loop's first
        poll happens before the database is even started, and a stop that resized
        one pool before the database and one after would satisfy a `first`-based
        check while doing exactly the thing the check forbids.
        """
        for i in range(len(self.calls) - 1, -1, -1):
            if needle in self.calls[i]:
                return i
        return -1

    def count(self, needle):
        return len([c for c in self.calls if needle in c])


def new_state(tmp, name, pools=None, sql="RUNNABLE", flip_after=0,
              labels=None, saved=None, project="p", cluster="map-staging"):
    state = pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix=name + "."))
    lab = dict(TF_LABEL if labels is None else labels)
    for pool, size in (saved or {}).items():
        lab["power-saved-" + pool] = str(size)
    db = {
        "project": project,
        "clusters": {
            cluster: {
                "location": "us-central1-a",
                "labels": lab,
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
        # Measured against the LAST resize: a stop that drained one pool, stopped
        # the database, and only then drained the other would satisfy a check on
        # the first one while doing the thing the ordering forbids.
        check("EVERY resize precedes the database patch",
              r.last("clusters resize") < r.first("sql instances patch"), r.calls)
        check("the sizes are saved before the first resize",
              r.first("--update-labels") < r.first("clusters resize"), r.calls)

        print("the saved sizes are the real ones, and no other label is lost")
        check("platform's size is recorded", r.saved().get("platform") == "3", r.labels())
        check("sandbox's size is recorded", r.saved().get("sandbox") == "1", r.labels())
        # --update-labels REPLACES, so preserving this one is the script sending
        # the complete set rather than gcloud merging on its behalf.
        check("terraform's own label survives the replace",
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
        check("the saved sizes are cleared once restored", r.saved() == {}, r.labels())
        check("and clearing them kept every other label",
              r.labels() == dict(TF_LABEL), r.labels())

        print("start waits for the database instead of trusting the patch")
        st = new_state(tmp, "wait", pools={"platform": 0, "sandbox": 0},
                       sql="STOPPED", flip_after=3, saved={"platform": 3, "sandbox": 1})
        r = run(tmp, st, "start", env_extra={"SQL_TIMEOUT": "30"})
        check("exits 0", r.code == 0, r.out)
        check("it polled more than once", r.count("sql instances describe") >= 3, r.calls)
        check("no pool was resized before the database reported ready",
              r.first("clusters resize") > r.last("sql instances describe"), r.calls)
        check("both pools are back", r.pools() == {"platform": 3, "sandbox": 1}, r.pools())

        print("a database that never comes up leaves the pools at zero")
        # Resizing into a dead Postgres buys nothing but a CrashLoopBackOff and
        # a node bill, so giving up has to mean giving up on the whole start.
        st = new_state(tmp, "nevercomesup", pools={"platform": 0, "sandbox": 0},
                       sql="STOPPED", flip_after=9999, saved={"platform": 3, "sandbox": 1})
        r = run(tmp, st, "start")
        check("refuses", r.code != 0, r.out)
        check("resizes nothing", r.first("clusters resize") == -1, r.calls)
        check("the pools stay at zero", r.pools() == {"platform": 0, "sandbox": 0}, r.pools())
        check("the saved sizes are still there for the next attempt",
              r.saved() == {"platform": "3", "sandbox": "1"}, r.labels())

        print("start refuses to guess when a saved size is gone")
        st = new_state(tmp, "noguess", pools={"platform": 0, "sandbox": 0}, sql="STOPPED")
        r = run(tmp, st, "start")
        check("refuses", r.code != 0, r.out)
        check("says why", "refusing to guess" in r.out, r.out)
        check("names the way out", "NODES=" in r.out, r.out)
        check("leaves the pools at zero", r.pools() == {"platform": 0, "sandbox": 0}, r.pools())
        # The refusal has to come BEFORE the database is started, or "refusing"
        # has already left the environment part-live and billing.
        check("and never started the database", r.first("sql instances patch") == -1, r.calls)

        print("...and refuses even when only ONE pool's size is missing")
        # The dangerous shape: the first pool resolves, so a pool-by-pool check
        # would restore it and only then discover the second has nothing.
        st = new_state(tmp, "onemissing", pools={"platform": 0, "sandbox": 0},
                       sql="STOPPED", saved={"platform": 3})
        r = run(tmp, st, "start")
        check("refuses", r.code != 0, r.out)
        check("names the pool that is missing", "sandbox" in r.out, r.out)
        check("resizes nothing at all", r.first("clusters resize") == -1, r.calls)
        check("never started the database", r.first("sql instances patch") == -1, r.calls)
        check("kept the size it did have", r.saved() == {"platform": "3"}, r.labels())

        print("...and takes an explicit size when given one")
        st = new_state(tmp, "explicit", pools={"platform": 0, "sandbox": 0}, sql="STOPPED")
        r = run(tmp, st, "start", env_extra={"NODES": "2"})
        check("exits 0", r.code == 0, r.out)
        check("uses the size it was given", r.pools() == {"platform": 2, "sandbox": 2}, r.pools())
        # No power-saved label ever existed here, so there is nothing to remove --
        # and asking real gcloud to remove an absent label is an error.
        check("removes no label it never wrote", r.first("--remove-labels") == -1, r.calls)
        check("the cluster's other labels are untouched", r.labels() == dict(TF_LABEL), r.labels())

        print("a size that is not a positive integer is refused")
        for bad in ("0", "-1", "two", "1.5", ""):
            st = new_state(tmp, "bad" + (bad or "empty"),
                           pools={"platform": 0, "sandbox": 0}, sql="STOPPED")
            r = run(tmp, st, "start", env_extra={"NODES": bad})
            # NODES="" is indistinguishable from unset and takes the refusal path;
            # both are refusals, which is all this asserts.
            check("NODES=%r refuses" % bad, r.code != 0, r.out)
            check("NODES=%r resizes nothing" % bad, r.first("clusters resize") == -1, r.calls)

        print("a stop interrupted halfway keeps the size it had already saved")
        # The failure that would quietly destroy the saved state: the re-run sees
        # platform already at zero and must not record THAT as its parked size --
        # and because the write REPLACES, it must also carry platform's old value
        # forward rather than sending sandbox's alone.
        st = new_state(tmp, "halfway")
        r = run(tmp, st, "stop", faults=["resize.sandbox"])
        check("the interrupted run reports failure", r.code != 0, r.out)
        check("platform was parked", r.pools()["platform"] == 0, r.pools())
        check("sandbox was not", r.pools()["sandbox"] == 1, r.pools())
        check("the database was never reached", r.sql() == "RUNNABLE", r.sql())
        r2 = run(tmp, st, "stop")
        check("the re-run finishes the job", r2.code == 0, r2.out)
        check("both pools are parked", r2.pools() == {"platform": 0, "sandbox": 0}, r2.pools())
        check("platform's ORIGINAL size survived the replacing write",
              r2.saved() == {"platform": "3", "sandbox": "1"}, r2.labels())
        check("and so did terraform's label",
              r2.labels().get("goog-terraform-provisioned") == "true", r2.labels())
        r3 = run(tmp, st, "start")
        check("and a start brings both back to where they began",
              r3.pools() == {"platform": 3, "sandbox": 1}, r3.pools())

        print("a start interrupted halfway leaves no label behind on the re-run")
        # The one that silently disables CD: a leftover power-saved-* label tells
        # deploy.yml the environment is parked, so every deploy afterwards is
        # skipped with a notice while staging is fully up. A re-run finds the
        # restored pool already running and skips it, so a removal list built
        # from "pools this run resized" would never name it again.
        st = new_state(tmp, "starthalf", pools={"platform": 0, "sandbox": 0},
                       sql="STOPPED", saved={"platform": 3, "sandbox": 1})
        r = run(tmp, st, "start", faults=["resize.sandbox"])
        check("the interrupted run reports failure", r.code != 0, r.out)
        check("platform is back", r.pools()["platform"] == 3, r.pools())
        check("both labels are still there", r.saved() == {"platform": "3", "sandbox": "1"},
              r.labels())
        r2 = run(tmp, st, "start")
        check("the re-run finishes the job", r2.code == 0, r2.out)
        check("both pools are running", r2.pools() == {"platform": 3, "sandbox": 1}, r2.pools())
        check("NO parked marker is left behind", r2.saved() == {}, r2.labels())
        check("and the cluster's other labels are intact", r2.labels() == dict(TF_LABEL),
              r2.labels())

        print("a stale marker on a fully running cluster is cleaned up by start")
        # However it got there -- a `terraform apply` while parked scales the
        # pools back up behind this script's back -- `start` has to be the way out.
        st = new_state(tmp, "stale", saved={"platform": 3})
        r = run(tmp, st, "start")
        check("exits 0", r.code == 0, r.out)
        check("resizes nothing", r.first("clusters resize") == -1, r.calls)
        check("clears the stale marker", r.saved() == {}, r.labels())
        check("keeping every other label", r.labels() == dict(TF_LABEL), r.labels())

        print("a failed size save parks nothing")
        # If the sizes cannot be recorded, zeroing the pools would strand them:
        # start would have nothing to restore from and would refuse.
        st = new_state(tmp, "savefails")
        r = run(tmp, st, "stop", faults=["update-labels"])
        check("refuses", r.code != 0, r.out)
        check("resizes nothing", r.first("clusters resize") == -1, r.calls)
        check("the pools are untouched", r.pools() == {"platform": 3, "sandbox": 1}, r.pools())
        check("the database is untouched", r.sql() == "RUNNABLE", r.sql())

        print("stopping what is already parked is a no-op, not a re-save")
        st = new_state(tmp, "already", pools={"platform": 0, "sandbox": 0}, sql="STOPPED",
                       saved={"platform": 3, "sandbox": 1})
        r = run(tmp, st, "stop")
        check("exits 0", r.code == 0, r.out)
        check("says so", "already parked" in r.out, r.out)
        check("writes no labels", r.first("--update-labels") == -1, r.calls)
        check("the saved sizes are the original ones",
              r.saved() == {"platform": "3", "sandbox": "1"}, r.labels())

        print("...but says so out loud when the sizes were never recorded")
        # Physically parked, no marker: CD will not skip and `start` will refuse.
        # Reporting a bare "already parked" would hide both.
        st = new_state(tmp, "parkednomark", pools={"platform": 0, "sandbox": 0}, sql="STOPPED")
        r = run(tmp, st, "stop")
        check("exits 0", r.code == 0, r.out)
        check("warns that no size is recorded", "warning" in r.out.lower(), r.out)
        check("names the way out", "NODES=" in r.out, r.out)

        print("a label that merely CONTAINS the prefix is not a parked marker")
        # `value()` renders the map as `key=value;key=value`, so a naive glob for
        # the prefix also matches a value carrying it. Getting this wrong is worse
        # in deploy.yml, where the same match decides whether to deploy at all: an
        # unrelated note would silently stop every deployment to a live cluster.
        st = new_state(tmp, "decoy", pools={"platform": 0, "sandbox": 0}, sql="STOPPED",
                       labels={"goog-terraform-provisioned": "true",
                               "deployment-note": "power-saved-example",
                               "not-power-saved-test": "1"})
        r = run(tmp, st, "stop")
        check("exits 0", r.code == 0, r.out)
        check("still warns — neither decoy is a marker", "warning" in r.out.lower(), r.out)
        # And the real thing, beside the decoys, must still register.
        st = new_state(tmp, "decoyplusreal", pools={"platform": 0, "sandbox": 0}, sql="STOPPED",
                       labels={"deployment-note": "power-saved-example"},
                       saved={"platform": 3, "sandbox": 1})
        r = run(tmp, st, "stop")
        check("exits 0", r.code == 0, r.out)
        check("a real marker beside them is recognised", "warning" not in r.out.lower(), r.out)

        print("starting what is already running changes nothing")
        st = new_state(tmp, "alreadyup")
        r = run(tmp, st, "start")
        check("exits 0", r.code == 0, r.out)
        check("resizes nothing", r.first("clusters resize") == -1, r.calls)
        check("the pools keep their sizes", r.pools() == {"platform": 3, "sandbox": 1}, r.pools())
        check("removes no label it never wrote", r.first("--remove-labels") == -1, r.calls)

        print("status reports without changing anything")
        st = new_state(tmp, "status", pools={"platform": 0, "sandbox": 0}, sql="STOPPED",
                       saved={"platform": 3, "sandbox": 1})
        r = run(tmp, st, "status")
        check("exits 0", r.code == 0, r.out)
        check("names both pools", "platform" in r.out and "sandbox" in r.out, r.out)
        check("reports the database state", "STOPPED" in r.out, r.out)
        check("shows what they were parked from", "parked from 3" in r.out, r.out)
        check("issues no write", not any(w in c for c in r.calls
                                         for w in ("resize", "update", "patch")), r.calls)

        print("a listing that dies halfway parks nothing")
        # The quiet one: a paginated list that prints `platform` and then fails.
        # A caller that cannot see the exit status parks only what it read, then
        # stops the database while the other pool is still serving.
        st = new_state(tmp, "partial")
        r = run(tmp, st, "stop", faults=["list-partial"])
        check("refuses", r.code != 0, r.out)
        check("resizes nothing", r.first("clusters resize") == -1, r.calls)
        check("never touches the database", r.first("sql instances patch") == -1, r.calls)
        check("the pools are untouched", r.pools() == {"platform": 3, "sandbox": 1}, r.pools())

        print("a denied listing is not reported as a missing cluster")
        st = new_state(tmp, "denied")
        r = run(tmp, st, "stop", faults=["list-denied"])
        check("refuses", r.code != 0, r.out)
        check("shows gcloud's own error", "permission" in r.out.lower(), r.out)
        check("does not blame NAME_PREFIX", "NAME_PREFIX" not in r.out, r.out)

        print("CRLF line endings do not break pool discovery")
        # Windows gcloud terminates lines CRLF. With the CR left attached, every
        # per-pool call after the listing 404s on `platform\r`.
        st = new_state(tmp, "crlf")
        r = run(tmp, st, "stop", faults=["crlf"])
        check("exits 0", r.code == 0, r.out)
        check("both pools are parked", r.pools() == {"platform": 0, "sandbox": 0}, r.pools())
        check("the sizes are recorded without a stray CR",
              r.saved() == {"platform": "3", "sandbox": "1"}, r.labels())
        r2 = run(tmp, st, "start", faults=["crlf"])
        check("and a start brings them back", r2.pools() == {"platform": 3, "sandbox": 1},
              r2.pools())
        check("clearing the labels works too", r2.saved() == {}, r2.labels())

        print("pools are discovered, not hard-coded to platform and sandbox")
        # main.tf declares two today. A third must be parked without this script
        # being edited -- and, just as important, must be RESTORED.
        st = new_state(tmp, "threepools", pools={"platform": 3, "sandbox": 1, "gpu": 5})
        r = run(tmp, st, "stop")
        check("all three are parked", r.pools() == {"platform": 0, "sandbox": 0, "gpu": 0}, r.pools())
        check("the third one's size is saved", r.saved().get("gpu") == "5", r.labels())
        r2 = run(tmp, st, "start")
        check("and all three come back", r2.pools() == {"platform": 3, "sandbox": 1, "gpu": 5},
              r2.pools())

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
