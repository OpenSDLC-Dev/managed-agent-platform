#!/usr/bin/env python3
"""Enforce the foundation/environment split that makes deploy/gcp/ safe to tear down.

Two rules, both structural rather than conventional, because the failure they
prevent is silent and irreversible (plan 20, Decision 9):

  1. `environment/` may not OWN a resource of an unrecoverable kind. It is the
     half `make gcp-env-destroy` tears down, and these kinds cannot survive that:
     KMS key rings and crypto keys cannot be deleted at all, so a destroy leaves
     them behind to collide on the next apply — and the crypto key is the only
     thing that can decrypt the vault ciphertext in Postgres. Deleting a service
     account deletes its HMAC keys, stranding the once-readable GCS HMAC secret
     in Secret Manager, valid-looking and dead.

  2. Every resource of an unrecoverable kind in `foundation/` must carry
     `prevent_destroy`.

Rule 2 is expressed over KINDS, not over a hand-maintained list of resource
addresses. A list would go stale the moment someone adds a tenth resource, and
would false-FAIL the moment someone moved one to another .tf file.

Comments are stripped before matching. An earlier version of this check regexed
the raw text and passed a block whose flag was `false` but which mentioned
`prevent_destroy = true` in a comment.
"""

import pathlib
import re
import sys

# Kinds that a destroy cannot undo. Adding one here is how a new hazard gets
# covered; nothing else needs changing.
UNRECOVERABLE = {
    "google_kms_key_ring",
    "google_kms_crypto_key",
    "google_service_account",
    "google_storage_hmac_key",
    "google_secret_manager_secret",
}

ROOT = pathlib.Path(__file__).parent
RESOURCE = re.compile(r'^resource\s+"([^"]+)"\s+"([^"]+)"\s*\{')


def strip_comments(text: str) -> str:
    """Remove # and // line comments, leaving anything inside a string alone.

    Terraform also has /* */ blocks; none appear here and a naive strip of them
    would be more likely to break valid config than to catch anything, so they
    are deliberately not handled — a block comment claiming prevent_destroy
    would have to sit inside the resource it is lying about, which is a
    conspicuous thing to write.
    """
    out = []
    for line in text.splitlines():
        quoted = False
        for i, ch in enumerate(line):
            if ch == '"' and (i == 0 or line[i - 1] != "\\"):
                quoted = not quoted
            elif not quoted and (ch == "#" or line[i : i + 2] == "//"):
                line = line[:i]
                break
        out.append(line)
    return "\n".join(out)


def blocks(path: pathlib.Path):
    """Yield (kind, name, body) for each top-level resource block in a .tf file."""
    text = strip_comments(path.read_text())
    lines = text.splitlines()
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
            if not re.search(r"^\s*prevent_destroy\s*=\s*true\s*$", body, re.M):
                failures.append(
                    f"{path.relative_to(ROOT)}: foundation/ {kind}.{name} is missing "
                    f"`prevent_destroy = true`."
                )

    # A rule that silently covers nothing is worse than no rule: if a refactor
    # ever moves these resources out from under this scan, say so rather than
    # printing ok.
    if found == 0:
        failures.append("foundation/ declares no unrecoverable resources at all — this check is scanning nothing.")

    for f in failures:
        print(f, file=sys.stderr)
    if failures:
        return 1
    print(f"ok: foundation/environment split holds ({found} protected resources)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
