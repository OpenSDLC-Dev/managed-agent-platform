#!/usr/bin/env python3
"""Keep one type, priority and difficulty on each open GitHub issue."""
import json
import os
import re
import subprocess


# Each group starts with the provisional choice for issues filed without a form.
GROUPS = (("feature", "bug"), ("p1", "p0", "p2"), ("hard", "medium", "easy"))
# GitHub renders the three dropdowns before the free-text Details field.
FORM = re.compile(r"\A### Type\n\n([^\n]*)\n\n### Priority\n\n([^\n]*)\n\n"
                  r"### Difficulty\n\n([^\n]*)\n\n### Details(?:\n|$)")


def api(endpoint, *args, payload=None):
    command = ["gh", "api", endpoint, *args]
    if payload is not None:
        command.extend(["--input", "-"])
    result = subprocess.run(command, input=json.dumps(payload) if payload is not None else None,
                            stdout=subprocess.PIPE, check=True, encoding="utf-8")
    return json.loads(result.stdout)


def compliant(labels):
    return len(labels) == len(GROUPS) and all(labels.intersection(group) for group in GROUPS)


def recent_assignments(path):
    pages = api(path + "/events?per_page=100", "--paginate", "--slurp")
    events = (e for page in pages for e in page if e["event"] == "labeled")
    rank = {}
    for index, event in enumerate(sorted(events, key=lambda e: (e["created_at"], e["id"])), start=1):
        # A provisional label may land while a maintainer adds the real one.
        actor = (event["actor"] or {}).get("login")
        rank[event["label"]["name"]] = (actor != "github-actions[bot]", index)
    return rank


def reconcile(repository, number):
    path = f"repos/{repository}/issues/{number}"
    issue = api(path)
    if (issue["state"] != "open" or "pull_request" in issue or
            not issue["repository_url"].lower().endswith("/repos/" + repository.lower())):
        return
    current = {label["name"]: label["node_id"] for label in issue["labels"]}
    names = set(current)
    if compliant(names):
        return

    matches = [names.intersection(group) for group in GROUPS]
    # A later webhook can run first, or replace one queued for this issue.
    # Resolve conflicting live labels from their assignment history.
    rank = recent_assignments(path) if any(len(names) > 1 for names in matches) else {}
    form = FORM.match((issue["body"] or "").replace("\r\n", "\n"))
    answers = form.groups() if form else (None,) * len(GROUPS)
    desired = set()
    for group, existing, answer in zip(GROUPS, matches, answers):
        if existing:
            # Keep a deterministic choice for imported labels without events.
            chosen = max((name for name in group if name in existing),
                         key=lambda name: rank.get(name, (False, 0)))
        else:
            chosen = answer if answer in group else group[0]
        desired.add(chosen)

    # Apply only the observed delta. Replacing the entire set would erase a
    # label added after the read. Node IDs also handle arbitrary old label names.
    missing = sorted(desired - names)
    if missing:
        api(path + "/labels", payload={"labels": missing})
    extra = sorted(names - desired)
    if extra:
        api("graphql", payload={
            "query": """mutation($input: RemoveLabelsFromLabelableInput!) {
                removeLabelsFromLabelable(input: $input) { clientMutationId }
            }""",
            "variables": {"input": {"labelableId": issue["node_id"],
                                    "labelIds": [current[name] for name in extra]}},
        })


def sweep(repository):
    # Issues created by another workflow's GITHUB_TOKEN can miss the issues event.
    pages = api(f"repos/{repository}/issues?state=open&per_page=100", "--paginate", "--slurp")
    for page in pages:
        for issue in page:
            if "pull_request" not in issue and not compliant({label["name"] for label in issue["labels"]}):
                reconcile(repository, issue["number"])


if __name__ == "__main__":
    repo = os.environ["GITHUB_REPOSITORY"]
    if number := os.environ.get("ISSUE_NUMBER"):
        reconcile(repo, int(number))
    else:
        sweep(repo)
