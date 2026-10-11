#!/usr/bin/env python3
"""Table test for push_main_guard.py, the hook that refuses a push to main.

Run it rather than read it: `make push-guard-test`.

The guard has two ways to fail, and both are quiet. A missed shape lets a
session push to main with nothing in its way. A false match refuses a command
the workflow needs, such as the release ritual's tag push or the post-merge
sync, and the session then works around its own guard. So the near-misses
below matter as much as the refusals.
"""

import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

HOOK = pathlib.Path(__file__).with_name("push_main_guard.py")
sys.path.insert(0, str(HOOK.parent))
# The import below would leave a __pycache__ beside the hook in every checkout.
sys.dont_write_bytecode = True

import push_main_guard as guard  # noqa: E402

# (command, branch checked out, refused?, what this stands for)
CASES = [
    # The pushes that update main or master.
    ("git push origin main", "feature", True, "the plain push to main"),
    ("git push origin master", "feature", True, "the other default-branch name"),
    ("git push -f origin main", "feature", True, "a force-push to main"),
    ("git push --force-with-lease origin HEAD:main", "feature", True, "HEAD pushed onto main"),
    ("git push origin +main", "feature", True, "a forced refspec"),
    ("git push origin feature:main", "feature", True, "another branch pushed onto main"),
    ("git push origin feature:refs/heads/main", "feature", True, "the full destination ref"),
    ("git push origin :main", "feature", True, "deleting the remote main"),
    ("git push origin --delete main", "feature", True, "deleting it with --delete"),
    ("git push", "main", True, "no refspec while main is checked out"),
    ("git push origin", "main", True, "a remote and no refspec while on main"),
    ("git push -u origin HEAD", "main", True, "HEAD while on main"),
    ("git push origin @", "main", True, "@, git's alias for HEAD"),
    ("git push --all origin", "feature", True, "every branch, main included"),
    ("git push --mirror origin", "feature", True, "a mirror push"),
    ("git -C ../other push origin main", "feature", True, "a global option before push"),
    ("cd /tmp && git push origin main", "feature", True, "behind a cd"),
    ("git status; git push origin main", "feature", True, "the second of two commands"),
    ("git status\ngit push origin main", "feature", True, "the second line of a script"),
    ("FOO=1 git push origin main", "feature", True, "behind an environment assignment"),
    ("env GIT_TRACE=1 git push origin main", "feature", True, "behind env"),
    ("/usr/bin/git push origin main", "feature", True, "git by its full path"),
    ("git push origin main 'unbalanced", "feature", True,
     "a command shlex cannot split falls back to a loose match"),
    # The near-misses: each of these must run.
    ("git push -u origin chore/opus-5-5-steering", "chore/opus-5-5-steering", False,
     "the iteration workflow's own push"),
    ("git push --force-with-lease origin feature", "feature", False,
     "force-pushing your own branch"),
    ("git push", "feature", False, "no refspec on a branch"),
    ("git push origin main:feature", "main", False, "main as the source, a branch as the destination"),
    ("git push origin maintenance", "feature", False, "a branch whose name starts with main"),
    ("git push origin fix/main-loop", "feature", False, "a branch whose name contains main"),
    ("git push origin v0.6.0", "main", False, "the release ritual's tag push, from main"),
    ("git push --tags", "main", False, "tags only, from main"),
    ("git checkout main && git pull", "main", False, "the post-merge sync"),
    ("git log --oneline main", "feature", False, "a read that names main"),
    ("gh pr merge --squash --delete-branch", "feature", False, "the squash merge itself"),
    ("echo git push origin main", "feature", False, "the words as an argument"),
    ("git commit -m 'never git push origin main'", "feature", False, "the words inside a message"),
    ("echo 'it''s fine", "feature", False, "an unsplittable command with no push in it"),
]


class Table(unittest.TestCase):
    def test_cases(self):
        for command, branch, refused, meaning in CASES:
            with self.subTest(meaning=meaning, command=command):
                reason = guard.refusal(command, "/repo", lambda directory: branch)
                self.assertEqual(reason is not None, refused, reason)

    def test_global_c_option_reads_the_branch_of_that_directory(self):
        def branch_of(directory):
            return "main" if directory == "/repo/sub" else "feature"

        self.assertIsNotNone(guard.refusal("git -C sub push", "/repo", branch_of))
        self.assertIsNone(guard.refusal("git push", "/repo", branch_of))

    def test_cd_moves_the_directory_whose_branch_is_read(self):
        def branch_of(directory):
            return "main" if directory == "/repo/sub" else "feature"

        self.assertIsNotNone(guard.refusal("cd sub && git push", "/repo", branch_of))


class Hook(unittest.TestCase):
    """The hook as Claude Code runs it: the payload on stdin, the verdict as the exit code."""

    def run_hook(self, stdin):
        return subprocess.run([sys.executable, str(HOOK)], input=stdin,
                              capture_output=True, text=True, timeout=30)

    def repo_on(self, branch):
        directory = tempfile.mkdtemp()
        subprocess.run(["git", "init", "-q", "-b", branch, directory], check=True)
        return directory

    def payload(self, command, cwd):
        return json.dumps({"tool_name": "Bash", "tool_input": {"command": command}, "cwd": cwd})

    def test_refuses_with_exit_2_and_names_the_branch(self):
        result = self.run_hook(self.payload("git push", self.repo_on("main")))
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("main", result.stderr)

    def test_allows_a_branch_push(self):
        result = self.run_hook(self.payload("git push", self.repo_on("feature")))
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_allows_a_payload_it_cannot_read(self):
        # Refusing here would block every Bash call in every session.
        for stdin in ("not json", "{}", json.dumps({"tool_input": {"command": 7}})):
            with self.subTest(stdin=stdin):
                self.assertEqual(self.run_hook(stdin).returncode, 0)


if __name__ == "__main__":
    unittest.main()
