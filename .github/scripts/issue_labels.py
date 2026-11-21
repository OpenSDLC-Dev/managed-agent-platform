#!/usr/bin/env python3
"""Keep the native issue type and one label each for priority and difficulty."""
import json
import os
import re
import subprocess
from datetime import datetime


ROLLOUT = "2026-11-21T04:40:00Z"
GROUPS = (("p1", "p0", "p2"), ("hard", "medium", "easy"))
FORM_TYPES = {"Bug details": "Bug", "Feature request": "Feature", "Task details": "Task"}
FORM = re.compile(r"\A### Priority\n\n([^\n]*)\n\n### Difficulty\n\n([^\n]*)\n\n"
                  r"### (Bug details|Feature request|Task details)(?:\n|$)")
BOT = {"__typename": "Bot", "login": "github-actions"}

# GitHub returns the timeline in order. Use the same snapshot for a webhook
# and a sweep, including on retries where the current fields already look valid.
HISTORY = """
    timelineItems(first: 100, after: $historyCursor, since: $rollout, itemTypes: [
        LABELED_EVENT, UNLABELED_EVENT, ISSUE_TYPE_ADDED_EVENT, ISSUE_TYPE_CHANGED_EVENT, ISSUE_TYPE_REMOVED_EVENT
    ]) {
        pageInfo { hasNextPage endCursor }
        nodes {
            __typename
            ... on LabeledEvent { actor { __typename login } label { name } }
            ... on UnlabeledEvent { actor { __typename login } label { name } }
            ... on IssueTypeAddedEvent { actor { __typename login } issueType { name } }
            ... on IssueTypeChangedEvent { actor { __typename login } issueType { name } prevIssueType { name } }
            ... on IssueTypeRemovedEvent { actor { __typename login } issueType { name } }
        }
    }
"""
SNAPSHOT = """fragment Triage on Issue {
    id number state createdAt body repository { nameWithOwner } issueType { name }
    labels(first: 100) { nodes { id name } pageInfo { hasNextPage } }
""" + HISTORY + "}"


def api(endpoint, *args, payload=None):
    command = ["gh", "api", endpoint, *args]
    if payload is not None:
        command.extend(["--input", "-"])
    result = subprocess.run(command, input=json.dumps(payload) if payload is not None else None,
                            stdout=subprocess.PIPE, check=True, encoding="utf-8")
    return json.loads(result.stdout)


def read_query(query, **variables):
    return api("graphql", payload={"query": query, "variables": {**variables, "rollout": ROLLOUT}})["data"]


def filed_since_rollout(timestamp):
    return datetime.fromisoformat(timestamp.replace("Z", "+00:00")) >= datetime.fromisoformat(
        ROLLOUT.replace("Z", "+00:00"))


def issue_history(issue):
    page = issue["timelineItems"]
    events = list(page["nodes"])
    while page["pageInfo"]["hasNextPage"]:
        query = """query IssueTriageHistory($id: ID!, $historyCursor: String, $rollout: DateTime!) {
            node(id: $id) { ... on Issue { """ + HISTORY + "} } }"
        page = read_query(query, id=issue["id"], historyCursor=page["pageInfo"]["endCursor"])["node"]["timelineItems"]
        events.extend(page["nodes"])
    return events


def desired_type(current, labels, form, events):
    changes = [event for event in events if "issueType" in event]
    chosen = current
    if changes:
        first = changes[0]
        # Recover the type before rollout from the first recorded transition.
        chosen = (first.get("prevIssueType") or {}).get("name")
        if first["__typename"] == "IssueTypeRemovedEvent":
            chosen = (first["issueType"] or {}).get("name")
    for event in changes:
        if event["actor"] == BOT and chosen in FORM_TYPES.values():
            continue  # A bot can fill an empty type. Only a maintainer can replace it.
        chosen = None if event["__typename"] == "IssueTypeRemovedEvent" else (event["issueType"] or {}).get("name")
    if chosen in FORM_TYPES.values():
        return chosen
    legacy = labels & {"bug", "feature"}
    if len(legacy) == 1:
        return next(iter(legacy)).title()
    return FORM_TYPES[form[3]] if form else "Feature"


def desired_label(group, current, answer, events):
    # Undo the recorded changes to seed imported or pre-rollout labels.
    changes = [event for event in events if (event.get("label") or {}).get("name") in group]
    rank = {name: (True, 0) for name in current.intersection(group)}
    for event in reversed(changes):
        name = event["label"]["name"]
        if event["__typename"] == "LabeledEvent":
            rank.pop(name, None)
        else:
            rank[name] = (True, 0)

    def winner():
        return max((name for name in group if name in rank), key=rank.get, default=None)

    for index, event in enumerate(changes, 1):
        name = event["label"]["name"]
        bot = event["actor"] == BOT
        if event["__typename"] == "LabeledEvent":
            if not bot or name not in rank:
                rank[name] = (not bot, index)
        elif not bot or name != winner():
            # Cleanup can remove a loser. A stale deletion of the current
            # winner cannot cancel its newer human assignment.
            rank.pop(name, None)
    return winner() or (answer if answer in group else group[0])


def reconcile_snapshot(repository, issue, *, write):
    if (not issue or issue["repository"]["nameWithOwner"].lower() != repository.lower()
            or (issue["state"] == "CLOSED" and not filed_since_rollout(issue["createdAt"]))):
        return False
    path = f"repos/{repository}/issues/{issue['number']}"
    if issue["labels"]["pageInfo"]["hasNextPage"]:
        pages = api(path + "/labels?per_page=100", "--paginate", "--slurp")
        current = {label["name"]: label["node_id"] for page in pages for label in page}
    else:
        current = {label["name"]: label["id"] for label in issue["labels"]["nodes"]}
    names = set(current)
    events = issue_history(issue)
    form = FORM.match((issue["body"] or "").replace("\r\n", "\n"))
    native = (issue["issueType"] or {}).get("name")
    kind = desired_type(native, names, form, events)
    answers = form.groups()[:2] if form else (None, None)
    desired = {desired_label(group, names, answer, events) for group, answer in zip(GROUPS, answers)}
    if kind == native and desired == names:
        return False
    if write:
        if kind != native:
            saved = api(path, "--method", "PATCH", payload={"type": kind})
            if (saved["type"] or {}).get("name") != kind:
                raise RuntimeError(f"GitHub did not apply issue type {kind}")
        missing = sorted(desired - names)
        if missing:
            api(path + "/labels", payload={"labels": missing})
        extra = sorted(names - desired)
        if extra:
            api("graphql", payload={
                "query": """mutation($input: RemoveLabelsFromLabelableInput!) {
                    removeLabelsFromLabelable(input: $input) { clientMutationId }
                }""",
                "variables": {"input": {"labelableId": issue["id"], "labelIds": [current[name] for name in extra]}},
            })
    return True


def reconcile(repository, number):
    owner, name = repository.split("/")
    query = """query IssueTriage($owner: String!, $name: String!, $number: Int!,
                                 $rollout: DateTime!, $historyCursor: String) {
        repository(owner: $owner, name: $name) { issueOrPullRequest(number: $number) { ...Triage } }
    }""" + SNAPSHOT
    # A GITHUB_TOKEN write does not trigger a new issues run. A second read
    # settles a concurrent edit. Future runs can replay even a lost write reply.
    for _ in range(2):
        issue = read_query(query, owner=owner, name=name, number=number)["repository"]["issueOrPullRequest"]
        if not reconcile_snapshot(repository, issue, write=True):
            break


def eligible_issues(repository):
    owner, name = repository.split("/")
    query = """query IssueTriageSweep($owner: String!, $name: String!, $state: IssueState!, $cursor: String,
                                     $closedSince: DateTime, $rollout: DateTime!, $historyCursor: String) {
        repository(owner: $owner, name: $name) {
            issues(states: [$state], filterBy: {since: $closedSince}, first: 25, after: $cursor,
                   orderBy: {field: CREATED_AT, direction: DESC}) {
                nodes { ...Triage } pageInfo { hasNextPage endCursor }
            }
        }
    }""" + SNAPSHOT
    for state in ("OPEN", "CLOSED"):
        cursor = None
        while True:
            page = read_query(query, owner=owner, name=name, state=state, cursor=cursor,
                              closedSince=ROLLOUT if state == "CLOSED" else None)["repository"]["issues"]
            for issue in page["nodes"]:
                if state == "CLOSED" and not filed_since_rollout(issue["createdAt"]):
                    return
                yield issue
            if not page["pageInfo"]["hasNextPage"]:
                break
            cursor = page["pageInfo"]["endCursor"]


def sweep(repository):
    failures = []
    for issue in eligible_issues(repository):
        try:
            if reconcile_snapshot(repository, issue, write=False):
                reconcile(repository, issue["number"])
        except Exception as error:
            failures.append(f"#{issue['number']} ({error})")
    if failures:
        raise RuntimeError("Failed to classify " + ", ".join(failures))


if __name__ == "__main__":
    repo = os.environ["GITHUB_REPOSITORY"]
    if number := os.environ.get("ISSUE_NUMBER"):
        reconcile(repo, int(number))
    else:
        sweep(repo)
