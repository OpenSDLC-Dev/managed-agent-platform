#!/usr/bin/env python3
"""Regression tests for .claude/hooks/verify-stamp.sh and verify-commit-gate.sh (plan 75 slice 7)."""
import json
import os
import stat
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
GATE = ROOT / ".claude/hooks/verify-commit-gate.sh"
STAMP = ROOT / ".claude/hooks/verify-stamp.sh"
SETTINGS = ROOT / ".claude/settings.json"


def payload_for(cmd: str, session_id: str = "test-session") -> str:
    return json.dumps({
        "tool_name": "Bash",
        "session_id": session_id,
        "tool_input": {"command": cmd},
    })


def run_git(repo: Path, *args: str) -> subprocess.CompletedProcess:
    env = {
        **os.environ,
        "GIT_AUTHOR_NAME": "Test",
        "GIT_AUTHOR_EMAIL": "test@example.com",
        "GIT_COMMITTER_NAME": "Test",
        "GIT_COMMITTER_EMAIL": "test@example.com",
    }
    return subprocess.run(
        ["git", *args],
        cwd=repo,
        env=env,
        text=True,
        capture_output=True,
        check=True,
    )


def init_repo(path: Path) -> None:
    run_git(path, "init", "-q")
    (path / "hello.txt").write_text("v1\n", encoding="utf-8")
    run_git(path, "add", "hello.txt")
    run_git(path, "commit", "-q", "-m", "initial")


def run_gate(repo: Path, cmd: str, *, mode: str = "observe", extra_env=None, session_id: str = "test-session"):
    env = {**os.environ, "VERIFY_GATE_MODE": mode}
    if extra_env:
        env.update(extra_env)
    return subprocess.run(
        [str(GATE)],
        input=payload_for(cmd, session_id=session_id),
        text=True,
        capture_output=True,
        cwd=repo,
        env=env,
    )


def run_stamp(repo: Path, *args: str):
    return subprocess.run(
        [str(STAMP), *args],
        text=True,
        capture_output=True,
        cwd=repo,
        check=True,
    )


class VerifyCommitGateTests(unittest.TestCase):
    def test_scripts_executable_and_hook_wired_in_settings(self):
        for script in (GATE, STAMP):
            self.assertTrue(script.exists(), f"missing {script}")
            mode = script.stat().st_mode
            self.assertTrue(mode & stat.S_IXUSR, f"{script} must be executable")
        settings = json.loads(SETTINGS.read_text(encoding="utf-8"))
        pre_hooks = settings.get("hooks", {}).get("PreToolUse", [])
        commands = [
            h.get("command", "")
            for entry in pre_hooks
            if entry.get("matcher") == "Bash"
            for h in entry.get("hooks", [])
        ]
        self.assertTrue(
            any("verify-commit-gate.sh" in c for c in commands),
            f"verify-commit-gate.sh not wired in .claude/settings.json PreToolUse Bash hooks: {commands}",
        )

    def test_non_commit_commands_always_pass(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            init_repo(repo)
            for cmd in [
                "git status",
                "git diff",
                "make test-fast",
                'echo "remember to run git commit"',
                "grep 'git commit' README.md",
            ]:
                with self.subTest(cmd=cmd):
                    res = run_gate(repo, cmd, mode="enforce")
                    self.assertEqual(res.returncode, 0, res.stderr)
                    self.assertEqual(res.stderr, "")

    def test_off_and_observe_modes(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "hello.txt").write_text("v2\n", encoding="utf-8")

            off = run_gate(repo, 'git commit -m "wip"', mode="off")
            self.assertEqual(off.returncode, 0)
            self.assertEqual(off.stderr, "")

            obs = run_gate(repo, 'git commit -m "wip"', mode="observe")
            self.assertEqual(obs.returncode, 0)
            self.assertIn("[observe]", obs.stderr)
            self.assertIn("no verification stamp found", obs.stderr)

            # Default mode (env unset) behaves as observe.
            env = {k: v for k, v in os.environ.items() if k != "VERIFY_GATE_MODE"}
            default_res = subprocess.run(
                [str(GATE)],
                input=payload_for('git commit -m "wip"'),
                text=True,
                capture_output=True,
                cwd=repo,
                env=env,
            )
            self.assertEqual(default_res.returncode, 0)
            self.assertIn("[observe]", default_res.stderr)

            # Unknown mode falls back safely to observe with a warning.
            unk = run_gate(repo, 'git commit -m "wip"', mode="bogus")
            self.assertEqual(unk.returncode, 0)
            self.assertIn("unknown VERIFY_GATE_MODE=bogus", unk.stderr)

    def test_enforce_stamp_staging_invariance_and_invalidation(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "hello.txt").write_text("v2\n", encoding="utf-8")
            (repo / "new.txt").write_text("brand new\n", encoding="utf-8")

            # Unstamped commit is blocked in enforce mode.
            blocked = run_gate(repo, 'git add hello.txt new.txt && git commit -m "update"', mode="enforce")
            self.assertEqual(blocked.returncode, 2)
            self.assertIn("[block 1/2]", blocked.stderr)

            # Stamp BEFORE `git add`; staging files must NOT invalidate the fingerprint.
            run_stamp(repo, "--write", "make test-fast")
            run_git(repo, "add", "hello.txt", "new.txt")
            allowed = run_gate(repo, 'git commit -m "update"', mode="enforce")
            self.assertEqual(allowed.returncode, 0, allowed.stderr)

            # Editing a file AFTER stamping invalidates the stamp immediately.
            (repo / "hello.txt").write_text("v3-edited-after-verify\n", encoding="utf-8")
            stale = run_gate(repo, 'git -C . commit -m "stale"', mode="enforce")
            self.assertEqual(stale.returncode, 2)
            self.assertIn("worktree changed since last verification", stale.stderr)

    def test_enforce_expired_stamp_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "hello.txt").write_text("v2\n", encoding="utf-8")
            run_stamp(repo, "--write", "make verify")

            stamp_file = Path(run_stamp(repo, "--stamp-path").stdout.strip())
            lines = stamp_file.read_text(encoding="utf-8").splitlines()
            aged = [f"timestamp={int(time.time()) - 7200}" if l.startswith("timestamp=") else l for l in lines]
            stamp_file.write_text("\n".join(aged) + "\n", encoding="utf-8")

            res = run_gate(repo, 'git commit -m "late"', mode="enforce")
            self.assertEqual(res.returncode, 2)
            self.assertIn("older than 3600s", res.stderr)

    def test_verified_by_trailer_satisfies_enforce_and_resets_blocks(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "hello.txt").write_text("v2\n", encoding="utf-8")

            # Trigger 1 block first.
            b1 = run_gate(repo, 'git commit -m "wip"', mode="enforce", session_id="s-trailer")
            self.assertEqual(b1.returncode, 2)

            # Commit with --trailer Verified-By passes and resets the counter.
            ok1 = run_gate(
                repo,
                'git commit -m "docs tweak" --trailer "Verified-By: none (docs-only)"',
                mode="enforce",
                session_id="s-trailer",
            )
            self.assertEqual(ok1.returncode, 0, ok1.stderr)

            # Multiline -m with Verified-By trailer also passes.
            ok2 = run_gate(
                repo,
                'git commit -m "fix: thing\n\nVerified-By: make test-fast"',
                mode="enforce",
                session_id="s-trailer",
            )
            self.assertEqual(ok2.returncode, 0, ok2.stderr)

            # Counter was reset, so the next unverified commit starts at block 1/2 again.
            b_again = run_gate(repo, 'git commit -m "wip2"', mode="enforce", session_id="s-trailer")
            self.assertEqual(b_again.returncode, 2)
            self.assertIn("[block 1/2]", b_again.stderr)

    def test_escape_valve_allows_third_attempt_and_resets(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "hello.txt").write_text("v2\n", encoding="utf-8")

            b1 = run_gate(repo, 'git commit -m "try1"', mode="enforce", session_id="s-escape")
            self.assertEqual(b1.returncode, 2)
            self.assertIn("[block 1/2]", b1.stderr)

            b2 = run_gate(repo, 'git commit -m "try2"', mode="enforce", session_id="s-escape")
            self.assertEqual(b2.returncode, 2)
            self.assertIn("[block 2/2]", b2.stderr)

            # 3rd attempt triggers the escape valve and exits 0.
            esc = run_gate(repo, 'git commit -m "try3"', mode="enforce", session_id="s-escape")
            self.assertEqual(esc.returncode, 0)
            self.assertIn("escape valve triggered after 2 consecutive blocks", esc.stderr)

            # Counter resets after escape valve fires.
            b_next = run_gate(repo, 'git commit -m "try4"', mode="enforce", session_id="s-escape")
            self.assertEqual(b_next.returncode, 2)
            self.assertIn("[block 1/2]", b_next.stderr)

    def test_multiline_and_semicolon_commit_boundaries_detected(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "hello.txt").write_text("v2\n", encoding="utf-8")

            for idx, cmd in enumerate([
                'git add hello.txt\ngit commit -m "multiline"',
                'git add hello.txt;git commit -m "semicolon"',
            ]):
                with self.subTest(cmd=cmd):
                    res = run_gate(repo, cmd, mode="enforce", session_id=f"s-sep-{idx}")
                    self.assertEqual(res.returncode, 2, f"expected commit gate to catch {cmd!r}")

    def test_unicode_filename_edit_invalidates_stamp(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            init_repo(repo)
            uni = repo / "é.txt"
            uni.write_text("v1\n", encoding="utf-8")
            run_git(repo, "add", "é.txt")
            run_git(repo, "commit", "-q", "-m", "add unicode file")

            uni.write_text("v2\n", encoding="utf-8")
            run_stamp(repo, "--write", "make test-fast")
            run_git(repo, "add", "é.txt")
            ok = run_gate(repo, 'git commit -m "verified unicode"', mode="enforce")
            self.assertEqual(ok.returncode, 0, ok.stderr)

            # Modifying the Unicode file after stamping must invalidate the stamp.
            uni.write_text("v3\n", encoding="utf-8")
            run_git(repo, "add", "é.txt")
            stale = run_gate(repo, 'git commit -m "unverified unicode"', mode="enforce")
            self.assertEqual(stale.returncode, 2)
            self.assertIn("worktree changed since last verification", stale.stderr)

    def test_partial_staging_mismatch_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            init_repo(repo)

            # Stage v2, then edit worktree to v3 and stamp v3.
            (repo / "hello.txt").write_text("v2-staged\n", encoding="utf-8")
            run_git(repo, "add", "hello.txt")
            (repo / "hello.txt").write_text("v3-worktree\n", encoding="utf-8")
            run_stamp(repo, "--write", "make test-fast")

            # Committing now would record unverified v2, so enforce mode must reject it.
            res = run_gate(repo, 'git commit -m "partial"', mode="enforce")
            self.assertEqual(res.returncode, 2)
            self.assertIn("partial staging", res.stderr)

            # Re-staging v3 aligns index with verified worktree and passes.
            run_git(repo, "add", "hello.txt")
            ok = run_gate(repo, 'git commit -m "aligned"', mode="enforce")
            self.assertEqual(ok.returncode, 0, ok.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
