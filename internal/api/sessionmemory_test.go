package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The memory_store arm of sessions.resources (plan 36 slice 3, decisions 7–8;
// #52): the element is the SDK's BetaManagedAgentsMemoryStoreResource verbatim
// — no id, no timestamps — with name, description and mount_path snapshotted
// from the store at attach time, and nothing mounted or said to the agent
// until slice 4.

func memoryStoreWith(t *testing.T, s *tserver, body map[string]any) string {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/memory_stores", body)
	if status != http.StatusOK {
		t.Fatalf("create memory store: status %d (%v)", status, res)
	}
	return res["id"].(string)
}

func memoryElement(storeID string, extra map[string]any) map[string]any {
	el := map[string]any{"type": "memory_store", "memory_store_id": storeID}
	for k, v := range extra {
		el[k] = v
	}
	return el
}

func TestSessionMemoryStoreAttachment(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	store := memoryStoreWith(t, s, map[string]any{"name": "User Preferences", "description": "what the user likes"})
	bare := createMemoryStore(t, s, "Notes")

	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{
			memoryElement(store, map[string]any{"access": "read_only", "instructions": "consult before answering"}),
			memoryElement(bare, nil),
		},
	})
	res := resourcesOf(t, sess)
	if len(res) != 2 {
		t.Fatalf("resources = %v, want two", res)
	}
	// The response variant's exact key set — no id, no timestamps, unlike file
	// and repo elements — on both the fully specified and the bare element.
	for _, el := range res {
		wantExactFields(t, el, "type", "memory_store_id", "access", "description", "instructions", "mount_path", "name")
	}
	if res[0]["type"] != "memory_store" || res[0]["memory_store_id"] != store {
		t.Errorf("element = %v, want the memory_store element for %s", res[0], store)
	}
	if res[0]["access"] != "read_only" || res[0]["instructions"] != "consult before answering" {
		t.Errorf("access/instructions = %v/%v, want the request's", res[0]["access"], res[0]["instructions"])
	}
	if res[0]["name"] != "User Preferences" || res[0]["description"] != "what the user likes" {
		t.Errorf("snapshot = %v/%v, want the store's name and description", res[0]["name"], res[0]["description"])
	}
	if res[0]["mount_path"] != "/mnt/memory/user-preferences" {
		t.Errorf("mount_path = %v, want /mnt/memory/user-preferences", res[0]["mount_path"])
	}
	// Omitted access is the documented default, echoed as the string; omitted
	// instructions is null; a store without a description snapshots "".
	if res[1]["access"] != "read_write" {
		t.Errorf("default access = %v, want read_write", res[1]["access"])
	}
	if v, ok := res[1]["instructions"]; !ok || v != nil {
		t.Errorf("omitted instructions = %v, want null", v)
	}
	if res[1]["description"] != "" || res[1]["mount_path"] != "/mnt/memory/notes" {
		t.Errorf("bare store element = %v", res[1])
	}

	// The session GET echoes the same elements, and the snapshot survives a
	// later rename of the store (documented: "Later edits to the store's name
	// do not propagate").
	sid := sess["id"].(string)
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+store, map[string]any{"name": "Renamed"}); status != http.StatusOK {
		t.Fatalf("rename store: %d (%v)", status, body)
	}
	got := resourcesOf(t, createGetSession(t, s, sid))
	if len(got) != 2 || got[0]["name"] != "User Preferences" || got[0]["mount_path"] != "/mnt/memory/user-preferences" {
		t.Errorf("session GET after rename = %v, want the attach-time snapshot", got)
	}

	// The resources list serves the element too, and neither get nor delete
	// can name it: an id-less element has no {rid}, so both answer the
	// shape-check 404 — for the memory_store_id as much as for a sesrsc_ id.
	status, list := s.do(http.MethodGet, "/v1/sessions/"+sid+"/resources", nil)
	if status != http.StatusOK || len(listData(t, list)) != 2 {
		t.Errorf("resources list: %d %v", status, list)
	}
	for _, rid := range []string{store, "sesrsc_" + strings.Repeat("0", 23) + "1"} {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			status, body := s.do(method, "/v1/sessions/"+sid+"/resources/"+rid, nil)
			wantErr(t, status, body, http.StatusNotFound, "not_found_error")
		}
	}
	// Attach is create-time only: the add endpoint keeps its files-only rule.
	status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/resources", memoryElement(bare, nil))
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// A deleted store's attachment is tolerated: the element stays as
	// snapshotted (the files precedent; the prompt-side hedge is slice 4's).
	if status, body := s.do(http.MethodDelete, "/v1/memory_stores/"+bare, nil); status != http.StatusOK {
		t.Fatalf("delete store: %d (%v)", status, body)
	}
	if got := resourcesOf(t, createGetSession(t, s, sid)); len(got) != 2 || got[1]["memory_store_id"] != bare {
		t.Errorf("session GET after store delete = %v, want the element kept", got)
	}

	// A self_hosted session attaches a store the same way (plan 36 slice 6
	// lifted slice 3's 400): the BYOC worker mounts it from the sessions
	// token its work items carry.
	selfHosted := createEnvironment(t, s, map[string]any{"name": "byoc", "config": map[string]any{"type": "self_hosted"}})["id"].(string)
	byoc := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": selfHosted, "resources": []any{memoryElement(store, nil)},
	})
	if got := resourcesOf(t, byoc); len(got) != 1 || got[0]["type"] != "memory_store" || got[0]["memory_store_id"] != store {
		t.Errorf("self_hosted attachment = %v, want the store's element", got)
	}
}

// The slug (decision 8): lowercased, every non-[a-z0-9] run one hyphen, a
// leading or trailing hyphen trimmed, ASCII only, and a name with no
// alphanumerics falling back to the store id's token.
func TestSessionMemoryStoreMountPathSlug(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	symbols := createMemoryStore(t, s, "!!!")
	for name, want := range map[string]string{
		"(Notes)":      "/mnt/memory/notes",
		"Ünïcode":      "/mnt/memory/n-code",
		"a  b__c":      "/mnt/memory/a-b-c",
		"2026 Q3 plan": "/mnt/memory/2026-q3-plan",
	} {
		store := createMemoryStore(t, s, name)
		sess := createSession(t, s, map[string]any{
			"agent": agentID, "environment_id": envID,
			"resources": []any{memoryElement(store, nil)},
		})
		if got := resourcesOf(t, sess)[0]["mount_path"]; got != want {
			t.Errorf("%q: mount_path = %v, want %s", name, got, want)
		}
	}
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{memoryElement(symbols, nil)},
	})
	if got, want := resourcesOf(t, sess)[0]["mount_path"], "/mnt/memory/"+strings.TrimPrefix(symbols, "memstore_"); got != want {
		t.Errorf("all-symbol name: mount_path = %v, want %s", got, want)
	}
}

// The create-time rejections (decision 7), each a 400 in the standard
// envelope; the cap and the same-store rule are judged before any row is
// read, the store's existence and state inside the create transaction.
func TestSessionMemoryStoreAttachmentRejections(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	store := createMemoryStore(t, s, "Notes")
	twin := createMemoryStore(t, s, "notes") // slugs collide with store's
	archived := createMemoryStore(t, s, "Old")
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+archived+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: %d (%v)", status, body)
	}
	nine := make([]any, 0, 9)
	for i := 0; i < 9; i++ {
		nine = append(nine, memoryElement(createMemoryStore(t, s, "s"+string(rune('a'+i))), nil))
	}

	for name, tc := range map[string]struct {
		envID     string
		resources []any
		want      string
	}{
		"unknown store":   {envID, []any{memoryElement("memstore_"+strings.Repeat("0", 23)+"1", nil)}, "not found"},
		"archived store":  {envID, []any{memoryElement(archived, nil)}, "is archived"},
		"nine stores":     {envID, nine, "at most 8"},
		"the same twice":  {envID, []any{memoryElement(store, nil), memoryElement(store, map[string]any{"access": "read_only"})}, "more than once"},
		"slug collision":  {envID, []any{memoryElement(store, nil), memoryElement(twin, nil)}, "both mount at /mnt/memory/notes"},
		"instructions":    {envID, []any{memoryElement(store, map[string]any{"instructions": strings.Repeat("x", 4097)})}, "4096"},
		"malformed id":    {envID, []any{memoryElement("mem_x", nil)}, "memory_store_id"},
		"missing id":      {envID, []any{map[string]any{"type": "memory_store"}}, "memory_store_id"},
		"bad access":      {envID, []any{memoryElement(store, map[string]any{"access": "append"})}, "access"},
		"output-only key": {envID, []any{memoryElement(store, map[string]any{"mount_path": "/mnt/memory/x"})}, "unknown field"},
	} {
		status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
			"agent": agentID, "environment_id": tc.envID, "resources": tc.resources,
		})
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
		if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, tc.want) {
			t.Errorf("%s: message %q does not mention %q", name, msg, tc.want)
		}
	}

	// Eight stores, instructions of exactly 4,096 characters, and explicit
	// nulls for access and instructions are all accepted.
	eight := nine[:8]
	eight[0] = memoryElement(eight[0].(map[string]any)["memory_store_id"].(string),
		map[string]any{"access": nil, "instructions": nil})
	eight[1] = memoryElement(eight[1].(map[string]any)["memory_store_id"].(string),
		map[string]any{"instructions": strings.Repeat("ü", 4096)})
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID, "resources": eight})
	res := resourcesOf(t, sess)
	if len(res) != 8 || res[0]["access"] != "read_write" || res[0]["instructions"] != nil {
		t.Errorf("eight stores with nulls = %v", res)
	}
}

// The resources-list cursor names the last element by a key every element
// has — a memory element's is memstore:<id> — so a page boundary on or after
// a memory element neither repeats nor skips one.
func TestSessionResourceListPagesAcrossMemoryElements(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	store := createMemoryStore(t, s, "Notes")
	fileA, fileB := uploadOneFile(t, s, "a"), uploadOneFile(t, s, "b")
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{
			map[string]any{"type": "file", "file_id": fileA, "mount_path": "/a"},
			memoryElement(store, nil),
			map[string]any{"type": "file", "file_id": fileB, "mount_path": "/b"},
		},
	})
	sid := sess["id"].(string)

	walk := func(limit string) []string {
		t.Helper()
		var seen []string
		page := ""
		for {
			status, body := s.do(http.MethodGet, "/v1/sessions/"+sid+"/resources?limit="+limit+page, nil)
			if status != http.StatusOK {
				t.Fatalf("list: %d (%v)", status, body)
			}
			for _, el := range listData(t, body) {
				if el["type"] == "memory_store" {
					seen = append(seen, "memstore")
				} else {
					seen = append(seen, el["file_id"].(string))
				}
			}
			next, _ := body["next_page"].(string)
			if next == "" {
				return seen
			}
			page = "&page=" + next
		}
	}
	want := fileA + " memstore " + fileB
	// limit=2 ends page one on the memory element; limit=1 starts page two on it.
	for _, limit := range []string{"1", "2", "3"} {
		if got := strings.Join(walk(limit), " "); got != want {
			t.Errorf("limit=%s walk = %q, want %q", limit, got, want)
		}
	}
}

// GET /v1/sessions?memory_store_id= is a containment match on the resources
// array: a session attaching two stores matches both ids, and a deleted store
// still matches (the element is a snapshot).
func TestSessionListFiltersByMemoryStore(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	x, y := createMemoryStore(t, s, "x"), createMemoryStore(t, s, "y")
	both := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID,
		"resources": []any{memoryElement(x, nil), memoryElement(y, nil)}})["id"]
	onlyX := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID,
		"resources": []any{memoryElement(x, nil)}})["id"]
	createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})

	ids := func(q string) []any {
		t.Helper()
		status, body := s.do(http.MethodGet, "/v1/sessions?memory_store_id="+q, nil)
		if status != http.StatusOK {
			t.Fatalf("filter %s: %d (%v)", q, status, body)
		}
		var out []any
		for _, row := range listData(t, body) {
			out = append(out, row["id"])
		}
		return out
	}
	if got := ids(x); len(got) != 2 || got[0] != onlyX || got[1] != both {
		t.Errorf("filter x = %v, want [%v %v] newest first", got, onlyX, both)
	}
	if got := ids(y); len(got) != 1 || got[0] != both {
		t.Errorf("filter y = %v, want [%v]", got, both)
	}
	if got := ids("memstore_" + strings.Repeat("0", 23) + "1"); len(got) != 0 {
		t.Errorf("filter on an absent store = %v, want empty", got)
	}
	if status, body := s.do(http.MethodDelete, "/v1/memory_stores/"+y, nil); status != http.StatusOK {
		t.Fatalf("delete y: %d (%v)", status, body)
	}
	if got := ids(y); len(got) != 1 || got[0] != both {
		t.Errorf("filter on the deleted store = %v, want [%v] still", got, both)
	}
	// A malformed id can never name a store: rejected on shape (#135).
	for _, bad := range []string{"memstore_X", "mem_" + strings.Repeat("a", 24), "memstore_%00"} {
		status, body := s.do(http.MethodGet, "/v1/sessions?memory_store_id="+bad, nil)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
}

// /mnt/memory is reserved: a repository may not mount at it or below it
// (decision 8); a sibling path stays legal. The path rule runs at parse time,
// before the create touches the cipher or the database.
func TestSessionRepoMountRefusedUnderMemoryRoot(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	for _, mount := range []string{"/mnt/memory", "/mnt/memory/notes"} {
		status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
			"agent": agentID, "environment_id": envID,
			"resources": []any{map[string]any{"type": "github_repository", "url": "https://github.com/x/y",
				"authorization_token": "t", "mount_path": mount}},
		})
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
		if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "reserved") {
			t.Errorf("%s: message %q, want the reservation named", mount, msg)
		}
	}
	// A sibling path is not below the parent, and stays legal.
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "github_repository", "url": "https://github.com/x/y",
			"authorization_token": "t", "mount_path": "/mnt/memoryx"}},
	})
	if got := resourcesOf(t, sess)[0]["mount_path"]; got != "/mnt/memoryx" {
		t.Errorf("/mnt/memoryx: mount_path = %v", got)
	}
}

// TestSessionMemoryAttachLocksTheStoreRow pins the window between reading a
// store's state and inserting the session — the console-key test's twin for
// the environment row (TestConsoleKeyIssueLocksTheEnvironmentRow). An
// uncommitted archive or delete on the store must block the create at its FOR
// SHARE read, so that when the write lands the create answers for the row as it
// now is — the archived store's 400, the deleted store's "not found" 400 —
// rather than attaching a store that is gone. Without the lock the read never
// waits: the poll below times out, which is exactly how that mutant fails.
func TestSessionMemoryAttachLocksTheStoreRow(t *testing.T) {
	for _, tc := range []struct {
		name, sql, want string
	}{
		{"concurrent archive", `UPDATE memory_stores SET archived_at = now() WHERE id = $1`, "is archived"},
		{"concurrent delete", `DELETE FROM memory_stores WHERE id = $1`, "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			ctx := context.Background()
			agentID, envID := fixture(t, s)
			store := createMemoryStore(t, s, "raced")

			tx, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the racing transaction: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, tc.sql, store); err != nil {
				t.Fatalf("racing write: %v", err)
			}

			// Plain net/http in the goroutine: tserver.do fatals on transport
			// errors, and t.Fatalf must not run off the test goroutine.
			type result struct {
				status int
				body   map[string]any
				err    error
			}
			done := make(chan result, 1)
			payload, _ := json.Marshal(map[string]any{"agent": agentID, "environment_id": envID,
				"resources": []any{memoryElement(store, nil)}})
			go func() {
				req, err := http.NewRequest(http.MethodPost, s.url+"/v1/sessions", bytes.NewReader(payload))
				if err != nil {
					done <- result{err: err}
					return
				}
				req.Header.Set("x-api-key", testKey)
				req.Header.Set("content-type", "application/json")
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					done <- result{err: err}
					return
				}
				defer res.Body.Close()
				var body map[string]any
				if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
					done <- result{err: err}
					return
				}
				done <- result{status: res.StatusCode, body: body}
			}()

			// Commit only once the create is observably waiting on the held row
			// lock (the poll's own query never matches: its wait_event_type is
			// null, not Lock).
			waitSQL := `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND query LIKE '%FROM memory_stores%FOR SHARE%')`
			for deadline := time.Now().Add(10 * time.Second); ; {
				select {
				case r := <-done:
					t.Fatalf("the create answered (%d, err %v) before the racing write committed", r.status, r.err)
				default:
				}
				var waiting bool
				if err := s.pool.QueryRow(ctx, waitSQL).Scan(&waiting); err != nil {
					t.Fatalf("poll pg_stat_activity: %v", err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the create never blocked on the memory store row lock")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit the racing write: %v", err)
			}

			got := <-done
			if got.err != nil {
				t.Fatalf("create request: %v", got.err)
			}
			wantErr(t, got.status, got.body, http.StatusBadRequest, "invalid_request_error")
			if msg, _ := got.body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not mention %q", msg, tc.want)
			}
		})
	}
}
