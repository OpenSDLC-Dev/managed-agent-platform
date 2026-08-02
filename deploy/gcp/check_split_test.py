#!/usr/bin/env python3
"""Plant violations under a scratch copy of deploy/gcp and require check_split.py to catch them.

A guard is only worth what it refuses, and this one has twice printed `ok` over a
configuration it had silently stopped reading. Running it against the real tree
proves nothing: the real tree is compliant, so a checker that scanned zero bytes
would pass that too. Each case below is a `google_kms_crypto_key` or a
`google_service_account` planted in `environment/` — the half `make gcp-env-destroy`
tears down, where destroying either is irreversible — plus the parser states that
can hide one.

Every case asserts a NON-ZERO exit and, where it matters, which message came back.
The last group is the inverse: things that look suspicious and must stay green, so
this file cannot be satisfied by a checker that simply refuses everything.

Run: make gcp-split-check-test
"""

import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).parent.resolve()
CHECKER = HERE / "check_split.py"

ROGUE_KEY = '''
resource "google_kms_crypto_key" "rogue" {
  name     = "rogue"
  key_ring = "projects/p/locations/us-central1/keyRings/r"
}
'''

failures = []


def run_checker(root):
    p = subprocess.run([sys.executable, str(root / "check_split.py")],
                       capture_output=True, text=True, timeout=120)
    return p.returncode, p.stdout + p.stderr


def case(tmp, label, mutate, expect_ok=False, expect_text=None):
    root = pathlib.Path(tempfile.mkdtemp(dir=tmp)) / "gcp"
    shutil.copytree(HERE, root, ignore=shutil.ignore_patterns(
        ".terraform", "*.tfstate*", "terraform.tfvars", "__pycache__"))
    mutate(root)
    code, out = run_checker(root)
    if expect_ok:
        ok, detail = code == 0, out
    else:
        ok = code != 0 and (expect_text is None or expect_text in out)
        detail = out if code != 0 else "checker returned 0 — the violation was NOT caught"
    print(("  ok   " if ok else "  FAIL ") + label)
    if not ok:
        print("       " + detail.strip().replace("\n", "\n       ")[:400])
        failures.append(label)


def append(rel, text):
    return lambda root: (root / rel).write_text((root / rel).read_text() + text)


def write(rel, text):
    def go(root):
        p = root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text)
    return go


def main():
    if not CHECKER.exists():
        print("check_split.py not found next to this test", file=sys.stderr)
        return 1
    tmp = tempfile.mkdtemp(prefix="check-split-test.")
    try:
        print("the unmodified tree passes")
        case(tmp, "baseline is green", lambda root: None, expect_ok=True)

        print("an unrecoverable resource in environment/ is caught wherever it hides")
        case(tmp, "at the top level", append("environment/main.tf", ROGUE_KEY),
             expect_text="must not OWN")
        case(tmp, "in a child module (the glob must recurse)",
             write("environment/modules/nodepool/main.tf",
                   'resource "google_service_account" "nodes" {\n  account_id = "n"\n}\n'),
             expect_text="must not OWN")

        print("a module this check cannot follow is refused, not skipped")
        case(tmp, "sourced from a registry",
             append("environment/main.tf",
                    '\nmodule "gke" {\n  source = "terraform-google-modules/kubernetes-engine/google"\n}\n'),
             expect_text="not a local path")
        case(tmp, "resolving outside the half",
             append("environment/main.tf", '\nmodule "shared" {\n  source = "../foundation"\n}\n'),
             expect_text="resolves outside")
        case(tmp, "with a source computed at plan time",
             append("environment/main.tf", '\nmodule "rogue" {\n  source = "./${var.escape}"\n}\n'),
             expect_text="interpolated source")
        case(tmp, "with no literal source at all",
             append("environment/main.tf", '\nmodule "rogue" {\n  count = 1\n}\n'),
             expect_text="no literal `source`")

        print("JSON configuration is refused rather than quietly unread")
        case(tmp, "at the top level", write("environment/extra.tf.json", "{}"),
             expect_text="not readable by this check")
        case(tmp, "inside a child module", write("environment/modules/x/main.tf.json", "{}"),
             expect_text="not readable by this check")

        print("a parser that loses its place fails loudly instead of reporting ok")
        case(tmp, "a <<WORD inside a string does not open a heredoc",
             append("environment/main.tf",
                    '\nvariable "note" {\n  default = "build with: docker build - <<EOF"\n}\n' + ROGUE_KEY),
             expect_text="must not OWN")
        # Both forms, because they are one hazard with two syntaxes and the first
        # fix covered only `${`: nested quotes shift the parity seen from outside,
        # so the `{` here and the `}` in the closer both fall out of the string.
        # Depth desyncs around the rogue resource and still balances at EOF, so
        # nothing downstream notices.
        case(tmp, "a nested quote inside ${...} is refused",
             append("environment/main.tf",
                    '\nlocals {\n  opener = "${format("%s", "{")}"\n}\n' + ROGUE_KEY),
             expect_text="cannot be read by this guard")
        case(tmp, "a nested quote inside a %{...} template directive is refused",
             append("environment/main.tf",
                    '\nlocals {\n  opener = "%{ if "x{" == "y" }t%{ endif }"\n}\n' + ROGUE_KEY),
             expect_text="cannot be read by this guard")
        case(tmp, "an unterminated heredoc is refused",
             append("environment/main.tf", '\nlocals {\n  x = <<NEVERCLOSED\nbody\n}\n'),
             expect_text="never terminated")
        case(tmp, "a multi-line interpolation is refused",
             append("environment/main.tf",
                    '\nlocals {\n  x = "${coalesce(\n    var.a,\n    "b",\n  )}"\n}\n'),
             expect_text="still open at end of line")
        case(tmp, "unbalanced braces are refused",
             append("environment/main.tf", "\n}\n"), expect_text="unbalanced braces")
        case(tmp, "a block comment is refused",
             append("environment/main.tf", "\n/* hidden */\n"), expect_text="block comments")

        print("foundation/ must keep both guards on every unrecoverable resource")

        # Anchored to the START of a line, and matched by regex rather than by
        # literal text. Both matter, and both were got wrong first:
        #   - `terraform fmt` aligns the `=` within a block, so the spacing is
        #     whatever the neighbouring attributes dictate;
        #   - foundation/main.tf's header COMMENT contains the exact phrase
        #     `deletion_policy = "PREVENT"`, so an unanchored match rewrites prose
        #     the checker rightly ignores. The mutation then changes nothing, the
        #     checker correctly returns 0, and the case fails while looking like a
        #     hole in the guard.
        def edit(rel, pattern, repl):
            def go(root):
                p = root / rel
                new, n = re.subn(pattern, repl, p.read_text(), count=1, flags=re.M)
                assert n == 1, "mutation matched nothing: %s" % pattern
                p.write_text(new)
            return go

        case(tmp, "a dropped prevent_destroy",
             edit("foundation/main.tf", r"^(\s*)prevent_destroy(\s*)=(\s*)true",
                  r"\1prevent_destroy\2=\3false"),
             expect_text="prevent_destroy")
        case(tmp, "a weakened deletion_policy",
             edit("foundation/main.tf", r'^(\s*)deletion_policy(\s*)=(\s*)"PREVENT"',
                  r'\1deletion_policy\2=\3"DELETE"'),
             expect_text="deletion_policy")
        case(tmp, "a false guard under a comment that claims otherwise",
             edit("foundation/main.tf", r"^(\s*)prevent_destroy(\s*)=(\s*)true",
                  r"\1# prevent_destroy\2=\3true\n\1prevent_destroy\2=\3false"),
             expect_text="prevent_destroy")

        print("things that only look suspicious stay green")
        case(tmp, "a plain ${...} interpolation",
             append("environment/main.tf",
                    '\nlocals {\n  plain = "serviceAccount:${var.project_id}-x"\n}\n'), expect_ok=True)
        case(tmp, "a brace inside a string",
             append("environment/main.tf",
                    '\nlocals {\n  brace = "a { b } c"\n}\n'), expect_ok=True)
        case(tmp, "an unrecoverable kind read as a data source, not owned",
             append("environment/main.tf",
                    '\ndata "google_kms_crypto_key" "read" {\n  name     = "x"\n  key_ring = "y"\n}\n'),
             expect_ok=True)
        case(tmp, "a local child module that owns nothing unrecoverable",
             write("environment/modules/ok/main.tf",
                   'resource "google_storage_bucket_object" "x" {\n  name = "n"\n  bucket = "b"\n}\n'),
             expect_ok=True)

        print()
        if failures:
            print("FAILED: %d case(s)" % len(failures))
            for f in failures:
                print("  - %s" % f)
            return 1
        print("ok: the split guard catches every planted violation and passes every decoy")
        return 0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
