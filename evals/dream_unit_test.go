package evals

import "testing"

// The in-place dream's tombstone grader, on the tombstones nightly runs wrote
// and on the ones it must still refuse (#951).
func TestTombstoneNamesSuccessor(t *testing.T) {
	store := map[string]string{
		"/facts/vendor-portal.md":  "Superseded by facts/expenses.md.\n",
		"/facts/Expenses-2026.md":  "# Expenses\n\nFiled at finance.internal.\n",
		"/MEMORY.md":               "# Memory\n\n- facts/expenses.md: filed at finance.internal\n- Remove: facts/vendor-portal.md\n",
		"/facts/build.md":          "# Build\n\nValidated fact: the build runs `make verify`, which takes about nine minutes.\n",
		"/facts/expenses.md":       "# Expenses\n\nExpenses are filed in the internal finance app at finance.internal.\n",
		"/facts/expense-filing.md": "# Expense filing\n\nFiled at finance.internal since 2026-09-01.\n",
	}
	for _, tc := range []struct {
		name, tomb string
		want       bool
	}{
		// Run 89's tombstone: the successor is the memory, and that memory
		// states the new place.
		{"names the successor memory", "Superseded by facts/expenses.md.", true},
		// Run 78's: the same, in code spans and with more around it.
		{"names it in a code span", "Tombstone — superseded by `facts/expense-filing.md` (the portal was shut down).", true},
		{"names it from the store root", "Superseded by /facts/expenses.md.", true},
		// A path is the model's to spell, capitals included.
		{"names a path with capitals", "Superseded by facts/Expenses-2026.md.", true},
		{"names the place itself", "Shut down on 2026-09-01; expenses go through the finance app now.", true},
		{"names nothing", "Retired: the vendor portal was shut down on 2026-09-01.", false},
		// A memory that is not the successor: it says nothing of the new place.
		{"names an unrelated memory", "Superseded by facts/build.md.", false},
		// The index mentions the new place only because it describes the
		// memory that states it.
		{"names only the index", "See MEMORY.md for what replaced this.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tombstoneNamesSuccessor(tc.tomb, store); got != tc.want {
				t.Errorf("tombstoneNamesSuccessor(%q) = %v, want %v", tc.tomb, got, tc.want)
			}
		})
	}
}
