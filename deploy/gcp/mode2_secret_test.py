#!/usr/bin/env python3
"""Exercise mode2-secret.sh against a fake gcloud, kubectl and jq — credential-free.

Why this exists: until this test, NOTHING automated read the mode-2 assembly at
all. It lived twice, as a step in `.github/workflows/deploy.yml` and as a worked
block in `deploy/gcp/README.md`, and the two had already drifted — 193a3aae
rewrote the workflow's model-providers error routing and left the document's copy
alone. One copy is now the script both callers run, and this is what holds it.

Shellcheck cannot know that `--from-literal` would put a credential on argv, that
a blank secret version applies cleanly and authenticates as nobody, or that `jq -e`
without `-s` takes its status from the LAST of several top-level documents. Every
one of those is a check the script makes and a check something has to prove is
still there, so the script has to be RUN.

The fakes model only what the script depends on:

  - `gcloud secrets versions access --out-file` writes bytes to a file, so a
    version's exact content — blank, newline-terminated, not an array — reaches
    the checks verbatim. Its NOT_FOUND text is the real one, because the script
    routes on it.
  - `kubectl create secret -o json` records its whole argv, so the six keys,
    their order and the absence of `--from-literal` are all assertable. It also
    records each file's bytes, which is how "the key is the file NAME" is checked.
  - `kubectl apply -f -` reads the manifest off the pipe and reports one line —
    "created", "configured" or "unchanged" — which is the only thing the caller
    routes on.
  - `jq` is the real one where it exists, because the shape check is the jq
    expression; faking jq would test the fake.

Faults are marker files in the state directory, so a scenario can fail one call
rather than the whole binary.

Run: make gcp-mode2-secret-test
"""

import os
import json
import base64
import pathlib
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).parent.resolve()
SCRIPT = HERE / "mode2-secret.sh"
BASH = shutil.which("bash")

# The six keys the Secret must carry — a set, not a sequence: a Secret is a
# map, so what is asserted below is that these six and no others are applied.
# The chart reads all six off one object; a missing `blob-backend` does not
# fail, it reads as the default "s3" and then looks for an endpoint that is not
# there.
KEYS = [
    "database-url",
    "model-providers.json",
    "blob-backend",
    "blob-bucket",
    "secrets-backend",
    "gcpkms-key-name",
]

# What a healthy project holds. Each is the exact bytes `--out-file` would write.
GOOD = {
    "database-url": b"postgres://map:pw@10.0.0.3:5432/map",
    "model-providers": b'[{"model":"*","protocol":"anthropic",'
                       b'"base_url":"https://api.anthropic.com","api_key":"sk-ant-0123456789"}]',
}

# The gateway writes its own route and needs only the database credential.
KEYS_GW = [k for k in KEYS if k != "model-providers.json"]
GOOD_GW = {"database-url": GOOD["database-url"]}

FAKE_GCLOUD = r'''#!/usr/bin/env python3
import os, sys, pathlib
S = pathlib.Path(os.environ["FAKE_STATE"])
a = sys.argv[1:]


def log(line):
    with (S / "calls.log").open("a") as f:
        f.write(line + "\n")


log("gcloud " + " ".join(a))

if a[:3] == ["secrets", "versions", "access"]:
    name = [x.split("=", 1)[1] for x in a if x.startswith("--secret=")][0]
    out = [x.split("=", 1)[1] for x in a if x.startswith("--out-file=")]
    body = S / ("ver." + name)
    if not body.exists():
        # The real wording, because the script routes on NOT_FOUND and on
        # nothing else. gcloud says this for an absent secret AND for one the
        # caller cannot see, which is why the script's message commits to
        # neither.
        sys.stderr.write(
            "ERROR: (gcloud.secrets.versions.access) NOT_FOUND: Secret "
            "[projects/p/secrets/%s/versions/latest] not found or has no versions.\n" % name)
        sys.exit(1)
    if (S / ("fault.denied." + name)).exists():
        sys.stderr.write("ERROR: (gcloud.secrets.versions.access) PERMISSION_DENIED: "
                         "Permission 'secretmanager.versions.access' denied.\n")
        sys.exit(1)
    # Per secret, not global. Only the `model-providers` read has its stderr
    # captured; the other two pass theirs straight through, so a warning on all
    # three would satisfy a check about the capture without exercising it.
    if (S / ("fault.warn." + name)).exists():
        sys.stderr.write("WARNING: This command is using service account impersonation.\n")
    data = body.read_bytes()
    if out:
        # --out-file writes the bytes and prints NOTHING. A script that dropped
        # the flag would print the credential instead, which is the leak the
        # masking exists for.
        pathlib.Path(out[0]).write_bytes(data)
        sys.exit(0)
    sys.stdout.buffer.write(data)
    sys.exit(0)

sys.stderr.write("fake gcloud: unhandled invocation: " + " ".join(a) + "\n")
sys.exit(2)
'''

FAKE_KUBECTL = r'''#!/usr/bin/env python3
import os, sys, pathlib, json, base64
S = pathlib.Path(os.environ["FAKE_STATE"])
a = sys.argv[1:]


def log(line):
    with (S / "calls.log").open("a") as f:
        f.write(line + "\n")


log("kubectl " + " ".join(a))

if a[:2] == ["create", "namespace"]:
    if "--dry-run=client" not in a:
        sys.stderr.write("fake kubectl: namespace created for real\n")
        sys.exit(9)
    print("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + a[2])
    sys.exit(0)

if a[:3] == ["create", "secret", "generic"]:
    if "--dry-run=client" not in a:
        # A `create` without the dry-run/apply shape fails the SECOND time the
        # job runs. Refusing it here is what stops that from being reintroduced.
        sys.stderr.write("fake kubectl: a real create would fail on re-run\n")
        sys.exit(9)
    data = {}
    for x in a:
        if x.startswith("--from-file="):
            p = pathlib.Path(x.split("=", 1)[1])
            data[p.name] = base64.b64encode(p.read_bytes()).decode()
    manifest = {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": a[3]}, "data": data}
    if "--type=Opaque" in a:
        manifest["type"] = "Opaque"
    print(json.dumps(manifest))
    sys.exit(0)

if a[:2] == ["get", "secret"]:
    if (S / "fault.read-secret").exists():
        sys.stderr.write("cluster unavailable\n")
        sys.exit(1)
    if (S / "existing-secret.json").exists():
        print((S / "existing-secret.json").read_text())
    sys.exit(0)

if a[:2] in (["apply", "-f"], ["create", "-f"]):
    manifest = sys.stdin.read()
    (S / "applied.manifest").write_text(manifest)
    if manifest.startswith("{"):
        doc = json.loads(manifest)
        if a[0] == "create" and (S / "fault.concurrent-create").exists():
            sys.stderr.write("AlreadyExists: " + manifest)
            sys.exit(1)
        if (S / "fault.apply-secret").exists():
            sys.stderr.write("failed patch: " + manifest)
            sys.exit(1)
        for key, value in doc["data"].items():
            (S / ("key." + key)).write_bytes(base64.b64decode(value))
        # The API server defaults an omitted type; client dry-run does not.
        doc.setdefault("type", "Opaque")
        (S / "secret.manifest.json").write_text(json.dumps(doc))
        verb = (S / "apply.verb").read_text().strip() if (S / "apply.verb").exists() else "created"
        print("secret/%s %s" % (doc["metadata"]["name"], verb))
        sys.exit(0)
    kind = "secret/unknown"
    for line in manifest.splitlines():
        if line.startswith("kind: "):
            kind = line.split(": ", 1)[1].lower()
        if line.strip().startswith("name: "):
            kind = kind + "/" + line.strip().split(": ", 1)[1]
    verb = (S / "apply.verb").read_text().strip() if (S / "apply.verb").exists() else "created"
    print("%s %s" % (kind, verb))
    sys.exit(0)

sys.stderr.write("fake kubectl: unhandled invocation: " + " ".join(a) + "\n")
sys.exit(2)
'''

failures = []


class Run:
    def __init__(self, state, proc):
        self.state, self.proc = state, proc
        self.out = proc.stdout + proc.stderr
        self.code = proc.returncode

    def calls(self):
        p = self.state / "calls.log"
        return p.read_text() if p.exists() else ""

    def key(self, name):
        p = self.state / ("key." + name)
        return p.read_bytes() if p.exists() else None


def run(tmp, name, versions=None, faults=(), env_extra=None, github=False, verb=None, existing=None):
    state = pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix=name + "."))
    for secret, body in (GOOD if versions is None else versions).items():
        (state / ("ver." + secret)).write_bytes(body)
    for f in faults:
        (state / ("fault." + f)).touch()
    if existing is not None:
        (state / "existing-secret.json").write_text(json.dumps(existing))
    if verb:
        (state / "apply.verb").write_text(verb)
    env = dict(os.environ)
    env["PATH"] = str(pathlib.Path(tmp) / "bin") + os.pathsep + env["PATH"]
    # The script's own `mktemp -d` reads TMPDIR, so pointing it here keeps every
    # directory it makes inside this run's scratch. Without that, the cleanup
    # check below would scan the SHARED system temp dir: one interrupted run — or
    # a mutation probe, or a second worktree running this suite at the same
    # moment — leaves a `mode2-secret.*` behind there and every later run fails
    # on someone else's litter until a human deletes it.
    env["TMPDIR"] = str(tmp)
    env["FAKE_STATE"] = str(state)
    env["PROJECT"] = "p"
    env["BLOB_BUCKET"] = "map-blobs"
    env["KMS_KEY_NAME"] = "projects/p/locations/us-central1/keyRings/map/cryptoKeys/vault"
    # Set, not inherited: an operator who exported MODELGATEWAY=true for a
    # hand run would otherwise turn every gateway-off scenario into a gateway one.
    env["MODELGATEWAY"] = "false"
    env.pop("K8S_NAMESPACE", None)
    env.pop("K8S_SECRET", None)
    env.pop("SECRET_DIR", None)
    env.pop("GITHUB_ENV", None)
    env.pop("OIDC_CLIENT_SECRET_NAME", None)
    if github:
        env["GITHUB_ACTIONS"] = "true"
    else:
        env.pop("GITHUB_ACTIONS", None)
    for k, v in (env_extra or {}).items():
        if v is None:
            env.pop(k, None)
        else:
            env[k] = v
    # Absolute, resolved from the real environment: `subprocess` looks the
    # program up on the CHILD's PATH, and one scenario hands the child a PATH
    # holding nothing but the fakes.
    return Run(state, subprocess.run([BASH, str(SCRIPT)], env=env, text=True,
                                     capture_output=True, timeout=120))


def before(text, first, second):
    """True when `first` occurs before `second` in `text`.

    `str.index` would be shorter, but it raises when a marker is missing, and a
    traceback is not a FAIL line: the run would die mid-suite with nothing in the
    summary. Absence is exactly the regression these orderings exist to catch, so
    it has to be answered False rather than thrown.
    """
    i, j = text.find(first), text.find(second)
    return i >= 0 and j >= 0 and i < j


def check(label, cond, detail=""):
    if cond:
        print("  ok   %s" % label)
    else:
        print("  FAIL %s%s" % (label, ("\n       " + detail.replace("\n", "\n       ")) if detail else ""))
        failures.append(label)


def main():
    if not SCRIPT.exists():
        print("mode2-secret.sh not found next to this test", file=sys.stderr)
        return 1
    if shutil.which("jq") is None:
        print("jq is not on PATH; mode2-secret.sh requires it", file=sys.stderr)
        return 1
    tmp = tempfile.mkdtemp(prefix="mode2-secret-test.")
    try:
        binp = pathlib.Path(tmp) / "bin"
        binp.mkdir()
        for name, body in (("gcloud", FAKE_GCLOUD), ("kubectl", FAKE_KUBECTL)):
            (binp / name).write_text(body)
            (binp / name).chmod(0o755)

        print("a healthy project assembles the Secret")
        r = run(tmp, "clean")
        check("exits 0", r.code == 0, r.out)
        check("writes those six keys and no others",
              sorted(p.name[len("key."):] for p in r.state.glob("key.*")) == sorted(KEYS),
              repr(sorted(p.name for p in r.state.glob("key.*"))))
        check("the namespace is applied before the Secret",
              before(r.calls(), "create namespace", "create secret"), r.calls())
        # Applied, not merely built. `create namespace` alone renders a manifest
        # to stdout and touches nothing; without this, dropping the
        # `| kubectl apply -f -` from the namespace line still passes, and the
        # Secret then lands in a namespace a fresh cluster does not have.
        check("namespace is applied and an absent Secret is created from its manifest",
              r.calls().count("kubectl apply -f -") == 1
              and r.calls().count("kubectl create -f -") == 1, r.calls())
        check("reports what apply did on stdout", "created" in r.proc.stdout, r.proc.stdout)

        print("native OIDC reads its configured client secret without exposing it")
        oidc_value = b"oauth-client-secret-fixture"
        for gateway in ["false", "true"]:
            versions = {**(GOOD_GW if gateway == "true" else GOOD), "fixture-oidc": oidc_value}
            r = run(tmp, "oidc-" + gateway, versions=versions,
                    env_extra={"MODELGATEWAY": gateway, "OIDC_CLIENT_SECRET_NAME": "fixture-oidc"})
            check("OIDC assembly succeeds with gateway=" + gateway, r.code == 0, r.out)
            check("configured OIDC payload reaches only its named Secret key", r.key("oidc-client-secret") == oidc_value)
            check("OIDC credential is absent from commands and ordinary output", oidc_value.decode() not in r.calls() + r.out)
        for name, value in [("missing", None), ("empty", b""), ("blank", b"  "), ("newline", oidc_value + b"\n")]:
            versions = dict(GOOD)
            if value is not None:
                versions["fixture-oidc"] = value
            r = run(tmp, "oidc-bad-" + name, versions=versions,
                    env_extra={"OIDC_CLIENT_SECRET_NAME": "fixture-oidc"})
            if name == "newline":
                check("OIDC repair command names the Secret Manager source",
                      "versions add fixture-oidc " in r.out and
                      "Secret Manager secret 'fixture-oidc' contains a newline" in r.out, r.out)
            check("invalid OIDC secret is refused before any apply: " + name,
                  r.code != 0 and "kubectl" not in r.calls(), r.out)

        r = run(tmp, "clean-again")

        print("credentials never reach an argv")
        check("no --from-literal anywhere", "--from-literal" not in r.calls(), r.calls())
        for secret, body in GOOD.items():
            check("the %s value is not in any command line" % secret,
                  body.decode() not in r.calls())

        print("each key carries the bytes of its version, verbatim")
        check("database-url", r.key("database-url") == GOOD["database-url"])
        check("model-providers.json", r.key("model-providers.json") == GOOD["model-providers"])

        print("the four literals are written without a trailing newline")
        check("blob-backend is exactly gcs", r.key("blob-backend") == b"gcs", repr(r.key("blob-backend")))
        check("secrets-backend is exactly gcpkms", r.key("secrets-backend") == b"gcpkms",
              repr(r.key("secrets-backend")))
        check("blob-bucket carries BLOB_BUCKET", r.key("blob-bucket") == b"map-blobs",
              repr(r.key("blob-bucket")))
        check("gcpkms-key-name carries KMS_KEY_NAME",
              r.key("gcpkms-key-name") == b"projects/p/locations/us-central1/keyRings/map/cryptoKeys/vault",
              repr(r.key("gcpkms-key-name")))

        # The defaults are what every other scenario takes, so nothing else would
        # notice them being hardcoded — and deploy.yml passes both explicitly, to
        # name the Secret `existingSecret` binds to.
        print("the namespace and the Secret's name are inputs, not literals")
        n = run(tmp, "names", env_extra={"K8S_NAMESPACE": "other-ns",
                                         "K8S_SECRET": "other-secret"})
        check("exits 0", n.code == 0, n.out)
        check("the namespace asked for is the one created",
              "create namespace other-ns" in n.calls(), n.calls())
        check("the Secret asked for is the one created",
              "create secret generic other-secret" in n.calls(), n.calls())
        check("...in that namespace", "--namespace other-ns" in n.calls(), n.calls())

        print("the Secret is built with dry-run before its manifest is submitted")
        check("the Secret is created --dry-run=client", "--dry-run=client" in r.calls())
        check("apply read a manifest off the pipe",
              (r.state / "applied.manifest").exists())

        # Both secrets, not just the first: the guard lives in one `fetch`, but a
        # regression that special-cased the other one would pass a suite that
        # checks the database URL as well as the OIDC secret.
        print("a version that would apply cleanly and fail much later is refused")
        for secret in ("database-url",):
            for label, body in (("empty", b""), ("whitespace-only", b"   \n"),
                                ("a lone space", b" ")):
                bad = dict(GOOD)
                bad[secret] = body
                b = run(tmp, "blank", versions=bad)
                check("a %s %s is refused" % (label, secret), b.code != 0, b.out)
                check("...and no Secret is applied",
                      not (b.state / "applied.manifest").exists())

        print("a trailing newline is refused on the two values read verbatim")
        for secret in ("database-url",):
            bad = dict(GOOD)
            bad[secret] = GOOD[secret] + b"\n"
            b = run(tmp, "newline", versions=bad)
            check("a newline in %s is refused" % secret, b.code != 0, b.out)
            check("...and no Secret is applied", not (b.state / "applied.manifest").exists())

        print("...and NOT on model-providers, which every editor ends with one")
        ok = dict(GOOD)
        ok["model-providers"] = GOOD["model-providers"] + b"\n"
        g = run(tmp, "jsonnewline", versions=ok)
        check("a newline in model-providers.json is accepted", g.code == 0, g.out)

        print("model-providers must be a non-empty JSON array of routes")
        for label, body in (
            ("an object", b'{"model":"*"}'),
            ("an empty array", b"[]"),
            ("an array of non-routes", b'[{"protocol":"anthropic"}]'),
            ("not JSON at all", b"sk-ant-oops"),
            # -s is what makes it ONE document. Without it, `jq -e` takes its
            # status from the LAST top-level document, so a bare object followed
            # by a valid array passes while the stored bytes are not an array.
            ("two concatenated documents", b'{"a":1}\n[{"model":"*"}]'),
        ):
            bad = dict(GOOD)
            bad["model-providers"] = body
            b = run(tmp, "shape", versions=bad)
            check("%s is refused" % label, b.code != 0, b.out)
            check("...and no Secret is applied", not (b.state / "applied.manifest").exists())

        print("an absent model-providers is named as the one a human must supply")
        missing = {k: v for k, v in GOOD.items() if k != "model-providers"}
        b = run(tmp, "notfound", versions=missing)
        check("exits non-zero", b.code != 0, b.out)
        check("names `gcloud secrets create`", "secrets create model-providers" in b.out, b.out)
        check("names `versions add`", "versions add model-providers" in b.out, b.out)

        print("a failure that is NOT absence does not send the operator to create it")
        bad = dict(GOOD)
        b = run(tmp, "denied", versions=bad, faults=("denied.model-providers",))
        check("exits non-zero", b.code != 0, b.out)
        check("replays what gcloud said", "PERMISSION_DENIED" in b.out, b.out)
        check("does NOT tell them to create the secret",
              "secrets create model-providers" not in b.out, b.out)

        # `model-providers` is the one read whose stderr is captured, because the
        # NOT_FOUND routing has to grep it. That capture is what could swallow a
        # warning on the way past, so the replay is checked by its text: a
        # swallowed warning leaves the exit status untouched, which is why an
        # earlier version of this case passed while replaying nothing.
        print("a warning on the success path is not swallowed")
        w = run(tmp, "warn", faults=("warn.model-providers",))
        check("exits 0", w.code == 0, w.out)
        check("the warning gcloud printed reaches the operator",
              "service account impersonation" in w.out, w.out)

        print("GitHub annotations appear only under GITHUB_ACTIONS")
        gh = run(tmp, "gh", github=True)
        check("exits 0", gh.code == 0, gh.out)
        check("masks each credential before anything can print it",
              gh.out.count("::add-mask::") == 2, gh.out)
        check("masks the api_key inside model-providers, not the JSON",
              "::add-mask::sk-ant-0123456789" in gh.out, gh.out)
        check("plain run emits no annotations",
              "::add-mask::" not in r.out and "::error::" not in r.out, r.out)
        # ...and on a run that FAILS, which is the only path `fail()` takes. A
        # success never reaches it, so the clean run alone would stay green with
        # the GITHUB_ACTIONS guard deleted from `fail`.
        plainbad = run(tmp, "plainerr",
                       versions={k: v for k, v in GOOD.items() if k != "model-providers"})
        check("a plain failure is annotated by neither",
              plainbad.code != 0 and "::error::" not in plainbad.out
              and "::add-mask::" not in plainbad.out, plainbad.out)
        # The other half of the same split. There is no workflow to re-run from a
        # terminal, and the guidance says so only inside Actions.
        check("...and a local run is not told to re-run a workflow",
              "Run workflow" not in plainbad.out, plainbad.out)
        ghbad = run(tmp, "gherr", versions={k: v for k, v in GOOD.items() if k != "model-providers"},
                    github=True)
        check("a failure is annotated under GITHUB_ACTIONS", "::error::" in ghbad.out, ghbad.out)
        check("...and a CD run IS told the deploy does not resume by itself",
              "Run workflow" in ghbad.out, ghbad.out)
        # Ordering, not just presence. Masking a value after something has
        # already printed it is no protection at all, so take the path that
        # fails AFTER the first value is fetched and require the redaction to be
        # registered by then.
        late = dict(GOOD)
        late["database-url"] = GOOD["database-url"] + b"\n"
        gl = run(tmp, "ghorder", versions=late, github=True)
        # Within ONE stream. `Run.out` is stdout followed by stderr, so comparing
        # offsets across it would make any stdout marker precede any stderr one
        # by construction — the check would go on passing, provingly nothing, the
        # day someone moved the annotation to stderr. Both markers are asserted
        # to be on stdout, which is where a workflow command has to be for GitHub
        # to act on it at all.
        check("a credential is masked before the run that fails can print anything",
              gl.code != 0
              and "::add-mask::" + GOOD["database-url"].decode() in gl.proc.stdout
              and before(gl.proc.stdout, "::add-mask::", "contains a newline"),
              gl.proc.stdout)

        print("the caller can tell a rotation from a no-op")
        for verb, want in (("created", "true"), ("configured", "true"),
                           ("unchanged", "false")):
            genv = pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix="genv." + verb)) / "env"
            genv.write_text("")
            v = run(tmp, "verb." + verb, verb=verb, env_extra={"GITHUB_ENV": str(genv)})
            check("apply said %s and it reaches stdout" % verb, verb in v.proc.stdout,
                  v.proc.stdout)
            check("...and GITHUB_ENV carries SECRET_CHANGED=%s" % want,
                  ("SECRET_CHANGED=" + want) in genv.read_text(), genv.read_text())
        # The masks go to stdout because that is where GitHub reads workflow
        # commands, so the verdict cannot travel by the caller capturing stdout
        # — that would swallow every redaction. This is what makes GITHUB_ENV
        # the channel rather than a convenience.
        nov = run(tmp, "noghenv", verb="created", env_extra={"GITHUB_ENV": None})
        check("no GITHUB_ENV means no SECRET_CHANGED and no failure", nov.code == 0, nov.out)

        # One PATH per tool, holding the others and nothing else, because a
        # scenario that only ever hides `jq` leaves the other entries of the
        # preflight loop unverifiable: cutting them to `for tool in jq` would
        # keep the suite green. Everything a refusal needs is a bash builtin,
        # which is why the script can still speak with no tool in reach.
        print("a missing tool is named rather than misdiagnosed")
        for missing in ("gcloud", "kubectl", "jq"):
            only = pathlib.Path(tmp) / ("bin.no-" + missing)
            only.mkdir(exist_ok=True)
            for name, src in (("gcloud", binp / "gcloud"), ("kubectl", binp / "kubectl"),
                              ("jq", pathlib.Path(shutil.which("jq")))):
                if name != missing and not (only / name).exists():
                    (only / name).symlink_to(src)
            t = run(tmp, "no" + missing, env_extra={"PATH": str(only)})
            check("a missing %s is named" % missing,
                  t.code != 0 and "%s is required and not on PATH" % missing in t.out, t.out)
            check("...and no credential was fetched first",
                  not (t.state / "key.database-url").exists())
            check("...and nothing healthy is blamed for it",
                  "not a non-empty JSON array" not in t.out
                  and "EMPTY or whitespace-only" not in t.out, t.out)

        print("required inputs are refused rather than applied empty")
        for var in ("PROJECT", "BLOB_BUCKET", "KMS_KEY_NAME"):
            m = run(tmp, "missing." + var, env_extra={var: None})
            check("%s unset is refused" % var, m.code != 0, m.out)
            check("...and no Secret is applied", not (m.state / "applied.manifest").exists())
            m = run(tmp, "empty." + var, env_extra={var: ""})
            check("%s empty is refused" % var, m.code != 0, m.out)

        print("SECRET_DIR is honoured, and the script cleans up only what it made")
        keep = pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix="keep."))
        # Deliberately group- and world-readable first. mkdtemp already makes a
        # 0700 directory, so a test that used its default would pass whether or
        # not the script chmods anything — it would share the premise it is
        # supposed to be checking.
        keep.chmod(0o755)
        k = run(tmp, "keepdir", env_extra={"SECRET_DIR": str(keep)})
        check("exits 0", k.code == 0, k.out)
        check("the caller's directory survives with the values in it",
              (keep / "database-url").exists(), sorted(p.name for p in keep.glob("*")))
        # The values are written by `gcloud --out-file`, which does not chmod
        # what it creates, so the directory is the only thing standing between
        # three credentials and every other process on the machine.
        check("...and is left mode 700", (keep.stat().st_mode & 0o777) == 0o700,
              oct(keep.stat().st_mode & 0o777))
        check("a directory the script made does not survive",
              not any(p.is_dir() for p in pathlib.Path(tmp).glob("mode2-secret.*")),
              "scratch dirs left behind: %r"
              % sorted(p.name for p in pathlib.Path(tmp).glob("mode2-secret.*")))

        print("the gateway needs no API credential")
        gw = {"MODELGATEWAY": "true"}
        on = run(tmp, "gw", versions=GOOD_GW, env_extra=gw)
        check("exits 0 without an API credential", on.code == 0, on.out)
        check("writes only native gateway dependencies", sorted(p.name[4:] for p in on.state.glob("key.*")) == sorted(KEYS_GW), on.out)
        check("only database and optional direct routes are fetched", all("--secret=database-url" in line or "--secret=model-providers" in line for line in on.calls().splitlines() if line.startswith("gcloud")), on.calls())

        # `kubectl apply` drops a key its previous apply wrote, and the Secret is
        # applied before `helm upgrade --atomic`: a flip of the switch that rolls
        # back would restart the old revision on a Secret without its key. So
        # each mode carries the other's when Secret Manager has it — and only
        # then, and never at the price of a deploy that does not read it.
        print("gateway mode retains optional direct routes for rollback")
        both = dict(GOOD_GW, **{"model-providers": GOOD["model-providers"]})
        c = run(tmp, "carryon", versions=both, env_extra=gw)
        check("gateway on: optional direct routes ride along",
              c.code == 0 and sorted(p.name[len("key."):] for p in c.state.glob("key.*"))
              == sorted(KEYS_GW + ["model-providers.json"])
              and c.key("model-providers.json") == GOOD["model-providers"], c.out)
        ghc = run(tmp, "ghcarryon", versions=both, env_extra=gw, github=True)
        check("...with the api_key inside them masked",
              ghc.code == 0 and "::add-mask::sk-ant-0123456789" in ghc.proc.stdout, ghc.out)
        check("...and their absence is silent", on.code == 0 and "Not carrying" not in on.out, on.out)
        for label, versions, faults in (
            ("unreadable routes", both, ("denied.model-providers",)),
            ("routes no rollback could use", dict(both, **{"model-providers": b'{"model":"*"}'}), ()),
        ):
            u = run(tmp, "carrybad", versions=versions, env_extra=gw, faults=faults)
            check("gateway on, %s: deployed without them, and said so" % label,
                  u.code == 0 and u.key("model-providers.json") is None
                  and u.key("database-url") == GOOD_GW["database-url"]
                  and "Not carrying 'model-providers'" in u.out, u.out)
        print("operator-managed Secret data survives without being fetched")
        retained = b"operator-managed-credential"
        existing = {"metadata": {"resourceVersion": "41"}, "data": {
            "operator-credential": base64.b64encode(retained).decode(),
            "database-url": base64.b64encode(b"old-dsn").decode(),
        }}
        preserved = run(tmp, "retained", versions=GOOD_GW, env_extra=gw, existing=existing)
        check("operator data survives and current fields rotate", preserved.code == 0 and preserved.key("operator-credential") == retained and preserved.key("database-url") == GOOD["database-url"], preserved.out)
        manifest = json.loads((preserved.state / "secret.manifest.json").read_text()) if preserved.code == 0 else {}
        check("apply carries the observed resource version", manifest.get("metadata", {}).get("resourceVersion") == "41")
        check("retained values never reach argv or logs", retained.decode() not in preserved.calls() + preserved.out and base64.b64encode(retained).decode() not in preserved.out)
        same_env = pathlib.Path(tmp) / "same-secret.env"
        same = run(tmp, "identical", versions=GOOD_GW, env_extra={**gw, "GITHUB_ENV": str(same_env)}, existing=manifest)
        check("identical data does not rewrite last-applied resourceVersion",
              same.code == 0 and "secret/map-platform unchanged" in same.out
              and same.calls().count("kubectl apply -f -") == 1
              and not (same.state / "secret.manifest.json").exists(), same.out + same.calls())
        check("an unchanged Secret does not restart consumers",
              same_env.exists() and same_env.read_text() == "SECRET_CHANGED=false\n")
        raced = run(tmp, "concurrent-create", versions=GOOD_GW, env_extra=gw, faults=["concurrent-create"])
        check("a concurrent creator cannot be overwritten",
              raced.code != 0 and raced.key("database-url") is None
              and "kubectl create -f -" in raced.calls(), raced.out + raced.calls())
        check("failed create diagnostics expose no encoded credential",
              base64.b64encode(GOOD["database-url"]).decode() not in raced.out, raced.out)
        for fault in ["read-secret", "apply-secret"]:
            failed = run(tmp, "failed-" + fault, versions=GOOD_GW, env_extra=gw, existing=existing, faults=[fault])
            check("cluster failure stops the deployment: " + fault, failed.code != 0 and failed.key("database-url") is None, failed.out)
            check("failed patch never exposes encoded credentials", retained.decode() not in failed.out and base64.b64encode(retained).decode() not in failed.out and base64.b64encode(GOOD["database-url"]).decode() not in failed.out, failed.out)

        print("MODELGATEWAY is required, and takes true or false and nothing that only looks like one")
        for value in (None, "", "1", "yes", "True", " true"):
            m = run(tmp, "gwvalue", versions=GOOD_GW, env_extra={"MODELGATEWAY": value})
            check("MODELGATEWAY=%r is refused" % value,
                  m.code != 0 and "MODELGATEWAY" in m.out, m.out)
            check("...and nothing is fetched or applied", "gcloud" not in m.calls()
                  and not (m.state / "applied.manifest").exists(), m.calls())

        print("a SECRET_DIR that does not exist yet is created, not refused")
        # The scenario above hands over a directory that already exists, so the
        # `mkdir -p` branch runs in neither it nor any other case. The header
        # documents SECRET_DIR as a path the caller chooses, which reads as one
        # they may not have made.
        fresh = pathlib.Path(tmp) / "fresh.secretdir" / "nested"
        f = run(tmp, "freshdir", env_extra={"SECRET_DIR": str(fresh)})
        check("exits 0", f.code == 0, f.out)
        check("the directory is created with the values in it",
              (fresh / "database-url").exists(),
              sorted(p.name for p in fresh.glob("*")) if fresh.exists() else "absent")
        check("...and at mode 700", fresh.exists()
              and (fresh.stat().st_mode & 0o777) == 0o700,
              oct(fresh.stat().st_mode & 0o777) if fresh.exists() else "absent")

        print()
        if failures:
            print("FAILED: %d check(s)" % len(failures))
            for f in failures:
                print("  - %s" % f)
            return 1
        print("ok: mode2-secret.sh assembles the Secret and refuses every input that would apply cleanly and fail later")
        return 0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
