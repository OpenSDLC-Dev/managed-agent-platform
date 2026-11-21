#!/usr/bin/env python3
"""Exercise the issue label workflow without GitHub credentials."""
import unittest
from itertools import product
from pathlib import Path
from unittest.mock import call, patch

from issue_labels import FORM as FORM_PATTERN, FORM_TYPES
from issue_labels import reconcile, sweep


REPO = "owner/repo"
ISSUE = f"repos/{REPO}/issues/23"
FORM = "### Priority\n\np0\n\n### Difficulty\n\neasy\n\n### Bug details\n\nSteps to reproduce."


def ticket(names=(), body=None, number=23, kind="Task", **fields):
    return {
        "number": number, "state": "open", "node_id": "issue-id",
        "repository_url": f"https://api.github.com/repos/{REPO}", "body": body,
        "created_at": "2026-11-19T03:00:00Z",
        "type": {"name": kind} if kind else None,
        "labels": [{"name": n, "node_id": f"id-{n}"} for n in names], **fields,
    }


def label_event(name, event_id, actor="maintainer", action="labeled"):
    return {
        "event": action, "id": event_id, "created_at": "2026-06-01T10:00:00Z",
        "actor": {"login": actor}, "label": {"name": name},
    }


class IssueLabelsTest(unittest.TestCase):
    def setUp(self):
        patched_api = patch("issue_labels.api")
        self.api = patched_api.start()
        self.addCleanup(patched_api.stop)

    def test_form_only_fills_missing_fields(self):
        self.api.side_effect = [ticket(["p2"], FORM.replace("\n", "\r\n")), [[]], None]
        reconcile(REPO, 23)
        self.assertEqual(self.api.call_args_list, [
            call(ISSUE), call(ISSUE + "/events?per_page=100", "--paginate", "--slurp"),
            call(ISSUE + "/labels", payload={"labels": ["easy"]}),
        ])

    def test_template_headings_render_the_expected_type_and_answers(self):
        templates = Path(__file__).resolve().parents[1] / "ISSUE_TEMPLATE"
        for kind in ("Bug", "Feature", "Task"):
            lines = (templates / (kind.lower() + ".yml")).read_text(encoding="utf-8").splitlines()
            self.assertIn("type: " + kind, lines)
            headings = [line.strip().split(": ", 1)[1] for line in lines if line.lstrip().startswith("label: ")]
            for priority, difficulty in product(("p0", "p1", "p2"), ("easy", "medium", "hard")):
                answers = (priority, difficulty, "Sample report.")
                body = "\n\n".join(f"### {heading}\n\n{answer}" for heading, answer in zip(headings, answers))
                form = FORM_PATTERN.match(body)
                self.assertIsNotNone(form)
                self.assertEqual((form[1], form[2], FORM_TYPES[form[3]]), (priority, difficulty, kind))

    def test_free_text_and_invalid_answers_cannot_supply_other_labels(self):
        for body in [None, "Example:\n" + FORM, "### Details\n\n" + FORM]:
            with self.subTest(body=body):
                self.api.reset_mock()
                self.api.side_effect = [ticket(body=body), [[]], None]
                reconcile(REPO, 23)
                self.assertEqual(self.api.call_args,
                                 call(ISSUE + "/labels", payload={"labels": ["hard", "p1"]}))
        self.api.side_effect = [ticket(body=FORM.replace("p0", "p0, urgent") + "\n" + FORM), [[]], None]
        reconcile(REPO, 23)
        self.assertEqual(self.api.call_args, call(ISSUE + "/labels", payload={"labels": ["easy", "p1"]}))

    def test_complete_manual_classification_survives_edits(self):
        self.api.return_value = ticket(["p2", "medium"], FORM)
        reconcile(REPO, 23)
        self.api.assert_called_once_with(ISSUE)

    def test_form_choices_and_legacy_labels_in_one_pass(self):
        self.api.side_effect = [ticket(["old label"], FORM + "\n\n### Priority\n\np2"), [[]], None, None]
        reconcile(REPO, 23)
        added, removed = self.api.call_args_list[2:]
        self.assertEqual(added, call(ISSUE + "/labels", payload={"labels": ["easy", "p0"]}))
        self.assertEqual(removed.kwargs["payload"]["variables"]["input"],
                         {"labelableId": "issue-id", "labelIds": ["id-old label"]})

    def test_recent_manual_label_wins_despite_out_of_order_delivery_and_bot_fallback(self):
        self.api.side_effect = [
            ticket(["p0", "p1", "p2", "medium", "old, label"]),
            [[label_event("p1", 8), label_event("p0", 3)], [{"event": "renamed"}, label_event("p2", 9, "github-actions[bot]")]],
            None, ticket(["p1", "medium"]),
        ]
        reconcile(REPO, 23)
        self.assertEqual(self.api.call_args_list[1], call(ISSUE + "/events?per_page=100", "--paginate", "--slurp"))
        cleanup = self.api.call_args_list[-2]
        endpoint = cleanup.args[0]
        payload = cleanup.kwargs["payload"]
        self.assertEqual(endpoint, "graphql")
        self.assertIn("removeLabelsFromLabelable", payload["query"])
        self.assertEqual(payload["variables"]["input"], {
            "labelableId": "issue-id", "labelIds": ["id-old, label", "id-p0", "id-p2"],
        })

    def test_skip_closed_transferred_away_and_pull_requests(self):
        for fields in [{"state": "closed"}, {"repository_url": "https://api.github.com/repos/elsewhere/repo"},
                       {"pull_request": {}, "state": "closed", "created_at": "2026-11-22T04:00:00Z"}]:
            with self.subTest(fields=fields):
                self.api.reset_mock()
                self.api.return_value = ticket(**fields)
                reconcile(REPO, 23)
                self.api.assert_called_once_with(ISSUE)

    def test_conflict_cleanup_races_with_human_removing_the_winner(self):
        names = {"p2", "p0", "medium"}
        events = [label_event("p2", 1), label_event("p0", 2)]

        def github(endpoint, *args, payload=None):
            if endpoint == ISSUE:
                return ticket(names)
            if endpoint == ISSUE + "/events?per_page=100":
                return [list(events)]
            if endpoint == "graphql":
                self.assertEqual(payload["variables"]["input"]["labelIds"], ["id-p2"])
                # Between the read and the mutation the maintainer kept p2.
                names.remove("p0")
                events.append(label_event("p0", 3, action="unlabeled"))
                names.remove("p2")
                events.append(label_event("p2", 4, "github-actions[bot]", "unlabeled"))
                return
            self.assertEqual(endpoint, ISSUE + "/labels")
            names.update(payload["labels"])

        self.api.side_effect = github
        reconcile(REPO, 23)
        self.assertEqual(names, {"p2", "medium"})

    def test_human_clearing_last_label_does_not_resurrect_an_earlier_bot_removal(self):
        events = [label_event("p2", 1), label_event("p0", 2),
                  label_event("p2", 3, "github-actions[bot]", "unlabeled"),
                  label_event("p0", 4, action="unlabeled")]
        for body, expected in [(None, "p1"), (FORM, "p0")]:
            with self.subTest(body=body):
                self.api.side_effect = [ticket(["medium"], body), [events], None]
                reconcile(REPO, 23)
                self.assertEqual(self.api.call_args, call(ISSUE + "/labels", payload={"labels": [expected]}))

    def test_issue_closed_immediately_after_rollout_gets_classified(self):
        self.api.side_effect = [ticket(["medium"], state="closed", created_at="2026-11-21T04:40:00Z"), [[]], None]
        reconcile(REPO, 23)
        self.assertEqual(self.api.call_args, call(ISSUE + "/labels", payload={"labels": ["p1"]}))

    def test_native_type_precedes_legacy_label_form_and_default(self):
        cases = [
            ("Task", "bug", FORM, "Task"), (None, "bug", None, "Bug"),
            (None, "feature", FORM, "Feature"), (None, None, FORM.replace("Bug details", "Task details"), "Task"),
            (None, None, FORM, "Bug"), (None, None, "### Bug details\n\nexample", "Feature"),
            (None, None, FORM.replace("p0", "unknown"), "Bug"),
        ]
        for current, legacy, body, expected in cases:
            with self.subTest(current=current, legacy=legacy, expected=expected):
                issue = ticket(["p2", "easy"] + ([legacy] if legacy else []), body, kind=current)

                def github(endpoint, *args, payload=None):
                    if endpoint == ISSUE and not args:
                        return issue
                    if endpoint == ISSUE and args == ("--method", "PATCH"):
                        issue["type"] = {"name": payload["type"]}
                        return issue
                    if "timelineItems" in payload["query"]:
                        return {"data": {"node": {"issueType": issue["type"], "timelineItems": {"nodes": []}}}}
                    self.assertEqual(issue["type"]["name"], expected)
                    self.assertEqual(payload["variables"]["input"]["labelIds"], [f"id-{legacy}"])
                    issue["labels"] = [v for v in issue["labels"] if v["name"] != legacy]

                self.api.side_effect = github
                reconcile(REPO, 23)
                self.assertEqual(issue["type"], {"name": expected})
                self.assertEqual({v["name"] for v in issue["labels"]}, {"p2", "easy"})

    def test_type_write_rejected_before_clearing_legacy_label(self):
        issue = ticket(["bug", "p2", "easy"], kind=None)
        self.api.side_effect = [issue, issue]
        with self.assertRaisesRegex(RuntimeError, "type"):
            reconcile(REPO, 23)
        self.assertEqual(self.api.call_args, call(ISSUE, "--method", "PATCH", payload={"type": "Bug"}))

    def test_human_type_set_during_provisional_write_survives(self):
        issue = ticket(["p2", "easy"], kind=None)
        writes = []
        change = {}

        def github(endpoint, *args, payload=None):
            nonlocal change
            if endpoint == ISSUE and not args:
                return issue
            if endpoint == ISSUE and args == ("--method", "PATCH"):
                if not writes:
                    # The maintainer selected Task after the bot saw no type.
                    issue["type"] = {"name": "Task"}
                change = {"actor": {"__typename": "Bot", "login": "github-actions"},
                          "prevIssueType": issue["type"]}
                issue["type"] = {"name": payload["type"]}
                writes.append(payload["type"])
                return issue
            self.assertIn("timelineItems", payload["query"])
            return {"data": {"node": {"issueType": issue["type"], "timelineItems": {"nodes": [change]}}}}

        self.api.side_effect = github
        reconcile(REPO, 23)
        self.assertEqual(writes, ["Feature", "Task"])
        self.assertEqual(issue["type"]["name"], "Task")

    def test_sweep_paginates_issues_only_and_stops_at_pre_rollout_closed_history(self):
        def node(number, labels, created_at, kind="Task"):
            return {"number": number, "createdAt": created_at, "issueType": {"name": kind} if kind else None,
                    "labels": {"nodes": [{"name": n} for n in labels]}}

        def page(nodes, cursor=None):
            return {"data": {"repository": {"issues": {
                "nodes": nodes, "pageInfo": {"hasNextPage": bool(cursor), "endCursor": cursor},
            }}}}

        old = "2026-11-21T04:39:59Z"
        recent = "2026-11-21T04:40:00Z"
        self.api.side_effect = [
            page([node(1, ["p1", "hard"], old)], "open-next"),
            page([node(23, ["p2", "easy"], old, kind=None)]),
            ticket(["p2", "easy"]),  # Type was set after the list response.
            page([node(42, [], recent)], "closed-next"),
            ticket(number=42, state="closed", created_at=recent), [[]], None,
            page([node(3, [], old)], "never-fetched"),
        ]
        sweep(REPO)
        pages = [c.kwargs["payload"] for c in self.api.call_args_list if c.args == ("graphql",)]
        self.assertEqual([(p["variables"]["state"], p["variables"]["cursor"]) for p in pages],
                         [("OPEN", None), ("OPEN", "open-next"), ("CLOSED", None), ("CLOSED", "closed-next")])
        self.assertIn("issues(states: [$state]", pages[0]["query"])
        self.assertIn("orderBy: {field: CREATED_AT, direction: DESC}", pages[0]["query"])
        closed = f"repos/{REPO}/issues/42"
        self.assertEqual([c for c in self.api.call_args_list if c.args[0] != "graphql"], [
            call(ISSUE), call(closed), call(closed + "/events?per_page=100", "--paginate", "--slurp"),
            call(closed + "/labels", payload={"labels": ["hard", "p1"]}),
        ])


if __name__ == "__main__":
    unittest.main()
