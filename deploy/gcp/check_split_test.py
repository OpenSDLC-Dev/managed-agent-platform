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


def append_bytes(rel, data):
    """Append bytes rather than text.

    These fixtures turn on a byte the reader has to see exactly, which append()
    above cannot promise: it round-trips the file through `read_text()`, whose
    universal-newline translation rewrites a lone carriage return into "\n"
    before the fixture is ever written. Appending in binary also leaves the file
    ending exactly where the caller put it, which is what the no-trailing-newline
    cases need.
    """
    def go(root):
        with (root / rel).open("ab") as f:
            f.write(data)
    return go


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
        case(tmp, "with a source built by a template directive",
             append("environment/main.tf",
                    '\nmodule "rogue" {\n  source = "./%{ if true }escape%{ endif }"\n}\n'),
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
        # Terraform ends a heredoc at the first line that, TRIMMED, is the
        # terminator — `<<EOT` exactly as much as `<<-EOT`; the marker dedents
        # the body, it does not move the end. Measured against terraform
        # 1.15.8, whose parser accepts `    EOT`, `EOT   ` and a tab-indented
        # one, and rejects `EOTX`, `x EOT` and `EOT }`. Requiring an exact match
        # instead reads on past a terminator Terraform honoured, and everything
        # after it is configuration to Terraform and string content here (#758).
        #
        # Two cases because the wrong rule fails two different ways, and only
        # one of them is loud.
        # Four paddings, not just the indented one, because every narrower trim
        # passes a suite that pins fewer: `lstrip()` reads on past `EOT   `, and
        # `strip(" \t")` reads on past the non-breaking space. Terraform honours
        # all four — measured, and `terraform fmt -check` leaves every byte of
        # them alone — so a tree can carry any one, and the heredoc has to close
        # for the key after it to sit at depth 0 and be seen.
        #
        # One file per form, deliberately. Put them together and a form that
        # fails to close is rescued by the NEXT form's terminator line, the key
        # is found anyway, and the case passes over a reader that is wrong —
        # which is how this started out written.
        for label, term in (("an indented", "    EOT"),
                            ("a trailing-space", "EOT   "),
                            ("a tab-indented", "\tEOT"),
                            ("a non-breaking-space", " EOT")):
            case(tmp, "%s terminator ends a plain heredoc, so what follows is real" % label,
                 append("environment/main.tf",
                        '\nlocals {\n  a = <<EOT\n' + term + '\n}\n' + ROGUE_KEY),
                 expect_text="must not OWN")
        # The quiet one: a second heredoc's bare terminator closes the FIRST for
        # a checker still inside it, so the braces it swallowed balance again at
        # end of file and neither the unterminated-heredoc nor the unbalanced-
        # brace refusal fires. `terraform fmt -check` exits 0 on this exact text,
        # so a tree can carry it.
        case(tmp, "a swallowed key that re-balances at end of file",
             append("environment/main.tf",
                    '\nlocals {\n  a = <<EOT\n    EOT\n}\n' + ROGUE_KEY
                    + '\nlocals {\n  b = <<EOT2\nEOT\nEOT2\n}\n'),
             expect_text="must not OWN")
        # The desync also runs the other way, and there it is Python's own
        # whitespace that opens it: str.strip() removes U+001C-U+001F and
        # Terraform does not, so those four end the string HERE and not THERE
        # and this reader takes body lines for structure. The `{` among them
        # buries the key below depth 0, where neither rule looks; the second
        # heredoc returns the depth, so nothing is unbalanced at EOF. This is
        # the one boundary no other gate covers — `terraform fmt -check
        # -recursive` exits 0 on the whole tree with this appended (#761).
        case(tmp, "a separator character forging a heredoc terminator",
             append("environment/main.tf",
                    '\nlocals {\n  a = <<EOT\n\x1cEOT\n{\nEOT\n}\n' + ROGUE_KEY
                    + '\nlocals {\n  b = <<EOT2\n\x1cEOT2\n}\nEOT2\n}\n'),
             expect_text="must not OWN")
        # The two boundaries #761 names, where this reader and the binary
        # disagreed. Both are files terraform refuses outright, so `make gcp-fmt`
        # reddens on them — but what this guard owes is narrower than refusing
        # every such file, and is what it broke here: never lose its place, and
        # never report over configuration it did not read. On both of these it
        # read part of the file and printed `ok` over the rest. Files terraform
        # refuses are not automatically this guard's business: a NUL among
        # structure is an `Invalid character` to terraform and this guard reads
        # straight through it and still catches what it was hiding, measured on
        # 1.15.8. Adding a refusal for every byte terraform dislikes would be
        # this guard rejecting what it can perfectly well read.
        #
        # HCL wants the newline after a terminator that a file's last line never
        # gets: terraform 1.15.8 says `Unterminated template string`, and
        # accepts the very same bytes once a trailing newline is added. An
        # ordinary last line without one it accepts either way, so the rule
        # belongs to the terminator and not to the file. Written at depth 0 on
        # purpose: inside a block the unbalanced-brace refusal fires and blames
        # a brace, while here the depth never moves and nothing fires at all.
        case(tmp, "a heredoc terminator as a last line with no trailing newline",
             append_bytes("environment/main.tf", b'\ny = <<EOT\ntext\nEOT'),
             expect_text="no trailing newline")
        # And a carriage return that ends no CRLF. `read_text()` translated it
        # into a line break, so this reader parsed a file terraform refuses as an
        # `Invalid character` — and tools/kmsrole/hcl.go, which breaks on "\n"
        # alone, read the same bytes as a single line where only the first header
        # can match. Neither said anything.
        case(tmp, "a bare carriage return is refused rather than translated",
             append_bytes("environment/main.tf", b'\nlocals {\r  a = 1\r}\r'),
             expect_text="carriage return")
        # Three more positions across the two halves of the check. The first two
        # reach the raw body check, because a body line is never scrubbed: a
        # return anywhere in the body, and one before the terminator word, which
        # would close the string in the wrong place, being padding to str.strip()
        # and not to terraform. The third reaches the scrubbed-line check, where
        # an ESCAPED return used to vanish — the scrubber collapsing `\` plus its
        # character to a single `_`. terraform 1.15.8 refuses all three.
        for label, body in (
                ("in a heredoc body", b'\nlocals {\n  a = <<EOT\nbody\rjunk\nEOT\n}\n'),
                ("before a heredoc terminator", b'\nlocals {\n  a = <<EOT\ntext\n\rEOT\n}\n'),
                ("escaped inside a quoted string", b'\nlocals {\n  a = "x\\\ry"\n}\n')):
            case(tmp, "a carriage return %s is refused" % label,
                 append_bytes("environment/main.tf", body),
                 expect_text="carriage return")
        # The other half of that rule, and the reason it names the LONE return
        # rather than the character: terraform accepts a file written entirely
        # in CRLF, so this guard has to read one — and still catch what is in it.
        # A file of its own, not an append: appending CRLF to main.tf leaves the
        # bytes already in it LF-terminated, and a mixed file does not answer the
        # question this case asks.
        case(tmp, "a file written entirely in CRLF is still read, and still checked",
             append_bytes("environment/zz_crlf.tf",
                          ROGUE_KEY.replace("\n", "\r\n").encode()),
             expect_text="must not OWN")
        # And the position where terraform reads a lone return as ordinary text
        # rather than refusing it: inside a comment, which runs to the newline.
        # Measured on 1.15.8 — `# note\r` at end of file, `# a\rb` and
        # `# note\r\r\n` are all accepted, so refusing them would be this guard
        # rejecting configuration terraform takes. Three forms, because the
        # cheap rule (refuse any return the CRLF pass leaves) accepts none of
        # them: in `\r\r\n` the pass eats the second return and leaves the first.
        for label, comment in (("at end of file", b"# note\r"),
                               ("before a CRLF", b"# note\r\r\n"),
                               ("mid-comment", b"# a\rb\n")):
            case(tmp, "a lone return inside a comment %s is read, not refused" % label,
                 append_bytes("environment/main.tf", b"\n" + comment), expect_ok=True)
        # The accept side of the last-line rule, which the Go suite pins and this
        # one did not: what the refusal must NOT reach. A missing final newline
        # is only ever about the terminator, and terraform accepts both of these.
        # Mirrors whose suites pin different things are the #762 channel.
        case(tmp, "an ordinary last line with no trailing newline is read",
             append_bytes("environment/main.tf", b"\nlocals {\n  a = 1\n}"),
             expect_ok=True)
        case(tmp, "a heredoc that closes before such a line is read",
             append_bytes("environment/main.tf", b"\nlocals {\n  a = <<EOT\ntext\nEOT\n}"),
             expect_ok=True)
        # A byte that is not UTF-8 is carried through rather than raised on, the
        # way tools/kmsrole/hcl.go's string(b) carries it: a reader that dies
        # where its mirror reads on is a divergence, and terraform accepts this
        # file. What is planted after it must still be caught, so this is not an
        # `expect_ok` — it is the guard doing its job across the odd byte.
        case(tmp, "a byte that is not UTF-8, inside a comment, does not stop the scan",
             append_bytes("environment/main.tf",
                          b"\n# \xff\n" + ROGUE_KEY.encode()),
             expect_text="must not OWN")
        # ...but outside one it is refused, because carrying a byte through is
        # not the same as reading past it. Glued to the header the way it is
        # here, `\xffresource` is one word to RESOURCE and the key behind it is
        # invisible: before this case the guard printed `ok` over this file,
        # which is the shape #761 is about. terraform refuses it outright
        # (`Invalid character encoding`), so refusing is also what it does.
        case(tmp, "a byte that is not UTF-8, outside a comment, is refused",
             append_bytes("environment/main.tf",
                          b"\n\xff" + ROGUE_KEY.strip().encode() + b"\n"),
             expect_text="not UTF-8")
        # A BOM is the other direction: terraform parses the file (`fmt -check`
        # reports formatting drift and nothing else), so refusing it would
        # reject configuration the binary takes — but left in the text it sits
        # in front of the first header, which `^\s*` does not match, so that
        # header alone goes unseen (a block further down still matches).
        # Dropped on decode, so the key here is seen and this case is the guard
        # catching it (#765).
        case(tmp, "a leading BOM does not hide the file's first block",
             append_bytes("environment/zz_bom.tf",
                          b"\xef\xbb\xbf" + ROGUE_KEY.strip().encode() + b"\n"),
             expect_text="must not OWN")
        # Only the FIRST U+FEFF is a byte-order mark. A second, or one further
        # in, is `Invalid character` to terraform and glues to the header behind
        # it exactly as `\xff` does — so the key here went unlisted while the
        # block before it was reported, which is a partial scan reported as a
        # whole one. Measured on 1.15.8; the decode strips one mark, and what is
        # left has to be refused rather than read past.
        case(tmp, "a U+FEFF past the leading one is refused",
             append_bytes("environment/zz_bom2.tf",
                          b'resource "google_project" "seen" {\n}\n'
                          b"\xef\xbb\xbf" + ROGUE_KEY.strip().encode() + b"\n"),
             expect_text="leading byte-order mark")
        case(tmp, "two byte-order marks at the start of a file are refused",
             append_bytes("environment/zz_bom3.tf",
                          b"\xef\xbb\xbf\xef\xbb\xbf" + ROGUE_KEY.strip().encode() + b"\n"),
             expect_text="leading byte-order mark")
        # The two positions the scrubbed-line check cannot reach. A body line is
        # never scrubbed; and an escaped byte used to vanish, the scrubber
        # collapsing `\` plus its character to a single `_` — the same hole the
        # carriage return had. terraform 1.15.8 refuses both files.
        case(tmp, "a byte that is not UTF-8 in a heredoc body is refused",
             append_bytes("environment/main.tf",
                          b"\nlocals {\n  a = <<EOT\nbody\xffjunk\nEOT\n}\n"),
             expect_text="not UTF-8")
        case(tmp, "a byte that is not UTF-8 escaped in a quoted string is refused",
             append_bytes("environment/main.tf", b'\nlocals {\n  a = "x\\\xffy"\n}\n'),
             expect_text="not UTF-8")
        # The pair that can splice, which this reader gets right for free and
        # its Go mirror had to be changed for: decoding the whole file up front
        # makes 0xC3 and 0xA9 two independent characters that can never
        # recombine, while a byte-at-a-time scrubber deleting the escape's
        # backslash would have left them adjacent and legal. Pinned on both
        # sides, so the claim that they agree here has an anchor in each.
        case(tmp, "two bytes a deleted backslash could splice are still refused",
             append_bytes("environment/main.tf", b'\nlocals {\n  a = "x\xc3\\\xa9y"\n}\n'),
             expect_text="not UTF-8")
        # And the three positions terraform READS a U+FEFF in — inside a string,
        # a comment and a heredoc body, all measured clean on 1.15.8. The
        # refusal above must reach none of them, or this guard rejects
        # configuration the binary takes.
        for label, body in (
                ("a quoted string", b'\nlocals {\n  a = "p\xef\xbb\xbfq"\n}\n'),
                ("a comment", b"\n# \xef\xbb\xbf\n"),
                ("a heredoc body", b"\nlocals {\n  a = <<EOT\n\xef\xbb\xbf\nEOT\n}\n")):
            case(tmp, "a U+FEFF inside %s is read, not refused" % label,
                 append_bytes("environment/main.tf", body), expect_ok=True)
        # A backslash before a multi-byte character. terraform refuses this file
        # (`Invalid escape sequence`) and this guard reads it, which is the
        # permitted direction — what must not happen is refusing it as invalid
        # UTF-8. Python collapses the pair by CHARACTER and cannot manufacture a
        # broken sequence; its Go mirror consumes a whole rune for the same
        # reason, and this case is what pins the two together.
        case(tmp, "a backslash before a multi-byte character is read",
             append_bytes("environment/main.tf", b'\nlocals {\n  a = "x\\\xc3\xa9y"\n}\n'),
             expect_ok=True)
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
