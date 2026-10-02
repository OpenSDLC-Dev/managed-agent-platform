package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// TestCredentialFromFailsClosed: who signed a request is read from the value
// the two environment lanes set, never inferred from an environment being in
// the context, so a lane that stores one for another reason reads as a
// management caller and is refused a user.tool_result (#662).
func TestCredentialFromFailsClosed(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyEnvironment, "env_elsewhere")
	if got := credentialFrom(ctx); got != events.ManagementCredential {
		t.Errorf("an environment alone reads as %v, want the management credential", got)
	}
	if got := credentialFrom(context.WithValue(ctx, ctxKeyCredential, events.EnvironmentCredential)); got != events.EnvironmentCredential {
		t.Errorf("a marked request reads as %v, want the environment credential", got)
	}
}

// TestTheManagementLaneFailsClosedWhenTheKeyLookupFails: the management lane's
// environment-key lookup runs for a request nothing has authenticated, so a
// failed lookup — a database down, a context gone — must not turn that request
// into a 500 and an ERROR line. It answers the missing-key 401 the lane gives
// without the lookup, and the failure goes to the operator's log at Warn.
func TestTheManagementLaneFailsClosedWhenTheKeyLookupFails(t *testing.T) {
	// Nothing listens on port 1, so every query fails at connect.
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=2")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mux := http.NewServeMux()
	reached := false
	mux.HandleFunc("GET /v1/agents", func(http.ResponseWriter, *http.Request) { reached = true })
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+environmentKeySecretPrefix+"looked-up-against-nothing")
	rec := httptest.NewRecorder()
	dispatchManagementAuth(pool, nil, mux).ServeHTTP(rec, req)

	if reached {
		t.Fatal("the handler was reached")
	}
	var body struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusUnauthorized || body.Error.Message != "x-api-key header is required" {
		t.Errorf("status %d, body %s; want the missing-key 401", rec.Code, rec.Body.String())
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "environment key lookup failed") ||
		strings.Contains(out, "level=ERROR") {
		t.Errorf("want the failed lookup logged at Warn and nothing at Error:\n%s", out)
	}
}
