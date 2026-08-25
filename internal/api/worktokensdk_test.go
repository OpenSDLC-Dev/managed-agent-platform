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
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// No brain runs here, so no turn ends on its own: an idle end_turn is
	// planted every three seconds until the worker returns — the stream tails
	// from its open and never replays, so only a plant after it is up counts —
	// and the runner's idle watchdog (MaxIdle) then returns the ErrIdleTimeout
	// HandleItem tolerates. The idle clock arms only from a streamed event, so
	// a plant cannot mask a token failure on the stream.
	planted := make(chan struct{})
	var plantErr error
	go func() {
		defer close(planted)
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
			}
			plantCtx, done := context.WithTimeout(ctx, 5*time.Second)
			_, err := events.NewLog(s.pool).Append(plantCtx, domain.ID(sessionID), []events.NewEvent{{
				Type:    domain.EventSessionStatusIdle,
				Payload: []byte(`{"stop_reason":{"type":"end_turn"}}`)}})
			done()
			if err != nil {
				plantErr = err
				cancel() // no idle event will come; end the run now, not at the deadline
				return
			}
		}
	}()
	started := time.Now()
	err := w.HandleItem(runCtx, environments.HandleItemOptions{
		WorkID: workID, EnvironmentID: envID, SessionID: sessionID,
		EnvironmentKey: key, WorkSecret: secret.(string),
	})
	took := time.Since(started)
	cancel()
	<-planted
	if plantErr != nil {
		t.Fatalf("planting the idle event: %v", plantErr)
	}
	if err != nil {
		t.Fatalf("HandleItem with the sessions token: %v", err)
	}
	t.Logf("HandleItem returned after %s (the run context's deadline is 30 s; the plant ends it in seconds)", took.Round(time.Millisecond))
	var state string
	var beat *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT state, last_heartbeat FROM work_items WHERE id = $1`, workID).Scan(&state, &beat); err != nil {
		t.Fatal(err)
	}
	if state != "stopped" || beat == nil {
		t.Errorf("after the worker's run: state=%s heartbeat=%v; want stopped with a heartbeat recorded", state, beat)
	}
}
