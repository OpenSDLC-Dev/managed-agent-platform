package api

import (
	"net/http/httptest"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestNativeIssuanceStoreFailureDoesNotSignOut(t *testing.T) {
	pool := pgtest.NewPool(t)
	manager := login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: "https://console.example", Development: true})
	pool.Close()
	s := &server{pool: pool, login: manager}
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	w := httptest.NewRecorder()
	s.issueLogin(w, r, "principal_existing")
	if w.Code != 500 || w.Header().Get("x-platform-signed-out") != "" {
		t.Fatalf("real store failure was misclassified: status=%d marker=%q body=%s", w.Code, w.Header().Get("x-platform-signed-out"), w.Body.String())
	}
}
