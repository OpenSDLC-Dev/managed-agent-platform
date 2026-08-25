package toolset_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// TestMemoryRootsGuardTheFileTools pins plan 36 decision 12's file-tool half:
// write and edit refuse a path inside a read-only store and any path under
// /mnt/memory that is inside no mounted store, and allow everything else —
// a read-write store, the workdir, a relative path that resolves into a
// store. read is not guarded, and neither is bash (the sync's own rules
// cover what bash writes).
func TestMemoryRootsGuardTheFileTools(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{
		"/mnt/memory/notes/a.md":   "rw",
		"/mnt/memory/archive/b.md": "ro",
		"/mnt/memory/loose.md":     "loose",
		"/workspace/c.md":          "work",
	}}
	r := toolset.Runner{
		Sandbox: sb, Session: domain.NewID("sesn"), Workdir: "/workspace",
		MemoryRoots:   []string{"/mnt/memory/notes", "/mnt/memory/archive"},
		ReadOnlyRoots: []string{"/mnt/memory/archive"},
	}
	run := func(t *testing.T, tool, input string) toolset.Result {
		t.Helper()
		res, err := r.Run(context.Background(), domain.NewID("toolu"), tool, []byte(input))
		if err != nil {
			t.Fatalf("%s: backend fault %v", tool, err)
		}
		return res
	}
	refused := func(t *testing.T, tool, input, want string) {
		t.Helper()
		res := run(t, tool, input)
		if !res.IsError || !strings.Contains(res.Content, want) {
			t.Errorf("%s %s = %+v, want an error naming %q", tool, input, res, want)
		}
	}
	allowed := func(t *testing.T, tool, input string) {
		t.Helper()
		if res := run(t, tool, input); res.IsError {
			t.Errorf("%s %s refused: %s", tool, input, res.Content)
		}
	}

	// Inside a read-only store: both writers refuse, naming the root.
	refused(t, "write", `{"file_path":"/mnt/memory/archive/b.md","content":"x"}`, "read-only memory store directory /mnt/memory/archive")
	refused(t, "write", `{"file_path":"/mnt/memory/archive/new/deep.md","content":"x"}`, "/mnt/memory/archive")
	refused(t, "edit", `{"file_path":"/mnt/memory/archive/b.md","old_string":"ro","new_string":"rw"}`, "read-only memory store directory")
	// A path that only looks like it is inside: a sibling whose name shares the prefix.
	refused(t, "write", `{"file_path":"/mnt/memory/archive-2/x.md","content":"x"}`, "nothing is mounted at that path")
	// Under /mnt/memory but in no store — the baselines' directory included.
	refused(t, "write", `{"file_path":"/mnt/memory/loose.md","content":"x"}`, "holds only mounted memory stores")
	refused(t, "write", `{"file_path":"/mnt/memory/.sync/memstore_x","content":"{}"}`, "holds only mounted memory stores")
	refused(t, "edit", `{"file_path":"/mnt/memory/loose.md","old_string":"loose","new_string":"x"}`, "holds only mounted memory stores")
	refused(t, "write", `{"file_path":"/mnt/memory","content":"x"}`, "holds only mounted memory stores")
	// A traversal that resolves into the read-only store is judged resolved.
	refused(t, "write", `{"file_path":"/mnt/memory/notes/../archive/b.md","content":"x"}`, "read-only memory store directory")
	refused(t, "write", `{"file_path":"../mnt/memory/loose.md","content":"x"}`, "holds only mounted memory stores")

	// A read-write store, and everything outside the tree.
	allowed(t, "write", `{"file_path":"/mnt/memory/notes/a.md","content":"changed"}`)
	allowed(t, "write", `{"file_path":"/mnt/memory/notes/sub/new.md","content":"new"}`)
	allowed(t, "edit", `{"file_path":"/mnt/memory/notes/a.md","old_string":"changed","new_string":"edited"}`)
	allowed(t, "write", `{"file_path":"c.md","content":"work"}`)
	allowed(t, "write", `{"file_path":"/tmp/x","content":"x"}`)
	// Reading a read-only store is what it is for.
	allowed(t, "read", `{"file_path":"/mnt/memory/archive/b.md"}`)
	if got := sb.files["/mnt/memory/archive/b.md"]; got != "ro" {
		t.Errorf("the read-only file was written: %q", got)
	}
	if got := sb.files["/mnt/memory/notes/a.md"]; got != "edited" {
		t.Errorf("the read-write file = %q", got)
	}

	// No stores at all: the tree is still reserved, and nothing else changes.
	bare := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn"), Workdir: "/workspace"}
	res, err := bare.Run(context.Background(), domain.NewID("toolu"), "write", []byte(`{"file_path":"/mnt/memory/notes/a.md","content":"x"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Content, "nothing is mounted at that path") {
		t.Errorf("a store-less runner writing under /mnt/memory = %+v, %v", res, err)
	}
}
