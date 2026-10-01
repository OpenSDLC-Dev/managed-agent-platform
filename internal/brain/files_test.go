package brain

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// wantUploadsPointer is the reference's own sentence, as the model quoted it in
// the 2026-09-02 recording (probe sessF.events.after-ls-after-delete). Spelled
// out here rather than read from the code, so a wording change fails a test.
const wantUploadsPointer = "User uploads (files uploaded to the session by the user) are available at " +
	"`/mnt/session/uploads`. Use `ls` on that directory to see available files."

func TestResolveFilesBlock(t *testing.T) {
	pool := pgtest.NewPool(t)
	b := &Brain{pool: pool}
	ctx := context.Background()

	if block, n, m := b.resolveFilesBlock(ctx, nil); block != "" || n != 0 || m != 0 {
		t.Errorf("nil resources = %q,%d,%d", block, n, m)
	}
	if block, n, m := b.resolveFilesBlock(ctx, []byte("[]")); block != "" || n != 0 || m != 0 {
		t.Errorf("empty resources = %q,%d,%d", block, n, m)
	}

	seedFileRow(t, b, "file_here", "report.pdf", "application/pdf", 2048)
	seedFileRow(t, b, "file_also", "data.csv", "text/csv", 512)
	resources := mustResourcesJSON(t,
		map[string]string{"type": "file", "file_id": "file_here", "mount_path": "/mnt/session/uploads/file_here"},
		map[string]string{"type": "file", "file_id": "file_also", "mount_path": "/mnt/session/uploads/custom/data.csv"},
		map[string]string{"type": "file", "file_id": "file_gone", "mount_path": "/data/missing"}, // dangling -> miss
		map[string]string{"type": "github_repository"},                                           // non-file -> skip, not a miss
	)
	block, n, misses := b.resolveFilesBlock(ctx, resources)
	if n != 2 {
		t.Errorf("injected = %d, want 2 (dangling + non-file skipped)", n)
	}
	if misses != 1 {
		t.Errorf("misses = %d, want 1 (the dangling mount; the non-file type is a skip, not a miss)", misses)
	}
	// One pointer however many mounts, and nothing about any of them: the
	// reference lists no file and tells the agent to ls (#681).
	if block != wantUploadsPointer {
		t.Errorf("block = %q, want the reference's pointer %q", block, wantUploadsPointer)
	}

	// Malformed resources JSON is a logged skip, not a panic.
	if block, n, m := b.resolveFilesBlock(ctx, []byte("not json")); block != "" || n != 0 || m != 0 {
		t.Errorf("malformed resources = %q,%d,%d", block, n, m)
	}
}

// TestResolveFilesBlockSkipsExpired: an expired mount is the same counted miss a
// deleted one already was. The executor no longer materializes it, so telling
// the model the file is mounted would describe a path with nothing at it (#655).
func TestResolveFilesBlockSkipsExpired(t *testing.T) {
	pool := pgtest.NewPool(t)
	b := &Brain{pool: pool}
	ctx := context.Background()

	seedFileRow(t, b, "file_expired", "gone.pdf", "application/pdf", 2048)
	if _, err := pool.Exec(ctx,
		`UPDATE files SET expires_at = now() - interval '1 second' WHERE id = $1`,
		"file_expired"); err != nil {
		t.Fatalf("expire the seeded file: %v", err)
	}
	resources := mustResourcesJSON(t, map[string]string{
		"type": "file", "file_id": "file_expired",
		"mount_path": "/mnt/session/uploads/file_expired"})

	block, n, misses := b.resolveFilesBlock(ctx, resources)
	if n != 0 {
		t.Errorf("injected = %d, want 0: an expired file has no content to describe", n)
	}
	if misses != 1 {
		t.Errorf("misses = %d, want 1: an expired mount is counted like a dangling one", misses)
	}
	if block != "" {
		t.Errorf("block = %q, want empty", block)
	}
}

func seedFileRow(t *testing.T, b *Brain, id, filename, mime string, size int64) {
	t.Helper()
	if _, err := b.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable) VALUES ($1,$2,$3,$4,false)`,
		id, filename, mime, size); err != nil {
		t.Fatalf("seed file %s: %v", id, err)
	}
}

func mustResourcesJSON(t *testing.T, entries ...map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
