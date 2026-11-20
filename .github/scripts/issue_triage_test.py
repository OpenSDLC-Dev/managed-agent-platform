#!/usr/bin/env python3
"""Regression tests for the issue-triage read-only wrapper and PreToolUse Bash guard (#1052)."""
import json
import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
GUARD = ROOT / ".claude/hooks/issue-triage-bash-guard.sh"
READER = ROOT / ".claude/hooks/issue-triage-read.sh"


def run_guard(payload, *, env=None):
    return subprocess.run(
        ["/bin/sh", str(GUARD)],
        input=payload,
        text=True,
        capture_output=True,
        env=env,
    )


def payload_for(cmd):
    return json.dumps({"tool_name": "Bash", "tool_input": {"command": cmd}})


class IssueTriageGuardTests(unittest.TestCase):
    def _no_python_env(self, tmpdir):
        # Provide a minimal PATH containing standard POSIX utilities (cat, awk,
        # sed, dirname, pwd, sh) without python/python3 so the POSIX fallback
        # path (#295) is exercised directly.
        bindir = Path(tmpdir) / "bin"
        bindir.mkdir()
        for tool in ["sh", "cat", "awk", "sed", "dirname", "pwd"]:
            found = None
            for base in ["/bin", "/usr/bin"]:
                candidate = Path(base) / tool
                if candidate.exists():
                    found = candidate
                    break
            if found is None:
                raise AssertionError(f"required POSIX utility missing: {tool}")
            (bindir / tool).symlink_to(found)
        env = {"PATH": str(bindir)}
        return env

    def test_allowed_structured_commands(self):
        allowed = [
            ".claude/hooks/issue-triage-read.sh issue-view 1052",
            ".claude/hooks/issue-triage-read.sh issue-view 1052 --comments",
            ".claude/hooks/issue-triage-read.sh issue-list",
            ".claude/hooks/issue-triage-read.sh issue-list --state open --limit 25 --label security --label area/vault",
            ".claude/hooks/issue-triage-read.sh pr-view 1054",
            ".claude/hooks/issue-triage-read.sh pr-view 1054 --comments",
            ".claude/hooks/issue-triage-read.sh git-log",
            ".claude/hooks/issue-triage-read.sh git-log -n 10 --oneline",
            ".claude/hooks/issue-triage-read.sh git-log -n 5 --stat HEAD~5..HEAD -- internal/gate docs/plan",
            ".claude/hooks/issue-triage-read.sh git-show HEAD",
            ".claude/hooks/issue-triage-read.sh git-show --stat --no-patch HEAD~1 -- internal/api/server.go",
            ".claude/hooks/issue-triage-read.sh git-show HEAD:README.md",
        ]
        with tempfile.TemporaryDirectory() as tmpdir:
            no_py_env = self._no_python_env(tmpdir)
            for label, env in [("python", None), ("posix-fallback", no_py_env)]:
                for cmd in allowed:
                    with self.subTest(mode=label, cmd=cmd):
                        res = run_guard(payload_for(cmd), env=env)
                        self.assertEqual(res.returncode, 0, res.stderr)

    def test_rejected_commands_and_shell_bypasses(self):
        rejected = [
            # Raw commands outside the structured wrapper
            "git log",
            "git show HEAD",
            "gh issue view 1052",
            "gh issue list",
            "gh pr view 1054",
            "ls -la",
            "cat /etc/passwd",
            # Empty / missing subcommand / leading whitespace
            "",
            ".claude/hooks/issue-triage-read.sh",
            " .claude/hooks/issue-triage-read.sh git-log",
            # Shell quoting / backslash / metacharacter bypasses
            '.claude/hooks/issue-triage-read.sh git-log --out""put=/tmp/pwn',
            r".claude/hooks/issue-triage-read.sh git-log --out\put=/tmp/pwn",
            ".claude/hooks/issue-triage-read.sh git-log '--output=/tmp/pwn'",
            ".claude/hooks/issue-triage-read.sh git-log --output=/tmp/pwn",
            ".claude/hooks/issue-triage-read.sh git-log --output /tmp/pwn",
            ".claude/hooks/issue-triage-read.sh git-show --output /tmp/pwn HEAD",
            ".claude/hooks/issue-triage-read.sh git-log --ext-diff",
            ".claude/hooks/issue-triage-read.sh git-show --ext-diff HEAD",
            ".claude/hooks/issue-triage-read.sh git-show --textconv HEAD",
            ".claude/hooks/issue-triage-read.sh git-log -c core.pager=sh",
            ".claude/hooks/issue-triage-read.sh issue-view 1052 --web",
            ".claude/hooks/issue-triage-read.sh issue-view 1052 -w",
            ".claude/hooks/issue-triage-read.sh issue-list --web",
            ".claude/hooks/issue-triage-read.sh issue-list --jq .",
            ".claude/hooks/issue-triage-read.sh issue-list --template x",
            ".claude/hooks/issue-triage-read.sh issue-view 1052; touch /tmp/pwn",
            ".claude/hooks/issue-triage-read.sh issue-view 1052 && id",
            ".claude/hooks/issue-triage-read.sh issue-view 1052 | cat",
            ".claude/hooks/issue-triage-read.sh issue-view $(id)",
            ".claude/hooks/issue-triage-read.sh issue-view `id`",
            ".claude/hooks/issue-triage-read.sh issue-view 1052 > /tmp/pwn",
            ".claude/hooks/issue-triage-read.sh git-log\ntouch /tmp/pwn",
            ".claude/hooks/issue-triage-read.sh git-log\rtouch /tmp/pwn",
            ".claude/hooks/issue-triage-read.sh git-log\t--oneline",
            ".claude/hooks/issue-triage-read.sh git-log *",
            ".claude/hooks/issue-triage-read.sh git-log ~root",
            "FOO=bar .claude/hooks/issue-triage-read.sh git-log",
            # Argument validation edge cases
            ".claude/hooks/issue-triage-read.sh issue-view abc",
            ".claude/hooks/issue-triage-read.sh issue-view 0",
            ".claude/hooks/issue-triage-read.sh issue-list --state invalid",
            ".claude/hooks/issue-triage-read.sh issue-list --limit 0",
            ".claude/hooks/issue-triage-read.sh issue-list --limit 101",
            ".claude/hooks/issue-triage-read.sh issue-list --label -bad",
            ".claude/hooks/issue-triage-read.sh git-log -n 0",
            ".claude/hooks/issue-triage-read.sh git-log -n 201",
            ".claude/hooks/issue-triage-read.sh git-log -- ../outside",
            ".claude/hooks/issue-triage-read.sh git-log -- /etc/passwd",
            ".claude/hooks/issue-triage-read.sh git-log -- -rf",
            ".claude/hooks/issue-triage-read.sh git-show",
            ".claude/hooks/issue-triage-read.sh git-show --stat",
            ".claude/hooks/issue-triage-read.sh --check git-log",
            ".claude/hooks/issue-triage-read.sh unknown-subcmd",
        ]
        with tempfile.TemporaryDirectory() as tmpdir:
            no_py_env = self._no_python_env(tmpdir)
            for label, env in [("python", None), ("posix-fallback", no_py_env)]:
                for cmd in rejected:
                    with self.subTest(mode=label, cmd=cmd):
                        res = run_guard(payload_for(cmd), env=env)
                        self.assertEqual(res.returncode, 2, f"expected deny (2) for {cmd!r}, got {res.returncode}")
                        self.assertNotIn("syntax error", res.stderr.lower(), res.stderr)
                        self.assertTrue(
                            "issue-triage guard:" in res.stderr or "issue-triage-read:" in res.stderr,
                            f"expected explicit guard/reader refusal for {cmd!r}, got {res.stderr!r}",
                        )

    def test_malformed_and_ambiguous_payloads_rejected(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            no_py_env = self._no_python_env(tmpdir)
            for bad_payload in [
                "not json",
                json.dumps({"tool_input": {"command": 123}}),
                json.dumps(["not", "a", "dict"]),
            ]:
                res = run_guard(bad_payload)
                self.assertEqual(res.returncode, 2)
            ambiguous = '{"other":{"command":"x"},"tool_input":{"command":".claude/hooks/issue-triage-read.sh git-log"}}'
            res = run_guard(ambiguous, env=no_py_env)
            self.assertEqual(res.returncode, 2)
            self.assertIn("issue-triage guard:", res.stderr)


class IssueTriageReaderExecutionTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self._git("init", "-q")
        self._git("config", "user.email", "test@example.com")
        self._git("config", "user.name", "Test User")
        (self.repo / "hello.txt").write_text("line1\n")
        self._git("add", "hello.txt")
        self._git("commit", "-q", "-m", "initial commit")
        (self.repo / "hello.txt").write_text("line1\nline2\n")
        self._git("add", "hello.txt")
        self._git("commit", "-q", "-m", "second commit")

        # Install a fake `gh` binary that records its exact argv and GH_PAGER.
        self.fakebin = self.root / "fakebin"
        self.fakebin.mkdir()
        self.gh_log = self.root / "gh-invocations.txt"
        fake_gh = self.fakebin / "gh"
        fake_gh.write_text(
            "#!/bin/sh\n"
            f'printf "GH_PAGER=%s ARGV=%s\\n" "${{GH_PAGER:-}}" "$*" > "{self.gh_log}"\n'
            'printf "ok: %s\\n" "$*"\n'
        )
        fake_gh.chmod(0o755)
        self.env = {**os.environ, "PATH": f"{self.fakebin}:{os.environ['PATH']}"}

    def tearDown(self):
        self.tmp.cleanup()

    def _git(self, *args):
        subprocess.run(["git", *args], cwd=self.repo, check=True, capture_output=True, text=True)

    def _read(self, *args):
        return subprocess.run(
            [str(READER), *args],
            cwd=self.repo,
            env=self.env,
            capture_output=True,
            text=True,
        )

    def test_scripts_are_executable(self):
        for p in (GUARD, READER):
            mode = p.stat().st_mode
            self.assertTrue(mode & stat.S_IXUSR, f"{p} must be executable")

    def test_git_log_and_show_queries_work(self):
        r = self._read("git-log", "-n", "2", "--oneline")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("second commit", r.stdout)
        self.assertIn("initial commit", r.stdout)

        r = self._read("git-log", "-n", "1", "--stat", "--", "hello.txt")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("hello.txt", r.stdout)

        r = self._read("git-show", "--stat", "HEAD", "--", "hello.txt")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("second commit", r.stdout)

        r = self._read("git-show", "HEAD:hello.txt")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout, "line1\nline2\n")

    def test_git_disables_malicious_external_diff_and_textconv(self):
        marker = self.root / "pwned"
        evil = self.root / "evil.sh"
        evil.write_text(f'#!/bin/sh\ntouch "{marker}"\n')
        evil.chmod(0o755)
        self._git("config", "diff.external", str(evil))
        self._git("config", "diff.txt.textconv", str(evil))
        (self.repo / ".gitattributes").write_text("*.txt diff=txt\n")

        r = self._read("git-show", "HEAD")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(marker.exists(), "git diff.external/textconv executed despite read-only wrapper")

        r = self._read("git-log", "-n", "1", "--stat")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(marker.exists(), "git log triggered external diff/textconv")

    def test_gh_queries_pass_exact_args_with_no_pager(self):
        for args, expected in [
            (("issue-view", "1052", "--comments"), "GH_PAGER=cat ARGV=issue view 1052 --comments"),
            (("issue-list", "--state", "open", "--limit", "10", "--label", "security"),
             "GH_PAGER=cat ARGV=issue list --state open --limit 10 --label security"),
            (("pr-view", "1054"), "GH_PAGER=cat ARGV=pr view 1054"),
        ]:
            with self.subTest(args=args):
                r = self._read(*args)
                self.assertEqual(r.returncode, 0, r.stderr)
                self.assertEqual(self.gh_log.read_text().strip(), expected)

    def test_write_capable_options_rejected_without_side_effects(self):
        out_file = self.root / "should-not-exist"
        for bad in [
            ("git-log", f"--output={out_file}"),
            ("git-log", "--output", str(out_file)),
            ("git-show", "--ext-diff", "HEAD"),
            ("issue-view", "1052", "--web"),
            ("issue-view", "1052", "-w"),
            ("issue-list", "--jq", "."),
        ]:
            with self.subTest(bad=bad):
                r = self._read(*bad)
                self.assertEqual(r.returncode, 2)
                self.assertFalse(out_file.exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
