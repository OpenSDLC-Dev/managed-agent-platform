#!/usr/bin/env python3
"""Exercise the lookup used for notifier updates and recovery closes, with two API pages."""

import json
import pathlib
import subprocess

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
            issue(7, title, None, labels=[{"name": "bug"}]),
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
    print("ok: all four notifiers find only their own current and historical issues across pages")


if __name__ == "__main__":
    main()
