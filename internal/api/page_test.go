package api_test

import (
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
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

// TestListTerminalPageEnvelope pins next_page on a terminal page, route by
// route, because the reference keeps two envelopes (#676): most lists omit the
// key when no page follows, and five send an explicit null. A prev_page with
// nothing before it is omitted as well. Some lists hold a row and some are
// empty; both are terminal pages, and the envelope follows the route, not the
// rows.
func TestListTerminalPageEnvelope(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sessionID := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	_, threads := s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/threads", nil)
	threadID := listData(t, threads)[0]["id"].(string)
	storeID := createMemoryStore(t, s, "envelope-store")
	vaultID := createVault(t, s, "envelope-vault")
	skillID := s.createSkill(t)["id"].(string)
	uploadOneFile(t, s, "envelope")
	workEnvID, _, key := selfHostedWorker(t, s, "ek-envelope")
	enqueueToolExec(t, s, agentID, workEnvID)

	for _, path := range []string{
		"/v1/agents",
		"/v1/agents/" + agentID + "/versions",
		"/v1/sessions",
		"/v1/sessions/" + sessionID + "/events",
		"/v1/sessions/" + sessionID + "/threads",
		"/v1/sessions/" + sessionID + "/threads/" + threadID + "/events",
		"/v1/sessions/" + sessionID + "/resources",
		"/v1/deployments",
		"/v1/deployment_runs",
		"/v1/dreams",
		"/v1/memory_stores",
		"/v1/memory_stores/" + storeID + "/memories",
		"/v1/memory_stores/" + storeID + "/memory_versions",
		"/v1/vaults",
		"/v1/vaults/" + vaultID + "/credentials",
	} {
		t.Run(listLabel(path), func(t *testing.T) {
			status, body := s.do(http.MethodGet, path, nil)
			if status != http.StatusOK {
				t.Fatalf("list: %d %v", status, body)
			}
			wantNoFields(t, body, "next_page", "prev_page")
		})
	}

	wantNullNext := func(t *testing.T, body map[string]any) {
		t.Helper()
		if v, ok := body["next_page"]; !ok || v != nil {
			t.Errorf("next_page = %v (present %v), want an explicit null", v, ok)
		}
		wantNoFields(t, body, "prev_page")
	}
	for _, path := range []string{
		"/v1/files",
		"/v1/skills",
		"/v1/skills/" + skillID + "/versions",
		"/v1/environments",
	} {
		t.Run(listLabel(path), func(t *testing.T) {
			status, body := s.do(http.MethodGet, path, nil)
			if status != http.StatusOK {
				t.Fatalf("list: %d %v", status, body)
			}
			wantNullNext(t, body)
		})
	}
	t.Run("environments_work", func(t *testing.T) {
		res, body, raw := s.workReq(t, http.MethodGet, "/v1/environments/"+workEnvID+"/work", key, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("list: %d %s", res.StatusCode, raw)
		}
		wantNullNext(t, body)
	})
}

// TestSDKPagersEndOnEitherTerminalEnvelope is the compatibility half of #676:
// the typed SDK's pagers read an omitted next_page and a null one alike, as the
// end of the list. Each walk crosses a cursor page into a terminal one — the
// agents list (PageCursor) and the sessions list (BidirectionalPageCursor)
// omitting the key, the environments list sending null.
func TestSDKPagersEndOnEitherTerminalEnvelope(t *testing.T) {
	s := newTestServer(t)
	client := sdk.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL(s.url), option.WithAPIKey(testKey))
	for range 3 {
		agentID, envID := fixture(t, s)
		createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
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
}
