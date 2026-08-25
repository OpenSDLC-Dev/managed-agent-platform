package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/lib/environments"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
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
	// No brain runs here, so no turn ends on its own: an idle end_turn is
	// planted once the worker's stream is up (the stream tails from its open,
	// never replays), and the runner's idle watchdog (MaxIdle) then returns
	// the ErrIdleTimeout HandleItem tolerates, seconds in rather than at the
	// deadline — which still bounds the run if the plant lands too early.
	planted := time.AfterFunc(3*time.Second, func() {
		_, _ = events.NewLog(s.pool).Append(ctx, domain.ID(sessionID), []events.NewEvent{{
			Type: domain.EventSessionStatusIdle,
			Payload: []byte(`{"stop_reason":{"type":"end_turn"}}`)}})
	})
	defer planted.Stop()
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := w.HandleItem(runCtx, environments.HandleItemOptions{
		WorkID: workID, EnvironmentID: envID, SessionID: sessionID,
		EnvironmentKey: key, WorkSecret: secret.(string),
	}); err != nil {
		t.Fatalf("HandleItem with the sessions token: %v", err)
	}
	t.Logf("HandleItem returned after %s", time.Since(started).Round(time.Millisecond))
	var state string
	var beat *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT state, last_heartbeat FROM work_items WHERE id = $1`, workID).Scan(&state, &beat); err != nil {
		t.Fatal(err)
	}
	if state != "stopped" || beat == nil {
		t.Errorf("after the worker's run: state=%s heartbeat=%v; want stopped with a heartbeat recorded", state, beat)
	}
}
