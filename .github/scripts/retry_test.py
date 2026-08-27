#!/usr/bin/env python3
"""Runs the `retry` helper the CD notifiers define, instead of reading it.

`make retry-test`.

`deploy-alert.yml` and `staging-parked.yml` each wrap their READ calls in a
three-attempt `retry`, and every caller captures its output. The obvious spelling
of the helper — `until "$@"` — runs each attempt with stdout already connected to
that capture, so an attempt that emits bytes before failing leaves them in it and
the caller parses them joined to the next attempt's. That is #507, filed against
`deploy-alert.yml` after review caught the same line in `staging-parked.yml`.

The two were deliberately NOT extracted into a shared script — unlike
`parked.sh`, where the two callers disagreeing about one cluster would make one of
them a liar, two notifiers retrying a different number of times harms nothing.
That argument holds only while both are correct, and this file is what holds it:
it finds every `retry` definition under `.github/workflows/` and runs all of them
through the same table, so a third copy is covered the day it appears and no copy
can regress alone.

The extraction is textual because the definition lives inside a workflow's `run:`
block, where nothing else can reach it: there is no `.sh` file to source and, by
the decision above, there should not be one. `workflow_run` runs the default
branch's copy of these files, so this table is also the only thing that executes
either helper before it is already live.
"""

import os
import pathlib
import subprocess
import sys
import tempfile

WORKFLOWS = pathlib.Path(__file__).resolve().parents[1] / "workflows"

# Every failing attempt writes this to stderr. It must reach the job log — that
# is where an operator reads why a call was retried — and must never reach the
# caller's captured value, which is parsed as JSON or as an issue number.
STDERR_MARK = "RETRY-STDERR-MARK"

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

# The helper sleeps 5s then 10s between attempts. A no-op shadow keeps this
# instant without editing the workflow, whose text is the thing under test.
sleep() {{ :; }}

{definition}

rc=0
captured="$(retry "$@")" || rc=$?
printf '%s' "$captured" >"$CAPTURE"
exit "$rc"
"""

FLAKY = """#!/usr/bin/env bash
# One attempt. It emits its bytes BEFORE failing, which is the entire point: a
# truncated response and a proxy error page both do exactly that.
n=$(($(cat "$COUNTER") + 1))
printf '%s' "$n" >"$COUNTER"
if [ "$n" -le "$FAIL_TIMES" ]; then
  printf '%s' "$FAIL_OUT"
  printf '%s\\n' "$STDERR_MARK" >&2
  exit 1
fi
printf '%s' "$OK_OUT"
"""


def definitions(path):
    """Every `retry() { ... }` in `path`, dedented into a runnable function."""
    lines = path.read_text(encoding="utf-8").splitlines()
    found = []
    for i, line in enumerate(lines):
        if line.strip() != "retry() {":
            continue
        indent = line[: len(line) - len(line.lstrip())]
        body = ["retry() {"]
        for lineno, nxt in enumerate(lines[i + 1:], start=i + 2):
            if not nxt.strip():
                body.append("")
            elif nxt.startswith(indent):
                body.append(nxt[len(indent):])
            else:
                # A YAML block scalar cannot hold a line less indented than its
                # own body, so this is a broken extraction, not a style choice.
                raise SystemExit(
                    f"{path.name}:{lineno}: dedents out of the retry() opened"
                    f" at line {i + 1}"
                )
            if nxt == indent + "}":
                break
        else:
            raise SystemExit(
                f"{path.name}:{i + 1}: retry() never closes at its own indent"
            )
        found.append((f"{path.name}:{i + 1}", "\n".join(body)))
    return found


def run_case(workdir, definition, case):
    _, fail_times, fail_out, ok_out, _, _, _, _ = case
    counter = workdir / "counter"
    counter.write_text("0", encoding="utf-8")
    capture = workdir / "capture"
    capture.write_text("", encoding="utf-8")
    driver = workdir / "driver.sh"
    driver.write_text(DRIVER.format(definition=definition), encoding="utf-8")

    env = {
        **os.environ,
        "COUNTER": str(counter),
        "CAPTURE": str(capture),
        "FAIL_TIMES": str(fail_times),
        "FAIL_OUT": fail_out,
        "OK_OUT": ok_out,
        "STDERR_MARK": STDERR_MARK,
    }
    proc = subprocess.run(
        ["bash", str(driver), str(workdir / "flaky")],
        capture_output=True, text=True, check=False, env=env,
    )
    return (
        capture.read_text(encoding="utf-8"),
        proc.returncode,
        int(counter.read_text(encoding="utf-8")),
        proc.stderr,
    )


def check(workdir, name, definition):
    failures = []
    for case in CASES:
        label, fail_times, _, _, want_out, want_rc, want_attempts, why = case
        got_out, got_rc, attempts, stderr = run_case(workdir, definition, case)
        if (got_out, got_rc, attempts) != (want_out, want_rc, want_attempts):
            failures.append(
                f"  {name} / {label}: captured {got_out!r} (exit {got_rc},"
                f" {attempts} attempt(s)), want {want_out!r} (exit {want_rc},"
                f" {want_attempts}) — {why}"
            )
        if fail_times and STDERR_MARK not in stderr:
            failures.append(
                f"  {name} / {label}: a failed attempt's stderr never reached"
                " the job log — nobody can see why the call was retried"
            )
        if STDERR_MARK in got_out:
            failures.append(
                f"  {name} / {label}: a failed attempt's stderr was captured"
                " into the value the caller parses"
            )
    return failures


def main():
    helpers = []
    for path in sorted(WORKFLOWS.glob("*.yml")):
        helpers += definitions(path)

    # Two copies is the state this file was written for. Fewer means either a
    # helper was reformatted past the scan above — which would leave it silently
    # unchecked — or the two really were consolidated into one script, in which
    # case this number is what says so out loud.
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
        flaky = workdir / "flaky"
        flaky.write_text(FLAKY, encoding="utf-8")
        flaky.chmod(0o755)
        for name, definition in helpers:
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
