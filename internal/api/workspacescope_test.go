package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// grantFixture records the workspace access required by a fixture independently
// of the subject's organization role.
func grantFixture(t *testing.T, pool *pgxpool.Pool, id, role string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO workspace_memberships VALUES ('default',$1,$2) ON CONFLICT DO NOTHING`, id, role); err != nil {
		t.Fatal(err)
	}
}

// The second key belongs to the same org admin. Its binding alone must isolate
// resources even though the operator can use both workspaces.
func secondWorkspace(t *testing.T, first *tserver) *tserver {
	t.Helper()
	const id = "research"
	const publicID = "wrkspc_00000000000000000000000002"
	if _, err := first.pool.Exec(t.Context(), `INSERT INTO workspaces(id,public_id,name) VALUES($1,$2,'Research')`, id, publicID); err != nil {
		t.Fatal(err)
	}
	second := *first
	second.key = "test-research-workspace-key"
	if _, err := first.pool.Exec(t.Context(), `INSERT INTO api_keys(id,name,owned_key_hash,owner_principal_id,created_by,workspace_id)
		SELECT $1,'Research key',$2,owner_principal_id,created_by,$3 FROM api_keys WHERE owned_key_hash=$4`,
		domain.NewID(domain.PrefixAPIKey).String(), apikey.Hash(second.key), id, apikey.Hash(first.managementKey())); err != nil {
		t.Fatal(err)
	}
	return &second
}

func TestWorkspaceResourceIsolation(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	for _, family := range []struct {
		path string
		body any
	}{
		{"/v1/agents", agentBody(nil)},
		{"/v1/environments", map[string]any{"name": "shared-name"}},
	} {
		t.Run(family.path, func(t *testing.T) {
			var ids []string
			for _, s := range []*tserver{a, b} {
				status, row := s.do(http.MethodPost, family.path, family.body)
				if status != http.StatusOK {
					t.Fatalf("create %s: %d %v", family.path, status, row)
				}
				ids = append(ids, row["id"].(string))
			}
			for i, s := range []*tserver{a, b} {
				status, page := s.do(http.MethodGet, family.path, nil)
				rows := listData(t, page)
				if status != http.StatusOK || len(rows) != 1 || rows[0]["id"] != ids[i] {
					t.Fatalf("list %s: %d %v", family.path, status, page)
				}
				status, row := s.do(http.MethodGet, family.path+"/"+ids[1-i], nil)
				if status != http.StatusNotFound {
					t.Fatalf("foreign %s: %d %v", family.path, status, row)
				}
			}
		})
	}
}

func TestWorkspaceSessionExecution(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	aa, ae := readableFixture(t, a)
	ba, be := readableFixture(t, b)
	for _, tc := range []struct {
		name, agent, env string
	}{
		{"foreign environment", ba, ae},
		{"foreign agent", aa, be},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, res := b.do("POST", "/v1/sessions", map[string]any{"agent": tc.agent, "environment_id": tc.env})
			if status != http.StatusNotFound {
				t.Fatalf("create: %d %v", status, res)
			}
		})
	}
	file := a.uploadFile(t, "input.txt", nil, "workspace A")["id"].(string)
	status, body := b.do("POST", "/v1/sessions", map[string]any{
		"agent": ba, "environment_id": be,
		"resources": []any{map[string]any{"type": "file", "file_id": file}},
	})
	if status != http.StatusNotFound {
		t.Fatalf("foreign file mount: %d %v", status, body)
	}
	sid := createSession(t, b, map[string]any{"agent": ba, "environment_id": be,
		"initial_events": []any{map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": "Hello"}}}},
	})["id"].(string)
	for _, suffix := range []string{"", "/events", "/threads", "/resources"} {
		status, body := a.do("GET", "/v1/sessions/"+sid+suffix, nil)
		if status != http.StatusNotFound {
			t.Errorf("foreign session %s: %d %v", suffix, status, body)
		}
	}
	for _, relation := range []string{"session_threads", "events", "work_items"} {
		var count, wrong int
		err := b.pool.QueryRow(t.Context(), "SELECT count(*), count(*) FILTER (WHERE workspace_id <> 'research') FROM "+relation+" WHERE session_id=$1", sid).Scan(&count, &wrong)
		if err != nil || count == 0 || wrong != 0 {
			t.Errorf("%s: %d rows, %d outside owning workspace: %v", relation, count, wrong, err)
		}
	}
}

func TestWorkspaceDatabaseRejectsCrossReferences(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	agent, envA := fixture(t, a)
	_, envB := fixture(t, b)
	sid := createSession(t, a, map[string]any{"agent": agent, "environment_id": envA})["id"].(string)
	for _, tc := range []struct {
		name, query string
		args        []any
	}{
		{"other workspace environment", "UPDATE sessions SET environment_id=$1 WHERE id=$2", []any{envB, sid}},
		{"unknown workspace", "UPDATE environments SET workspace_id='missing' WHERE id=$1", []any{envB}},
		{"unsupported project", "UPDATE sessions SET project_id='other' WHERE id=$1", []any{sid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := a.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), tc.query, tc.args...); err == nil {
				t.Fatal("invalid scope was accepted")
			}
		})
	}
}

func TestWorkspacePrivateResourceIDs(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	store := createMemoryStore(t, b, "Shared")
	mem := createMemory(t, b, store, "/notes", "private fixture")
	skill := b.createSkill(t)
	vault := createVault(t, b, "Shared")
	cred := createCredential(t, b, vault, envVarAuth("TOKEN"))
	file := b.uploadFile(t, "shared.txt", nil, "private")
	// Downloadable outputs use the same blob lookup as this seeded fixture file.
	if _, err := b.pool.Exec(t.Context(), `UPDATE files SET downloadable=true WHERE id=$1`, file["id"]); err != nil {
		t.Fatal(err)
	}
	agent, env := fixture(t, b)
	depl := createDeployment(t, b, deploymentBody(agent, env))
	for _, r := range []struct{ prefix, id, suffix string }{
		{"/v1/memory_stores/", store, ""},
		{"/v1/memory_stores/" + store + "/memories/", mem["id"].(string), ""},
		{"/v1/memory_stores/" + store + "/memory_versions/", mem["memory_version_id"].(string), ""},
		{"/v1/vaults/", vault, ""},
		{"/v1/vaults/" + vault + "/credentials/", cred["id"].(string), ""},
		{"/v1/files/", file["id"].(string), ""},
		{"/v1/files/", file["id"].(string), "/content"},
		{"/v1/deployments/", depl["id"].(string), ""},
		{"/v1/skills/", skill["id"].(string), ""},
		{"/v1/skills/", skill["id"].(string), "/versions"},
		{"/v1/skills/", skill["id"].(string), "/versions/" + skill["latest_version_id"].(string) + "/content"},
	} {
		t.Run(r.prefix+r.suffix, func(t *testing.T) {
			own := b.doRaw("GET", r.prefix+r.id+r.suffix, nil, map[string]string{"x-api-key": b.managementKey()})
			own.Body.Close()
			if own.StatusCode != http.StatusOK {
				t.Fatalf("owner got %d for %s", own.StatusCode, r.prefix+r.id+r.suffix)
			}
			missing := domain.NewID(strings.SplitN(r.id, "_", 2)[0]).String()
			var foreignError string
			for _, id := range []string{r.id, missing} {
				status, row := a.do("GET", r.prefix+id+r.suffix, nil)
				if status != http.StatusNotFound {
					t.Fatalf("lookup %s: %d %v", id, status, row)
				}
				delete(row, "request_id")
				wire, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				normalized := strings.ReplaceAll(string(wire), id, "<id>")
				if id == r.id {
					foreignError = normalized
				} else if normalized != foreignError {
					t.Fatalf("refusals differ: %s and %s", foreignError, normalized)
				}
				msg, _ := row["error"].(map[string]any)
				if msg["type"] != "not_found_error" {
					t.Fatalf("unexpected refusal for %s: %v", id, msg)
				}
			}
		})
	}
	for _, tests := range []struct {
		name, path string
		body       map[string]any
	}{
		{"agent skill", "/v1/agents", map[string]any{"name": "shared", "model": "claude-opus-4-8", "skills": []any{map[string]any{"type": "custom", "skill_id": skill["id"]}}}},
		{"deployment", "/v1/deployments", deploymentBody(agent, env)},
	} {
		t.Run(tests.name, func(t *testing.T) {
			status, row := a.do("POST", tests.path, tests.body)
			if status != http.StatusNotFound && status != http.StatusBadRequest {
				t.Fatalf("accepted foreign reference: %d %v", status, row)
			}
		})
	}
}

func TestWorkspaceConcurrentResourceWrite(t *testing.T) {
	for _, action := range []string{"remove grant", "archive workspace"} {
		t.Run(action, func(t *testing.T) {
			a := newTestServer(t)
			b := secondWorkspace(t, a)
			owner := ownedPrincipalFixture(t, b.pool, "user", "developer")
			if _, err := b.pool.Exec(t.Context(), `INSERT INTO workspace_memberships VALUES ('research',$1,'developer')`, owner); err != nil {
				t.Fatal(err)
			}
			b.key = "research-developer-test-key"
			if _, err := b.pool.Exec(t.Context(), `INSERT INTO api_keys(id,name,workspace_id,owner_principal_id,created_by,owned_key_hash) VALUES ($1,'dev','research',$2,$2,$3)`, domain.NewID(domain.PrefixAPIKey).String(), owner, apikey.Hash(b.key)); err != nil {
				t.Fatal(err)
			}
			tx, err := b.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			switch action {
			case "remove grant":
				_, err = tx.Exec(t.Context(), `DELETE FROM workspace_memberships WHERE workspace_id='research' AND principal_id=$1`, owner)
			default:
				_, err = tx.Exec(t.Context(), `UPDATE workspaces SET archived_at=now() WHERE id='research'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan int, 1)
			go func() { status, _ := b.do("POST", "/v1/agents", agentBody(nil)); done <- status }()
			wait, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ticks := time.NewTicker(10 * time.Millisecond)
			defer ticks.Stop()
			for {
				var waiting bool
				if err = b.pool.QueryRow(wait, `SELECT EXISTS(SELECT FROM pg_stat_activity WHERE datname=current_database() AND $1::int = ANY(pg_blocking_pids(pid)))`, int(tx.Conn().PgConn().PID())).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case <-wait.Done():
					t.Fatal("request did not reach the workspace write fence")
				case <-ticks.C:
				}
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got != http.StatusForbidden {
					t.Fatalf("write after %s: HTTP %d", action, got)
				}
			case <-wait.Done():
				t.Fatal("write did not settle after the workspace grant change")
			}
			var n int
			if err := b.pool.QueryRow(t.Context(), `SELECT count(*) FROM agents WHERE workspace_id='research'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("unauthorized writes: %d, %v", n, err)
			}
		})
	}
}

func TestWorkspaceBackgroundExecution(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	for _, tc := range []struct {
		server, other *tserver
		workspace     string
	}{{a, b, "default"}, {b, a, "research"}} {
		t.Run(tc.workspace, func(t *testing.T) {
			s := tc.server
			store, inputs := createDreamInputs(t, s, 1)
			dream, pipeline := startedDream(t, s, dreamBody(store, inputs))
			var dreamScope, agentScope, envScope, fileScope, cloneScope string
			if err := s.pool.QueryRow(t.Context(), `SELECT d.workspace_id,a.workspace_id,e.workspace_id,f.workspace_id,m.workspace_id
    FROM dreams d JOIN sessions s ON s.id=d.session_id JOIN agents a ON a.id=s.agent_id
    JOIN environments e ON e.id=s.environment_id JOIN files f ON f.dream_id=d.id
    JOIN memory_stores m ON m.id=d.outputs->0->>'memory_store_id' WHERE d.id=$1 AND s.id=$2 AND a.internal AND e.internal`, dream, pipeline).Scan(&dreamScope, &agentScope, &envScope, &fileScope, &cloneScope); err != nil {
				t.Fatal(err)
			}
			for _, scope := range []string{dreamScope, agentScope, envScope, fileScope, cloneScope} {
				if scope != tc.workspace {
					t.Fatalf("dream artifact stored in %q", scope)
				}
			}
			if st, _ := tc.other.do("GET", "/v1/dreams/"+dream, nil); st != 404 {
				t.Fatalf("foreign dream: %d", st)
			}
			agent, env := fixture(t, s)
			depl := createDeployment(t, s, scheduledBody(agent, env, "* * * * *", "UTC"))["id"].(string)
			makeDue(t, s, depl)
			if err := api.SchedulerTick(t.Context(), s.pool, dbNow(t, s)); err != nil {
				t.Fatal(err)
			}
			runs := scheduledRuns(t, s, depl)
			if len(runs) != 1 || runs[0].sessionID == nil {
				t.Fatalf("scheduled runs: %+v", runs)
			}
			if st, _ := s.do("GET", "/v1/sessions/"+*runs[0].sessionID, nil); st != 200 {
				t.Fatalf("own scheduled session: %d", st)
			}
			if st, _ := tc.other.do("GET", "/v1/sessions/"+*runs[0].sessionID, nil); st != 404 {
				t.Fatalf("foreign scheduled session: %d", st)
			}
		})
	}
}

func TestWorkspaceWorkerCredentials(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	_, ea, sa, ma, ka := storeWorker(t, a, "a")
	_, eb, sb, mb, kb := storeWorker(t, b, "b")
	for _, tc := range []struct{ env, key, ownSession, ownStore, foreignSession, foreignStore string }{
		{ea, ka, sa, ma, sb, mb}, {eb, kb, sb, mb, sa, ma},
	} {
		_, _, tok := pollItem(t, a, tc.env, tc.key)
		if tok == "" {
			t.Fatal("missing sessions token")
		}
		for _, cred := range []string{tc.key, tok} {
			for _, check := range []struct {
				id   string
				want int
			}{{tc.ownSession, 200}, {tc.foreignSession, 404}} {
				if got := status(t, a, "GET", "/v1/sessions/"+check.id, nil, asBearer(cred)); got != check.want {
					t.Fatalf("session lookup: got %d want %d", got, check.want)
				}
			}
		}
		for _, check := range []struct {
			id   string
			want int
		}{{tc.ownStore, 200}, {tc.foreignStore, 404}} {
			if got := status(t, a, "GET", "/v1/memory_stores/"+check.id+"/memories", nil, asBearer(tok)); got != check.want {
				t.Fatalf("worker memory lookup: got %d want %d", got, check.want)
			}
		}
	}
}

func TestWorkspaceRubricReferences(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	agent, env := fixture(t, a)
	sid := createSession(t, a, map[string]any{"agent": agent, "environment_id": env})["id"].(string)
	dep := createDeployment(t, a, deploymentBody(agent, env))["id"].(string)
	own := a.uploadFile(t, "rubric.txt", nil, "private A")["id"].(string)
	foreign := b.uploadFile(t, "rubric.txt", nil, "private B")["id"].(string)
	missing := domain.NewID(domain.PrefixFile).String()
	for _, tc := range []struct {
		path, field string
		body        map[string]any
	}{
		{"/v1/sessions", "initial_events", map[string]any{"agent": agent, "environment_id": env}},
		{"/v1/sessions/" + sid + "/events", "events", map[string]any{}},
		{"/v1/deployments", "initial_events", deploymentBody(agent, env)},
		{"/v1/deployments/" + dep, "initial_events", map[string]any{}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			var missingMessage string
			for _, id := range []string{missing, foreign, own} {
				tc.body[tc.field] = []any{defineOutcome("Grade the result", map[string]any{"rubric": map[string]any{"type": "file", "file_id": id}})}
				st, row := a.do("POST", tc.path, tc.body)
				if id == own {
					if st != 200 {
						t.Fatalf("own rubric: HTTP %d, %v", st, row)
					}
					continue
				}
				if st != 400 {
					t.Fatalf("rubric lookup: HTTP %d, %v", st, row)
				}
				msg := row["error"].(map[string]any)
				if msg["type"] != "invalid_request_error" {
					t.Fatalf("unexpected error %v", msg)
				}
				m := strings.ReplaceAll(msg["message"].(string), id, "<file>")
				if id == missing {
					missingMessage = m
				} else if missingMessage != m {
					t.Fatalf("foreign and absent errors differ: %q, %q", m, missingMessage)
				}
			}
		})
	}
}

func TestWorkspaceDeploymentMountReferences(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	agent, env := fixture(t, a)
	dep := createDeployment(t, a, deploymentBody(agent, env))["id"].(string)
	for _, resource := range []struct{ kind, field, own, foreign, missing string }{
		{"file", "file_id", a.uploadFile(t, "source", nil, "A")["id"].(string), b.uploadFile(t, "source", nil, "B")["id"].(string), domain.NewID(domain.PrefixFile).String()},
		{"memory_store", "memory_store_id", createMemoryStore(t, a, "A"), createMemoryStore(t, b, "B"), domain.NewID(domain.PrefixMemoryStore).String()},
	} {
		for _, path := range []string{"/v1/deployments", "/v1/deployments/" + dep} {
			t.Run(resource.kind+path, func(t *testing.T) {
				body := map[string]any{}
				if path == "/v1/deployments" {
					body = deploymentBody(agent, env)
				}
				var missingMessage string
				for _, id := range []string{resource.missing, resource.foreign, resource.own} {
					body["resources"] = []any{map[string]any{"type": resource.kind, resource.field: id}}
					st, row := a.do("POST", path, body)
					if id == resource.own {
						if st != 200 {
							t.Fatalf("own reference: %d %v", st, row)
						}
						continue
					}
					if st != 404 {
						t.Fatalf("out-of-scope reference: %d %v", st, row)
					}
					message := row["error"].(map[string]any)["message"].(string)
					normalized := strings.ReplaceAll(message, id, "<id>")
					if id == resource.missing {
						missingMessage = normalized
					} else if normalized != missingMessage {
						t.Fatalf("foreign and absent errors differ: %q, %q", missingMessage, normalized)
					}
				}
			})
		}
	}
}

func TestWorkspaceMessageFiles(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	agent, env := fixture(t, a)
	sid := createSession(t, a, map[string]any{"agent": agent, "environment_id": env})["id"].(string)
	own := a.uploadFile(t, "picture.png", nil, "fixture A")["id"].(string)
	foreign := b.uploadFile(t, "picture.png", nil, "fixture B")["id"].(string)
	missing := domain.NewID(domain.PrefixFile).String()
	for _, tc := range []struct {
		path, field string
		body        map[string]any
	}{
		{"/v1/sessions", "initial_events", map[string]any{"agent": agent, "environment_id": env}},
		{"/v1/sessions/" + sid + "/events", "events", map[string]any{}},
		{"/v1/deployments", "initial_events", deploymentBody(agent, env)},
	} {
		t.Run(tc.path, func(t *testing.T) {
			for _, kind := range []string{"image", "document"} {
				for _, id := range []string{missing, foreign, own} {
					tc.body[tc.field] = []any{map[string]any{"type": "user.message", "content": []any{map[string]any{"type": kind, "source": map[string]any{"type": "file", "file_id": id}}}}}
					st, row := a.do("POST", tc.path, tc.body)
					if id == own {
						if st != 200 {
							t.Fatalf("own %s: %d %v", kind, st, row)
						}
						continue
					}
					if st != 404 || !strings.Contains(row["error"].(map[string]any)["message"].(string), id) {
						t.Fatalf("absent %s: %d %v", kind, st, row)
					}
				}
			}
		})
	}
}
