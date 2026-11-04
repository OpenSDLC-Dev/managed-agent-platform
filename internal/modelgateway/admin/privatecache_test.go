package admin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

func TestGatewayPrivateResponsesForbidSharedCache(t *testing.T) {
	e, m := newCookieAdmin(t, login.ModeOIDC, cookieOrigin, true)
	e.ok(e.do("GET", "/admin/v1/profiles", nil, map[string]string{"Authorization": "Bearer " + e.token("devs")}), "seed")
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM principals`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	issued, err := m.Issue(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	status, header, raw := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", cookieAdminHeaders(issued))
	if status != 200 {
		t.Fatalf("developer=%d %s", status, raw)
	}
	policy := strings.ToLower(strings.Join(header.Values("Cache-Control"), ","))
	if !strings.Contains(policy, "private") || !strings.Contains(policy, "no-store") {
		t.Errorf("gateway private policy=%q", policy)
	}
	stored := !strings.Contains(policy, "private") && !strings.Contains(policy, "no-store")
	for _, caller := range []string{"anonymous", "user", "revoked"} {
		h := cookieAdminHeaders(issued)
		want := 401
		switch caller {
		case "anonymous":
			h.Del("Cookie")
		case "user":
			e.setPrincipal(`role='user'`)
			want = 403
		case "revoked":
			e.setPrincipal(`role='developer'`)
			if _, err := e.pool.Exec(context.Background(), `DELETE FROM browser_sessions`); err != nil {
				t.Fatal(err)
			}
		}
		next := 200
		if !stored {
			next, _, _ = cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", h)
		}
		if next != want {
			t.Errorf("heuristic shared cache served developer response to %s: %d want %d", caller, next, want)
		}
	}
}
