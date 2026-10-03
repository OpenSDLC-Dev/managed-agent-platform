package toolset

import (
	"slices"
	"testing"
)

// The glob that leaves the memory sync's baselines out is spelled from where
// rg runs (memoryGlobs): relative to the working directory where that leads
// MemorySyncDir, absolute — a doubled leading slash — where it does not, and
// absent where the root is not above the baselines. The marker is left out by
// name everywhere. TestGrepLeavesOutTheMemorySyncState holds rg itself to the
// spellings.
func TestMemoryGlobs(t *testing.T) {
	const marker = "--glob=!.anthropic-memory-store"
	for _, tc := range []struct {
		root, cwd string
		want      []string
	}{
		{"/workspace", "/workspace", []string{marker}},
		{"/mnt/memory/s1", "/workspace", []string{marker}},
		{"/mnt/memory/.sync", "/workspace", []string{marker}},
		{"/mnt/memory/.sync/memstore_1", "/workspace", []string{marker}},
		{"/mnt/memory/.synced", "/workspace", []string{marker}},
		{"/mnt/mem", "/workspace", []string{marker}},
		{"/mnt", "/workspace", []string{marker, "--glob=!//mnt/memory/.sync"}},
		{"/mnt/memory", "/workspace", []string{marker, "--glob=!//mnt/memory/.sync"}},
		{"/", "/workspace", []string{marker, "--glob=!//mnt/memory/.sync"}},
		{"/mnt", "/", []string{marker, "--glob=!/mnt/memory/.sync"}},
		{"/", "/", []string{marker, "--glob=!/mnt/memory/.sync"}},
		{"/mnt/memory", "/mnt", []string{marker, "--glob=!/memory/.sync"}},
		{"/mnt", "/mnt/memory", []string{marker, "--glob=!/.sync"}},
		// rg strips the working directory's bytes, not its path elements.
		{"/mnt", "/mnt/mem", []string{marker, "--glob=!/ory/.sync"}},
	} {
		if got := memoryGlobs(tc.root, tc.cwd); !slices.Equal(got, tc.want) {
			t.Errorf("memoryGlobs(%q, %q) = %q, want %q", tc.root, tc.cwd, got, tc.want)
		}
	}
}

// A refusal quotes the number the model sent as it most likely wrote it.
func TestNumberSpellsAWholeNumberAsDigits(t *testing.T) {
	for v, want := range map[float64]string{2147483648: "2147483648", 1e12: "1000000000000", -1: "-1", 0.5: "0.5",
		-0.25: "-0.25", 1e21: "1e+21", 1.5e300: "1.5e+300"} {
		if got := number(v); got != want {
			t.Errorf("number(%v) = %q, want %q", v, got, want)
		}
	}
}
