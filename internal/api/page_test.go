package api_test

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// listLabel turns a request path into a subtest name. "/" is testing's own
// subtest separator, so a path used verbatim reports as a nest several levels
// deep with a generated parent id inside it — which -run cannot address and a
// CI failure line cannot be read from.
func listLabel(path string) string {
	return strings.ReplaceAll(strings.TrimPrefix(path, "/v1/"), "/", "_")
}

// foreignKindCursors is dreams_test.go's foreignCursors without its one
// direction arm: a prev-direction time cursor is a foreign thing to a
// unidirectional list but a perfectly good cursor on the sessions list, the one
// list that paginates backwards. Derived rather than listed, so a kind added to
// the grammar later is covered here by having been added there.
var foreignKindCursors = func() map[string]string {
	m := maps.Clone(foreignCursors)
	delete(m, "prev time")
	return m
}()

// TestTimeKeyedListsRejectForeignCursors: a cursor belonging to one of the
// three lists ordered by something other than (created_at, id) is the
// reference's 400 on every list ordered by it, never a 200 page (#534).
//
// The failure it guards is quiet, which is why it is a case per list rather
// than one for the predicate. A foreign cursor decodes to a zero time and an
// empty id — a legal keyset position — so binding it answers 200 with a page
// that looks ordinary. The unidirectional lists compare `<` and report
// end-of-history; the sessions list compares `>` on its ascending arm and
// serves the whole first page as though no cursor had been sent. Two different
// wrong answers from one bug, neither recognisable from outside as a bug.
func TestTimeKeyedListsRejectForeignCursors(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sessionID := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	storeID := createMemoryStore(t, s, "cursor-store")
	vaultID := createVault(t, s, "cursor-vault")
	skillID := s.createSkill(t)["id"].(string)

	// Every unidirectional list whose keyset is (created_at, id), against all
	// four arms — the three foreign kinds and a backwards time cursor, which
	// these lists do not serve either. The nested ones carry a real parent
	// because most of those handlers resolve it before they reach the cursor,
	// so a made-up id would answer 404 and prove nothing about the guard. The
	// threads list is the exception, checking the cursor first; it takes a real
	// session anyway rather than making the table depend on which order a
	// handler happens to use.
	for _, path := range []string{
		"/v1/agents",
		"/v1/environments",
		"/v1/files",
		"/v1/sessions/" + sessionID + "/threads",
		"/v1/deployments",
		"/v1/deployment_runs",
		"/v1/dreams",
		"/v1/memory_stores",
		"/v1/memory_stores/" + storeID + "/memory_versions",
		"/v1/skills",
		"/v1/skills/" + skillID + "/versions",
		"/v1/vaults",
		"/v1/vaults/" + vaultID + "/credentials",
	} {
		for name, cur := range foreignCursors {
			t.Run(listLabel(path)+"_"+name, func(t *testing.T) {
				status, res := s.do(http.MethodGet, path+"?page="+url.QueryEscape(cur), nil)
				wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
			})
		}
	}

	// The sessions list takes ?order= and paginates both ways, so it gets the
	// three kinds on both arms and not the backwards cursor, which it serves.
	// Its ascending arm is the one that answered with a full first page rather
	// than an empty one, so a fix that only ever produced "no rows" would pass
	// everywhere above and fail here.
	for _, order := range []string{"asc", "desc"} {
		for name, cur := range foreignKindCursors {
			t.Run("sessions_order="+order+"_"+name, func(t *testing.T) {
				status, res := s.do(http.MethodGet,
					"/v1/sessions?order="+order+"&page="+url.QueryEscape(cur), nil)
				wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
			})
		}
	}
}

// TestWorkListRejectsForeignCursors is the same guard on the one time-keyed
// list behind the worker wire, which authenticates with an environment key
// rather than the management key and so cannot ride the table above.
func TestWorkListRejectsForeignCursors(t *testing.T) {
	s := newTestServer(t)
	envID, _, key := selfHostedWorker(t, s, "ek-cursor")
	for name, cur := range foreignCursors {
		t.Run(name, func(t *testing.T) {
			res, body, _ := s.workReq(t, http.MethodGet,
				"/v1/environments/"+envID+"/work?page="+url.QueryEscape(cur), key, nil)
			wantErr(t, res.StatusCode, body, http.StatusBadRequest, "invalid_request_error")
		})
	}
}

// listEnvelope is what a list's last page carries besides data (#676).
type listEnvelope int

const (
	omitsNext    listEnvelope = iota // nothing: next_page appears only when a page follows
	nullNext                         // "next_page": null
	filesNext                        // filePageJSON's four keys, next_page null among them
	memoriesNext                     // omitsNext plus the recorded "prefixes": []
)

// listRoutes classifies every list route server.go registers, keyed by the
// pattern it registers. TestListRoutesAreClassified holds the two to each
// other, so a list added later fails until it is classified here.
var listRoutes = map[string]listEnvelope{
	"/v1/agents":                             omitsNext,
	"/v1/agents/{id}/versions":               omitsNext,
	"/v1/environments":                       nullNext,
	"/v1/environments/{id}/work":             nullNext,
	"/v1/deployments":                        omitsNext,
	"/v1/deployment_runs":                    omitsNext,
	"/v1/sessions":                           omitsNext,
	"/v1/sessions/{id}/events":               omitsNext,
	"/v1/sessions/{id}/threads":              omitsNext,
	"/v1/sessions/{id}/threads/{tid}/events": omitsNext,
	"/v1/sessions/{id}/resources":            omitsNext,
	"/v1/vaults":                             omitsNext,
	"/v1/vaults/{id}/credentials":            omitsNext,
	"/v1/skills":                             nullNext,
	"/v1/skills/{id}/versions":               nullNext,
	"/v1/files":                              filesNext,
	"/v1/memory_stores":                      omitsNext,
	"/v1/memory_stores/{id}/memories":        memoriesNext,
	"/v1/memory_stores/{id}/memory_versions": omitsNext,
	"/v1/dreams":                             omitsNext,
}

// TestListRoutesAreClassified reads server.go's registrations rather than a
// running mux, which cannot be enumerated: every `GET /v1/…` served by a
// list* handler must be in listRoutes, and listRoutes must name nothing else.
func TestListRoutesAreClassified(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, m := range regexp.MustCompile(`HandleFunc\("GET (/v1/[^"]+)", s\.handle\([\w.]+, s\.list\w+\)\)`).FindAllSubmatch(src, -1) {
		registered[string(m[1])] = true
	}
	for pattern := range registered {
		if _, ok := listRoutes[pattern]; !ok {
			t.Errorf("list route %s has no terminal envelope in listRoutes", pattern)
		}
	}
	for pattern := range listRoutes {
		if !registered[pattern] {
			t.Errorf("listRoutes names %s, which server.go does not register as a list", pattern)
		}
	}
}

// TestListTerminalPageEnvelope pins next_page route by route, because the
// reference keeps two envelopes (#676): most lists omit the key on their last
// page and five send an explicit null. Every list holds at least two rows, and
// is read whole and then walked one row per page: each page before the last
// must carry a cursor, and the last must be exactly its route's envelope — so
// a cursor dropped mid-walk fails, and so does one minted on a last page that
// happens to be full.
func TestListTerminalPageEnvelope(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	if status, res := s.do(http.MethodPost, "/v1/agents/"+agentID, map[string]any{"version": 1, "name": "v2"}); status != http.StatusOK {
		t.Fatalf("second agent version: %d %v", status, res)
	}
	sessionID := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	sendEvents(t, s, sessionID, userMessage("one"), userMessage("two"))
	insertChild(t, s, sessionID, "idle")
	primaryID := domain.PrimaryThreadID(domain.ID(sessionID)).String()
	fileA, fileB := uploadOneFile(t, s, "a"), uploadOneFile(t, s, "b")
	resourceSession := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID, "resources": []any{
		map[string]any{"type": "file", "file_id": fileA, "mount_path": "/a"},
		map[string]any{"type": "file", "file_id": fileB, "mount_path": "/b"},
	}})["id"].(string)
	deploymentID := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	createDeployment(t, s, deploymentBody(agentID, envID))
	runDeployment(t, s, deploymentID)
	runDeployment(t, s, deploymentID)
	storeID := createMemoryStore(t, s, "envelope-store")
	createMemory(t, s, storeID, "/a.md", "a")
	createMemory(t, s, storeID, "/b.md", "b")
	dreamStore, dreamSessions := createDreamInputs(t, s, 1)
	createDream(t, s, dreamBody(dreamStore, dreamSessions))
	createDream(t, s, dreamBody(dreamStore, dreamSessions))
	vaultID := createVault(t, s, "envelope-vault")
	createVault(t, s, "envelope-vault-2")
	createCredential(t, s, vaultID, envVarAuth("A"))
	createCredential(t, s, vaultID, envVarAuth("B"))
	skillID := s.createSkill(t)["id"].(string)
	s.createSkill(t)
	ct, form := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
	if status, res := s.doForm(http.MethodPost, "/v1/skills/"+skillID+"/versions", ct, form); status != http.StatusOK {
		t.Fatalf("second skill version: %d %v", status, res)
	}
	workEnvID, _, key := selfHostedWorker(t, s, "ek-envelope")
	enqueueToolExec(t, s, agentID, workEnvID)
	enqueueToolExec(t, s, agentID, workEnvID)

	paths := map[string]string{
		"/v1/agents":                             "/v1/agents",
		"/v1/agents/{id}/versions":               "/v1/agents/" + agentID + "/versions",
		"/v1/environments":                       "/v1/environments",
		"/v1/environments/{id}/work":             "/v1/environments/" + workEnvID + "/work",
		"/v1/deployments":                        "/v1/deployments",
		"/v1/deployment_runs":                    "/v1/deployment_runs",
		"/v1/sessions":                           "/v1/sessions",
		"/v1/sessions/{id}/events":               "/v1/sessions/" + sessionID + "/events",
		"/v1/sessions/{id}/threads":              "/v1/sessions/" + sessionID + "/threads",
		"/v1/sessions/{id}/threads/{tid}/events": "/v1/sessions/" + sessionID + "/threads/" + primaryID + "/events",
		"/v1/sessions/{id}/resources":            "/v1/sessions/" + resourceSession + "/resources",
		"/v1/vaults":                             "/v1/vaults",
		"/v1/vaults/{id}/credentials":            "/v1/vaults/" + vaultID + "/credentials",
		"/v1/skills":                             "/v1/skills",
		"/v1/skills/{id}/versions":               "/v1/skills/" + skillID + "/versions",
		"/v1/files":                              "/v1/files",
		"/v1/memory_stores":                      "/v1/memory_stores",
		"/v1/memory_stores/{id}/memories":        "/v1/memory_stores/" + storeID + "/memories",
		"/v1/memory_stores/{id}/memory_versions": "/v1/memory_stores/" + storeID + "/memory_versions",
		"/v1/dreams":                             "/v1/dreams",
	}
	get := func(t *testing.T, pattern, path string) map[string]any {
		t.Helper()
		if pattern == "/v1/environments/{id}/work" {
			res, body, raw := s.workReq(t, http.MethodGet, path, key, nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: %d %s", path, res.StatusCode, raw)
			}
			return body
		}
		status, body := s.do(http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s: %d %v", path, status, body)
		}
		return body
	}
	// lastPage holds a page to its route's envelope; a sessions page reached
	// through a cursor also carries the prev_page leading back.
	lastPage := func(t *testing.T, env listEnvelope, body map[string]any, prev bool) {
		t.Helper()
		switch env {
		case omitsNext:
			keys := []string{"data"}
			if prev {
				keys = append(keys, "prev_page")
			}
			wantExactKeys(t, body, keys...)
			nextPage(t, body)
		case memoriesNext:
			wantExactKeys(t, body, "data", "prefixes")
			nextPage(t, body)
			wantEmptyPrefixes(t, body)
		case nullNext:
			wantExactKeys(t, body, "data", "next_page")
		case filesNext:
			wantExactKeys(t, body, "data", "next_page", "has_more", "first_id", "last_id")
		}
		if env == nullNext || env == filesNext {
			if c := nextPageOrNull(t, body); c != "" {
				t.Errorf("last page next_page = %q, want null", c)
			}
		}
	}

	for pattern, env := range listRoutes {
		t.Run(listLabel(pattern), func(t *testing.T) {
			path, ok := paths[pattern]
			if !ok {
				t.Fatalf("no fixture path for %s", pattern)
			}
			whole := get(t, pattern, path)
			n := len(listData(t, whole))
			if n < 2 || n >= 20 {
				t.Fatalf("the fixture holds %d rows, want 2 to 19 so the default page is the last", n)
			}
			lastPage(t, env, whole, false)
			query := path + "?limit=1"
			for i := range n {
				body := get(t, pattern, query)
				if got := len(listData(t, body)); got != 1 {
					t.Fatalf("page %d of %d holds %d rows, want 1: %v", i+1, n, got, body)
				}
				if i == n-1 {
					lastPage(t, env, body, pattern == "/v1/sessions" && i > 0)
					break
				}
				if env == memoriesNext {
					wantExactKeys(t, body, "data", "next_page", "prefixes")
					wantEmptyPrefixes(t, body)
				}
				query = path + "?limit=1&page=" + url.QueryEscape(wantCursor(t, body))
			}
		})
	}
}

// TestSDKPagersEndOnEitherTerminalEnvelope is the compatibility half of #676:
// the typed SDK's pagers read an omitted next_page and a null one alike, as the
// end of the list. Each walk crosses a cursor page into a terminal one — the
// agents list (PageCursor) and the sessions list (BidirectionalPageCursor)
// omitting the key, the environments list sending null, and the memories list
// omitting it beside the recorded "prefixes": [], which the SDK does not model
// and so files under JSON.ExtraFields.
func TestSDKPagersEndOnEitherTerminalEnvelope(t *testing.T) {
	s := newTestServer(t)
	client := sdk.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL(s.url), option.WithAPIKey(testKey))
	storeID := createMemoryStore(t, s, "sdk-pager")
	for i := range 3 {
		agentID, envID := fixture(t, s)
		createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
		createMemory(t, s, storeID, fmt.Sprintf("/m%d.md", i), "x")
	}
	walk := func(name string, pager interface {
		Next() bool
		Err() error
	}) {
		t.Helper()
		n := 0
		for pager.Next() {
			if n++; n > 3 {
				t.Fatalf("%s: the walk read past the last row", name)
			}
		}
		if err := pager.Err(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n != 3 {
			t.Errorf("%s: walked %d rows, want 3", name, n)
		}
	}
	ctx := t.Context()
	walk("agents", client.Beta.Agents.ListAutoPaging(ctx, sdk.BetaAgentListParams{Limit: sdk.Int(2)}))
	walk("sessions", client.Beta.Sessions.ListAutoPaging(ctx, sdk.BetaSessionListParams{Limit: sdk.Int(2)}))
	walk("environments", client.Beta.Environments.ListAutoPaging(ctx, sdk.BetaEnvironmentListParams{Limit: sdk.Int(2)}))
	walk("memories", client.Beta.MemoryStores.Memories.ListAutoPaging(ctx, storeID, sdk.BetaMemoryStoreMemoryListParams{Limit: sdk.Int(2)}))
	page, err := client.Beta.MemoryStores.Memories.List(ctx, storeID, sdk.BetaMemoryStoreMemoryListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := page.JSON.ExtraFields["prefixes"]; !ok || f.Raw() != "[]" {
		t.Errorf("typed memories page ExtraFields[prefixes] = %q (present %v), want the recorded []", f.Raw(), ok)
	}
}
