#!/usr/bin/env python3
"""Exercise tfvars.sh against a fake `gcloud`, credential-free and free of charge.

Why this exists rather than a careful read: the whole job of this script is to
turn one gcloud answer into a value Terraform will accept, and every way that
goes wrong is invisible to shellcheck. `gcloud builds get-default-service-account`
prints `projects/…/serviceAccounts/EMAIL` in some versions and the bare email in
others; on Windows it terminates the line CRLF. Both produce a file that looks
right and then fails Terraform's own variable validation. That happens during
planning, so nothing is left half-built — but it fails a `destroy` in exactly
the same way, and a destroy is what you run against an environment that already
exists and is billing.

The other half is refusal. An existing terraform.tfvars may carry
master_authorized_cidrs, iap_backend_service and iap_members, all three of which
decide who can reach the cluster; a generator that overwrote it would widen or
close that door with no diff to notice it in.

Run: make gcp-tfvars-test
"""

import os
import pathlib
import re
import shutil
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
#
# It checks the FLAGS, not just the verb, because the two that matter are exactly
# the two whose absence still produces a plausible answer. Without --project,
# gcloud reads the CONFIGURED project and returns another project's build account
# — syntactically valid, silently wrong, and then granted artifactregistry.writer
# in a project no build of it will ever run in. Without --format the output shape
# is whatever the version defaults to. A fake that ignored them would let either
# deletion pass the whole suite.
case "$1 $2" in
  "builds get-default-service-account") ;;
  *) echo "fake gcloud: unexpected call: $*" >&2; exit 99 ;;
esac
# Compared per ARGUMENT, not against a flattened "$*": `--project=X --format=Y`
# passed as one string containing a space would satisfy a substring search and
# reach real gcloud as a single malformed argument.
want_project="--project=$EXPECT_PROJECT"
want_format="--format=value(serviceAccountEmail)"
seen_project=0
seen_format=0
for a in "$@"; do
  [ "$a" = "$want_project" ] && seen_project=1
  [ "$a" = "$want_format" ] && seen_format=1
done
if [ "$seen_project" -ne 1 ]; then
  echo "fake gcloud: expected exactly '$want_project' among the arguments: $*" >&2; exit 98
fi
if [ "$seen_format" -ne 1 ]; then
  echo "fake gcloud: expected exactly '$want_format' among the arguments: $*" >&2; exit 97
fi
if [ -n "${FAKE_FAIL:-}" ]; then
  printf '%s\n' "$FAKE_FAIL" >&2
  exit 1
fi
printf '%b' "$FAKE_ACCOUNT"
"""


def run(tmp, account=None, fail=None, project="my-proj", prefix=None, out=None,
        kms=None, args=()):
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
    # What the fake `gcloud` asserts it was told to look in. Separate from
    # PROJECT so the fake cannot read the same variable the script does and
    # agree with itself.
    env["EXPECT_PROJECT"] = project or ""
    if prefix is not None:
        env["NAME_PREFIX"] = prefix
    else:
        env.pop("NAME_PREFIX", None)
    # Empty is how the Makefile passes an unset KMS_LOCATION, so the empty case
    # is the one that has to leave the key out rather than write `= ""`.
    env["KMS_LOCATION"] = kms or ""
    env["OUT"] = str(out or (tmp / "terraform.tfvars"))
    return subprocess.run(["bash", str(SCRIPT), *args], capture_output=True,
                          text=True, env=env, check=False)


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

        # 9. The property that matters, and the one a list of examples cannot
        #    give you: **anything the script writes, Terraform accepts**. One
        #    direction, deliberately. Equality was the first attempt and it was
        #    wrong: `[^@ /]` admits `"`, `\`, `$` and `%`, so Terraform's own
        #    regex takes `x"@y.gserviceaccount.com` — which is then emitted
        #    inside quotes and is not HCL at all. Being STRICTER than Terraform
        #    is correct there; being LOOSER never is, because looser is what
        #    writes a file that only fails later.
        #
        #    So: no false accepts (checked against the regex read out of
        #    variables.tf), and separately every shape GCP actually issues is
        #    still accepted, which is what stops "stricter" drifting into
        #    "rejects everything".
        malformed = [
            "a@b@c.gserviceaccount.com",          # two @
            "@x.gserviceaccount.com",             # empty local part
            "x@.gserviceaccount.com",             # empty domain label
            "x@y.gserviceaccount.com.evil.com",   # suffix is not the end
            "x@ygserviceaccount.com",             # missing the dot
            "x y@z.gserviceaccount.com",          # a space
            "x@gserviceaccount.com",
            "not-an-account",
            "",
            'x"@y.gserviceaccount.com',           # quote: breaks the HCL string
            "x\\@y.gserviceaccount.com",          # backslash: an escape
            "x$@y.gserviceaccount.com",           # $ : ${...} interpolation
            "x%@y.gserviceaccount.com",           # % : %{...} directive
        ]
        real_forms = [
            "123456789@cloudbuild.gserviceaccount.com",
            "123456789-compute@developer.gserviceaccount.com",
            "svc@my-proj.iam.gserviceaccount.com",
            "9@y.z.gserviceaccount.com",
        ]
        for i, value in enumerate(malformed + real_forms):
            out = tmp / f"d{i}.tfvars"
            # No trailing-newline handling to confuse the comparison: the strip
            # is tested above, this is about the accept/reject decision alone.
            r = run(tmp, account=value, out=out)
            written = parse(out).get("cloud_build_service_account") if out.exists() else None
            if value in real_forms:
                check(f"a real service account is accepted: {value!r}",
                      written == value, f"wrote {written!r} rc={r.returncode}")
            else:
                check(f"never writes what Terraform would reject: {value!r}",
                      written is None or bool(accepts.fullmatch(written)),
                      f"wrote {written!r}, which variables.tf "
                      f"{'accepts' if written and accepts.fullmatch(written) else 'rejects'}")

        # 9a. A DIFFERENT project, and it exists for one reason: every other
        #     generation case uses "my-proj", so any constant that happens to
        #     equal it is indistinguishable from the variable. Both of these
        #     survived the suite until this case existed —
        #       gcloud … --project=my-proj          (reads the wrong project)
        #       project_id = "my-proj"              (writes the wrong project)
        #     and the second is worse than it looks: it is a plausible file
        #     naming somebody else's project, which then owns the apply.
        out = tmp / "other.tfvars"
        r = run(tmp, account="9@cloudbuild.gserviceaccount.com",
                project="other-project-77", out=out)
        check("a second project is followed, not a constant that matched the first",
              out.exists() and parse(out).get("project_id") == "other-project-77",
              f"{r.returncode} {r.stdout} {r.stderr}")

        # 9b. Every value must be QUOTED. parse() above strips quotes, so it
        #     cannot see the difference and neither can any assertion built on
        #     it — but Terraform can: `project_id = my-proj` in a var-file is
        #     "Variables may not be used here", another file that looks right
        #     and fails validation. Asserted on the raw text, not the parse.
        out = tmp / "quoted.tfvars"
        run(tmp, account="9@cloudbuild.gserviceaccount.com", out=out, kms="europe-west1")
        assignments = [ln for ln in out.read_text(encoding="utf-8").splitlines()
                       if ln.strip() and not ln.strip().startswith("#")]
        check("every generated assignment quotes its value",
              len(assignments) == 4
              and all(re.fullmatch(r'\S+ += +"[^"]+"', ln) for ln in assignments),
              repr(assignments))

        # 9c. The DEFAULT output path, which is the only one production uses —
        #     `make gcp-env-tfvars` passes no OUT — and which every other case
        #     here overrides, so without this it could point anywhere and stay
        #     green. Exercised against a COPY of the script in a scratch tree,
        #     never with OUT unset against the real one: that would write a
        #     terraform.tfvars into the checkout on any machine where none
        #     exists yet, which is most of them, CI included.
        copy_root = tmp / "copy"
        (copy_root / "environment").mkdir(parents=True)
        shutil.copy(SCRIPT, copy_root / SCRIPT.name)
        env = dict(os.environ)
        env["PATH"] = f"{tmp / 'bin'}:{env['PATH']}"
        env["PROJECT"] = env["EXPECT_PROJECT"] = "my-proj"
        env["FAKE_ACCOUNT"] = "9@cloudbuild.gserviceaccount.com"
        env.pop("FAKE_FAIL", None)
        env.pop("NAME_PREFIX", None)
        env["KMS_LOCATION"] = ""
        env.pop("OUT", None)
        subprocess.run(["bash", str(copy_root / SCRIPT.name)], capture_output=True,
                       text=True, env=env, check=False)
        landed = copy_root / "environment" / "terraform.tfvars"
        check("with OUT unset it writes <script dir>/environment/terraform.tfvars",
              landed.exists()
              and parse(landed).get("project_id") == "my-proj",
              f"exists={landed.exists()}")

        # 10. --check: the mode that stops an apply or a destroy running against
        #     one environment's state while configuring another's. It reads the
        #     file back and compares it to the coordinates that choose the state
        #     bucket, so a disagreement has to be a refusal, not a warning.
        out = tmp / "chk.tfvars"
        run(tmp, account="9@cloudbuild.gserviceaccount.com", out=out, prefix="acme")

        r = run(tmp, out=out, prefix="acme", args=("--check",))
        check("--check passes when the file and the coordinates agree",
              r.returncode == 0, f"{r.returncode} {r.stderr}")

        r = run(tmp, out=out, prefix="map", args=("--check",))
        check("--check refuses a name_prefix the file does not name",
              r.returncode == 1 and "name_prefix" in r.stderr, f"{r.returncode} {r.stderr}")

        r = run(tmp, out=out, prefix="acme", project="other-project", args=("--check",))
        check("--check refuses a PROJECT the file does not name",
              r.returncode == 1 and "project_id" in r.stderr, f"{r.returncode} {r.stderr}")

        r = run(tmp, out=(tmp / "absent.tfvars"), args=("--check",))
        check("--check on a missing file names the target that writes it",
              r.returncode == 1 and "gcp-env-tfvars" in r.stderr, f"{r.returncode} {r.stderr}")

        # A hand-written tfvars may leave name_prefix out, because variables.tf
        # defaults it to the same "map" this script does. That has to READ as
        # agreement, or the check would refuse the most ordinary file there is.
        out = tmp / "implicit.tfvars"
        out.write_text('project_id = "my-proj"\n', encoding="utf-8")
        r = run(tmp, out=out, args=("--check",))
        check("--check treats an omitted name_prefix as the default it is",
              r.returncode == 0, f"{r.returncode} {r.stderr}")

        # 10b. Comments. `/* */` is the bypass that mattered: an assignment inside
        #      one is invisible to Terraform and was visible to a line-oriented
        #      sed, so the check agreed with a value Terraform never saw. `#` and
        #      `//` are the same defect in cheaper clothes.
        for style, text in (
            ("a block comment",
             'project_id = "my-proj"\n\n/*\nname_prefix = "acme"\n*/\n'),
            ("a # comment",
             'project_id = "my-proj"\n# name_prefix = "acme"\n'),
            ("a // comment",
             'project_id = "my-proj"\n// name_prefix = "acme"\n'),
            ("a trailing block comment on the live line",
             'project_id = "my-proj"\nname_prefix = "map" /* not "acme" */\n'),
        ):
            out = tmp / f"c{abs(hash(style))}.tfvars"
            out.write_text(text, encoding="utf-8")
            # NAME_PREFIX=acme: only a checker fooled by the comment agrees.
            r = run(tmp, out=out, prefix="acme", args=("--check",))
            check(f"--check is not fooled by {style}",
                  r.returncode == 1 and "name_prefix" in r.stderr,
                  f"{r.returncode} {r.stderr}")

        # 10c. A quoted `#` or `/*` is data, not a comment, and stripping it
        #      would make the checker read the wrong value — the mirror image of
        #      the bug above, and just as capable of a false refusal.
        out = tmp / "quoted-hash.tfvars"
        out.write_text('project_id = "my-proj" # real\nname_prefix = "a#b"\n', encoding="utf-8")
        r = run(tmp, out=out, prefix="a#b", args=("--check",))
        check("--check keeps a # that is inside a string",
              r.returncode == 0, f"{r.returncode} {r.stderr}")

        # 10d. `*.auto.tfvars` is loaded automatically, AFTER this file, and can
        #      set the same two variables where --check cannot see them. Nothing
        #      in this repo writes one, so its presence means inputs were layered
        #      deliberately — and a guard that quietly stops covering what it
        #      names is worse than no guard.
        d = tmp / "autodir"
        d.mkdir()
        (d / "terraform.tfvars").write_text('project_id = "my-proj"\n', encoding="utf-8")
        (d / "zz.auto.tfvars").write_text('name_prefix = "acme"\n', encoding="utf-8")
        r = run(tmp, out=(d / "terraform.tfvars"), args=("--check",))
        check("--check refuses when a *.auto.tfvars would be loaded too",
              r.returncode == 1 and "auto.tfvars" in r.stderr, f"{r.returncode} {r.stderr}")

        # 10e. A transposed letter must not turn a check into a write. Falling
        #      through to generate mode is the wrong default for the one mode
        #      that has a side effect.
        out = tmp / "typo.tfvars"
        r = run(tmp, account="9@cloudbuild.gserviceaccount.com", out=out,
                args=("--chek",))
        check("a misspelt mode is refused, not treated as generate",
              r.returncode == 2 and not out.exists(), f"{r.returncode} {r.stderr}")

    if failures:
        print("\n" + "\n".join(failures), file=sys.stderr)
        return 1
    print("ok: tfvars.sh writes what Terraform accepts, and refuses everything else")
    return 0


if __name__ == "__main__":
    sys.exit(main())
