package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// zipOf builds an in-memory zip with the given name→content entries, in order.
func zipOf(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		fw, err := w.CreateHeader(&zip.FileHeader{Name: e[0], Method: zip.Deflate})
		if err != nil {
			t.Fatalf("create %q: %v", e[0], err)
		}
		if _, err := fw.Write([]byte(e[1])); err != nil {
			t.Fatalf("write %q: %v", e[0], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestExtract(t *testing.T) {
	// The canonical form: one wrapper directory, stripped on extraction.
	data := zipOf(t,
		[2]string{"my-skill/", ""},
		[2]string{"my-skill/SKILL.md", "hello"},
		[2]string{"my-skill/scripts/run.sh", "echo hi"},
	)
	files, err := Extract(data)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = string(f.Data)
	}
	want := map[string]string{"SKILL.md": "hello", "scripts/run.sh": "echo hi"}
	if len(got) != len(want) || got["SKILL.md"] != "hello" || got["scripts/run.sh"] != "echo hi" {
		t.Errorf("extracted = %v, want %v", got, want)
	}

	// A flat archive (no shared wrapper with nesting) extracts unchanged,
	// mirroring the reference's archiveTopDir semantics.
	flat := zipOf(t, [2]string{"SKILL.md", "flat"}, [2]string{"other.md", "x"})
	files, err = Extract(flat)
	if err != nil {
		t.Fatalf("flat extract: %v", err)
	}
	if len(files) != 2 || files[0].Path != "SKILL.md" && files[1].Path != "SKILL.md" {
		t.Errorf("flat extracted = %v", files)
	}

	// Two top-level dirs: no stripping, paths keep their roots.
	multi := zipOf(t, [2]string{"a/x.md", "1"}, [2]string{"b/y.md", "2"})
	files, err = Extract(multi)
	if err != nil {
		t.Fatalf("multi extract: %v", err)
	}
	if len(files) != 2 || !strings.Contains(files[0].Path, "/") {
		t.Errorf("multi extracted = %v", files)
	}
}

func TestExtractRefusals(t *testing.T) {
	cases := map[string][]byte{
		"not a zip":     []byte("plain text"),
		"empty zip":     zipOf(t),
		"parent escape": zipOf(t, [2]string{"skill/../../etc/passwd", "x"}),
		"absolute path": zipOf(t, [2]string{"/etc/passwd", "x"}),
		"backslash":     zipOf(t, [2]string{`skill\evil.md`, "x"}),
		"NUL in name":   zipOf(t, [2]string{"skill/bad\x00name", "x"}),
	}
	for name, data := range cases {
		if _, err := Extract(data); err == nil {
			t.Errorf("%s: extract accepted, want error", name)
		}
	}
}

func TestExtractCaps(t *testing.T) {
	// The limits are enforced by extractWithLimits; Extract passes the
	// reference constants (10k members / 1 GiB), too large to build in a test.
	small := zipOf(t,
		[2]string{"s/SKILL.md", "0123456789"},
		[2]string{"s/big.txt", strings.Repeat("a", 100)},
	)
	if _, err := extractWithLimits(small, 1, 1<<30); err == nil {
		t.Error("member cap not enforced")
	}
	if _, err := extractWithLimits(small, 100, 50); err == nil {
		t.Error("decompressed byte cap not enforced")
	}
	if _, err := extractWithLimits(small, 100, 1<<30); err != nil {
		t.Errorf("within caps: %v", err)
	}
}

func TestTargetDir(t *testing.T) {
	for _, tc := range []struct{ name, id, want string }{
		{"financial-skill", "skill_abc", "financial-skill"},
		{"  spaced  ", "skill_abc", "spaced"},
		{"", "xlsx", "xlsx"},
		{".", "xlsx", "xlsx"},
		{"..", "xlsx", "xlsx"},
		{"a/b", "xlsx", "xlsx"},
		{`a\b`, "xlsx", "xlsx"},
	} {
		if got := TargetDir(tc.name, tc.id); got != tc.want {
			t.Errorf("TargetDir(%q, %q) = %q, want %q", tc.name, tc.id, got, tc.want)
		}
	}
}

func TestSentinel(t *testing.T) {
	a := Sentinel([]SentinelEntry{
		{ID: "pdf", Version: "20260101", Dir: "pdf"},
		{ID: "skill_x", Version: "175917", Dir: "notes"},
	})
	b := Sentinel([]SentinelEntry{
		{ID: "skill_x", Version: "175917", Dir: "notes"},
		{ID: "pdf", Version: "20260101", Dir: "pdf"},
	})
	if !bytes.Equal(a, b) {
		t.Errorf("sentinel is not order-independent: %s vs %s", a, b)
	}
	c := Sentinel([]SentinelEntry{
		{ID: "pdf", Version: "20260102", Dir: "pdf"},
		{ID: "skill_x", Version: "175917", Dir: "notes"},
	})
	if bytes.Equal(a, c) {
		t.Error("sentinel ignores version changes")
	}
	if !bytes.Equal(Sentinel(nil), Sentinel([]SentinelEntry{})) {
		t.Error("nil and empty sets differ")
	}
	entries, ok := ParseSentinel(a)
	if !ok || len(entries) != 2 || entries[0].ID != "pdf" || entries[1].Dir != "notes" {
		t.Errorf("ParseSentinel = %v %v", entries, ok)
	}
	if _, ok := ParseSentinel([]byte("not json")); ok {
		t.Error("ParseSentinel accepted garbage")
	}
}

func TestSentinelMatches(t *testing.T) {
	files := map[string]string{"/ws/skills/notes/SKILL.md": "x"}
	read := func(_ context.Context, p string) ([]byte, error) {
		if data, ok := files[p]; ok {
			return []byte(data), nil
		}
		return nil, errors.New("no such file")
	}
	data := Sentinel([]SentinelEntry{{ID: "skill_x", Version: "100", Dir: "notes"}})
	resolved := map[string]string{"skill_x": "100"}

	if !SentinelMatches(context.Background(), read, "/ws", data, resolved) {
		t.Error("intact tree did not match")
	}
	// The workdir is agent-writable: a deleted skill tree must void the
	// sentinel even though the marker bytes still match.
	delete(files, "/ws/skills/notes/SKILL.md")
	if SentinelMatches(context.Background(), read, "/ws", data, resolved) {
		t.Error("matched with the skill tree gone")
	}
	files["/ws/skills/notes/SKILL.md"] = "x"
	if SentinelMatches(context.Background(), read, "/ws", data,
		map[string]string{"skill_x": "200"}) {
		t.Error("matched a version change")
	}
	if SentinelMatches(context.Background(), read, "/ws", data,
		map[string]string{"skill_x": "100", "extra": "1"}) {
		t.Error("matched with an extra resolved skill")
	}
	if SentinelMatches(context.Background(), read, "/ws", []byte("junk"), resolved) {
		t.Error("matched an unparsable sentinel")
	}
	// A pre-upgrade marker (no "directory" field) parses with an empty Dir,
	// so its probe path collapses to a nonexistent {workdir}/skills/SKILL.md
	// and re-materialization is forced — upgrade safety without a rejection.
	oldFormat := []byte(`[{"skill_id":"skill_x","version":"100"}]`)
	if entries, ok := ParseSentinel(oldFormat); !ok || len(entries) != 1 || entries[0].Dir != "" {
		t.Errorf("pre-upgrade marker did not parse to an empty Dir: %v %v", entries, ok)
	}
	if SentinelMatches(context.Background(), read, "/ws", oldFormat, resolved) {
		t.Error("matched a pre-upgrade directory-less marker")
	}
}
