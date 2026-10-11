#!/usr/bin/env python3
"""PreToolUse guard: refuse a Bash command whose `git push` would update main.

Every change lands through a branch, a PR and a squash merge (CLAUDE.md,
"Iteration workflow"), so no session needs to push to main or master. A session
that runs with permission prompts off has nothing else between a mistaken push
and the default branch.

The guard reads the command text. It catches mistakes, not intent: a push run
from a script or an alias goes through unseen. It reads the PreToolUse payload
on stdin. It exits 2 with the reason on stderr to refuse the call, and exits 0
otherwise, also for a payload it cannot read, because blocking every Bash call
is worse than missing one push. `make push-guard-test` runs its table.
"""

import json
import os
import re
import shlex
import subprocess
import sys

PROTECTED = {"main", "master"}
# Push flags that update every local branch, so main as well.
EVERY_BRANCH = {"--all", "--branches", "--mirror"}
# Options whose value is the next word.
GIT_VALUE_OPTIONS = {"-C", "-c"}
PUSH_VALUE_OPTIONS = {"-o", "--push-option", "--repo", "--receive-pack", "--exec"}
# Words that run the command after them.
WRAPPERS = {"command", "env", "exec", "nohup", "sudo", "time"}
ASSIGNMENT = re.compile(r"[A-Za-z_][A-Za-z0-9_]*=.*")
SEPARATORS = set("();<>|&")
# The fallback for a command shlex cannot split, such as one with an unbalanced
# quote: a push on one line that names a protected branch as a word of its own.
LOOSE = re.compile(
    r"\bgit\b[^;&|\n]*\bpush\b[^;&|\n]*(?<![\w/.-])(?:refs/heads/)?(?:main|master)(?![\w/.-])"
)


def current_branch(directory):
    """Return the branch checked out in directory, or "" when there is none."""
    try:
        result = subprocess.run(
            ["git", "-C", directory, "symbolic-ref", "--short", "-q", "HEAD"],
            capture_output=True, text=True, timeout=10,
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    return result.stdout.strip()


def simple_commands(command):
    """Split command into the words of each simple command. Raises ValueError."""
    lexer = shlex.shlex(command.replace("\n", " ; "), posix=True, punctuation_chars=True)
    lexer.whitespace_split = True
    commands, words = [], []
    for token in lexer:
        if token and set(token) <= SEPARATORS:
            commands.append(words)
            words = []
        else:
            words.append(token)
    commands.append(words)
    return [words for words in commands if words]


def refusal(command, cwd, branch_of=current_branch):
    """Return why command must not run, or None when it may."""
    try:
        commands = simple_commands(command)
    except ValueError:
        if LOOSE.search(command):
            return "this `git push` names main or master"
        return None
    for words in commands:
        if words[0] == "cd" and len(words) > 1:
            cwd = os.path.join(cwd, os.path.expanduser(words[1]))
            continue
        reason = push_refusal(words, cwd, branch_of)
        if reason:
            return reason
    return None


def push_refusal(words, cwd, branch_of):
    """Return why one simple command's push must not run, or None."""
    i = 0
    while i < len(words) and (words[i] in WRAPPERS or ASSIGNMENT.fullmatch(words[i])):
        i += 1
    if i == len(words) or os.path.basename(words[i]) != "git":
        return None
    i += 1
    directory = cwd
    while i < len(words) and words[i].startswith("-"):
        if words[i] in GIT_VALUE_OPTIONS and i + 1 < len(words):
            if words[i] == "-C":
                directory = os.path.join(directory, os.path.expanduser(words[i + 1]))
            i += 2
        else:
            i += 1
    if i == len(words) or words[i] != "push":
        return None

    flags, positional = set(), []
    args = words[i + 1:]
    k = 0
    while k < len(args):
        arg = args[k]
        if arg == "--":
            positional += args[k + 1:]
            break
        if arg.startswith("-"):
            flags.add(arg.split("=", 1)[0])
            k += 2 if arg in PUSH_VALUE_OPTIONS else 1
            continue
        positional.append(arg)
        k += 1

    if flags & EVERY_BRANCH:
        return "this `git push` updates every branch, main included"
    refspecs = positional[1:]
    if not refspecs:
        if "--tags" in flags:
            return None
        refspecs = ["HEAD"]
    for refspec in refspecs:
        destination = refspec.lstrip("+")
        if ":" in destination:
            destination = destination.split(":", 1)[1]
        if destination in ("HEAD", "@"):
            destination = branch_of(directory)
        destination = destination.removeprefix("refs/heads/")
        if destination in PROTECTED:
            return f"this `git push` would update {destination}"
    return None


def main():
    try:
        payload = json.load(sys.stdin)
        command = payload["tool_input"]["command"]
        cwd = payload.get("cwd") or os.getcwd()
    except (ValueError, KeyError, TypeError, AttributeError):
        return 0
    if not isinstance(command, str) or not isinstance(cwd, str):
        return 0
    reason = refusal(command, cwd)
    if reason is None:
        return 0
    print(
        f"push-main guard: {reason}. Every change lands through a branch, a PR and a "
        "squash merge (CLAUDE.md, \"Iteration workflow\"): push your branch and open a PR.",
        file=sys.stderr,
    )
    return 2


if __name__ == "__main__":
    sys.exit(main())
