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
GUARD = ROOT / "harness/hooks/issue-triage-bash-guard.sh"
READER = ROOT / "harness/hooks/issue-triage-read.sh"


def run_guard(payload, *, env=None, cwd=ROOT):
    return subprocess.run(
        ["/bin/sh", str(GUARD)],
        input=payload,
        text=True,
        capture_output=True,
        env=env,
        cwd=cwd,
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
            "harness/hooks/issue-triage-read.sh issue-view 1052",
            "harness/hooks/issue-triage-read.sh issue-view 1052 --comments",
            "harness/hooks/issue-triage-read.sh issue-list",
            "harness/hooks/issue-triage-read.sh issue-list --state open --limit 25 --label security --label area/vault",
            "harness/hooks/issue-triage-read.sh pr-view 1054",
            "harness/hooks/issue-triage-read.sh pr-view 1054 --comments",
            "harness/hooks/issue-triage-read.sh git-log",
            "harness/hooks/issue-triage-read.sh git-log -n 10 --oneline",
            "harness/hooks/issue-triage-read.sh git-log -n 5 --stat HEAD~5..HEAD -- internal/gate docs/plan",
            "harness/hooks/issue-triage-read.sh git-show HEAD",
            "harness/hooks/issue-triage-read.sh git-show --stat --no-patch HEAD~1 -- internal/api/server.go",
            "harness/hooks/issue-triage-read.sh git-show HEAD:README.md",
        ]
        with tempfile.TemporaryDirectory() as tmpdir:
            no_py_env = self._no_python_env(tmpdir)
            utf8_env = {**os.environ, "LC_ALL": "en_US.UTF-8"}
            for label, env in [("python", utf8_env), ("posix-fallback", no_py_env)]:
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
            "harness/hooks/issue-triage-read.sh",
            " harness/hooks/issue-triage-read.sh git-log",
            # Shell quoting / backslash / metacharacter bypasses
            'harness/hooks/issue-triage-read.sh git-log --out""put=/tmp/pwn',
            r"harness/hooks/issue-triage-read.sh git-log --out\put=/tmp/pwn",
            "harness/hooks/issue-triage-read.sh git-log '--output=/tmp/pwn'",
            "harness/hooks/issue-triage-read.sh git-log --output=/tmp/pwn",
            "harness/hooks/issue-triage-read.sh git-log --output /tmp/pwn",
            "harness/hooks/issue-triage-read.sh git-show --output /tmp/pwn HEAD",
            "harness/hooks/issue-triage-read.sh git-log --ext-diff",
            "harness/hooks/issue-triage-read.sh git-show --ext-diff HEAD",
            "harness/hooks/issue-triage-read.sh git-show --textconv HEAD",
            "harness/hooks/issue-triage-read.sh git-log -c core.pager=sh",
            "harness/hooks/issue-triage-read.sh issue-view 1052 --web",
            "harness/hooks/issue-triage-read.sh issue-view 1052 -w",
            "harness/hooks/issue-triage-read.sh issue-list --web",
            "harness/hooks/issue-triage-read.sh issue-list --jq .",
            "harness/hooks/issue-triage-read.sh issue-list --template x",
            "harness/hooks/issue-triage-read.sh issue-view 1052; touch /tmp/pwn",
            "harness/hooks/issue-triage-read.sh issue-view 1052 && id",
            "harness/hooks/issue-triage-read.sh issue-view 1052 | cat",
            "harness/hooks/issue-triage-read.sh issue-view $(id)",
            "harness/hooks/issue-triage-read.sh issue-view `id`",
            "harness/hooks/issue-triage-read.sh issue-view 1052 > /tmp/pwn",
            "harness/hooks/issue-triage-read.sh git-log\ntouch /tmp/pwn",
            "harness/hooks/issue-triage-read.sh git-log\n",
            "harness/hooks/issue-triage-read.sh git-log\x00; touch /tmp/pwn",
            "harness/hooks/issue-triage-read.sh\x00 git-log",
            "harness/hooks/issue-triage-read.sh git-log\rtouch /tmp/pwn",
            "harness/hooks/issue-triage-read.sh git-log\t--oneline",
            "harness/hooks/issue-triage-read.sh git-log *",
            "harness/hooks/issue-triage-read.sh git-log ~root",
            "harness/hooks/issue-triage-read.sh git-log ^HEAD",
            "harness/hooks/issue-triage-read.sh git-log café",
            "FOO=bar harness/hooks/issue-triage-read.sh git-log",
            # Argument validation edge cases
            "harness/hooks/issue-triage-read.sh issue-view abc",
            "harness/hooks/issue-triage-read.sh issue-view 0",
            "harness/hooks/issue-triage-read.sh issue-view 000010",
            "harness/hooks/issue-triage-read.sh issue-view 99999999999999999999999999999999",
            "harness/hooks/issue-triage-read.sh issue-list --state invalid",
            "harness/hooks/issue-triage-read.sh issue-list --limit 0",
            "harness/hooks/issue-triage-read.sh issue-list --limit 010",
            "harness/hooks/issue-triage-read.sh issue-list --limit 101",
            "harness/hooks/issue-triage-read.sh issue-list --label -bad",
            "harness/hooks/issue-triage-read.sh git-log -n 0",
            "harness/hooks/issue-triage-read.sh git-log -n 001",
            "harness/hooks/issue-triage-read.sh git-log -n 201",
            "harness/hooks/issue-triage-read.sh git-log -n 99999999999999999999999999999999",
            "harness/hooks/issue-triage-read.sh git-log -- ../outside",
            "harness/hooks/issue-triage-read.sh git-log -- /etc/passwd",
            "harness/hooks/issue-triage-read.sh git-log -- -rf",
            "harness/hooks/issue-triage-read.sh git-show",
            "harness/hooks/issue-triage-read.sh git-show --stat",
            "harness/hooks/issue-triage-read.sh --check git-log",
            "harness/hooks/issue-triage-read.sh unknown-subcmd",
        ]
        with tempfile.TemporaryDirectory() as tmpdir:
            no_py_env = self._no_python_env(tmpdir)
            utf8_env = {**os.environ, "LC_ALL": "en_US.UTF-8"}
            for label, env in [("python", utf8_env), ("posix-fallback", no_py_env)]:
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
            ambiguous = '{"other":{"command":"x"},"tool_input":{"command":"harness/hooks/issue-triage-read.sh git-log"}}'
            res = run_guard(ambiguous, env=no_py_env)
            self.assertEqual(res.returncode, 2)
            self.assertIn("issue-triage guard:", res.stderr)

    def test_subdirectory_cwd_cannot_shadow_reader(self):
        # Running from a subdirectory (even one containing a decoy
        # harness/hooks/issue-triage-read.sh) must be rejected so the relative
        # command path always resolves to the validated repo-root reader.
        with tempfile.TemporaryDirectory(dir=ROOT) as tmpdir:
            fixture = Path(tmpdir) / "harness/hooks/issue-triage-read.sh"
            fixture.parent.mkdir(parents=True)
            fixture.write_text("#!/bin/sh\nexit 0\n")
            fixture.chmod(0o755)
            res = run_guard(payload_for("harness/hooks/issue-triage-read.sh git-log"), cwd=tmpdir)
            self.assertEqual(res.returncode, 2)
            self.assertIn("repository root", res.stderr)


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

    def test_git_disables_malicious_external_diff_textconv_and_gpg(self):
        marker = self.root / "pwned"
        evil = self.root / "evil.sh"
        evil.write_text(f'#!/bin/sh\ntouch "{marker}"\n')
        evil.chmod(0o755)
        self._git("config", "diff.external", str(evil))
        self._git("config", "diff.txt.command", str(evil))
        self._git("config", "diff.txt.textconv", str(evil))
        self._git("config", "format.pretty", "format:%G?")
        self._git("config", "log.showSignature", "true")
        self._git("config", "gpg.program", str(evil))
        (self.repo / ".gitattributes").write_text("*.txt diff=txt\n")

        # Create a commit carrying a gpgsig header so log.showSignature=true or
        # format.pretty=format:%G? would invoke gpg.program unless suppressed.
        tree = subprocess.run(
            ["git", "rev-parse", "HEAD^{tree}"],
            cwd=self.repo, check=True, capture_output=True, text=True,
        ).stdout.strip()
        parent = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=self.repo, check=True, capture_output=True, text=True,
        ).stdout.strip()
        signed_obj = (
            f"tree {tree}\n"
            f"parent {parent}\n"
            "author Test User <test@example.com> 1700000000 +0000\n"
            "committer Test User <test@example.com> 1700000000 +0000\n"
            "gpgsig -----BEGIN PGP SIGNATURE-----\n"
            " \n"
            " fake\n"
            " -----END PGP SIGNATURE-----\n"
            "\n"
            "signed commit\n"
        )
        signed_sha = subprocess.run(
            ["git", "hash-object", "-t", "commit", "-w", "--stdin"],
            cwd=self.repo, input=signed_obj, check=True, capture_output=True, text=True,
        ).stdout.strip()
        self._git("reset", "--hard", "-q", signed_sha)

        env_with_ext = {**self.env, "GIT_EXTERNAL_DIFF": str(evil)}
        # Include HEAD~1 (which renders a changed-file patch on hello.txt) and
        # signed HEAD so external diff, textconv, and gpg.program paths are all
        # actively exercised and proven suppressed.
        for args in [
            ("git-show", "HEAD"),
            ("git-show", "HEAD~1"),
            ("git-show", "HEAD:hello.txt"),
            ("git-log", "-n", "3"),
            ("git-log", "-n", "2", "--stat"),
        ]:
            r = subprocess.run(
                [str(READER), *args],
                cwd=self.repo, env=env_with_ext, capture_output=True, text=True,
            )
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertFalse(marker.exists(), f"{args} triggered external diff, textconv, or gpg.program")

        # Positive control: unsuppressed raw git show HEAD~1 triggers the hook,
        # proving the test fails if wrapper suppression is removed.
        subprocess.run(["git", "--no-pager", "show", "HEAD~1"], cwd=self.repo, env=env_with_ext, capture_output=True, text=True)
        self.assertTrue(marker.exists(), "positive control failed: raw git show did not trigger configured hook")

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
