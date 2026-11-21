#!/usr/bin/env python3
"""Run the notifier open branches and the paginated lookup used for recovery closes."""

import datetime
import json
import os
import pathlib
import subprocess
import tempfile

from retry_test import block_scalars

ROOT = pathlib.Path(__file__).resolve().parents[2]
BOT = "github-actions[bot]"
CASES = {
    "deploy-failure": (
        "CD: the deploy workflow is failing on main",
        "`deploy` finished **failure** on `abc1234`.\n\n<!-- deploy-alert run:12 attempt:2 -->",
    ),
    "deploy-notifier": (
        "CD: the deploy notifier can no longer tell deployed from parked",
        "`deploy` succeeded on `abc1234`, but this workflow could not find a step named **Deploy the chart**."
        "\n\n<!-- deploy-alert run:16 attempt:1 -->",
    ),
    "evals": (
        "evals: the nightly eval suite is not passing on main",
        "None of the newest 100 runs reached a verdict.\n\n<!-- evals-alert stale since run:- attempt:- -->",
    ),
    "staging-parked": (
        "staging is parked and nothing is deploying to it",
        "The staging cluster `example` carries `power-saved-*` labels.\n\n"
        "It closes automatically: the next run of this workflow that finds the cluster unparked, "
        "or gone, closes it, and a completed `deploy` is one of its triggers.",
    ),
}

# The fake gh requires the lookup to request every page of open bot issues.
DRIVER = """
set -euo pipefail
source .github/scripts/notifier-issues.sh
REPO=example/platform
gh() {
  if [[ "$#" != 3 || "$1" != api || "$2" != --paginate ||
        "$3" != "repos/$REPO/issues?state=open&creator=github-actions%5Bbot%5D&per_page=100" ]]; then
    echo "unexpected gh call: $*" >&2
    return 1
  fi
  cat
}
[[ "$(notifier_marker "$1")" == "$3" ]]
open_notifier_issues "$1" "$2"
"""


# Exercise the reconcilers' actual open branches without contacting GitHub or GCP.
# Other reads return the shape gh prints after applying the workflow's --jq.
CREATE_FIXTURE = r"""
sleep() { :; }
gcloud() { echo 'power-saved-nodes=yes'; }
gh() {
  case "$1 $2" in
    "api --method")
      printf '%s\0' "$@" >> "$REQUEST"
      printf '%s\n' "$CREATED"
      ;;
    "api --paginate") echo '[]' ;;
    "api "*/jobs) printf '%s\n' "$JOBS" ;;
    "api "*/workflows/*) printf '%s\n' "$RUNS" ;;
    "issue edit") [[ "$3" == 42 ]] ;;
    *) echo "unexpected gh call: $*" >&2; return 1 ;;
  esac
}
"""


def check_creation(kind, title, tmp):
    workflow = {"deploy-failure": "deploy-alert", "deploy-notifier": "deploy-alert",
                "evals": "evals-alert", "staging-parked": "staging-parked"}[kind]
    path = ROOT / ".github" / "workflows" / (workflow + ".yml")
    lines = path.read_text(encoding="utf-8").splitlines()
    (reconcile,) = [body for first, body in block_scalars(lines)
                    if next(line.strip() for line in reversed(lines[:first - 1])
                            if line.strip().startswith("- name:")).startswith("- name: Reconcile")]
    status = "success" if kind == "deploy-notifier" else "failure"
    now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    run = {"id": 21, "attempt": 1, "conclusion": status, "sha": "abc1234", "actor": "actor",
           "run_number": 2, "run_attempt": 1, "updated_at": now}
    steps = [] if kind == "deploy-notifier" else [{"name": "Deploy the chart", "conclusion": "failure"}]
    issue_type, labels = ("Task", ["p2", "hard"]) if kind == "staging-parked" else ("Bug", ["p1", "medium"])
    url = "https://example.invalid/issues/42"
    created = {"html_url": url, "type": {"name": issue_type},
               "labels": [{"name": label} for label in reversed(labels)]}
    request = tmp / "request"
    env = {**os.environ, "GH_TOKEN": "local-fixture", "REPO": "example/platform",
           "TITLE": CASES["deploy-failure"][0] if workflow == "deploy-alert" else title,
           "MISWIRED_TITLE": CASES["deploy-notifier"][0], "WOKE_BY": "1", "WINDOW": "12",
           "STEP_NAME": "Deploy the chart", "DEPLOY_WORKFLOW": "deploy.yml",
           "EVALS_WORKFLOW": "evals.yml", "STALE_HOURS": "48",
           "GITHUB_SERVER_URL": "https://example.invalid", "GITHUB_EVENT_NAME": "schedule",
           "GITHUB_RUN_ID": "4", "OPEN_ISSUE": "", "GCP_PROJECT_ID": "example",
           "GCP_ZONE": "example", "GKE_CLUSTER": "example", "RUNNER_TEMP": str(tmp),
           "RUNS": json.dumps([run]), "JOBS": json.dumps({"jobs": [{"steps": steps}]}),
           "REQUEST": str(request), "CREATED": json.dumps(created)}
    command = ["bash", "-c", CREATE_FIXTURE + "\n" + "\n".join(reconcile)]
    request.write_bytes(b"")
    result = subprocess.run(command, env=env, cwd=ROOT, text=True, capture_output=True, check=True)
    assert f"opened {url}" in result.stdout, result.stdout
    args = request.read_bytes().decode().split("\0")[:-1]
    assert args[:4] == ["api", "--method", "POST", "repos/example/platform/issues"], args
    assert args[4::2] == ["-f"] * 5, args
    fields = args[5::2]
    assert fields[0] == "title=" + title and fields[1].startswith(f"body=<!-- platform-notifier:{kind} -->\n\n"), fields
    assert fields[2:] == ["type=" + issue_type, *["labels[]=" + label for label in labels]], fields

    # GitHub may create the issue while silently dropping type or labels.
    if kind == "evals":
        for missing in ({"type": None}, {"labels": []}):
            request.write_bytes(b"")
            result = subprocess.run(command, env={**env, "CREATED": json.dumps({**created, **missing})},
                                    cwd=ROOT, text=True, capture_output=True)
            assert result.returncode and url in result.stderr, result
            assert request.read_bytes().count(b"--method\0POST\0") == 1, "retried a successful create"


def issue(number, title, body, author=BOT, **extra):
    return {"number": number, "user": {"login": author}, "title": title, "body": body, **extra}


def main():
    for kind, (title, original) in CASES.items():
        marker = f"<!-- platform-notifier:{kind} -->"
        # A new issue can be renamed and relabelled. Historical issues still
        # close after losing their old label. Every source must stay distinct.
        first = [
            issue(12, "renamed", marker + "\r\nReport", labels=[{"name": "p0"}]),
            issue(11, title, marker + "\nReport", author="reporter"),
            issue(10, title, original, author="reporter"),
            issue(9, title, marker + "\nReport", pull_request={"url": "example"}),
            issue(8, title, "A related report quoted this marker:\n" + marker),
            issue(7, title, None, type={"name": "Bug"}, labels=[{"name": "p1"}, {"name": "medium"}]),
        ]
        second = [
            issue(6, title, original),
            issue(5, "renamed duplicate", marker + "\nReport"),
        ]
        for other, (other_title, other_body) in CASES.items():
            if other != kind:
                first.append(issue(4, other_title, other_body))
                first.append(issue(3, title, f"<!-- platform-notifier:{other} -->\n" + original))
        result = subprocess.run(
            ["bash", "-c", DRIVER, "bash", kind, title, marker],
            input=json.dumps(first) + "\n" + json.dumps(second),
            text=True, capture_output=True, cwd=ROOT, check=True,
        )
        if result.stdout.splitlines() != ["12", "6", "5"]:
            raise AssertionError(f"{kind} matched the wrong issues: {result.stdout!r}")
    with tempfile.TemporaryDirectory() as tmp:
        for kind, (title, _) in CASES.items():
            check_creation(kind, title, pathlib.Path(tmp))
    print("ok: all four notifiers create typed issues with two labels and find their own reports across pages")


if __name__ == "__main__":
    main()
