#!/usr/bin/env python3
"""Exercise both OpenBao init scripts against a fake `bao`, credential-free and Docker-free.

Why this exists: CI's compose job starts postgres, minio, the control plane and the
brain and never openbao, and `helm template` renders the chart's init script without
executing a line of it. So the two scripts that decide whether a self-hosted stack can
encrypt anything at all were the part of this repository nothing ever ran, and #439 is
what that cost. A redirect creates and truncates its target BEFORE the command filling
it runs, so a failed `bao operator init` left a 0-byte init.json behind; the branch that
recovers a lost volume tested for a MISSING file, walked past the empty one, and the
stack died on `no root_token` against a vault that was initialized and whose root token
had never been captured. shellcheck has no opinion on the order in which a redirect and
its command take effect. Reviewing harder is not a fix for that; the scripts have to be
RUN.

The fake models the parts of `bao` whose behaviour the scripts actually depend on:

  - every call but `status` is AUTHENTICATED against the fake's own token table, so a
    script that greps the wrong thing out of init.json is refused rather than humoured;
  - `operator init` prints the root token on stdout and flips the vault to initialized,
    which reproduces the redirect-versus-command ordering rather than describing it.

Faults are injected by dropping marker files in the state directory. The one that
matters cannot be produced by an exit code alone: an `operator init` that fails HAVING
initialized the vault ("Vault is already initialized" -- #438 is why it raced), which is
the state that made #439 unrecoverable rather than merely noisy.

Both scripts are covered, each from a scratch copy with one line rewritten -- INIT_FILE,
whose /openbao path belongs to a container volume -- and the chart's lifted out of the
ConfigMap block scalar it ships inside. The chart's script ends in an unbounded renewal
loop, so it is driven only through the branches that exit before reaching it, which is
exactly the #439 surface it shares with its compose sibling.

Then, because an assertion that cannot go red is a comment, each half of the #439 fix is
reverted in a scratch copy and the checks guarding it are REQUIRED to fail.

Run: make openbao-init-test
"""

import json
import os
import pathlib
import re
import shlex
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).parent.resolve()
COMPOSE = HERE / "openbao-init.sh"
CHART = HERE.parent / "helm" / "managed-agent-platform" / "templates" / "openbao-init-configmap.yaml"

# The compose defaults, so the fake can be diffed against the real stack.
PLATFORM_TOKEN = "map-bao-dev-token"
TRANSIT_KEY = "map-secrets"
ROOT_TOKEN = "hvs.fake-root-token"

FAKE_BAO = r'''#!/usr/bin/env python3
import json, os, pathlib, sys

S = pathlib.Path(os.environ["FAKE_STATE"])
a = sys.argv[1:]
tok = os.environ.get("BAO_TOKEN", "")


def die(msg, code=2):
    sys.stderr.write(msg + "\n")
    sys.exit(code)


def state():
    return json.loads((S / "state.json").read_text())


def save(st):
    (S / "state.json").write_text(json.dumps(st))


with (S / "calls.log").open("a") as f:
    f.write(" ".join(a) + "\n")

st = state()

# Unauthenticated, and the only call a vault answers before it is initialized.
if a[:1] == ["status"]:
    if a[1:] == ["-format=json"]:
        print(json.dumps({"initialized": st["initialized"], "sealed": False}, indent=2))
    sys.exit(0)

if a[:2] == ["operator", "init"]:
    if (S / "fault.init").exists():
        # Fails, but the vault IS initialized afterwards. That is what turns a
        # failed init from noise into the state #439 could not be talked out of.
        st["initialized"] = True
        save(st)
        die("Error initializing: Vault is already initialized")
    if (S / "fault.truncated").exists():
        # Some stdout, then death -- the half-written init.json that `[ -s ]`
        # alone would still read as good.
        sys.stdout.write('{"unseal_keys_b64": ["k1"], "roo')
        sys.stdout.flush()
        st["initialized"] = True
        save(st)
        die("Error initializing: connection reset")
    st["initialized"] = True
    save(st)
    print(json.dumps({"unseal_keys_b64": ["k1", "k2", "k3"], "root_token": st["root_token"]}, indent=2))
    sys.exit(0)

# `token lookup` is the one call a script makes as somebody other than root, and
# it is allowed to fail: that is how a first run learns the token does not exist.
if a[:2] == ["token", "lookup"]:
    if tok not in st["tokens"]:
        die("Error looking up token: permission denied")
    print(json.dumps({"data": {"id": tok, "policies": st["tokens"][tok]}}, indent=2))
    sys.exit(0)

if a[:2] == ["token", "renew"]:
    if tok not in st["tokens"]:
        die("Error renewing token: permission denied")
    st["renewed"] = st.get("renewed", 0) + 1
    save(st)
    sys.exit(0)

# Everything past here is root's, so a script that extracted the wrong root token
# is refused here instead of quietly appearing to work.
if tok != st["root_token"]:
    die("Error: permission denied")

if a[:2] == ["secrets", "list"]:
    mounts = {"cubbyhole/": {"type": "cubbyhole"}}
    if st["transit"]:
        mounts["transit/"] = {"type": "transit"}
    print(json.dumps(mounts, indent=2))
    sys.exit(0)

if a[:3] == ["secrets", "enable", "transit"]:
    if st["transit"]:
        die("Error enabling: path is already in use at transit/")
    st["transit"] = True
    save(st)
    sys.exit(0)

if a[:3] == ["policy", "write", "map-transit"]:
    (S / "policy.hcl").write_text(sys.stdin.read())
    sys.exit(0)

if a[:2] == ["token", "create"]:
    ids = [x.split("=", 1)[1] for x in a if x.startswith("-id=")]
    if not ids:
        die("Error creating token: this fake requires -id=")
    st["tokens"][ids[0]] = [x.split("=", 1)[1] for x in a if x.startswith("-policy=")]
    save(st)
    sys.exit(0)

die("fake bao: unhandled invocation: " + " ".join(a))
'''

# The two halves of the #439 fix, each written as the pre-fix text it replaced, so
# reverting one puts the old defect back. Both scripts carry these verbatim; a
# revert that stopped matching would silently test nothing, so main() guards it.
REDIRECT = (
    'bao operator init -format=json >"$INIT_FILE.tmp"\n\tmv "$INIT_FILE.tmp" "$INIT_FILE"',
    'bao operator init -format=json >"$INIT_FILE"',
)
RECOVERY = (
    "elif ! grep -q '\"root_token\"' \"$INIT_FILE\" 2>/dev/null; then",
    'elif [ ! -f "$INIT_FILE" ]; then',
)

failures = []


def check(label, cond, detail=""):
    if cond:
        print("  ok   %s" % label)
        return
    failures.append(label)
    print("  FAIL %s" % label)
    for line in str(detail).strip().splitlines():
        print("       %s" % line)


def chart_source():
    """The chart's init script, lifted out of the ConfigMap block scalar it ships in."""
    lines = CHART.read_text(encoding="utf-8").splitlines()
    start = None
    for i, line in enumerate(lines):
        if line.strip() == "openbao-init.sh: |":
            start = i + 1
            break
    if start is None:
        return None
    body = []
    for line in lines[start:]:
        if not line.strip():
            body.append("")
        elif line.startswith("    "):
            body.append(line[4:])
        else:
            break  # the block scalar ended -- {{- end -}} and anything after it
    return "\n".join(body) + "\n"


def rewrite(source, init_file, reverts=()):
    """`source` with INIT_FILE repointed and `reverts` applied, or None if any
    substitution did not match exactly what it expected."""
    text, n = re.subn(
        r"(?m)^INIT_FILE=.*$",
        "INIT_FILE=" + shlex.quote(str(init_file)),
        source,
        count=1,
    )
    if n != 1:
        return None
    for old, new in reverts:
        if old not in text:
            return None
        text = text.replace(old, new, 1)
    return text


class Run:
    def __init__(self, work, proc):
        self.work = work
        self.volume = work / "volume"
        self.init_file = self.volume / "init.json"
        self.tmp_file = self.volume / "init.json.tmp"
        self.out = "TIMED OUT" if proc is None else proc.stdout + proc.stderr
        self.code = None if proc is None else proc.returncode

    def calls(self):
        log = self.work / "state" / "calls.log"
        return log.read_text().splitlines() if log.exists() else []

    def policy(self):
        hcl = self.work / "state" / "policy.hcl"
        return hcl.read_text() if hcl.exists() else ""


def run(tmp, name, source, initialized=False, transit=False, tokens=None,
        faults=(), reverts=(), reuse=None):
    """Stage one script in a scratch stack and run it under /bin/sh, as both the
    compose entrypoint and the chart's sidecar command do."""
    if reuse is None:
        work = pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix=name + "."))
        for sub in ("bin", "state", "volume"):
            (work / sub).mkdir()
        fake = work / "bin" / "bao"
        fake.write_text(FAKE_BAO)
        fake.chmod(0o755)
        (work / "state" / "state.json").write_text(
            json.dumps({
                "initialized": initialized,
                "transit": transit,
                "root_token": ROOT_TOKEN,
                "tokens": dict({ROOT_TOKEN: ["root"]}, **(tokens or {})),
            })
        )
    else:
        work = reuse
    # Faults are per-run, not per-state: a reused stack asks "does the NEXT run
    # recover?", which it cannot answer with the previous run's fault still armed.
    for stale in (work / "state").glob("fault.*"):
        stale.unlink()
    for fault in faults:
        (work / "state" / ("fault." + fault)).touch()

    script = work / "openbao-init.sh"
    script.write_text(rewrite(source, work / "volume" / "init.json", reverts))
    env = dict(os.environ)
    env["PATH"] = str(work / "bin") + os.pathsep + env["PATH"]
    env["FAKE_STATE"] = str(work / "state")
    env["BAO_ADDR"] = "http://openbao:8200"
    env["BAO_PLATFORM_TOKEN"] = PLATFORM_TOKEN
    env["BAO_TRANSIT_KEY"] = TRANSIT_KEY
    # The scripts export this themselves; a developer's shell must not supply it.
    env.pop("BAO_TOKEN", None)
    try:
        proc = subprocess.run(
            ["/bin/sh", str(script)], env=env, text=True, capture_output=True, timeout=120
        )
    except subprocess.TimeoutExpired:
        proc = None  # the chart's renewal loop was reached; every check will say so
    return Run(work, proc)


def main():
    if not COMPOSE.exists():
        print("openbao-init.sh not found next to this test", file=sys.stderr)
        return 1
    if not CHART.exists():
        print("the chart's openbao-init ConfigMap is not where this test expects it", file=sys.stderr)
        return 1
    sources = [("compose", COMPOSE.read_text(encoding="utf-8")), ("chart", chart_source())]
    if sources[1][1] is None:
        print("no 'openbao-init.sh: |' block in %s" % CHART, file=sys.stderr)
        return 1
    if "{{" in sources[1][1]:
        # A templated body would be tested as literal text and prove nothing.
        print("the chart's init script now contains Go template syntax", file=sys.stderr)
        return 1
    # Guards, not checks: every scenario below runs a rewritten copy, so a rewrite
    # that silently matched nothing would turn the whole file green for free.
    probe = pathlib.Path("/probe/init.json")
    for label, source in sources:
        if rewrite(source, probe) is None:
            print("%s: no single INIT_FILE= line to repoint" % label, file=sys.stderr)
            return 1
        for old, _ in (REDIRECT, RECOVERY):
            if old not in source:
                print("%s: the #439 fix has changed shape; the reverts below would test "
                      "nothing:\n  %r" % (label, old), file=sys.stderr)
                return 1

    compose = sources[0][1]
    tmp = tempfile.mkdtemp(prefix="openbao-init-test.")
    try:
        print("a first boot initializes the vault, mounts transit and mints the platform token")
        r = run(tmp, "firstboot", compose)
        check("the script succeeds", r.code == 0, r.out)
        check("it reports itself ready", "openbao ready" in r.out, r.out)
        check("init.json exists", r.init_file.exists(), r.out)
        check("init.json carries the root token",
              r.init_file.exists() and ROOT_TOKEN in r.init_file.read_text(), r.out)
        # init.json is the root token in a file, and 0600 from the moment it exists
        # is what the script's umask and chmod are both there for.
        check("init.json is 0600",
              r.init_file.exists() and (r.init_file.stat().st_mode & 0o777) == 0o600,
              r.init_file.exists() and oct(r.init_file.stat().st_mode & 0o777))
        check("the move left no .tmp behind", not r.tmp_file.exists())
        check("transit is mounted", "secrets enable transit" in r.calls(), r.calls())
        check("the platform token is minted",
              any(c.startswith("token create") and "-id=%s" % PLATFORM_TOKEN in c for c in r.calls()),
              r.calls())
        check("the policy is scoped to the one transit key",
              "transit/keys/%s" % TRANSIT_KEY in r.policy() and "*" not in r.policy(), r.policy())

        # #439. The redirect used to create and truncate init.json before the init
        # that fills it, so this check reads a 0-byte file on the pre-fix script --
        # and a 0-byte init.json is the artifact every later run then had to
        # distinguish from a lost volume.
        print("a failed init leaves nothing that could be mistaken for a good init.json")
        r = run(tmp, "failedinit", compose, faults=("init",))
        check("the script fails", r.code not in (0, None), r.out)
        check("no init.json is left behind", not r.init_file.exists(),
              r.init_file.exists() and "%d bytes" % r.init_file.stat().st_size)

        # #439 as reported: the vault WAS already initialized (that is why the init
        # failed), so the next run can only tell the operator what is wrong. It used
        # to say "no root_token", which names the file rather than the problem.
        print("the run after a failed init names the real problem")
        r = run(tmp, "failedinit", compose, faults=("init",))
        r = run(tmp, "failedinit", compose, reuse=r.work)
        check("it fails", r.code == 1, r.out)
        check("it does not die on the root_token grep", "no root_token in" not in r.out, r.out)
        check("it points at the volume", "volume lost?" in r.out, r.out)

        # The stacks already stuck this way before the fix: a 0-byte init.json on
        # the volume, which `[ ! -f ]` walked straight past.
        print("an empty init.json reads as unusable rather than as good")
        r = run(tmp, "emptyfile", compose, initialized=True)
        r.init_file.touch()
        r = run(tmp, "emptyfile", compose, reuse=r.work)
        check("it fails", r.code == 1, r.out)
        check("it does not die on the root_token grep", "no root_token in" not in r.out, r.out)
        check("it points at the volume", "volume lost?" in r.out, r.out)

        # And the damage class `[ ! -s ]` would have missed: init wrote some of its
        # output before dying, so the file is neither missing nor empty nor usable.
        print("a half-written init.json reads as unusable too")
        r = run(tmp, "partialfile", compose, faults=("truncated",))
        check("the script fails", r.code not in (0, None), r.out)
        r.tmp_file.replace(r.init_file)  # what the pre-fix redirect would have left
        r = run(tmp, "partialfile", compose, reuse=r.work)
        check("it fails", r.code == 1, r.out)
        check("it does not die on the root_token grep", "no root_token in" not in r.out, r.out)
        check("it points at the volume", "volume lost?" in r.out, r.out)

        # The one path on which the .tmp holds anything worth having: init wrote it
        # and the container died before the move. Sweeping it would destroy the only
        # copy of the root token, so the script keeps it -- and has to say so, since
        # the recovery it prints in the same breath is "destroy the volume".
        print("a leftover .tmp is pointed at rather than left to be thrown away")
        r = run(tmp, "leftovertmp", compose, initialized=True)
        r.tmp_file.write_text('{"root_token": "hvs.only-copy"}')
        r = run(tmp, "leftovertmp", compose, reuse=r.work)
        check("it fails", r.code == 1, r.out)
        check("the .tmp is named", "init.json.tmp is not empty" in r.out, r.out)
        check("the .tmp still exists", r.tmp_file.exists())

        print("a re-run renews the platform token rather than re-creating it")
        r = run(tmp, "rerun", compose, initialized=True, transit=True,
                tokens={PLATFORM_TOKEN: ["map-transit"]})
        r.init_file.write_text(json.dumps({"root_token": ROOT_TOKEN}, indent=2))
        r = run(tmp, "rerun", compose, reuse=r.work)
        check("the script succeeds", r.code == 0, r.out)
        check("it reports itself ready", "openbao ready" in r.out, r.out)
        check("the token is renewed", any(c.startswith("token renew") for c in r.calls()), r.calls())
        check("the token is not re-created",
              not any(c.startswith("token create") for c in r.calls()), r.calls())
        check("transit is not mounted twice", "secrets enable transit" not in r.calls(), r.calls())

        # Adopting a token that is not ours would hand the platform whatever that
        # token can do -- including root, if the ID were re-used for one.
        print("a token under the platform ID that is not ours fails closed")
        r = run(tmp, "foreign", compose, initialized=True, transit=True,
                tokens={PLATFORM_TOKEN: ["default"]})
        r.init_file.write_text(json.dumps({"root_token": ROOT_TOKEN}, indent=2))
        r = run(tmp, "foreign", compose, reuse=r.work)
        check("it fails", r.code == 1, r.out)
        check("it says which policy is missing", "map-transit policy" in r.out, r.out)

        # The chart ships the same logic as a sidecar, and its own header says to
        # keep the two in sync -- so #439 was in both. In-cluster the kubelet re-runs
        # a failed init on every restart, which makes it a CrashLoopBackOff rather
        # than one failed `compose up`. Only the branches that exit before the
        # script's unbounded renewal loop are driven here; they are the shared ones.
        chart = sources[1][1]
        print("the chart's sidecar behaves the same way on all three #439 states")
        r = run(tmp, "chartfailed", chart, faults=("init",))
        check("a failed init leaves no init.json", not r.init_file.exists(), r.out)
        r = run(tmp, "chartfailed", chart, reuse=r.work)
        check("the next run does not die on the root_token grep",
              "no root_token in" not in r.out, r.out)
        check("the next run points at the volume", "volume lost?" in r.out, r.out)
        r = run(tmp, "chartempty", chart, initialized=True)
        r.init_file.touch()
        r = run(tmp, "chartempty", chart, reuse=r.work)
        check("an empty init.json reads as unusable", "volume lost?" in r.out, r.out)
        r = run(tmp, "chartpartial", chart, initialized=True)
        r.init_file.write_text('{"unseal_keys_b64": ["k1"], "roo')
        r = run(tmp, "chartpartial", chart, reuse=r.work)
        check("a half-written init.json reads as unusable", "volume lost?" in r.out, r.out)

        # Every check above is worth exactly what it refuses. Each half of the fix
        # goes back into a scratch copy of each script and the checks guarding it
        # have to fail -- and fail on the #439 symptom, not on something incidental.
        print("reverting either half of the fix breaks the checks that guard it")
        for label, source in sources:
            r = run(tmp, "revert-redirect", source, faults=("init",), reverts=[REDIRECT])
            check("%s: the pre-fix redirect leaves a 0-byte init.json" % label,
                  r.init_file.exists() and r.init_file.stat().st_size == 0, r.out)

            r = run(tmp, "revert-recovery", source, initialized=True, reverts=[RECOVERY])
            r.init_file.touch()
            r = run(tmp, "revert-recovery", source, reuse=r.work, reverts=[RECOVERY])
            check("%s: the pre-fix `[ -f ]` walks past an empty init.json" % label,
                  "no root_token in" in r.out, r.out)

            r = run(tmp, "revert-both", source, faults=("init",), reverts=[REDIRECT, RECOVERY])
            r = run(tmp, "revert-both", source, reuse=r.work, reverts=[REDIRECT, RECOVERY])
            check("%s: both halves reverted reproduce the dead end #439 reported" % label,
                  "no root_token in" in r.out, r.out)

        print()
        if failures:
            print("FAILED: %d check(s)" % len(failures))
            for f in failures:
                print("  - %s" % f)
            return 1
        print("ok: both init scripts behave correctly across every simulated state, and "
              "the #439 checks go red without the fix")
        return 0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
