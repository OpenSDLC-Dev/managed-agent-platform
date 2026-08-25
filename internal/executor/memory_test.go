package executor

import (
	"context"
	"encoding/json"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
)

// Plan 36 slice 4's executor rows (docs/plan/36_memory-stores.md decisions
// 9-12): a store lands in the sandbox before the tools run, and the run-end
// sync carries what the agent did back to the store — pushes, pulls,
// deletions on either side, the store winning a conflict, the wipe guard,
// pull-only stores, and what the store refuses.

const (
	memStoreID = "memstore_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	memMount   = "/mnt/memory/notes"
)

func (h *harness) seedMemoryStore(t *testing.T, id, name string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO memory_stores (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, id, name); err != nil {
		t.Fatalf("seed memory store: %v", err)
	}
}

// seedMemory plants a memory as the API's create would, with its created
// version, and returns the memory id.
func (h *harness) seedMemory(t *testing.T, storeID, path, content string) string {
	t.Helper()
	ctx := context.Background()
	memoryID := domain.NewID(domain.PrefixMemory).String()
	versionID := domain.NewID(domain.PrefixMemoryVersion).String()
	sha := sha256hex([]byte(content))
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO memory_versions (id, memory_store_id, memory_id, operation, path, content, content_sha256, content_size_bytes)
		 VALUES ($1, $2, $3, 'created', $4, $5, $6, $7)`,
		versionID, storeID, memoryID, path, content, sha, len(content)); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO memories (id, memory_store_id, path, content, content_sha256, content_size_bytes, memory_version_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		memoryID, storeID, path, content, sha, len(content), versionID); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	return memoryID
}

// refMemory points the session's resources[] at one store, the id-less element
// the API stores at create (slice 3).
func (h *harness) refMemory(t *testing.T, storeID, mount, access string) {
	t.Helper()
	raw, _ := json.Marshal([]map[string]any{{
		"type": "memory_store", "memory_store_id": storeID, "access": access, "instructions": nil,
		"name": "Notes", "description": "", "mount_path": mount,
	}})
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resources = $2::jsonb WHERE id = $1`, h.sid.String(), raw); err != nil {
		t.Fatalf("set session resources: %v", err)
	}
}

// memoryContent reads a memory's stored content by path; ok is false when
// there is none.
func (h *harness) memoryContent(t *testing.T, storeID, path string) (content string, ok bool) {
	t.Helper()
	err := h.pool.QueryRow(context.Background(),
		`SELECT content FROM memories WHERE memory_store_id = $1 AND path = $2`, storeID, path).Scan(&content)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", false
		}
		t.Fatalf("read memory %s: %v", path, err)
	}
	return content, true
}

// versionsOf lists a path's versions oldest first as "operation/actor-type".
func (h *harness) versionsOf(t *testing.T, storeID, path string) []string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT operation, coalesce(created_by->>'type', 'none'), coalesce(created_by->>'session_id', '')
		   FROM memory_versions WHERE memory_store_id = $1 AND path = $2 ORDER BY created_at, id`, storeID, path)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var op, actor, sid string
		if err := rows.Scan(&op, &actor, &sid); err != nil {
			t.Fatal(err)
		}
		if actor == "session_actor" && sid != h.sid.String() {
			t.Errorf("a session_actor version names session %q, want %s", sid, h.sid)
		}
		out = append(out, op+"/"+actor)
	}
	return out
}

// danglingHeads counts the store's memories whose memory_version_id names no
// version row — what the wire's pointer and a redact's head check both need
// to be zero.
func (h *harness) danglingHeads(t *testing.T, storeID string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM memories m WHERE m.memory_store_id = $1
		    AND NOT EXISTS (SELECT 1 FROM memory_versions v WHERE v.id = m.memory_version_id)`, storeID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) step(t *testing.T) {
	t.Helper()
	h.suspend(t, writeUse("out.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
}

func baselineOf(t *testing.T, sb *fakeSandbox, storeID string) memsync.Baseline {
	t.Helper()
	b, err := memsync.DecodeBaseline([]byte(sb.files[baselinePath(storeID)]))
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	return b
}

// materialized seeds a store with two memories, attaches it with the given
// access, and runs one tool_exec so the store is in the sandbox.
func materialized(t *testing.T, access string) (*harness, *fakeSandbox) {
	t.Helper()
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/notes.md", "hello")
	h.seedMemory(t, memStoreID, "/a/b.md", "deep")
	h.refMemory(t, memStoreID, memMount, access)
	h.step(t)
	return h, sb
}

// TestMaterializesMemoryStore: the store's memories land at the mount with the
// marker and a baseline that names them, in one batch; a second run finds the
// marker and lands nothing again.
func TestMaterializesMemoryStore(t *testing.T) {
	h, sb := materialized(t, "read_write")
	if got := sb.files[memMount+"/notes.md"]; got != "hello" {
		t.Errorf("/notes.md = %q", got)
	}
	if got := sb.files[memMount+"/a/b.md"]; got != "deep" {
		t.Errorf("/a/b.md = %q", got)
	}
	if got := sb.files[memMount+"/.anthropic-memory-store"]; got != "version 1\n"+memStoreID {
		t.Errorf("marker = %q", got)
	}
	b := baselineOf(t, sb, memStoreID)
	if b.Synced["/notes.md"] != sha256hex([]byte("hello")) || b.Synced["/a/b.md"] != sha256hex([]byte("deep")) || len(b.Synced) != 2 {
		t.Errorf("baseline = %+v", b)
	}
	// Two memories, the marker, the baseline: one batch.
	if !slices.Contains(sb.bulkSizes, 4) {
		t.Errorf("batches = %v, want one of four members", sb.bulkSizes)
	}
	// Every memory file is 0666 (decision 10); the marker and the baseline
	// take the default.
	for p, want := range map[string]fs.FileMode{
		memMount + "/notes.md": 0o666, memMount + "/a/b.md": 0o666,
		memMount + "/.anthropic-memory-store": 0, baselinePath(memStoreID): 0,
	} {
		if got := sb.modes[p]; got != want {
			t.Errorf("%s written with mode %o, want %o", p, got, want)
		}
	}

	before := len(sb.bulkSizes)
	h.step(t)
	if len(sb.bulkSizes) != before {
		t.Errorf("a second run wrote %d more batches; the marker should have been trusted", len(sb.bulkSizes)-before)
	}
	if got := sb.files[memMount+"/notes.md"]; got != "hello" {
		t.Errorf("after the second run /notes.md = %q", got)
	}
}

// TestMemorySyncPushesLocalChanges: a new file is created in the store, an
// edited one updated, a deleted one deleted — each with a session_actor
// version — and the baseline follows.
func TestMemorySyncPushesLocalChanges(t *testing.T) {
	h, sb := materialized(t, "read_write")
	sb.files[memMount+"/new.md"] = "fresh"
	sb.files[memMount+"/notes.md"] = "hello v2"
	delete(sb.files, memMount+"/a/b.md")
	h.step(t)

	if got, _ := h.memoryContent(t, memStoreID, "/new.md"); got != "fresh" {
		t.Errorf("/new.md in the store = %q", got)
	}
	if got, _ := h.memoryContent(t, memStoreID, "/notes.md"); got != "hello v2" {
		t.Errorf("/notes.md in the store = %q", got)
	}
	if _, ok := h.memoryContent(t, memStoreID, "/a/b.md"); ok {
		t.Error("/a/b.md still in the store after its local deletion")
	}
	for path, want := range map[string][]string{
		"/new.md":   {"created/session_actor"},
		"/notes.md": {"created/none", "modified/session_actor"},
		"/a/b.md":   {"created/none", "deleted/session_actor"},
	} {
		if got := h.versionsOf(t, memStoreID, path); !slices.Equal(got, want) {
			t.Errorf("versions of %s = %v, want %v", path, got, want)
		}
	}
	// Each head points at the version the push wrote, not at an id nobody
	// inserted — the wire's memory_version_id resolves, and a redact of the
	// head sees a head.
	if n := h.danglingHeads(t, memStoreID); n != 0 {
		t.Errorf("%d memories point at a version that was never written", n)
	}
	b := baselineOf(t, sb, memStoreID)
	if b.Synced["/new.md"] != sha256hex([]byte("fresh")) || b.Synced["/notes.md"] != sha256hex([]byte("hello v2")) || len(b.Synced) != 2 {
		t.Errorf("baseline = %+v", b)
	}
}

// TestMemoryMountsOrderByStore: the stores a run acts on come in store-id
// order, not mount order — the one order every session shares, so two
// sessions settling the same stores lock their rows the same way round.
func TestMemoryMountsOrderByStore(t *testing.T) {
	got := memoryMounts([]memoryRef{
		{Type: "memory_store", MemoryStoreID: "memstore_b", MountPath: "/mnt/memory/alpha"},
		{Type: "github_repository", MountPath: "/workspace/x"},
		{Type: "memory_store", MemoryStoreID: "memstore_a", MountPath: "/mnt/memory/zeta"},
	})
	if len(got) != 2 || got[0].MemoryStoreID != "memstore_a" || got[1].MemoryStoreID != "memstore_b" {
		t.Fatalf("mounts = %+v", got)
	}
}

// TestMemorySettleFailureSkipsTheStore: a store whose settlement fails — here
// a version row the database refuses — is skipped for the run, its baseline
// kept and its sync retried next time, while the run's own result commits:
// the tools do not re-run for a memory they never depended on.
func TestMemorySettleFailureSkipsTheStore(t *testing.T) {
	h, sb := materialized(t, "read_write")
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION map_test_refuse_version() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'refused by the test'; END $$;
		CREATE TRIGGER map_test_refuse_version BEFORE INSERT ON memory_versions
		  FOR EACH ROW WHEN (NEW.memory_store_id = '`+memStoreID+`') EXECUTE FUNCTION map_test_refuse_version()`); err != nil {
		t.Fatal(err)
	}
	dropTrigger := func() { _, _ = h.pool.Exec(ctx, `DROP TRIGGER IF EXISTS map_test_refuse_version ON memory_versions`) }
	t.Cleanup(dropTrigger)
	before := sb.files[baselinePath(memStoreID)]
	sb.files[memMount+"/new.md"] = "fresh"
	h.step(t)
	results := h.toolResults(t)
	if len(results) == 0 || results[len(results)-1].IsError {
		t.Fatalf("the run's own result did not commit: %+v", results)
	}
	if _, ok := h.memoryContent(t, memStoreID, "/new.md"); ok {
		t.Error("a push landed through a failed settlement")
	}
	if sb.files[baselinePath(memStoreID)] != before {
		t.Error("the baseline was rewritten for a store whose settlement failed")
	}
	dropTrigger()
	h.step(t)
	if got, _ := h.memoryContent(t, memStoreID, "/new.md"); got != "fresh" {
		t.Errorf("/new.md after the retry = %q", got)
	}
}

// TestMemorySyncPullsRemoteChanges: what changed in the store since the last
// sync lands in the directory — a changed file, a new one, and a deletion,
// which removes the local file.
func TestMemorySyncPullsRemoteChanges(t *testing.T) {
	h, sb := materialized(t, "read_write")
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx,
		`UPDATE memories SET content = 'remote v2', content_sha256 = $2 WHERE memory_store_id = $1 AND path = '/notes.md'`,
		memStoreID, sha256hex([]byte("remote v2"))); err != nil {
		t.Fatal(err)
	}
	h.seedMemory(t, memStoreID, "/c.md", "new remote")
	if _, err := h.pool.Exec(ctx, `DELETE FROM memories WHERE memory_store_id = $1 AND path = '/a/b.md'`, memStoreID); err != nil {
		t.Fatal(err)
	}
	h.step(t)

	if got := sb.files[memMount+"/notes.md"]; got != "remote v2" {
		t.Errorf("/notes.md = %q", got)
	}
	if got := sb.files[memMount+"/c.md"]; got != "new remote" {
		t.Errorf("/c.md = %q", got)
	}
	if _, ok := sb.files[memMount+"/a/b.md"]; ok {
		t.Error("/a/b.md still in the directory after its remote deletion")
	}
	b := baselineOf(t, sb, memStoreID)
	if b.Synced["/notes.md"] != sha256hex([]byte("remote v2")) || b.Synced["/c.md"] != sha256hex([]byte("new remote")) || len(b.Synced) != 2 {
		t.Errorf("baseline = %+v", b)
	}
	// Nothing was written to the store by the pull.
	if got := h.versionsOf(t, memStoreID, "/notes.md"); !slices.Equal(got, []string{"created/none"}) {
		t.Errorf("versions of /notes.md = %v", got)
	}
}

// TestMemorySyncStoreWinsAConflict is the lost race: a file edited locally
// while the store changed it too. The store's content wins, the local edit
// is overwritten, and the store's history shows no version from the
// session — decision 11's no-corroboration rule, counted as a conflict.
func TestMemorySyncStoreWinsAConflict(t *testing.T) {
	h, sb := materialized(t, "read_write")
	sb.files[memMount+"/notes.md"] = "mine"
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE memories SET content = 'theirs', content_sha256 = $2 WHERE memory_store_id = $1 AND path = '/notes.md'`,
		memStoreID, sha256hex([]byte("theirs"))); err != nil {
		t.Fatal(err)
	}
	h.step(t)

	if got := sb.files[memMount+"/notes.md"]; got != "theirs" {
		t.Errorf("/notes.md = %q, want the store's", got)
	}
	if got, _ := h.memoryContent(t, memStoreID, "/notes.md"); got != "theirs" {
		t.Errorf("the store's /notes.md = %q; the losing edit was pushed", got)
	}
	if got := h.versionsOf(t, memStoreID, "/notes.md"); !slices.Equal(got, []string{"created/none"}) {
		t.Errorf("versions of /notes.md = %v; the session wrote one", got)
	}
	if b := baselineOf(t, sb, memStoreID); b.Synced["/notes.md"] != sha256hex([]byte("theirs")) {
		t.Errorf("baseline = %+v", b)
	}
}

// TestMemorySyncWipeGuard: an `rm -rf` of the mount — marker and all — against
// a baseline of two files is a wiped mount, not two deletions: both come
// back with the marker, nothing is deleted from the store, and the rebuilt
// directory pushes again at the next run rather than staying pull-only. The
// wipe happens inside a run, after materialization — the reaper's standalone
// sync is that shape exactly, and a wipe between runs would be repaired by
// the next materialization before any sync judged it.
func TestMemorySyncWipeGuard(t *testing.T) {
	h, sb := materialized(t, "read_write")
	delete(sb.files, memMount+"/notes.md")
	delete(sb.files, memMount+"/a/b.md")
	delete(sb.files, memMount+"/.anthropic-memory-store")
	if err := h.exec.syncMemoryStandalone(context.Background(), sb, h.sid); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if sb.files[memMount+"/notes.md"] != "hello" || sb.files[memMount+"/a/b.md"] != "deep" {
		t.Errorf("the wiped directory was not rebuilt: %v", sb.files)
	}
	if got := sb.files[memMount+"/.anthropic-memory-store"]; got != string(memsync.MarkerBytes(memStoreID)) {
		t.Errorf("the rebuilt directory's marker = %q", got)
	}
	for _, path := range []string{"/notes.md", "/a/b.md"} {
		if _, ok := h.memoryContent(t, memStoreID, path); !ok {
			t.Errorf("%s was deleted from the store", path)
		}
	}
	sb.files[memMount+"/after.md"] = "written after the rebuild"
	h.step(t)
	if got, _ := h.memoryContent(t, memStoreID, "/after.md"); got != "written after the rebuild" {
		t.Errorf("/after.md in the store = %q; the rebuilt directory stayed pull-only", got)
	}
}

// TestMemorySyncPullOnly: a read_only attachment, an archived store, and a
// directory whose marker was altered are each pulled from and never pushed to
// — a local edit and a new local file stay local, a remote change still lands.
func TestMemorySyncPullOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		access string
		arm    func(t *testing.T, h *harness, sb *fakeSandbox)
	}{
		{"read_only attachment", "read_only", func(*testing.T, *harness, *fakeSandbox) {}},
		{"archived store", "read_write", func(t *testing.T, h *harness, _ *fakeSandbox) {
			if _, err := h.pool.Exec(context.Background(),
				`UPDATE memory_stores SET archived_at = now() WHERE id = $1`, memStoreID); err != nil {
				t.Fatal(err)
			}
		}},
		{"altered marker", "read_write", func(_ *testing.T, _ *harness, sb *fakeSandbox) {
			sb.files[memMount+"/.anthropic-memory-store"] = "version 1\nmemstore_00000000000000000000000001"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sb := materialized(t, tc.access)
			tc.arm(t, h, sb)
			sb.files[memMount+"/notes.md"] = "edited locally"
			sb.files[memMount+"/new.md"] = "new locally"
			if _, err := h.pool.Exec(context.Background(),
				`UPDATE memories SET content = 'deeper', content_sha256 = $2 WHERE memory_store_id = $1 AND path = '/a/b.md'`,
				memStoreID, sha256hex([]byte("deeper"))); err != nil {
				t.Fatal(err)
			}
			h.step(t)

			if got, _ := h.memoryContent(t, memStoreID, "/notes.md"); got != "hello" {
				t.Errorf("the store's /notes.md = %q; a pull-only store was pushed to", got)
			}
			if _, ok := h.memoryContent(t, memStoreID, "/new.md"); ok {
				t.Error("a new local file reached a pull-only store")
			}
			if got := sb.files[memMount+"/a/b.md"]; got != "deeper" {
				t.Errorf("/a/b.md = %q; the remote change was not pulled", got)
			}
			if got := sb.files[memMount+"/notes.md"]; got != "edited locally" {
				t.Errorf("/notes.md = %q; the local edit was overwritten though the store did not change", got)
			}
		})
	}
}

// TestMemorySyncRefusals: what the store would refuse is not pushed and is
// remembered so it is not re-read every run — a body over 100 kB, invalid
// UTF-8, a path the store rejects — and a create at an ancestor of a memory
// is the conflict the store wins: the file goes, not remembered either way.
func TestMemorySyncRefusals(t *testing.T) {
	h, sb := materialized(t, "read_write")
	big := strings.Repeat("x", memsync.MaxContentBytes+1)
	sb.files[memMount+"/big.md"] = big
	sb.files[memMount+"/bad.md"] = "not utf-8 \xff"
	sb.files[memMount+"/bad\x01name"] = "control byte in the name"
	sb.files[memMount+"/a"] = "occupies /a/b.md's ancestor"
	h.step(t)

	for _, path := range []string{"/big.md", "/bad.md", "/bad\x01name", "/a"} {
		if _, ok := h.memoryContent(t, memStoreID, path); ok {
			t.Errorf("%s reached the store", path)
		}
	}
	b := baselineOf(t, sb, memStoreID)
	for path, content := range map[string]string{"/big.md": big, "/bad.md": "not utf-8 \xff", "/bad\x01name": "control byte in the name"} {
		if b.Refused[path] != sha256hex([]byte(content)) {
			t.Errorf("%s not remembered as refused: %+v", path, b.Refused)
		}
	}
	if _, ok := b.Refused["/a"]; ok {
		t.Error("an occupancy conflict was recorded as a refusal")
	}
	if _, ok := b.Synced["/a"]; ok {
		t.Error("an occupancy conflict was recorded as synced")
	}
	// The refused files stay as the agent wrote them; the file in the way of
	// the store's `/a/b.md` is gone, and `/a/b.md` is still there.
	if sb.files[memMount+"/big.md"] != big {
		t.Error("a refused file was removed or rewritten")
	}
	if _, ok := sb.files[memMount+"/a"]; ok {
		t.Error("a file at the ancestor of a memory survived the conflict; the store did not win")
	}
	if sb.files[memMount+"/a/b.md"] != "deep" {
		t.Errorf("/a/b.md = %q after the conflict", sb.files[memMount+"/a/b.md"])
	}
}

// TestMemorySyncStoreWinsAPrefixConflict: a memory created in the store at
// an ancestor or a descendant of a file the agent created cannot share the
// filesystem with it, and left in place the file would block the memory's
// pull at every later run. The store wins: the file goes, the memory lands.
func TestMemorySyncStoreWinsAPrefixConflict(t *testing.T) {
	h, sb := materialized(t, "read_write")
	h.seedMemory(t, memStoreID, "/e", "the store's e")        // local /e/f.md is under it
	h.seedMemory(t, memStoreID, "/c/d.md", "the store's c/d") // local /c is above it
	sb.files[memMount+"/e/f.md"] = "the agent's e/f.md"
	sb.files[memMount+"/c"] = "the agent's c"
	h.step(t)
	for _, gone := range []string{"/e/f.md", "/c"} {
		if _, ok := sb.files[memMount+gone]; ok {
			t.Errorf("%s survived the conflict with the store's memory", gone)
		}
		if _, ok := h.memoryContent(t, memStoreID, gone); ok {
			t.Errorf("%s reached the store", gone)
		}
	}
	if sb.files[memMount+"/e"] != "the store's e" || sb.files[memMount+"/c/d.md"] != "the store's c/d" {
		t.Errorf("the store's memories did not land: %v", sb.files)
	}
	b := baselineOf(t, sb, memStoreID)
	if b.Synced["/e"] != sha256hex([]byte("the store's e")) || b.Synced["/c/d.md"] != sha256hex([]byte("the store's c/d")) || len(b.Refused) != 0 {
		t.Errorf("baseline = %+v", b)
	}
}

// TestMemorySyncCapRefusesTheCreate: the 2,001st memory is the API's own
// refusal, so the sync's create push holds it too — but the store's fullness
// is not remembered against the file, and a deletion in the same settlement
// makes the room the create then takes.
func TestMemorySyncCapRefusesTheCreate(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedMemoryStore(t, memStoreID, "Full")
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO memories (id, memory_store_id, path, content, content_sha256, content_size_bytes, memory_version_id)
		 SELECT 'mem_' || lpad(i::text, 26, '0'), $1, '/m/' || i, '', $2, 0, 'memver_' || lpad(i::text, 26, '0')
		   FROM generate_series(1, $3) AS i`, memStoreID, sha256hex(nil), memsync.MaxMemoriesPerStore); err != nil {
		t.Fatal(err)
	}
	h.refMemory(t, memStoreID, memMount, "read_write")
	h.step(t)
	if !slices.Contains(sb.bulkSizes, memsync.MaxMemoriesPerStore+2) {
		t.Fatalf("batches = %v, want the full store in one", sb.bulkSizes)
	}
	sb.files[memMount+"/one-more.md"] = "over the cap"
	h.step(t)
	if _, ok := h.memoryContent(t, memStoreID, "/one-more.md"); ok {
		t.Error("the 2,001st memory was created by the sync")
	}
	if b := baselineOf(t, sb, memStoreID); len(b.Refused) != 0 {
		t.Errorf("the store's fullness was remembered against the file: %+v", b.Refused)
	}
	// `/m/1` deleted locally sorts before `/one-more.md`: its deletion settles
	// first and the create fits.
	delete(sb.files, memMount+"/m/1")
	h.step(t)
	if got, _ := h.memoryContent(t, memStoreID, "/one-more.md"); got != "over the cap" {
		t.Errorf("/one-more.md in the store = %q; the room a deletion made was not used", got)
	}
	if _, ok := h.memoryContent(t, memStoreID, "/m/1"); ok {
		t.Error("/m/1 survived its local deletion")
	}
}

// TestMemorySyncSkipsAnUnreliableListing: a listing that overflows the exec
// output cap, or one that failed — a directory find could not enter, which
// the command's pipefail turns into its exit — skips the store: nothing
// pushed, nothing pulled, nothing deleted, the baseline untouched, because a
// partial listing reads as deletions.
func TestMemorySyncSkipsAnUnreliableListing(t *testing.T) {
	for name, arm := range map[string]func(*fakeSandbox){
		"truncated": func(sb *fakeSandbox) { sb.listTruncated = true },
		"failed":    func(sb *fakeSandbox) { sb.listExit = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			h, sb := materialized(t, "read_write")
			before := sb.files[baselinePath(memStoreID)]
			arm(sb)
			sb.files[memMount+"/new.md"] = "fresh"
			delete(sb.files, memMount+"/a/b.md")
			h.step(t)
			if _, ok := h.memoryContent(t, memStoreID, "/new.md"); ok {
				t.Error("a store with an unreliable listing was pushed to")
			}
			if _, ok := h.memoryContent(t, memStoreID, "/a/b.md"); !ok {
				t.Error("a store with an unreliable listing had a memory deleted")
			}
			if sb.files[baselinePath(memStoreID)] != before {
				t.Error("the baseline was rewritten for a skipped store")
			}
		})
	}
}

// TestMemoryStoreMissingOrUntrusted: a store whose row is gone materializes
// nothing (the brain's block hedges it); a directory that holds files but no
// marker is left as found and never pushed from; a store deleted after the
// attachment leaves the directory alone.
func TestMemoryStoreMissingOrUntrusted(t *testing.T) {
	t.Run("missing store", func(t *testing.T) {
		sb := &fakeSandbox{}
		h := newHarness(t, sb)
		h.refMemory(t, memStoreID, memMount, "read_write")
		h.step(t)
		if _, ok := sb.files[memMount+"/.anthropic-memory-store"]; ok {
			t.Error("a marker was written for a store that does not exist")
		}
	})
	t.Run("files without a marker", func(t *testing.T) {
		sb := &fakeSandbox{files: map[string]string{memMount + "/loose.md": "already here"}}
		h := newHarness(t, sb)
		h.seedMemoryStore(t, memStoreID, "Notes")
		h.seedMemory(t, memStoreID, "/notes.md", "hello")
		h.refMemory(t, memStoreID, memMount, "read_write")
		h.step(t)
		// Not re-materialized: no marker is stamped over a directory nothing
		// vouches for. The run-end sync still pulls into it (decision 12:
		// "pulled from and never pushed to"), and its own file stays local.
		if _, ok := sb.files[memMount+"/.anthropic-memory-store"]; ok {
			t.Error("a marker was stamped over a directory with files and no marker")
		}
		if got := sb.files[memMount+"/notes.md"]; got != "hello" {
			t.Errorf("/notes.md = %q; the untrusted directory was not pulled into", got)
		}
		if _, ok := h.memoryContent(t, memStoreID, "/loose.md"); ok {
			t.Error("a file from an untrusted directory was pushed")
		}
		if sb.files[memMount+"/loose.md"] != "already here" {
			t.Error("the untrusted directory's own file was changed")
		}
	})
	t.Run("listing failed", func(t *testing.T) {
		// A listing that fails vouches for nothing: the directory is neither
		// overwritten with the store nor synced from.
		sb := &fakeSandbox{listExit: 123}
		h := newHarness(t, sb)
		h.seedMemoryStore(t, memStoreID, "Notes")
		h.seedMemory(t, memStoreID, "/notes.md", "hello")
		h.refMemory(t, memStoreID, memMount, "read_write")
		h.step(t)
		for _, p := range []string{memMount + "/.anthropic-memory-store", memMount + "/notes.md", baselinePath(memStoreID)} {
			if _, ok := sb.files[p]; ok {
				t.Errorf("%s was written over a directory whose listing failed", p)
			}
		}
	})
	t.Run("store deleted after attach", func(t *testing.T) {
		h, sb := materialized(t, "read_write")
		if _, err := h.pool.Exec(context.Background(), `DELETE FROM memory_stores WHERE id = $1`, memStoreID); err != nil {
			t.Fatal(err)
		}
		sb.files[memMount+"/notes.md"] = "edited after the delete"
		h.step(t)
		if sb.files[memMount+"/notes.md"] != "edited after the delete" {
			t.Error("the directory of a deleted store was changed")
		}
	})
}

// TestMemorySyncSkipsAFaultedRun: a run that ended in a backend fault does
// not sync — the sandbox is not one to wait on, and the next run does.
func TestMemorySyncSkipsAFaultedRun(t *testing.T) {
	h, sb := materialized(t, "read_write")
	sb.files[memMount+"/new.md"] = "fresh"
	sb.failPath = "faulty.txt"
	uses := h.suspend(t, writeUse("faulty.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	// step reports a fault from inside its span, not as its error: the run
	// faulted if the tool it ran stays unanswered for the lease's reclaim.
	if answered, err := events.Answered(context.Background(), h.pool, h.sid, uses[0].ID); err != nil || answered {
		t.Fatalf("answered = %v, %v; the write was meant to fault", answered, err)
	}
	if _, ok := h.memoryContent(t, memStoreID, "/new.md"); ok {
		t.Error("a faulted run synced")
	}
	// The faulted item stays leased for the reclaim; lapse the lease so the
	// next step reclaims it, now against a sandbox that answers.
	sb.failPath = ""
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE session_id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
	}
	if _, ok := h.memoryContent(t, memStoreID, "/new.md"); !ok {
		t.Error("the next healthy run did not sync")
	}
}

// TestReaperSyncsBeforeReaping: the idle reaper's twin of the run-end sync —
// what is in the directory reaches the store before the sandbox goes, through
// Attach rather than a provision.
func TestReaperSyncsBeforeReaping(t *testing.T) {
	h, sb := materialized(t, "read_write")
	sb.files[memMount+"/late.md"] = "written after the last run"
	setStatus(t, h, "terminated")
	h.prov.owned = []domain.ID{h.sid}
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
		t.Error("the sandbox was not reaped")
	}
	if got, _ := h.memoryContent(t, memStoreID, "/late.md"); got != "written after the last run" {
		t.Errorf("the store's /late.md = %q; the reaper did not sync", got)
	}
	if got := h.versionsOf(t, memStoreID, "/late.md"); !slices.Equal(got, []string{"created/session_actor"}) {
		t.Errorf("versions = %v", got)
	}
}
