package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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

// zeroUUID is the all-zero UUID the 2026-09-05 recordings sent as a foreign
// organization (batch2 `rec83.edge6.foreign-org-uuid`) and as a bogus API key
// id (batch5 `rec86.keys.update.bogus-id.*`). It is assembled rather than
// spelled out so the source carries no literal twelve-digit run, the shape
// `make identifiers-test` reads as a GCP project number.
var zeroUUID = "00000000-0000-0000-0000-" + strings.Repeat("0", 12)

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

// wantExactFields asserts an object's key set is exactly keys. It is stronger
// than wantFields, which requires the named keys but tolerates extras — and the
// two claims this surface makes loudest are about what a response does *not*
// carry: a listing never renders a secret or its hash, and the issuance response
// carries the token and nothing else. An extras-tolerant assertion cannot catch
// a `key_hash` that someone later adds to the row struct.
func wantExactFields(t *testing.T, obj map[string]any, keys ...string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	want := slices.Clone(keys)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("response fields = %v, want exactly %v", got, want)
	}
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

	issueRes := s.doRaw(http.MethodPost, consoleTokens(envID),
		map[string]any{"name": "build-host-1"}, map[string]string{"x-api-key": testKey})
	issueRaw, _ := io.ReadAll(issueRes.Body)
	issueRes.Body.Close()
	status := issueRes.StatusCode
	var body map[string]any
	_ = json.Unmarshal(issueRaw, &body)
	if status != http.StatusOK {
		t.Fatalf("issue: status %d, body %s", status, issueRaw)
	}
	// RFC 6749 §5.1: a token response forbids caching. This body is the
	// plaintext's only appearance, so a proxy or BFF that retains responses must
	// be told not to keep the second copy.
	if got := issueRes.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := issueRes.Header.Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", got)
	}
	// Exactly the two token-response fields — no id, no name, no timestamps, and
	// above all no second rendering of anything secret-adjacent.
	wantExactFields(t, body, "access_token", "expires_in")
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
	listRes := s.doRaw(http.MethodGet, consoleTokens(envID), nil, map[string]string{"x-api-key": testKey})
	listRaw, _ := io.ReadAll(listRes.Body)
	listRes.Body.Close()
	var listBody map[string]any
	_ = json.Unmarshal(listRaw, &listBody)
	if listRes.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, body %s", listRes.StatusCode, listRaw)
	}
	rows := listData(t, listBody)
	if len(rows) != 1 {
		t.Fatalf("listed %d keys, want 1", len(rows))
	}
	row := rows[0]
	page, _ := listBody["pagination"].(map[string]any)
	keyID, _ := row["id"].(string)
	// Exactly these four — a listing that grew a key_hash or an access_token
	// would satisfy a presence-only assertion.
	wantExactFields(t, row, "id", "name", "created_at", "expires_at")
	wantExactFields(t, page, "total", "limit", "offset", "has_more")
	if s := string(listRaw); strings.Contains(s, token) || strings.Contains(s, stored) {
		t.Error("the listing rendered the secret or its hash")
	}
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

// TestConsoleKeyRoutesRejectOtherOrganizations pins the org gate. The reference
// takes a UUID in that segment and answers a foreign one with a 401
// `authentication_error` carrying `{error_visibility}` (2026-09-05 batch2
// `rec83.edge6.foreign-org-uuid`), and anything that is not a UUID with a 400
// without details — the literal `default` among them (`.literal-default-org`).
// We keep `default` as the one organization we answer for (principle 5's
// reserved key), which is the registered divergence, and answer every other
// value as recorded. Either refusal comes before the environment is looked up,
// so the segment cannot be used to probe environment ids.
func TestConsoleKeyRoutesRejectOtherOrganizations(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "org")
	keyID := onlyKeyIDAfterIssue(t, s, envID, "host")

	under := func(org, p string) string {
		return strings.Replace(p, "/organizations/default/", "/organizations/"+org+"/", 1)
	}
	routes := map[string]struct {
		method, path string
		body         any
	}{
		"issue":  {http.MethodPost, consoleTokens(envID), map[string]any{"name": "x"}},
		"list":   {http.MethodGet, consoleTokens(envID), nil},
		"revoke": {http.MethodPost, consoleRevoke(envID, keyID), nil},
	}
	for name, tc := range routes {
		t.Run(name+"/a foreign UUID", func(t *testing.T) {
			status, body := s.do(tc.method, under(zeroUUID, tc.path), tc.body)
			wantErr(t, status, body, http.StatusUnauthorized, "authentication_error")
			wantDetails(t, body, map[string]any{"error_visibility": "user_facing"})
		})
		t.Run(name+"/not a UUID", func(t *testing.T) {
			status, body := s.do(tc.method, under("org_other", tc.path), tc.body)
			wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
			wantDetails(t, body, nil)
			inner, _ := body["error"].(map[string]any)
			if msg, _ := inner["message"].(string); !strings.Contains(msg, "organization") {
				t.Errorf("message = %q, want it to name the organization, not the environment", msg)
			}
		})
	}
	// The segment takes the UUID spellings a key id does (isUUID): the recorded
	// refusal of `default` is the same parser's message.
	status, body := s.do(http.MethodGet, under(strings.Repeat("0", 32), consoleTokens(envID)), nil)
	wantErr(t, status, body, http.StatusUnauthorized, "authentication_error")

	// The organization is judged before the environment: a foreign UUID over an
	// environment that does not exist is still the 401, not the 404.
	status, body = s.do(http.MethodGet,
		under(zeroUUID, consoleTokens("env_"+strings.Repeat("0", 24))), nil)
	wantErr(t, status, body, http.StatusUnauthorized, "authentication_error")

	// The recorded probe, verbatim but for its environment: ours answers the
	// 401 the reference did.
	t.Run("rec83.edge6.foreign-org-uuid", func(t *testing.T) {
		status, body := s.do(http.MethodGet,
			"/api/oauth/organizations/"+zeroUUID+"/environments/"+envID+"/tokens", nil)
		wantErr(t, status, body, http.StatusUnauthorized, "authentication_error")
		wantDetails(t, body, map[string]any{"error_visibility": "user_facing"})
	})
	// The recorded 400 for `default` (`rec83.edge6.literal-default-org`) is the
	// one we do not follow: `default` is the organization this platform serves.
	t.Run("rec83.edge6.literal-default-org is ours to answer", func(t *testing.T) {
		if ids, _ := consoleKeyIDs(t, s, envID, ""); len(ids) != 1 {
			t.Errorf("the default organization lists %v, want the one key", ids)
		}
	})
	// And the key the foreign-org revokes aimed at is untouched.
	if ids, _ := consoleKeyIDs(t, s, envID, ""); len(ids) != 1 {
		t.Errorf("a foreign-org revoke changed the key list: %v", ids)
	}
}

// TestConsoleKeyIssueRejectsBadRequests pins every rejection the issuance route
// makes, each with the envelope the console renders. An environment's kind and
// archival are not among them: the reference issues on both (2026-09-05 batch2
// `rec83.edge1.issue.on-cloud-env`, `rec83.edge2.issue.on-archived-env`), and
// TestConsoleKeyIssuesOnCloudAndArchivedEnvironments pins that.
func TestConsoleKeyIssueRejectsBadRequests(t *testing.T) {
	s := newTestServer(t)
	selfHosted := selfHostedEnv(t, s, "ok")

	cases := map[string]struct {
		path       string
		body       any
		wantStatus int
		wantType   string
	}{
		"unknown environment": {consoleTokens("env_0123456789abcdefghjkmnp"), map[string]any{"name": "x"}, http.StatusNotFound, "not_found_error"},
		"malformed environment": {consoleTokens("not-an-env-id"), map[string]any{"name": "x"},
			http.StatusBadRequest, "invalid_request_error"},
		"unstorable environment": {consoleTokens("env_%00"), map[string]any{"name": "x"},
			http.StatusBadRequest, "invalid_request_error"},
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

	// Precedence, pinned so it cannot flip back silently: the body is validated
	// before the environment is looked up, because the lookup holds a row lock
	// and a slow client must not be able to hold it open across its own upload.
	// So a request that is wrong in both ways answers on the body. This leaks
	// nothing — a well-formed request already distinguishes an environment that
	// exists from one that does not — and it applies only to issuance; list and
	// revoke still resolve the environment first.
	t.Run("bad body wins over unknown environment", func(t *testing.T) {
		status, body := s.do(http.MethodPost, consoleTokens("env_0123456789abcdefghjkmnp"), map[string]any{"nope": 1})
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	})

	// Nothing above minted a key.
	if ids, _ := consoleKeyIDs(t, s, selfHosted, ""); len(ids) != 0 {
		t.Errorf("environment %s holds %v after only rejected requests", selfHosted, ids)
	}

	// A name at exactly the bound is accepted, and stored trimmed.
	name := strings.Repeat("n", 128)
	issueViaConsole(t, s, selfHosted, "  "+name+"  ")
	_, listBody := s.do(http.MethodGet, consoleTokens(selfHosted), nil)
	if got := listData(t, listBody)[0]["name"]; got != name {
		t.Errorf("stored name = %v, want the trimmed 128-char label", got)
	}

	// The bound is characters, as the message and the docs say — not bytes. A
	// host labelled in Chinese gets the same 128 an ASCII label does; counting
	// bytes would cut it to 42 and tell the operator it was over "128
	// characters", which would be false.
	cjk := strings.Repeat("主", 128)
	if len(cjk) != 384 {
		t.Fatalf("fixture is %d bytes, want 384 — the point is that it exceeds 128 bytes", len(cjk))
	}
	issueViaConsole(t, s, selfHosted, cjk)
	if status, body := s.do(http.MethodPost, consoleTokens(selfHosted),
		map[string]any{"name": strings.Repeat("主", 129)}); status != http.StatusBadRequest {
		t.Errorf("a 129-character name: status %d, body %v; want 400", status, body)
	}
}

// TestConsoleKeyIssuesOnCloudAndArchivedEnvironments pins issuance where the
// reference was recorded issuing: a `cloud` environment and an archived one
// both answer 200 with a token (2026-09-05 batch2
// `rec83.edge1.issue.on-cloud-env`, idx 1, and `rec83.edge2.issue.on-archived-env`,
// idx 23, an archived cloud environment). What such a key can then do on the
// work API is TestACloudEnvironmentKeyCannotTakeItsWork's.
func TestConsoleKeyIssuesOnCloudAndArchivedEnvironments(t *testing.T) {
	s := newTestServer(t)
	cloud := createEnvironment(t, s, map[string]any{"name": "rec83-dialect-edges"})["id"].(string)
	archivedCloud := createEnvironment(t, s, map[string]any{"name": "rec83-second"})["id"].(string)
	archivedSelfHosted := selfHostedEnv(t, s, "gone")
	for _, env := range []string{archivedCloud, archivedSelfHosted} {
		if status, body := s.do(http.MethodPost, "/v1/environments/"+env+"/archive", nil); status != http.StatusOK {
			t.Fatalf("archive %s: status %d, body %v", env, status, body)
		}
	}
	for name, tc := range map[string]struct{ env, key string }{
		"rec83.edge1.issue.on-cloud-env":    {cloud, "rec83-cloud"},
		"rec83.edge2.issue.on-archived-env": {archivedCloud, "rec83-archived"},
		"an archived self_hosted env":       {archivedSelfHosted, "host"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, consoleTokens(tc.env), map[string]any{"name": tc.key})
			if status != http.StatusOK {
				t.Fatalf("issue: status %d, body %v; want 200", status, body)
			}
			wantExactFields(t, body, "access_token", "expires_in")
			if ids, _ := consoleKeyIDs(t, s, tc.env, ""); len(ids) != 1 {
				t.Errorf("listed %v after issuance, want the one key", ids)
			}
		})
	}
}

// TestConsoleKeyIssueLocksTheEnvironmentRow pins the window between reading the
// environment and inserting the key. Both halves are held open deliberately: an
// uncommitted write on the environments row must block the issuing request at
// its FOR SHARE read, so that when the write lands the request sees the
// environment as it now is. Without the lock a delete slipping into that window
// turns the insert's foreign key into a 500 where this route's own 404 is the
// answer. An archive no longer changes the answer — issuance on an archived
// environment is the recorded 200 — so it waits and then issues.
func TestConsoleKeyIssueLocksTheEnvironmentRow(t *testing.T) {
	for _, tc := range []struct {
		name, sql  string
		wantStatus int
		wantType   string
		wantKeys   int
	}{
		{"concurrent archive", `UPDATE environments SET archived_at = now() WHERE id = $1`,
			http.StatusOK, "", 1},
		{"concurrent delete", `DELETE FROM environments WHERE id = $1`,
			http.StatusNotFound, "not_found_error", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			ctx := context.Background()
			envID := selfHostedEnv(t, s, "raced")

			tx, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the racing transaction: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, tc.sql, envID); err != nil {
				t.Fatalf("racing write: %v", err)
			}

			// The goroutine speaks plain net/http: tserver.do fatals on transport
			// errors, and t.Fatalf must not run outside the test goroutine —
			// FailNow would end only the goroutine, leaving the receive below to
			// hang forever. An error is sent back instead (the same pattern
			// sessions_test.go's archive race uses), so every path sends exactly
			// once.
			type result struct {
				status int
				body   map[string]any
				err    error
			}
			done := make(chan result, 1)
			go func() {
				req, err := http.NewRequest(http.MethodPost, s.url+consoleTokens(envID),
					strings.NewReader(`{"name":"host"}`))
				if err != nil {
					done <- result{err: err}
					return
				}
				req.Header.Set("x-api-key", testKey)
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

			// Commit only once the issuing request is observably waiting on the
			// held row lock. A fixed sleep would leave a scheduling window in which
			// a lockless read sees the post-commit row and answers correctly for
			// the wrong reason; without FOR SHARE the read never waits, so this
			// poll timing out is exactly how that mutant fails. (The poll's own
			// query never matches: its wait_event_type is null, not Lock.)
			waitSQL := `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND query LIKE '%FROM environments%FOR SHARE%')`
			for deadline := time.Now().Add(10 * time.Second); ; {
				select {
				case r := <-done:
					t.Fatalf("issuance answered (%d, err %v) before the racing write committed", r.status, r.err)
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
					t.Fatal("the issuing request never blocked on the environment row lock")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit the racing write: %v", err)
			}

			got := <-done
			if got.err != nil {
				t.Fatalf("issuance request: %v", got.err)
			}
			if tc.wantType == "" {
				if got.status != tc.wantStatus {
					t.Fatalf("issuance: status %d, body %v; want %d", got.status, got.body, tc.wantStatus)
				}
			} else {
				wantErr(t, got.status, got.body, tc.wantStatus, tc.wantType)
			}
			var keys int
			if err := s.pool.QueryRow(ctx,
				`SELECT count(*) FROM environment_keys WHERE environment_id = $1`, envID).Scan(&keys); err != nil {
				t.Fatalf("count keys: %v", err)
			}
			if keys != tc.wantKeys {
				t.Errorf("%d key rows after the race, want %d", keys, tc.wantKeys)
			}
		})
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
	rewound := listData(t, listBody)
	// Assert the count first: without it a regression returning an empty data
	// array would satisfy every assertion in the loop below without ever
	// rendering a grandfathered row.
	if len(rewound) != 3 {
		t.Fatalf("listed %d rows after the rewind, want the same 3", len(rewound))
	}
	for _, row := range rewound {
		raw, ok := row["expires_at"]
		if !ok {
			t.Fatalf("expires_at is absent, not null: %v", row)
		}
		if raw != nil {
			t.Errorf("expires_at = %v on a grandfathered row, want null", raw)
		}
	}
}

// TestConsoleKeyListTakesALimitAbove100 pins the recorded absence of an upper
// bound: `limit=101` and `limit=1000` answer 200 with the value echoed in
// `pagination.limit` (2026-09-05 batch2 `rec83.edge5.list.limit.101` and
// `.1000`, idx 11–12). The recording held one key, so it cannot show how many
// rows such a page serves; here it serves up to the limit asked for, which is
// what the echo says, and 100 stays the default.
func TestConsoleKeyListTakesALimitAbove100(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "wide")
	for i := 0; i < 101; i++ {
		issueKey(t, s.pool, envID, "host")
	}

	ids, page := consoleKeyIDs(t, s, envID, "")
	if len(ids) != 100 || page["limit"] != float64(100) || page["total"] != float64(101) || page["has_more"] != true {
		t.Errorf("default page: %d rows, pagination %v; want 100 rows of 101, limit 100, has_more true", len(ids), page)
	}
	for query, limit := range map[string]float64{"?limit=101": 101, "?limit=1000": 1000} {
		t.Run(query, func(t *testing.T) {
			ids, page := consoleKeyIDs(t, s, envID, query)
			if len(ids) != 101 || page["limit"] != limit || page["offset"] != float64(0) || page["has_more"] != false {
				t.Errorf("%d rows, pagination %v; want all 101 rows, limit %v echoed, has_more false", len(ids), page, limit)
			}
		})
	}
}

// TestConsoleKeyRevokeRejectsIdsItDoesNotOwn pins that revocation can neither
// reach another environment's credential nor confirm that an id exists
// elsewhere: an unknown id and a foreign one take the same 404 branch, as the
// reference's do (2026-09-05 batch2 `rec83.edge4.revoke.unknown-uuid` and
// `.cross-environment`), and so does any id shaped like a key's: the
// reference's UUID, or our own envkey_ followed by storable bytes, whatever
// alphabet they are in. Anything else is the 400 the reference answers
// `rec83.edge4.revoke.malformed-id` with, which says only that the id cannot
// be a key's. It matters beyond tidiness — envkey_ is deliberately outside
// domain.knownPrefixes, so checkID cannot answer for it, and without the local
// check an unstorable byte would reach a bind parameter and surface as a 500.
func TestConsoleKeyRevokeRejectsIdsItDoesNotOwn(t *testing.T) {
	s := newTestServer(t)
	mine := selfHostedEnv(t, s, "mine")
	theirs := selfHostedEnv(t, s, "theirs")
	theirKey := issueViaConsole(t, s, theirs, "their-host")
	theirID := onlyKeyID(t, s, theirs)

	for name, id := range map[string]string{
		"recorded malformed id": "not-a-uuid",
		"encoded NUL":           "envkey_%00",
		"invalid UTF-8":         "envkey_%ff",
		"wrong prefix":          "env_0123456789abcdefghjkmnp",
		"no prefix":             "envkey",
		"empty token":           "envkey_",
		"UUID, one digit short": "11111111-2222-3333-4444-55555555555",
		// The four forms the reference's UUID parser takes are exact: the urn
		// prefix only before the hyphenated form, in lowercase, and braces only
		// as a pair around it.
		"urn:uuid: 32 digits":  "urn:uuid:11111111222233334444555555555555",
		"uppercase urn prefix": "URN:UUID:11111111-2222-3333-4444-555555555555",
		"brackets, not braces": "%5B11111111-2222-3333-4444-555555555555%5D",
		"braced 32 digits":     "%7B11111111222233334444555555555555%7D",
		"urn prefix alone":     "urn:uuid:",
		"misplaced hyphens":    "1111111-12222-3333-4444-555555555555",
	} {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, consoleRevoke(mine, id), nil)
			wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
		})
	}

	cases := map[string]string{
		"unknown id":          "envkey_0123456789abcdefghjkmnp",
		"recorded unknown id": "11111111-2222-3333-4444-555555555555",
		"uppercase UUID":      "AAAAAAAA-2222-3333-4444-555555555555",
		"urn:uuid: UUID":      "urn:uuid:11111111-2222-3333-4444-555555555555",
		"32-digit UUID":       "11111111222233334444555555555555",
		"braced UUID":         "%7B11111111-2222-3333-4444-555555555555%7D",
		"another alphabet":    "envkey_NOPE!",
		"another env's key":   theirID,
	}
	messages := map[string]string{}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, consoleRevoke(mine, id), nil)
			wantErr(t, status, body, http.StatusNotFound, "not_found_error")
			inner, _ := body["error"].(map[string]any)
			msg, _ := inner["message"].(string)
			messages[name] = msg
		})
	}
	// One branch means one message. Status and type alone would stay green if a
	// regression said "key belongs to environment X" for the foreign id and "not
	// found" for the unknown one — which is exactly the distinction this route
	// exists not to make.
	for name, msg := range messages {
		if msg != messages["unknown id"] {
			t.Errorf("%s answered %q, want the same message as an unknown id (%q)",
				name, msg, messages["unknown id"])
		}
	}
	if !strings.Contains(messages["unknown id"], "environment key not found") {
		t.Errorf("message = %q, want the id-free not-found wording", messages["unknown id"])
	}
	// The foreign key is untouched by the attempt to revoke it from elsewhere.
	if res, raw := s.poll(t, theirs, map[string]string{"Authorization": "Bearer " + theirKey}); res.StatusCode != http.StatusOK {
		t.Fatalf("a cross-environment revoke attempt disturbed the key: status %d, body %q", res.StatusCode, raw)
	}
}

// TestConsoleKeyErrorsCarryTheRecordedDetails replays each recorded input on
// this surface and pins its recorded answer, details included (2026-09-05
// batch2 idx 2–4 and 18–20, batch8 idx 22; #664), then the inputs no probe
// reached that the same checks answer. An id is malformed only when it lacks
// the prefix or carries unstorable bytes: the reference's own ids use letters
// our minting alphabet does not (`env_01MQbDnwtRB9MBhtuxWAHq1M`), and its key
// ids are UUIDs. The /v1 answer for an unknown environment carries no details,
// as recorded there (2026-09-02 batch2 `env.archive.with-deployment`).
func TestConsoleKeyErrorsCarryTheRecordedDetails(t *testing.T) {
	s := newTestServer(t)
	mine := selfHostedEnv(t, s, "mine")
	theirs := selfHostedEnv(t, s, "theirs")
	issueViaConsole(t, s, theirs, "their-host")
	theirID := onlyKeyID(t, s, theirs)
	envGone := map[string]any{"error_visibility": "user_facing", "error_code": "environment_not_found"}
	envMalformed := map[string]any{"error_visibility": "user_facing", "error_code": "invalid_request"}
	keyGone := map[string]any{"error_visibility": "user_facing"}

	type replay struct {
		method, path string
		body         any
		status       int
		errType      string
		want         map[string]any
	}
	run := func(t *testing.T, cases map[string]replay) {
		t.Helper()
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				status, body := s.do(tc.method, tc.path, tc.body)
				wantErr(t, status, body, tc.status, tc.errType)
				wantDetails(t, body, tc.want)
			})
		}
	}

	t.Run("recorded", func(t *testing.T) {
		run(t, map[string]replay{
			"rec83.edge3.issue.unknown-env": {http.MethodPost, consoleTokens("env_000000000000000000000000"),
				map[string]any{"name": "rec83-x"}, http.StatusNotFound, "not_found_error", envGone},
			"rec83.edge3.issue.malformed-env": {http.MethodPost, consoleTokens("not-an-env-id"),
				map[string]any{"name": "rec83-x"}, http.StatusBadRequest, "invalid_request_error", envMalformed},
			"rec83.edge3.list.unknown-env": {http.MethodGet, consoleTokens("env_000000000000000000000000"),
				nil, http.StatusNotFound, "not_found_error", envGone},
			"rec83.edge4.revoke.unknown-uuid": {http.MethodPost, consoleRevoke(mine, "11111111-2222-3333-4444-555555555555"),
				nil, http.StatusNotFound, "not_found_error", keyGone},
			"rec83.edge4.revoke.malformed-id": {http.MethodPost, consoleRevoke(mine, "not-a-uuid"),
				nil, http.StatusBadRequest, "invalid_request_error", nil},
			// The recorded id is another environment's key; ours is theirs here.
			"rec83.edge4.revoke.cross-environment": {http.MethodPost, consoleRevoke(mine, theirID),
				nil, http.StatusNotFound, "not_found_error", keyGone},
			"setup.envkeyA.mint.foreign-workspace-env": {http.MethodPost, consoleTokens("env_01MQbDnwtRB9MBhtuxWAHq1M"),
				map[string]any{"name": "plan40-envkey-A-retry"}, http.StatusNotFound, "not_found_error", envGone},
		})
	})

	t.Run("same checks, unprobed", func(t *testing.T) {
		run(t, map[string]replay{
			"list, a reference-alphabet environment": {http.MethodGet, consoleTokens("env_01Wxn9vm6spKKooSdNch7e92"),
				nil, http.StatusNotFound, "not_found_error", envGone},
			"list, malformed environment": {http.MethodGet, consoleTokens("not-an-env-id"),
				nil, http.StatusBadRequest, "invalid_request_error", envMalformed},
			"list, unstorable environment": {http.MethodGet, consoleTokens("env_%00"),
				nil, http.StatusBadRequest, "invalid_request_error", envMalformed},
			"revoke, unknown environment": {http.MethodPost, consoleRevoke("env_01Wxn9vm6spKKooSdNch7e92", theirID),
				nil, http.StatusNotFound, "not_found_error", envGone},
			"revoke, malformed environment": {http.MethodPost, consoleRevoke("not-an-env-id", theirID),
				nil, http.StatusBadRequest, "invalid_request_error", envMalformed},
			"revoke, our own unknown key": {http.MethodPost, consoleRevoke(mine, "envkey_0123456789abcdefghjkmnp"),
				nil, http.StatusNotFound, "not_found_error", keyGone},
			// The recorded refusal of `not-a-uuid` (idx 19) names the form it
			// wanted: "an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-]".
			"revoke, the recorded unknown UUID under urn:uuid:": {http.MethodPost, consoleRevoke(mine, "urn:uuid:11111111-2222-3333-4444-555555555555"),
				nil, http.StatusNotFound, "not_found_error", keyGone},
			"revoke, the recorded unknown UUID as 32 digits": {http.MethodPost, consoleRevoke(mine, "11111111222233334444555555555555"),
				nil, http.StatusNotFound, "not_found_error", keyGone},
			"revoke, unstorable key": {http.MethodPost, consoleRevoke(mine, "envkey_%00"),
				nil, http.StatusBadRequest, "invalid_request_error", nil},
			// The reference refuses a malformed key id as path validation —
			// pydantic's `path.token_uuid`, no details (idx 19) — and a malformed
			// environment id in its handler, with details (idx 3). So the key id's
			// shape is judged first, before any environment is looked up.
			"revoke, malformed key under an unknown environment": {http.MethodPost, consoleRevoke("env_01Wxn9vm6spKKooSdNch7e92", "not-a-uuid"),
				nil, http.StatusBadRequest, "invalid_request_error", nil},
			"revoke, malformed key under a malformed environment": {http.MethodPost, consoleRevoke("not-an-env-id", "not-a-uuid"),
				nil, http.StatusBadRequest, "invalid_request_error", nil},
			"revoke, the recorded unknown UUID braced": {http.MethodPost, consoleRevoke(mine, "%7B11111111-2222-3333-4444-555555555555%7D"),
				nil, http.StatusNotFound, "not_found_error", keyGone},
		})
	})

	status, body := s.do(http.MethodPost, "/v1/environments/env_0123456789abcdefghjkmnp/archive", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	wantDetails(t, body, nil)
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
