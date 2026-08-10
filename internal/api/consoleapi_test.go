package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// The console-API paths, spelled out rather than imported from the handler. This
// path is the contract the console's BFF proxies to, mirrored segment-for-segment
// from the reference console's private API; a test that reused the
// implementation's own constant could not notice it drift.
func consoleTokens(envID string) string {
	return "/api/oauth/organizations/default/environments/" + envID + "/tokens"
}

func consoleRevoke(envID, keyID string) string {
	return consoleTokens(envID) + "/" + keyID + "/revoke"
}

// issueViaConsole issues one key over real HTTP and returns the plaintext.
func issueViaConsole(t *testing.T, s *tserver, envID, name string) string {
	t.Helper()
	status, body := s.do(http.MethodPost, consoleTokens(envID), map[string]any{"name": name})
	if status != http.StatusOK {
		t.Fatalf("issue %q: status %d, body %v", name, status, body)
	}
	tok, _ := body["access_token"].(string)
	if tok == "" {
		t.Fatalf("issue %q returned no access_token: %v", name, body)
	}
	return tok
}

// consoleKeyIDs lists an environment's keys over HTTP and returns their ids in
// listing order, plus the pagination block.
func consoleKeyIDs(t *testing.T, s *tserver, envID, query string) ([]string, map[string]any) {
	t.Helper()
	status, body := s.do(http.MethodGet, consoleTokens(envID)+query, nil)
	if status != http.StatusOK {
		t.Fatalf("list keys%s: status %d, body %v", query, status, body)
	}
	page, _ := body["pagination"].(map[string]any)
	if page == nil {
		t.Fatalf("listing carries no pagination block: %v", body)
	}
	ids := make([]string, 0)
	for _, row := range listData(t, body) {
		id, _ := row["id"].(string)
		ids = append(ids, id)
	}
	return ids, page
}

// TestConsoleIssuedKeyDrivesAWorkerAndIsRevocable is the issue's whole point in
// one pass: an operator with only the console can mint a worker credential, the
// worker authenticates the real /v1 work API with it, and revoking it through the
// console takes that worker — and only that worker — off the queue. Nothing here
// touches the database except to prove the plaintext was never stored.
func TestConsoleIssuedKeyDrivesAWorkerAndIsRevocable(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	envID := selfHostedEnv(t, s, "console-e2e")

	status, body := s.do(http.MethodPost, consoleTokens(envID), map[string]any{"name": "build-host-1"})
	if status != http.StatusOK {
		t.Fatalf("issue: status %d, body %v", status, body)
	}
	wantFields(t, body, "access_token", "expires_in")
	token, _ := body["access_token"].(string)
	if !strings.HasPrefix(token, "sk-map-env01-") {
		t.Errorf("access_token = %q, want the sk-map-env01- prefix", token)
	}
	// One year, in seconds — the reference's own expires_in, matched exactly so a
	// console rendering "expires in a year" is telling the truth.
	if n, _ := body["expires_in"].(float64); int(n) != 31536000 {
		t.Errorf("expires_in = %v, want 31536000", body["expires_in"])
	}

	// The plaintext exists only in that response. What the database holds is a
	// hash, and a stolen dump cannot be replayed against the work API.
	var stored string
	if err := s.pool.QueryRow(ctx,
		`SELECT key_hash FROM environment_keys WHERE environment_id = $1`, envID).Scan(&stored); err != nil {
		t.Fatalf("read the stored row: %v", err)
	}
	if stored == token || strings.Contains(stored, token) {
		t.Fatal("the issued secret was stored verbatim")
	}
	if len(stored) != 64 {
		t.Errorf("key_hash = %q (%d chars), want a 64-char SHA-256 digest", stored, len(stored))
	}

	// The credential works on the real wire surface, which is the only thing an
	// operator actually wants from it.
	auth := map[string]string{"Authorization": "Bearer " + token}
	if res, raw := s.poll(t, envID, auth); res.StatusCode != http.StatusOK {
		t.Fatalf("a console-issued key cannot poll the work queue: status %d, body %q", res.StatusCode, raw)
	}

	// It is listed with the name it was issued under and a real expiry.
	listStatus, listBody := s.do(http.MethodGet, consoleTokens(envID), nil)
	if listStatus != http.StatusOK {
		t.Fatalf("list: status %d, body %v", listStatus, listBody)
	}
	rows := listData(t, listBody)
	if len(rows) != 1 {
		t.Fatalf("listed %d keys, want 1", len(rows))
	}
	row := rows[0]
	page, _ := listBody["pagination"].(map[string]any)
	keyID, _ := row["id"].(string)
	wantFields(t, row, "id", "name", "created_at", "expires_at")
	if row["name"] != "build-host-1" {
		t.Errorf("name = %v, want build-host-1", row["name"])
	}
	if row["expires_at"] == nil {
		t.Error("expires_at is null on a freshly issued key")
	}
	if !strings.HasPrefix(keyID, "envkey_") {
		t.Errorf("id = %q, want the envkey_ prefix", keyID)
	}
	if page["total"] != float64(1) || page["has_more"] != false {
		t.Errorf("pagination = %v, want total 1 and has_more false", page)
	}

	// Revoking is a bodiless 204, and the worker is off the queue immediately.
	res := s.doRaw(http.MethodPost, consoleRevoke(envID, keyID), nil,
		map[string]string{"x-api-key": testKey})
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status %d, body %q", res.StatusCode, raw)
	}
	if len(raw) != 0 {
		t.Errorf("204 carried a body: %q", raw)
	}
	pollRes, pollRaw := s.poll(t, envID, auth)
	var pollBody map[string]any
	_ = json.Unmarshal([]byte(pollRaw), &pollBody)
	wantErr(t, pollRes.StatusCode, pollBody, http.StatusUnauthorized, "authentication_error")

	// A retried revocation is a success, not an error — an operator who did not
	// see the first 204 must be able to press the button again.
	if again, _ := s.do(http.MethodPost, consoleRevoke(envID, keyID), nil); again != http.StatusNoContent {
		t.Errorf("repeat revoke: status %d, want 204", again)
	}
	if ids, page := consoleKeyIDs(t, s, envID, ""); len(ids) != 0 || page["total"] != float64(0) {
		t.Errorf("a revoked key is still listed: ids %v, pagination %v", ids, page)
	}
}

// TestConsoleKeyRoutesRequireManagementAuth pins the namespace's auth lane. It
// takes the management x-api-key and nothing else — in particular an environment
// key, the very credential these routes mint, cannot mint or revoke another. That
// falls out of dispatchAuth's default lane rather than an explicit /api/ arm, so
// it is exactly the kind of property that would rot silently without a test.
func TestConsoleKeyRoutesRequireManagementAuth(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "auth")
	envKey := issueKey(t, s.pool, envID, "a-worker")
	keyID := onlyKeyID(t, s, envID)

	routes := map[string]struct {
		method, path string
		body         any
	}{
		"issue":  {http.MethodPost, consoleTokens(envID), map[string]any{"name": "x"}},
		"list":   {http.MethodGet, consoleTokens(envID), nil},
		"revoke": {http.MethodPost, consoleRevoke(envID, keyID), nil},
	}
	headers := map[string]map[string]string{
		"no credential":     {},
		"environment key":   {"Authorization": "Bearer " + envKey},
		"wrong api key":     {"x-api-key": "map-not-the-key"},
		"api key as bearer": {"Authorization": "Bearer " + testKey},
	}
	for rname, route := range routes {
		for hname, h := range headers {
			t.Run(rname+"/"+hname, func(t *testing.T) {
				res := s.doRaw(route.method, route.path, route.body, h)
				raw, _ := io.ReadAll(res.Body)
				res.Body.Close()
				var body map[string]any
				_ = json.Unmarshal(raw, &body)
				wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")
			})
		}
	}
}

// TestConsoleKeyRoutesRejectOtherOrganizations pins the reserved-org gate. The
// segment exists because the reference's does and because org/workspace/project
// are this platform's reserved tenancy keys; until they are real scoping, any
// other value names an organization that does not exist — and says so before the
// environment is looked up, so the segment cannot be used to probe environment
// ids under an org that does not exist.
func TestConsoleKeyRoutesRejectOtherOrganizations(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "org")
	keyID := onlyKeyIDAfterIssue(t, s, envID, "host")

	foreign := func(p string) string {
		return strings.Replace(p, "/organizations/default/", "/organizations/org_other/", 1)
	}
	cases := map[string]struct {
		method, path string
		body         any
	}{
		"issue":  {http.MethodPost, foreign(consoleTokens(envID)), map[string]any{"name": "x"}},
		"list":   {http.MethodGet, foreign(consoleTokens(envID)), nil},
		"revoke": {http.MethodPost, foreign(consoleRevoke(envID, keyID)), nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(tc.method, tc.path, tc.body)
			wantErr(t, status, body, http.StatusNotFound, "not_found_error")
			inner, _ := body["error"].(map[string]any)
			if msg, _ := inner["message"].(string); !strings.Contains(msg, "organization") {
				t.Errorf("message = %q, want it to name the organization, not the environment", msg)
			}
		})
	}
	// And the key the foreign-org revoke aimed at is untouched.
	if ids, _ := consoleKeyIDs(t, s, envID, ""); len(ids) != 1 {
		t.Errorf("a foreign-org revoke changed the key list: %v", ids)
	}
}

// TestConsoleKeyIssueRejectsBadRequests pins every rejection the issuance route
// makes, each with the envelope the console renders. The two environment-kind
// cases are the substantive ones: a cloud environment's work is consumed by this
// platform's own executor, which holds no environment key, so issuing one there
// would hand an operator a credential nothing can use.
func TestConsoleKeyIssueRejectsBadRequests(t *testing.T) {
	s := newTestServer(t)
	selfHosted := selfHostedEnv(t, s, "ok")
	cloud := createEnvironment(t, s, map[string]any{"name": "cloudy"})["id"].(string)
	archived := selfHostedEnv(t, s, "gone")
	if status, body := s.do(http.MethodPost, "/v1/environments/"+archived+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d, body %v", status, body)
	}

	cases := map[string]struct {
		path       string
		body       any
		wantStatus int
		wantType   string
	}{
		"cloud environment":    {consoleTokens(cloud), map[string]any{"name": "x"}, http.StatusBadRequest, "invalid_request_error"},
		"archived environment": {consoleTokens(archived), map[string]any{"name": "x"}, http.StatusBadRequest, "invalid_request_error"},
		"unknown environment":  {consoleTokens("env_0123456789abcdefghjkmnp"), map[string]any{"name": "x"}, http.StatusNotFound, "not_found_error"},
		"malformed environment": {consoleTokens("env_NOT!VALID"), map[string]any{"name": "x"},
			http.StatusNotFound, "not_found_error"},
		"name missing":     {consoleTokens(selfHosted), map[string]any{}, http.StatusBadRequest, "invalid_request_error"},
		"name empty":       {consoleTokens(selfHosted), map[string]any{"name": ""}, http.StatusBadRequest, "invalid_request_error"},
		"name whitespace":  {consoleTokens(selfHosted), map[string]any{"name": "   "}, http.StatusBadRequest, "invalid_request_error"},
		"name not string":  {consoleTokens(selfHosted), map[string]any{"name": 7}, http.StatusBadRequest, "invalid_request_error"},
		"name too long":    {consoleTokens(selfHosted), map[string]any{"name": strings.Repeat("n", 129)}, http.StatusBadRequest, "invalid_request_error"},
		"unknown body key": {consoleTokens(selfHosted), map[string]any{"name": "x", "scopes": []string{"poll"}}, http.StatusBadRequest, "invalid_request_error"},
		"malformed body":   {consoleTokens(selfHosted), "{", http.StatusBadRequest, "invalid_request_error"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, tc.path, tc.body)
			wantErr(t, status, body, tc.wantStatus, tc.wantType)
		})
	}

	// Nothing above minted a key anywhere.
	for _, env := range []string{selfHosted, cloud, archived} {
		if ids, _ := consoleKeyIDs(t, s, env, ""); len(ids) != 0 {
			t.Errorf("environment %s holds %v after only rejected requests", env, ids)
		}
	}

	// A name at exactly the bound is accepted, and stored trimmed.
	name := strings.Repeat("n", 128)
	issueViaConsole(t, s, selfHosted, "  "+name+"  ")
	_, listBody := s.do(http.MethodGet, consoleTokens(selfHosted), nil)
	if got := listData(t, listBody)[0]["name"]; got != name {
		t.Errorf("stored name = %v, want the trimmed 128-char label", got)
	}
}

// TestConsoleKeyListPagesAndRendersNullExpiry pins the listing dialect: the
// offset-paginated envelope the reference console uses (not this platform's
// keyset next_page, which is the wire surface's), and the nullable expires_at a
// pre-0021 key renders — the console must show that as "never", so it has to
// survive the round trip rather than be defaulted to a timestamp.
func TestConsoleKeyListPagesAndRendersNullExpiry(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "paging")
	for _, n := range []string{"host-a", "host-b", "host-c"} {
		issueViaConsole(t, s, envID, n)
	}

	all, page := consoleKeyIDs(t, s, envID, "")
	if len(all) != 3 || page["total"] != float64(3) || page["limit"] != float64(100) ||
		page["offset"] != float64(0) || page["has_more"] != false {
		t.Fatalf("default page = %v, %v; want three rows, total 3, limit 100, offset 0, has_more false", all, page)
	}

	first, page := consoleKeyIDs(t, s, envID, "?limit=2")
	if len(first) != 2 || page["total"] != float64(3) || page["has_more"] != true {
		t.Fatalf("?limit=2 = %v, %v; want two rows with has_more true", first, page)
	}
	rest, page := consoleKeyIDs(t, s, envID, "?limit=2&offset=2")
	if len(rest) != 1 || page["offset"] != float64(2) || page["has_more"] != false {
		t.Fatalf("?limit=2&offset=2 = %v, %v; want the last row with has_more false", rest, page)
	}
	if got := append(append([]string{}, first...), rest...); !slices.Equal(got, all) {
		t.Errorf("paging through = %v, want the unpaged order %v", got, all)
	}
	// Past the end is an empty page, not an error, and still reports the total —
	// a console that lands there can render "3 keys" and page back.
	past, page := consoleKeyIDs(t, s, envID, "?offset=99")
	if len(past) != 0 || page["total"] != float64(3) {
		t.Errorf("?offset=99 = %v, %v; want an empty page reporting total 3", past, page)
	}

	for name, query := range map[string]string{
		"limit zero":          "?limit=0",
		"limit negative":      "?limit=-1",
		"limit over cap":      "?limit=101",
		"limit not a number":  "?limit=many",
		"offset negative":     "?offset=-1",
		"offset not a number": "?offset=soon",
	} {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(http.MethodGet, consoleTokens(envID)+query, nil)
			wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
		})
	}

	// A row from before migration 0021 has no expiry and was promised it would
	// live until revoked; the listing must say so rather than invent a date.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE environment_keys SET expires_at = NULL WHERE environment_id = $1`, envID); err != nil {
		t.Fatalf("rewind the rows to their pre-0021 shape: %v", err)
	}
	_, listBody := s.do(http.MethodGet, consoleTokens(envID), nil)
	for _, row := range listData(t, listBody) {
		raw, ok := row["expires_at"]
		if !ok {
			t.Fatalf("expires_at is absent, not null: %v", row)
		}
		if raw != nil {
			t.Errorf("expires_at = %v on a grandfathered row, want null", raw)
		}
	}
}

// TestConsoleKeyRevokeRejectsIdsItDoesNotOwn pins that revocation can neither
// reach another environment's credential nor confirm that an id exists
// elsewhere: an unknown id, a malformed one and a foreign one all take the same
// branch. The malformed case matters beyond tidiness — envkey_ is deliberately
// outside domain.knownPrefixes, so checkID cannot answer for it, and without the
// local check an unstorable byte would reach a bind parameter and surface as a
// 500 instead of a 404.
func TestConsoleKeyRevokeRejectsIdsItDoesNotOwn(t *testing.T) {
	s := newTestServer(t)
	mine := selfHostedEnv(t, s, "mine")
	theirs := selfHostedEnv(t, s, "theirs")
	theirKey := issueViaConsole(t, s, theirs, "their-host")
	theirID := onlyKeyID(t, s, theirs)

	cases := map[string]string{
		"unknown id":         "envkey_0123456789abcdefghjkmnp",
		"malformed alphabet": "envkey_NOPE!",
		"encoded NUL":        "envkey_%00",
		"wrong prefix":       "env_0123456789abcdefghjkmnp",
		"no prefix":          "envkey",
		"another env's key":  theirID,
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, consoleRevoke(mine, id), nil)
			wantErr(t, status, body, http.StatusNotFound, "not_found_error")
		})
	}
	// The foreign key is untouched by the attempt to revoke it from elsewhere.
	if res, raw := s.poll(t, theirs, map[string]string{"Authorization": "Bearer " + theirKey}); res.StatusCode != http.StatusOK {
		t.Fatalf("a cross-environment revoke attempt disturbed the key: status %d, body %q", res.StatusCode, raw)
	}
}

// TestConsoleKeyRoutesRejectWrongMethods pins the house error envelope on the
// methods these paths do not answer, and the 404 envelope on a neighbouring path
// that does not exist. Go's ServeMux would otherwise write plain text, which the
// console's error handling cannot read.
func TestConsoleKeyRoutesRejectWrongMethods(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "methods")
	keyID := onlyKeyIDAfterIssue(t, s, envID, "host")

	for name, tc := range map[string]struct{ method, path string }{
		"DELETE on tokens": {http.MethodDelete, consoleTokens(envID)},
		"PUT on tokens":    {http.MethodPut, consoleTokens(envID)},
		"GET on revoke":    {http.MethodGet, consoleRevoke(envID, keyID)},
		"DELETE on revoke": {http.MethodDelete, consoleRevoke(envID, keyID)},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(tc.method, tc.path, nil)
			wantErr(t, status, body, http.StatusMethodNotAllowed, "invalid_request_error")
		})
	}
	for name, path := range map[string]string{
		"unknown console path": "/api/oauth/organizations/default/environments/" + envID + "/secrets",
		"namespace root":       "/api/oauth",
		"token without revoke": consoleTokens(envID) + "/" + keyID,
	} {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(http.MethodGet, path, nil)
			wantErr(t, status, body, http.StatusNotFound, "not_found_error")
		})
	}
}

// onlyKeyID returns the id of an environment's single key, failing otherwise.
func onlyKeyID(t *testing.T, s *tserver, envID string) string {
	t.Helper()
	ids, _ := consoleKeyIDs(t, s, envID, "")
	if len(ids) != 1 {
		t.Fatalf("environment %s holds %d keys, want exactly 1", envID, len(ids))
	}
	return ids[0]
}

// onlyKeyIDAfterIssue issues one key over HTTP and returns its id.
func onlyKeyIDAfterIssue(t *testing.T, s *tserver, envID, name string) string {
	t.Helper()
	issueViaConsole(t, s, envID, name)
	return onlyKeyID(t, s, envID)
}

// TestEnvironmentKeyTTLMatchesTheAdvertisedExpiry pins the one number the console
// renders directly against the one the database stores: expires_in seconds and
// the row's expires_at must describe the same instant, or a console counting down
// to renewal lies to the operator.
func TestEnvironmentKeyTTLMatchesTheAdvertisedExpiry(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "ttl")
	status, body := s.do(http.MethodPost, consoleTokens(envID), map[string]any{"name": "host"})
	if status != http.StatusOK {
		t.Fatalf("issue: status %d, body %v", status, body)
	}
	advertised, _ := body["expires_in"].(float64)

	keys, _, err := api.ListEnvironmentKeys(context.Background(), s.pool, envID, 10, 0)
	if err != nil || len(keys) != 1 || keys[0].ExpiresAt == nil {
		t.Fatalf("ListEnvironmentKeys = %v (err %v), want one key with an expiry", keys, err)
	}
	stored := keys[0].ExpiresAt.Sub(keys[0].CreatedAt).Seconds()
	if diff := stored - advertised; diff > 2 || diff < -2 {
		t.Errorf("stored TTL %.0fs vs advertised expires_in %.0fs", stored, advertised)
	}
	if advertised != api.EnvironmentKeyTTL.Seconds() {
		t.Errorf("expires_in = %.0f, want EnvironmentKeyTTL %.0f", advertised, api.EnvironmentKeyTTL.Seconds())
	}
}
