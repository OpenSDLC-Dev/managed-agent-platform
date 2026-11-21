#!/usr/bin/env python3
"""Exercise the issue label workflow without GitHub credentials."""
import unittest
from unittest.mock import call, patch

from issue_labels import reconcile, sweep


REPO = "owner/repo"
ISSUE = f"repos/{REPO}/issues/23"
FORM = "### Type\n\nbug\n\n### Priority\n\np0\n\n### Difficulty\n\neasy\n\n### Details\n\nSteps to reproduce."


def ticket(names=(), body=None, number=23, **fields):
    return {
        "number": number, "state": "open", "node_id": "issue-id",
        "repository_url": f"https://api.github.com/repos/{REPO}", "body": body,
        "labels": [{"name": n, "node_id": f"id-{n}"} for n in names], **fields,
    }


def labeled(name, event_id, actor="maintainer"):
    return {
        "event": "labeled", "id": event_id, "created_at": "2026-06-01T10:00:00Z",
        "actor": {"login": actor}, "label": {"name": name},
    }


class IssueLabelsTest(unittest.TestCase):
    def setUp(self):
        patched_api = patch("issue_labels.api")
        self.api = patched_api.start()
        self.addCleanup(patched_api.stop)

    def test_form_only_fills_missing_fields(self):
        self.api.side_effect = [ticket(["p2"], FORM.replace("\n", "\r\n")), None]
        reconcile(REPO, 23)
        self.assertEqual(self.api.call_args_list, [
            call(ISSUE), call(ISSUE + "/labels", payload={"labels": ["bug", "easy"]}),
        ])

    def test_free_text_and_invalid_answers_cannot_supply_other_labels(self):
        for body in [None, "Example:\n" + FORM, "### Details\n\n" + FORM]:
            with self.subTest(body=body):
                self.api.reset_mock()
                self.api.side_effect = [ticket(body=body), None]
                reconcile(REPO, 23)
                self.assertEqual(self.api.call_args,
                                 call(ISSUE + "/labels", payload={"labels": ["feature", "hard", "p1"]}))
        self.api.side_effect = [ticket(body=FORM.replace("p0", "p0, urgent") + "\n" + FORM), None]
        reconcile(REPO, 23)
        self.assertEqual(self.api.call_args, call(ISSUE + "/labels", payload={"labels": ["bug", "easy", "p1"]}))

    def test_complete_manual_classification_survives_edits(self):
        self.api.return_value = ticket(["feature", "p2", "medium"], FORM)
        reconcile(REPO, 23)
        self.api.assert_called_once_with(ISSUE)

    def test_form_choices_and_legacy_labels_in_one_pass(self):
        self.api.side_effect = [ticket(["old label"], FORM + "\n\n### Priority\n\np2"), None, None]
        reconcile(REPO, 23)
        added, removed = self.api.call_args_list[1:]
        self.assertEqual(added, call(ISSUE + "/labels", payload={"labels": ["bug", "easy", "p0"]}))
        self.assertEqual(removed.kwargs["payload"]["variables"]["input"],
                         {"labelableId": "issue-id", "labelIds": ["id-old label"]})

    def test_recent_manual_label_wins_despite_out_of_order_delivery_and_bot_fallback(self):
        self.api.side_effect = [
            ticket(["bug", "p0", "p1", "p2", "medium", "old, label"]),
            [[labeled("p1", 8), labeled("p0", 3)], [{"event": "renamed"}, labeled("p2", 9, "github-actions[bot]")]],
            None,
        ]
        reconcile(REPO, 23)
        self.assertEqual(self.api.call_args_list[1], call(ISSUE + "/events?per_page=100", "--paginate", "--slurp"))
        endpoint = self.api.call_args.args[0]
        payload = self.api.call_args.kwargs["payload"]
        self.assertEqual(endpoint, "graphql")
        self.assertIn("removeLabelsFromLabelable", payload["query"])
        self.assertEqual(payload["variables"]["input"], {
            "labelableId": "issue-id", "labelIds": ["id-old, label", "id-p0", "id-p2"],
        })

    def test_skip_closed_transferred_away_and_pull_requests(self):
        for fields in [{"state": "closed"}, {"repository_url": "https://api.github.com/repos/elsewhere/repo"},
                       {"pull_request": {}}]:
            with self.subTest(fields=fields):
                self.api.reset_mock()
                self.api.return_value = ticket(**fields)
                reconcile(REPO, 23)
                self.api.assert_called_once_with(ISSUE)

    def test_sweep_paginates_and_only_reloads_noncompliant_open_issues(self):
        pages = [
            [ticket(["feature", "p1", "hard"], number=1), ticket(number=2, pull_request={})],
            [ticket(["bug"], number=23)],
        ]
        # The maintainer supplied the remaining labels after the list response.
        self.api.side_effect = [pages, ticket(["bug", "p2", "easy"])]
        sweep(REPO)
        self.assertEqual(self.api.call_args_list, [
            call(f"repos/{REPO}/issues?state=open&per_page=100", "--paginate", "--slurp"), call(ISSUE),
        ])


if __name__ == "__main__":
    unittest.main()
