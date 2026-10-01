package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// The management-key console paths, spelled out rather than imported from the
// handler. These are the contract the console's BFF proxies to, mirrored
// segment-for-segment from the reference's private API; a test reusing the
// implementation's own constant could not notice it drift.
const consoleAPIKeysPath = "/api/console/organizations/default/workspaces/default/api_keys"

func consoleAPIKey(id string) string { return consoleAPIKeysPath + "/" + id }

// consoleOrgAPIKey is the update route as the reference serves it, with no
// workspace segment (2026-09-05 batch5 `rec86.keys.update.*`; #820).
func consoleOrgAPIKey(id string) string { return "/api/console/organizations/default/api_keys/" + id }

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
	// A bound on how much of the secret the hint publishes, not a spot check.
	// The previous assertion here compared the hint to the key and looked for the
	// trimmed hint inside it — both structurally impossible, since the hint always
	// carries an ellipsis and a base64url body never contains a dot, so it could
	// not fail whatever partialKeyHint did. A rule emitting `IssuedKeyPrefix` plus
	// twenty body characters would have passed it, publishing half a live
	// credential against an unsalted SHA-256.
	body := strings.TrimPrefix(raw, api.IssuedKeyPrefix)
	shown := utf8.RuneCountInString(
		strings.ReplaceAll(strings.TrimPrefix(hint, api.IssuedKeyPrefix), "...", ""))
	if hidden := utf8.RuneCountInString(body) - shown; hidden < shown {
		t.Errorf("partial_key_hint %q shows %d of the key's %d secret characters, hiding only %d",
			hint, shown, utf8.RuneCountInString(body), hidden)
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
	// The bootstrap row is seeded at boot, milliseconds before these two are
	// issued, and "newest first" is part of what this pins — so the three are
	// stamped a second apart rather than left to that margin (#561). The
	// bootstrap is named here the way the assertion below finds it, by its null
	// creator, which is not the ordering under test.
	var (
		bootstrapID string
		seeded      int
	)
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*), min(id) FROM api_keys WHERE created_by IS NULL`).Scan(&seeded, &bootstrapID); err != nil {
		t.Fatalf("find the bootstrap key: %v", err)
	}
	if seeded != 1 {
		t.Fatalf("%d keys have a null creator; the stamp below would leave the others at their own now()", seeded)
	}
	stampCreatedAt(t, s, "api_keys", bootstrapID, first, second)

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

	// A past instant is accepted and the key is born reporting `expired`. The
	// reference does exactly this (#389, measured 2026-08-13): posting an
	// expires_at of 2020 answers 200 with status `expired`. We used to refuse it.
	past := issueAPIKey(t, s, map[string]any{
		"name": "born-dead", "expires_at": "2020-01-01T00:00:00Z"})
	if past["status"] != api.KeyStatusExpired {
		t.Errorf("a key minted with a past expires_at reports %v, want expired", past["status"])
	}
	if res := s.doRaw(http.MethodGet, "/v1/agents", nil,
		map[string]string{"x-api-key": past["raw_key"].(string)}); res.StatusCode != http.StatusUnauthorized {
		res.Body.Close()
		t.Errorf("a key born expired authenticates: %d", res.StatusCode)
	} else {
		res.Body.Close()
	}

	// Move the stored instant into the past to reach the derived state on a key
	// whose stored status is still `active`.
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

	// Re-activating it is refused rather than answered 200. The stored status is
	// already `active` — expiry is derived — so the write would succeed, change
	// nothing usable, and hand back a resource reporting `expired`: a success for
	// an action that had no effect. This route cannot set expires_at, so there is
	// no version of the request that would work; the honest answer says why.
	status, obj := s.do(http.MethodPost, consoleAPIKey(id), map[string]any{"status": api.KeyStatusActive})
	if status != http.StatusBadRequest {
		t.Errorf("re-activating a lapsed key: %d %v, want 400", status, obj)
	} else if !strings.Contains(errMessage(obj), "expired") {
		t.Errorf("the refusal %q does not name the expiry", errMessage(obj))
	}
	// The same refusal must hold by the route a review found around it: disable a
	// key, let it lapse while disabled, then re-enable.
	disabled := issueAPIKey(t, s, map[string]any{
		"name": "disabled-then-lapsed", "expires_at": at.Format(time.RFC3339)})
	other := disabled["id"].(string)
	if code, body := s.do(http.MethodPost, consoleAPIKey(other),
		map[string]any{"status": api.KeyStatusInactive}); code != http.StatusOK {
		t.Fatalf("disable: %d %v", code, body)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, other); err != nil {
		t.Fatalf("age the disabled key: %v", err)
	}
	// Expired outranks the operator's own disable in the rendering. Measured on
	// the reference (#389): a key set `inactive` while live and then left to lapse
	// lists as `expired`. We used to report `inactive`, on the argument that the
	// operator's action is the more useful answer; the reference says the clock is.
	if row := rowByID(listAPIKeys(t, s), other); row["status"] != api.KeyStatusExpired {
		t.Errorf("a key disabled and then lapsed lists as %v, want expired", row["status"])
	}
	status, obj = s.do(http.MethodPost, consoleAPIKey(other), map[string]any{"status": api.KeyStatusActive})
	if status != http.StatusBadRequest {
		t.Errorf("re-enabling a key that lapsed while disabled: %d %v, want 400", status, obj)
	}

	// A lapsed key admits ONE operation: archiving it. The reference says so in
	// the refusal itself — "Expired API keys can only be deleted, not renamed or
	// reactivated" — and "deleted" there is `status: archived`, since it serves no
	// DELETE verb. We used to permit the disable and the rename too, on the
	// argument that cleanup must not depend on the clock; only the archive does.
	// The empty patch is measured here as well, on a key minted with a past
	// expires_at so it was lapsed but not archived: 400, and the expiry message —
	// so "one operation" really does mean one, and asking for nothing is not it.
	for _, patch := range []map[string]any{
		{"status": api.KeyStatusInactive},
		{"name": "renamed-while-lapsed"},
		{"status": api.KeyStatusArchived, "name": "renamed-while-lapsed"},
		{},
	} {
		code, body := s.do(http.MethodPost, consoleAPIKey(id), patch)
		if code != http.StatusBadRequest {
			t.Errorf("patch %v on a lapsed key: %d %v, want 400", patch, code, body)
		} else if !strings.Contains(errMessage(body), "expired") {
			t.Errorf("the refusal %q does not name the expiry", errMessage(body))
		}
	}

	// Retiring either is the one thing that still works — an operator must be able
	// to clean up, and the reference permits exactly this.
	for _, retire := range []string{id, other} {
		if code, body := s.do(http.MethodPost, consoleAPIKey(retire),
			map[string]any{"status": api.KeyStatusArchived}); code != http.StatusOK {
			t.Errorf("archiving a lapsed key: %d %v, want 200", code, body)
		}
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
		{"name too long", map[string]any{"name": strings.Repeat("x", 501)}, "at most 500 characters"},
		{"name not a string", map[string]any{"name": 7}, "name must be a string"},
		{"unknown field", map[string]any{"name": "n", "nope": 1}, `unknown field "nope"`},
		{"expiry not a timestamp", map[string]any{"name": "n", "expires_at": "tomorrow"}, "RFC 3339"},
		// A past expires_at is NOT here: the reference accepts it and mints a key
		// already reporting `expired` (#389), and so do we —
		// TestAPIKeyExpiryIsClientSuppliedAndDerived covers it.
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
		// A supplied null is not a missing field. `requiredString` folds absent,
		// null and empty into one branch, which would tell a caller who did send
		// the field that they had not.
		{"null status", map[string]any{"status": nil}, "status cannot be null"},
		{"null name", map[string]any{"name": nil}, "name cannot be null"},
		{"unknown field", map[string]any{"status": "active", "nope": 1}, `unknown field "nope"`},
		{"name too long", map[string]any{"name": strings.Repeat("x", 501)}, "at most 500 characters"},
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

	// An empty patch is a no-op that succeeds, not a validation error. The
	// reference answers 200 with the unchanged resource (#389), so we do too.
	if code, obj := s.do(http.MethodPost, consoleAPIKey(id), map[string]any{}); code != http.StatusOK {
		t.Errorf("empty patch on a live key: %d %v, want 200", code, obj)
	} else if obj["status"] != api.KeyStatusActive || obj["name"] != "guarded" {
		t.Errorf("empty patch changed the resource: %v", obj)
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

	// EVERY patch is refused, the repeated archive included. Measured against the
	// reference on 2026-08-13 (#389): all four of these, and a second archive, come
	// back 400 "Archived API keys cannot be updated." An earlier round of #388 made
	// the repeated archive a succeeding no-op on the reasoning that a retried Delete
	// should not error — sound in the abstract, and wrong about this API.
	//
	// The empty patch is in the table for a reason a reviewer found: without it an
	// implementation that answered `{}` before consulting the row's state would pass
	// this test while breaking the rule it exists to pin. It is measured too, on the
	// same reference and on an already-archived key — 400, and the *archived*
	// message, so the terminality check runs ahead of any shape check.
	for _, patch := range []map[string]any{
		{"status": api.KeyStatusActive},
		{"status": api.KeyStatusInactive},
		{"name": "revived"},
		{"status": api.KeyStatusArchived, "name": "revived"},
		// The repeated archive: a transition to the state that already holds.
		{"status": api.KeyStatusArchived},
		// The empty patch: nothing asked for, still refused.
		{},
	} {
		status, obj := s.do(http.MethodPost, consoleAPIKey(id), patch)
		if status != http.StatusBadRequest {
			t.Errorf("patch %v on an archived key: %d %v, want 400", patch, status, obj)
		} else if !strings.Contains(errMessage(obj), api.KeyStatusArchived) {
			t.Errorf("the refusal %q does not name the archived state", errMessage(obj))
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
		t.Errorf("stored status = %q after six refused patches", stored)
	}

	// Archived outranks expired in the rendering. A row archived after its expiry
	// had passed reports `archived`, not `expired` — measured on the reference,
	// where a key that had already lapsed rendered `archived` the moment it was
	// retired. So the precedence is archived > expired > the stored status.
	if _, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("age the archived key: %v", err)
	}
	if row := rowByID(listAPIKeys(t, s), id); row["status"] != api.KeyStatusArchived {
		t.Errorf("an archived key past its expiry lists as %v, want archived", row["status"])
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
// enumeration oracle: an unknown workspace and an unknown key id answer 404
// with the reference's details. The organization segment answers as the
// environment-key surface's does (TestConsoleKeyRoutesRejectOtherOrganizations):
// a foreign UUID is the reference's 401, anything else but `default` its 400.
// A malformed key id — no apikey_ prefix, or bytes that cannot be stored — is
// the reference's 400 (2026-09-05 batch5 `rec86.keys.update.bogus-id.*`),
// which says only that the id cannot be a key's.
func TestAPIKeyRoutesRejectUnknownScopesAndIDs(t *testing.T) {
	s := newTestServer(t)
	id := issueAPIKey(t, s, map[string]any{"name": "scoped"})["id"].(string)

	for name, tc := range map[string]struct {
		path string
		want int
	}{
		"organization not a UUID": {"/api/console/organizations/other/workspaces/default/api_keys", http.StatusBadRequest},
		"foreign organization":    {"/api/console/organizations/" + zeroUUID + "/workspaces/default/api_keys", http.StatusUnauthorized},
		"unknown workspace":       {"/api/console/organizations/default/workspaces/other/api_keys", http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			if status, obj := s.do(http.MethodGet, tc.path, nil); status != tc.want {
				t.Errorf("status %d, want %d (body %v)", status, tc.want, obj)
			}
		})
	}

	// Each case names the status it must produce, not merely "not 200". The whole
	// point of validating an id at the edge is that an unstorable byte becomes a
	// 400 instead of binding into a query as a 500 — an assertion that accepted any
	// non-200 would pass on exactly the failure it exists to catch.
	for name, tc := range map[string]struct {
		keyID string
		want  int
	}{
		"unknown id": {"apikey_" + strings.Repeat("a", 24), http.StatusNotFound},
		// The reference's ids are not drawn from our minting alphabet, so the
		// alphabet is no test of shape.
		"another alphabet": {"apikey_" + strings.Repeat("!", 24), http.StatusNotFound},
		// The id the reference was recorded refusing, and a well-formed id of the
		// wrong family: neither can name a key, whatever exists elsewhere.
		"recorded bogus id": {"00000000-0000-0000-0000-000000000000", http.StatusBadRequest},
		"wrong prefix":      {"envkey_" + strings.Repeat("a", 24), http.StatusBadRequest},
		"empty token":       {"apikey_", http.StatusBadRequest},
		// Bytes Postgres cannot store in a text column — the reason the shape check
		// runs before the id reaches a query at all. They are written
		// percent-encoded because net/url refuses to build a URL containing a raw
		// control character, so a literal NUL never leaves the client; encoding is
		// also how one would actually arrive, since ServeMux decodes each segment
		// before PathValue sees it.
		"NUL byte":      {"apikey_%00" + strings.Repeat("a", 23), http.StatusBadRequest},
		"invalid UTF-8": {"apikey_%ff" + strings.Repeat("a", 23), http.StatusBadRequest},
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

	// The real id under the wrong scope is refused by the scope, not by the id,
	// on both update routes.
	for path, want := range map[string]int{
		"/api/console/organizations/other/workspaces/default/api_keys/" + id:            http.StatusBadRequest,
		"/api/console/organizations/" + zeroUUID + "/api_keys/" + id:                    http.StatusUnauthorized,
		"/api/console/organizations/default/workspaces/other/api_keys/" + id:            http.StatusNotFound,
		"/api/console/organizations/" + zeroUUID + "/workspaces/default/api_keys/" + id: http.StatusUnauthorized,
	} {
		if status, obj := s.do(http.MethodPost, path, map[string]any{"status": "inactive"}); status != want {
			t.Errorf("a real key under %s: %d, want %d (body %v)", path, status, want, obj)
		}
	}
	if row := rowByID(listAPIKeys(t, s), id); row == nil || row["status"] != api.KeyStatusActive {
		t.Errorf("a refused update changed the key: %v", row)
	}
}

// TestAPIKeyErrorsCarryTheRecordedDetails replays each recorded input on this
// surface and pins its recorded answer, details included (2026-09-05 batch5
// idx 7–12, batch8 idx 33–34; #664), on the recorded update route, which has
// no workspace segment, and on ours, which keeps one as an alias (#820). An id
// without the apikey_ prefix is a 400 whatever the body says; a well-formed id
// that names nothing is a 404 whatever the body says, so the lookup precedes
// the body's validation (`rec86.keys.update.wellformed-id.bad-field`).
func TestAPIKeyErrorsCarryTheRecordedDetails(t *testing.T) {
	s := newTestServer(t)
	for route, keyPath := range map[string]func(string) string{
		"recorded route":  consoleOrgAPIKey,
		"workspace alias": consoleAPIKey,
	} {
		bogus := keyPath(zeroUUID)
		unknown := keyPath("apikey_01ABCDEFGHJKMNPQRSTVWXYZ")
		for name, tc := range map[string]struct {
			method, path string
			body         any
			status       int
			errType      string
		}{
			"rec86.keys.update.bogus-id.empty-body":           {http.MethodPost, bogus, map[string]any{}, http.StatusBadRequest, "invalid_request_error"},
			"rec86.keys.update.bogus-id.status":               {http.MethodPost, bogus, map[string]any{"status": "inactive"}, http.StatusBadRequest, "invalid_request_error"},
			"rec86.keys.update.bogus-id.bad-status":           {http.MethodPost, bogus, map[string]any{"status": "not-a-real-status"}, http.StatusBadRequest, "invalid_request_error"},
			"rec86.keys.update.wellformed-id.empty-body":      {http.MethodPost, unknown, map[string]any{}, http.StatusNotFound, "not_found_error"},
			"rec86.keys.update.wellformed-id.status-inactive": {http.MethodPost, unknown, map[string]any{"status": "inactive"}, http.StatusNotFound, "not_found_error"},
			"rec86.keys.update.wellformed-id.bad-field":       {http.MethodPost, unknown, map[string]any{"nonexistent_field": 1}, http.StatusNotFound, "not_found_error"},
			"unprobed: update, wrong-prefix id":               {http.MethodPost, keyPath("envkey_" + strings.Repeat("a", 24)), map[string]any{"status": "active"}, http.StatusBadRequest, "invalid_request_error"},
			"unprobed: update, unknown id, malformed body":    {http.MethodPost, unknown, "{", http.StatusNotFound, "not_found_error"},
		} {
			t.Run(route+"/"+name, func(t *testing.T) {
				status, body := s.do(tc.method, tc.path, tc.body)
				wantErr(t, status, body, tc.status, tc.errType)
				wantDetails(t, body, map[string]any{"error_visibility": "user_facing"})
			})
		}
	}
	for name, tc := range map[string]struct {
		method, path string
		body         any
	}{
		"item6.after-archive.workspaceB.get":      {http.MethodGet, "/api/console/organizations/default/workspaces/wrkspc_01Spc4DriXcw2C5LMAYvNNkp", nil},
		"item6.after-archive.workspaceB.api_keys": {http.MethodGet, "/api/console/organizations/default/workspaces/wrkspc_01Spc4DriXcw2C5LMAYvNNkp/api_keys", nil},
		"unprobed: issue, unknown workspace":      {http.MethodPost, "/api/console/organizations/default/workspaces/other/api_keys", map[string]any{"name": "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(tc.method, tc.path, tc.body)
			wantErr(t, status, body, http.StatusNotFound, "not_found_error")
			wantDetails(t, body, map[string]any{"error_visibility": "user_facing"})
		})
	}

	// On a key that exists, the body is still validated, before any state guard.
	id := issueAPIKey(t, s, map[string]any{"name": "present"})["id"].(string)
	status, body := s.do(http.MethodPost, consoleAPIKey(id), map[string]any{"nonexistent_field": 1})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	wantDetails(t, body, nil)
}

// TestAPIKeyUpdateJudgesTheBodyWithoutTheRowLock: an invalid body on a key
// that exists is refused without taking the key's row lock, so it neither
// waits behind a writer holding the row nor holds one up. Only a body too
// large to read is refused before the lookup, unknown key or not.
func TestAPIKeyUpdateJudgesTheBodyWithoutTheRowLock(t *testing.T) {
	s := newTestServer(t)
	id := issueAPIKey(t, s, map[string]any{"name": "locked"})["id"].(string)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM api_keys WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := s.roundTrip(reqCtx, http.MethodPost, consoleAPIKey(id), map[string]any{"nonexistent_field": 1},
		map[string]string{"x-api-key": testKey})
	if err != nil {
		t.Fatalf("an invalid body waited on the row lock: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid body on a locked key: status %d, want 400", res.StatusCode)
	}

	oversize := `{"name":"` + strings.Repeat("x", 4<<20) + `"}`
	status, body := s.do(http.MethodPost, consoleAPIKey("apikey_01ABCDEFGHJKMNPQRSTVWXYZ"), oversize)
	wantErr(t, status, body, http.StatusRequestEntityTooLarge, "request_too_large")
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
		{http.MethodPost, consoleOrgAPIKey("apikey_" + strings.Repeat("a", 24))},
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

// TestAPIKeyNameIsTheRecordedBound pins the name rule the reference was
// recorded applying (2026-09-05 batch5 `rec86.create.name.*`, idx 13–23): 1
// to 500 characters, counted as characters, with nothing trimmed — a
// whitespace-only name passes, and is stored and echoed as sent (2026-09-24
// `api-key`, a key named three spaces). 501 and 1,000 are refused with the
// recorded message. A rename is held to the same rule, which no recording
// shows.
func TestAPIKeyNameIsTheRecordedBound(t *testing.T) {
	s := newTestServer(t)
	for name, sent := range map[string]string{
		"rec86.create.name.1char":      "x",
		"rec86.create.name.129":        strings.Repeat("x", 129),
		"rec86.create.name.500":        strings.Repeat("x", 500),
		"rec86.create.name.whitespace": "   ",
		"rec86.create.name.tab-nl":     "\t\n ",
		"rec86.create.name.unicode":    "中文名称 🚀",
		"500 characters, 1,500 bytes":  strings.Repeat("主", 500),
	} {
		t.Run(name, func(t *testing.T) {
			issued := issueAPIKey(t, s, map[string]any{"name": sent})
			if issued["name"] != sent {
				t.Errorf("echoed name %q, want %q as sent", issued["name"], sent)
			}
			if row := rowByID(listAPIKeys(t, s), issued["id"].(string)); row == nil || row["name"] != sent {
				t.Errorf("listed %v, want the name stored as sent", row)
			}
		})
	}
	for name, body := range map[string]map[string]any{
		"rec86.create.name.501":        {"name": strings.Repeat("x", 501)},
		"rec86.create.name.1000":       {"name": strings.Repeat("x", 1000)},
		"501 characters":               {"name": strings.Repeat("主", 501)},
		"rec86.create.name.missing":    {},
		"rec86.create.name.empty":      {"name": ""},
		"rec86.create.name.wrong-type": {"name": 123},
	} {
		t.Run(name, func(t *testing.T) {
			status, obj := s.do(http.MethodPost, consoleAPIKeysPath, body)
			wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
			wantDetails(t, obj, nil)
			if strings.HasPrefix(name, "rec86.create.name.501") || strings.HasPrefix(name, "rec86.create.name.1000") {
				if msg := errMessage(obj); !strings.Contains(msg, "at most 500 characters") {
					t.Errorf("message %q, want the recorded \"at most 500 characters\"", msg)
				}
			}
		})
	}

	id := issueAPIKey(t, s, map[string]any{"name": "rename-me"})["id"].(string)
	status, obj := s.do(http.MethodPost, consoleAPIKey(id), map[string]any{"name": strings.Repeat("x", 501)})
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")
	if status, obj := s.do(http.MethodPost, consoleAPIKey(id), map[string]any{"name": "  "}); status != http.StatusOK || obj["name"] != "  " {
		t.Errorf("rename to two spaces: %d %v, want 200 with the name as sent", status, obj)
	}
}

// TestAPIKeyPrincipalIDIsJudgedAsRecorded pins `principal_id` on create as far
// as the recordings reach. Its shape is recorded: a value that is not a `user_`
// or `svac_` id is a 400 with `{error_visibility}` (2026-09-05 batch5
// `rec86.create.principal_id.bogus-string`, idx 26), whose message names the
// two families. What a well-formed one does — link the key to that user or
// service account — is not, and this platform has neither, so one names
// nothing here: the namespace's 404 with the same details, inferred. Nothing
// refused mints a key.
func TestAPIKeyPrincipalIDIsJudgedAsRecorded(t *testing.T) {
	s := newTestServer(t)
	before := len(listAPIKeys(t, s))
	for name, tc := range map[string]struct {
		principal any
		status    int
		errType   string
		details   map[string]any
	}{
		"rec86.create.principal_id.bogus-string": {"zzz-not-a-principal", http.StatusBadRequest, "invalid_request_error", map[string]any{"error_visibility": "user_facing"}},
		"one of our principal_ ids":              {"principal_01ABCDEFGHJKMNPQRSTVWXYZ", http.StatusBadRequest, "invalid_request_error", map[string]any{"error_visibility": "user_facing"}},
		"an empty user_ id":                      {"user_", http.StatusBadRequest, "invalid_request_error", map[string]any{"error_visibility": "user_facing"}},
		"empty":                                  {"", http.StatusBadRequest, "invalid_request_error", map[string]any{"error_visibility": "user_facing"}},
		"not a string":                           {7, http.StatusBadRequest, "invalid_request_error", nil},
		// The user id the reference stamped as a key's created_by (batch9
		// `setup.keyC.create`), and a service account id in the same alphabet.
		"a user id":            {"user_014nwQChpX4bYXrKwPAEUM47", http.StatusNotFound, "not_found_error", map[string]any{"error_visibility": "user_facing"}},
		"a service account id": {"svac_01ABCDEFGHJKMNPQRSTVWXYZ", http.StatusNotFound, "not_found_error", map[string]any{"error_visibility": "user_facing"}},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, consoleAPIKeysPath,
				map[string]any{"name": "rec86 probe", "principal_id": tc.principal})
			wantErr(t, status, body, tc.status, tc.errType)
			wantDetails(t, body, tc.details)
		})
	}
	if after := len(listAPIKeys(t, s)); after != before {
		t.Errorf("refused creates changed the listing from %d keys to %d", before, after)
	}
	// The name is judged first. That order is inferred, not recorded: idx 21's
	// body holds the 501-character name alone, and no recording pairs a bad
	// name with a bad principal_id. Its refusal reads as the reference's schema
	// validation (pydantic's message, no details), and the principal_id one
	// (idx 26, with details) as its handler's, which would run after.
	status, body := s.do(http.MethodPost, consoleAPIKeysPath,
		map[string]any{"name": strings.Repeat("x", 501), "principal_id": "zzz-not-a-principal"})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	wantDetails(t, body, nil)
	// An explicit null is no principal at all.
	issued := issueAPIKey(t, s, map[string]any{"name": "unlinked", "principal_id": nil})
	if issued["principal"] != nil {
		t.Errorf("principal = %v, want null", issued["principal"])
	}
}

// TestAPIKeyUpdatesOnTheRecordedRoute pins the update route the reference
// serves, `POST …/organizations/{org}/api_keys/{id}` with no workspace segment
// (2026-09-05 batch5 `rec86.keys.update.*`), driven with the body the
// reference console's own Disable sends (2026-09-24 `api-key`,
// `{"status":"inactive"}`, answered 200 with the key inactive). It is the same
// update as the workspace route ours kept as an alias, which re-enables the key
// here. Every other method is a 405, as recorded (`rec86.keys.method.*` and
// `rec86.keys.revoke.route-probe.bogus-id`, idx 2 and 4–6).
func TestAPIKeyUpdatesOnTheRecordedRoute(t *testing.T) {
	s := newTestServer(t)
	issued := issueAPIKey(t, s, map[string]any{"name": "   "})
	id := issued["id"].(string)
	raw := issued["raw_key"].(string)

	status, obj := s.do(http.MethodPost, consoleOrgAPIKey(id), map[string]any{"status": "inactive"})
	if status != http.StatusOK || obj["status"] != api.KeyStatusInactive || obj["name"] != "   " || obj["id"] != id {
		t.Fatalf("disable on the recorded route: %d %v, want 200 with the key inactive", status, obj)
	}
	res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": raw})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a disabled key still authenticates: %d", res.StatusCode)
	}
	if status, obj := s.do(http.MethodPost, consoleAPIKey(id), map[string]any{"status": "active"}); status != http.StatusOK || obj["status"] != api.KeyStatusActive {
		t.Errorf("re-enable on the workspace alias: %d %v", status, obj)
	}

	for _, method := range []string{http.MethodDelete, http.MethodPatch, http.MethodPut, http.MethodGet} {
		status, body := s.do(method, consoleOrgAPIKey(zeroUUID), map[string]any{"status": "inactive"})
		wantErr(t, status, body, http.StatusMethodNotAllowed, "invalid_request_error")
	}
}

// TestConsoleWorkspaceReadIsTheRecordedNotFound pins `GET
// …/organizations/{org}/workspaces/{id}`. The one recorded read answered 404
// with `{error_visibility}` (2026-09-05 batch8
// `item6.after-archive.workspaceB.get`, idx 33), and every other recorded
// workspace body is a `wrkspc_` row whose fields — a display color, a data
// residency, a compartment id — this platform has no values for. The reserved
// `default` workspace is no such row: the reference's own Default workspace
// never appears in its workspace listing (batch9 idx 4). So every id answers
// that 404, `default` included, and none renders a workspace.
func TestConsoleWorkspaceReadIsTheRecordedNotFound(t *testing.T) {
	s := newTestServer(t)
	for _, ws := range []string{"wrkspc_01Spc4DriXcw2C5LMAYvNNkp", "default", "other"} {
		t.Run(ws, func(t *testing.T) {
			status, body := s.do(http.MethodGet, "/api/console/organizations/default/workspaces/"+ws, nil)
			wantErr(t, status, body, http.StatusNotFound, "not_found_error")
			wantDetails(t, body, map[string]any{"error_visibility": "user_facing"})
		})
	}
	status, body := s.do(http.MethodGet, "/api/console/organizations/"+zeroUUID+"/workspaces/default", nil)
	wantErr(t, status, body, http.StatusUnauthorized, "authentication_error")
	status, body = s.do(http.MethodPost, "/api/console/organizations/default/workspaces/default", map[string]any{})
	wantErr(t, status, body, http.StatusMethodNotAllowed, "invalid_request_error")
	res := s.doRaw(http.MethodGet, "/api/console/organizations/default/workspaces/default", nil, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the workspace read with no credential: %d, want 401", res.StatusCode)
	}
}
