#!/usr/bin/env python3
"""Re-ask the terraform binary for the `fmt` exit every corpus row records.

The corpus is only worth what its oracle is. A fixture whose expected blocks
were written from a comment pins the comment; one whose terraform exit was
measured pins the binary, which is the property #758 and #761 both turned on —
each was a rule the readers agreed on and terraform disagreed with.

What this rung can and cannot say is worth being exact about. It puts each
file's `fmt` exit to `terraform fmt -check`, so a terraform upgrade that moves
one is read here rather than discovered by the next reviewer. It does NOT check
a row's `blocks` or `refuse` against terraform: `fmt` says whether the file
parses, never where the binary closed a heredoc, so those stay human-derived and
the two readers' agreement is what holds them.

Run: make tf-corpus-check
"""

import shutil
import subprocess
import sys

import loader

CASES = loader.HERE / "cases"

# No fixture needs anything like this long. It is here because a wedged
# terraform would otherwise hang the CI job to its own six-hour default, and a
# rung that cannot say "terraform stopped answering" is a rung that reports
# nothing at all.
TIMEOUT = 60


def main() -> int:
    tf = shutil.which("terraform")
    if tf is None:
        # A skip here would be the corpus's own failure mode: a check that
        # reads nothing reporting as if it had read everything.
        print("terraform is not on PATH — this rung IS the binary, so it fails "
              "rather than skips. brew install hashicorp/tap/terraform",
              file=sys.stderr)
        return 1

    cases, failures = loader.load()
    if not cases:
        print("\n".join(failures) or "the corpus manifest lists no cases", file=sys.stderr)
        return 1

    on_disk = {p.name for p in CASES.glob("*.tf")}
    for c in cases:
        if c.get("file") not in on_disk or loader.row_problem(c):
            continue  # loader.load() already named it
        # Handed a path relative to terraform's own cwd, not an absolute one.
        # loader.HERE is resolved, and terraform relativizes what it is given
        # against the cwd it INHERITS — which under a symlinked checkout
        # (macOS's /tmp -> /private/tmp) is the unresolved spelling. The two do
        # not meet, so terraform reported `No file or directory` for every case
        # and this rung blamed the binary for a file it had never opened.
        #
        # Output is captured as BYTES. A row can turn on a byte that is not
        # UTF-8, and terraform echoes the offending source line back: decoding
        # it would kill the run with a UnicodeDecodeError mid-loop, leaving
        # every later case unchecked. Only the exit code is read.
        try:
            r = subprocess.run([tf, "fmt", "-check", "-no-color", "cases/" + c["file"]],
                               cwd=loader.HERE, stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, timeout=TIMEOUT)
        except subprocess.TimeoutExpired:
            failures.append("%s: terraform did not answer within %ds" % (c["file"], TIMEOUT))
            print("  FAIL timeout %s" % c["file"])
            continue
        if r.returncode != c["fmt"]:
            failures.append(
                "%s: terraform fmt -check exits %d, the manifest records %d"
                % (c["file"], r.returncode, c["fmt"]))
        # A file the binary TAKES that a reader refuses anyway. Legitimate —
        # reading it needs real HCL lexing — but it is a standing cost, so a row
        # has to say so in as many words. Without that, a fourth one could be
        # added and the only trace would be a line in a log nobody opens.
        accepted = r.returncode in (0, 3)
        claimed = bool(c.get("terraform_accepts"))
        if c.get("refuse") and accepted and not claimed:
            failures.append(
                "%s: terraform accepts this file and a reader refuses it — a "
                "deliberate refusal, so the row has to carry "
                "\"terraform_accepts\": true" % c["file"])
        elif claimed and not (c.get("refuse") and accepted):
            failures.append(
                "%s: the row claims \"terraform_accepts\" and this is not a "
                "refusal over a file terraform takes" % c["file"])
        print("  %s fmt=%d %s%s"
              % ("ok  " if r.returncode == c["fmt"] else "FAIL",
                 r.returncode, c["file"],
                 "   (refused on purpose)" if claimed else ""))

    if failures:
        print("\nFAILED:", file=sys.stderr)
        for f in failures:
            print("  - %s" % f, file=sys.stderr)
        return 1
    print("\nok: terraform gives all %d cases the `fmt` exit the manifest records"
          % len(cases))
    return 0


if __name__ == "__main__":
    sys.exit(main())
