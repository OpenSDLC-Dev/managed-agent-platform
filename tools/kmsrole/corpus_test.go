package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Go half of the shared .tf corpus. hcl.go is a hand-written mirror of
// deploy/gcp/check_split.py's reader, and until #762 nothing executable tied the
// two together: three consecutive commits on #760 read the same heredoc rule
// three different ways, and each divergence was caught by a reviewer running
// terraform rather than by a test. ../tfcorpus is that tie — one directory of
// .tf files with, beside each, the blocks a reader must return or the refusal it
// must give. check_split_test.py reads the same manifest, so a rule that moves
// in one reader and not the other now fails on the side that did not move.
//
// The corpus deliberately pins only what BOTH readers can say. Line numbers,
// Kind and Label as separate fields, the scrubbed bytes and the guard's own
// rules are things one side can express and the other cannot, so they stay in
// each suite's own tests — the rows here sit at a coarser grain beside them
// rather than replacing them.
type corpusCase struct {
	File string `json:"file"`
	Why  string `json:"why"`
	// The exit `terraform fmt -check` gives this file: 0 formatted, 3
	// formatting drift, 1 or 2 a parse error. A pointer because a row that
	// omits it must be a named failure and not a silent zero; `make
	// tf-corpus-check` is what puts the value to the binary.
	Fmt *int `json:"fmt"`
	// Exactly one of the two. Blocks is every top-level block a reader must
	// return, in order, spelled the way Terraform addresses it; Refuse is a
	// substring both readers' messages contain.
	//
	// The rule is over VALUES, not over which keys are present: this decoder
	// cannot tell an absent key from a JSON null or an empty string, so a rule
	// stated over keys would be a rule tools/tfcorpus/loader.py states
	// differently. `[]` is a real expectation — a file read cleanly with no
	// top-level blocks in it — and decodes non-nil, which is what separates it
	// from both.
	Blocks []string `json:"blocks"`
	Refuse string   `json:"refuse"`
	// Set on a row where terraform TAKES the file and a reader refuses it
	// anyway. Read by the oracle, which fails when the two disagree, so the
	// standing set of deliberate refusals cannot grow without a row saying so.
	TerraformAccepts bool `json:"terraform_accepts"`
}

func TestTheSharedCorpus(t *testing.T) {
	const dir = "../tfcorpus"
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("the corpus manifest: %v", err)
	}
	var m struct{ Cases []corpusCase }
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the corpus manifest: %v", err)
	}
	// Reading nothing must never read as clean. An empty manifest, a row
	// naming a file that is not there, and a .tf that no row names would each
	// leave this test green over a corpus it had not checked — which is the
	// failure mode the corpus exists to close.
	if len(m.Cases) == 0 {
		t.Fatal("the corpus manifest lists no cases")
	}
	onDisk, err := filepath.Glob(filepath.Join(dir, "cases", "*.tf"))
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, p := range onDisk {
		present[filepath.Base(p)] = true
	}
	listed := map[string]bool{}
	for _, c := range m.Cases {
		listed[c.File] = true
		if !present[c.File] {
			// Without this the row below would fail on the open() rather than
			// on the manifest, and a "refuse" row could even pass on it.
			t.Errorf("%s is named by a manifest row and is not in cases/", c.File)
		}
	}
	for _, p := range onDisk {
		if !listed[filepath.Base(p)] {
			t.Errorf("%s is in the corpus and no manifest row names it, so nothing checks it", filepath.Base(p))
		}
	}
	// And what neither loop can see: two rows naming the same file leave a
	// third unchecked with every name still listed.
	if len(onDisk) != len(m.Cases) {
		t.Errorf("the corpus holds %d .tf files and the manifest %d rows", len(onDisk), len(m.Cases))
	}

	for _, c := range m.Cases {
		if !present[c.File] {
			continue // already reported above
		}
		if c.Fmt == nil {
			t.Errorf("%s: no `fmt` exit recorded — `make tf-corpus-check` is what puts it to the binary", c.File)
			continue
		}
		// Required here as in loader.py: a row nobody can review is a row
		// nobody can tell from a row that pins nothing, and this is what the
		// failures below read aloud.
		if c.Why == "" {
			t.Errorf("%s: no `why` — a row has to say what it pins", c.File)
			continue
		}
		// A row states one thing or the other. Neither would check nothing;
		// both would let a reader satisfy the row by refusing.
		if (c.Refuse != "") == (c.Blocks != nil) {
			t.Errorf("%s: a row names either the blocks a reader returns or the refusal it gives, never both and never neither", c.File)
			continue
		}
		got, err := tfBlocks(filepath.Join(dir, "cases", c.File))
		if c.Refuse != "" {
			switch {
			case err == nil:
				t.Errorf("%s: read %d blocks, want a refusal mentioning %q — %s", c.File, len(got), c.Refuse, c.Why)
			case !strings.Contains(err.Error(), c.Refuse):
				t.Errorf("%s: refused with %v, want one mentioning %q", c.File, err, c.Refuse)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v — want %v (%s)", c.File, err, c.Blocks, c.Why)
			continue
		}
		addrs := make([]string, 0, len(got))
		for _, b := range got {
			if b.Type == "module" {
				addrs = append(addrs, "module."+b.Label)
			} else {
				addrs = append(addrs, b.Kind+"."+b.Label)
			}
		}
		if strings.Join(addrs, ",") != strings.Join(c.Blocks, ",") {
			t.Errorf("%s: read %v, want %v — %s", c.File, addrs, c.Blocks, c.Why)
		}
	}
}
