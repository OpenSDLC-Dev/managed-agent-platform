#!/usr/bin/env python3
"""Runs the `retry` helper the CD notifiers define, instead of reading it.

`make retry-test`.

`deploy-alert.yml` and `staging-parked.yml` each wrap their READ calls in a
three-attempt `retry`, and every caller captures what it prints. The obvious
spelling of the helper — `until "$@"` — runs each attempt with stdout already
connected to that capture, so an attempt that emits bytes before failing leaves
them in it and the caller parses them joined to the next attempt's. That is #507,
filed against `deploy-alert.yml` after review caught the same line in
`staging-parked.yml`.

The two were deliberately NOT extracted into a shared script — unlike
`parked.sh`, where the two callers disagreeing about one cluster would make one of
them a liar, two notifiers retrying a different number of times harms nothing.
That argument holds only while both are correct, and this file is what holds it:
it finds every `retry` definition under `.github/workflows/` and runs all of them
through the same table, so a third copy is covered the day it appears and no copy
can regress alone.

The extraction is textual because the definition lives inside a workflow's `run:`
block, where nothing else can reach it: there is no `.sh` file to source and, by
the decision above, there should not be one. `deploy-alert.yml` cannot be run
from a pull request at all — `workflow_run` executes the DEFAULT branch's copy —
so for that file this table is the only thing that runs the helper before it is
already live. `staging-parked.yml` adds `workflow_dispatch`, which would run a
branch's copy, but only against real staging; this table is still where both are
exercised in anger.

**A helper this cannot parse is refused, not skipped.** A scanner that quietly
passes over one is worse than no scanner, because CI then reports success over an
unchecked copy. So the detector is deliberately looser than the parser: both
workflow extensions are searched, five spellings parse, and anything else that
looks like a `retry` definition — a brace on the next line, a subshell body, a
second statement sharing the line — is a hard error naming file and line rather
than a quiet skip. Extraction is restricted to literal `run: |` scalars for the
same reason, and every definition survives `bash -n` before it runs, so a cut at
the wrong `}` reports a syntax error instead of testing a fragment.

No regex recognises every way of writing a shell function, so that refusal is a
backstop rather than a proof. If you are adding a helper and this file refuses
it, the fix is to spell it the way the other two are spelled, or to teach `DEFN`
the new shape — not to quietly narrow `LOOKS_LIKE` until the refusal stops.
"""

import os
import pathlib
import re
import subprocess
import sys
import tempfile

WORKFLOWS = pathlib.Path(__file__).resolve().parents[1] / "workflows"

# Every failing attempt writes this to stderr. It must reach the job log — that
# is where an operator reads why a call was retried — and must never reach the
# caller's captured value, which is parsed as JSON or as an issue number.
STDERR_MARK = "RETRY-STDERR-MARK"

# The arguments the fake command insists on receiving. `retry "$@"` forwards all
# of them, and every real call site passes four or more (`gh api URL --jq …`); a
# helper that forwarded only `"$1"` would otherwise pass every row below.
ARGV = ["--marker", "arg with spaces"]

# What the helper is expected to wait between attempts. The driver shadows
# `sleep` so the suite stays instant, and that shadow would hide a backoff of
# 5000 seconds as readily as one of 5 — so it records what it was asked for.
BACKOFF = [5, 10]

# `run:` introducing a block scalar, which is the only shape this can read. A
# folded or quoted scalar reaches Actions with its newlines rewritten, so the
# text here would not be the text that runs; that case is refused below rather
# than guessed at.
BLOCK_RUN = re.compile(r"^(?P<indent>[ \t]*)run:[ \t]*\|[-+]?[ \t]*$")

# `retry() {`, `retry () {`, `function retry {`, `function retry() {`, each with
# an optional trailing comment.
DEFN = re.compile(
    r"^(?P<indent>[ \t]*)(?:function[ \t]+)?retry[ \t]*(?:\([ \t]*\))?[ \t]*\{[ \t]*(?:#.*)?$"
)
# Deliberately looser than DEFN, and it has to stay that way: it must match the
# shapes DEFN CANNOT parse, because a line it matches that was not extracted is
# refused by name and line instead of skipped. The gap it exists to close was
# real — an Allman brace on the next line, a subshell body (`retry() ( … )`), and
# a second statement sharing the line were all silently unchecked until it did.
LOOKS_LIKE = re.compile(
    r"(?:^|;)[ \t]*(?:function[ \t]+)?retry[ \t]*(?:\([ \t]*\))?[ \t]*[({]?[ \t]*(?:#.*)?$"
)


def looks_like_a_definition(line):
    """Does this line open something named `retry`, however it is spelled?"""
    m = LOOKS_LIKE.search(line)
    if not m:
        return False
    # A bare `retry` alone on a line is a CALL taking no arguments: it matches
    # the pattern and defines nothing. A bracket, or the `function` keyword, is
    # what tells the two apart without narrowing the shapes that matter.
    return any(c in m.group(0) for c in "({") or "function" in m.group(0)

# name, attempts that fail, bytes each failure emits, bytes the success emits,
# what the caller must end up with, exit status, attempts made, and what the row
# stands for in the notifiers.
CASES = [
    ("partial-then-good", 2, '{"partial":', '{"ok":1}', '{"ok":1}', 0, 3,
     "a truncated body before a 5xx — the defect itself: bare, the caller reads"
     ' \'{"partial":{"partial":{"ok":1}\''),
    ("clean-first-try", 0, "", '{"ok":1}', '{"ok":1}', 0, 1,
     "the ordinary path — buffering must not eat the answer"),
    ("all-attempts-fail", 3, '{"partial":', "", "", 1, 3,
     "the giving-up path: the caller gets a failure and NOT the junk"),
    ("empty-answer", 0, "", "", "", 0, 1,
     "`gh issue list --jq 'first | .number // empty'` with nothing open — an"
     " empty answer is a real answer here"),
    ("empty-answer-after-failures", 2, "123", "", "", 0, 3,
     "the same lookup after two failures. Bare, the caller reads `123123` and"
     " comments on an issue nobody filed"),
    ("multi-line", 1, "x", "line one\nline two", "line one\nline two", 0, 2,
     "a multi-line body — `--jq` can emit one line per issue"),
    ("shell-hostile", 0, "", "* a  b *", "* a  b *", 0, 1,
     "a value a bare `echo $out` would glob-expand and re-split"),
]

DRIVER = """set -euo pipefail

# The helper sleeps 5s then 10s between attempts. This shadow keeps the suite
# instant without editing the workflow, whose text is the thing under test — and
# records what it was asked to wait, so a backoff quietly changed to 5000 is a
# failed row rather than a test that still passes in a millisecond.
sleep() {{ printf '%s\\n' "$1" >>"$SLEEPS"; }}

{definition}

rc=0
captured="$(retry "$@")" || rc=$?
printf '%s' "$captured" >"$CAPTURE"
exit "$rc"
"""

FLAKY = """#!/usr/bin/env bash
# One attempt. It emits its bytes BEFORE failing, which is the entire point: a
# truncated response and a proxy error page both do exactly that.
if [ "$1" != "--marker" ] || [ "$2" != "arg with spaces" ]; then
  printf 'ARGS-LOST(%s)' "$*"
  exit 1
fi
n=$(($(cat "$COUNTER") + 1))
printf '%s' "$n" >"$COUNTER"
if [ "$n" -le "$FAIL_TIMES" ]; then
  printf '%s' "$FAIL_OUT"
  printf '%s\\n' "$STDERR_MARK" >&2
  exit 1
fi
printf '%s' "$OK_OUT"
"""


def block_scalars(lines):
    """The `run: |` regions of a workflow, dedented, as (first_line_no, lines).

    Only literal block scalars. A folded or quoted `run:` reaches Actions with
    its newlines rewritten, so its text is not the text that runs.
    """
    regions = []
    i = 0
    while i < len(lines):
        m = BLOCK_RUN.match(lines[i])
        if not m:
            i += 1
            continue
        key_indent = len(m.group("indent"))
        body, j = [], i + 1
        while j < len(lines):
            line = lines[j]
            if line.strip() and (len(line) - len(line.lstrip())) <= key_indent:
                break
            body.append(line)
            j += 1
        strip = min(
            (len(x) - len(x.lstrip()) for x in body if x.strip()),
            default=0,
        )
        regions.append((i + 2, [x[strip:] if x.strip() else "" for x in body]))
        i = j
    return regions


def definitions(path):
    """Every `retry` definition in `path`, dedented into a runnable function."""
    lines = path.read_text(encoding="utf-8").splitlines()
    found, covered = [], set()

    for first, body in block_scalars(lines):
        for i, line in enumerate(body):
            m = DEFN.match(line)
            if not m:
                continue
            indent = m.group("indent")
            close = re.compile(r"^" + re.escape(indent) + r"\}[ \t]*(?:#.*)?$")
            out, end = [line[len(indent):]], None
            for k in range(i + 1, len(body)):
                nxt = body[k]
                out.append(nxt[len(indent):] if nxt.startswith(indent) else nxt.lstrip())
                if close.match(nxt):
                    end = k
                    break
            if end is None:
                raise SystemExit(
                    f"{path.name}:{first + i}: this retry definition never closes"
                    " at its own indentation"
                )
            covered.update(range(first + i, first + end + 1))
            found.append((f"{path.name}:{first + i}", "\n".join(out)))

    # Anything retry-shaped that the loop above did not take is a hard error.
    # Silently skipping one is the failure this whole file exists to prevent:
    # CI would report success over an unchecked copy.
    for n, line in enumerate(lines, start=1):
        if looks_like_a_definition(line) and n not in covered:
            raise SystemExit(
                f"{path.name}:{n}: this looks like a `retry` definition but is not"
                " inside a `run: |` block this test can read, or is spelled in a"
                " way it does not recognise. It would go UNTESTED — move it into"
                " a literal block scalar and spell it the way the other helpers"
                " are, or teach DEFN/BLOCK_RUN the new shape."
            )
    return found


def run_case(workdir, definition, case):
    _, fail_times, fail_out, ok_out, _, _, _, _ = case
    counter = workdir / "counter"
    counter.write_text("0", encoding="utf-8")
    capture = workdir / "capture"
    capture.write_text("", encoding="utf-8")
    sleeps = workdir / "sleeps"
    sleeps.write_text("", encoding="utf-8")
    driver = workdir / "driver.sh"
    driver.write_text(DRIVER.format(definition=definition), encoding="utf-8")

    env = {
        **os.environ,
        "COUNTER": str(counter),
        "CAPTURE": str(capture),
        "SLEEPS": str(sleeps),
        "FAIL_TIMES": str(fail_times),
        "FAIL_OUT": fail_out,
        "OK_OUT": ok_out,
        "STDERR_MARK": STDERR_MARK,
    }
    proc = subprocess.run(
        ["bash", str(driver), "bash", str(workdir / "flaky"), *ARGV],
        capture_output=True, text=True, check=False, env=env,
    )
    # Kept as text on purpose: a helper spelled `sleep 0.5`, or `sleep $BACKOFF`
    # with the variable unset, would otherwise raise out of the harness with a
    # traceback instead of failing its row like every other assertion.
    waited = sleeps.read_text(encoding="utf-8").split()
    return (
        capture.read_text(encoding="utf-8"),
        proc.returncode,
        int(counter.read_text(encoding="utf-8")),
        proc.stderr,
        waited,
    )


def check(workdir, name, definition):
    failures = []
    for case in CASES:
        label, fail_times, _, _, want_out, want_rc, want_attempts, why = case
        got_out, got_rc, attempts, stderr, waited = run_case(workdir, definition, case)
        if (got_out, got_rc, attempts) != (want_out, want_rc, want_attempts):
            failures.append(
                f"  {name} / {label}: captured {got_out!r} (exit {got_rc},"
                f" {attempts} attempt(s)), want {want_out!r} (exit {want_rc},"
                f" {want_attempts}) — {why}"
            )
        # One marker per failed attempt, not merely one overall: a helper that
        # kept stderr for the first failure and dropped it thereafter would
        # otherwise satisfy every row while blinding the operator to the rest.
        want_marks = fail_times
        got_marks = stderr.count(STDERR_MARK)
        if got_marks != want_marks:
            failures.append(
                f"  {name} / {label}: {got_marks} failed-attempt diagnostic(s)"
                f" reached the job log, want {want_marks} — one per failure, or"
                " nobody can see why the call was retried"
            )
        if STDERR_MARK in got_out:
            failures.append(
                f"  {name} / {label}: a failed attempt's stderr was captured"
                " into the value the caller parses"
            )
        want_waits = [str(s) for s in BACKOFF[: min(fail_times, len(BACKOFF))]]
        if waited != want_waits:
            failures.append(
                f"  {name} / {label}: waited [{', '.join(waited)}] between"
                f" attempts, want [{', '.join(want_waits)}] — the backoff is"
                " what keeps a retry from hammering an API that is already"
                " unwell"
            )
    return failures


def main():
    helpers = []
    for path in sorted(WORKFLOWS.glob("*.yml")) + sorted(WORKFLOWS.glob("*.yaml")):
        helpers += definitions(path)

    # Two copies is the state this file was written for. Fewer means one was
    # deleted, or the two were consolidated into a script after all, and this
    # floor is what says so out loud. It is not the discovery check — that is
    # the refusal in definitions(), which catches the spellings the parser does
    # not handle. Neither is a proof: no regex recognises every way of writing a
    # shell function, so the floor and the refusal are two backstops, not one
    # guarantee.
    if len(helpers) < 2:
        print(
            f"expected at least 2 `retry` helpers under {WORKFLOWS}, found"
            f" {len(helpers)}: {[n for n, _ in helpers]}",
            file=sys.stderr,
        )
        return 1

    failures = []
    with tempfile.TemporaryDirectory() as tmp:
        workdir = pathlib.Path(tmp)
        (workdir / "flaky").write_text(FLAKY, encoding="utf-8")
        for name, definition in helpers:
            # Before running it: does it even parse? A `}` matched inside a
            # heredoc would otherwise hand bash a fragment, and the rows would
            # all fail with something that reads like a behaviour change.
            syntax = subprocess.run(
                ["bash", "-n"], input=definition,
                capture_output=True, text=True, check=False,
            )
            if syntax.returncode != 0:
                failures.append(
                    f"  {name}: the extracted text is not valid shell —"
                    f" {syntax.stderr.strip()}. The definition was probably cut"
                    " at the wrong `}`."
                )
                continue
            failures += check(workdir, name, definition)

    if failures:
        print(f"retry helper: {len(failures)} wrong answer(s):", file=sys.stderr)
        print("\n".join(failures), file=sys.stderr)
        return 1
    print(
        f"ok: the {len(helpers)} `retry` helper(s) —"
        f" {', '.join(n for n, _ in helpers)} — pass all {len(CASES)} rows,"
        " and none leaks a failed attempt's output into the caller"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
