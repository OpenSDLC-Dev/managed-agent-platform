#!/usr/bin/env python3
"""Exercise tfvars.sh against a fake `gcloud`, credential-free and free of charge.

Why this exists rather than a careful read: the whole job of this script is to
turn one gcloud answer into a value Terraform will accept, and every way that
goes wrong is invisible to shellcheck. `gcloud builds get-default-service-account`
prints `projects/…/serviceAccounts/EMAIL` in some versions and the bare email in
others; on Windows it terminates the line CRLF. Both produce a file that looks
right and fails Terraform's own validation later, during an apply that has
already built a cluster — which is exactly where variables.tf says not to find
out.

The other half is refusal. An existing terraform.tfvars may carry
master_authorized_cidrs, iap_backend_service and iap_members, all three of which
decide who can reach the cluster; a generator that overwrote it would widen or
close that door with no diff to notice it in.

Run: make gcp-tfvars-test
"""

import os
import pathlib
import re
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).parent.resolve()
SCRIPT = HERE / "tfvars.sh"
VARIABLES = HERE / "environment" / "variables.tf"


def terraform_validation_regex():
    """The pattern variables.tf ENFORCES on cloud_build_service_account.

    Read out of the configuration rather than restated here: the whole point of
    this test is that the generator cannot write a file Terraform will reject,
    and a paraphrase of the rule could drift from it without either side moving.
    Terraform's regex flavour is RE2 and this one is plain enough to hold in
    Python unchanged; the only translation is HCL's string escaping.
    """
    text = VARIABLES.read_text(encoding="utf-8")
    m = re.search(r'can\(regex\("(.*?)",\s*var\.cloud_build_service_account\)\)', text)
    if not m:
        raise SystemExit("could not find cloud_build_service_account's validation regex "
                         "in environment/variables.tf — this test can no longer check "
                         "what Terraform enforces")
    return re.compile(m.group(1).replace("\\\\", "\\"))

FAKE_GCLOUD = r"""#!/usr/bin/env bash
# Answers only the one call tfvars.sh makes; anything else is a test bug.
case "$1 $2" in
  "builds get-default-service-account") ;;
  *) echo "fake gcloud: unexpected call: $*" >&2; exit 99 ;;
esac
if [ -n "${FAKE_FAIL:-}" ]; then
  printf '%s\n' "$FAKE_FAIL" >&2
  exit 1
fi
printf '%b' "$FAKE_ACCOUNT"
"""


def run(tmp, account=None, fail=None, project="my-proj", prefix=None, out=None, kms=None):
    bin_dir = tmp / "bin"
    bin_dir.mkdir(exist_ok=True)
    g = bin_dir / "gcloud"
    g.write_text(FAKE_GCLOUD, encoding="utf-8")
    g.chmod(0o755)

    env = dict(os.environ)
    env["PATH"] = f"{bin_dir}:{env['PATH']}"
    env["FAKE_ACCOUNT"] = account or ""
    if fail is not None:
        env["FAKE_FAIL"] = fail
    else:
        env.pop("FAKE_FAIL", None)
    if project is None:
        env.pop("PROJECT", None)
    else:
        env["PROJECT"] = project
    if prefix is not None:
        env["NAME_PREFIX"] = prefix
    else:
        env.pop("NAME_PREFIX", None)
    # Empty is how the Makefile passes an unset KMS_LOCATION, so the empty case
    # is the one that has to leave the key out rather than write `= ""`.
    env["KMS_LOCATION"] = kms or ""
    env["OUT"] = str(out or (tmp / "terraform.tfvars"))
    return subprocess.run(["bash", str(SCRIPT)], capture_output=True, text=True,
                          env=env, check=False)


def parse(path):
    """The generated file as {key: value}, ignoring comments."""
    got = {}
    for line in pathlib.Path(path).read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        k, _, v = line.partition("=")
        got[k.strip()] = v.strip().strip('"')
    return got


def main():
    failures = []
    accepts = terraform_validation_regex()

    def check(name, cond, detail=""):
        if cond:
            print(f"ok   {name}")
        else:
            print(f"FAIL {name}")
            failures.append(f"{name}: {detail}")

    with tempfile.TemporaryDirectory() as d:
        tmp = pathlib.Path(d)

        # 1. The ordinary answer.
        out = tmp / "a.tfvars"
        r = run(tmp, account="123-compute@developer.gserviceaccount.com\\n", out=out)
        got = parse(out) if out.exists() else {}
        check("a bare email is written verbatim",
              r.returncode == 0 and got.get("cloud_build_service_account")
              == "123-compute@developer.gserviceaccount.com", f"{r.stderr} {got}")
        check("project_id and name_prefix come from the environment",
              got.get("project_id") == "my-proj" and got.get("name_prefix") == "map",
              str(got))

        # 2. The resource-name form variables.tf's validation message warns about.
        out = tmp / "b.tfvars"
        r = run(tmp, account="projects/p/serviceAccounts/9@cloudbuild.gserviceaccount.com\\n",
                out=out)
        got = parse(out) if out.exists() else {}
        check("a projects/…/serviceAccounts/ prefix is stripped",
              r.returncode == 0 and got.get("cloud_build_service_account")
              == "9@cloudbuild.gserviceaccount.com", f"{r.stderr} {got}")

        # 3. Windows CRLF, which env-power.sh hit for real.
        out = tmp / "c.tfvars"
        r = run(tmp, account="9@cloudbuild.gserviceaccount.com\\r\\n", out=out)
        got = parse(out) if out.exists() else {}
        check("a CR from gcloud on Windows is stripped",
              r.returncode == 0
              and got.get("cloud_build_service_account") == "9@cloudbuild.gserviceaccount.com"
              and "\r" not in out.read_text(encoding="utf-8"), f"{r.stderr} {got!r}")

        # 4. NAME_PREFIX is carried through, so the file and env-power.sh agree.
        out = tmp / "d.tfvars"
        r = run(tmp, account="9@cloudbuild.gserviceaccount.com\\n", prefix="alt", out=out)
        got = parse(out) if out.exists() else {}
        check("NAME_PREFIX reaches the file", got.get("name_prefix") == "alt", str(got))

        # 5. kms_location has to reach the file when it is not the shared default,
        #    because environment/ looks the key ring up by location and foundation/
        #    created it in one. Absent when unset: writing the default explicitly
        #    would be a second place for it to drift.
        out = tmp / "k1.tfvars"
        run(tmp, account="9@cloudbuild.gserviceaccount.com\\n", kms="europe-west1", out=out)
        got = parse(out) if out.exists() else {}
        check("KMS_LOCATION reaches the file when set",
              got.get("kms_location") == "europe-west1", str(got))
        out = tmp / "k2.tfvars"
        run(tmp, account="9@cloudbuild.gserviceaccount.com\\n", out=out)
        got = parse(out) if out.exists() else {}
        check("kms_location is absent when unset, not written empty",
              "kms_location" not in got, str(got))

        # 6. The API is off — the documented order was not followed.
        out = tmp / "e.tfvars"
        r = run(tmp, fail="ERROR: (gcloud.builds.get-default-service-account) "
                          "SERVICE_DISABLED: Cloud Build API has not been used in project",
                out=out)
        check("a disabled API refuses, names the fix, and writes nothing",
              r.returncode == 1 and not out.exists()
              and "gcp-foundation-apply" in r.stderr, f"{r.returncode} {r.stderr}")

        # 6. Anything that is not an account must not reach the file: Terraform's
        #    own validation would reject it, mid-apply.
        for junk in ("not-an-account", "", "has space@x.gserviceaccount.com",
                     "projects/p/serviceAccounts/"):
            out = tmp / f"f{abs(hash(junk))}.tfvars"
            r = run(tmp, account=junk + "\\n", out=out)
            check(f"junk {junk!r} is refused",
                  r.returncode == 1 and not out.exists(), f"{r.returncode} {r.stderr}")

        # 6b. The empty answer is refused like the rest, but it is the only one
        #     that does not look like a failure — `--format='value(KEY)'` prints
        #     nothing and exits 0 when KEY is absent — so it has to say where to
        #     look. Asserted separately because the loop above only proves the
        #     refusal, and a message that points nowhere would still pass it.
        out = tmp / "empty.tfvars"
        r = run(tmp, account="\\n", out=out)
        check("an empty answer names --format as the thing to check",
              r.returncode == 1 and not out.exists() and "--format" in r.stderr,
              f"{r.returncode} {r.stderr}")

        # 7. Whatever it writes must satisfy the rule Terraform itself applies —
        #    checked against the regex read out of variables.tf, so the two
        #    cannot drift apart. This is the assertion that makes the prefix and
        #    CR handling above matter rather than merely tidy.
        for form in ("9@cloudbuild.gserviceaccount.com",
                     "projects/p/serviceAccounts/9-compute@developer.gserviceaccount.com",
                     "9@my-proj.iam.gserviceaccount.com"):
            out = tmp / f"v{abs(hash(form))}.tfvars"
            run(tmp, account=form + "\\r\\n", out=out)
            got = parse(out).get("cloud_build_service_account", "") if out.exists() else ""
            check(f"what it writes for {form.split('/')[-1]!r} passes Terraform's own validation",
                  bool(accepts.fullmatch(got)), f"wrote {got!r}")

        # 8. An existing file is never overwritten.
        out = tmp / "g.tfvars"
        out.write_text('master_authorized_cidrs = ["203.0.113.4/32"]\n', encoding="utf-8")
        before = out.read_text(encoding="utf-8")
        r = run(tmp, account="9@cloudbuild.gserviceaccount.com\\n", out=out)
        check("an existing file is refused, not merged or clobbered",
              r.returncode == 1 and out.read_text(encoding="utf-8") == before
              and "master_authorized_cidrs" in r.stderr, f"{r.returncode} {r.stderr}")

        # 8. Usage.
        out = tmp / "h.tfvars"
        r = run(tmp, account="9@cloudbuild.gserviceaccount.com\\n", project=None, out=out)
        check("no PROJECT is a usage error, and calls nothing",
              r.returncode == 2 and not out.exists(), f"{r.returncode} {r.stderr}")

    if failures:
        print("\n" + "\n".join(failures), file=sys.stderr)
        return 1
    print("ok: tfvars.sh writes what Terraform accepts, and refuses everything else")
    return 0


if __name__ == "__main__":
    sys.exit(main())
