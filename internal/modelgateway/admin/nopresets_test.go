package admin_test

import (
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
)

func TestAdminStartsWithoutPresetCredentials(t *testing.T) {
	e, manager := newCookieAdmin(t, login.ModePassword, cookieOrigin, false)
	_, err := admin.New(admin.Config{Store: e.store, Cipher: e.cipher, Login: manager, Prober: e.prober})
	if err != nil {
		t.Fatalf("cookie admin cannot start without a preset: %v", err)
	}
}

func TestRetiredPolicyRoutesAreAbsent(t *testing.T) {
	e := newEnv(t, false)
	for _, path := range []string{"/admin/v1/key_policies", "/admin/v1/key_policies/apikey_retired"} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			r := e.admin(method, path, nil)
			if r.status != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404: %s", method, path, r.status, r.raw)
			}
		}
	}
}

func TestAdminRequiresCookieOnEveryRoute(t *testing.T) {
	e, manager := newCookieAdmin(t, login.ModeTrustedProxy, cookieOrigin, true)
	principalID := domain.NewID(domain.PrefixPrincipal).String()
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO principals(id,issuer,subject,kind,role)
		VALUES ($1,'cookie-fixture','admin','user','admin')`, principalID); err != nil {
		t.Fatal(err)
	}
	issued, err := manager.Issue(t.Context(), principalID)
	if err != nil {
		t.Fatal(err)
	}
	token := e.token("admins")
	for _, path := range []string{"/admin/v1/profiles", "/admin/v1/aliases", "/admin/v1/rate_limits"} {
		for name, headers := range map[string]http.Header{
			"preset-header":   {"X-Api-Key": {bootstrap}},
			"preset-bearer":   {"Authorization": {"Bearer " + bootstrap}},
			"idp-bearer":      {"Authorization": {"Bearer " + token}},
			"proxy-assertion": {"X-Goog-IAP-JWT-Assertion": {token}},
			"mixed-cookie":    {"Cookie": {"map_session=" + issued.Token}, "X-Api-Key": {bootstrap}},
		} {
			t.Run(name+path, func(t *testing.T) {
				status, _, raw := cookieAdminRequest(t, e, "GET", path, "", headers)
				if status != http.StatusUnauthorized {
					t.Fatalf("non-cookie admission=%d body=%s", status, raw)
				}
			})
		}
		status, _, raw := cookieAdminRequest(t, e, "GET", path, "", cookieAdminHeaders(issued))
		if status != http.StatusOK {
			t.Fatalf("cookie control=%d body=%s", status, raw)
		}
	}
}
