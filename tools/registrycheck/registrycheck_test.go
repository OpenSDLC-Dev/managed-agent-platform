package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doc is a miniature registry carrying every shape the real one uses: a bare
// live pointer, a shared live tracker with and without a parenthetical, a
// `(delivered; …)` head whose parenthetical names a different, open issue, a
// tail whose issue is open, a multi-issue head, and a `Landed for` clause
// citing a plan file rather than an issue.
const doc = `# DIVERGENCES.md

## Architecture & compatibility notes (not divergences)

- **A note** — Not a mismatch. *Evidence: somewhere*

## Deliberate divergences (CONFIRMED)

- **A settled divergence** — Ours by choice. *Evidence: code* *Tracked: #50 (delivered; the endpoint shipped with plan 12's gate, and the widening is the open #166).*
- **A converged one** — Closed now. *Evidence: code* *Landed for docs/plan/20_gcp-deployment.md slice 2.*
- **A divergence with a live residual** — *Evidence: code* *Tracked: #209 (non-root docker images).*

## Inferences to confirm (INFERRED)

- **One the recording would settle** — *Evidence: code* *Tracked: #78 (a recording would settle what the reference echoes); landed for #45.*
- **Another the same recording would settle** — *Evidence: code* *Tracked: #78 (a recording would settle the ordering); the transport landed for #45, and the fallback itself is #348.*
- **One with its own tracker** — *Evidence: code* *Tracked: #242.*
- **One naming two** — *Evidence: code* *Tracked: #430 (the budget shape), #432 (the redaction rule).*
`

// state answers the fixture's issue numbers. #45, #50 and #74 are delivered;
// #166, #209, #242, #348, #430, #432 and #78 are open.
func state(n int) (bool, bool) {
	switch n {
	case 45, 50, 74:
		return false, true
	case 78, 166, 209, 242, 348, 430, 432:
		return true, true
	}
	return false, false
}

func rules(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func TestFixtureIsCleanOnEveryRung(t *testing.T) {
	if got := Check(doc, nil); len(got) != 0 {
		t.Fatalf("offline rungs on a clean fixture = %v, want none", got)
	}
	if got := Check(doc, state); len(got) != 0 {
		t.Fatalf("every rung on a clean fixture = %v, want none", got)
	}
}

// TestEachRungGoesRed is the point of the guard. A checker that never saw a
// broken file proves nothing, and this one cannot be proved against the real
// registry because the registry is clean — so each rung is shown failing on a
// document mutated to break exactly it, and on nothing else.
func TestEachRungGoesRed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		old     string
		new     string
		rule    string
		wantMsg string
	}{{
		name: "a live tracker that has since closed",
		old:  "*Tracked: #242.*",
		new:  "*Tracked: #50.*",
		rule: "live-tracker-open",
		// The message has to say what to do, because the reader of a red
		// scheduled run is not the person who wrote the entry.
		wantMsg: "demote it to provenance",
	}, {
		name:    "an entry marked delivered whose issue reopened",
		old:     "#50 (delivered;",
		new:     "#242 (delivered;",
		rule:    "provenance-closed",
		wantMsg: "cited as `(delivered)` but is open",
	}, {
		name:    "a shared tracker named without a parenthetical",
		old:     "*Tracked: #78 (a recording would settle the ordering);",
		new:     "*Tracked: #78;",
		rule:    "shared-parenthetical",
		wantMsg: "live tracker of 2 entries",
	}, {
		name:    "an INFERRED entry with no live tracker at all",
		old:     "*Tracked: #242.*",
		new:     "*Landed for #45.*",
		rule:    "inferred-pointer",
		wantMsg: "must name the open issue",
	}, {
		name:    "an INFERRED entry whose only head segment is provenance",
		old:     "*Tracked: #242.*",
		new:     "*Tracked: #45 (delivered).*",
		rule:    "inferred-pointer",
		wantMsg: "must name the open issue",
	}, {
		name:    "a hardcoded intra-file line number",
		old:     "- **A note** — Not a mismatch.",
		new:     "- **A note** — Not a mismatch, as the skills block (line 69) is.",
		rule:    "line-reference",
		wantMsg: "any insertion above it falsifies",
	}, {
		name:    "a pointer clause missing its full stop",
		old:     "*Tracked: #242.*",
		new:     "*Tracked: #242*",
		rule:    "pointer-shape",
		wantMsg: "does not end with a full stop",
	}, {
		name:    "a head segment that is not an issue number",
		old:     "*Tracked: #242.*",
		new:     "*Tracked: the outcome grader.*",
		rule:    "pointer-shape",
		wantMsg: "does not open with `#N`",
	}, {
		name:    "a tracker GitHub does not know",
		old:     "*Tracked: #242.*",
		new:     "*Tracked: #99999.*",
		rule:    "unknown-issue",
		wantMsg: "GitHub does not know it",
	}, {
		// Each of the four below is a way the guard used to go GREEN on a
		// broken clause, which is worse than any false positive it could have.
		name:    "a clause whose interior asterisk stops it matching at all",
		old:     "*Tracked: #242.*",
		new:     "*Tracked: #242 (what *this* entry leaves open).*",
		rule:    "pointer-shape",
		wantMsg: "does not parse as one",
	}, {
		name:    "a clause whose parentheses do not balance",
		old:     "*Tracked: #242.*",
		new:     "*Tracked: #242 (a residual)), #99999 (never reached).*",
		rule:    "pointer-shape",
		wantMsg: "unbalanced parentheses",
	}, {
		name:    "a shared tracker whose parenthetical says nothing",
		old:     "*Tracked: #78 (a recording would settle the ordering);",
		new:     "*Tracked: #78 ();",
		rule:    "shared-parenthetical",
		wantMsg: "live tracker of 2 entries",
	}, {
		name:    "a delivered citation GitHub does not know",
		old:     "#50 (delivered;",
		new:     "#99999 (delivered;",
		rule:    "unknown-issue",
		wantMsg: "cited as delivered but GitHub does not know it",
	}, {
		name:    "a second issue smuggled in after a parenthetical closes",
		old:     "*Tracked: #242.*",
		new:     "*Tracked: #242 (a residual) — but see #50.*",
		rule:    "pointer-shape",
		wantMsg: "continues after its parenthetical closes",
	}, {
		name:    "a label with no space after the colon",
		old:     "*Tracked: #242.*",
		new:     "*Tracked:#242.*",
		rule:    "pointer-shape",
		wantMsg: "does not parse as one",
	}, {
		name:    "a near-miss word demoting a live tracker to provenance",
		old:     "*Tracked: #242.*",
		new:     "*Tracked: #50 (delivereds).*",
		rule:    "live-tracker-open",
		wantMsg: "demote it to provenance",
	}, {
		name:    "a plural line cross-reference",
		old:     "- **A note** — Not a mismatch.",
		new:     "- **A note** — Not a mismatch, as the skills block (lines 69-71) is.",
		rule:    "line-reference",
		wantMsg: "any insertion above it falsifies",
	}, {
		// The colon is what tells `Tracked:` from prose. `:?` made it optional
		// on both labels, so this typo parsed clean and the tracker inside it
		// was checked against nothing.
		name:    "a Tracked clause with no colon",
		old:     "*Tracked: #242.*",
		new:     "*Tracked #242.*",
		rule:    "pointer-shape",
		wantMsg: "does not parse as one",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(doc, tc.old) {
				t.Fatalf("fixture no longer contains %q — the mutation would prove nothing", tc.old)
			}
			mutated := strings.Replace(doc, tc.old, tc.new, 1)
			got := Check(mutated, state)
			var hit *Finding
			for i := range got {
				if got[i].Rule == tc.rule {
					hit = &got[i]
					break
				}
			}
			if hit == nil {
				t.Fatalf("rung %q stayed green on a document that breaks it; findings = %v", tc.rule, rules(got))
			}
			if !strings.Contains(hit.Msg, tc.wantMsg) {
				t.Errorf("message %q does not carry %q", hit.Msg, tc.wantMsg)
			}
			// "and on nothing else" is the half of the claim that says a rung
			// fires for its own reason rather than as a side effect of another
			// one, so it has to be asserted, not just written down.
			if len(got) != 1 {
				t.Errorf("mutation tripped %v; exactly one rung should fire", rules(got))
			}
		})
	}
}

// TestProvenanceIsNotStateChecked pins the two false positives the #452
// prototype hit: a tail may name an open issue, and a `(delivered; …)`
// parenthetical may name one too. Both are prose about other work, not claims
// that the entry is waiting on it.
func TestProvenanceIsNotStateChecked(t *testing.T) {
	if got := Check(doc, state); len(got) != 0 {
		t.Fatalf("clean fixture = %v; #348 in a tail and #166 inside a (delivered) parenthetical are both open and must not be flagged", got)
	}
}

// TestOfflineRungsRunWithoutState is what the merge gate relies on: the shape
// rules must not need the network, and must not quietly do nothing without it.
func TestOfflineRungsRunWithoutState(t *testing.T) {
	mutated := strings.Replace(doc, "*Tracked: #78 (a recording would settle the ordering);", "*Tracked: #78;", 1)
	got := Check(mutated, nil)
	if len(got) != 1 || got[0].Rule != "shared-parenthetical" {
		t.Fatalf("offline Check = %v, want exactly one shared-parenthetical finding", rules(got))
	}
}

// TestSharedIsCountedPerEntry: "shared" means several entries lean on the same
// tracker, so one entry naming an issue twice must not make it shared with
// itself and demand a parenthetical it already has.
func TestSharedIsCountedPerEntry(t *testing.T) {
	one := strings.Replace(doc, "*Tracked: #242.*", "*Tracked: #242, #242.*", 1)
	for _, f := range Check(one, nil) {
		if f.Rule == "shared-parenthetical" {
			t.Fatalf("a single entry naming #242 twice was read as two entries: %v", f)
		}
	}
}

// TestAClauseIsNeverSkippedInSilence is the property behind two of the rows
// above, and the one worth stating on its own: for a guard, going green on a
// clause it could not read is the only unrecoverable outcome. A false positive
// gets argued in review; a silent skip is indistinguishable from a clean file,
// and the rotted tracker inside the unread clause is never looked up again.
func TestAClauseIsNeverSkippedInSilence(t *testing.T) {
	for _, tc := range []struct{ name, clause string }{
		{"interior asterisk", "*Tracked: #50 (the *live* half).*"},
		{"unbalanced parens", "*Tracked: #50 (a)) , #242.*"},
		{"no closing asterisk", "*Tracked: #50 (a residual)."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(doc, "*Tracked: #242.*", tc.clause, 1)
			// #50 is closed, so a clause naming it live is rot. Whether the
			// guard calls that pointer-shape or live-tracker-open is its own
			// business; saying nothing is not.
			if got := Check(mutated, state); len(got) == 0 {
				t.Fatalf("Check went green on %q — a clause it cannot read must never pass", tc.clause)
			}
		})
	}
}

// TestTheInferredRungCannotDisappearQuietly: the rung finds its section by
// name, so renaming the heading would switch it off rather than fail it — and a
// rung that can vanish in silence is the defect this tool exists to catch.
func TestTheInferredRungCannotDisappearQuietly(t *testing.T) {
	renamed := strings.Replace(doc, "## Inferences to confirm (INFERRED)", "## Inferences to confirm", 1)
	got := Check(renamed, state)
	var saw bool
	for _, f := range got {
		if f.Rule == "inferred-section" {
			saw = true
		}
		if f.Rule == "inferred-pointer" {
			t.Errorf("an entry was blamed against a section that no longer exists: %s", f.Msg)
		}
	}
	if !saw {
		t.Fatalf("renaming the INFERRED heading turned a rung off in silence; findings = %v", rules(got))
	}
}

// TestAnEntryCannotHideBehindItsBullet: entry detection is a text match, so a
// bullet spelled any other way used to be invisible to every rung at once.
func TestAnEntryCannotHideBehindItsBullet(t *testing.T) {
	for _, bullet := range []string{"* ", "  - ", "+ "} {
		hidden := doc + bullet + "**A hidden inference** — x. *Evidence: code*\n"
		var saw bool
		for _, f := range Check(hidden, state) {
			if f.Rule == "inferred-pointer" {
				saw = true
			}
		}
		if !saw {
			t.Errorf("an entry bulleted %q escaped every rung", strings.TrimSpace(bullet))
		}
	}
}

// TestASecondIssueIsNeverSwallowed: the greedy `(\(.*\))?` read everything
// between the first "(" and the last ")" as one parenthetical, so an issue
// named after it was never state-checked at all.
func TestASecondIssueIsNeverSwallowed(t *testing.T) {
	s, err := readSegment("#78 (a) — but see #50 (b)")
	if err == "" {
		t.Fatalf("readSegment accepted a segment naming two issues: %+v", s)
	}
	if got, _ := readSegment("#78 (a (nested) b)"); got.issue != 78 || got.paren != "(a (nested) b)" {
		t.Errorf("a nested parenthetical was misread: %+v", got)
	}
}

func TestDeliveredIsAWholeWord(t *testing.T) {
	for _, provenance := range []string{"(delivered)", "(delivered; a note)", "(Delivered)"} {
		if !(segment{paren: provenance}).delivered() {
			t.Errorf("delivered(%q) = false, want true", provenance)
		}
	}
	for _, live := range []string{"(delivereds)", "(delivered-ish)", "(deliverable)", "(the redaction rule)", ""} {
		if (segment{paren: live}).delivered() {
			t.Errorf("delivered(%q) = true — a near miss must not demote a live tracker", live)
		}
	}
}

// TestASecondInferredHeadingDoesNotDisarmTheFirst: `inferred` used to hold the
// last heading seen, so a second INFERRED section would quietly stop the rung
// applying to every entry under the first — the same disappearing-rung defect as
// renaming the heading, one step subtler.
func TestASecondInferredHeadingDoesNotDisarmTheFirst(t *testing.T) {
	two := doc + "\n## More inferences (INFERRED)\n\n- **A second-section entry** — *Evidence: code* *Landed for #45.*\n"
	var first, second bool
	for _, f := range Check(two, state) {
		if f.Rule != "inferred-pointer" {
			continue
		}
		if strings.Contains(f.Msg, "A second-section entry") {
			second = true
		}
		if strings.Contains(f.Msg, "One the recording would settle") {
			first = true
		}
	}
	if !second {
		t.Error("the entry under the second INFERRED heading was not checked")
	}
	// The fixture's first-section entries all carry a live tracker, so none of
	// them should be reported — but they must still be *checked*. Prove that by
	// breaking one and watching it surface.
	broken := strings.Replace(two, "- **One with its own tracker** — *Evidence: code* *Tracked: #242.*",
		"- **One with its own tracker** — *Evidence: code* *Landed for #45.*", 1)
	for _, f := range Check(broken, state) {
		if f.Rule == "inferred-pointer" && strings.Contains(f.Msg, "One with its own tracker") {
			first = true
		}
	}
	if !first {
		t.Error("a pointerless entry under the FIRST INFERRED heading escaped the rung once a second heading existed")
	}
}

func TestBalancedRejectsOnlyRealImbalance(t *testing.T) {
	for _, ok := range []string{"", "#78", "#78 (a; b)", "#55 (delivered; a (nested) b), #50"} {
		if !balanced(ok) {
			t.Errorf("balanced(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"#78 (a", "#78 a)", "#78 (a)) (b"} {
		if balanced(bad) {
			t.Errorf("balanced(%q) = true, want false", bad)
		}
	}
}

func TestSaysWantsContentNotPunctuation(t *testing.T) {
	for _, empty := range []string{"", "()", "(   )", "(-)", "(;)"} {
		if (segment{paren: empty}).says() {
			t.Errorf("says(%q) = true — a parenthetical that says nothing satisfies presence, not purpose", empty)
		}
	}
	if !(segment{paren: "(the redaction rule)"}).says() {
		t.Error("says() rejected a real parenthetical")
	}
}

func TestSplitTopIgnoresSeparatorsInsideParentheses(t *testing.T) {
	got := splitTop("#55 (delivered; files with plan 08, repos with plan 25), #50 (delivered)", ',')
	if len(got) != 2 {
		t.Fatalf("splitTop = %q, want 2 segments", got)
	}
	if s := splitTop("#78 (a; b); landed for #45", ';'); len(s) != 2 {
		t.Fatalf("splitTop on ';' = %q, want 2 parts", s)
	}
}

func TestReferencedListsHeadIssuesOnce(t *testing.T) {
	got := Referenced(doc)
	seen := map[int]int{}
	for _, n := range got {
		seen[n]++
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("#%d listed %d times", n, c)
		}
	}
	for _, want := range []int{50, 209, 78, 242, 430, 432} {
		if seen[want] == 0 {
			t.Errorf("#%d missing from Referenced", want)
		}
	}
	// A tail's issue is provenance and is never state-checked, so it has no
	// business in the lookup set.
	if seen[348] != 0 {
		t.Errorf("#348 is a tail citation and must not be looked up")
	}
}

// TestTheRegistryPassesTheOfflineRungs is the rung that actually guards the
// repository, and the reason this package's test belongs in `make verify`: it
// holds the real file to the grammar its own Format legend promises, on every
// run, for free.
func TestTheRegistryPassesTheOfflineRungs(t *testing.T) {
	path := filepath.Join("..", "..", File)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := Check(string(src), nil); len(got) != 0 {
		for _, f := range got {
			t.Errorf("%s:%s", File, f)
		}
		t.Fatalf("%s fails %d of its own pointer rules", File, len(got))
	}
}

func TestFetchStatesPaginatesAndFillsGaps(t *testing.T) {
	var listed, byNumber int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			listed++
			var b strings.Builder
			b.WriteString("[")
			for i := 1; i <= 100; i++ {
				if i > 1 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"number":%d,"state":"closed"}`, i)
			}
			b.WriteString("]")
			_, _ = w.Write([]byte(b.String()))
			return
		case "2":
			listed++
			_, _ = w.Write([]byte(`[{"number":242,"state":"open"}]`))
			return
		}
		// A number the listing never carried, fetched individually.
		if strings.HasSuffix(r.URL.Path, "/issues/900") {
			byNumber++
			_, _ = w.Write([]byte(`{"number":900,"state":"open"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	lookup, err := fetchStates(context.Background(), srv.URL, "o/r", []int{1, 242, 900, 901})
	if err != nil {
		t.Fatalf("fetchStates: %v", err)
	}
	if listed != 2 || byNumber != 1 {
		t.Fatalf("listed=%d byNumber=%d, want 2 and 1 — the listing must paginate and only gaps may be fetched one by one", listed, byNumber)
	}
	for _, tc := range []struct {
		n     int
		open  bool
		known bool
	}{{1, false, true}, {242, true, true}, {900, true, true}, {901, false, false}} {
		open, known := lookup(tc.n)
		if open != tc.open || known != tc.known {
			t.Errorf("lookup(%d) = (%v,%v), want (%v,%v)", tc.n, open, known, tc.open, tc.known)
		}
	}
}

// TestFetchStatesTellsAbsentFromUnreachable pins the trap the substring test
// walked into: every error here names the URL it failed on, so a transport
// failure fetching /issues/404 carries "404" in its message. Issue #404 is a
// real number in this repository's range.
func TestFetchStatesTellsAbsentFromUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"a genuinely absent issue", http.StatusNotFound, false},
		{"the server failing on issue 404", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/issues/404") {
					http.Error(w, `{"message":"nope"}`, tc.status)
					return
				}
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()

			lookup, err := fetchStates(context.Background(), srv.URL, "o/r", []int{404})
			if tc.wantErr {
				if err == nil {
					t.Fatal("a server error on issue #404 was reported as an absent issue")
				}
				return
			}
			if err != nil {
				t.Fatalf("a 404 must be a finding, not a crash: %v", err)
			}
			if _, known := lookup(404); known {
				t.Error("lookup(404) claims to know an issue GitHub does not serve")
			}
		})
	}
}

func TestFetchStatesNamesTheRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1787400000")
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := fetchStates(context.Background(), srv.URL, "o/r", nil)
	if err == nil || !strings.Contains(err.Error(), "rate limit is exhausted") {
		t.Fatalf("err = %v, want it to name the rate limit — a red run must not read as a rotted registry", err)
	}
}
