#!/usr/bin/env python3
"""Table test for evals-health.sh, the classifier evals-alert.yml keys on.

Run it rather than read it — `make evals-alert-test`.

Each row names the run history it stands for. The risk in this classifier is a
history that lands in `healthy` while no eval has passed: that is the silence
this notifier exists to end, and it fails nothing on its own.

The histories are in the shape the workflow passes: the runs endpoint's
`workflow_runs` (`GET /repos/{owner}/{repo}/actions/workflows/evals.yml/runs?branch=main`),
each entry its run's latest attempt, plus the earlier attempts of every re-run
(`GET /repos/{owner}/{repo}/actions/runs/{id}/attempts/{n}`), cut to the fields
the script reads. The incident rows are measured: they are the history the
runs endpoint returned at 2026-10-17T04:48Z, after the stuck run was cancelled
and before the next run finished, and the run that then finished. No evals run
had been re-run by then. The re-run rows rest on CI run 38659188445 instead,
measured the same day: its list entry carries attempt 2 (`success`, updated
05:38:03), and the attempts endpoint answers attempt 1 as `failure`, updated
00:39:06, with the same `run_number`.
"""

import datetime
import json
import pathlib
import subprocess
import sys

SCRIPT = pathlib.Path(__file__).with_name("evals-health.sh")

STALE_HOURS = 48


def epoch(stamp):
    return int(datetime.datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ")
               .replace(tzinfo=datetime.timezone.utc).timestamp())


def stamp(seconds):
    return (datetime.datetime.fromtimestamp(seconds, datetime.timezone.utc)
            .strftime("%Y-%m-%dT%H:%M:%SZ"))


NOW = epoch("2026-10-17T12:00:00Z")
HOUR = 3600


def at(run_id, number, conclusion, updated_at, attempt=1):
    return {"id": run_id, "run_number": number, "run_attempt": attempt,
            "conclusion": conclusion, "updated_at": updated_at}


def run(number, conclusion, hours_ago, attempt=1):
    """Attempt `attempt` of run `number`, last updated `hours_ago` before NOW.
    The run's id is its number. `conclusion` None is still in progress."""
    return at(number, number, conclusion, stamp(NOW - hours_ago * HOUR), attempt)


# The history the runs endpoint returned at 2026-10-17T04:48Z, newest first.
# Run 37611523380 sat in `waiting` from 2026-10-07 and held the `evals`
# concurrency group, so each later scheduled run queued behind it and was
# cancelled by the next one. It was cancelled by hand on 2026-10-17, which is
# why its own `updated_at` is the newest of all.
INCIDENT = [
    at(38710656616, 88, "cancelled", "2026-10-16T09:01:02Z"),
    at(38542763502, 87, "cancelled", "2026-10-15T09:07:47Z"),
    at(38392524911, 86, "cancelled", "2026-10-14T09:29:50Z"),
    at(38245954938, 85, "cancelled", "2026-10-13T11:46:21Z"),
    at(38133269097, 84, "cancelled", "2026-10-12T11:57:15Z"),
    at(38045448271, 83, "cancelled", "2026-10-11T10:48:31Z"),
    at(37922950786, 82, "cancelled", "2026-10-10T10:35:46Z"),
    at(37769461459, 81, "cancelled", "2026-10-09T11:18:53Z"),
    at(37611523380, 80, "cancelled", "2026-10-17T04:48:05Z"),
    at(37455198881, 79, "success", "2026-10-06T11:37:43Z"),
    at(37303016408, 78, "failure", "2026-10-05T11:45:52Z"),
]
# The run queued behind the stuck one, which started once it was cancelled.
RELEASED = at(38877555478, 89, "failure", "2026-10-17T05:01:47Z")

# (name, history, now, expected line, why)
CASES = [
    ("fresh pass", [run(3, "success", 6)], NOW,
     f"healthy 3 1 success {stamp(NOW - 6 * HOUR)}",
     "the ordinary day"),
    ("fresh failure", [run(3, "failure", 6)], NOW,
     f"failing 3 1 failure {stamp(NOW - 6 * HOUR)}",
     "the suite ran and went red"),
    ("timed out", [run(3, "timed_out", 6)], NOW,
     f"failing 3 1 timed_out {stamp(NOW - 6 * HOUR)}",
     "a verdict that is not a pass"),
    ("startup failure", [run(3, "startup_failure", 6)], NOW,
     f"failing 3 1 startup_failure {stamp(NOW - 6 * HOUR)}",
     "evals.yml itself is invalid"),
    # A cancelled run carries no verdict: a pending run evicted by a newer one,
    # or a manual cancel. It must neither pass nor fail the suite.
    ("cancelled ahead of a pass", [run(4, "cancelled", 1), run(3, "success", 20)], NOW,
     f"healthy 3 1 success {stamp(NOW - 20 * HOUR)}",
     "a cancel is not a failure"),
    ("cancelled ahead of a failure", [run(4, "cancelled", 1), run(3, "failure", 20)], NOW,
     f"failing 3 1 failure {stamp(NOW - 20 * HOUR)}",
     "a cancel does not clear a failure either"),
    ("in flight ahead of a pass", [run(4, None, 0), run(3, "success", 20)], NOW,
     f"healthy 3 1 success {stamp(NOW - 20 * HOUR)}",
     "tonight's run has not finished, so last night's verdict stands"),
    ("no verdict conclusions", [run(9, c, 1) for c in
                                ("cancelled", "skipped", "neutral", "action_required", "stale", None)],
     NOW, "stale - - - -",
     "none of these says the suite passed"),
    ("no runs", [], NOW, "stale - - - -",
     "an empty history is not a healthy one"),
    # The incident: the newest verdict is eleven days old, so the history
    # must read stale however many cancelled runs sit on top of it.
    ("the 2026-10-07 stall", INCIDENT, epoch("2026-10-17T04:48:05Z"),
     "stale 37455198881 1 success 2026-10-06T11:37:43Z",
     "a stuck run held the group for ten days and every later run was cancelled"),
    ("the run the cancel released", [RELEASED] + INCIDENT, epoch("2026-10-17T05:06:15Z"),
     "failing 38877555478 1 failure 2026-10-17T05:01:47Z",
     "it ran, and failed"),
    # The history as it stood then: the stuck run and the one queued behind
    # it had not completed, so only the last two verdicts were listed.
    ("the stall, the day it turned stale",
     INCIDENT[-2:], epoch("2026-10-08T11:37:44Z"),
     "stale 37455198881 1 success 2026-10-06T11:37:43Z",
     "one second past 48 hours after the last pass"),
    ("the stall, the second before",
     INCIDENT[-2:], epoch("2026-10-08T11:37:43Z"),
     "healthy 37455198881 1 success 2026-10-06T11:37:43Z",
     "exactly 48 hours old still counts"),
    # Staleness is checked before the verdict: an old failure says the suite
    # has stopped as well as that it broke, and stopping is the newer news.
    ("old failure", [run(3, "failure", STALE_HOURS + 1)], NOW,
     f"stale 3 1 failure {stamp(NOW - (STALE_HOURS + 1) * HOUR)}",
     "nothing has run since the failure"),
    # The newest verdict is the highest run number, whatever finished last: a
    # re-run of an older run tests older code. The older run is the re-run
    # here, so ranking by attempt or by `updated_at` fails these.
    ("an older run re-run passes after a newer failure",
     [run(5, "failure", 10), run(4, "success", 2, attempt=2), run(4, "failure", 30)], NOW,
     f"failing 5 1 failure {stamp(NOW - 10 * HOUR)}",
     "the newest code is red"),
    ("an older run re-run fails after a newer pass",
     [run(5, "success", 10), run(4, "failure", 1, attempt=2), run(4, "success", 30)], NOW,
     f"healthy 5 1 success {stamp(NOW - 10 * HOUR)}",
     "the newest code is green"),
    ("listed oldest first", [run(4, "success", 20), run(5, "failure", 3)], NOW,
     f"failing 5 1 failure {stamp(NOW - 3 * HOUR)}",
     "the rank is the run number, not the endpoint's order"),
    # Within a run, the latest attempt with a verdict is the run's verdict.
    ("the newest run re-run passes",
     [run(5, "success", 1, attempt=2), run(5, "failure", 20), run(4, "failure", 40)], NOW,
     f"healthy 5 2 success {stamp(NOW - 1 * HOUR)}",
     "its latest attempt is its verdict"),
    # A re-run in flight, or cancelled, has no verdict of its own: the
    # earlier attempt's still stands, failure and timestamp alike.
    ("the newest run is being re-run after a failure",
     [run(5, None, 0, attempt=2), run(5, "failure", 20), run(4, "success", 40)], NOW,
     f"failing 5 1 failure {stamp(NOW - 20 * HOUR)}",
     "an older pass must not close a failure nobody fixed"),
    ("the newest run's re-run was cancelled",
     [run(5, "cancelled", 1, attempt=2), run(5, "failure", 20), run(4, "success", 40)], NOW,
     f"failing 5 1 failure {stamp(NOW - 20 * HOUR)}",
     "the same, for good"),
    ("a re-run does not reset the stale clock",
     [run(5, "cancelled", 1, attempt=2), run(5, "success", STALE_HOURS + 24)], NOW,
     f"stale 5 1 success {stamp(NOW - (STALE_HOURS + 24) * HOUR)}",
     "starting or cancelling a re-run is not a verdict"),
]

# (name, stale-hours, history, expected line): the limit is the argument, not
# a constant that happens to equal the workflow's.
THRESHOLDS = [
    ("24 hours, a 30-hour-old pass", "24", [run(3, "success", 30)],
     f"stale 3 1 success {stamp(NOW - 30 * HOUR)}"),
    ("24 hours, a 20-hour-old pass", "24", [run(3, "success", 20)],
     f"healthy 3 1 success {stamp(NOW - 20 * HOUR)}"),
    # Decimal, not octal: bash arithmetic reads `048` as a bad octal number.
    ("a leading zero", "048", [run(3, "success", 47)],
     f"healthy 3 1 success {stamp(NOW - 47 * HOUR)}"),
    # Past where bash's 64-bit arithmetic would wrap negative.
    ("a huge limit", "2562047788015216", [run(3, "success", 1)],
     f"healthy 3 1 success {stamp(NOW - 1 * HOUR)}"),
]


def call(argv, stdin):
    return subprocess.run(
        ["bash", str(SCRIPT), *argv],
        input=stdin, capture_output=True, text=True, check=False,
    )


def main():
    failures = []
    for name, history, now, want, why in CASES:
        got = call([str(now), str(STALE_HOURS)], json.dumps(history))
        actual = got.stdout.strip()
        if actual != want or got.returncode != 0:
            failures.append(
                f"  {name}: {actual or '(nothing)'!r} (exit {got.returncode},"
                f" stderr {got.stderr.strip()!r}), want {want!r} — {why}"
            )

    for name, hours, history, want in THRESHOLDS:
        got = call([str(NOW), hours], json.dumps(history))
        if got.stdout.strip() != want or got.returncode != 0:
            failures.append(f"  {name}: {got.stdout.strip()!r} (exit {got.returncode},"
                            f" stderr {got.stderr.strip()!r}), want {want!r}")

    # A classifier that accepts anything is a classifier that hides a caller bug.
    # The usage line is checked as well as the status, because jq exits 2 on
    # an argument it cannot parse too: the status alone cannot tell the
    # script's own refusal from jq's.
    for argv in ([], [str(NOW)], [str(NOW), "48", "x"], ["soon", "48"],
                 [str(NOW), "two"], [str(NOW), "0"], [str(NOW), ""]):
        got = call(argv, "[]")
        if got.returncode != 2 or "usage:" not in got.stderr:
            failures.append(f"  {argv!r} -> exit {got.returncode}, stderr"
                            f" {got.stderr.strip()!r}, want 2 and the usage line")

    # Input that is not one array must fail, never classify: read as an empty
    # history, most of these would report `stale` forever over a suite that is
    # passing. The first two are what a caller passes when it drops its `--jq`
    # or misspells the key; the object of runs is one that reshaped its `--jq`
    # into an object, which jq's `map` would iterate all the same.
    one = run(3, "success", 1)
    for name, stdin in (("the whole response", json.dumps({"total_count": 1, "workflow_runs": [one]})),
                        ("null", "null"),
                        ("an empty object", "{}"),
                        ("an object of runs", json.dumps({"a": one})),
                        ("a second value", "[] []"),
                        ("not JSON", "<html>502</html>"),
                        ("nothing", "")):
        got = call([str(NOW), str(STALE_HOURS)], stdin)
        if got.returncode == 0 or got.stdout.strip():
            failures.append(
                f"  {name}: exit {got.returncode}, stdout {got.stdout.strip()!r};"
                " want a non-zero exit and no state"
            )

    if failures:
        print(f"evals-health.sh: {len(failures)} case(s) wrong:", file=sys.stderr)
        print("\n".join(failures), file=sys.stderr)
        return 1
    print(f"ok: evals-health.sh classifies all {len(CASES)} histories and"
          f" {len(THRESHOLDS)} limits, refuses bad arguments, and fails on input"
          " that is not one array")
    return 0


if __name__ == "__main__":
    sys.exit(main())
