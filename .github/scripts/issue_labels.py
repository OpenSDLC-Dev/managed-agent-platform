#!/usr/bin/env python3
"""Set the native issue type and keep one priority and one difficulty label."""
import json
import os
import re
import subprocess
from datetime import datetime, timezone


ROLLOUT = datetime(2026, 11, 21, 4, 40, tzinfo=timezone.utc)
# Each group starts with the provisional choice for issues filed without a form.
GROUPS = (("p1", "p0", "p2"), ("hard", "medium", "easy"))
FORM_TYPES = {"Bug details": "Bug", "Feature request": "Feature", "Task details": "Task"}
# The two dropdowns precede each template's distinct free-text field.
FORM = re.compile(r"\A### Priority\n\n([^\n]*)\n\n### Difficulty\n\n([^\n]*)\n\n"
                  r"### (Bug details|Feature request|Task details)(?:\n|$)")


def api(endpoint, *args, payload=None):
    command = ["gh", "api", endpoint, *args]
    if payload is not None:
        command.extend(["--input", "-"])
    result = subprocess.run(command, input=json.dumps(payload) if payload is not None else None,
                            stdout=subprocess.PIPE, check=True, encoding="utf-8")
    return json.loads(result.stdout)


def labels_compliant(labels):
    return len(labels) == len(GROUPS) and all(labels.intersection(group) for group in GROUPS)


def filed_since_rollout(created_at):
    return datetime.fromisoformat(created_at.replace("Z", "+00:00")) >= ROLLOUT


def fill_type(path, issue, labels, form):
    previous = (issue["type"] or {}).get("name")
    if previous in FORM_TYPES.values():
        return
    legacy = labels & {"bug", "feature"}
    target = "Feature"
    if len(legacy) == 1:
        target = next(iter(legacy)).title()
    elif form:
        target = FORM_TYPES[form[3]]

    while True:
        saved = api(path, "--method", "PATCH", payload={"type": target})
        if (saved["type"] or {}).get("name") != target:
            raise RuntimeError(f"GitHub did not apply issue type {target} to {path}")
        # A maintainer can set a type between the read and PATCH. The change
        # event records the previous type even if our write overwrote it.
        receipt = api("graphql", payload={
            "query": """query($id: ID!) { node(id: $id) { ... on Issue {
                issueType { name }
                timelineItems(last: 1, itemTypes: [ISSUE_TYPE_ADDED_EVENT, ISSUE_TYPE_CHANGED_EVENT,
                                                   ISSUE_TYPE_REMOVED_EVENT]) { nodes {
                    ... on IssueTypeChangedEvent { actor { __typename login } prevIssueType { name } }
                } }
            } } }""",
            "variables": {"id": issue["node_id"]},
        })["data"]["node"]
        events = receipt["timelineItems"]["nodes"]
        change = events[-1] if events else {}
        displaced = (change.get("prevIssueType") or {}).get("name")
        if ((receipt["issueType"] or {}).get("name") != target
                or change.get("actor") != {"__typename": "Bot", "login": "github-actions"}
                or displaced not in FORM_TYPES.values() or displaced == previous):
            return
        previous, target = target, displaced


def label_history(path):
    pages = api(path + "/events?per_page=100", "--paginate", "--slurp")
    events = (e for page in pages for e in page if e["event"] in ("labeled", "unlabeled"))
    rank, restore = {}, {}
    active = set()
    for index, event in enumerate(sorted(events, key=lambda e: (e["created_at"], e["id"])), start=1):
        name = event["label"]["name"]
        group = next((group for group in GROUPS if name in group), None)
        if group is None:
            continue
        bot = (event["actor"] or {}).get("login") == "github-actions[bot]"
        restore.pop(group, None)
        if event["event"] == "labeled":
            active.add(name)
            # A provisional label may land while a maintainer adds the real one.
            rank[name] = (not bot, index)
        else:
            active.discard(name)
            # Conflict cleanup may race with a human removing its intended winner.
            # Restore only the bot removal that actually emptied this group.
            if bot and active.isdisjoint(group):
                restore[group] = name
    return rank, restore


def reconcile(repository, number, *, confirm_cleanup=True):
    path = f"repos/{repository}/issues/{number}"
    issue = api(path)
    if ("pull_request" in issue or
            (issue["state"] == "closed" and not filed_since_rollout(issue["created_at"])) or
            not issue["repository_url"].lower().endswith("/repos/" + repository.lower())):
        return
    current = {label["name"]: label["node_id"] for label in issue["labels"]}
    names = set(current)
    form = FORM.match((issue["body"] or "").replace("\r\n", "\n"))
    fill_type(path, issue, names, form)
    if labels_compliant(names):
        return

    matches = [names.intersection(group) for group in GROUPS]
    # A later webhook can run first, or replace one queued for this issue.
    # Resolve conflicting live labels from their assignment history.
    rank, restore = label_history(path) if any(len(group) != 1 for group in matches) else ({}, {})
    answers = form.groups()[:2] if form else (None,) * len(GROUPS)
    desired = set()
    for group, existing, answer in zip(GROUPS, matches, answers):
        if existing:
            # Keep a deterministic choice for imported labels without events.
            chosen = max((name for name in group if name in existing),
                         key=lambda name: rank.get(name, (False, 0)))
        else:
            chosen = restore.get(group, answer if answer in group else group[0])
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
        if confirm_cleanup and any(len(group) > 1 for group in matches):
            # GITHUB_TOKEN removals do not schedule a follow-up issues run.
            reconcile(repository, number, confirm_cleanup=False)


def sweep(repository):
    # repository.issues excludes pull requests. Three names suffice to detect
    # every bad set, even if an issue carries many more noncanonical labels.
    query = """query($owner: String!, $name: String!, $state: IssueState!, $cursor: String) {
        repository(owner: $owner, name: $name) {
            issues(states: [$state], first: 100, after: $cursor,
                   orderBy: {field: CREATED_AT, direction: DESC}) {
                nodes { number createdAt issueType { name } labels(first: 3) { nodes { name } } }
                pageInfo { hasNextPage endCursor }
            }
        }
    }"""
    owner, name = repository.split("/")
    for state in ("OPEN", "CLOSED"):
        cursor = None
        while True:
            result = api("graphql", payload={
                "query": query, "variables": {"owner": owner, "name": name, "state": state, "cursor": cursor},
            })
            page = result["data"]["repository"]["issues"]
            for issue in page["nodes"]:
                if state == "CLOSED" and not filed_since_rollout(issue["createdAt"]):
                    return
                if ((issue["issueType"] or {}).get("name") not in FORM_TYPES.values()
                        or not labels_compliant({label["name"] for label in issue["labels"]["nodes"]})):
                    reconcile(repository, issue["number"])
            if not page["pageInfo"]["hasNextPage"]:
                break
            cursor = page["pageInfo"]["endCursor"]


if __name__ == "__main__":
    repo = os.environ["GITHUB_REPOSITORY"]
    if number := os.environ.get("ISSUE_NUMBER"):
        reconcile(repo, int(number))
    else:
        sweep(repo)
