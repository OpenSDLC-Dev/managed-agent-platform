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

// awaitKick waits for one notification on the kick channel, and holds the
// producer to the empty payload the consumer's design depends on: a session id
// here would be a fact the reaper must not act on, so publishing one would be
// an invitation to read it.
func awaitKick(t *testing.T, conn *pgx.Conn, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := conn.WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("%s published no reap kick: %v", what, err)
	}
	if n.Payload != "" {
		t.Fatalf("%s published the kick with payload %q; it carries none", what, n.Payload)
	}
}

// TestSessionEndKicksTheReaper: ending a session publishes the wake its
// sandbox's teardown rides on (#354), so the container goes in seconds rather
// than at the executor's next interval. Both endings, because an operator who
// archives rather than deletes was watching the same container linger.
func TestSessionEndKicksTheReaper(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   func(sid string) string
	}{
		{"delete", http.MethodDelete, func(sid string) string { return "/v1/sessions/" + sid }},
		{"archive", http.MethodPost, func(sid string) string { return "/v1/sessions/" + sid + "/archive" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			agentID, envID := fixture(t, s)
			sid := createSession(t, s, map[string]any{
				"agent": agentID, "environment_id": envID})["id"].(string)

			// Listening before the request, not after: the notification is not
			// queued for a connection that was not listening when it fired.
			conn := listenReapKick(t, s.pool)

			if status, res := s.do(tc.method, tc.path(sid), nil); status != http.StatusOK {
				t.Fatalf("%s: %d %v", tc.name, status, res)
			}
			awaitKick(t, conn, tc.name)
		})
	}
}

// TestRearchivingKicksNothing: archiving is idempotent, and the second call
// ends nothing — the stamp it would set is already there. A kick for it would
// have every listening executor sweep everything it owns for a request that
// changed no row, which a client polling the endpoint turns into a standing
// load. The refusal rung below cannot cover this one: this request succeeds.
func TestRearchivingKicksNothing(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil); status != http.StatusOK {
		t.Fatalf("first archive: %d %v", status, res)
	}

	// Listening only now, so the first archive's kick — which is owed and was
	// published — is not the notification this rung could mistake for a second.
	conn := listenReapKick(t, s.pool)
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil); status != http.StatusOK {
		t.Fatalf("second archive: %d %v", status, res)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if n, err := conn.WaitForNotification(ctx); err == nil {
		t.Fatalf("re-archiving published a reap kick: %+v", n)
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
