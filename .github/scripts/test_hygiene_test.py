#!/usr/bin/env python3
"""Ratchet guard for time.Sleep(...) and t.Skip*(...) calls in Go tests.

Ad-hoc sleeps and silent skips train coding agents to hide flaky tests instead
of fixing root causes (plan 75 slice 4). Existing occurrences are recorded in
test_hygiene_baseline.json per file so this guard:

  - fails on any NEW time.Sleep(...) or .Skip*(...) above a file's baseline
    (or in any new *_test.go file) unless the line carries `// hygiene:allow`
    with a brief justification;
  - notices when a file drops below baseline so the baseline can be tightened;
  - supports TEST_HYGIENE_MODE=observe (warn only) vs enforce (default).

Run: make test-hygiene-test
"""
import json
import os
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
BASELINE_PATH = pathlib.Path(__file__).resolve().with_name("test_hygiene_baseline.json")

SLEEP_RE = re.compile(r"\btime\.Sleep\s*\(")
SKIP_RE = re.compile(r"\.(?:Skip|Skipf|SkipNow)\s*\(")
ALLOW_RE = re.compile(r"//\s*hygiene:allow\b")


def strip_go_comments_and_strings(source: str) -> list[str]:
    """Return per-line Go code with comments and string/rune literals blanked.

    Lines carrying `// hygiene:allow` are cleared completely so justified uses
    are explicitly exempted without weakening the baseline for the rest of the
    file.
    """
    out_lines: list[str] = []
    in_block = False
    in_raw = False
    for raw_line in source.splitlines():
        if not in_block and not in_raw and ALLOW_RE.search(raw_line):
            out_lines.append("")
            continue
        buf: list[str] = []
        i = 0
        n = len(raw_line)
        in_str = False
        in_rune = False
        esc = False
        while i < n:
            ch = raw_line[i]
            nxt = raw_line[i + 1] if i + 1 < n else ""
            if in_block:
                if ch == "*" and nxt == "/":
                    in_block = False
                    i += 2
                else:
                    i += 1
                continue
            if in_raw:
                if ch == "`":
                    in_raw = False
                i += 1
                continue
            if in_str:
                if esc:
                    esc = False
                elif ch == "\\":
                    esc = True
                elif ch == '"':
                    in_str = False
                i += 1
                continue
            if in_rune:
                if esc:
                    esc = False
                elif ch == "\\":
                    esc = True
                elif ch == "'":
                    in_rune = False
                i += 1
                continue
            if ch == "/" and nxt == "/":
                break
            if ch == "/" and nxt == "*":
                in_block = True
                i += 2
                continue
            if ch == "`":
                in_raw = True
                i += 1
                continue
            if ch == '"':
                in_str = True
                i += 1
                continue
            if ch == "'":
                in_rune = True
                i += 1
                continue
            buf.append(ch)
            i += 1
        out_lines.append("".join(buf))
    return out_lines


def count_source(source: str) -> tuple[int, int]:
    sleep_n = 0
    skip_n = 0
    for code in strip_go_comments_and_strings(source):
        sleep_n += len(SLEEP_RE.findall(code))
        skip_n += len(SKIP_RE.findall(code))
    return sleep_n, skip_n


def tracked_test_files() -> list[pathlib.Path]:
    out = subprocess.check_output(
        ["git", "ls-files", "*_test.go"], cwd=ROOT, text=True
    )
    paths = [ROOT / line for line in out.splitlines() if line.strip()]
    # Include untracked *_test.go files under internal/, cmd/, tools/, acceptance/, evals/
    # so a brand-new test file in a working tree is checked before git add too.
    seen = {p.resolve() for p in paths}
    for top in ("internal", "cmd", "tools", "acceptance", "evals"):
        d = ROOT / top
        if not d.exists():
            continue
        for p in d.rglob("*_test.go"):
            rp = p.resolve()
            if rp not in seen:
                seen.add(rp)
                paths.append(p)
    return sorted(paths, key=lambda p: p.relative_to(ROOT).as_posix())


def scan_repo() -> dict[str, dict[str, int]]:
    counts: dict[str, dict[str, int]] = {}
    for p in tracked_test_files():
        if not p.exists():
            continue
        s, k = count_source(p.read_text(encoding="utf-8"))
        if s or k:
            counts[p.relative_to(ROOT).as_posix()] = {"sleep": s, "skip": k}
    return counts


def self_test() -> None:
    sample = '''package x
func TestSample(t *testing.T) {
    // time.Sleep(time.Second) t.Skip("commented")
    /* time.Sleep(1)
       t.SkipNow() */
    _ = "time.Sleep(1) t.Skipf(`x`)"
    _ = `time.Sleep(1)
t.Skip("raw")`
    time.Sleep(time.Millisecond) // hygiene:allow intentional backoff test
    t.Skip("opt-in")             // hygiene:allow live tier opt-in
    time.Sleep(2 * time.Millisecond)
    t.Skipf("bad %d", 1)
    t.SkipNow()
}
'''
    s, k = count_source(sample)
    assert (s, k) == (1, 2), f"self-test failed: got {(s, k)}, want (1, 2)"
    print("detector self-test: ok")


def main() -> int:
    self_test()
    counts = scan_repo()
    if "--write-baseline" in sys.argv:
        BASELINE_PATH.write_text(
            json.dumps(counts, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        print(f"wrote baseline: {len(counts)} files")
        return 0

    if not BASELINE_PATH.exists():
        print(f"missing baseline file {BASELINE_PATH}", file=sys.stderr)
        return 1
    baseline: dict[str, dict[str, int]] = json.loads(
        BASELINE_PATH.read_text(encoding="utf-8")
    )

    mode = os.environ.get("TEST_HYGIENE_MODE", "enforce").strip().lower()
    if mode not in ("enforce", "observe"):
        print(f"invalid TEST_HYGIENE_MODE={mode!r} (want enforce or observe)", file=sys.stderr)
        return 1

    regressions: list[str] = []
    improvements: list[str] = []
    for path in sorted(set(counts) | set(baseline)):
        cur = counts.get(path, {"sleep": 0, "skip": 0})
        base = baseline.get(path, {"sleep": 0, "skip": 0})
        for kind in ("sleep", "skip"):
            if cur[kind] > base[kind]:
                regressions.append(
                    f"{path}: {kind} grew from {base[kind]} to {cur[kind]} "
                    f"(use synchronization/fake clock or annotate with `// hygiene:allow <reason>`)"
                )
            elif cur[kind] < base[kind]:
                improvements.append(
                    f"{path}: {kind} dropped from {base[kind]} to {cur[kind]}"
                )

    total_sleep = sum(v["sleep"] for v in counts.values())
    total_skip = sum(v["skip"] for v in counts.values())
    if improvements:
        print(
            f"note: {len(improvements)} file(s) improved below baseline "
            "(run `python3 .github/scripts/test_hygiene_test.py --write-baseline` to ratchet down)"
        )
    if regressions:
        header = "WARN (observe mode)" if mode == "observe" else "FAIL"
        print(f"{header}: {len(regressions)} test-hygiene regression(s):", file=sys.stderr)
        for r in regressions:
            print(f"  - {r}", file=sys.stderr)
        return 0 if mode == "observe" else 1

    print(
        f"ok — {len(counts)} baselined test files ({total_sleep} sleep, {total_skip} skip); "
        f"0 regressions (mode={mode})"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
