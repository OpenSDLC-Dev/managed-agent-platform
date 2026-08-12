package evals

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The registered trial set, pinned. Offline, so it runs on every `make verify`
// rather than only when someone pays for a live run.
//
// It exists because nothing tied the suite to the two documents that count it.
// #331 took the set from fourteen to fifteen without touching either, and both
// read "fourteen" for three weeks until #358's parking made them true again as a
// side effect — a doc that silently describes a different suite than the one
// that runs. `make verify` runs only go vet and gofmt over this package and
// grades no coverage on it, so a test is the only thing here that can fail.
//
// The IDs, not merely the count: a rename or a swap holds the count and changes
// what the suite proves. They are also the report's row keys and the artifacts'
// filenames, so a duplicate would silently merge two trials' evidence.
func TestTaskSetIsPinned(t *testing.T) {
	want := []string{
		"fib-quickstart", "echo-notool", "shell-state",
		"edit-config", "needle-search", "perm-allow", "perm-deny",
		"exit-code", "journal-multiturn", "view-range", "skill-answer",
		"file-answer", "repo-answer", "outcome-satisfy", "outcome-revise",
	}

	got := make([]string, 0, len(want))
	seen := map[string]int{}
	for _, task := range tasks() {
		got = append(got, task.ID)
		seen[task.ID]++
	}
	for _, id := range got {
		if seen[id] > 1 {
			t.Errorf("trial ID %q is registered %d times; an ID keys the report's rows "+
				"and its transcript file, so duplicates merge two trials' evidence", id, seen[id])
			seen[id] = 1 // reported once
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the registered trial set changed.\n got: %v\nwant: %v\n\n"+
			"If the change is intended, update the list in this test AND the trial count "+
			"spelled out in words in README.md's Live-system evals row and in "+
			"docs/ARCHITECTURE.md's eval paragraph — TestDocsSpellTheTrialCount holds "+
			"those two to it.", got, want)
	}
}

// TestDocsSpellTheTrialCount is the second half of the same gate: the list above
// fails when the set changes, but only a reader acts on its message, and the
// drift it was written for happened because nobody read one.
//
// It matches the whole phrase each document states the count in, not the count
// word alone. Alone would be fooled by the word arriving for an unrelated
// reason, and that is not hypothetical: docs/ARCHITECTURE.md already says
// "sixteen" about an nginx buffer, so a bare-word check would go quietly green
// for that document the day a sixteenth trial is registered — the one day it has
// to fire.
//
// Matched after collapsing whitespace, because both documents are hard-wrapped
// and today's ARCHITECTURE.md already breaks this very phrase across two lines.
// The cost is that rewording the sentence fails this test until the phrase here
// is updated too; that is the intended direction — the sentence carrying the
// count is not one to reword absent-mindedly.
func TestDocsSpellTheTrialCount(t *testing.T) {
	n := len(tasks())
	if n < 0 || n >= len(countWords) {
		t.Fatalf("the suite registers %d trials and this test cannot spell that number; "+
			"extend countWords", n)
	}

	for _, doc := range []struct{ path, phrase string }{
		{"README.md", "%s regression tasks"},
		{filepath.Join("docs", "ARCHITECTURE.md"), "%s deterministic regression tasks"},
	} {
		b, err := os.ReadFile(filepath.Join(evalsRepoRoot(), doc.path))
		if err != nil {
			t.Fatalf("read %s: %v", doc.path, err)
		}
		want := fmt.Sprintf(doc.phrase, countWords[n])
		if !strings.Contains(collapseSpace(strings.ToLower(string(b))), want) {
			t.Errorf("%s never says %q, but the suite registers %d trials and that document "+
				"states the count in words", doc.path, want, n)
		}
	}
}

// collapseSpace reduces every run of whitespace to a single space, so a phrase
// a hard-wrapped document split across lines still matches.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// countWords spells the counts the check above can assert. Indexed by the
// number, so countWords[15] is "fifteen". A suite that outgrows it fails loudly
// rather than passing quietly.
var countWords = []string{
	"zero", "one", "two", "three", "four", "five", "six", "seven", "eight",
	"nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen",
	"sixteen", "seventeen", "eighteen", "nineteen", "twenty",
}
