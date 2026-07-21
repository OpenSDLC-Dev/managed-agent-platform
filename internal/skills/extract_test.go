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
	a := Sentinel([]Resolved{
		{ID: "pdf", Version: "20260101", Dir: "pdf"},
		{ID: "skill_x", Version: "175917", Dir: "notes"},
	})
	b := Sentinel([]Resolved{
		{ID: "skill_x", Version: "175917", Dir: "notes"},
		{ID: "pdf", Version: "20260101", Dir: "pdf"},
	})
	if !bytes.Equal(a, b) {
		t.Errorf("sentinel is not order-independent: %s vs %s", a, b)
	}
	c := Sentinel([]Resolved{
		{ID: "pdf", Version: "20260102", Dir: "pdf"},
		{ID: "skill_x", Version: "175917", Dir: "notes"},
	})
	if bytes.Equal(a, c) {
		t.Error("sentinel ignores version changes")
	}
	// The directory is deliberately NOT part of the marker: two sets that
	// differ only by landing directory encode identically, because the
	// directory is recomputed from trusted metadata, never read back.
	d := Sentinel([]Resolved{
		{ID: "pdf", Version: "20260101", Dir: "elsewhere"},
		{ID: "skill_x", Version: "175917", Dir: "notes"},
	})
	if !bytes.Equal(a, d) {
		t.Error("sentinel bytes depend on the directory")
	}
	if bytes.Contains(a, []byte("directory")) || bytes.Contains(a, []byte("notes")) {
		t.Errorf("marker records a directory: %s", a)
	}
	if !bytes.Equal(Sentinel(nil), Sentinel([]Resolved{})) {
		t.Error("nil and empty sets differ")
	}
	entries, ok := ParseSentinel(a)
	if !ok || len(entries) != 2 || entries[0].ID != "pdf" || entries[1].Version != "175917" {
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
	data := Sentinel([]Resolved{{ID: "skill_x", Version: "100", Dir: "notes"}})
	rs := []Resolved{{ID: "skill_x", Version: "100", Dir: "notes"}}

	if !SentinelMatches(context.Background(), read, "/ws", data, rs) {
		t.Error("intact tree did not match")
	}
	// The workdir is agent-writable: a deleted skill tree must void the
	// sentinel even though the marker bytes still match.
	delete(files, "/ws/skills/notes/SKILL.md")
	if SentinelMatches(context.Background(), read, "/ws", data, rs) {
		t.Error("matched with the skill tree gone")
	}
	files["/ws/skills/notes/SKILL.md"] = "x"
	if SentinelMatches(context.Background(), read, "/ws", data,
		[]Resolved{{ID: "skill_x", Version: "200", Dir: "notes"}}) {
		t.Error("matched a version change")
	}
	if SentinelMatches(context.Background(), read, "/ws", data,
		[]Resolved{{ID: "skill_x", Version: "100", Dir: "notes"}, {ID: "extra", Version: "1", Dir: "extra"}}) {
		t.Error("matched with an extra resolved skill")
	}
	if SentinelMatches(context.Background(), read, "/ws", []byte("junk"), rs) {
		t.Error("matched an unparsable sentinel")
	}

	// The probe follows the TRUSTED directory in rs, not anything the marker
	// carries: point rs at a directory with no SKILL.md and the skip is voided
	// even though the marker's {id, version} still matches. This is the trust
	// boundary — an agent that rewrote the marker cannot redirect the probe.
	if SentinelMatches(context.Background(), read, "/ws", data,
		[]Resolved{{ID: "skill_x", Version: "100", Dir: "decoy"}}) {
		t.Error("matched against a directory with no SKILL.md")
	}
	files["/ws/skills/decoy/SKILL.md"] = "x"
	if !SentinelMatches(context.Background(), read, "/ws", data,
		[]Resolved{{ID: "skill_x", Version: "100", Dir: "decoy"}}) {
		t.Error("probe did not follow the trusted directory")
	}

	// Bijection: a forged marker cannot mask a missing skill. A duplicated id,
	// a zero-value entry, or an unknown id — each keeps len equal to rs but
	// breaks the exact one-to-one mapping, so the skip is voided.
	two := []Resolved{
		{ID: "skill_x", Version: "100", Dir: "notes"},
		{ID: "skill_y", Version: "100", Dir: "notes"},
	}
	dupMarker := []byte(`[{"skill_id":"skill_x","version":"100"},{"skill_id":"skill_x","version":"100"}]`)
	if SentinelMatches(context.Background(), read, "/ws", dupMarker, two) {
		t.Error("matched a marker with a duplicated id against two distinct skills")
	}
	unknownMarker := []byte(`[{"skill_id":"skill_z","version":"100"}]`)
	if SentinelMatches(context.Background(), read, "/ws", unknownMarker, rs) {
		t.Error("matched a marker naming an unresolved skill")
	}
	emptyMarker := []byte(`[{}]`)
	if SentinelMatches(context.Background(), read, "/ws", emptyMarker, rs) {
		t.Error("matched a zero-value marker entry")
	}
	// A caller that passes a non-deduplicated set gets a safe (no-skip) answer.
	if SentinelMatches(context.Background(), read, "/ws", dupMarker,
		[]Resolved{{ID: "skill_x", Version: "100", Dir: "notes"}, {ID: "skill_x", Version: "100", Dir: "notes"}}) {
		t.Error("matched against a duplicated resolved set")
	}

	// An older marker that still carries a "directory" field parses cleanly —
	// the field is ignored — and matches, because the probe uses the trusted
	// directory regardless. Upgrade is seamless and still sound.
	legacy := []byte(`[{"skill_id":"skill_x","version":"100","directory":"notes"}]`)
	if !SentinelMatches(context.Background(), read, "/ws", legacy, rs) {
		t.Error("a legacy directory-bearing marker did not match")
	}
}

func TestReadArchive(t *testing.T) {
	body := []byte("PK\x03\x04 pretend archive bytes")
	got, err := ReadArchive(bytes.NewReader(body))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("ReadArchive = %q, %v", got, err)
	}
	if got, err := ReadArchive(bytes.NewReader(nil)); err != nil || len(got) != 0 {
		t.Fatalf("ReadArchive(empty) = %q, %v", got, err)
	}

	// A stream larger than the 64 KiB initial buffer but under the cap exercises
	// the clamped-doubling grow loop and must round-trip whole.
	big := []byte(strings.Repeat("z", 200<<10))
	if out, err := readArchiveLimited(bytes.NewReader(big), 1<<20); err != nil || len(out) != len(big) {
		t.Errorf("readArchiveLimited(grow) = %d bytes, %v", len(out), err)
	}

	// The cap is enforced on bytes actually read, tested through the limited
	// core with a small ceiling so the test need not build a gigabyte.
	small := []byte(strings.Repeat("a", 100))
	// Just under the cap: read whole.
	if out, err := readArchiveLimited(bytes.NewReader(small), 200); err != nil || len(out) != 100 {
		t.Errorf("readArchiveLimited(under cap) = %d bytes, %v", len(out), err)
	}
	// Exactly at the cap is allowed and the returned bytes are complete — this
	// is the boundary the buffer-growth bug lived at (a full-to-cap buffer must
	// not force an over-cap regrow before the EOF/overflow check).
	if out, err := readArchiveLimited(bytes.NewReader(small), 100); err != nil || len(out) != 100 {
		t.Errorf("readArchiveLimited(at cap) = %d bytes, %v", len(out), err)
	}
	// One byte over the cap is refused via the overflow probe.
	if _, err := readArchiveLimited(bytes.NewReader(small), 99); err == nil {
		t.Error("byte cap not enforced at one-over")
	}
	if _, err := readArchiveLimited(bytes.NewReader(small), 50); err == nil {
		t.Error("byte cap not enforced")
	}
}
