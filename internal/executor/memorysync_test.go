package executor

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
)

// TestMemoryPushCompareAndSet pins the settle's compare-and-set on its own,
// without the store lock that serializes writers in production: an update
// whose baseline the store no longer holds lands nothing — no head change,
// no version row — and a create over a path taken since is a conflict. The
// `WHERE content_sha256 = $baseline` is what this shows red without.
func TestMemoryPushCompareAndSet(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	ctx := context.Background()
	h.seedMemoryStore(t, memStoreID, "Notes")
	memoryID := h.seedMemory(t, memStoreID, "/notes.md", "hello")
	// The store moved on after the baseline was taken.
	if _, err := h.pool.Exec(ctx,
		`UPDATE memories SET content = 'moved', content_sha256 = $2 WHERE id = $1`, memoryID, sha256hex([]byte("moved"))); err != nil {
		t.Fatal(err)
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	actor := json.RawMessage(`{"type":"session_actor","session_id":"` + h.sid.String() + `"}`)
	held := 1
	outcome, err := h.exec.pushMemory(ctx, tx, memStoreID, memsync.Action{
		Kind: memsync.Push, Path: "/notes.md", ID: memoryID,
		BaselineSHA: sha256hex([]byte("hello")), LocalSHA: sha256hex([]byte("mine")),
	}, []byte("mine"), actor, &held)
	if err != nil || outcome != memoryPushConflict {
		t.Fatalf("stale update = %s, %v; want a conflict", outcome, err)
	}
	outcome, err = h.exec.pushMemory(ctx, tx, memStoreID, memsync.Action{
		Kind: memsync.Push, Path: "/notes.md", LocalSHA: sha256hex([]byte("mine")),
	}, []byte("mine"), actor, &held)
	if err != nil || outcome != memoryPushConflict {
		t.Fatalf("create over a taken path = %s, %v; want a conflict", outcome, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.memoryContent(t, memStoreID, "/notes.md"); got != "moved" {
		t.Errorf("the store's /notes.md = %q; a stale push landed", got)
	}
	if got := h.versionsOf(t, memStoreID, "/notes.md"); !slices.Equal(got, []string{"created/none"}) {
		t.Errorf("versions = %v; a landed-nothing push wrote one", got)
	}
}

// TestMemorySyncRetriesAFailedApply: a pull whose apply could not be written
// keeps the old baseline, and the next sync lands it — a push already
// committed then reads as agreement, a pull as still pending.
func TestMemorySyncRetriesAFailedApply(t *testing.T) {
	h, sb := materialized(t, "read_write")
	before := sb.files[baselinePath(memStoreID)]
	h.seedMemory(t, memStoreID, "/c.md", "new remote")
	// The baseline is the batch's last member; failing it fails the batch
	// after the pull landed — the shape of a write that died mid-apply.
	sb.failPath = memStoreID
	h.step(t)
	if sb.files[baselinePath(memStoreID)] != before {
		t.Fatal("the baseline was rewritten by a failed apply")
	}
	sb.failPath = ""
	h.step(t)
	if got := sb.files[memMount+"/c.md"]; got != "new remote" {
		t.Errorf("/c.md = %q after the retry", got)
	}
	if b := baselineOf(t, sb, memStoreID); b.Synced["/c.md"] != sha256hex([]byte("new remote")) {
		t.Errorf("baseline after the retry = %+v", b)
	}
}

// TestMemoryArchivedStoreRefusesWritesAtTheNextRun: a store archived between
// two runs is read-only to the file tools at the next run — the refusal names
// the directory — and its sync pulls only. The attachment still says
// read_write; the store's state is what the run reads.
func TestMemoryArchivedStoreRefusesWritesAtTheNextRun(t *testing.T) {
	h, sb := materialized(t, "read_write")
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE memory_stores SET archived_at = now() WHERE id = $1`, memStoreID); err != nil {
		t.Fatal(err)
	}
	h.suspend(t, writeUse(memMount+"/notes.md", "after the archive"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	results := h.toolResults(t)
	last := results[len(results)-1]
	if !last.IsError || !strings.Contains(resultText(last), "read-only memory store directory "+memMount) {
		t.Errorf("the write to an archived store answered %+v; want a read-only refusal", last)
	}
	if got := sb.files[memMount+"/notes.md"]; got != "hello" {
		t.Errorf("/notes.md = %q; the refused write landed", got)
	}
}

func resultText(r resultBody) string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}
