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

// The in-place dream's index grader, on indexes nightly and local runs wrote
// and on the shapes the reviews of #1002 found. A listing is an entry under
// the removal heading, or after a "remove:" label; the tombstone's own line,
// and an empty section that explains itself by naming the path, are not.
func TestListsForRemoval(t *testing.T) {
	const retired = "/facts/vendor-portal.md"
	const entries = "# Memory index\n\n" +
		"- `facts/vendor-portal.md` — tombstone: superseded by facts/expenses.md.\n" +
		"- `facts/expenses.md` — expenses are filed at finance.internal.\n\n"
	for _, tc := range []struct {
		name, index string
		want        bool
	}{
		{"lists it in a trailing section", entries + "## To remove\n\n- `facts/vendor-portal.md`\n", true},
		{"lists it under an emphasized heading, with a note after it", entries +
			"*To remove*\n\n- `facts/vendor-portal.md`\n\nThe caller removes these through the memories API.\n", true},
		{"lists it under a removal heading", entries + "## Pending removal\n\n- `facts/vendor-portal.md`\n", true},
		{"lists it numbered, under a colon label", entries +
			"**To remove:**\n\n1. `facts/vendor-portal.md` — superseded by facts/expenses.md\n", true},
		{"lists it from the store root", entries + "## To remove\n\n- `/facts/vendor-portal.md`\n", true},
		{"lists it as a link", entries + "## To remove\n\n- [facts/vendor-portal.md](facts/vendor-portal.md)\n", true},
		{"lists it after a label on one line", entries + "To remove: `facts/vendor-portal.md`\n", true},
		{"lists it after an inline label", "# Memory\n\n" +
			"- facts/expenses.md: filed at finance.internal\n- Remove: facts/vendor-portal.md\n", true},
		// The 2026-10-30 nightly's index: the memory was updated, not retired.
		{"has no removal section", "# Memory index\n\n" +
			"- `facts/build.md` — the build command `make verify` and how long it takes.\n" +
			"- `facts/vendor-portal.md` — the 2026-09-01 shutdown of vendor.example.com; " +
			"expenses now filed at finance.internal.\n", false},
		// Two local runs on main: the section says nothing was retired, and
		// names the memory to say so.
		{"explains an empty section by naming it", "# Memory index\n\n" +
			"- `facts/vendor-portal.md` — expense filing: vendor.example.com shut down 2026-09-01.\n" +
			"- `facts/build.md` — the build runs `make verify`.\n\n## to remove\n\n" +
			"Nothing to remove. No memory was retired or superseded; the vendor portal's end was " +
			"recorded as a fact inside `facts/vendor-portal.md` rather than as a tombstone.\n", false},
		{"lists none, after a line that says not to remove", "# Memory index\n\n" +
			"- `facts/build.md` — build runs `make verify`.\n" +
			"- `facts/vendor-portal.md` — vendor portal shut down 2026-09-01.\n" +
			"- `.anthropic-memory-store` — store metadata file (not a memory; do not remove).\n\n" +
			"## to remove\n\n- None — no tombstone files were created. The one retired statement was " +
			"retired inside `facts/vendor-portal.md`, whose subject survives in that file.\n", false},
		{"says nothing to remove", entries + "## To remove\n\nNothing to remove.\n", false},
		{"says to keep it", entries + "## To remove\n\nNothing to remove; keep `facts/vendor-portal.md` as the record.\n", false},
		// "remove" above the entries opens nothing.
		{"mentions removal above the entries", "# Memory index\n\n" +
			"Tombstones are listed again under *To remove* at the end. No files were removed.\n\n" +
			"- `facts/expenses.md` — vendor portal removed 2026-09-01; expenses go to finance.internal.\n" +
			"- `facts/vendor-portal.md` — vendor portal shut down 2026-09-01.\n\n" +
			"## To remove\n\nNothing to remove.\n", false},
		{"lists another memory", entries + "## To remove\n\n- `facts/old.md`\n", false},
		{"lists a longer path", entries + "## To remove\n\n- `facts/vendor-portal.md.bak`\n", false},
		{"lists it under another directory", entries + "## To remove\n\n- `archive/facts/vendor-portal.md`\n", false},
		{"lists it in another case", entries + "## To remove\n\n- `facts/Vendor-Portal.md`\n", false},
		// An index line resolves from the store root, so a base name is not
		// the memory.
		{"lists its base name", entries + "## To remove\n\n- `vendor-portal.md`\n", false},
		// The last removal section decides.
		{"empties the section it listed it in", entries +
			"## To remove\n\n- `facts/vendor-portal.md`\n\n## To remove (revised)\n\nNothing.\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := listsForRemoval(tc.index, retired); got != tc.want {
				t.Errorf("listsForRemoval(%q) = %v, want %v", tc.index, got, tc.want)
			}
		})
	}
}
