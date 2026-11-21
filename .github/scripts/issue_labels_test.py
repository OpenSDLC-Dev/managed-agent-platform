#!/usr/bin/env python3
"""Exercise the issue workflow with a persistent in-memory GitHub timeline."""
import copy
import unittest
from itertools import product
from pathlib import Path
from unittest.mock import patch

from issue_labels import FORM as FORM_PATTERN, FORM_TYPES, reconcile, sweep


REPO = "owner/repo"
FORM = "### Priority\n\np0\n\n### Difficulty\n\neasy\n\n### Bug details\n\nSteps to reproduce."
BOT = {"__typename": "Bot", "login": "github-actions"}
HUMAN = {"__typename": "User", "login": "maintainer"}
ROLLOUT = "2026-11-21T04:40:00Z"
OLD = "2026-11-21T04:39:59Z"


def connection(nodes, cursor=None):
    return {"nodes": nodes, "pageInfo": {"hasNextPage": cursor is not None, "endCursor": cursor}}


class GitHub:
    def __init__(self, labels=(), kind="Task", body=None, number=23, state="OPEN", created=ROLLOUT):
        self.labels = set(labels)
        self.kind = kind
        self.body = body
        self.number = number
        self.state = state
        self.created = created
        self.repo = REPO
        self.is_pr = False
        self.events = []
        self.writes = []
        self.before_write = None
        self.lose_reply = False
        self.reject_type = False
        self.per_page = 100

    def label(self, name, applied, actor):
        if applied:
            self.labels.add(name)
        else:
            self.labels.discard(name)
        self.events.append({"__typename": "LabeledEvent" if applied else "UnlabeledEvent",
                            "actor": actor, "label": {"name": name}})

    def set_type(self, value, actor):
        if self.kind == value:
            return
        previous = self.kind
        self.kind = value
        event = {"actor": actor, "issueType": {"name": value or previous}}
        if value is None:
            event["__typename"] = "IssueTypeRemovedEvent"
        elif previous is None:
            event["__typename"] = "IssueTypeAddedEvent"
        else:
            event.update(__typename="IssueTypeChangedEvent", prevIssueType={"name": previous})
        self.events.append(event)

    def history(self, cursor=None):
        offset = int(cursor or 0)
        end = offset + self.per_page
        return connection(copy.deepcopy(self.events[offset:end]), str(end) if end < len(self.events) else None)

    def snapshot(self):
        return {
            "id": "I" + str(self.number), "number": self.number,
            "repository": {"nameWithOwner": self.repo}, "state": self.state, "createdAt": self.created,
            "issueType": {"name": self.kind} if self.kind else None, "body": self.body,
            "labels": connection([{"name": name, "id": "id-" + name} for name in sorted(self.labels)]),
            "timelineItems": self.history(),
        }

    def rest(self):
        return {"number": self.number, "node_id": "I" + str(self.number), "state": self.state.lower(),
                "repository_url": "https://api.github.com/repos/" + self.repo, "created_at": self.created,
                "type": {"name": self.kind} if self.kind else None, "body": self.body,
                "labels": [{"name": n, "node_id": "id-" + n} for n in self.labels]}

    def __call__(self, endpoint, *args, payload=None):
        path = f"repos/{REPO}/issues/{self.number}"
        if endpoint == "graphql" and "mutation" not in payload["query"]:
            variables = payload["variables"]
            if variables.get("historyCursor"):
                return {"data": {"node": {"timelineItems": self.history(variables["historyCursor"])}}}
            return {"data": {"repository": {"issueOrPullRequest": {} if self.is_pr else self.snapshot()}}}
        if self.before_write:
            hook, self.before_write = self.before_write, None
            hook()
        if endpoint == path and args == ("--method", "PATCH"):
            self.writes.append(("type", payload["type"]))
            if not self.reject_type:
                self.set_type(payload["type"], BOT)
            result = self.rest()
        elif endpoint == path + "/labels":
            for name in payload["labels"]:
                self.writes.append(("add", name))
                self.label(name, True, BOT)
            result = self.rest()["labels"]
        else:
            assert endpoint == "graphql" and "removeLabelsFromLabelable" in payload["query"], endpoint
            for label_id in payload["variables"]["input"]["labelIds"]:
                name = label_id.removeprefix("id-")
                self.writes.append(("remove", name))
                self.label(name, False, BOT)
            result = {"data": {"removeLabelsFromLabelable": {"clientMutationId": None}}}
        if self.lose_reply:
            self.lose_reply = False
            raise RuntimeError("write committed but reply lost")
        return result


class IssueLabelsTest(unittest.TestCase):
    def run_issue(self, gh):
        with patch("issue_labels.api", side_effect=gh):
            reconcile(REPO, gh.number)

    def test_valid_type_recovers_displaced_human_choice_on_a_fresh_run(self):
        gh = GitHub(["p2", "easy"], kind=None)
        gh.set_type("Bug", HUMAN)
        gh.set_type("Feature", BOT)  # Committed before the previous runner died.
        self.run_issue(gh)
        self.assertEqual(gh.kind, "Bug")
        self.run_issue(gh)
        self.assertEqual(gh.writes, [("type", "Bug")])
        gh.set_type("Feature", HUMAN)
        self.run_issue(gh)
        self.assertEqual(gh.writes, [("type", "Bug")])

    def test_valid_label_recovers_reselected_human_winner_on_a_fresh_run(self):
        gh = GitHub(["easy"])
        gh.label("p0", True, HUMAN)
        gh.label("p2", True, HUMAN)
        gh.label("p0", False, HUMAN)
        gh.label("p0", True, HUMAN)  # New generation after the bot read p0 + p2.
        gh.label("p0", False, BOT)  # Committed before the previous runner died.
        self.run_issue(gh)
        self.assertEqual(gh.labels, {"p0", "easy"})
        self.run_issue(gh)
        self.assertEqual(gh.writes, [("add", "p0"), ("remove", "p2")])

    def test_interleaved_type_write_and_lost_reply(self):
        gh = GitHub(["p2", "easy"], kind=None)
        gh.before_write = lambda: gh.set_type("Task", HUMAN)
        gh.lose_reply = True
        with self.assertRaisesRegex(RuntimeError, "reply lost"):
            self.run_issue(gh)
        self.assertEqual(gh.kind, "Feature")
        self.run_issue(gh)
        self.assertEqual(gh.kind, "Task")

    def test_cleanup_withdrawn_winner_restores_the_other_human_choice(self):
        gh = GitHub(["medium"])
        gh.label("p2", True, HUMAN)
        gh.label("p0", True, HUMAN)
        gh.before_write = lambda: gh.label("p0", False, HUMAN)
        self.run_issue(gh)
        self.assertEqual(gh.labels, {"p2", "medium"})

    def test_reselected_loser_between_read_and_remove_survives_a_lost_reply(self):
        gh = GitHub(["hard"])
        gh.label("p0", True, HUMAN)
        gh.label("p2", True, HUMAN)

        def reselect():
            gh.label("p0", False, HUMAN)
            gh.label("p0", True, HUMAN)

        gh.before_write = reselect
        gh.lose_reply = True
        with self.assertRaisesRegex(RuntimeError, "reply lost"):
            self.run_issue(gh)
        self.assertEqual(gh.labels, {"p2", "hard"})
        self.run_issue(gh)
        self.assertEqual(gh.labels, {"p0", "hard"})

    def test_legitimate_cleanup_and_manual_clear_do_not_resurrect_invalidated_choices(self):
        gh = GitHub(["medium"], kind=None, body=FORM)
        gh.set_type("Task", HUMAN)
        gh.set_type(None, HUMAN)
        gh.label("p2", True, HUMAN)
        gh.label("p1", True, HUMAN)
        gh.label("p2", False, BOT)  # Removed the loser while the human winner remained.
        gh.label("p1", False, HUMAN)
        self.run_issue(gh)
        self.assertEqual((gh.kind, gh.labels), ("Bug", {"p0", "medium"}))
        self.run_issue(gh)
        self.assertEqual(gh.writes, [("type", "Bug"), ("add", "p0")])

    def test_replacement_and_bot_fallback_preserve_the_latest_human_selection(self):
        gh = GitHub(["medium", "legacy, label"])
        gh.label("p0", True, HUMAN)
        gh.label("p2", True, HUMAN)
        gh.label("p1", True, BOT)
        gh.per_page = 1
        self.run_issue(gh)
        self.assertEqual(gh.labels, {"medium", "p2"})
        self.assertCountEqual(gh.writes, [("remove", "legacy, label"), ("remove", "p1"), ("remove", "p0")])

    def test_native_type_and_each_label_dimension_are_independent(self):
        cases = [
            ("Task", ["bug", "p2"], FORM, "Task", {"p2", "easy"}),
            (None, ["bug", "medium"], None, "Bug", {"p1", "medium"}),
            (None, ["feature"], FORM, "Feature", {"p0", "easy"}),
            (None, [], FORM.replace("Bug details", "Task details"), "Task", {"p0", "easy"}),
            (None, [], FORM.replace("p0", "invalid"), "Bug", {"p1", "easy"}),
            (None, [], "Example:\n" + FORM, "Feature", {"p1", "hard"}),
            (None, [], "### Bug details\n\n" + FORM, "Feature", {"p1", "hard"}),
            ("Bug", ["p2", "medium"], FORM.replace("\n", "\r\n"), "Bug", {"p2", "medium"}),
        ]
        for kind, labels, body, expected_type, expected_labels in cases:
            with self.subTest(kind=kind, labels=labels, body=body):
                gh = GitHub(labels, kind=kind, body=body)
                self.run_issue(gh)
                self.assertEqual((gh.kind, gh.labels), (expected_type, expected_labels))

    def test_rejected_type_does_not_remove_migration_evidence(self):
        gh = GitHub(["bug", "p2", "easy"], kind=None)
        gh.reject_type = True
        with self.assertRaisesRegex(RuntimeError, "type"):
            self.run_issue(gh)
        self.assertEqual(gh.labels, {"bug", "p2", "easy"})

    def test_only_eligible_issues_are_written(self):
        gh = GitHub(kind=None, state="CLOSED", created=OLD)
        self.run_issue(gh)
        gh.created, gh.is_pr = ROLLOUT, True
        self.run_issue(gh)
        gh.is_pr, gh.repo = False, "elsewhere/repo"
        self.run_issue(gh)
        self.assertEqual(gh.writes, [])
        gh.repo = REPO
        self.run_issue(gh)
        self.assertEqual((gh.kind, gh.labels), ("Feature", {"p1", "hard"}))

    def test_all_template_headings_and_answers_match_the_policy(self):
        folder = Path(__file__).resolve().parents[1] / "ISSUE_TEMPLATE"
        for kind in ("Bug", "Feature", "Task"):
            lines = (folder / (kind.lower() + ".yml")).read_text(encoding="utf-8").splitlines()
            self.assertIn("type: " + kind, lines)
            headings = [line.strip().split(": ", 1)[1] for line in lines if line.lstrip().startswith("label: ")]
            for priority, difficulty in product(("p0", "p1", "p2"), ("easy", "medium", "hard")):
                values = (priority, difficulty, "Details.")
                body = "\r\n\r\n".join(f"### {heading}\r\n\r\n{value}" for heading, value in zip(headings, values))
                form = FORM_PATTERN.match(body.replace("\r\n", "\n"))
                self.assertEqual((form[1], form[2], FORM_TYPES[form[3]]), (priority, difficulty, kind))

    def test_sweep_repairs_valid_stale_state_continues_after_failure_and_honors_cutoff(self):
        failed = GitHub(["p2", "easy"], kind=None, number=4)
        failed.reject_type = True
        stale = GitHub(["p1", "hard"], kind=None, number=3)
        stale.set_type("Task", HUMAN)
        stale.set_type("Bug", BOT)
        recent = GitHub(number=2, state="CLOSED")
        another_failure = GitHub(kind=None, number=6, state="CLOSED")
        another_failure.reject_type = True
        old = GitHub(number=1, state="CLOSED", created=OLD)
        all_issues = [failed, stale, recent, another_failure, old]
        pages = []

        def github(endpoint, *args, payload=None):
            if endpoint == "graphql" and "state" in payload["variables"]:
                state, cursor = payload["variables"]["state"], payload["variables"]["cursor"]
                pages.append((state, cursor))
                if state == "OPEN":
                    rows, next_page = ([failed], "open") if cursor is None else ([stale], None)
                else:
                    rows, next_page = ([recent, another_failure], "closed") if cursor is None else ([old], "never")
                return {"data": {"repository": {"issues": connection([g.snapshot() for g in rows], next_page)}}}
            for gh in all_issues:
                if endpoint.startswith(f"repos/{REPO}/issues/{gh.number}") or (
                    endpoint == "graphql" and payload["variables"].get("number") == gh.number
                ):
                    return gh(endpoint, *args, payload=payload)
            self.fail(f"Unexpected call to {endpoint}")

        with patch("issue_labels.api", side_effect=github), self.assertRaisesRegex(RuntimeError, "#4.*#6"):
            sweep(REPO)
        self.assertEqual(pages, [("OPEN", None), ("OPEN", "open"), ("CLOSED", None), ("CLOSED", "closed")])
        self.assertEqual(stale.kind, "Task")
        self.assertEqual(recent.labels, {"p1", "hard"})
        self.assertEqual(old.writes, [])

    def test_all_triggers_share_one_repository_queue(self):
        workflow = Path(__file__).resolve().parents[1] / "workflows" / "issue-labels.yml"
        block = workflow.read_text(encoding="utf-8").split("\nconcurrency:\n", 1)[1].split("\n\n", 1)[0]
        settings = dict(line.strip().split(": ", 1) for line in block.splitlines())
        self.assertTrue(settings["group"])
        self.assertNotIn("${{", settings["group"])
        self.assertEqual(settings["queue"], "max")
        self.assertEqual(settings["cancel-in-progress"], "false")


if __name__ == "__main__":
    unittest.main()
