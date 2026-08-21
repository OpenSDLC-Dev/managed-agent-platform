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
  - `status` exits 2 while the vault is sealed, which an uninitialized one always is,
    because that 2 is the whole right-hand side of the readiness loop;
  - `operator init` prints the root token on stdout and flips the vault to initialized,
    which reproduces the redirect-versus-command ordering rather than describing it.

Faults are injected by dropping marker files in the state directory. The one that
matters cannot be produced by an exit code alone: an `operator init` that fails HAVING
initialized the vault ("Vault is already initialized" -- #438 is why it raced), which is
the state that made #439 unrecoverable rather than merely noisy.

Both scripts are covered, each from a scratch copy with one line rewritten -- INIT_FILE,
whose /openbao path belongs to a container volume -- and the chart's lifted out of the
ConfigMap block scalar it ships inside. The chart's ends in an unbounded renewal loop,
so it is run against a deadline and killed by PROCESS GROUP: the `sleep` it forks
outlives a plain kill by a day and holds the pipe open.

Because the chart hand-maintains a copy of its compose sibling, which is why #439 was in
both, one check pins the region they must share and lets the rest -- the remedy each
prints, the renewal loop -- move independently.

Then, because an assertion that cannot go red is a comment, each half of the #439 fix is
reverted in a scratch copy and the checks guarding it are REQUIRED to fail.

Both run under the HOST's /bin/sh, which is dash on CI: POSIX like the image's busybox
ash without being it. shellcheck in POSIX mode covers what that leaves -- a bashism would
otherwise pass here and fail in the container.

Run: make openbao-init-test
"""

import json
import os
import pathlib
import re
import shlex
import shutil
import signal
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
# Real `bao status` exits 2 while the vault is sealed, which an uninitialized one
# always is, and that 2 is the whole right-hand side of the scripts' readiness
# loop (`bao status || [ $? -eq 2 ]`) -- exiting 0 here would leave the branch
# that actually runs on a first boot unexercised.
if a[:1] == ["status"]:
    sealed = not st["initialized"]
    if a[1:] == ["-format=json"]:
        print(json.dumps({"initialized": st["initialized"], "sealed": sealed}, indent=2))
    sys.exit(2 if sealed else 0)

if a[:2] == ["operator", "init"]:
    if (S / "fault.init").exists():
        # Fails, but the vault IS initialized afterwards. That is what turns a
        # failed init from noise into the state #439 could not be talked out of.
        st["initialized"] = True
        save(st)
        die("Error initializing: Vault is already initialized")
    if (S / "fault.truncated").exists():
        # Some stdout, then death. Real `bao` writes root_token LAST, so the
        # likeliest cut is through the token itself: a file carrying the key and
        # no readable value, which `[ -s ]` and a test for the key both wave
        # through.
        sys.stdout.write('{"unseal_keys_b64": ["k1", "k2"], "root_token": "hvs.ab')
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

# The two halves of the #439 fix, each written as the pre-fix text it replaced.
# REDIRECT restores the pre-fix statement exactly. RECOVERY restores the pre-fix
# PREDICATE only -- the pre-fix tail also carried a `no root_token` exit that the
# fix deleted outright, so a reverted script walks past an unusable init.json and
# then authenticates with the token it failed to read, rather than exiting where
# the old one did. That still proves the guard is load-bearing, which is all the
# checks below claim. Both scripts carry these verbatim; main() guards that.
REDIRECT = (
    'bao operator init -format=json >"$INIT_FILE.tmp"\n\tmv "$INIT_FILE.tmp" "$INIT_FILE"',
    'bao operator init -format=json >"$INIT_FILE"',
)
RECOVERY = (
    'if [ -z "$BAO_TOKEN" ]; then',
    'if [ ! -f "$INIT_FILE" ]; then',
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


def code_lines(text):
    """Executable lines only. Comments are dropped so that a comment reflowed to a
    narrower column -- which the ConfigMap's indentation forces on the chart -- does
    not read as drift, and so that drifting logic cannot hide behind one."""
    return [ln for ln in text.splitlines() if ln.strip() and not ln.strip().startswith("#")]


def shared_region(text):
    """The stretch both scripts must keep identical: first boot through the guard."""
    lines = code_lines(text)
    if "if ! initialized; then" not in lines or "export BAO_TOKEN" not in lines:
        return None
    return lines[lines.index("if ! initialized; then"):lines.index("export BAO_TOKEN") + 1]


def rewrite(source, init_file, reverts=()):
    """`source` with INIT_FILE repointed and `reverts` applied, or None if any
    substitution did not match exactly what it expected."""
    # A function replacement, not a template: the path is data, and re.sub reads
    # a backslash or a `\g<...>` in a replacement as syntax. TMPDIR decides this
    # path, so nothing here gets to assume it is free of either.
    text, n = re.subn(
        r"(?m)^INIT_FILE=.*$",
        lambda _: "INIT_FILE=" + shlex.quote(str(init_file)),
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


class Stack:
    """One scratch stack: a fake `bao` on PATH, its state, and the init volume."""

    def __init__(self, work):
        self.work = work
        self.state = work / "state"
        self.volume = work / "volume"
        self.init_file = self.volume / "init.json"
        self.tmp_file = self.volume / "init.json.tmp"

    def fault(self, name):
        (self.state / ("fault." + name)).touch()

    def calls(self):
        log = self.state / "calls.log"
        return log.read_text().splitlines() if log.exists() else []

    def policy(self):
        hcl = self.state / "policy.hcl"
        return hcl.read_text() if hcl.exists() else ""


def stack(tmp, name, initialized=False, transit=False, tokens=None):
    """Build a stack without running anything -- the state IS the scenario, and a
    throwaway run to produce it would only have to be undone."""
    work = pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix=name + "."))
    s = Stack(work)
    for d in (s.state, s.volume, work / "bin"):
        d.mkdir()
    fake = work / "bin" / "bao"
    fake.write_text(FAKE_BAO)
    fake.chmod(0o755)
    (s.state / "state.json").write_text(
        json.dumps({
            "initialized": initialized,
            "transit": transit,
            "root_token": ROOT_TOKEN,
            "tokens": dict({ROOT_TOKEN: ["root"]}, **(tokens or {})),
        })
    )
    return s


class Result:
    def __init__(self, out, code):
        self.out = out
        self.code = code  # None means the deadline was reached


def run(s, source, reverts=(), deadline=120):
    """Stage one script in `s` and run it under /bin/sh, as both the compose
    entrypoint and the chart's sidecar command do."""
    # calls.log is per-run, so what a re-run asserts about is the run just made
    # and not everything the stack has ever been asked.
    (s.state / "calls.log").write_text("")
    script = s.work / "openbao-init.sh"
    script.write_text(rewrite(source, s.init_file, reverts))
    env = dict(os.environ)
    env["PATH"] = str(s.work / "bin") + os.pathsep + env["PATH"]
    env["FAKE_STATE"] = str(s.state)
    env["BAO_ADDR"] = "http://openbao:8200"
    env["BAO_PLATFORM_TOKEN"] = PLATFORM_TOKEN
    env["BAO_TRANSIT_KEY"] = TRANSIT_KEY
    # grep's own diagnostics are asserted on below, and they are translated.
    env["LC_ALL"] = "C"
    # The scripts export this themselves; a developer's shell must not supply it.
    env.pop("BAO_TOKEN", None)
    proc = subprocess.Popen(
        ["/bin/sh", str(script)],
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    try:
        out, err = proc.communicate(timeout=deadline)
        return Result(out + err, proc.returncode)
    except subprocess.TimeoutExpired:
        # The chart's script ends in an unbounded renewal loop, so a deadline is
        # how it gets driven at all. Kill the PROCESS GROUP: killing the shell
        # leaves the `sleep 86400` it forked running for a day, holding the pipe
        # open and a reference into a temp tree this run is about to delete.
        os.killpg(proc.pid, signal.SIGKILL)
        out, err = proc.communicate()
        return Result(out + err, None)


def main():
    if os.name != "posix":
        print("this runs the scripts under /bin/sh; run it on Linux (or WSL)", file=sys.stderr)
        return 1
    if not COMPOSE.exists():
        print("openbao-init.sh not found next to this test", file=sys.stderr)
        return 1
    if not CHART.exists():
        print("the chart's openbao-init ConfigMap is not where this test expects it", file=sys.stderr)
        return 1
    if shutil.which("shellcheck") is None:
        # Required, not optional: nothing else in CI lints these two files, and
        # the host shell here is dash while the container's is busybox ash.
        print("shellcheck is required by this test and is not on PATH", file=sys.stderr)
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

    compose, chart = sources[0][1], sources[1][1]
    tmp = tempfile.mkdtemp(prefix="openbao-init-test.")
    try:
        # #439 was in both scripts because the chart hand-maintains a copy of its
        # compose sibling. The next edit to one and not the other ships the same
        # class of bug, so the region they must share is pinned here; everything
        # outside it -- the remedy each prints, the chart's renewal loop -- moves
        # freely.
        print("the two scripts still share the logic they are meant to share")
        a, b = shared_region(compose), shared_region(chart)
        check("both carry the shared region", a is not None and b is not None)
        if a and b:
            diff = [p for p in zip(a, b) if p[0] != p[1]]
            check("it differs in exactly one line, the remedy each prints",
                  len(a) == len(b) and len(diff) == 1 and "initialize manually" in diff[0][0],
                  "\n".join("compose: %r\n  chart: %r" % p for p in diff) or
                  "compose has %d lines, chart %d" % (len(a), len(b)))

        print("both scripts are clean POSIX sh")
        for label, source in sources:
            path = pathlib.Path(tmp) / ("lint-%s.sh" % label)
            path.write_text(source)
            p = subprocess.run(["shellcheck", "-s", "sh", str(path)],
                               capture_output=True, text=True, timeout=120)
            check("%s passes shellcheck" % label, p.returncode == 0, p.stdout + p.stderr)

        print("a first boot initializes the vault, mounts transit and mints the platform token")
        s = stack(tmp, "firstboot")
        r = run(s, compose)
        check("the script succeeds", r.code == 0, r.out)
        check("it reports itself ready", "openbao ready" in r.out, r.out)
        check("init.json exists", s.init_file.exists(), r.out)
        check("init.json carries the root token",
              s.init_file.exists() and ROOT_TOKEN in s.init_file.read_text(), r.out)
        # init.json is the root token in a file, and 0600 from the moment it exists
        # is what the script's umask and chmod are both there for.
        check("init.json is 0600",
              s.init_file.exists() and (s.init_file.stat().st_mode & 0o777) == 0o600,
              s.init_file.exists() and oct(s.init_file.stat().st_mode & 0o777))
        check("the move left no .tmp behind", not s.tmp_file.exists())
        check("transit is mounted", "secrets enable transit" in s.calls(), s.calls())
        check("the platform token is minted",
              any(c.startswith("token create") and "-id=%s" % PLATFORM_TOKEN in c for c in s.calls()),
              s.calls())
        check("the policy is scoped to the one transit key",
              "transit/keys/%s" % TRANSIT_KEY in s.policy() and "*" not in s.policy(), s.policy())

        # #439. The redirect used to create and truncate init.json before the init
        # that fills it, so this check reads a 0-byte file on the pre-fix script --
        # and a 0-byte init.json is the artifact every later run then had to
        # distinguish from a lost volume.
        print("a failed init leaves nothing that could be mistaken for a good init.json")
        s = stack(tmp, "failedinit")
        s.fault("init")
        r = run(s, compose)
        check("the script fails", r.code not in (0, None), r.out)
        check("no init.json is left behind", not s.init_file.exists(),
              s.init_file.exists() and "%d bytes" % s.init_file.stat().st_size)

        # #439 as reported: the vault WAS already initialized (that is why the init
        # failed), so the next run can only tell the operator what is wrong. It used
        # to say "no root_token", which names the file rather than the problem.
        print("the run after a failed init names the real problem")
        r = run(s, compose)
        check("it fails", r.code == 1, r.out)
        check("it points at the volume", "volume lost?" in r.out, r.out)
        check("it stops before authenticating with an empty token",
              not any(c.startswith("secrets list") for c in s.calls()), s.calls())

        # The stacks already stuck this way before the fix: a 0-byte init.json on
        # the volume, which `[ ! -f ]` walked straight past.
        print("an empty init.json reads as unusable rather than as good")
        s = stack(tmp, "emptyfile", initialized=True)
        s.init_file.touch()
        r = run(s, compose)
        check("it fails", r.code == 1, r.out)
        check("it points at the volume", "volume lost?" in r.out, r.out)
        check("it stops before authenticating with an empty token",
              not any(c.startswith("secrets list") for c in s.calls()), s.calls())

        # And the damage `[ ! -s ]` would have missed. `bao` writes root_token
        # LAST, so a truncated init.json most often cuts through the token itself:
        # a file carrying the key and no readable value, which a test for the key
        # waves through as readily as `-s` does. Guarding on the extracted token is
        # what collapses every cut into the one state they really are.
        print("a part-written init.json reads as unusable, wherever it was cut")
        s = stack(tmp, "partialinit")
        s.fault("truncated")
        r = run(s, compose)
        check("the script fails", r.code not in (0, None), r.out)
        s.tmp_file.replace(s.init_file)  # what the pre-fix redirect would have left
        r = run(s, compose)
        check("a cut inside the token value is unusable", "volume lost?" in r.out, r.out)
        for label, payload in (
            ("a cut before the key", '{"unseal_keys_b64": ["k1"], "roo'),
            ("a cut just after the key", '{"unseal_keys_b64": ["k1"], "root_token"'),
            ("a key with no value", '{"root_token": ""}'),
        ):
            s.init_file.write_text(payload)
            r = run(s, compose)
            check("%s is unusable" % label, "volume lost?" in r.out, r.out)
            check("%s stops before authenticating" % label,
                  not any(c.startswith("secrets list") for c in s.calls()), s.calls())

        # An init.json that is INTACT but cannot be READ is the state that must not
        # be reported as a lost volume, because the recovery printed under that
        # diagnostic destroys the volume holding the only copy of the root token.
        # Root can read anything, so this is the one check needing a uid that can
        # be refused; CI's runner is not root, which is where it has to bite.
        print("an unreadable init.json says why, rather than reading as a lost volume")
        if os.geteuid() == 0:
            print("  --   not run: root cannot be refused by any mode (runs in CI, which is not root)")
        else:
            s = stack(tmp, "unreadable", initialized=True)
            s.init_file.write_text(json.dumps({"root_token": ROOT_TOKEN}))
            s.init_file.chmod(0o000)
            r = run(s, compose)
            check("it fails", r.code == 1, r.out)
            check("the read error reaches the operator", "Permission denied" in r.out, r.out)
            check("the recovery still follows it", "volume lost?" in r.out, r.out)

        # The one path on which the .tmp holds anything worth having: init wrote it
        # and the container died before the move. Sweeping it would destroy the only
        # copy of the root token, so the script keeps it -- and has to say so, since
        # the recovery it prints in the same breath is "destroy the volume".
        print("a leftover .tmp is pointed at rather than left to be thrown away")
        s = stack(tmp, "leftovertmp", initialized=True)
        s.tmp_file.write_text('{"root_token": "hvs.only-copy"}')
        r = run(s, compose)
        check("it fails", r.code == 1, r.out)
        check("the .tmp is named", "init.json.tmp is not empty" in r.out, r.out)
        check("the .tmp still exists", s.tmp_file.exists())

        print("a re-run renews the platform token rather than re-creating it")
        s = stack(tmp, "rerun", initialized=True, transit=True,
                  tokens={PLATFORM_TOKEN: ["map-transit"]})
        s.init_file.write_text(json.dumps({"root_token": ROOT_TOKEN}, indent=2))
        r = run(s, compose)
        check("the script succeeds", r.code == 0, r.out)
        check("it reports itself ready", "openbao ready" in r.out, r.out)
        check("the token is renewed", any(c.startswith("token renew") for c in s.calls()), s.calls())
        check("the token is not re-created",
              not any(c.startswith("token create") for c in s.calls()), s.calls())
        check("transit is not mounted twice", "secrets enable transit" not in s.calls(), s.calls())

        # Adopting a token that is not ours would hand the platform whatever that
        # token can do -- including root, if the ID were re-used for one.
        print("a token under the platform ID that is not ours fails closed")
        s = stack(tmp, "foreign", initialized=True, transit=True,
                  tokens={PLATFORM_TOKEN: ["default"]})
        s.init_file.write_text(json.dumps({"root_token": ROOT_TOKEN}, indent=2))
        r = run(s, compose)
        check("it fails", r.code == 1, r.out)
        check("it says which policy is missing", "map-transit policy" in r.out, r.out)

        # The chart ships the same logic as a sidecar. In-cluster the kubelet
        # re-runs a failed init on every restart, which makes #439 a
        # CrashLoopBackOff rather than one failed `compose up`.
        print("the chart's sidecar behaves the same way on every #439 state")
        s = stack(tmp, "chartfailed")
        s.fault("init")
        r = run(s, chart)
        check("a failed init leaves no init.json", not s.init_file.exists(), r.out)
        r = run(s, chart)
        check("the next run points at the volume", "volume lost?" in r.out, r.out)
        check("the next run stops before authenticating",
              not any(c.startswith("secrets list") for c in s.calls()), s.calls())
        check("the next run names the PVC the two subPaths share",
              "data PVC" in r.out and "restore the data PVC" not in r.out, r.out)
        s = stack(tmp, "chartempty", initialized=True)
        s.init_file.touch()
        r = run(s, chart)
        check("an empty init.json reads as unusable", "volume lost?" in r.out, r.out)
        s = stack(tmp, "chartpartial", initialized=True)
        s.init_file.write_text('{"unseal_keys_b64": ["k1"], "root_token": "hvs.ab')
        r = run(s, chart)
        check("a token cut in half reads as unusable", "volume lost?" in r.out, r.out)

        # The chart's own tail -- transit, the policy heredoc the ConfigMap
        # re-indents, and the token mint -- is otherwise never executed by
        # anything, which is the condition #439 was the bill for. It ends in an
        # unbounded renewal loop, so it is driven to the loop and killed there.
        print("the chart's sidecar configures the vault and then settles into renewing")
        s = stack(tmp, "chartready")
        r = run(s, chart, deadline=30)
        check("it reports itself ready", "openbao ready" in r.out, r.out)
        check("it is still running when the deadline arrives", r.code is None, r.out)
        check("transit is mounted", "secrets enable transit" in s.calls(), s.calls())
        check("the platform token is minted",
              any(c.startswith("token create") and "-id=%s" % PLATFORM_TOKEN in c for c in s.calls()),
              s.calls())
        check("it renews inside the loop", any(c.startswith("token renew") for c in s.calls()), s.calls())
        # The heredoc body is re-indented by the ConfigMap block scalar and its
        # terminator has to survive that; a policy arriving indented would be a
        # different document than the one the compose script writes.
        check("the policy survives the block scalar's indentation",
              s.policy().startswith('path "transit/keys/%s"' % TRANSIT_KEY), s.policy())

        # Every check above is worth exactly what it refuses. Each half of the fix
        # goes back into a scratch copy of each script and the checks guarding it
        # have to fail.
        print("reverting either half of the fix breaks the checks that guard it")
        for label, source in sources:
            s = stack(tmp, "revert-redirect")
            s.fault("init")
            r = run(s, source, reverts=[REDIRECT])
            check("%s: the pre-fix redirect leaves a 0-byte init.json" % label,
                  s.init_file.exists() and s.init_file.stat().st_size == 0, r.out)

            # The pre-fix predicate cannot see an empty init.json, so the run walks
            # past it and goes on to authenticate with the token it just failed to
            # read -- which is the shape of #439, whatever it dies of afterwards.
            s = stack(tmp, "revert-recovery", initialized=True)
            s.init_file.touch()
            r = run(s, source, reverts=[RECOVERY])
            check("%s: the pre-fix `[ -f ]` walks past an empty init.json" % label,
                  "volume lost?" not in r.out, r.out)
            check("%s: ...so the run authenticates with a token it never read" % label,
                  any(c.startswith("secrets list") for c in s.calls()), s.calls())

            s = stack(tmp, "revert-both")
            s.fault("init")
            run(s, source, reverts=[REDIRECT, RECOVERY])
            r = run(s, source, reverts=[REDIRECT, RECOVERY])
            check("%s: with both halves gone the 0-byte init.json is back in play" % label,
                  "volume lost?" not in r.out and any(c.startswith("secrets list") for c in s.calls()),
                  "%s\n%s" % (r.out, s.calls()))

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
