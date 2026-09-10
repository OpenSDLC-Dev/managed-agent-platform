package api_test

import (
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
