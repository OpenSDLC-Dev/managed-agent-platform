package admin_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

// This write stores configuration only; it never calls the endpoint.
const issuedBearerProvider = `{"name":"mixed-credential","profile":"deepseek","endpoints":{"anthropic":"https://provider.example.test/anthropic"}}`

// Snapshot both sign-in effects (including profile, mapping and last_seen_at)
// and the management write. Never print the snapshot or offered credentials.
func issuedBearerState(t *testing.T, e *env) string {
	t.Helper()
	var state string
	if err := e.pool.QueryRow(context.Background(), `SELECT jsonb_build_array(
		(SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY id), '[]') FROM principals p),
		(SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY id), '[]') FROM modelgateway.providers p)
	)::text`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func issuedBearerKey(t *testing.T, e *env, kind, state string) string {
	t.Helper()
	ctx := context.Background()
	raw := "sk-map-api01-" + rand.Text()
	if kind == "unknown" {
		return raw
	}
	status := state
	if state == "expired" || state == "owner-deactivated" {
		status = "active"
	}
	var owner *string
	var legacyHash, ownedHash *string
	digest := apikey.Hash(raw)
	if kind == "unowned" {
		legacyHash = &digest
	} else {
		id := "principal_keyowner"
		if _, err := e.pool.Exec(ctx, `INSERT INTO principals (id,issuer,subject,kind,role)
			VALUES ($1,'key-owner','owner',$2,'admin')`, id, kind); err != nil {
			t.Fatal(err)
		}
		owner, ownedHash = &id, &digest
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO api_keys
		(id,name,key_hash,owned_key_hash,owner_principal_id,created_by,status,expires_at)
		VALUES ('key_companion','companion',$1,$2,$3,'apikey_issuer',$4,
			CASE WHEN $5 THEN now() - interval '1 hour' ELSE NULL END)`,
		legacyHash, ownedHash, owner, status, state == "expired"); err != nil {
		t.Fatal(err)
	}
	if state == "owner-deactivated" {
		if _, err := e.pool.Exec(ctx, `UPDATE principals SET status='deactivated' WHERE id=$1`, *owner); err != nil {
			t.Fatal(err)
		}
	}
	return raw
}

func TestGatewayAdminIssuedBearerAssertionRefusal(t *testing.T) {
	for _, kind := range []string{"user", "service_account", "unknown"} {
		states := []string{"active", "inactive", "expired", "archived", "owner-deactivated"}
		if kind == "unknown" {
			states = []string{"missing"}
		}
		for _, state := range states {
			t.Run(kind+"/"+state, func(t *testing.T) {
				e, _ := newCookieAdmin(t, login.ModeTrustedProxy, cookieOrigin, true)
				raw := issuedBearerKey(t, e, kind, state)
				h := http.Header{"Authorization": {"Bearer " + raw}, "X-Goog-Iap-Jwt-Assertion": {e.token("admins")}}
				for _, operator := range []string{"new-operator", "existing-operator"} {
					if operator == "existing-operator" {
						// The assertion would overwrite all of these on sign-in.
						if _, err := e.pool.Exec(context.Background(), `INSERT INTO principals
							(id,issuer,subject,role,role_source,email,display_name,last_seen_at)
							VALUES ('principal_operator',$1,'user-1','developer','console',
							'current@example.test','Current profile','2026-01-01T00:00:00Z')
							ON CONFLICT (issuer,subject) DO UPDATE SET role='developer',role_source='console',
							email='current@example.test',display_name='Current profile',last_seen_at='2026-01-01T00:00:00Z'`, e.idp.Issuer()); err != nil {
							t.Fatal(err)
						}
					}
					for _, method := range []string{"GET", "POST"} {
						t.Run(operator+"/"+method, func(t *testing.T) {
							before := issuedBearerState(t, e)
							path, body := "/admin/v1/profiles", ""
							if method == "POST" {
								path, body = "/admin/v1/providers", issuedBearerProvider
							}
							status, header, response := cookieAdminRequest(t, e, method, path, body, h)
							if status != http.StatusUnauthorized || !strings.Contains(response, `"authentication_error"`) {
								t.Errorf("issued Bearer plus assertion: HTTP %d, want 401 authentication_error", status)
							}
							if header.Get("x-platform-signed-out") != "" {
								t.Error("issued Bearer refusal was misreported as a cookie sign-out")
							}
							if issuedBearerState(t, e) != before {
								t.Error("refused issued Bearer changed principals or providers")
							}
						})
					}
				}
			})
		}
	}
}

func TestGatewayAdminIssuedBearerLegacyCompanion(t *testing.T) {
	for _, state := range []string{"active", "inactive", "expired", "archived", "assertion-only", "non-issued-bearer"} {
		t.Run(state, func(t *testing.T) {
			e, _ := newCookieAdmin(t, login.ModeTrustedProxy, cookieOrigin, true)
			h := make(http.Header)
			switch state {
			case "assertion-only":
			case "non-issued-bearer":
				h.Set("Authorization", "Bearer ordinary-legacy-companion")
			default:
				h.Set("Authorization", "Bearer "+issuedBearerKey(t, e, "unowned", state))
			}
			before := issuedBearerState(t, e)
			status, _, _ := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", h)
			if status != http.StatusUnauthorized || issuedBearerState(t, e) != before {
				t.Fatal("a legacy companion supplied authority without an assertion")
			}
			h.Set("X-Goog-IAP-JWT-Assertion", e.token("admins"))
			for _, method := range []string{"GET", "POST"} {
				status, _, _ := cookieAdminRequest(t, e, method, "/admin/v1/providers", issuedBearerProvider, h)
				if status != http.StatusOK {
					t.Errorf("legacy companion with admin assertion: %s HTTP %d, want 200", method, status)
				}
			}
			var principals, providers int
			if err := e.pool.QueryRow(context.Background(), `SELECT
				(SELECT count(*) FROM principals WHERE subject='user-1' AND role='admin'),
				(SELECT count(*) FROM modelgateway.providers)`).Scan(&principals, &providers); err != nil {
				t.Fatal(err)
			}
			if principals != 1 || providers != 1 {
				t.Error("legacy assertion did not sign in and persist the management write")
			}
			h.Set("X-Goog-IAP-JWT-Assertion", e.token("devs"))
			status, _, _ = cookieAdminRequest(t, e, "POST", "/admin/v1/providers", issuedBearerProvider, h)
			if status != http.StatusForbidden {
				t.Errorf("companion overrode developer assertion: HTTP %d, want 403", status)
			}
			before = issuedBearerState(t, e)
			h.Set("X-Goog-IAP-JWT-Assertion", "invalid")
			status, _, _ = cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", h)
			if status != http.StatusUnauthorized || issuedBearerState(t, e) != before {
				t.Error("legacy companion rescued an invalid assertion")
			}
			h.Set("X-Goog-IAP-JWT-Assertion", e.token("admins"))
			h.Set("Origin", "https://other.example")
			status, _, _ = cookieAdminRequest(t, e, "POST", "/admin/v1/providers", issuedBearerProvider, h)
			if status != http.StatusForbidden || issuedBearerState(t, e) != before {
				t.Error("legacy companion bypassed proxy browser-origin protection")
			}
		})
	}
}

func TestGatewayAdminIssuedBearerPrecedence(t *testing.T) {
	e, m := newCookieAdmin(t, login.ModeTrustedProxy, cookieOrigin, true)
	raw := issuedBearerKey(t, e, "user", "active")
	cookie, err := m.Issue(context.Background(), "principal_keyowner")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    int
	}{
		{"owned-with-cookie", nil, 401},
		{"selected-bootstrap-key", http.Header{"X-Api-Key": {bootstrap}}, 200},
		{"selected-invalid-key", http.Header{"X-Api-Key": {"invalid"}}, 401},
		{"selected-empty-key", http.Header{"X-Api-Key": {""}}, 401},
		{"repeated-key", http.Header{"X-Api-Key": {bootstrap, bootstrap}}, 401},
		{"repeated-authorization", http.Header{"Authorization": {"Bearer " + bootstrap, "Bearer " + raw}, "X-Api-Key": {bootstrap}}, 401},
		{"issued-looking-bootstrap-bearer", http.Header{"Authorization": {"Bearer " + bootstrap}}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := cookieAdminHeaders(cookie)
			h.Set("Authorization", "Bearer "+raw)
			h.Set("X-Goog-IAP-JWT-Assertion", e.token("admins"))
			for k, v := range tc.headers {
				h[k] = v
			}
			before := issuedBearerState(t, e)
			status, _, _ := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", h)
			if status != tc.want {
				t.Errorf("credential selection: HTTP %d, want %d", status, tc.want)
			}
			if issuedBearerState(t, e) != before {
				t.Error("bootstrap or refused credential fell through to operator sign-in")
			}
		})
	}
}

func TestGatewayAdminIssuedBearerLookupFailure(t *testing.T) {
	e, _ := newCookieAdmin(t, login.ModeTrustedProxy, cookieOrigin, true)
	raw := issuedBearerKey(t, e, "unowned", "active")
	if _, err := e.pool.Exec(context.Background(), `ALTER TABLE api_keys RENAME TO unavailable_api_keys`); err != nil {
		t.Fatal(err)
	}
	h := http.Header{"Authorization": {"Bearer " + raw}, "X-Goog-Iap-Jwt-Assertion": {e.token("admins")}}
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			before := issuedBearerState(t, e)
			status, header, response := cookieAdminRequest(t, e, method, "/admin/v1/providers", issuedBearerProvider, h)
			if status != http.StatusInternalServerError || !strings.Contains(response, `"api_error"`) || header.Get("x-platform-signed-out") != "" {
				t.Errorf("key-shape lookup failure: HTTP %d, want 500 api_error without sign-out", status)
			}
			if issuedBearerState(t, e) != before {
				t.Error("failed key-shape lookup changed principals or providers")
			}
		})
	}
	// Exact bootstrap selection must not depend on the key table at all.
	for _, headers := range []http.Header{{"X-Api-Key": {bootstrap}}, {"Authorization": {"Bearer " + bootstrap}}} {
		status, _, _ := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", headers)
		if status != http.StatusOK {
			t.Errorf("bootstrap required the key table: HTTP %d, want 200", status)
		}
	}
}
