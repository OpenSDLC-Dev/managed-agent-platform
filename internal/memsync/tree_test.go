package memsync_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
)

func TestMarkerBytes(t *testing.T) {
	got := string(memsync.MarkerBytes("memstore_01ARZ3NDEKTSV4RRFFQ69G5FAV"))
	if got != "version 1\nmemstore_01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Fatalf("marker = %q", got)
	}
	if memsync.MarkerName != ".anthropic-memory-store" {
		t.Fatalf("marker name = %q", memsync.MarkerName)
	}
}

func TestBaselineRoundTrip(t *testing.T) {
	b := memsync.Baseline{
		Synced:  map[string]string{"/notes.md": strings.Repeat("a", 64), "/a/b": strings.Repeat("b", 64)},
		Refused: map[string]string{"/big.bin": strings.Repeat("c", 64)},
	}
	enc := b.Encode()
	// Keys land sorted, so two baselines with the same content are the same
	// bytes — a baseline file is rewritten every run and must not churn.
	if want := `{"synced":{"/a/b":"` + strings.Repeat("b", 64) + `","/notes.md":"` + strings.Repeat("a", 64) +
		`"},"refused":{"/big.bin":"` + strings.Repeat("c", 64) + `"}}`; string(enc) != want {
		t.Fatalf("encoded = %s\nwant      %s", enc, want)
	}
	got, err := memsync.DecodeBaseline(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Synced) != 2 || got.Synced["/a/b"] != strings.Repeat("b", 64) || got.Refused["/big.bin"] != strings.Repeat("c", 64) {
		t.Fatalf("decoded = %+v", got)
	}

	// An absent or empty file is an empty baseline with usable maps, not an
	// error and not nil maps a caller would panic writing into.
	for _, raw := range [][]byte{nil, {}, []byte("  \n"), []byte("{}")} {
		got, err := memsync.DecodeBaseline(raw)
		if err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if got.Synced == nil || got.Refused == nil || len(got.Synced)+len(got.Refused) != 0 {
			t.Fatalf("decode %q = %+v", raw, got)
		}
		got.Synced["/x"] = "y" // must not panic
	}
	if _, err := memsync.DecodeBaseline([]byte("{not json")); err == nil {
		t.Fatal("malformed baseline decoded")
	}
	// An empty baseline encodes to something DecodeBaseline reads back.
	if _, err := memsync.DecodeBaseline(memsync.Baseline{}.Encode()); err != nil {
		t.Fatal(err)
	}
}

func TestHashTreeCommand(t *testing.T) {
	got := memsync.HashTreeCommand("/mnt/memory/notes")
	want := `[ -d '/mnt/memory/notes' ] || exit 0; cd -P '/mnt/memory/notes' && [ "$PWD" = '/mnt/memory/notes' ] || exit 3; set -o pipefail; find . -type f ! -path './.anthropic-memory-store' -print0 | LC_ALL=C sort -z | xargs -0r sha256sum -z`
	if got != want {
		t.Fatalf("command =\n%s\nwant\n%s", got, want)
	}
	// A mount with a quote in it is still one shell word.
	if got := memsync.HashTreeCommand("/mnt/it's"); !strings.HasPrefix(got, `[ -d '/mnt/it'\''s' ] || exit 0; cd -P '/mnt/it'\''s' && [ "$PWD" = '/mnt/it'\''s' ] || exit 3; `) {
		t.Fatalf("quoting: %s", got)
	}
}

// RemoveCommands: the unlinks, then the parents pruned as far as empty, one
// command while it fits; a mount's worth of the longest paths is several
// commands, each well under the shell's single-argument cap, together
// naming every path once.
// TestRemoveCommandsBudgetIsTheQuotedCommand: a path's apostrophes are four
// bytes each once quoted, and the budget bounds the command as emitted.
func TestRemoveCommandsBudgetIsTheQuotedCommand(t *testing.T) {
	var paths []string
	for i := 0; i < 400; i++ {
		paths = append(paths, fmt.Sprintf("/q/%d-%s.md", i, strings.Repeat("'", 250)))
	}
	cmds := memsync.RemoveCommands("/mnt/memory/notes", paths)
	if len(cmds) < 2 {
		t.Fatalf("%d commands for %d paths of 1,000 quoted bytes", len(cmds), len(paths))
	}
	for _, cmd := range cmds {
		if len(cmd) > 48<<10 {
			t.Errorf("a command of %d bytes; the budget measured the paths unquoted", len(cmd))
		}
	}
}

func TestRemoveCommands(t *testing.T) {
	got := memsync.RemoveCommands("/mnt/memory/notes", []string{"/a/b.md", "/c.md", "/a/d/e.md"})
	const prelude = `[ -d '/mnt/memory/notes' ] || exit 0; cd -P '/mnt/memory/notes' && [ "$PWD" = '/mnt/memory/notes' ] || exit 1; `
	want := []string{prelude + `rm -f -- 'a/b.md' 'c.md' 'a/d/e.md' || exit 1; rmdir -p --ignore-fail-on-non-empty -- 'a' 'a/d' 2>/dev/null; exit 0`}
	if !slices.Equal(got, want) {
		t.Fatalf("commands =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := memsync.RemoveCommands("/mnt/memory/notes", []string{"/c.md"}); !slices.Equal(got, []string{prelude + `rm -f -- 'c.md' || exit 1`}) {
		t.Fatalf("root-level removal = %q", got)
	}
	if got := memsync.RemoveCommands("/mnt/memory/notes", nil); len(got) != 0 {
		t.Fatalf("nothing to remove = %q", got)
	}

	paths := make([]string, memsync.MaxMemoriesPerStore)
	for i := range paths {
		paths[i] = "/" + fmt.Sprintf("%04d", i) + strings.Repeat("x", memsync.MaxPathBytes-5)
	}
	cmds := memsync.RemoveCommands("/mnt/memory/notes", paths)
	if len(cmds) < 2 {
		t.Fatalf("%d paths of %d bytes fit one command", len(paths), memsync.MaxPathBytes)
	}
	named := 0
	for _, cmd := range cmds {
		if len(cmd) > 96<<10 {
			t.Errorf("a command of %d bytes is past the single-argument cap", len(cmd))
		}
		named += strings.Count(cmd, "x'")
	}
	if named != len(paths) {
		t.Errorf("%d paths named across %d commands, want %d", named, len(cmds), len(paths))
	}
}

func TestParseHashTree(t *testing.T) {
	sha := func(c byte) string { return strings.Repeat(string(c), 64) }
	// `sha256sum -z` output: digest, two spaces (or " *" in binary mode), the
	// name find printed, NUL. No escaping, so a newline in a name is literal.
	out := sha('a') + "  ./notes.md\x00" +
		sha('b') + " *./a/b.md\x00" +
		sha('c') + "  ./odd\nname\x00" +
		sha('d') + "  ./.hidden\x00"
	got, err := memsync.ParseHashTree([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/notes.md": sha('a'), "/a/b.md": sha('b'), "/odd\nname": sha('c'), "/.hidden": sha('d'),
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %v", len(got), len(want), got)
	}
	for p, s := range want {
		if got[p] != s {
			t.Errorf("%q = %q, want %q", p, got[p], s)
		}
	}

	// No files is no entries — `xargs -r` runs nothing on empty input.
	if got, err := memsync.ParseHashTree(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v", got, err)
	}
	// A record without its NUL is refused rather than trusted: the last one
	// is what a truncated capture would cut.
	for _, bad := range []string{
		"nonsense\x00",
		sha('a') + "  notes.md\x00",                     // not find's ./ prefix
		sha('a') + " ./notes.md\x00",                    // one separator byte
		strings.Repeat("g", 64) + "  ./x\x00",           // not hex
		sha('a') + "  ./x\x00" + sha('b') + "  ./x\x00", // repeated
		sha('a') + "  ./\x00",                           // no name
		sha('a') + "  ./x",                              // unterminated
	} {
		if _, err := memsync.ParseHashTree([]byte(bad)); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}
