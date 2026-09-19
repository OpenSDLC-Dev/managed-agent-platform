package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTF(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.tf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScrubKeepsTextAndDropsStructure(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"line comment", `role = "a" # role = "b"`, `role = "a" `},
		{"slash comment", `role = "a" // role = "b"`, `role = "a" `},
		{"brace in string", `name = "a{b}c"`, `name = "a_b_c"`},
		{"heredoc in string", `cmd = "cat <<EOF"`, `cmd = "cat __EOF"`},
		{"hash in string", `name = "a#b"`, `name = "a_b"`},
		{"interpolation", `m = "x:${data.a.b}"`, `m = "x:$_data.a.b_"`},
		{"directive", `m = "${%{ if true }y%{ endif }}"`, `m = "$_%_ if true _y%_ endif __"`},
		{"escape", `name = "a\"{b"`, `name = "a__b"`},
	} {
		got, _, err := scrubTF(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: scrubTF(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if braces(got) != 0 {
			t.Errorf("%s: scrubbed %q is not brace-neutral", tc.name, got)
		}
	}
}

func TestScrubRefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"nested quote in interpolation", `m = "${format("%s", "{")}"`, "cannot be read by this guard"},
		{"open string", `m = "${`, "still open at end of line"},
		{"block comment", `/* role = "x" */`, "block comments are not supported"},
	} {
		if _, _, err := scrubTF(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: scrubTF(%q) error = %v, want one mentioning %q", tc.name, tc.in, err, tc.want)
		}
	}
}

// TestAHeredocBodyIsNeitherStructureNorValue is the case that decides whether
// prose can answer for configuration: a description whose text happens to
// contain an assignment, or a brace, must contribute neither.
func TestAHeredocBodyIsNeitherStructureNorValue(t *testing.T) {
	p := writeTF(t, `resource "google_kms_crypto_key_iam_member" "x" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyEncrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
  description   = <<-EOT
    role = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
    an unbalanced { brace, and a resource "google_x" "y" { header
  EOT
}
`)
	blocks, err := tfBlocks(p)
	if err != nil {
		t.Fatalf("tfBlocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("read %d blocks, want 1", len(blocks))
	}
	label, g, ok, err := readGrant(blocks[0], map[string]bool{"executor": true})
	if err != nil {
		t.Fatalf("readGrant: %v", err)
	}
	if !ok || label != "executor" || g.Role != "roles/cloudkms.cryptoKeyEncrypter" {
		t.Fatalf("readGrant = %q %+v %v, want the executor's Encrypter role", label, g, ok)
	}
	// And the body must not carry the prose at all: tfBlocks drops a heredoc
	// line rather than storing it, and this is the assertion that fails when
	// that filter goes. Scrubbing is only the second line of defence — a
	// heredoc line's scrubbed copy is empty, so attr would not match it even if
	// it were stored — and a reader reaching for Raw has no such protection.
	for _, l := range blocks[0].Body {
		if strings.Contains(l.Raw, "EncrypterDecrypter") {
			t.Errorf("line %d of the heredoc body is in the block body: %q", l.N, l.Raw)
		}
	}
}

// TestAnIndentedTerminatorEndsAPlainHeredoc pins the rule to what the binary
// does rather than to what the marker looks like it should mean. terraform
// 1.15.8 ends `<<EOT` at `    EOT` exactly as it ends `<<-EOT` there — the
// marker dedents the body, it does not move the end — and this reader required
// an exact match until #758.
//
// The fixture is the shape that makes the exact rule silent rather than loud:
// the second heredoc's bare `EOT` closes the FIRST one for a reader still
// inside it, so every brace it swallowed balances again at end of file and the
// refusals that would otherwise fire — unterminated heredoc, unbalanced braces
// — both stay quiet. `terraform fmt -check` exits 0 on it, so the tree can
// carry it. Under the exact rule this reads zero blocks and reports nothing.
func TestAnIndentedTerminatorEndsAPlainHeredoc(t *testing.T) {
	swallowed := `locals {
  a = <<EOT
    EOT
}
resource "google_kms_crypto_key_iam_member" "real" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyDecrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
}
locals {
  b = <<EOT2
EOT
EOT2
}
`
	blocks, err := tfBlocks(writeTF(t, swallowed))
	if err != nil {
		t.Fatalf("tfBlocks: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Label != "real" {
		t.Fatalf("read %d blocks, want the one resource Terraform sees after the heredoc", len(blocks))
	}
	// And the other direction: a terminator that only looks like one leaves the
	// heredoc open, so the reader refuses rather than reading the body as
	// configuration. `EOTX` is not the word, trimmed or not.
	if _, err := tfBlocks(writeTF(t, "locals {\n  a = <<EOT\nEOTX\n}\n")); err == nil {
		t.Fatal("a heredoc closed by EOTX was read as terminated")
	}
	// Four paddings, not only the indented one, because every narrower trim
	// passes a suite that pins fewer: TrimLeft(" \t") reads on past `EOT   `,
	// and Trim(" \t") reads on past the non-breaking space. Terraform honours
	// all four — measured, and `terraform fmt -check` leaves every byte of them
	// alone — so a tree can carry any one.
	for _, term := range []string{"    EOT", "EOT   ", "\tEOT", " EOT"} {
		got, err := tfBlocks(writeTF(t, "locals {\n  a = <<EOT\n"+term+"\n}\nresource \"a\" \"b\" {\n}\n"))
		if err != nil {
			t.Fatalf("terminator %q: %v", term, err)
		}
		if len(got) != 1 || got[0].Label != "b" {
			t.Fatalf("terminator %q: read %d blocks, want the one resource", term, len(got))
		}
	}
	// And the padding terraform does NOT honour, which is why this reader never
	// had the bug #761 is named for: strings.TrimSpace leaves U+001C-U+001F
	// alone, so the terminator they forge closes nothing. Pinned here because
	// the claim is load-bearing in this file's header — a later harmonisation
	// toward Python's wider trim would otherwise re-open it with the suite green.
	sep, err := tfBlocks(writeTF(t, "locals {\n  a = <<EOT\n\x1cEOT\n{\nEOT\n}\nresource \"a\" \"b\" {\n}\n"))
	if err != nil {
		t.Fatalf("separator-forged terminator: %v", err)
	}
	if len(sep) != 1 || sep[0].Label != "b" {
		t.Fatalf("read %d blocks after a separator-forged terminator, want the one resource", len(sep))
	}
	// The indented form still terminates where it says it does.
	ok, err := tfBlocks(writeTF(t, "locals {\n  note = <<-EOT\n    text\n    EOT\n}\n\nresource \"a\" \"b\" {\n}\n"))
	if err != nil {
		t.Fatalf("indented heredoc: %v", err)
	}
	if len(ok) != 1 || ok[0].Label != "b" {
		t.Fatalf("read %d blocks after an indented heredoc, want the one resource", len(ok))
	}
}

func TestTheReaderRefusesRatherThanShortRead(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			"unterminated heredoc",
			"variable \"v\" {\n  description = <<-EOT\n    text\n}\n",
			"is never terminated",
		},
		{
			"file ends inside a block",
			"resource \"a\" \"b\" {\n  x = 1\n",
			"the file ended inside",
		},
		{
			"unbalanced at end of file",
			"resource \"a\" \"b\" {\n  x = 1\n}\n}\n",
			"unbalanced braces at end of file",
		},
		{
			// HCL wants a newline after the terminator and the file's last line
			// never gets one, so terraform 1.15.8 answers `Unterminated
			// template string` — while the SAME file with a trailing newline it
			// accepts, and an ordinary last line without one it accepts too.
			// The reader closed the string here and answered anyway. Inside a
			// block the braces then caught it, but blamed a brace: this fixture
			// is the shape where they balance and nothing fires at all.
			"terminator is the last line of a file with no trailing newline",
			"resource \"a\" \"b\" {\n  x = 1\n}\n\ny = <<EOT\ntext\nEOT",
			"no trailing newline",
		},
		{
			// The same boundary one level in, where the reader used to refuse
			// for the wrong reason — a reader sent after a brace that is not
			// the problem is a reader that names what it could not read.
			"the same terminator, inside a block",
			"resource \"a\" \"b\" {\n  x = <<EOT\ntext\nEOT",
			"no trailing newline",
		},
		{
			// Terraform refuses a bare CR among structure, inside a quoted
			// string and inside a heredoc body — `Invalid character` on 1.15.8,
			// or `Invalid multi-line string` in the quoted case — while CRLF
			// throughout is accepted. Go breaks lines on \n alone and Python's
			// read_text() broke on a lone \r, so this file was one line to one
			// reader and four to the other: a quiet short read on the Go side,
			// where only the first header can still match.
			"bare CR line endings",
			"resource \"a\" \"b\" {\r  x = 1\r}\r",
			"carriage return",
		},
		{
			// The body half of that check, which nothing else reaches: body
			// lines are never scrubbed, so they are tested raw.
			"a lone CR inside a heredoc body",
			"resource \"a\" \"b\" {\n  x = <<EOT\nbody\rjunk\nEOT\n}\n",
			"carriage return",
		},
		{
			// And the one that would close the string in the wrong place: a
			// return before the terminator word is padding to TrimSpace and
			// not to terraform.
			"a lone CR before a heredoc terminator",
			"resource \"a\" \"b\" {\n  x = <<EOT\ntext\n\rEOT\n}\n",
			"carriage return",
		},
		{
			// Escaped, where the scrubber used to collapse the pair to one `_`
			// and hide the return from every later look. terraform 1.15.8 gives
			// three errors on these bytes.
			"a lone CR escaped inside a quoted string",
			"resource \"a\" \"b\" {\n  x = \"a\\\rb\"\n}\n",
			"carriage return",
		},
		{
			// The byte terraform refuses as an `Invalid character encoding`,
			// where it hides a block rather than stopping the scan: it glues to
			// `resource`, so the header regexp matches nothing and a reader
			// that answered here would report over a file with a
			// google_kms_crypto_key in it. The same byte inside a comment is
			// read — that case is in the accept table below.
			"a byte that is not UTF-8, outside a comment",
			"\xffresource \"a\" \"b\" {\n  x = 1\n}\n",
			"not UTF-8",
		},
		{
			// A body line is never scrubbed, so the check above it is the only
			// one that reaches here. terraform 1.15.8: `Invalid character
			// encoding` plus `Unterminated template string`.
			"a byte that is not UTF-8 in a heredoc body",
			"locals {\n  x = <<EOT\na\xffb\nEOT\n}\nresource \"a\" \"b\" {\n}\n",
			"not UTF-8",
		},
		{
			// Escaped, where the scrubber used to collapse the pair to one `_`
			// and hide the byte from the check — the same hole the carriage
			// return had. terraform gives three errors on these bytes.
			"a byte that is not UTF-8 escaped inside a quoted string",
			"locals {\n  x = \"a\\\xffb\"\n}\nresource \"a\" \"b\" {\n}\n",
			"not UTF-8",
		},
		{
			// The pair that can splice. This scrubber deletes an escape's
			// backslash, so keeping the escaped byte would have put 0xA9 right
			// after the 0xC3 the byte before it wrote — a well-formed `é` the
			// file never contained, which utf8.ValidString accepts. terraform
			// gives this file three errors and check_split.py refuses it, so a
			// reader that read it would be the only one of the three that did.
			// This is why the check reads the raw code, not the scrubbed copy.
			"two bytes a deleted backslash could splice into a valid rune",
			"locals {\n  x = \"a\xc3\\\xa9b\"\n}\nresource \"a\" \"b\" {\n}\n",
			"not UTF-8",
		},
		{
			// Only the FIRST U+FEFF is a byte-order mark. A second, or one
			// further in, is `Invalid character` to terraform and glues to the
			// header behind it exactly as `\xff` does — which is how the
			// `hidden` resource here went unlisted while `first` was reported.
			"a U+FEFF among structure, past the leading one",
			"resource \"a\" \"first\" {\n}\n\ufeffresource \"a\" \"hidden\" {\n}\n",
			"not the file's leading byte-order mark",
		},
		{
			"two byte-order marks at the start of the file",
			"\ufeff\ufeffresource \"a\" \"b\" {\n}\n",
			"not the file's leading byte-order mark",
		},
	} {
		if _, err := tfBlocks(writeTF(t, tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one mentioning %q", tc.name, err, tc.want)
		}
	}
}

// CRLF is the other half of the CR rule, and the reason it is a refusal of the
// LONE carriage return rather than of the character: terraform accepts a file
// written entirely in CRLF, so this reader has to as well. A last line with no
// trailing newline is accepted for the same reason — terraform takes it, as
// long as it is not a heredoc terminator.
func TestCRLFAndAnUnterminatedLastLineAreStillRead(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"CRLF throughout", "resource \"a\" \"b\" {\r\n  x = 1\r\n}\r\n"},
		{"ordinary last line, no trailing newline", "resource \"a\" \"b\" {\n  x = 1\n}"},
		{"heredoc closed, then a last line with no newline", "resource \"a\" \"b\" {\n  x = <<EOT\ntext\nEOT\n}"},
		// A comment runs to the newline, so a lone return inside one is
		// ordinary text: terraform 1.15.8 accepts all three of these, and
		// refusing them would be this reader rejecting configuration the
		// binary takes. The `\r\r\n` form matters on its own — the CRLF pass
		// eats the second return and leaves the first, which is exactly what a
		// whole-file "any return left over" rule trips on.
		{"lone return in a comment at end of file", "resource \"a\" \"b\" {\n  x = 1\n}\n# note\r"},
		{"lone return in a comment before a CRLF", "resource \"a\" \"b\" {\n  x = 1\n}\n# note\r\r\n"},
		{"lone return mid-comment", "# a\rb\nresource \"a\" \"b\" {\n  x = 1\n}\n"},
		// The Go half of a claim deploy/gcp/check_split.py makes about this
		// reader: it decodes with surrogateescape rather than strict BECAUSE
		// this side carries a byte that is not UTF-8 through instead of dying,
		// and terraform accepts one in a comment (`# caf\xe9` is fmt- and
		// validate-clean on 1.15.8). Nothing here pinned that, so a later
		// utf8.Valid guard on this side would reopen the divergence with the Go
		// suite still green and the Python comment still asserting it closed.
		{"a byte that is not UTF-8, inside a comment", "# caf\xe9\nresource \"a\" \"b\" {\n  x = 1\n}\n"},
		// A BOM parses for terraform — `fmt -check` reports formatting drift
		// and nothing else — so refusing it would reject configuration the
		// binary takes. Left in place it is worse than harmless: `^\s*` does
		// not match U+FEFF, so the header on the file's first line went unseen
		// behind it, with nothing said (#765). Only that header — a block
		// further down still matched — which is why the refusal below handles
		// the marks the strip does not take.
		{"a leading BOM", "\xef\xbb\xbfresource \"a\" \"b\" {\n  x = 1\n}\n"},
		// The three positions terraform reads a U+FEFF in, all measured clean
		// on 1.15.8. The refusal above must reach none of them, or this guard
		// rejects configuration the binary takes.
		{"a BOM inside a quoted string", "resource \"a\" \"b\" {\n  x = \"p\ufeffq\"\n}\n"},
		{"a BOM inside a comment", "# \ufeff\nresource \"a\" \"b\" {\n  x = 1\n}\n"},
		{"a BOM inside a heredoc body", "resource \"a\" \"b\" {\n  x = <<EOT\n\ufeff\nEOT\n}\n"},
		// A backslash before a multi-byte character. terraform refuses this
		// file (`Invalid escape sequence`) and this reader reads it, which is
		// the permitted direction — what must NOT happen is refusing it as
		// invalid UTF-8, which is what a scrubber consuming one byte instead of
		// one rune made of it: the continuation bytes were left behind and the
		// line became invalid UTF-8 that the file never contained.
		// Three widths, not one: the byte-at-a-time version this replaces left
		// one continuation byte behind for a 2-byte rune, two for a 3-byte and
		// three for a 4-byte, so a fixture of a single width pins only a third
		// of the arithmetic. terraform refuses all three for `Invalid escape
		// sequence` and this reader reads all three, which is the permitted
		// direction — what must not happen is refusing them as invalid UTF-8
		// the reader itself manufactured.
		{"a backslash before a 2-byte character", "resource \"a\" \"b\" {\n  x = \"a\\éb\"\n}\n"},
		{"a backslash before a 3-byte character", "resource \"a\" \"b\" {\n  x = \"a\\€b\"\n}\n"},
		{"a backslash before a 4-byte character", "resource \"a\" \"b\" {\n  x = \"a\\\U0001f600b\"\n}\n"},
	} {
		got, err := tfBlocks(writeTF(t, tc.body))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(got) != 1 || got[0].Label != "b" {
			t.Errorf("%s: read %d blocks, want the one resource", tc.name, len(got))
		}
	}
}

// TestOnlyTopLevelBlocksAreRead: a `resource` line nested inside another block
// is not a resource, and an indented one still is — a reader that only works
// once `terraform fmt` has run can be bypassed by running the two in the wrong
// order. Modules are read too, because they are how a grant escapes the files
// this guard looked at.
func TestOnlyTopLevelBlocksAreRead(t *testing.T) {
	blocks, err := tfBlocks(writeTF(t, `  resource "indented" "a" {
  x = 1
}

module "child" {
  source = "./child"
}

locals {
  s = "resource \"nested\" \"b\" {"
}
`))
	if err != nil {
		t.Fatalf("tfBlocks: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("read %d blocks (%+v), want the indented resource and the module", len(blocks), blocks)
	}
	if blocks[0].Type != "resource" || blocks[0].Kind != "indented" || blocks[0].N != 1 {
		t.Errorf("first block = %+v", blocks[0])
	}
	if blocks[1].Type != "module" || blocks[1].Label != "child" {
		t.Errorf("second block = %+v", blocks[1])
	}
	if got := blocks[1].Addr(); !strings.HasPrefix(got, "module.child (") {
		t.Errorf("module Addr() = %q", got)
	}
}

func TestADuplicateAttributeIsRefused(t *testing.T) {
	blocks, err := tfBlocks(writeTF(t, `resource "a" "b" {
  role = "one"
  role = "two"
}
`))
	if err != nil {
		t.Fatalf("tfBlocks: %v", err)
	}
	if _, _, err := blocks[0].attr("role"); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("attr error = %v, want one naming the duplicate count", err)
	}
	// An attribute that is simply absent is not an error — only the callers
	// that require it say so, and they say which one is missing.
	if _, ok, err := blocks[0].attr("member"); ok || err != nil {
		t.Fatalf("attr(member) = %v, %v; want absent and no error", ok, err)
	}
}

// TestNestedNamesEveryBlock: detection is structural, so a block written on one
// line — which nets to zero braces — is still seen; an interpolated value is
// still not a block; and EVERY nested block is named, because returning only the
// first let an allowed `lifecycle` shadow a `condition` written after it.
func TestNestedNamesEveryBlock(t *testing.T) {
	blocks, err := tfBlocks(writeTF(t, `resource "a" "multi" {
  condition {
    title = "t"
  }
}

resource "a" "oneline" {
  condition { expression = "false" }
}

resource "a" "meta" {
  lifecycle {
    prevent_destroy = true
  }
}

resource "a" "shadowed" {
  lifecycle {
    prevent_destroy = true
  }
  condition {
    expression = "false"
  }
}

resource "a" "plain" {
  member = "serviceAccount:${data.google_service_account.x.email}"
  labels = {
    a = "b"
  }
}
`))
	if err != nil {
		t.Fatalf("tfBlocks: %v", err)
	}
	want := map[string][]string{
		"multi":    {"condition"},
		"oneline":  {"condition"},
		"meta":     {"lifecycle"},
		"shadowed": {"lifecycle", "condition"},
		"plain":    nil,
	}
	// Without this the loop asserts nothing when the reader returns no blocks,
	// and the shadowing case this test exists for would go unchecked.
	if len(blocks) != len(want) {
		t.Fatalf("read %d blocks, want %d", len(blocks), len(want))
	}
	for _, b := range blocks {
		got := b.nested()
		w := want[b.Label]
		if len(got) != len(w) {
			t.Errorf("%s.nested() = %v, want %v", b.Label, got, w)
			continue
		}
		for i := range w {
			if got[i] != w[i] {
				t.Errorf("%s.nested() = %v, want %v", b.Label, got, w)
				break
			}
		}
	}
}

func TestAddrNamesTheFileAndLine(t *testing.T) {
	blocks, err := tfBlocks(writeTF(t, "\nresource \"kind\" \"label\" {\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := blocks[0].Addr(); !strings.HasPrefix(got, "kind.label (") || !strings.HasSuffix(got, ":2)") {
		t.Errorf("Addr() = %q, want kind.label at line 2", got)
	}
}
