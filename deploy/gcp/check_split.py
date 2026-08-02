#!/usr/bin/env python3
"""Enforce the foundation/environment split that makes deploy/gcp/ safe to tear down.

Two rules, both structural rather than conventional, because the failure they
prevent is silent and irreversible (plan 20, Decision 9):

  1. `environment/` may not OWN a resource of an unrecoverable kind. It is the
     half `make gcp-env-destroy` tears down, and these kinds cannot survive that:
     KMS key rings and crypto keys cannot be deleted at all — worse, destroying
     the resource schedules every key version for destruction, so the name stays
     taken AND the key becomes unusable, and the vault ciphertext in Postgres is
     decryptable by that key and nothing else. Deleting a service account deletes
     its HMAC keys, stranding the once-readable GCS HMAC secret in Secret
     Manager, valid-looking and dead.

  2. Every resource of an unrecoverable kind in `foundation/` must carry BOTH
     guards it can: `prevent_destroy`, and — for the kinds that have it —
     `deletion_policy = "PREVENT"`. They cover different failure modes.
     `prevent_destroy` is a property of the CONFIGURATION and disappears along
     with the block it is written in, so Terraform will happily destroy a
     resource whose block you merely deleted. `deletion_policy` is read from
     STATE and survives exactly that. Checking only one would let the other rot.

Rule 2 is expressed over KINDS, not over a hand-maintained list of resource
addresses. A list goes stale the moment someone adds a tenth resource and
false-fails the moment someone moves one to another .tf file.

The parser is deliberately more careful than a regex sweep, because every
shortcut here has already produced a wrong answer in review:

  - comments are stripped, or a `prevent_destroy = false` under a comment reading
    `# prevent_destroy = true` reads as compliance;
  - braces inside strings do not count, or one `{` in a display_name swallows
    every resource after it and the check reports success over a fraction of the
    file;
  - heredoc bodies are skipped, or prose in a variable description that happens
    to begin a line with `resource "..."` is reported as an owned resource;
  - and the resource count is asserted against a floor, so an under-scan that
    slips past all of the above still cannot pass quietly.
"""

import pathlib
import re
import sys

# Kinds a destroy cannot undo. Adding one here is how a new hazard gets covered;
# nothing else needs changing. The value is whether the kind also supports
# `deletion_policy` — a key ring does not, because it has no delete at all.
UNRECOVERABLE = {
    "google_kms_key_ring": False,
    "google_kms_crypto_key": True,
    "google_service_account": True,
    "google_storage_hmac_key": True,
    "google_secret_manager_secret": True,
}

# What foundation/ is expected to protect today. A scan that finds fewer has
# lost sight of something — the whole check passing over three resources
# instead of eight is exactly the failure a "did I find anything at all?" guard
# is too weak to catch.
MIN_PROTECTED = 8

ROOT = pathlib.Path(__file__).parent
RESOURCE = re.compile(r'^resource\s+"([^"]+)"\s+"([^"]+)"')
HEREDOC = re.compile(r"<<[-~]?([A-Za-z_][A-Za-z0-9_]*)")


def scrub(line: str) -> str:
    """Drop comments, and neutralize the structural characters inside strings.

    String TEXT is kept — the resource header is read from it. What is removed
    is a string's ability to affect brace depth or start a comment, so that a
    `{` in a display_name cannot swallow the rest of the file.
    """
    out, quoted = [], False
    i = 0
    while i < len(line):
        ch = line[i]
        if quoted:
            if ch == "\\":
                out.append("_")
                i += 2
                continue
            if ch == '"':
                quoted = False
                out.append('"')
            else:
                out.append("_" if ch in "{}#" else ch)
            i += 1
            continue
        if ch == '"':
            quoted = True
            out.append('"')
        elif ch == "#" or line[i : i + 2] == "//":
            break
        elif line[i : i + 2] == "/*":
            # A block comment is not tracked across lines; opening one is enough
            # of an oddity in this tree to refuse rather than guess.
            raise ValueError("/* */ block comments are not supported here — use # so the guard checks can read the file")
        else:
            out.append(ch)
        i += 1
    return "".join(out)


def blocks(path: pathlib.Path):
    """Yield (kind, name, body) for each top-level resource block in a .tf file."""
    raw = path.read_text().replace("\r\n", "\n").splitlines()
    lines, skip_until = [], None
    for line in raw:
        if skip_until is not None:
            if line.strip() == skip_until:
                skip_until = None
            lines.append("")  # keep numbering, contribute no structure
            continue
        try:
            scrubbed = scrub(line)
        except ValueError as exc:
            raise ValueError(f"{path}: {exc}") from None
        m = HEREDOC.search(scrubbed)
        if m:
            skip_until = m.group(1)
            scrubbed = scrubbed[: m.start()]
        lines.append(scrubbed)

    i = 0
    while i < len(lines):
        m = RESOURCE.match(lines[i])
        if not m:
            i += 1
            continue
        depth = lines[i].count("{") - lines[i].count("}")
        body, i = [], i + 1
        while i < len(lines) and depth > 0:
            depth += lines[i].count("{") - lines[i].count("}")
            if depth > 0:
                body.append(lines[i])
            i += 1
        yield m.group(1), m.group(2), "\n".join(body)


def main() -> int:
    failures = []

    for path in sorted((ROOT / "environment").glob("*.tf")):
        for kind, name, _ in blocks(path):
            if kind in UNRECOVERABLE:
                failures.append(
                    f"{path.relative_to(ROOT)}: environment/ must not OWN {kind}.{name} — "
                    f"it is destroyed by `make gcp-env-destroy`. Read the foundation's copy "
                    f"with a data source instead."
                )

    found = 0
    for path in sorted((ROOT / "foundation").glob("*.tf")):
        for kind, name, body in blocks(path):
            if kind not in UNRECOVERABLE:
                continue
            found += 1
            where = f"{path.relative_to(ROOT)}: foundation/ {kind}.{name}"
            if not re.search(r"^\s*prevent_destroy\s*=\s*true\s*$", body, re.M):
                failures.append(f"{where} is missing `prevent_destroy = true`.")
            if UNRECOVERABLE[kind] and not re.search(
                r'^\s*deletion_policy\s*=\s*"PREVENT"\s*$', body, re.M
            ):
                failures.append(
                    f"{where} is missing `deletion_policy = \"PREVENT\"` — prevent_destroy "
                    f"alone disappears with the block it is written in."
                )

    # A rule that silently covers less than it did is worse than no rule. "Found
    # nothing" is not the only under-scan worth refusing: a parser that loses its
    # place halfway through a file reports success over the resources it still
    # saw.
    if found < MIN_PROTECTED:
        failures.append(
            f"scanned only {found} unrecoverable resources in foundation/, expected at least "
            f"{MIN_PROTECTED}. Either resources were removed (update MIN_PROTECTED deliberately) "
            f"or this parser lost its place — do not assume the former."
        )

    for f in failures:
        print(f, file=sys.stderr)
    if failures:
        return 1
    print(f"ok: foundation/environment split holds ({found} protected resources)")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except ValueError as exc:
        print(exc, file=sys.stderr)
        sys.exit(1)
