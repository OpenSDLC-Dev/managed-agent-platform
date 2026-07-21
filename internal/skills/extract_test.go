package skills

import (
	"archive/zip"
	"bytes"
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
	a := Sentinel(map[string]string{"pdf": "20260101", "skill_x": "175917"})
	b := Sentinel(map[string]string{"skill_x": "175917", "pdf": "20260101"})
	if !bytes.Equal(a, b) {
		t.Errorf("sentinel is not order-independent: %s vs %s", a, b)
	}
	c := Sentinel(map[string]string{"pdf": "20260102", "skill_x": "175917"})
	if bytes.Equal(a, c) {
		t.Error("sentinel ignores version changes")
	}
	if !bytes.Equal(Sentinel(nil), Sentinel(map[string]string{})) {
		t.Error("nil and empty sets differ")
	}
}
