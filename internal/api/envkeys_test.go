package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// spreadCreatedAt pushes each named key one further second into the past, oldest
// name last, so that a newest-first assertion tests `ORDER BY created_at DESC`
// rather than the microsecond resolution of two consecutive now() calls. Without
// it a tie falls through to `id DESC` over 120 random bits and the assertion
// flips at random — rare, but a test that is right by luck is not a test. Names
// are given newest-first, matching the order the listing must return.
//
// One statement, deliberately. A per-row UPDATE loop would take a fresh now()
// each time, so a slow round trip could hand the "older" row a later timestamp
// than the "newer" one and invert the very order this exists to fix — trading
// one nondeterminism for another. Inside a single statement now() is one value,
// so the offsets are exact.
func spreadCreatedAt(t *testing.T, s *tserver, envID string, newestFirst ...string) {
	t.Helper()
	ages := make([]float64, len(newestFirst))
	for i := range newestFirst {
		ages[i] = float64(i)
	}
	tag, err := s.pool.Exec(context.Background(),
		`UPDATE environment_keys k
		    SET created_at = now() - make_interval(secs => n.age)
		   FROM unnest($1::text[], $2::float8[]) AS n(name, age)
		  WHERE k.environment_id = $3 AND k.name = n.name`,
		newestFirst, ages, envID)
	if err != nil {
		t.Fatalf("spread created_at over %v: %v", newestFirst, err)
	}
	if tag.RowsAffected() != int64(len(newestFirst)) {
		t.Fatalf("spread created_at touched %d rows, want %d (names %v)",
			tag.RowsAffected(), len(newestFirst), newestFirst)
	}
}

// selfHostedEnv creates a self-hosted environment and returns its id — the only
// environment kind whose work queue a BYOC worker polls, and so the only one
// these credentials are ever issued for.
func selfHostedEnv(t *testing.T, s *tserver, name string) string {
	t.Helper()
	env := createEnvironment(t, s, map[string]any{
		"name": name, "config": map[string]any{"type": "self_hosted"}})
	id, _ := env["id"].(string)
	return id
}

// TestIssueEnvironmentKeyGivesEachHostItsOwnCredential is the model migration
// 0021 replaced rotate-on-mint with: an environment holds as many live keys as it
// has hosts, issuing one does not disturb the others, and revoking one takes down
// exactly that host. Under the old one-live invariant the second issue would have
// revoked the first, which is precisely what made per-host revocation impossible.
func TestIssueEnvironmentKeyGivesEachHostItsOwnCredential(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	envID := selfHostedEnv(t, s, "hosts")

	first := issueKey(t, s.pool, envID, "host-a")
	second := issueKey(t, s.pool, envID, "host-b")
	if first == second {
		t.Fatal("two issues returned the same key value")
	}

	authA := map[string]string{"Authorization": "Bearer " + first}
	authB := map[string]string{"Authorization": "Bearer " + second}
	if res, raw := s.poll(t, envID, authA); res.StatusCode != http.StatusOK {
		t.Fatalf("host-a's key does not poll: status %d, body %q", res.StatusCode, raw)
	}
	if res, raw := s.poll(t, envID, authB); res.StatusCode != http.StatusOK {
		t.Fatalf("issuing host-b's key revoked host-a's slot: status %d, body %q", res.StatusCode, raw)
	}

	// Both are listed, newest first, each carrying the name it was issued under
	// and an expiry a year out. The timestamps are spread first so "newest first"
	// is what is being tested, not the clock's resolution.
	spreadCreatedAt(t, s, envID, "host-b", "host-a")
	keys, total, err := api.ListEnvironmentKeys(ctx, s.pool, envID, 100, 0)
	if err != nil {
		t.Fatalf("ListEnvironmentKeys: %v", err)
	}
	if total != 2 || len(keys) != 2 {
		t.Fatalf("listed %d of total %d, want 2 of 2", len(keys), total)
	}
	if keys[0].Name != "host-b" || keys[1].Name != "host-a" {
		t.Errorf("names/order = %q, %q; want host-b then host-a (newest first)", keys[0].Name, keys[1].Name)
	}
	for _, k := range keys {
		if k.ExpiresAt == nil {
			t.Fatalf("key %s (%s) was issued without an expiry", k.ID, k.Name)
		}
		if got := k.ExpiresAt.Sub(k.CreatedAt); got < api.EnvironmentKeyTTL-time.Minute || got > api.EnvironmentKeyTTL+time.Minute {
			t.Errorf("key %s lifetime = %v, want %v", k.Name, got, api.EnvironmentKeyTTL)
		}
	}

	// Revoking host-a's key takes down host-a alone.
	found, err := api.RevokeEnvironmentKey(ctx, s.pool, envID, keys[1].ID)
	if err != nil || !found {
		t.Fatalf("RevokeEnvironmentKey(host-a) = %v, %v; want true, nil", found, err)
	}
	if res, _ := s.poll(t, envID, authA); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked key still polls: status %d", res.StatusCode)
	}
	if res, raw := s.poll(t, envID, authB); res.StatusCode != http.StatusOK {
		t.Errorf("revoking host-a's key took host-b's down too: status %d, body %q", res.StatusCode, raw)
	}
	// A revoked key is gone from the listing, not shown as retired.
	if _, total, err := api.ListEnvironmentKeys(ctx, s.pool, envID, 100, 0); err != nil || total != 1 {
		t.Errorf("total after revoke = %d (err %v), want 1", total, err)
	}
}

// TestIssueEnvironmentKeyStoresOnlyItsHash pins the property the whole
// show-it-once UX rests on: the plaintext returned by the issuing call is the
// only copy, so nothing can render the key again. It also pins the wire-visible
// shape of the secret — the prefix marks it as ours rather than an Anthropic
// credential (see environmentKeySecretPrefix).
func TestIssueEnvironmentKeyStoresOnlyItsHash(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "hash-only")
	key := issueKey(t, s.pool, envID, "host")

	if !strings.HasPrefix(key, "sk-map-env01-") {
		t.Errorf("issued key %q does not carry the platform's key prefix", key)
	}
	if len(key) < len("sk-map-env01-")+40 {
		t.Errorf("issued key %q is shorter than 256 bits of base64url", key)
	}
	var stored int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM environment_keys WHERE key_hash = $1 OR name = $1`, key).Scan(&stored); err != nil {
		t.Fatalf("search for the plaintext: %v", err)
	}
	if stored != 0 {
		t.Error("the issued key was stored in plaintext")
	}
}

// TestEnvironmentKeyExpiryStopsAuthentication: an expiry that nothing enforces is
// decoration. An expired key must fail exactly as a revoked or unknown one does —
// same status, same message — so a client cannot use the auth lane to learn which
// of the three it hit.
func TestEnvironmentKeyExpiryStopsAuthentication(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "expiring")
	key := issueKey(t, s.pool, envID, "host")
	auth := map[string]string{"Authorization": "Bearer " + key}

	if res, raw := s.poll(t, envID, auth); res.StatusCode != http.StatusOK {
		t.Fatalf("freshly issued key does not poll: status %d, body %q", res.StatusCode, raw)
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE environment_keys SET expires_at = now() - interval '1 second' WHERE environment_id = $1`,
		envID); err != nil {
		t.Fatalf("age the key past its expiry: %v", err)
	}

	expired := s.doRaw(http.MethodGet, "/v1/environments/"+envID+"/work/poll", nil, auth)
	expiredStatus, expiredBody := expired.StatusCode, decodeBody(t, expired)
	unknown := s.doRaw(http.MethodGet, "/v1/environments/"+envID+"/work/poll", nil,
		map[string]string{"Authorization": "Bearer sk-map-env01-not-a-real-key"})
	unknownStatus, unknownBody := unknown.StatusCode, decodeBody(t, unknown)

	wantErr(t, expiredStatus, expiredBody, http.StatusUnauthorized, "authentication_error")
	wantErr(t, unknownStatus, unknownBody, http.StatusUnauthorized, "authentication_error")
	message := func(body map[string]any) string {
		inner, _ := body["error"].(map[string]any)
		msg, _ := inner["message"].(string)
		return msg
	}
	if got, want := message(expiredBody), message(unknownBody); got != want {
		t.Errorf("expired key's message = %q, unknown key's = %q; they must not be distinguishable", got, want)
	}
}

// TestEnvironmentKeyWithoutExpiryStillAuthenticates covers the rows migration
// 0021 grandfathered: a key minted before expiries existed has a NULL expires_at
// and was promised it would live until revoked. The migration deliberately does
// not backfill one, so the auth query must read NULL as "never expires" rather
// than as "expired" — the difference between an upgrade and an outage.
func TestEnvironmentKeyWithoutExpiryStillAuthenticates(t *testing.T) {
	s := newTestServer(t)
	envID := selfHostedEnv(t, s, "grandfathered")
	key := issueKey(t, s.pool, envID, "legacy-host")
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE environment_keys SET expires_at = NULL, name = '' WHERE environment_id = $1`, envID); err != nil {
		t.Fatalf("rewind the row to its pre-0021 shape: %v", err)
	}

	auth := map[string]string{"Authorization": "Bearer " + key}
	if res, raw := s.poll(t, envID, auth); res.StatusCode != http.StatusOK {
		t.Fatalf("a pre-0021 key stopped authenticating: status %d, body %q", res.StatusCode, raw)
	}
	keys, _, err := api.ListEnvironmentKeys(context.Background(), s.pool, envID, 100, 0)
	if err != nil {
		t.Fatalf("ListEnvironmentKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].ExpiresAt != nil {
		t.Fatalf("grandfathered key = %+v, want one row with a nil expiry", keys)
	}
}

// TestRevokeEnvironmentKeyIsIdempotentAndEnvironmentScoped pins the two
// properties the revoke endpoint's status codes rest on: a repeat call is a
// success (a retried revocation is not an error), and a key id is only revocable
// through the environment that owns it — so the operation can neither reach
// another environment's credential nor confirm that the id exists at all.
func TestRevokeEnvironmentKeyIsIdempotentAndEnvironmentScoped(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	envA := selfHostedEnv(t, s, "a")
	envB := selfHostedEnv(t, s, "b")
	key := issueKey(t, s.pool, envA, "host")
	keys, _, err := api.ListEnvironmentKeys(ctx, s.pool, envA, 100, 0)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListEnvironmentKeys = %v (err %v), want one key", keys, err)
	}
	id := keys[0].ID

	// Env B cannot revoke env A's key, and A's key keeps working.
	if found, err := api.RevokeEnvironmentKey(ctx, s.pool, envB, id); err != nil || found {
		t.Errorf("revoking A's key through B = %v, %v; want false, nil", found, err)
	}
	if res, _ := s.poll(t, envA, map[string]string{"Authorization": "Bearer " + key}); res.StatusCode != http.StatusOK {
		t.Error("a cross-environment revoke attempt disturbed the key")
	}
	// An id that names nothing is likewise not found.
	if found, err := api.RevokeEnvironmentKey(ctx, s.pool, envA, "envkey_0000000000000000000000"); err != nil || found {
		t.Errorf("revoking an unknown id = %v, %v; want false, nil", found, err)
	}

	var revokedAt time.Time
	for i := range 2 {
		found, err := api.RevokeEnvironmentKey(ctx, s.pool, envA, id)
		if err != nil || !found {
			t.Fatalf("revoke #%d = %v, %v; want true, nil", i+1, found, err)
		}
		var at time.Time
		if err := s.pool.QueryRow(ctx,
			`SELECT revoked_at FROM environment_keys WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatalf("read revoked_at: %v", err)
		}
		if i == 0 {
			revokedAt = at
		} else if !at.Equal(revokedAt) {
			t.Errorf("a repeated revoke slid revoked_at from %v to %v", revokedAt, at)
		}
	}
}

// TestListEnvironmentKeysPages pins the offset paging the console's listing
// envelope reports: the page is a window over the newest-first order and total
// counts every live key, not just the ones on this page.
func TestListEnvironmentKeysPages(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	envID := selfHostedEnv(t, s, "paged")
	for _, name := range []string{"one", "two", "three"} {
		issueKey(t, s.pool, envID, name)
	}
	// Spread the timestamps so the window below is a window over a *known* order
	// rather than over whatever three consecutive now() calls happened to produce.
	spreadCreatedAt(t, s, envID, "three", "two", "one")

	page, total, err := api.ListEnvironmentKeys(ctx, s.pool, envID, 2, 0)
	if err != nil {
		t.Fatalf("ListEnvironmentKeys: %v", err)
	}
	if total != 3 || len(page) != 2 {
		t.Fatalf("first page = %d keys of total %d, want 2 of 3", len(page), total)
	}
	if page[0].Name != "three" || page[1].Name != "two" {
		t.Errorf("first page = %q, %q; want the two newest, %q then %q",
			page[0].Name, page[1].Name, "three", "two")
	}
	rest, total, err := api.ListEnvironmentKeys(ctx, s.pool, envID, 2, 2)
	if err != nil {
		t.Fatalf("ListEnvironmentKeys (offset): %v", err)
	}
	if total != 3 || len(rest) != 1 {
		t.Fatalf("second page = %d keys of total %d, want 1 of 3", len(rest), total)
	}
	if rest[0].Name != "one" {
		t.Errorf("last page holds %q, want the oldest key %q", rest[0].Name, "one")
	}
	// An offset past the end is an empty page, not a wrong total.
	beyond, total, err := api.ListEnvironmentKeys(ctx, s.pool, envID, 2, 99)
	if err != nil || len(beyond) != 0 || total != 3 {
		t.Errorf("page past the end = %d keys of total %d (err %v), want 0 of 3", len(beyond), total, err)
	}
	// An environment with no keys lists an empty page, not a nil one.
	empty, total, err := api.ListEnvironmentKeys(ctx, s.pool, selfHostedEnv(t, s, "bare"), 100, 0)
	if err != nil || empty == nil || len(empty) != 0 || total != 0 {
		t.Errorf("empty environment = %#v of total %d (err %v), want an empty non-nil page", empty, total, err)
	}
}

// TestListEnvironmentKeysBreaksTimestampTiesByID pins the second half of the
// listing's ORDER BY, which spreading the timestamps elsewhere deliberately
// keeps out of the way. `created_at DESC` alone is a partial order: rows sharing
// a timestamp could come back in any order, and under offset paging an unstable
// order can show one key twice and skip another across two pages. `, id DESC`
// is what makes it total, and nothing exercised it — every other ordering test
// now has distinct timestamps by construction, so deleting the tiebreak left
// them all green.
func TestListEnvironmentKeysBreaksTimestampTiesByID(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	envID := selfHostedEnv(t, s, "ties")
	const keys = 8
	for i := range keys {
		issueKey(t, s.pool, envID, fmt.Sprintf("host-%d", i))
	}
	// One timestamp for every row: the tiebreak is the only thing left to order
	// them by.
	if _, err := s.pool.Exec(ctx,
		`UPDATE environment_keys SET created_at = now() WHERE environment_id = $1`, envID); err != nil {
		t.Fatalf("collapse created_at: %v", err)
	}

	page, _, err := api.ListEnvironmentKeys(ctx, s.pool, envID, 100, 0)
	if err != nil {
		t.Fatalf("ListEnvironmentKeys: %v", err)
	}
	if len(page) != keys {
		t.Fatalf("listed %d keys, want %d", len(page), keys)
	}
	for i := 1; i < len(page); i++ {
		if page[i-1].ID <= page[i].ID {
			ids := make([]string, len(page))
			for j, k := range page {
				ids[j] = k.ID
			}
			t.Fatalf("ids are not strictly descending at %d: %v", i, ids)
		}
	}
	// And the order is stable, which is the property offset paging actually
	// needs: a second call must not shuffle rows between pages.
	again, _, err := api.ListEnvironmentKeys(ctx, s.pool, envID, 100, 0)
	if err != nil {
		t.Fatalf("ListEnvironmentKeys (repeat): %v", err)
	}
	for i := range page {
		if again[i].ID != page[i].ID {
			t.Fatalf("repeated listing reordered tied rows at %d: %s then %s", i, page[i].ID, again[i].ID)
		}
	}
}

// TestEnvironmentKeyValueBindsToOneEnvironment: dropping environment_keys_one_live
// retired the *count* invariant, not the binding one. key_hash stays UNIQUE, so a
// single key value can never authenticate two environments — the property that
// keeps a leaked-and-replayed value confined to the queue it was issued for.
//
// Both halves are asserted, because they fail independently. The schema half is
// the UNIQUE constraint refusing the row. The auth half is what an attacker
// would actually attempt, and it is the one that matters: a value that somehow
// reached two rows must still authenticate only the environment it was issued
// for. Asserting the constraint alone would leave the escalation itself unpinned
// at the HTTP layer if a future change ever relaxed key_hash or added an
// ON CONFLICT path — which is how this assertion went missing in the first place
// (plan 30 slice 1 replaced the envauth_test.go test that carried it).
func TestEnvironmentKeyValueBindsToOneEnvironment(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	envA := selfHostedEnv(t, s, "a")
	envB := selfHostedEnv(t, s, "b")
	key := issueKey(t, s.pool, envA, "host")

	var hash string
	if err := s.pool.QueryRow(ctx,
		`SELECT key_hash FROM environment_keys WHERE environment_id = $1`, envA).Scan(&hash); err != nil {
		t.Fatalf("read A's key hash: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO environment_keys (id, environment_id, key_hash) VALUES ($1, $2, $3)`,
		domain.NewID(domain.PrefixEnvironmentKey).String(), envB, hash); err == nil {
		t.Error("one key value was accepted for two environments")
	}

	// The auth half, over real HTTP: the value still polls the environment it was
	// issued for, and is a 401 against the other — no cross-environment
	// escalation, whatever the storage layer did.
	auth := map[string]string{"Authorization": "Bearer " + key}
	if res, raw := s.poll(t, envA, auth); res.StatusCode != http.StatusOK {
		t.Errorf("the key stopped authenticating its own environment: status %d, body %q", res.StatusCode, raw)
	}
	res, raw := s.poll(t, envB, auth)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("A's key authenticated environment B (escalation): status %d, body %q", res.StatusCode, raw)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(raw), &body)
	wantErr(t, res.StatusCode, body, http.StatusUnauthorized, "authentication_error")
}

// TestSecondLiveEnvironmentKeyIsAccepted is the schema half of the model change,
// and the inverse of what 0013 pinned: several live rows per environment must now
// be representable, or per-host keys could not exist. The api_keys half of 0013 is
// untouched and still admits one live credential per name (keyrotation_test.go).
func TestSecondLiveEnvironmentKeyIsAccepted(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	envID := selfHostedEnv(t, s, "many")
	issueKey(t, s.pool, envID, "first")

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO environment_keys (id, environment_id, key_hash) VALUES ($1, $2, $3)`,
		"envkey_second", envID, "a-second-live-hash"); err != nil {
		t.Errorf("a second live environment key was rejected: %v", err)
	}
	var index string
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(max(indexname), '') FROM pg_indexes
		  WHERE tablename = 'environment_keys' AND indexname = 'environment_keys_one_live'`).Scan(&index); err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if index != "" {
		t.Error("environment_keys_one_live survived migration 0021")
	}
}
