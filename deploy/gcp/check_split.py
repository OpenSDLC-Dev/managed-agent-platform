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
     its HMAC keys along with it, which would strand a once-readable secret in
     Secret Manager, valid-looking and dead — the hazard that put the `-storage`
     identity here before #240 retired it, and the reason the kind stays listed.

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
  - but `<<WORD` inside a STRING does not open one, or a shell snippet in a
    description makes the rest of the file invisible — a silent under-scan that
    reports ok over the resources it still saw;
  - a quoted string inside a ${...} interpolation or a %{...} template directive
    is REFUSED rather than parsed: nested quotes shift the parity seen from
    outside, so a later `{` and a later `}` both fall out of the string and the
    depth desyncs around an unread resource while still balancing at EOF;
  - and every way the parser can lose its place is a hard error rather than a
    quiet short read: an unterminated heredoc, unbalanced braces at EOF, a string
    still open at end of line, and a protected-resource count below a floor.

None of that makes this an HCL parser, and it is not trying to be one. It reads
what it can read and refuses everything else, because the only unacceptable
outcome is printing `ok` over configuration it did not look at.

Coverage is the other half of correctness, and a check that reads only some of
the configuration is worse than none, because it prints ok:

  - .tf files are found RECURSIVELY, so a resource moved into a child module
    under `environment/` is still read;
  - a `module` block whose source this checker cannot follow — a registry or git
    module, a local path outside the half being checked, or one computed at plan
    time (Terraform 1.15 permits constant variables in a source) — is refused
    outright rather than skipped, exactly as `.tf.json` is. A community module
    that creates a service account by default would otherwise put an
    unrecoverable resource in the disposable half with the check still green.
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
    # No HMAC key is declared anywhere since #240 removed the S3-interop path.
    # The entry stays as the guard that stops one coming back into the
    # disposable half: its secret is readable exactly once, at creation.
    "google_storage_hmac_key": True,
    "google_secret_manager_secret": True,
}

# What foundation/ is expected to protect today. A scan that finds fewer has
# lost sight of something — the whole check passing over three resources
# instead of seven is exactly the failure a "did I find anything at all?" guard
# is too weak to catch. Raise it with the foundation: left behind, it keeps
# printing ok over a scan that has quietly stopped seeing the newest resources.
#
# Ten until #240, which retired three: the `-storage` service account that held
# the GCS HMAC key, and the two secret containers holding the key's halves.
# Object storage authenticates as the workloads themselves now, so there is no
# fourth identity and no downloaded credential to protect. Lowered deliberately,
# which is exactly what the failure message below asks for — the number is a
# claim about the foundation, not a high-water mark.
MIN_PROTECTED = 7

ROOT = pathlib.Path(__file__).parent
RESOURCE = re.compile(r'^\s*resource\s+"([^"]+)"\s+"([^"]+)"')
MODULE = re.compile(r'^\s*module\s+"([^"]+)"')
SOURCE = re.compile(r'^\s*source\s*=\s*"([^"]+)"\s*$', re.M)
HEREDOC = re.compile(r"<<[-~]?([A-Za-z_][A-Za-z0-9_]*)")


def scrub(line: str) -> str:
    """Drop comments, and neutralize the structural characters inside strings.

    String TEXT is kept — the resource header is read from it. What is removed
    is a string's ability to affect brace depth, start a comment, or open a
    heredoc, so that neither a `{` in a display_name nor a `<<EOF` in a shell
    snippet can swallow the rest of the file.
    """
    out, quoted, interp = [], False, 0
    i = 0
    while i < len(line):
        ch = line[i]
        if quoted:
            if ch == "\\":
                out.append("_")
                i += 2
                continue
            if line[i : i + 2] in ("${", "%{"):
                # An interpolation `${...}` or a template directive `%{...}`.
                # BOTH, because they are the same hazard: their own braces are not
                # structure, but their CONTENTS are HCL again — including nested
                # strings. Handling only `${` leaves `%{ if "x{" == "y" }` free to
                # shift the quote parity and hide a resource, which is the very
                # defect this tracking was added to close.
                #
                # The leading character is kept deliberately: it is the only
                # surviving marker that a string was computed rather than literal,
                # and check_modules() reads it to refuse a module source it cannot
                # resolve.
                interp += 1
                out.append(ch + "_")
                i += 2
                continue
            if interp:
                if ch == "{":
                    interp += 1
                elif ch == "}":
                    interp -= 1
                elif ch == '"':
                    # A quoted string INSIDE `${...}` — e.g. `"${format("%s", "{")}"`.
                    # Reading it needs real HCL lexing, because from the outside the
                    # quote parity is shifted: a later `{` or `}` silently moves from
                    # inside a string to outside one, and the brace depth desyncs for
                    # the rest of the file. Terraform accepts that config; this guard
                    # would report `ok` over the part of it that went unread, which is
                    # precisely how an unrecoverable resource hides in `environment/`.
                    raise ValueError(
                        "a quoted string inside a ${...} interpolation or %{...} directive "
                        "cannot be read by this guard — assign it to a `locals` value and "
                        "interpolate that instead"
                    )
                out.append("_" if ch in "{}#<" else ch)
                i += 1
                continue
            if ch == '"':
                quoted = False
                out.append('"')
            else:
                out.append("_" if ch in "{}#<" else ch)
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
    if quoted:
        # A string still open at end of line means a multi-line `${...}`
        # interpolation. Quote state is tracked per line, so the closing `}"` on
        # a later line would be counted as structure and desync the brace depth
        # for the rest of the file — which is exactly how this was found. Reading
        # it properly needs full HCL interpolation lexing (a `"` inside `${}` is
        # a NESTED string, not a terminator); refusing is the honest answer.
        raise ValueError(
            "a string is still open at end of line — write multi-line interpolations as a "
            "single-line `locals` value so this guard can read the file"
        )
    return "".join(out)


def blocks(path: pathlib.Path):
    """Yield (btype, kind, name, body) for each top-level block in a .tf file.

    btype is "resource" (kind = resource type, name = local name) or "module"
    (kind = the module's local name, name = ""). Both are needed: resources are
    what the two rules are about, and modules are how a resource escapes them.
    """
    # split("\n"), not splitlines(): the latter also breaks on U+2028, U+2029,
    # \v, \f and \x85, which HCL treats as ordinary characters inside a string.
    # A description containing one would otherwise be cut mid-string and desync
    # the quote tracking for the rest of the file.
    raw = path.read_text().replace("\r\n", "\n").split("\n")
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

    if skip_until is not None:
        raise ValueError(
            f"{path}: heredoc <<{skip_until} is never terminated, so the rest of the file "
            f"was not read. Refusing to report on a partial scan."
        )

    # `resource` and `module` are matched only at brace depth 0, and with leading
    # whitespace allowed. `terraform fmt` would normally put a top-level block at
    # column 0, but a check that only works when another check already passed is
    # a check that can be bypassed by running them in the wrong order.
    i, depth0 = 0, 0
    while i < len(lines):
        m = RESOURCE.match(lines[i]) if depth0 == 0 else None
        mod = MODULE.match(lines[i]) if (depth0 == 0 and not m) else None
        if not m and not mod:
            depth0 += lines[i].count("{") - lines[i].count("}")
            i += 1
            continue
        depth = lines[i].count("{") - lines[i].count("}")
        body, i = [], i + 1
        while i < len(lines) and depth > 0:
            depth += lines[i].count("{") - lines[i].count("}")
            if depth > 0:
                body.append(lines[i])
            i += 1
        if depth > 0:
            raise ValueError(
                f"{path}: unbalanced braces — the file ended inside a block. The parser "
                f"lost its place, so anything after it went unread."
            )
        if m:
            yield "resource", m.group(1), m.group(2), "\n".join(body)
        else:
            yield "module", mod.group(1), "", "\n".join(body)

    if depth0 != 0:
        raise ValueError(
            f"{path}: unbalanced braces at end of file (depth {depth0}). The parser lost "
            f"its place, so part of the file went unread."
        )


def main() -> int:
    failures = []

    # Terraform also accepts JSON configuration, which this parser cannot read.
    # Refusing is the only safe answer: silently skipping one would let an
    # unrecoverable resource be declared in `environment/rogue.tf.json` with the
    # check still printing ok. rglob, not glob — a child module is exactly where
    # someone would put the file that this rule is inconvenient for.
    for half in ("foundation", "environment"):
        for path in sorted((ROOT / half).rglob("*.tf.json")):
            failures.append(
                f"{path.relative_to(ROOT)}: JSON configuration is not readable by this check. "
                f"Express it in HCL, or teach check_split.py to parse .tf.json — do not leave "
                f"it unchecked."
            )

    def check_modules(half: str, path: pathlib.Path, name: str, body: str):
        """Refuse any module whose .tf files this check will not have read."""
        src = SOURCE.search(body)
        where = f"{path.relative_to(ROOT)}: module.{name}"
        if not src:
            failures.append(f"{where} has no literal `source` — this check cannot follow it.")
            return
        source = src.group(1)
        if "$" in source or "%" in source:
            # Terraform 1.15 allows constant variables and locals in a module
            # source, so `source = "./${var.escape}"` can resolve anywhere —
            # `%` is caught too, for the template-directive form —
            # including outside this half. The literal cannot be resolved here,
            # and resolving it as written would accept a path that Terraform
            # never uses.
            failures.append(
                f"{where} has an interpolated source {source!r}. This check resolves module "
                f"paths literally and cannot follow one computed at plan time — write it as a "
                f"literal relative path."
            )
            return
        if not source.startswith("./") and not source.startswith("../"):
            failures.append(
                f"{where} has source {source!r}, which is not a local path. A registry or git "
                f"module's resources are invisible to this check — several community modules "
                f"create a service account by default. Vendor it under {half}/ or do not use it."
            )
            return
        target = (path.parent / source).resolve()
        if not target.is_relative_to((ROOT / half).resolve()):
            failures.append(
                f"{where} has source {source!r}, which resolves outside {half}/. Only .tf files "
                f"under {half}/ are read by this check."
            )

    for path in sorted((ROOT / "environment").rglob("*.tf")):
        for btype, kind, name, body in blocks(path):
            if btype == "module":
                check_modules("environment", path, kind, body)
            elif kind in UNRECOVERABLE:
                failures.append(
                    f"{path.relative_to(ROOT)}: environment/ must not OWN {kind}.{name} — "
                    f"it is destroyed by `make gcp-env-destroy`. Read the foundation's copy "
                    f"with a data source instead."
                )

    found = 0
    for path in sorted((ROOT / "foundation").rglob("*.tf")):
        for btype, kind, name, body in blocks(path):
            if btype == "module":
                check_modules("foundation", path, kind, body)
                continue
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
