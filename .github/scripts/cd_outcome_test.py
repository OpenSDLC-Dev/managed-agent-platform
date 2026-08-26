#!/usr/bin/env python3
"""Table test for cd-outcome.sh, the classifier deploy-alert.yml keys on.

Run it rather than read it — `make cd-outcome-test`.

Each row names the real situation it stands for, because the risk in a classifier
like this is not a wrong table but a MISSING row: a state that quietly lands in
the do-nothing arm and stops the notifier working without failing anything.

The `steps-ran` column is measured, not assumed. Every run that reaches a runner
has `Set up job` at `success`, so a parked run — the case this whole PR turns on —
reports ran=true even though it deployed nothing. Only a run that never started
reports false. Getting that column wrong once already made the parked row
describe a shape GitHub does not produce.
"""

import pathlib
import subprocess
import sys

SCRIPT = pathlib.Path(__file__).with_name("cd-outcome.sh")

# (run conclusion, "Deploy the chart" conclusion, any step ran) -> outcome
CASES = [
    # The two ordinary days. Both have ran=true: the parked run still executed
    # the steps up to and including the parked check (measured on run
    # 32916154759 — Set up job, Require the deployment variables, and the parked
    # check all `success`, Deploy the chart `skipped`).
    ("success", "success", "true", "deployed", "the release was installed"),
    ("success", "skipped", "true", "parked", "staging parked; deploy.yml skipped the deploying steps"),
    # Failure, in each shape GitHub reports it. A job that reached a runner has
    # ran=true even when it died early; ran=false means it never started.
    ("failure", "failure", "true", "broken", "the install itself failed"),
    ("failure", "success", "true", "broken", "installed, then a post-deploy check failed"),
    ("failure", "skipped", "true", "broken", "parked, and something after the parked check still failed"),
    ("failure", "absent", "true", "broken", "died before reaching the install"),
    ("failure", "absent", "false", "broken", "never started, and still concluded failure"),
    ("timed_out", "absent", "true", "broken", "the job hit its time limit"),
    ("startup_failure", "absent", "false", "broken", "deploy.yml itself is invalid"),
    ("neutral", "absent", "false", "broken", "no evidence anything deployed"),
    ("stale", "absent", "false", "broken", "likewise"),
    ("action_required", "absent", "false", "broken", "likewise"),
    # Cancellation is two different events wearing one word.
    ("cancelled", "absent", "false", "superseded",
     "cancelled while PENDING — a newer merge queued behind it, nothing ran"),
    ("cancelled", "success", "true", "broken",
     "cancelled MID-FLIGHT — deploy.yml calls this the wedged-release state"),
    ("cancelled", "cancelled", "true", "broken",
     "cancelled DURING helm upgrade — the release state is indeterminate"),
    ("cancelled", "skipped", "true", "broken",
     "something ran before the cancel, so it is not a clean supersede"),
    # The job's own `if` declined; no news either way. Defensive — today's
    # deploy.yml cannot produce it.
    ("skipped", "absent", "false", "noop", "dispatched where the job does not run"),
    # The step this classifier keys on is gone. Must be loud, never quiet.
    ("success", "absent", "true", "miswired", "the step was renamed or removed"),
    ("success", "failure", "true", "miswired", "a shape the caller should not produce"),
]


def run(argv):
    return subprocess.run(
        ["bash", str(SCRIPT), *argv],
        capture_output=True, text=True, check=False,
    )


def main():
    failures = []
    for conclusion, step, ran, want, why in CASES:
        got = run([conclusion, step, ran])
        actual = got.stdout.strip()
        if actual != want or got.returncode != 0:
            failures.append(
                f"  {conclusion:16s} {step:9s} ran={ran:5s} -> {actual or '(nothing)'!r}"
                f" (exit {got.returncode}), want {want!r} — {why}"
            )

    # A classifier that accepts anything is a classifier that hides a caller bug.
    for argv in ([], ["success"], ["success", "success"], ["a", "b", "c", "d"]):
        got = run(argv)
        if got.returncode != 2:
            failures.append(f"  {argv!r} -> exit {got.returncode}, want 2 (usage)")

    if failures:
        print(f"cd-outcome.sh: {len(failures)} case(s) wrong:", file=sys.stderr)
        print("\n".join(failures), file=sys.stderr)
        return 1
    print(f"ok: cd-outcome.sh classifies all {len(CASES)} cases, and refuses bad arity")
    return 0


if __name__ == "__main__":
    sys.exit(main())
