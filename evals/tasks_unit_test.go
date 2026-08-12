package evals

import (
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
// A tripwire, not a proof. It asserts the word appears in the file at all, which
// survives a reflow of the paragraph but would be fooled by the same word
// arriving for an unrelated reason. That is the right trade here: the failure it
// must catch is a count left behind, and for that a substring is enough.
func TestDocsSpellTheTrialCount(t *testing.T) {
	n := len(tasks())
	if n < 0 || n >= len(countWords) {
		t.Fatalf("the suite registers %d trials and this test cannot spell that number; "+
			"extend countWords", n)
	}
	word := countWords[n]

	for _, doc := range []string{"README.md", filepath.Join("docs", "ARCHITECTURE.md")} {
		b, err := os.ReadFile(filepath.Join(evalsRepoRoot(), doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		if !strings.Contains(strings.ToLower(string(b)), word) {
			t.Errorf("%s never says %q, but the suite registers %d trials and that document "+
				"states the count in words", doc, word, n)
		}
	}
}

// countWords spells the counts the check above can assert. Indexed by the
// number, so countWords[15] is "fifteen". A suite that outgrows it fails loudly
// rather than passing quietly.
var countWords = []string{
	"zero", "one", "two", "three", "four", "five", "six", "seven", "eight",
	"nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen",
	"sixteen", "seventeen", "eighteen", "nineteen", "twenty",
}
