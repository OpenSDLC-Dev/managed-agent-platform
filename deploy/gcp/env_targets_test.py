#!/usr/bin/env python3
"""Drive the gcp-env-* make targets against a fake `terraform`, credential-free.

The guards these targets carry are the most dangerous code in deploy/gcp/, and
until this existed they were the only part of it nothing ran. `tfvars.sh` has a
test; `env-power.sh` has one; the recipes that decide **whether to destroy** had
none, so every one of these stayed green under mutation: deleting
`gcp-env-vars-match` from a prerequisite list, deleting the empty-state refusal,
changing the derived bucket, letting `gcp-env-init` skip the coordinate check.
Each is one line, and each reopens a way to operate on the wrong environment's
state with the whole gate passing.

It runs `make` in a COPY of the tree with a fake `terraform` first on PATH, so
what is asserted is the real recipe — its prerequisite ORDER included — and not
a restatement of it. The fake logs every invocation and refuses to be silent
about one it did not expect, which is what makes "destroy was never called" an
assertion rather than an absence.

Run: make gcp-env-targets-test
"""

import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).parent.resolve()
ROOT = HERE.parent.parent

FAKE_TERRAFORM = r"""#!/usr/bin/env bash
# Logs the whole command line, one invocation per line, then answers minimally.
echo "$*" >>"$TF_LOG_FILE"
for a in "$@"; do
  case "$a" in
  state) # `state list` — the emptiness the destroy guard reads.
    if [ "${FAKE_STATE_FAILS:-}" = 1 ]; then
      echo "fake terraform: could not read state" >&2
      exit 1
    fi
    printf '%s' "${FAKE_STATE:-}"
    exit 0
    ;;
  esac
done
exit 0
"""


def build_tree(tmp):
    """A copy of the repo's Makefile and deploy/gcp/, and a fake terraform."""
    tree = tmp / "tree"
    (tree / "deploy").mkdir(parents=True)
    shutil.copy(ROOT / "Makefile", tree / "Makefile")
    shutil.copytree(HERE, tree / "deploy" / "gcp",
                    ignore=shutil.ignore_patterns(".terraform", "*.tfstate*"))

    bin_dir = tmp / "bin"
    bin_dir.mkdir()
    tf = bin_dir / "terraform"
    tf.write_text(FAKE_TERRAFORM, encoding="utf-8")
    tf.chmod(0o755)
    # gcp-env-tfvars shells out to gcloud. A fake is not optional here: without
    # one on PATH the REAL gcloud runs, against whatever project the developer
    # happens to be authenticated to. Tests do not touch anybody's cloud.
    gc = bin_dir / "gcloud"
    gc.write_text("#!/usr/bin/env bash\n"
                  "echo \"$*\" >>\"$TF_LOG_FILE\"\n"
                  "printf '9@cloudbuild.gserviceaccount.com'\n", encoding="utf-8")
    gc.chmod(0o755)
    return tree, bin_dir


def run_make(tree, bin_dir, target, project="my-proj", prefix=None,
             state="", *, state_fails=False, extra=(),
             tfvars='project_id = "my-proj"\nname_prefix = "map"\n',
             stray_out=None):
    """Run one target; return (returncode, stdout+stderr, [terraform calls])."""
    env_dir = tree / "deploy" / "gcp" / "environment"
    tfvars_path = env_dir / "terraform.tfvars"
    if tfvars is None:
        tfvars_path.unlink(missing_ok=True)
    else:
        tfvars_path.write_text(tfvars, encoding="utf-8")

    log = tree / "tf.log"
    log.write_text("", encoding="utf-8")

    env = dict(os.environ)
    env["PATH"] = f"{bin_dir}:{env['PATH']}"
    env["TF_LOG_FILE"] = str(log)
    env["FAKE_STATE"] = state
    if state_fails:
        env["FAKE_STATE_FAILS"] = "1"
    else:
        env.pop("FAKE_STATE_FAILS", None)
    # Never inherited from the developer's shell into an assertion about them.
    for k in ("PROJECT", "NAME_PREFIX", "OUT", "KMS_LOCATION"):
        env.pop(k, None)
    # ...except when the point IS the inherited one: the recipe pins OUT, and
    # popping it here is exactly what made the pin's absence invisible.
    if stray_out is not None:
        env["OUT"] = str(stray_out)

    argv = ["make", "-C", str(tree), target]
    if project is not None:
        argv.append(f"PROJECT={project}")
    if prefix is not None:
        argv.append(f"NAME_PREFIX={prefix}")
    argv.extend(extra)

    r = subprocess.run(argv, capture_output=True, text=True, env=env, check=False)
    calls = [c for c in log.read_text(encoding="utf-8").splitlines() if c]
    return r.returncode, r.stdout + r.stderr, calls


def main():
    failures = []

    def check(name, cond, detail=""):
        if cond:
            print(f"ok   {name}")
        else:
            print(f"FAIL {name}")
            failures.append(f"{name}: {detail}")

    def called(calls, verb):
        """Did terraform run this subcommand? Matched on the word, so
        `-chdir=…/environment` can never be mistaken for `destroy`."""
        return any(verb in c.split() for c in calls)

    with tempfile.TemporaryDirectory() as td:
        tmp = pathlib.Path(td)
        tree, bin_dir = build_tree(tmp)

        # 1. The bucket the recipes actually pass, composed the way
        #    foundation/main.tf composes it.
        rc, _, calls = run_make(tree, bin_dir, "gcp-env-init")
        check("gcp-env-init derives <project>-map-tfstate",
              rc == 0 and any('-backend-config=bucket=my-proj-map-tfstate' in c
                              for c in calls), f"{rc} {calls}")
        check("gcp-env-init inits and does nothing else",
              called(calls, "init") and not called(calls, "apply")
              and not called(calls, "destroy"), str(calls))

        rc, _, calls = run_make(tree, bin_dir, "gcp-env-init", prefix="acme",
                                tfvars='project_id = "my-proj"\nname_prefix = "acme"\n')
        check("NAME_PREFIX reaches the bucket",
              rc == 0 and any('bucket=my-proj-acme-tfstate' in c for c in calls),
              f"{rc} {calls}")

        # 2. The override that was removed. It chose the bucket while the tfvars
        #    still chose the resources, and the coordinate guard could not see
        #    it — so it has to be unreachable from the command line too, which
        #    only `override` achieves.
        #    Both variables, because locking only the inner one looked sufficient
        #    and was not: TF_BACKEND is what the recipes actually pass, so
        #    overriding IT replaces the whole flag and reaches any bucket at all.
        for var, value in (
            ("TF_STATE_BUCKET", "someone-elses-bucket"),
            ("TF_BACKEND", "-backend-config=bucket=someone-elses-bucket"),
        ):
            rc, _, calls = run_make(tree, bin_dir, "gcp-env-init",
                                    extra=(f"{var}={value}",))
            check(f"{var} cannot redirect the backend",
                  all("someone-elses-bucket" not in c for c in calls)
                  and any("bucket=my-proj-map-tfstate" in c for c in calls), str(calls))

        # 2b. The other half of that rule, and the reason the two above are the
        #     ONLY ones locked. A variable UPSTREAM of the guard's comparison —
        #     here the internal NAME_PREFIX_OR_DEFAULT — moves the bucket and
        #     what the guard expects together, so overriding it is just choosing
        #     another environment: it refuses when the tfvars disagrees, and
        #     works when it agrees. Locking it would break the legitimate
        #     NAME_PREFIX knob; not locking it is safe for a reason, not by
        #     omission, and this pins that reason.
        rc, out, calls = run_make(tree, bin_dir, "gcp-env-init",
                                  extra=("NAME_PREFIX_OR_DEFAULT=evil",))
        check("an upstream override still faces the guard",
              rc != 0 and calls == [] and "name_prefix" in out,
              f"rc={rc} calls={calls}")
        rc, out, calls = run_make(tree, bin_dir, "gcp-env-init",
                                  extra=("NAME_PREFIX_OR_DEFAULT=evil",),
                                  tfvars='project_id = "my-proj"\nname_prefix = "evil"\n')
        check("...and is coherent once the tfvars agrees",
              rc == 0 and any("bucket=my-proj-evil-tfstate" in c for c in calls),
              f"rc={rc} calls={calls}")

        # 3. The coordinate guard, on every target that selects a backend, and
        #    BEFORE terraform is invoked at all — not merely somewhere in the
        #    recipe. Nothing may have run by the time it refuses.
        # gcp-db-init is in this list because it READS the backend — seven
        # `terraform output` calls — so it selects a state the same way the
        # writers do, and was the one target the guard did not cover.
        for target in ("gcp-env-init", "gcp-env-apply", "gcp-env-destroy",
                       "gcp-env-migrate-state", "gcp-db-init"):
            rc, out, calls = run_make(tree, bin_dir, target, prefix="acme",
                                      tfvars='project_id = "my-proj"\nname_prefix = "map"\n')
            check(f"{target} refuses a coordinate mismatch before running terraform",
                  rc != 0 and calls == [] and "name_prefix" in out,
                  f"rc={rc} calls={calls}")

        # 4. And a missing tfvars, which is the fresh-checkout case: it must name
        #    the target that writes one rather than let Terraform prompt.
        rc, out, calls = run_make(tree, bin_dir, "gcp-env-apply", tfvars=None)
        check("a missing tfvars stops apply and names gcp-env-tfvars",
              rc != 0 and calls == [] and "gcp-env-tfvars" in out,
              f"rc={rc} calls={calls}")

        # 5. The empty-state refusal — #478 surviving its own fix. A bucket with
        #    no state object yields an empty state, and destroying it reports
        #    success over a running environment.
        rc, out, calls = run_make(tree, bin_dir, "gcp-env-destroy", state="")
        check("destroy refuses an EMPTY state and never calls destroy",
              rc != 0 and not called(calls, "destroy") and "holds no state" in out,
              f"rc={rc} calls={calls}")
        check("...having initialized first, so the emptiness is the bucket's",
              called(calls, "init"), str(calls))

        # 6. A state it cannot READ is not an empty one, and the diagnosis for
        #    the two is different. Empty stdout is what they share.
        rc, out, calls = run_make(tree, bin_dir, "gcp-env-destroy", state_fails=True)
        check("destroy refuses when the state cannot be READ, and says so",
              rc != 0 and not called(calls, "destroy")
              and "could not READ" in out, f"rc={rc} calls={calls}")

        # 7. The positive case, or all of the above would pass with destroy
        #    simply broken.
        rc, out, calls = run_make(tree, bin_dir, "gcp-env-destroy",
                                  state="google_container_cluster.map\n")
        check("destroy DOES run when the state holds resources",
              rc == 0 and called(calls, "destroy"), f"rc={rc} calls={calls}")

        # 8. apply and migrate-state reach terraform on the happy path, with the
        #    guard satisfied — and every one of them carries the BUCKET. Checking
        #    only that `init` was called leaves `$(TF_BACKEND)` deletable from
        #    three of the four recipes with the suite still green.
        bucket = "-backend-config=bucket=my-proj-map-tfstate"
        rc, out, calls = run_make(tree, bin_dir, "gcp-env-apply")
        check("apply inits WITH the backend config and then applies",
              rc == 0 and any("init" in c.split() and bucket in c for c in calls)
              and called(calls, "apply"), f"rc={rc} calls={calls}")

        rc, out, calls = run_make(tree, bin_dir, "gcp-env-destroy",
                                  state="google_container_cluster.map\n")
        check("destroy inits WITH the backend config",
              any("init" in c.split() and bucket in c for c in calls),
              f"rc={rc} calls={calls}")

        rc, out, calls = run_make(tree, bin_dir, "gcp-env-migrate-state")
        check("migrate-state inits with -migrate-state, the bucket, and NOT -input=false",
              rc == 0 and any("-migrate-state" in c and bucket in c
                              and "-input=false" not in c for c in calls),
              f"rc={rc} calls={calls}")

        # 7b. ...and the other three MUST carry -input=false, which the check
        #     above only says migrate-state must not. The flag is the reason
        #     running them before gcp-env-migrate-state is safe: with local state
        #     present and the backend changed, Terraform asks to migrate, and
        #     -input=false turns that prompt into a refusal instead of a silent
        #     adoption of an empty remote state. Asserting only its ABSENCE on
        #     one recipe leaves it deletable from the three where it is the
        #     safety property.
        for target, state in (("gcp-env-init", ""),
                              ("gcp-env-apply", ""),
                              ("gcp-env-destroy", "google_container_cluster.map\n")):
            rc, out, calls = run_make(tree, bin_dir, target, state=state)
            inits = [c for c in calls if "init" in c.split()]
            check(f"{target} inits with -input=false, so a migration prompt refuses",
                  bool(inits) and all("-input=false" in c for c in inits),
                  f"rc={rc} calls={calls}")

        # 7c. gcp-db-init must SELECT the backend before it reads anything from
        #     it. The mismatch loop above cannot see that: it goes green with
        #     either prerequisite deleted, because the other one still refuses —
        #     so it proves a guard exists, not that gcp-env-init runs first, and
        #     that ordering is the whole point of the prerequisite. Assert on the
        #     FIRST terraform call, which is the init, on the happy path.
        #
        #     dbinit.sh stops shortly after, on a missing kubectl or on the fake
        #     terraform answering `output` with nothing. Both are fine and both
        #     land after the init, so this reads calls[0] and ignores the rest.
        rc, out, calls = run_make(tree, bin_dir, "gcp-db-init")
        first = calls[0] if calls else ""
        check("gcp-db-init selects the backend before reading an output",
              bool(calls) and "init" in first.split() and bucket in first
              and "-input=false" in first,
              f"rc={rc} first={first!r} calls={len(calls)}")

        # 8b. The recipe PINS OUT, and popping it from the environment above is
        #     precisely what hid the pin's absence. A stray OUT — left over from
        #     anything — points --check at a file Terraform will not read, and
        #     without the pin it agrees with it. Assert against a stray that
        #     WOULD agree while the real tfvars does not.
        stray = tree / "stray.tfvars"
        stray.write_text('project_id = "somewhere-else"\n', encoding="utf-8")
        rc, out, calls = run_make(tree, bin_dir, "gcp-env-apply",
                                  project="somewhere-else", stray_out=stray)
        check("a stray OUT cannot redirect the coordinate check",
              rc != 0 and calls == [] and "project_id" in out,
              f"rc={rc} calls={calls} out={out[:200]}")

        # 8c. ...and the generator pins OUT too, where the consequence is worse:
        #     generation READS OUT, so a stray one writes the file where
        #     Terraform will never look while reporting success, and the next
        #     apply then tells you to run the command that just "worked".
        env_tfvars = tree / "deploy" / "gcp" / "environment" / "terraform.tfvars"
        env_tfvars.unlink(missing_ok=True)
        stray_gen = tree / "stray-generated.tfvars"
        rc, out, _ = run_make(tree, bin_dir, "gcp-env-tfvars",
                              stray_out=stray_gen, tfvars=None)
        check("a stray OUT cannot redirect where the generator writes",
              env_tfvars.exists() and not stray_gen.exists(),
              f"rc={rc} real={env_tfvars.exists()} stray={stray_gen.exists()} out={out[:200]}")

        # 8d. ...and the recipe has to PASS KMS_LOCATION, which 8c does not say:
        #     it asserts where the file is written and never what is in it. Drop
        #     the assignment and the operator's choice lands on the floor —
        #     foundation/ keyed in one location, environment/ configured for
        #     another. That fails at plan rather than silently, but only after a
        #     tfvars that looks correct has been written and reported as success.
        env_tfvars.unlink(missing_ok=True)
        rc, out, _ = run_make(tree, bin_dir, "gcp-env-tfvars", tfvars=None,
                              extra=("KMS_LOCATION=us-west1",))
        written = env_tfvars.read_text(encoding="utf-8") if env_tfvars.exists() else ""
        check("KMS_LOCATION reaches the generated tfvars",
              rc == 0 and 'kms_location' in written and '"us-west1"' in written,
              f"rc={rc} written={written[:200]!r} out={out[:200]}")

        # 9. No PROJECT, no bucket name — and nothing run.
        # gcp-db-init is deliberately NOT in this list, though it now carries the
        # same prerequisite. dbinit.sh refuses a missing PROJECT on its own, so
        # the assertion would hold with the prerequisite deleted — it would be a
        # check that cannot fail, which is worse than no check. What pins its
        # prerequisites is 7c, which reads the first terraform call: the
        # coordinate-mismatch loop alone does not, because deleting either
        # prerequisite leaves the other refusing and the loop green.
        for target in ("gcp-env-init", "gcp-env-apply", "gcp-env-destroy",
                       "gcp-env-migrate-state", "gcp-env-tfvars"):
            rc, out, calls = run_make(tree, bin_dir, target, project=None)
            check(f"{target} refuses without PROJECT, having run nothing",
                  rc == 2 and calls == [] and "PROJECT is required" in out,
                  f"rc={rc} calls={calls}")

    if failures:
        print("\n" + "\n".join(failures), file=sys.stderr)
        return 1
    print("ok: the gcp-env-* targets guard the state they select")
    return 0


if __name__ == "__main__":
    sys.exit(main())
