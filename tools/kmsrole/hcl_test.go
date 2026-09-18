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
		got, err := scrubTF(tc.in)
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
		if _, err := scrubTF(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
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

// TestAPlainHeredocNeedsAnUnindentedTerminator is the reader's one deliberate
// departure from check_split.py (#758). Terraform ends `<<EOT` only at a line
// that IS the terminator; matching an indented one would end the string early
// for the reader while Terraform read on, and everything between is
// configuration to one and text to the other — which is how a grant that does
// not exist gets read as one.
func TestAPlainHeredocNeedsAnUnindentedTerminator(t *testing.T) {
	ghost := `locals {
  note = <<EOT
    EOT
}
resource "google_kms_crypto_key_iam_member" "ghost" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyDecrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
}
EOT
}
`
	blocks, err := tfBlocks(writeTF(t, ghost))
	if err != nil {
		t.Fatalf("tfBlocks: %v", err)
	}
	for _, b := range blocks {
		if b.Label == "ghost" {
			t.Fatalf("a resource inside a plain heredoc's body was read as configuration (%s)", b.Addr())
		}
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
	} {
		if _, err := tfBlocks(writeTF(t, tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one mentioning %q", tc.name, err, tc.want)
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
