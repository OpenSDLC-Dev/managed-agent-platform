package evals

import "testing"

// The in-place dream's tombstone grader, on the tombstones nightly runs wrote
// and on the ones it must still refuse (#951).
func TestTombstoneNamesSuccessor(t *testing.T) {
	const retired = "/facts/vendor-portal.md"
	store := map[string]string{
		retired:                    "Superseded by facts/expenses.md.\n",
		"/MEMORY.md":               "# Memory\n\n- facts/expenses.md: filed at finance.internal\n- Remove: facts/vendor-portal.md\n",
		"/facts/build.md":          "# Build\n\nValidated fact: the build runs `make verify`, which takes about nine minutes.\n",
		"/facts/expenses.md":       "# Expenses\n\nExpenses are filed in the internal finance app at finance.internal.\n",
		"/facts/expense-filing.md": "# Expense filing\n\nFiled at finance.internal since 2026-09-01.\n",
		"/facts/Expenses-2026.md":  "# Expenses\n\nFiled at finance.internal.\n",
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
		// A relative link reads from the retired memory's own directory.
		{"names it beside the retired memory", "Superseded by expenses.md.", true},
		{"links to it", "Superseded by [expenses](./expenses.md).", true},
		{"names a path with capitals", "Superseded by facts/Expenses-2026.md.", true},
		{"names the place itself", "Shut down on 2026-09-01; expenses go through the finance app now.", true},
		{"names nothing", "Retired: the vendor portal was shut down on 2026-09-01.", false},
		// A memory that is not the successor: it says nothing of the new place.
		{"names an unrelated memory", "Superseded by facts/build.md.", false},
		// The index mentions the new place only because it describes the
		// memory that states it.
		{"names only the index", "See MEMORY.md for what replaced this.", false},
		// Each of these names a memory that does not exist, which a caller
		// cannot follow.
		{"names a longer path", "Superseded by facts/expenses.md.bak.", false},
		{"names it under another directory", "Superseded by other/facts/expenses.md.", false},
		{"names it in another case", "Superseded by facts/EXPENSES.md.", false},
		{"names a root path that is not there", "Superseded by /expenses.md.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tombstoneNamesSuccessor(tc.tomb, retired, store); got != tc.want {
				t.Errorf("tombstoneNamesSuccessor(%q) = %v, want %v", tc.tomb, got, tc.want)
			}
		})
	}
}
