package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// The management-key console paths, spelled out rather than imported from the
// handler. These are the contract the console's BFF proxies to, mirrored
// segment-for-segment from the reference's private API; a test reusing the
// implementation's own constant could not notice it drift.
const consoleAPIKeysPath = "/api/console/organizations/default/workspaces/default/api_keys"

func consoleAPIKey(id string) string { return consoleAPIKeysPath + "/" + id }

// listAPIKeys reads the listing. It goes through doRaw rather than do because
// the response is a **bare array** — `do` decodes into a map and would hand back
// nil, which is itself worth stating: this surface's collection shape is not the
// wire surface's envelope.
func listAPIKeys(t *testing.T, s *tserver) []map[string]any {
	t.Helper()
	res := s.doRaw(http.MethodGet, consoleAPIKeysPath, nil, map[string]string{"x-api-key": testKey})
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read listing: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list keys: status %d, body %s", res.StatusCode, raw)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("listing is not a bare JSON array: %v (body %s)", err, raw)
	}
	return rows
}

func issueAPIKey(t *testing.T, s *tserver, body map[string]any) map[string]any {
	t.Helper()
	status, obj := s.do(http.MethodPost, consoleAPIKeysPath, body)
	if status != http.StatusOK {
		t.Fatalf("issue %v: status %d, body %v", body, status, obj)
	}
	return obj
}

func rowByID(rows []map[string]any, id string) map[string]any {
	for _, r := range rows {
		if r["id"] == id {
			return r
		}
	}
	return nil
}

// TestAnAdminCanWorkTheAPIKeySurfaceEndToEnd is #378's whole point in one pass:
// an operator with only the console issues a second management credential, drives
// the real /v1 API with it, disables it and is refused, re-enables it and is
// served again, then archives it for good. Every transition is observed through
// HTTP on both surfaces — the console that changes the state and the wire that
// obeys it — rather than by reading the column back.
func TestAnAdminCanWorkTheAPIKeySurfaceEndToEnd(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	created := issueAPIKey(t, s, map[string]any{"name": "ci-runner"})
	raw, _ := created["raw_key"].(string)
	id, _ := created["id"].(string)
	if raw == "" || id == "" {
		t.Fatalf("issuance returned no key or id: %v", created)
	}

	call := func() int {
		res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": raw})
		res.Body.Close()
		return res.StatusCode
	}
	set := func(status string) map[string]any {
		t.Helper()
		code, obj := s.do(http.MethodPost, consoleAPIKey(id), map[string]any{"status": status})
		if code != http.StatusOK {
			t.Fatalf("set status %s: %d, body %v", status, code, obj)
		}
		if obj["status"] != status {
			t.Errorf("after setting %s the resource reports %v", status, obj["status"])
		}
		return obj
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("a freshly issued key: %d, want 200", got)
	}
	set(api.KeyStatusInactive)
	if got := call(); got != http.StatusUnauthorized {
		t.Errorf("a disabled key: %d, want 401", got)
	}
	set(api.KeyStatusActive)
	if got := call(); got != http.StatusOK {
		t.Errorf("a re-enabled key: %d, want 200 — a console disable must be reversible", got)
	}
	set(api.KeyStatusArchived)
	if got := call(); got != http.StatusUnauthorized {
		t.Errorf("an archived key: %d, want 401", got)
	}

	// Archived is not deleted: the row is still listed, which is what lets an
	// operator see that the credential existed at all.
	if row := rowByID(listAPIKeys(t, s), id); row == nil {
		t.Error("the archived key vanished from the listing; archive must not delete")
	} else if row["status"] != api.KeyStatusArchived {
		t.Errorf("archived key lists as %v", row["status"])
	}

	// The plaintext reached the caller and nowhere else.
	var stored int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE key_hash = $1 OR name = $1 OR partial_key_hint = $1`,
		raw).Scan(&stored); err != nil {
		t.Fatalf("scan for the plaintext: %v", err)
	}
	if stored != 0 {
		t.Errorf("the issued key's plaintext appears in %d rows; only its hash may be stored", stored)
	}
}

// TestAPIKeyIssuanceRendersTheRecordedShape pins the create response
// field-for-field against the resource observed live on 2026-08-13 (#378). The
// exact-field assertion is the load-bearing half: an extras-tolerant check could
// not catch a `key_hash` that someone later adds to the row struct.
func TestAPIKeyIssuanceRendersTheRecordedShape(t *testing.T) {
	s := newTestServer(t)
	created := issueAPIKey(t, s, map[string]any{"name": "shape"})

	wantExactFields(t, created,
		"id", "type", "name", "workspace_id", "created_at", "created_by",
		"partial_key_hint", "status", "expires_at", "principal", "raw_key")

	if created["type"] != "api_key" {
		t.Errorf("type = %v, want api_key", created["type"])
	}
	if created["status"] != api.KeyStatusActive {
		t.Errorf("status = %v, want active", created["status"])
	}
	// Null on this deployment, as they were on the reference's own single-tenant
	// account — mirroring rather than reserving by guess, so #56 can populate them
	// without a shape change. Present-and-null, not absent: wantExactFields above
	// already required the keys.
	for _, k := range []string{"workspace_id", "principal", "expires_at"} {
		if created[k] != nil {
			t.Errorf("%s = %v, want null", k, created[k])
		}
	}
	// created_by is the one actor we can answer today. With identity disabled the
	// issuer is the machine credential's own row, so it renders as an api_key.
	by, _ := created["created_by"].(map[string]any)
	if by == nil || by["type"] != "api_key" || !strings.HasPrefix(by["id"].(string), "apikey_") {
		t.Errorf("created_by = %v, want an apikey_ actor", created["created_by"])
	}
	raw, _ := created["raw_key"].(string)
	if !strings.HasPrefix(raw, api.IssuedKeyPrefix) {
		t.Errorf("raw_key %q does not carry the issued-key prefix", raw)
	}
	// The hint shows the public prefix and a little of the body, and is never the
	// key. The reference's own is `sk-ant-api03-XXX...uAAA`.
	hint, _ := created["partial_key_hint"].(string)
	if !strings.HasPrefix(hint, api.IssuedKeyPrefix) || !strings.Contains(hint, "...") {
		t.Errorf("partial_key_hint = %q, want the prefix plus an elision", hint)
	}
	if hint == raw || strings.Contains(raw, strings.TrimPrefix(hint, api.IssuedKeyPrefix)) {
		t.Errorf("partial_key_hint %q is not masked against the key", hint)
	}
	if id, _ := created["id"].(string); !strings.HasPrefix(id, "apikey_") {
		t.Errorf("id = %q, want the apikey_ prefix the reference uses", id)
	}
}

// TestAPIKeyListingIsABareArrayWithEveryRow covers the three things the recorded
// listing does that an invented one would not: no envelope, archived rows still
// present, and — our own addition — the bootstrap key visible beside the issued
// ones, because a listing that hid the credential the caller is authenticating
// with would misdescribe what can reach this API.
func TestAPIKeyListingIsABareArrayWithEveryRow(t *testing.T) {
	s := newTestServer(t)

	first := issueAPIKey(t, s, map[string]any{"name": "one"})["id"].(string)
	second := issueAPIKey(t, s, map[string]any{"name": "two"})["id"].(string)
	if code, obj := s.do(http.MethodPost, consoleAPIKey(first),
		map[string]any{"status": api.KeyStatusArchived}); code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, obj)
	}

	rows := listAPIKeys(t, s)
	if rowByID(rows, first) == nil {
		t.Error("the archived key is missing; the reference returns archived rows and filters client-side")
	}
	if rowByID(rows, second) == nil {
		t.Error("the live key is missing from the listing")
	}
	// The bootstrap row: nobody issued it, so it renders a null creator.
	var bootstrap map[string]any
	for _, r := range rows {
		if r["created_by"] == nil {
			bootstrap = r
		}
	}
	if bootstrap == nil {
		t.Fatalf("the env-var-managed key is not listed; rows = %v", rows)
	}
	// Newest first, so the two issued keys precede the bootstrap row seeded at boot.
	if rows[len(rows)-1]["id"] != bootstrap["id"] {
		t.Errorf("listing is not newest-first: %v", rows)
	}
	// A row carries no secret and no hash, and no can_manage — the field the
	// reference's listing adds and we deliberately omit while the surface is
	// admin-only.
	wantExactFields(t, rows[0],
		"id", "type", "name", "workspace_id", "created_at", "created_by",
		"partial_key_hint", "status", "expires_at", "principal")
}

// TestAPIKeyExpiryIsClientSuppliedAndDerived covers the expiry contract from both
// ends, as recorded: an absent field means never, an absolute instant is stored
// verbatim, and `expired` is computed at read time from that instant rather than
// stored — so it must agree, to the same clock, with the query that authenticates.
func TestAPIKeyExpiryIsClientSuppliedAndDerived(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	never := issueAPIKey(t, s, map[string]any{"name": "never"})
	if never["expires_at"] != nil {
		t.Errorf("a key issued with no expires_at reports %v, want null", never["expires_at"])
	}
	explicitNull := issueAPIKey(t, s, map[string]any{"name": "null", "expires_at": nil})
	if explicitNull["expires_at"] != nil {
		t.Errorf("an explicit null expires_at reports %v, want null", explicitNull["expires_at"])
	}

	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	dated := issueAPIKey(t, s, map[string]any{"name": "dated", "expires_at": at.Format(time.RFC3339)})
	got, _ := dated["expires_at"].(string)
	if parsed, err := time.Parse(time.RFC3339, got); err != nil || !parsed.Equal(at) {
		t.Errorf("expires_at round-tripped as %q, want %s", got, at.Format(time.RFC3339))
	}
	if dated["status"] != api.KeyStatusActive {
		t.Errorf("a key expiring in an hour reports %v", dated["status"])
	}

	// Move the stored instant into the past — the only way to reach the derived
	// state, since issuance refuses to mint one already lapsed.
	id := dated["id"].(string)
	if _, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("age the key: %v", err)
	}
	row := rowByID(listAPIKeys(t, s), id)
	if row["status"] != api.KeyStatusExpired {
		t.Errorf("a lapsed key lists as %v, want expired", row["status"])
	}
	// Derived, not stored: the column still says active.
	var stored string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM api_keys WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("read stored status: %v", err)
	}
	if stored != api.KeyStatusActive {
		t.Errorf("stored status = %q, want active — expired must never be written", stored)
	}
	// And the rendering agrees with the credential path, which is the point of
	// deriving it from the same comparison against the same clock.
	res := s.doRaw(http.MethodGet, "/v1/agents", nil,
		map[string]string{"x-api-key": dated["raw_key"].(string)})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a key the listing calls expired still authenticates: %d", res.StatusCode)
	}
}

// TestAPIKeyIssuanceRejectsBadRequests walks the input contract.
func TestAPIKeyIssuanceRejectsBadRequests(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		name string
		body any
		want string
	}{
		{"no name", map[string]any{}, "name is required"},
		{"blank name", map[string]any{"name": "   "}, "name must be"},
		{"name too long", map[string]any{"name": strings.Repeat("x", 129)}, "name must be"},
		{"name not a string", map[string]any{"name": 7}, "name must be a string"},
		{"unknown field", map[string]any{"name": "n", "nope": 1}, `unknown field "nope"`},
		{"expiry not a timestamp", map[string]any{"name": "n", "expires_at": "tomorrow"}, "RFC 3339"},
		{"expiry in the past", map[string]any{"name": "n", "expires_at": "2020-01-01T00:00:00Z"}, "must be in the future"},
		{"body is not an object", `[]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, obj := s.do(http.MethodPost, consoleAPIKeysPath, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %v)", status, obj)
			}
			if tc.want != "" && !strings.Contains(errMessage(obj), tc.want) {
				t.Errorf("message %q does not mention %q", errMessage(obj), tc.want)
			}
		})
	}
}

// TestAPIKeyUpdateGuardsTheEnumAndTheEnvManagedRow covers the update contract,
// including the two refusals that are ours rather than the reference's.
func TestAPIKeyUpdateGuardsTheEnumAndTheEnvManagedRow(t *testing.T) {
	s := newTestServer(t)
	id := issueAPIKey(t, s, map[string]any{"name": "guarded"})["id"].(string)

	for _, tc := range []struct {
		name string
		body any
		want string
	}{
		// `expired` gets its own message: an operator reaching for it means to
		// retire the key, and the useful answer names the state that does.
		{"expired is derived", map[string]any{"status": "expired"}, "cannot be set"},
		{"unknown status", map[string]any{"status": "deleted"}, "status must be one of"},
		{"empty patch", map[string]any{}, "at least one of"},
		{"unknown field", map[string]any{"status": "active", "nope": 1}, `unknown field "nope"`},
		{"blank name", map[string]any{"name": " "}, "name must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, obj := s.do(http.MethodPost, consoleAPIKey(id), tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %v)", status, obj)
			}
			if !strings.Contains(errMessage(obj), tc.want) {
				t.Errorf("message %q does not mention %q", errMessage(obj), tc.want)
			}
		})
	}

	// A name-only patch works, and leaves the status alone.
	status, obj := s.do(http.MethodPost, consoleAPIKey(id), map[string]any{"name": "renamed"})
	if status != http.StatusOK || obj["name"] != "renamed" || obj["status"] != api.KeyStatusActive {
		t.Errorf("name-only update: %d %v", status, obj)
	}

	// The env-var-managed row is listed but not the console's to mutate. Renaming
	// it would break EnsureAPIKey's rotation, which archives the incumbent by name.
	var bootstrapID string
	for _, r := range listAPIKeys(t, s) {
		if r["created_by"] == nil {
			bootstrapID = r["id"].(string)
		}
	}
	if bootstrapID == "" {
		t.Fatal("no env-var-managed row to test against")
	}
	status, obj = s.do(http.MethodPost, consoleAPIKey(bootstrapID), map[string]any{"name": "hijacked"})
	if status != http.StatusBadRequest || !strings.Contains(errMessage(obj), "CONTROLPLANE_API_KEY") {
		t.Errorf("renaming the env-var-managed key: %d %v, want 400 naming the variable", status, obj)
	}
	status, obj = s.do(http.MethodPost, consoleAPIKey(bootstrapID), map[string]any{"status": "archived"})
	if status != http.StatusBadRequest {
		t.Errorf("archiving the env-var-managed key: %d %v, want 400", status, obj)
	}
	// It really is untouched — the control plane's own credential still works.
	if code, _ := s.do(http.MethodGet, "/v1/agents", nil); code != http.StatusOK {
		t.Errorf("the bootstrap key stopped working: %d", code)
	}
}

// TestArchivingAnAPIKeyIsPermanent is what makes `archived` and `inactive` two
// states rather than one spelling of the same one. A review found the route
// accepting archived → active, which would have meant an operator who retired a
// key had no way to say so and no way to rely on it — while the plaintext may
// still sit in a leaked backup or an old shell history, one admin request away
// from working again. Migration 0024 already asserted the rule ("revocation was
// one-way, and archived is the one-way state"); this enforces it.
func TestArchivingAnAPIKeyIsPermanent(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	created := issueAPIKey(t, s, map[string]any{"name": "retired"})
	id, raw := created["id"].(string), created["raw_key"].(string)
	if code, obj := s.do(http.MethodPost, consoleAPIKey(id),
		map[string]any{"status": api.KeyStatusArchived}); code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, obj)
	}

	for _, patch := range []map[string]any{
		{"status": api.KeyStatusActive},
		{"status": api.KeyStatusInactive},
		{"status": api.KeyStatusArchived},
		{"name": "revived"},
	} {
		status, obj := s.do(http.MethodPost, consoleAPIKey(id), patch)
		if status != http.StatusBadRequest {
			t.Errorf("patch %v on an archived key: %d %v, want 400", patch, status, obj)
		} else if !strings.Contains(errMessage(obj), api.KeyStatusInactive) {
			t.Errorf("the refusal %q does not point at the reversible state", errMessage(obj))
		}
	}

	// The key stayed dead, and the row stayed archived.
	res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": raw})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an archived key authenticates: %d", res.StatusCode)
	}
	var stored string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM api_keys WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if stored != api.KeyStatusArchived {
		t.Errorf("stored status = %q after four refused patches", stored)
	}
}

// TestAPIKeyNamesNeedNotBeUnique pins the finding with the sharpest consequence
// for us: the reference created two live keys named alike, both 200, which is why
// migration 0024 narrowed api_keys_one_live rather than keeping it.
func TestAPIKeyNamesNeedNotBeUnique(t *testing.T) {
	s := newTestServer(t)
	first := issueAPIKey(t, s, map[string]any{"name": "dup"})
	second := issueAPIKey(t, s, map[string]any{"name": "dup"})
	if first["id"] == second["id"] {
		t.Fatal("the second issuance returned the first key")
	}
	for _, k := range []map[string]any{first, second} {
		res := s.doRaw(http.MethodGet, "/v1/agents", nil,
			map[string]string{"x-api-key": k["raw_key"].(string)})
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("key %v does not authenticate: %d", k["id"], res.StatusCode)
		}
	}
}

// TestAPIKeyRoutesRejectUnknownScopesAndIDs keeps the namespace from becoming an
// enumeration oracle: an unknown organization, an unknown workspace and a
// malformed key id all answer with the same 404 shape.
func TestAPIKeyRoutesRejectUnknownScopesAndIDs(t *testing.T) {
	s := newTestServer(t)
	id := issueAPIKey(t, s, map[string]any{"name": "scoped"})["id"].(string)

	for name, path := range map[string]string{
		"unknown organization": "/api/console/organizations/other/workspaces/default/api_keys",
		"unknown workspace":    "/api/console/organizations/default/workspaces/other/api_keys",
	} {
		t.Run(name, func(t *testing.T) {
			if status, obj := s.do(http.MethodGet, path, nil); status != http.StatusNotFound {
				t.Errorf("status %d, want 404 (body %v)", status, obj)
			}
		})
	}

	// Each case names the status it must produce, not merely "not 200". The whole
	// point of validating an id at the edge is that an unstorable byte becomes a
	// 404 instead of binding into a query as a 500 — an assertion that accepted any
	// non-200 would pass on exactly the failure it exists to catch.
	for name, tc := range map[string]struct {
		keyID string
		want  int
	}{
		// A well-formed id of the wrong family, and a well-formed id of the right
		// family that names nothing, are the same answer: this route cannot confirm
		// that an id exists somewhere else.
		"wrong prefix": {"envkey_" + strings.Repeat("a", 24), http.StatusNotFound},
		"unknown id":   {"apikey_" + strings.Repeat("a", 24), http.StatusNotFound},
		"bad alphabet": {"apikey_" + strings.Repeat("!", 24), http.StatusNotFound},
		// Bytes Postgres cannot store in a text column — the reason the shape check
		// runs before the id reaches a query at all. They are written
		// percent-encoded because net/url refuses to build a URL containing a raw
		// control character, so a literal NUL never leaves the client; encoding is
		// also how one would actually arrive, since ServeMux decodes each segment
		// before PathValue sees it.
		"NUL byte":      {"apikey_%00" + strings.Repeat("a", 23), http.StatusNotFound},
		"invalid UTF-8": {"apikey_%ff" + strings.Repeat("a", 23), http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			status, obj := s.do(http.MethodPost, consoleAPIKey(tc.keyID), map[string]any{"status": "active"})
			if status != tc.want {
				t.Errorf("update with %q: status %d, want %d (body %v)", tc.keyID, status, tc.want, obj)
			}
		})
	}

	// A traversal never reaches the handler at all — net/http normalises the path
	// before matching — so the guarantee here is that it does not resolve to this
	// route, and specifically that it is not a server error.
	t.Run("path traversal", func(t *testing.T) {
		status, obj := s.do(http.MethodPost, consoleAPIKey("../../etc/passwd"), map[string]any{"status": "active"})
		if status == http.StatusOK || status >= http.StatusInternalServerError {
			t.Errorf("traversal: status %d (body %v)", status, obj)
		}
	})

	// The real id under the wrong scope is refused by the scope, not by the id.
	status, _ := s.do(http.MethodPost,
		"/api/console/organizations/other/workspaces/default/api_keys/"+id,
		map[string]any{"status": "active"})
	if status != http.StatusNotFound {
		t.Errorf("a real key under an unknown organization: %d, want 404", status)
	}
}

// TestAPIKeyRoutesRejectWrongMethods proves the 405 fallbacks are registered
// against the same patterns the handlers are: a drifted pattern would 404 where
// the house envelope promises a 405, and nothing else would notice.
func TestAPIKeyRoutesRejectWrongMethods(t *testing.T) {
	s := newTestServer(t)
	id := issueAPIKey(t, s, map[string]any{"name": "methods"})["id"].(string)

	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, consoleAPIKeysPath},
		{http.MethodPut, consoleAPIKeysPath},
		{http.MethodGet, consoleAPIKey(id)},
		{http.MethodDelete, consoleAPIKey(id)},
	} {
		status, obj := s.do(tc.method, tc.path, nil)
		if status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want 405 (body %v)", tc.method, tc.path, status, obj)
		}
	}
}

// TestAPIKeyRoutesRequireManagementAuth: the console namespace is off the wire,
// not off authentication.
func TestAPIKeyRoutesRequireManagementAuth(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, consoleAPIKeysPath},
		{http.MethodPost, consoleAPIKeysPath},
		{http.MethodPost, consoleAPIKey("apikey_" + strings.Repeat("a", 24))},
	} {
		res := s.doRaw(tc.method, tc.path, map[string]any{"name": "x"}, nil)
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with no key: status %d, want 401", tc.method, tc.path, res.StatusCode)
		}
	}
}

// TestAPIKeyIssuanceIsNotCacheable: the create response is the plaintext's only
// appearance anywhere, so a console BFF or reverse proxy with response retention
// on must not be the thing that keeps a second copy.
func TestAPIKeyIssuanceIsNotCacheable(t *testing.T) {
	s := newTestServer(t)
	res := s.doRaw(http.MethodPost, consoleAPIKeysPath, map[string]any{"name": "nostore"},
		map[string]string{"x-api-key": testKey})
	defer res.Body.Close()
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	// The listing is not a secret-bearing response and carries no such promise;
	// asserting it here would pin a header nothing needs.
}
