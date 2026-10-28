package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

// Dropping a handler's cookie-context check must not be masked by middleware or
// requireRole: machine lanes deliberately bypass the old role guard.
func TestNativeHandlerRequiresCookieIdentity(t *testing.T) {
	s := &server{login: login.New(nil, login.Config{Mode: login.ModeOIDC, PublicOrigin: "https://console.example"})}
	for _, human := range []bool{false, true} {
		ctx := context.Background()
		if human {
			ctx = context.WithValue(ctx, ctxKeyIdentity, identityPrincipal{ID: "principal_machine", Role: identity.RoleAdmin})
		}
		r := httptest.NewRequest("GET", "/api/%61uth/session", nil).WithContext(ctx)
		_, err := s.authSession(r)
		var ae *apiError
		if !errors.As(err, &ae) || ae.status != 401 {
			t.Fatalf("session without cookie context=%v", err)
		}
		for _, handler := range []http.HandlerFunc{s.logout, s.redeemSetup} {
			r = httptest.NewRequest("POST", "/api/%61uth/logout", strings.NewReader(`{"code":"never-read"}`)).WithContext(ctx)
			w := httptest.NewRecorder()
			handler(w, r)
			if w.Code != 401 {
				t.Fatalf("non-cookie handler admission=%d %s", w.Code, w.Body.String())
			}
		}
	}
}

// These handlers must refuse machine and IdP contexts even if a future dispatch
// change accidentally reaches them. The old requireRole deliberately does not.
func TestMembersHandlersRequireCookieProvenance(t *testing.T) {
	s := &server{login: login.New(nil, login.Config{Mode: login.ModePassword, PublicOrigin: "https://console.example", Development: true})}
	for name, handler := range map[string]func(*http.Request) (any, error){
		"members-list": s.listMembers, "members-get": s.getMember,
		"members-create": s.createMember, "members-update": s.updateMember, "members-remove": s.removeMember,
		"sa-list": s.listServiceAccounts, "sa-get": s.getServiceAccount,
		"sa-create": s.createServiceAccount, "sa-update": s.updateServiceAccount, "sa-archive": s.archiveServiceAccount,
	} {
		t.Run(name, func(t *testing.T) {
			for _, idp := range []bool{false, true} {
				ctx := context.Background()
				if idp {
					ctx = context.WithValue(ctx, ctxKeyIdentity, identityPrincipal{ID: "principal_machine", Role: identity.RoleAdmin})
				}
				r := httptest.NewRequest("POST", "/api/console/organizations/default/members", strings.NewReader(`{}`)).WithContext(ctx)
				r.SetPathValue("org", "default")
				_, err := handler(r)
				var ae *apiError
				if !errors.As(err, &ae) || ae.status != 401 {
					t.Fatalf("handler admitted non-cookie context: %v", err)
				}
			}
		})
	}
}
