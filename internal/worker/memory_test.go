package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/worktoken"
)

// Plan 36 slice 6 (docs/plan/36_memory-stores.md decision 16): the BYOC
// worker's half of memory stores over the wire — the item's secret decoded
// to the sessions token, a store landed in the sandbox from the memory
// routes, the run-end sync's pushes, pulls and deletions through the same
// routes with their preconditions, the store winning a conflict, the races
// lost between a listing and a write, pull-only stores, what the store
// refuses, and the item a token-less server hands out.

const memMount = "/mnt/memory/notes"

// A valid, lowercase memory-store id — the API's own routes run checkID
// (domain.ID.Valid), which the executor tests' raw-SQL constant would fail.
var memStoreID = domain.NewID(domain.PrefixMemoryStore).String()

// memoryExec answers memsync.HashTreeCommand and memsync.RemoveCommands from
// the fake's in-memory tree — the executor test fake's answers: every file
// under the mount but the marker, digest first, in byte order, NUL-terminated
// as `sha256sum -z` prints it; an absent directory lists nothing and exits 0.
func (f *fakeSandbox) memoryExec(cmd string) (sandbox.ExecResult, bool) {
	if mount, ok := hashTreeMount(cmd); ok {
		var paths []string
		for p := range f.files {
			if strings.HasPrefix(p, mount+"/") && p != mount+"/"+memsync.MarkerName {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		var out strings.Builder
		for _, p := range paths {
			out.WriteString(sha256hex([]byte(f.files[p])) + "  ." + strings.TrimPrefix(p, mount) + "\x00")
		}
		return sandbox.ExecResult{Stdout: out.String()}, true
	}
	if strings.Contains(cmd, "sha256sum -z") {
		// A listing whose shape the fake no longer recognizes must not read
		// as an empty directory and arm the wipe guard.
		return sandbox.ExecResult{ExitCode: 2, Stderr: "fake sandbox: unrecognized memory listing"}, true
	}
	if rest, ok := strings.CutPrefix(cmd, "[ -d '"); ok && strings.Contains(rest, "; rm -f -- ") {
		mount, list, _ := strings.Cut(rest, "' ] || exit 0; ")
		_, list, _ = strings.Cut(list, "; rm -f -- ")
		list, _, _ = strings.Cut(list, " || exit 1")
		for _, q := range strings.Split(list, "' '") {
			p := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(q, "'"), "'"), `'\''`, "'")
			delete(f.files, mount+"/"+p)
		}
		return sandbox.ExecResult{}, true
	}
	return sandbox.ExecResult{}, false
}

func hashTreeMount(cmd string) (string, bool) {
	rest, ok := strings.CutPrefix(cmd, "[ -d '")
	if !ok || !strings.Contains(cmd, "sha256sum -z") {
		return "", false
	}
	mount, _, ok := strings.Cut(rest, "' ] || exit 0; cd -P '")
	return mount, ok
}

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

// rewriteMemory moves a memory's head, as another session's write would.
func (h *harness) rewriteMemory(t *testing.T, storeID, path, content string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE memories SET content = $3, content_sha256 = $4, content_size_bytes = $5, updated_at = now()
		  WHERE memory_store_id = $1 AND path = $2`,
		storeID, path, content, sha256hex([]byte(content)), len(content)); err != nil {
		t.Fatalf("rewrite memory: %v", err)
	}
}

// refMemory points the session's resources[] at stores, the id-less elements
// the API stores at create; each triple is store id, mount, access.
func (h *harness) refMemory(t *testing.T, refs ...[3]string) {
	t.Helper()
	elements := make([]map[string]any, 0, len(refs))
	for _, r := range refs {
		elements = append(elements, map[string]any{
			"type": "memory_store", "memory_store_id": r[0], "access": r[2], "instructions": nil,
			"name": "Notes", "description": "", "mount_path": r[1],
		})
	}
	raw, _ := json.Marshal(elements)
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
		if errors.Is(err, pgx.ErrNoRows) {
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

// baseline reads a store's baseline file back from the sandbox.
func (h *harness) baseline(t *testing.T, storeID string) memsync.Baseline {
	t.Helper()
	b, err := memsync.DecodeBaseline([]byte(h.prov.sb.files[baselinePath(storeID)]))
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	return b
}

// sessionsToken claims a live work item for the session and mints its
// token, as the poll does for a store session — the credential the memory
// routes admit. The item stays live for its lease, which the runs below
// share; a test that runs the lease loop itself lets the poll mint instead.
func (h *harness) sessionsToken(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	h.enqueueWork(t)
	work, err := queue.New(h.pool).Poll(ctx, h.envID, 10*time.Minute)
	if err != nil || work == nil {
		t.Fatalf("poll: %v, %v", work, err)
	}
	token, err := worktoken.Mint(ctx, h.pool, work.ID.String(), h.sid.String())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return token
}

// runWith runs the driver with the token, on a session suspended on the
// given uses (a workspace write when the test needs a run and nothing else).
func (h *harness) runWith(t *testing.T, token string, uses ...string) []resultBody {
	t.Helper()
	if len(uses) == 0 {
		uses = []string{writeUse("out.txt", "x")}
	}
	before := len(h.results(t))
	h.suspend(t, uses...)
	if err := RunSessionTools(context.Background(), h.client, h.prov, h.sid.String(), ToolExecConfig{SessionsToken: token}); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}
	return h.results(t)[before:]
}

// TestMemoryStoreServedThroughTheLeaseLoop is the item's whole path: the
// poll mints the token into the item's secret, the worker decodes it, lands
// the store in the sandbox with the marker and baseline, runs the tool, and
// the run's end pushes what the agent wrote as the session's own version.
func TestMemoryStoreServedThroughTheLeaseLoop(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/facts/a.md", "alpha")
	h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
	h.suspend(t, writeUse(memMount+"/log/b.md", "hello"))
	h.enqueueWork(t)

	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)
	waitExit(t, cancel, errc)

	var minted int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_session_tokens WHERE session_id = $1`, h.sid.String()).Scan(&minted); err != nil || minted != 1 {
		t.Fatalf("tokens minted for the session = %d (%v), want 1", minted, err)
	}
	if got := sb.files[memMount+"/"+memsync.MarkerName]; got != string(memsync.MarkerBytes(memStoreID)) {
		t.Errorf("marker = %q", got)
	}
	if got := sb.files[memMount+"/facts/a.md"]; got != "alpha" {
		t.Errorf("landed memory = %q, want alpha", got)
	}
	results := h.results(t)
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want one clean write", results)
	}
	if got, ok := h.memoryContent(t, memStoreID, "/log/b.md"); !ok || got != "hello" {
		t.Errorf("pushed memory = %q, %v; want hello", got, ok)
	}
	if got := h.versionsOf(t, memStoreID, "/log/b.md"); !equalStrings(got, []string{"created/session_actor"}) {
		t.Errorf("versions = %v, want the session's created", got)
	}
	b := h.baseline(t, memStoreID)
	if b.Synced["/facts/a.md"] != sha256hex([]byte("alpha")) || b.Synced["/log/b.md"] != sha256hex([]byte("hello")) {
		t.Errorf("baseline = %+v", b)
	}
}

// TestMemorySyncOverTheWire walks the decision table through the routes: a
// store's change reaches a held mount before the tools read it; a both-sides
// change is the store's, pulled over the local edit; a local deletion
// deletes with the baseline's digest and is attributed; a store's deletion
// removes the file; an edit of a memory the store deleted is re-created.
func TestMemorySyncOverTheWire(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/facts/a.md", "alpha")
	h.seedMemory(t, memStoreID, "/facts/b.md", "beta")
	h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
	token := h.sessionsToken(t)
	h.runWith(t, token)
	if sb.files[memMount+"/facts/a.md"] != "alpha" || sb.files[memMount+"/facts/b.md"] != "beta" {
		t.Fatalf("the store did not land: %v", sb.files)
	}

	// A held mount is reconciled before the tools run: the read sees the
	// store's newer content, not the run's stale copy.
	h.rewriteMemory(t, memStoreID, "/facts/a.md", "alpha two")
	res := h.runWith(t, token, readUse(memMount+"/facts/a.md"))
	if text, _ := res[0].Content[0]["text"].(string); !strings.Contains(text, "alpha two") {
		t.Errorf("read before the sync = %q, want the store's alpha two", text)
	}

	// Both sides changed: the store wins, the local edit is overwritten and
	// never pushed.
	sb.files[memMount+"/facts/a.md"] = "mine"
	h.rewriteMemory(t, memStoreID, "/facts/a.md", "theirs")
	h.runWith(t, token)
	if sb.files[memMount+"/facts/a.md"] != "theirs" {
		t.Errorf("conflict left the local edit: %q", sb.files[memMount+"/facts/a.md"])
	}
	if got, _ := h.memoryContent(t, memStoreID, "/facts/a.md"); got != "theirs" {
		t.Errorf("conflict pushed the local edit: %q", got)
	}
	if h.baseline(t, memStoreID).Synced["/facts/a.md"] != sha256hex([]byte("theirs")) {
		t.Errorf("baseline after the conflict = %+v", h.baseline(t, memStoreID))
	}

	// A local deletion (as bash would) deletes in the store, guarded by the
	// baseline's digest, and is the session's version.
	delete(sb.files, memMount+"/facts/b.md")
	h.runWith(t, token)
	if _, ok := h.memoryContent(t, memStoreID, "/facts/b.md"); ok {
		t.Error("the local deletion did not reach the store")
	}
	if got := h.versionsOf(t, memStoreID, "/facts/b.md"); !equalStrings(got, []string{"created/none", "deleted/session_actor"}) {
		t.Errorf("versions of the deleted memory = %v", got)
	}
	if _, ok := h.baseline(t, memStoreID).Synced["/facts/b.md"]; ok {
		t.Error("the baseline still names the deleted memory")
	}

	// The store's deletion removes the unchanged file.
	if _, err := h.pool.Exec(context.Background(), `DELETE FROM memories WHERE memory_store_id = $1 AND path = '/facts/a.md'`, memStoreID); err != nil {
		t.Fatal(err)
	}
	h.runWith(t, token)
	if _, ok := sb.files[memMount+"/facts/a.md"]; ok {
		t.Error("the store's deletion left the file")
	}

	// A memory the store deleted but the directory edited is re-created.
	h.seedMemory(t, memStoreID, "/facts/c.md", "gamma")
	h.runWith(t, token)
	sb.files[memMount+"/facts/c.md"] = "gamma kept"
	if _, err := h.pool.Exec(context.Background(), `DELETE FROM memories WHERE memory_store_id = $1 AND path = '/facts/c.md'`, memStoreID); err != nil {
		t.Fatal(err)
	}
	h.runWith(t, token)
	if got, ok := h.memoryContent(t, memStoreID, "/facts/c.md"); !ok || got != "gamma kept" {
		t.Errorf("edited-and-deleted memory = %q, %v; want re-created", got, ok)
	}
}

// TestMemorySyncLosesRacesToTheStore stages the two races the routes'
// preconditions exist for, between the listing and the write: an update
// whose memory is deleted meanwhile (404) re-creates it, the file being the
// only copy; a delete whose memory changed meanwhile (409) is dropped, the
// baseline kept so the next sync pulls the store's version.
func TestMemorySyncLosesRacesToTheStore(t *testing.T) {
	var (
		h        *harness
		race     string
		memoryOf = func(r *http.Request) string { return r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] }
	)
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/memories/mem_") {
				switch {
				case race == "update" && r.Method == http.MethodPost:
					race = ""
					if _, err := h.pool.Exec(r.Context(), `DELETE FROM memories WHERE id = $1`, memoryOf(r)); err != nil {
						panic(err)
					}
				case race == "delete" && r.Method == http.MethodDelete:
					race = ""
					if _, err := h.pool.Exec(r.Context(),
						`UPDATE memories SET content = 'raced', content_sha256 = $2 WHERE id = $1`,
						memoryOf(r), sha256hex([]byte("raced"))); err != nil {
						panic(err)
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	sb := &fakeSandbox{}
	h = newHarnessWrapped(t, sb, wrap)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/a.md", "alpha")
	h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
	token := h.sessionsToken(t)
	h.runWith(t, token)

	sb.files[memMount+"/a.md"] = "mine"
	race = "update"
	h.runWith(t, token)
	if got, ok := h.memoryContent(t, memStoreID, "/a.md"); !ok || got != "mine" {
		t.Errorf("after the 404: memory = %q, %v; want re-created with the edit", got, ok)
	}
	if got := h.versionsOf(t, memStoreID, "/a.md"); !equalStrings(got, []string{"created/none", "created/session_actor"}) {
		t.Errorf("versions = %v", got)
	}

	// A mounted store is reconciled before the tools run and again after, so
	// the lost delete and the pull that restores the store's version happen
	// in one run: the pre-tools sync's delete loses the precondition (the
	// wrap moved the memory to "raced"), and the post-tools sync sees the
	// store's newer version and pulls it back over the local deletion.
	delete(sb.files, memMount+"/a.md")
	race = "delete"
	h.runWith(t, token)
	if got, ok := h.memoryContent(t, memStoreID, "/a.md"); !ok || got != "raced" {
		t.Errorf("after the 409: memory = %q, %v; want the store's raced kept", got, ok)
	}
	if sb.files[memMount+"/a.md"] != "raced" {
		t.Errorf("the store's version was not pulled over the local deletion: %q", sb.files[memMount+"/a.md"])
	}
	if h.baseline(t, memStoreID).Synced["/a.md"] != sha256hex([]byte("raced")) {
		t.Errorf("baseline after the restore = %+v, want the pulled digest", h.baseline(t, memStoreID))
	}
}

// TestMemoryPullOnlyStores: a read_only attachment refuses the file tools'
// writes, and what bash writes there stays local while the store's changes
// still arrive; a directory found with files but no marker is not
// re-materialized and is synced pull-only until the sandbox is replaced.
func TestMemoryPullOnlyStores(t *testing.T) {
	otherID, otherMount := domain.NewID(domain.PrefixMemoryStore).String(), "/mnt/memory/other"
	sb := &fakeSandbox{files: map[string]string{otherMount + "/stray.md": "stray"}}
	h := newHarness(t, sb)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/a.md", "alpha")
	h.seedMemoryStore(t, otherID, "Other")
	h.seedMemory(t, otherID, "/r.md", "remote")
	h.refMemory(t, [3]string{memStoreID, memMount, "read_only"}, [3]string{otherID, otherMount, "read_write"})
	token := h.sessionsToken(t)

	res := h.runWith(t, token, writeUse(memMount+"/x.md", "nope"))
	if text, _ := res[0].Content[0]["text"].(string); !res[0].IsError || !strings.Contains(text, "read-only directory") {
		t.Errorf("write into a read_only store = %+v, want refused", res[0])
	}
	sb.files[memMount+"/y.md"] = "bash wrote"
	h.rewriteMemory(t, memStoreID, "/a.md", "alpha two")
	h.runWith(t, token)
	if _, ok := h.memoryContent(t, memStoreID, "/y.md"); ok {
		t.Error("a read_only store took a push")
	}
	if sb.files[memMount+"/a.md"] != "alpha two" {
		t.Errorf("a read_only store's change was not pulled: %q", sb.files[memMount+"/a.md"])
	}
	if _, ok := h.baseline(t, memStoreID).Synced["/y.md"]; ok {
		t.Error("a withheld push entered the baseline")
	}

	// The untrusted directory: no marker written, the stray file never
	// pushed, the store's memory pulled in beside it.
	if _, ok := sb.files[otherMount+"/"+memsync.MarkerName]; ok {
		t.Error("an untrusted directory was stamped")
	}
	if _, ok := h.memoryContent(t, otherID, "/stray.md"); ok {
		t.Error("an untrusted directory's file was pushed")
	}
	if sb.files[otherMount+"/r.md"] != "remote" {
		t.Errorf("an untrusted directory was not pulled into: %v", sb.files)
	}
}

// TestMemoryStoreRefusalsOverTheWire: the occupancy 409 on a create removes
// the file the store's memory is in the way of; the 2,000 cap's 400 is the
// store's state, refused but not remembered (so a retry lands once room is
// made); an archive's 400 makes the rest of the sync pull-only, the edit
// kept for the sync after the unarchive.
func TestMemoryStoreRefusalsOverTheWire(t *testing.T) {
	// The cap is the store's state, not a body's, so the reference worker's
	// remember-and-skip must not apply. Seeding 2,000 rows would also list
	// 2,000 heads to pull; the store's own message injected on the one create
	// is the same 400 the worker reads, without the bulk.
	var capNext string
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if capNext != "" && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories") {
				var body struct{ Path string }
				peek := &bytes.Buffer{}
				_ = json.NewDecoder(io.TeeReader(r.Body, peek)).Decode(&body)
				if body.Path == capNext {
					// Left set across the run: a mounted store syncs before the
					// tools and after, so a one-shot injection would let the
					// second create through. The test clears it between runs.
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"memory store ` + memStoreID +
						` holds ` + strconv.Itoa(memsync.MaxMemoriesPerStore) + ` memories"}}`))
					return
				}
				r.Body = io.NopCloser(peek)
			}
			next.ServeHTTP(w, r)
		})
	}
	sb := &fakeSandbox{}
	h := newHarnessWrapped(t, sb, wrap)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/a.md", "alpha")
	h.seedMemory(t, memStoreID, "/x/y.md", "why")
	h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
	token := h.sessionsToken(t)
	h.runWith(t, token)

	sb.files[memMount+"/x"] = "a file where the store has a directory"
	h.runWith(t, token)
	if _, ok := sb.files[memMount+"/x"]; ok {
		t.Error("the occupancy conflict left the file")
	}
	if _, ok := h.memoryContent(t, memStoreID, "/x"); ok {
		t.Error("the occupancy conflict pushed the file")
	}

	// The store answers the create with its cap 400 on every attempt this
	// run: refused, not remembered.
	capNext = "/new.md"
	sb.files[memMount+"/new.md"] = "one too many"
	h.runWith(t, token)
	if _, ok := h.memoryContent(t, memStoreID, "/new.md"); ok {
		t.Error("the cap did not refuse the create")
	}
	if b := h.baseline(t, memStoreID); len(b.Refused) != 0 {
		t.Errorf("the cap's refusal was remembered: %+v", b.Refused)
	}
	// Room made: the store no longer 400s, and the retry lands.
	capNext = ""
	h.runWith(t, token)
	if got, ok := h.memoryContent(t, memStoreID, "/new.md"); !ok || got != "one too many" {
		t.Errorf("after room was made: %q, %v; want the retry to land", got, ok)
	}

	// Archived: the edit stays local, the baseline keeps the digest the
	// store holds, and the unarchive lets the next sync push it.
	if _, err := h.pool.Exec(context.Background(), `UPDATE memory_stores SET archived_at = now() WHERE id = $1`, memStoreID); err != nil {
		t.Fatal(err)
	}
	sb.files[memMount+"/a.md"] = "edited while archived"
	sb.files[memMount+"/b.md"] = "created while archived"
	h.runWith(t, token)
	if got, _ := h.memoryContent(t, memStoreID, "/a.md"); got != "alpha" {
		t.Errorf("an archived store took a push: %q", got)
	}
	if b := h.baseline(t, memStoreID); b.Synced["/a.md"] != sha256hex([]byte("alpha")) || len(b.Refused) != 0 {
		t.Errorf("baseline while archived = %+v", b)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE memory_stores SET archived_at = NULL WHERE id = $1`, memStoreID); err != nil {
		t.Fatal(err)
	}
	h.runWith(t, token)
	if got, _ := h.memoryContent(t, memStoreID, "/a.md"); got != "edited while archived" {
		t.Errorf("after the unarchive: %q, want the edit pushed", got)
	}
	if got, _ := h.memoryContent(t, memStoreID, "/b.md"); got != "created while archived" {
		t.Errorf("after the unarchive: %q, want the create pushed", got)
	}
}

// TestMemoryStoreGoneIsSkipped: a store deleted since the attachment (the
// element stays, decision 7) is neither synced — the directory is left as
// the agent left it — nor landed in a fresh sandbox.
func TestMemoryStoreGoneIsSkipped(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/a.md", "alpha")
	h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
	token := h.sessionsToken(t)
	h.runWith(t, token)
	if _, err := h.pool.Exec(context.Background(), `DELETE FROM memory_stores WHERE id = $1`, memStoreID); err != nil {
		t.Fatal(err)
	}
	sb.files[memMount+"/a.md"] = "edited after the store went"
	before := len(sb.files)
	h.runWith(t, token)
	if sb.files[memMount+"/a.md"] != "edited after the store went" || len(sb.files) != before {
		t.Errorf("a gone store's directory was touched: %v", sb.files)
	}

	fresh := &fakeSandbox{}
	h.prov.sb = fresh
	h.runWith(t, token)
	for p := range fresh.files {
		if strings.HasPrefix(p, "/mnt/memory/") {
			t.Errorf("a gone store landed %s", p)
		}
	}
}

// TestMemoryNoTokenFailsTheItem: a session with a store served by a control
// plane that hands out no sessions token fails the item with the reference
// worker's own error, before any sandbox is provisioned, and the lease loop
// drains it rather than leaving it to reclaim without a token again.
func TestMemoryNoTokenFailsTheItem(t *testing.T) {
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/work/poll") {
				next.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			body := rec.Body.Bytes()
			if rec.Code == http.StatusOK {
				var item map[string]any
				if err := json.Unmarshal(body, &item); err == nil {
					delete(item, "secret")
					body, _ = json.Marshal(item)
				}
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(body)
		})
	}
	sb := &fakeSandbox{}
	h := newHarnessWrapped(t, sb, wrap)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
	h.suspend(t, writeUse("out.txt", "x"))

	err := RunSessionTools(context.Background(), h.client, h.prov, h.sid.String(), ToolExecConfig{})
	if !errors.Is(err, ErrSessionMemoryNoToken) {
		t.Fatalf("RunSessionTools without a token = %v, want ErrSessionMemoryNoToken", err)
	}
	if h.prov.provisions != 0 {
		t.Errorf("a sandbox was provisioned for an item that cannot run")
	}

	h.enqueueWork(t)
	w, done := h.newWorker(Config{})
	cancel, errc := runWorker(w)
	waitDone(t, done)
	waitExit(t, cancel, errc)
	var state string
	if err := h.pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE session_id = $1`, h.sid.String()).Scan(&state); err != nil || state != "stopped" {
		t.Errorf("the item's state = %q (%v), want stopped (drained)", state, err)
	}
	if got := h.results(t); len(got) != 0 {
		t.Errorf("results = %+v, want none: the tools must not run", got)
	}
}

// TestMemoryPushRefusedIsRememberedUntilBytesChange pins the safe-degrade of
// an unknown 400 (not the cap, not an archive — a body this platform's own
// routes never emit, reachable only from a server that words a refusal its
// own way): the file is refused, recorded so it is not retried, and retried
// only once its bytes change.
func TestMemoryPushRefusedIsRememberedUntilBytesChange(t *testing.T) {
	var refuseNext string
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if refuseNext != "" && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories") {
				var body struct{ Path string }
				peek := &bytes.Buffer{}
				_ = json.NewDecoder(io.TeeReader(r.Body, peek)).Decode(&body)
				if body.Path == refuseNext {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"the memory server refused this content"}}`))
					return
				}
				r.Body = io.NopCloser(peek)
			}
			next.ServeHTTP(w, r)
		})
	}
	sb := &fakeSandbox{}
	h := newHarnessWrapped(t, sb, wrap)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/a.md", "alpha")
	h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
	token := h.sessionsToken(t)
	h.runWith(t, token)

	refuseNext = "/new.md"
	sb.files[memMount+"/new.md"] = "v1"
	h.runWith(t, token)
	if _, ok := h.memoryContent(t, memStoreID, "/new.md"); ok {
		t.Error("the refused create reached the store")
	}
	if got := h.baseline(t, memStoreID).Refused["/new.md"]; got != sha256hex([]byte("v1")) {
		t.Errorf("refused digest = %q, want sha(v1) remembered", got)
	}

	// Injection off, bytes unchanged: the refusal is remembered, so nothing
	// is re-sent (a create now would succeed — the proof it was not tried).
	refuseNext = ""
	h.runWith(t, token)
	if _, ok := h.memoryContent(t, memStoreID, "/new.md"); ok {
		t.Error("a remembered refusal was retried without a change")
	}

	// Bytes change: the new digest is not the refused one, so it is retried.
	sb.files[memMount+"/new.md"] = "v2"
	h.runWith(t, token)
	if got, ok := h.memoryContent(t, memStoreID, "/new.md"); !ok || got != "v2" {
		t.Errorf("after the bytes changed: %q, %v; want v2 pushed", got, ok)
	}
	if _, ok := h.baseline(t, memStoreID).Refused["/new.md"]; ok {
		t.Error("the refusal was not cleared after a successful push")
	}
}

// TestMemoryDeleteWithheldWhileArchived pins the DeleteRemote-vs-archive arm:
// a local deletion against an archived store is refused (the store's 400),
// withheld with its baseline kept, and propagates only after the unarchive.
func TestMemoryDeleteWithheldWhileArchived(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/a.md", "alpha")
	h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
	token := h.sessionsToken(t)
	h.runWith(t, token)

	if _, err := h.pool.Exec(context.Background(), `UPDATE memory_stores SET archived_at = now() WHERE id = $1`, memStoreID); err != nil {
		t.Fatal(err)
	}
	delete(sb.files, memMount+"/a.md")
	h.runWith(t, token)
	if _, ok := h.memoryContent(t, memStoreID, "/a.md"); !ok {
		t.Error("a delete reached an archived store")
	}
	if h.baseline(t, memStoreID).Synced["/a.md"] != sha256hex([]byte("alpha")) {
		t.Errorf("baseline while archived = %+v, want the memory kept", h.baseline(t, memStoreID))
	}

	if _, err := h.pool.Exec(context.Background(), `UPDATE memory_stores SET archived_at = NULL WHERE id = $1`, memStoreID); err != nil {
		t.Fatal(err)
	}
	h.runWith(t, token)
	if _, ok := h.memoryContent(t, memStoreID, "/a.md"); ok {
		t.Error("the deletion did not propagate after the unarchive")
	}
}

// TestMemoryMaterializePagesTheListing pins that the store's whole listing is
// paged (the full view caps a page at 20) with the sessions token riding every
// page: a store of 25 memories lands all 25, which page 2 authorized by the
// env key alone could not.
func TestMemoryMaterializePagesTheListing(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedMemoryStore(t, memStoreID, "Notes")
	const n = 25
	for i := 0; i < n; i++ {
		h.seedMemory(t, memStoreID, fmt.Sprintf("/m/%02d.md", i), fmt.Sprintf("body %d", i))
	}
	h.refMemory(t, [3]string{memStoreID, memMount, "read_write"})
	token := h.sessionsToken(t)
	h.runWith(t, token)

	landed := 0
	for i := 0; i < n; i++ {
		if got := sb.files[fmt.Sprintf("%s/m/%02d.md", memMount, i)]; got == fmt.Sprintf("body %d", i) {
			landed++
		}
	}
	if landed != n {
		t.Errorf("landed %d of %d memories; a page beyond the first did not authorize or paginate", landed, n)
	}
	if got := len(h.baseline(t, memStoreID).Synced); got != n {
		t.Errorf("baseline records %d memories, want %d", got, n)
	}
}

func TestSessionsTokenFromSecret(t *testing.T) {
	payload := []byte(`{"sessions_token":"wtk_abc"}`)
	for name, tc := range map[string]struct{ secret, want string }{
		"raw":          {base64.RawURLEncoding.EncodeToString(payload), "wtk_abc"},
		"padded":       {base64.URLEncoding.EncodeToString(payload), "wtk_abc"},
		"empty":        {"", ""},
		"not base64":   {"%%%", ""},
		"not json":     {base64.RawURLEncoding.EncodeToString([]byte("nope")), ""},
		"another key":  {base64.RawURLEncoding.EncodeToString([]byte(`{"other":"x"}`)), ""},
		"the platform": {worktoken.Secret("wtk_xyz"), "wtk_xyz"},
	} {
		if got := sessionsTokenFromSecret(tc.secret); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
