package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// listenReapKick holds a LISTEN on the kick channel from a connection of its
// own. Not through the broker on purpose: the broker LISTENs on three named
// channels and its dispatch has no default arm, so a kick would reach it
// neither as a delivery nor as an error — an assertion built on it would pass
// whether or not the producer ever fired.
//
// Exec returns only once the server has processed the LISTEN, so everything
// the caller does afterwards is covered.
func listenReapKick(t *testing.T, pool *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the listening connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "LISTEN "+events.ChannelReapKick); err != nil {
		t.Fatalf("LISTEN %s: %v", events.ChannelReapKick, err)
	}
	return conn
}

// awaitKick waits for one notification on the kick channel.
func awaitKick(t *testing.T, conn *pgx.Conn, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.WaitForNotification(ctx); err != nil {
		t.Fatalf("%s published no reap kick: %v", what, err)
	}
}

// TestSessionEndKicksTheReaper: ending a session publishes the wake its
// sandbox's teardown rides on (#354), so the container goes in seconds rather
// than at the executor's next interval. Both endings, because an operator who
// archives rather than deletes was watching the same container linger.
func TestSessionEndKicksTheReaper(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(sid string) string
	}{
		{"delete", func(sid string) string { return "/v1/sessions/" + sid }},
		{"archive", func(sid string) string { return "/v1/sessions/" + sid + "/archive" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			agentID, envID := fixture(t, s)
			sid := createSession(t, s, map[string]any{
				"agent": agentID, "environment_id": envID})["id"].(string)

			// Listening before the request, not after: the notification is not
			// queued for a connection that was not listening when it fired.
			conn := listenReapKick(t, s.pool)

			method := http.MethodDelete
			if tc.name == "archive" {
				method = http.MethodPost
			}
			if status, res := s.do(method, tc.path(sid), nil); status != http.StatusOK {
				t.Fatalf("%s: %d %v", tc.name, status, res)
			}
			awaitKick(t, conn, tc.name)
		})
	}
}

// TestRefusedSessionEndKicksNothing: the kick reports that a session ended, so
// a request that ends nothing must publish none. Without this rung the
// producer could fire unconditionally and every other rung still passes.
func TestRefusedSessionEndKicksNothing(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'running' WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	conn := listenReapKick(t, s.pool)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodDelete, "/v1/sessions/" + sid},
		{http.MethodPost, "/v1/sessions/" + sid + "/archive"},
		{http.MethodDelete, "/v1/sessions/sesn_missing"},
	} {
		if status, _ := s.do(tc.method, tc.path, nil); status == http.StatusOK {
			t.Fatalf("%s %s = 200; this rung needs a refusal", tc.method, tc.path)
		}
	}

	// A kick that a successful end publishes would already be waiting here, so
	// the wait only has to outlast the round trips above, not a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if n, err := conn.WaitForNotification(ctx); err == nil {
		t.Fatalf("a refused request published a reap kick: %+v", n)
	}
}
