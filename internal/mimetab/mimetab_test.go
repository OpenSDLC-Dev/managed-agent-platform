package mimetab

import (
	"mime"
	"strings"
	"testing"
)

func TestByPathIsHostIndependent(t *testing.T) {
	// A registry mime must not depend on which host derived it (#264, #277):
	// ByPath consults only the pinned table, never the process mime registry
	// that merges the host's /etc/mime.types. Seeding the process registry
	// here proves a leak would be caught: the seeded extension must still
	// fall back. (The seeding is permanent for this test binary — the mime
	// package has no removal API. Harmless while nothing else in this package
	// consults mime.TypeByExtension; a future test that does will see
	// .hostonly.)
	if err := mime.AddExtensionType(".hostonly", "application/x-host-only"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"report.json":    "application/json",
		"build_dcf.py":   "text/x-python; charset=utf-8",
		"notes.md":       "text/markdown; charset=utf-8",
		"README.TXT":     "text/plain; charset=utf-8", // extension case-folded
		"data.csv":       "text/csv; charset=utf-8",
		"index.html":     "text/html; charset=utf-8",
		"chart.png":      "image/png",
		"paper.pdf":      "application/pdf",
		"archive.tar":    "application/x-tar",
		"bundle.zip":     "application/zip",
		"dump.gz":        "application/gzip",
		"config.yaml":    "text/yaml; charset=utf-8",
		"sheet.xlsx":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"memo.docx":      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"invite.ics":     "text/calendar; charset=utf-8",
		"captions.vtt":   "text/vtt; charset=utf-8",
		"data.tsv":       "text/tab-separated-values; charset=utf-8",
		"schema.sql":     "text/x-sql; charset=utf-8",
		"out/run.log":    "text/plain; charset=utf-8", // sandbox-relative path
		"model":          "application/octet-stream",  // no extension
		"weird.hostonly": "application/octet-stream",  // host-seeded mapping ignored
		"unknown.xyz123": "application/octet-stream",
	}
	for p, want := range cases {
		if got := ByPath(p); got != want {
			t.Errorf("ByPath(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestGraderVisibleEntriesStayInlineable(t *testing.T) {
	// The table's textual additions exist to feed the grader, whose inline
	// rule (inlineableMime, internal/brain/grader.go) accepts text/* and
	// application/json — and nothing ties the two packages at compile time.
	// Assert every grader-intended entry stays on the accepted side, so a
	// value edit (say, .yaml back to application/yaml) fails here instead of
	// silently un-inlining that deliverable class.
	graderVisible := []string{
		".csv", ".ini", ".json", ".jsonl", ".log", ".md", ".py", ".rst",
		".sql", ".tex", ".toml", ".tsv", ".txt", ".yaml", ".yml",
	}
	for _, ext := range graderVisible {
		m, ok := byExt[ext]
		if !ok {
			t.Errorf("byExt lacks grader-intended extension %q", ext)
			continue
		}
		if !strings.HasPrefix(m, "text/") && m != "application/json" {
			t.Errorf("byExt[%q] = %q — rejected by the grader inline rule (text/*, application/json)", ext, m)
		}
	}
}
