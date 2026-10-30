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
		"/facts/dépenses.md":       "# Dépenses\n\nDéclarées sur finance.internal.\n",
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
		{"links to it from the parent directory", "Superseded by [expenses](../facts/expenses.md).", true},
		{"names a path with capitals", "Superseded by facts/Expenses-2026.md.", true},
		{"names a path outside ASCII", "Superseded by `facts/dépenses.md`.", true},
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
		{"names it under a non-ASCII directory", "Superseded by 旧/facts/expenses.md.", false},
		// From /facts, two levels up is outside the store, where no memory is.
		{"climbs out of the store", "Superseded by ../../facts/expenses.md.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tombstoneNamesSuccessor(tc.tomb, retired, store); got != tc.want {
				t.Errorf("tombstoneNamesSuccessor(%q) = %v, want %v", tc.tomb, got, tc.want)
			}
		})
	}
}

// The in-place dream's index grader. The retired memory must be named after the
// word that opens the removal section: a tombstone keeps its own line in the
// index, so the path alone is there whether anything was retired or not (#1002).
func TestListsForRemoval(t *testing.T) {
	const retired = "/facts/vendor-portal.md"
	for _, tc := range []struct {
		name, index string
		want        bool
	}{
		{"lists it in a trailing section", "# Memory index\n\n" +
			"- `facts/vendor-portal.md` — tombstone: superseded by facts/expenses.md.\n" +
			"- `facts/expenses.md` — expenses are filed at finance.internal.\n\n" +
			"## To remove\n\n- `facts/vendor-portal.md`\n", true},
		{"lists it under an inline label", "# Memory\n\n" +
			"- facts/expenses.md: filed at finance.internal\n- Remove: facts/vendor-portal.md\n", true},
		{"lists it with a note after it", "# Memory index\n\n" +
			"- `facts/vendor-portal.md` — tombstone.\n\n*To remove*\n\n- `facts/vendor-portal.md`\n\n" +
			"The caller removes these through the memories API.\n", true},
		// The 2026-10-30 nightly's index: the memory was updated, not retired.
		{"has no removal section", "# Memory index\n\n" +
			"- `facts/build.md` — the build command `make verify` and how long it takes.\n" +
			"- `facts/vendor-portal.md` — the 2026-09-01 shutdown of vendor.example.com; " +
			"expenses now filed at finance.internal.\n", false},
		{"lists nothing under the section", "# Memory index\n\n" +
			"- `facts/vendor-portal.md` — tombstone.\n\n## To remove\n\nNothing to remove.\n", false},
		{"lists another memory", "# Memory index\n\n" +
			"- `facts/vendor-portal.md` — tombstone.\n\n## To remove\n\n- `facts/old.md`\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := listsForRemoval(tc.index, retired); got != tc.want {
				t.Errorf("listsForRemoval(%q) = %v, want %v", tc.index, got, tc.want)
			}
		})
	}
}
