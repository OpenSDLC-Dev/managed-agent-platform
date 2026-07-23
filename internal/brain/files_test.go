package brain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestRenderFilesBlock(t *testing.T) {
	if got := renderFilesBlock(nil); got != "" {
		t.Errorf("empty set = %q, want empty", got)
	}
	one := renderFilesBlock([]fileMeta{{Path: "/mnt/session/uploads/file_x", Filename: "a.txt", MimeType: "text/plain", Size: 12}})
	if !strings.HasPrefix(one, "Mounted files.") {
		t.Errorf("block missing lead line: %q", one)
	}
	if !strings.Contains(one, "- /mnt/session/uploads/file_x (a.txt, text/plain, 12 bytes)") {
		t.Errorf("one-mount block missing the bullet: %q", one)
	}
	// A mount with no MIME type drops the mime clause but keeps filename + size.
	noMime := renderFilesBlock([]fileMeta{{Path: "/p", Filename: "b.bin", MimeType: "", Size: 3}})
	if !strings.Contains(noMime, "- /p (b.bin, 3 bytes)") {
		t.Errorf("no-mime block wrong: %q", noMime)
	}
	multi := renderFilesBlock([]fileMeta{{Path: "/a", Filename: "a"}, {Path: "/b", Filename: "b"}})
	if strings.Count(multi, "\n- ") != 2 {
		t.Errorf("multi block should have two bullets: %q", multi)
	}
}

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
	resources := mustResourcesJSON(t,
		map[string]string{"type": "file", "file_id": "file_here", "mount_path": "/mnt/session/uploads/file_here"},
		map[string]string{"type": "file", "file_id": "file_gone", "mount_path": "/data/missing"}, // dangling -> miss
		map[string]string{"type": "github_repository"},                                           // non-file -> skip, not a miss
	)
	block, n, misses := b.resolveFilesBlock(ctx, resources)
	if n != 1 {
		t.Errorf("injected = %d, want 1 (dangling + non-file skipped)", n)
	}
	if misses != 1 {
		t.Errorf("misses = %d, want 1 (the dangling mount; the non-file type is a skip, not a miss)", misses)
	}
	if !strings.Contains(block, "- /mnt/session/uploads/file_here (report.pdf, application/pdf, 2048 bytes)") {
		t.Errorf("block missing the resolved mount: %q", block)
	}
	if strings.Count(block, "\n- ") != 1 {
		t.Errorf("block should carry exactly the one resolved mount: %q", block)
	}

	// Malformed resources JSON is a logged skip, not a panic.
	if block, n, m := b.resolveFilesBlock(ctx, []byte("not json")); block != "" || n != 0 || m != 0 {
		t.Errorf("malformed resources = %q,%d,%d", block, n, m)
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
