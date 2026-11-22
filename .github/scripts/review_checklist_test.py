#!/usr/bin/env python3
"""Validate REVIEW.md's diff-triggered checklist and match items against a diff.

Modes:
  1. Default (`make review-checklist-test`): runs built-in self-tests and
     validates REVIEW.md's structure (unique Rxx IDs, non-empty Trigger/Check/
     Incident fields, and non-empty gate-audit patterns).
  2. `--diff <base_ref>`: prints the exact checklist items triggered by files
     changed relative to `<base_ref>` plus an explicit audit warning whenever a
     PR modifies its own verification gates (.claude/, REVIEW.md, Makefile,
     ci.yml, or test_hygiene_baseline.json).

Run: make review-checklist-test
"""
import argparse
import fnmatch
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
REVIEW_MD = ROOT / "REVIEW.md"

ITEM_HEADER_RE = re.compile(r"^### (R\d{2}) — (.+)$")
FIELD_RE = re.compile(r"^- \*\*(Trigger|Check|Incident):\*\* (.+)$")
BACKTICK_RE = re.compile(r"`([^`]+)`")

GATE_PATHS = (
    ".claude/",
    ".codex/",
    ".agents/",
    "harness/",
    "tools/harnessdoctor/",
    "tools/harnesshook/",
    "AGENTS.md",
    "REVIEW.md",
    "Makefile",
    ".github/workflows/ci.yml",
    ".github/scripts/test_hygiene_baseline.json",
)


def parse_checklist(text: str) -> list[dict]:
    items: list[dict] = []
    cur: dict | None = None
    for lineno, line in enumerate(text.splitlines(), 1):
        m = ITEM_HEADER_RE.match(line)
        if m:
            if cur is not None:
                items.append(cur)
            cur = {
                "id": m.group(1),
                "title": m.group(2).strip(),
                "line": lineno,
                "triggers": [],
                "check": "",
                "incident": "",
            }
            continue
        if cur is not None:
            if line.startswith("## "):
                items.append(cur)
                cur = None
                continue
            fm = FIELD_RE.match(line)
            if fm:
                field, val = fm.group(1), fm.group(2).strip()
                if field == "Trigger":
                    cur["triggers"] = BACKTICK_RE.findall(val)
                elif field == "Check":
                    cur["check"] = val
                elif field == "Incident":
                    cur["incident"] = val
    if cur is not None:
        items.append(cur)
    return items


def validate_items(items: list[dict]) -> list[str]:
    errs: list[str] = []
    if len(items) < 10:
        errs.append(f"expected at least 10 checklist items, found {len(items)}")
    seen: set[str] = set()
    for it in items:
        if it["id"] in seen:
            errs.append(f"duplicate item id {it['id']} at line {it['line']}")
        seen.add(it["id"])
        if not it["triggers"]:
            errs.append(f"{it['id']} (line {it['line']}) has no backtick-quoted triggers")
        if not it["check"]:
            errs.append(f"{it['id']} (line {it['line']}) is missing Check")
        if not it["incident"]:
            errs.append(f"{it['id']} (line {it['line']}) is missing Incident")
    return errs


def path_matches(path: str, trigger: str) -> bool:
    if "*" in trigger:
        return fnmatch.fnmatch(path, trigger) or fnmatch.fnmatch(
            pathlib.PurePosixPath(path).name, trigger
        )
    if trigger.endswith("/"):
        return path.startswith(trigger)
    if trigger.startswith("_"):
        return path.endswith(trigger)
    return path == trigger or path.startswith(trigger)


def match_items(items: list[dict], changed_files: list[str]) -> list[dict]:
    matched: list[dict] = []
    for it in items:
        hits = [
            f for f in changed_files if any(path_matches(f, trig) for trig in it["triggers"])
        ]
        if hits:
            matched.append({**it, "hits": hits})
    return matched


def gate_modifications(changed_files: list[str]) -> list[str]:
    return [f for f in changed_files if any(path_matches(f, g) for g in GATE_PATHS)]


def self_test() -> None:
    sample = """
### R01 — Migrations
- **Trigger:** `internal/store/migrations/`
- **Check:** Never edit merged migrations.
- **Incident:** Plan 69.

### R02 — Tests
- **Trigger:** `_test.go`
- **Check:** No unannotated sleeps.
- **Incident:** Plan 75.

### R03 — List pagination
- **Trigger:** `internal/api/*list*`, `internal/api/page`
- **Check:** Bind cursors.
- **Incident:** #1064.
"""
    items = parse_checklist(sample)
    assert len(items) == 3, items
    hits = match_items(items, ["internal/api/page.go", "internal/store/migrations/0070_x.sql"])
    assert [h["id"] for h in hits] == ["R01", "R03"], hits
    hits2 = match_items(items, ["internal/api/foo_test.go"])
    assert [h["id"] for h in hits2] == ["R02"], hits2
    assert match_items(items, ["README.md"]) == []
    assert gate_modifications(["Makefile", "internal/api/page.go", ".claude/agents/verifier.md"]) == [
        "Makefile",
        ".claude/agents/verifier.md",
    ]
    harness_files = [
        "AGENTS.md", ".codex/hooks.json", ".agents/rules/wire.md",
        "harness/skills/verify/SKILL.md", "tools/harnessdoctor/main.go", "tools/harnesshook/main.go",
    ]
    assert gate_modifications(harness_files) == harness_files
    print("detector self-test: ok")


def diff_files(base_ref: str) -> list[str]:
    # --no-renames ensures a renamed gate file (e.g. moving .claude/agents/verifier.md)
    # reports its source path too so R16 and the gate-modification audit still fire.
    out = subprocess.check_output(
        ["git", "diff", "--no-renames", "--name-only", f"{base_ref}...HEAD"],
        cwd=ROOT,
        text=True,
    )
    files = [line.strip() for line in out.splitlines() if line.strip()]
    # Include uncommitted tracked changes when run locally before commit.
    wt = subprocess.check_output(
        ["git", "diff", "--no-renames", "--name-only", "HEAD"], cwd=ROOT, text=True
    )
    for line in wt.splitlines():
        p = line.strip()
        if p and p not in files:
            files.append(p)
    return sorted(files)


def load_review_from_base(base_ref: str) -> tuple[str, str]:
    """Load REVIEW.md from base_ref so a PR cannot weaken its own checklist.

    Falls back to the working-tree REVIEW.md only when REVIEW.md does not exist
    at base_ref (for example, on the PR introducing REVIEW.md).
    """
    proc = subprocess.run(
        ["git", "show", f"{base_ref}:REVIEW.md"],
        cwd=ROOT,
        capture_output=True,
        text=True,
    )
    if proc.returncode == 0:
        return proc.stdout, f"{base_ref}:REVIEW.md"
    if REVIEW_MD.exists():
        return REVIEW_MD.read_text(encoding="utf-8"), f"{REVIEW_MD.name} (fallback: absent on {base_ref})"
    raise FileNotFoundError(f"REVIEW.md absent on {base_ref} and in working tree")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--diff",
        metavar="BASE_REF",
        help="Print checklist items triggered by files changed against BASE_REF",
    )
    parser.add_argument(
        "--file",
        type=pathlib.Path,
        default=None,
        help="Explicit path to REVIEW.md (overrides base-ref loading in --diff mode)",
    )
    args = parser.parse_args()

    self_test()
    if args.file is not None:
        if not args.file.exists():
            print(f"missing {args.file}", file=sys.stderr)
            return 1
        text = args.file.read_text(encoding="utf-8")
        source_label = args.file.name
    elif args.diff:
        try:
            text, source_label = load_review_from_base(args.diff)
        except FileNotFoundError as exc:
            print(f"FAIL: {exc}", file=sys.stderr)
            return 1
    else:
        if not REVIEW_MD.exists():
            print(f"missing {REVIEW_MD}", file=sys.stderr)
            return 1
        text = REVIEW_MD.read_text(encoding="utf-8")
        source_label = REVIEW_MD.name

    items = parse_checklist(text)
    errs = validate_items(items)
    if errs:
        for e in errs:
            print(f"FAIL ({source_label}): {e}", file=sys.stderr)
        return 1

    if args.diff:
        files = diff_files(args.diff)
        matched = match_items(items, files)
        gate_hits = gate_modifications(files)
        print(
            f"Triggered reliability checklist items against {args.diff} "
            f"(rules from {source_label}; {len(files)} changed file(s)):"
        )
        if not matched:
            print("  (none triggered)")
        for it in matched:
            print(f"  - {it['id']} — {it['title']} ({len(it['hits'])} file(s))")
            print(f"    Check: {it['check']}")
        if gate_hits:
            print("\n[GATE-MODIFICATION AUDIT] This diff touches verification/review gates:")
            for g in gate_hits:
                print(f"  - {g}")
            print("Verify no gate, coverage denominator, or review rule was weakened without rationale.")
        return 0

    print(f"ok — {source_label} defines {len(items)} valid diff-triggered checklist items")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
