package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type inferenceLookupCounter struct{ calls atomic.Int32 }

func (c *inferenceLookupCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.calls.Add(1)
	return ctx
}
func (*inferenceLookupCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDelegatedCandidatesUseOneLookup(t *testing.T) {
	ctx := t.Context()
	pool := newPoolWithKey(t)
	sid, eid := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, eid, sid, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil || item.InferenceToken == "" {
		t.Fatalf("claim: %v %v", item, err)
	}
	counter := &inferenceLookupCounter{}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = counter
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	h := api.NewHandler(traced, nil, nil, nil)
	r := httptest.NewRequest("GET", "/api/auth/config", nil)
	for i := range 128 {
		r.Header.Add("Authorization", fmt.Sprintf("Bearer itk_nonlive%d", i))
	}
	r.Header.Add("Authorization", "Bearer "+item.InferenceToken)
	rr := httptest.NewRecorder()
	counter.calls.Store(0)
	h.ServeHTTP(rr, r)
	if rr.Code != 403 {
		t.Fatalf("live trailing token: %d %s", rr.Code, rr.Body.String())
	}
	if n := counter.calls.Load(); n != 1 {
		t.Fatalf("one request with repeated credentials made %d database lookups, want 1", n)
	}
}

func TestDelegatedTokenCannotFallThroughToPrivateIAP(t *testing.T) {
	s := newLaneServerWith(t, func(c *identity.Config) {
		c.Mode = identity.ModeTrustedProxy
		c.AssertionHeader = "X-Goog-IAP-JWT-Assertion"
	})
	sid, eid := pgtest.NewSession(t, s.pool, "cloud")
	q := queue.New(s.pool)
	if _, err := q.Enqueue(t.Context(), s.pool, eid, sid, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(t.Context(), queue.ModelTurn, time.Minute)
	if err != nil || item == nil || item.InferenceToken == "" {
		t.Fatalf("claim: %v %v", item, err)
	}
	bearer := "Bearer " + item.InferenceToken
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    int
	}{
		{"bearer", http.Header{"Authorization": {bearer}}, 403},
		{"empty key", http.Header{"Authorization": {bearer}, "X-Api-Key": {""}}, 403},
		{"duplicate first", http.Header{"Authorization": {bearer, "Bearer itk_not_issued"}}, 403},
		{"duplicate last", http.Header{"Authorization": {"Bearer itk_not_issued", bearer}}, 403},
		{"selected key with duplicate bearers", http.Header{"X-Api-Key": {item.InferenceToken}, "Authorization": {"Bearer one", "Bearer two"}}, 403},
		{"legacy prefix", http.Header{"Authorization": {"Bearer itk_not_issued", "Bearer legacy"}}, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.headers.Set("X-Goog-IAP-JWT-Assertion", s.token("platform-admins"))
			before := ownedDurableSnapshot(t, s.pool)
			assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, tc.headers), tc.want)
			if tc.want == 403 && ownedDurableSnapshot(t, s.pool) != before {
				t.Fatal("delegated credential provisioned or refreshed a human")
			}
		})
	}
}

// Use the same verified human for the native cookie and private assertion, with
// no IdP mapping to undo the stored-role changes exercised below.
func privateAssertionServer(t *testing.T) (*nativeServer, http.Header, string) {
	t.Helper()
	s := newNativeServer(t, login.ModeTrustedProxy)
	h := nativeHeaders("", "")
	h.Set("X-Goog-IAP-JWT-Assertion", s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now())))
	res, raw := s.request("POST", "/api/auth/login", "", h)
	sessionReply(t, res, raw)
	var human string
	if err := s.pool.QueryRow(t.Context(), `UPDATE principals SET role='admin' WHERE issuer=$1 AND subject='user-1' RETURNING id`, s.idp.Issuer()).Scan(&human); err != nil {
		t.Fatal(err)
	}
	cookie, session := s.issue(human)
	h.Set("Cookie", cookie)
	h.Set("X-CSRF-Token", session.CSRFToken)
	assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, h), 200)
	return s, h, human
}

// An active-only lookup would let an expired or archived owned Bearer fall
// through to a valid assertion. Both credential kinds stay private-negative.
func TestOwnedKeyPrivateAssertionIsolation(t *testing.T) {
	s, headers, human := privateAssertionServer(t)
	for _, kind := range []string{"user", "service_account"} {
		owner := ownedPrincipalFixture(t, s.pool, kind, "admin")
		for _, state := range []string{"active", "inactive", "expired", "archived"} {
			raw, id := ownedKeyFixture(t, s.pool, owner, human)
			status := state
			if state == "expired" {
				status = "active"
			}
			if _, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET status=$2,
				expires_at=CASE WHEN $3 THEN clock_timestamp()-interval '1 second' END WHERE id=$1`, id, status, state == "expired"); err != nil {
				t.Fatal(err)
			}
			before := ownedDurableSnapshot(t, s.pool)
			for _, path := range []string{consoleAPIKeysPath, consoleAPIKey(id), consoleOrgAPIKey(id),
				"/api/console/organizations/default/members", "/api/console/organizations/default/service_accounts",
				"/api/organizations/default/proxy/v1/agents", "/api/unknown"} {
				for _, prefix := range []string{"/api/", "/%61pi/"} {
					path := strings.Replace(path, "/api/", prefix, 1)
					for _, method := range []string{"GET", "POST", "PUT"} {
						for _, header := range []string{"x-api-key", "Bearer"} {
							t.Run(kind+"/"+state+"/"+method+path+"/"+header, func(t *testing.T) {
								h := headers.Clone()
								for k, v := range ownedHeaders(header, raw) {
									h.Set(k, v)
								}
								res, body := s.request(method, path, `{"name":"must not persist","status":"inactive"}`, h)
								if res.StatusCode != 401 || laneMessage(t, string(body)) == "" {
									t.Fatalf("status=%d body=%s want credential refusal 401", res.StatusCode, body)
								}
							})
						}
					}
				}
			}
			if ownedDurableSnapshot(t, s.pool) != before {
				t.Errorf("%s/%s owned private refusal changed durable keys or principals", kind, state)
			}
		}
	}
}

func TestPrivateLegacyBearerSelection(t *testing.T) {
	s, headers, human := privateAssertionServer(t)
	legacy := api.IssuedKeyPrefix + "historical-companion"
	insertHistoricalKey(t, s.pool, "legacy companion", legacy)
	owned, _ := ownedKeyFixture(t, s.pool, human, human)
	for _, tc := range []struct {
		name      string
		xkeys     []string
		auth      []string
		assertion bool
		want      int
	}{
		{"legacy_companion", nil, []string{"Bearer " + legacy}, true, 401},
		{"single_empty_x", []string{""}, []string{"Bearer " + legacy}, true, 401},
		{"invalid_x", []string{"wrong"}, []string{"Bearer " + legacy}, true, 401},
		{"repeated_x", []string{"", legacy}, []string{"Bearer " + legacy}, true, 401},
		{"repeated_empty_x", []string{"", ""}, []string{"Bearer " + legacy}, true, 401},
		{"unknown_issued", nil, []string{"Bearer " + api.IssuedKeyPrefix + "unknown"}, true, 401},
		{"owned_cannot_borrow_same_issuer", nil, []string{"Bearer " + owned}, true, 401},
		{"repeated_legacy_owned", nil, []string{"Bearer " + legacy, "Bearer " + owned}, true, 401},
		{"repeated_owned_legacy", nil, []string{"Bearer " + owned, "Bearer " + legacy}, true, 401},
		{"repeated_legacy", nil, []string{"Bearer " + legacy, "Bearer " + legacy}, true, 401},
		{"cookie_cannot_replace_assertion", nil, []string{"Bearer " + legacy}, false, 401},
		{"selected_legacy_x_wins", []string{legacy}, []string{"Bearer " + owned}, true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := headers.Clone()
			if !tc.assertion {
				h.Del("X-Goog-IAP-JWT-Assertion")
			}
			for _, value := range tc.xkeys {
				h.Add("x-api-key", value)
			}
			for _, value := range tc.auth {
				h.Add("Authorization", value)
			}
			before := ownedDurableSnapshot(t, s.pool)
			assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, h), tc.want)
			if tc.want == 401 && ownedDurableSnapshot(t, s.pool) != before {
				t.Fatal("refused selected credential fell back to human sign-in")
			}
		})
	}
	t.Run("method_fallback", func(t *testing.T) {
		res, body := s.request("PUT", consoleAPIKeysPath, "", headers)
		if res.StatusCode != 405 {
			t.Fatalf("status=%d body=%s want method fallback 405", res.StatusCode, body)
		}
	})
	t.Run("cross_origin_assertion", func(t *testing.T) {
		h := headers.Clone()
		h.Set("Origin", "https://foreign.example")
		before := ownedDurableSnapshot(t, s.pool)
		res, body := s.request("POST", consoleAPIKeysPath, `{"name":"must not persist"}`, h)
		if res.StatusCode != 403 || laneMessage(t, string(body)) != "invalid browser origin or CSRF token" {
			t.Fatalf("status=%d body=%s want origin refusal", res.StatusCode, body)
		}
		if ownedDurableSnapshot(t, s.pool) != before {
			t.Fatal("cross-origin assertion changed durable state")
		}
	})
	t.Run("stored_role_is_selected", func(t *testing.T) {
		if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET role='user' WHERE id=$1`, human); err != nil {
			t.Fatal(err)
		}
		status, typ, body := laneRead(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, headers))
		if status != 403 || typ != "permission_error" || laneMessage(t, body) != "this route requires the developer role" {
			t.Fatalf("status=%d error=%q body=%s want current human role refusal", status, typ, body)
		}
	})
}

// Neither the raw nor the mux-cleaned public boundary may inherit the private
// legacy exception. Encoded dots and slashes remain literal route segments.
func TestPrivateLegacyBearerCanonicalization(t *testing.T) {
	s := newLaneServerWith(t, func(c *identity.Config) {
		c.Mode = identity.ModeTrustedProxy
		c.AssertionHeader = "X-Goog-IAP-JWT-Assertion"
	})
	legacy := api.IssuedKeyPrefix + "historical-companion"
	insertHistoricalKey(t, s.pool, "legacy companion", legacy)
	for _, tc := range []struct {
		name, path, location string
		want                 int
	}{
		{"private", consoleAPIKeysPath, "", 401},
		{"encoded_private", "/%61pi/console/organizations/default/workspaces/default/api_keys", "", 401},
		{"private_cleaning", "/other/.." + consoleAPIKeysPath, "", 401},
		{"private_repeated_slash", "/" + consoleAPIKeysPath, "", 401},
		{"unknown_private", "/api/unknown", "", 401},
		{"encoded_dot", "/%2e/v1/agents", "", 401},
		{"encoded_parent", "/api/%2e%2e/v1/agents", "", 401},
		{"encoded_slash", "/v1%2fapi/agents", "", 401},
		{"raw_public_to_private", "/v1/.." + consoleAPIKeysPath, "", 401},
		{"encoded_public_to_private", "/%761/.." + consoleAPIKeysPath, "", 401},
		{"private_to_public", "/api/../v1/agents", "", 401},
		{"private_to_encoded_public", "/api/../%761/agents", "", 401},
		{"public", "/v1/agents", "", 401},
		{"encoded_public", "/%761/agents", "", 401},
		{"public_repeated_slash", "//v1/agents", "", 401},
		{"dual_events", "/v1/sessions/sesn_nonexistent/events", "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := s.claims("platform-admins")
			claims["sub"] = tc.name // A rejected boundary must not provision this subject.
			headers := http.Header{"Authorization": {"Bearer " + legacy}, "X-Goog-IAP-JWT-Assertion": {s.idp.Mint(t, claims)}}
			before := ownedDurableSnapshot(t, s.pool)
			res := ownedFirstHop(t, s.url, tc.path, headers)
			if location := res.Header.Get("Location"); location != tc.location {
				t.Errorf("location=%q want=%q", location, tc.location)
			}
			assertOwnedResponse(t, res, tc.want)
			if tc.want == 401 && ownedDurableSnapshot(t, s.pool) != before {
				t.Fatal("public boundary provisioned or refreshed a human")
			}
			if tc.location != "" {
				assertOwnedResponse(t, ownedFirstHop(t, s.url, tc.location, headers), 401)
			}
		})
	}
}
