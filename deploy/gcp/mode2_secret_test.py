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
  - `kubectl create secret -o yaml` records its whole argv, so the seven keys,
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
import pathlib
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).parent.resolve()
SCRIPT = HERE / "mode2-secret.sh"

# The seven keys, in the order the Secret must carry them. The chart reads all
# seven off one object; a missing `blob-backend` does not fail, it reads as the
# default "s3" and then looks for an endpoint that is not there.
KEYS = [
    "controlplane-api-key",
    "database-url",
    "model-providers.json",
    "blob-backend",
    "blob-bucket",
    "secrets-backend",
    "gcpkms-key-name",
]

# What a healthy project holds. Each is the exact bytes `--out-file` would write.
GOOD = {
    "controlplane-api-key": b"sk-cp-0123456789abcdef",
    "database-url": b"postgres://map:pw@10.0.0.3:5432/map",
    "model-providers": b'[{"model":"*","protocol":"anthropic",'
                       b'"base_url":"https://api.anthropic.com","api_key":"sk-ant-0123456789"}]',
}

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
    if (S / "fault.warn").exists():
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
import os, sys, pathlib
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
    # Record each --from-file's bytes under its Secret key, which is the file's
    # basename -- that identity is the reason the fetch destinations are named
    # after the keys rather than after the Secret Manager secrets.
    for x in a:
        if x.startswith("--from-file="):
            p = pathlib.Path(x.split("=", 1)[1])
            (S / ("key." + p.name)).write_bytes(p.read_bytes())
    print("apiVersion: v1\nkind: Secret\nmetadata:\n  name: " + a[3])
    sys.exit(0)

if a[:2] == ["apply", "-f"]:
    manifest = sys.stdin.read()
    (S / "applied.manifest").write_text(manifest)
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


def run(tmp, name, versions=None, faults=(), env_extra=None, github=False, verb=None):
    state = pathlib.Path(tempfile.mkdtemp(dir=tmp, prefix=name + "."))
    for secret, body in (GOOD if versions is None else versions).items():
        (state / ("ver." + secret)).write_bytes(body)
    for f in faults:
        (state / ("fault." + f)).touch()
    if verb:
        (state / "apply.verb").write_text(verb)
    env = dict(os.environ)
    env["PATH"] = str(pathlib.Path(tmp) / "bin") + os.pathsep + env["PATH"]
    env["FAKE_STATE"] = str(state)
    env["PROJECT"] = "p"
    env["BLOB_BUCKET"] = "map-blobs"
    env["KMS_KEY_NAME"] = "projects/p/locations/us-central1/keyRings/map/cryptoKeys/vault"
    env.pop("K8S_NAMESPACE", None)
    env.pop("K8S_SECRET", None)
    env.pop("SECRET_DIR", None)
    env.pop("GITHUB_ENV", None)
    if github:
        env["GITHUB_ACTIONS"] = "true"
    else:
        env.pop("GITHUB_ACTIONS", None)
    for k, v in (env_extra or {}).items():
        if v is None:
            env.pop(k, None)
        else:
            env[k] = v
    return Run(state, subprocess.run(["bash", str(SCRIPT)], env=env, text=True,
                                     capture_output=True, timeout=120))


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
        check("writes all seven keys", [k for k in KEYS if r.key(k) is not None] == KEYS,
              repr(sorted(p.name for p in r.state.glob("key.*"))))
        check("the namespace is applied before the Secret",
              r.calls().index("create namespace") < r.calls().index("create secret"), r.calls())
        check("reports what apply did on stdout", "created" in r.proc.stdout, r.proc.stdout)

        print("credentials never reach an argv")
        check("no --from-literal anywhere", "--from-literal" not in r.calls(), r.calls())
        for secret, body in GOOD.items():
            check("the %s value is not in any command line" % secret,
                  body.decode() not in r.calls())

        print("each key carries the bytes of its version, verbatim")
        check("controlplane-api-key", r.key("controlplane-api-key") == GOOD["controlplane-api-key"])
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

        print("nothing is created for real — every write goes through apply")
        check("the Secret is created --dry-run=client", "--dry-run=client" in r.calls())
        check("apply read a manifest off the pipe",
              (r.state / "applied.manifest").exists())

        print("a version that would apply cleanly and fail much later is refused")
        for label, body in (("empty", b""), ("whitespace-only", b"   \n"), ("a lone space", b" ")):
            bad = dict(GOOD)
            bad["controlplane-api-key"] = body
            b = run(tmp, "blank", versions=bad)
            check("a %s controlplane-api-key is refused" % label, b.code != 0, b.out)
            check("...and no Secret is applied", not (b.state / "applied.manifest").exists())

        print("a trailing newline is refused on the two values read verbatim")
        for secret in ("controlplane-api-key", "database-url"):
            bad = dict(GOOD)
            bad[secret] = GOOD[secret] + b"\n"
            b = run(tmp, "newline", versions=bad)
            check("a newline in %s is refused" % secret, b.code != 0, b.out)

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

        print("a warning on the success path is not swallowed")
        w = run(tmp, "warn", faults=("fault.warn",))
        check("exits 0 with a warning present", w.code == 0, w.out)

        print("GitHub annotations appear only under GITHUB_ACTIONS")
        gh = run(tmp, "gh", github=True)
        check("exits 0", gh.code == 0, gh.out)
        check("masks each credential before anything can print it",
              gh.out.count("::add-mask::") >= 3, gh.out)
        check("masks the api_key inside model-providers, not the JSON",
              "::add-mask::sk-ant-0123456789" in gh.out, gh.out)
        check("plain run emits no annotations",
              "::add-mask::" not in r.out and "::error::" not in r.out, r.out)
        ghbad = run(tmp, "gherr", versions={k: v for k, v in GOOD.items() if k != "model-providers"},
                    github=True)
        check("a failure is annotated under GITHUB_ACTIONS", "::error::" in ghbad.out, ghbad.out)
        # Ordering, not just presence. Masking a value after something has
        # already printed it is no protection at all, so take the path that
        # fails AFTER the first value is fetched and require the redaction to be
        # registered by then.
        late = dict(GOOD)
        late["database-url"] = GOOD["database-url"] + b"\n"
        gl = run(tmp, "ghorder", versions=late, github=True)
        check("a credential is masked before the run that fails can print anything",
              gl.code != 0
              and "::add-mask::" + GOOD["controlplane-api-key"].decode() in gl.out
              and gl.out.index("::add-mask::") < gl.out.index("contains a newline"),
              gl.out)

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
              (keep / "controlplane-api-key").exists(), sorted(p.name for p in keep.glob("*")))
        # The values are written by `gcloud --out-file`, which does not chmod
        # what it creates, so the directory is the only thing standing between
        # three credentials and every other process on the machine.
        check("...and is left mode 700", (keep.stat().st_mode & 0o777) == 0o700,
              oct(keep.stat().st_mode & 0o777))
        check("a directory the script made does not survive",
              not any(p.is_dir() and p.name.startswith("mode2-secret.")
                      for p in pathlib.Path(tempfile.gettempdir()).glob("mode2-secret.*")
                      if p != keep),
              "scratch dirs left behind")

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
