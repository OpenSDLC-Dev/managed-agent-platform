package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/lib/environments"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestSDKWorkerServesAnItemWithTheSessionsToken drives the v1.66.0 reference
// worker's per-item flow (EnvironmentWorker.HandleItem — the secret decoded,
// the session read, the events stream, the lease heartbeat, the force-stop)
// against the in-process server with the environment key revoked first, so
// every call it makes rides the sessions token or fails. Memory sync is
// switched off (a negative interval, the SDK's documented exception) — the
// worker's download is slice 6's.
func TestSDKWorkerServesAnItemWithTheSessionsToken(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	_, envID, sessionID, _, key := storeWorker(t, s, "sdk")
	workID, secret, token := pollItem(t, s, envID, key)
	if token == "" {
		t.Fatal("no sessions token on the poll")
	}
	// The poller's ack, with the environment key, precedes the per-item flow.
	if st := status(t, s, http.MethodPost, "/v1/environments/"+envID+"/work/"+workID+"/ack", nil, asBearer(key)); st != http.StatusOK {
		t.Fatalf("ack = %d", st)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE environment_keys SET revoked_at = now() WHERE environment_id = $1`, envID); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, http.MethodGet, "/v1/sessions/"+sessionID, nil, asBearer(key)); st != http.StatusUnauthorized {
		t.Fatalf("the revoked environment key still authenticates (%d)", st)
	}

	client := anthropic.NewClient(option.WithBaseURL(s.url+"/"), option.WithAPIKey("not-a-key"))
	idle := 2 * time.Second
	w := environments.NewEnvironmentWorker(client, environments.EnvironmentWorkerOptions{
		EnvironmentID:      envID,
		EnvironmentKey:     key,
		Workdir:            t.TempDir(),
		MaxIdle:            &idle,
		MemorySyncInterval: -1,
	})
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := w.HandleItem(runCtx, environments.HandleItemOptions{
		WorkID: workID, EnvironmentID: envID, SessionID: sessionID,
		EnvironmentKey: key, WorkSecret: secret.(string),
	}); err != nil {
		t.Fatalf("HandleItem with the sessions token: %v", err)
	}
	var state string
	var beat *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT state, last_heartbeat FROM work_items WHERE id = $1`, workID).Scan(&state, &beat); err != nil {
		t.Fatal(err)
	}
	if state != "stopped" || beat == nil {
		t.Errorf("after the worker's run: state=%s heartbeat=%v; want stopped with a heartbeat recorded", state, beat)
	}
}
