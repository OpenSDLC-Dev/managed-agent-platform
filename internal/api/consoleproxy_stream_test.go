package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// privateStream uses a real connection. Returning headers alone is insufficient:
// callers also wait for live frames and for server-side SQL/listener cleanup.
func privateStream(t *testing.T, s *nativeServer, path, cookie string) (*sseStream, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", s.url+privateProxy+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("stream did not flush headers: %d %s", res.StatusCode, raw)
	}
	if res.Header.Get("Content-Length") != "" || res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("SSE headers=%v", res.Header)
	}
	st := &sseStream{cancel: cancel, frames: make(chan sseFrame, 64)}
	done := make(chan error, 1)
	go func() {
		defer res.Body.Close()
		defer close(st.frames)
		sc := bufio.NewScanner(res.Body)
		var name, data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data += strings.TrimPrefix(line, "data: ")
			case line == "":
				var obj map[string]any
				if err := json.Unmarshal([]byte(data), &obj); err != nil {
					done <- err
					return
				}
				select {
				case st.frames <- sseFrame{name: name, data: obj}:
				case <-ctx.Done():
					done <- ctx.Err()
					return
				}
				name, data = "", ""
			}
		}
		done <- sc.Err()
	}()
	return st, done
}

func observePrivateSQL(t *testing.T, s *nativeServer, ctx context.Context, wantWaiters int, wantListener bool) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiters int
		var listener bool
		err := s.pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_locks WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database())
			 AND relation='browser_sessions'::regclass AND mode='AccessShareLock' AND NOT granted),
			EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND query LIKE 'LISTEN %')`).Scan(&waiters, &listener)
		if err != nil {
			t.Fatal(err)
		}
		if waiters == wantWaiters && listener == wantListener {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("SQL state waiters=%d listener=%t; want=%d/%t", waiters, listener, wantWaiters, wantListener)
		case <-tick.C:
		}
	}
}

func blockPrivateSessions(t *testing.T, s *nativeServer, ctx context.Context) pgx.Tx {
	t.Helper()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `LOCK TABLE browser_sessions IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	return tx
}

// A recheck is not a fixed stream lifetime or an admin-only gate. Observe the
// next blocked query to prove a successful developer recheck kept both streams.
func TestPrivateProxyStreamsKeepDeveloperAuthority(t *testing.T) {
	s := newPrivateServer(t)
	caller := seedMember(t, s, "developer-stream-reader", "developer", "console")
	cookie, _ := s.issue(caller)
	sid := eventsFixture(t, s.tserver)
	child := insertChild(t, s.tserver, sid, "running")
	parent, parentDone := privateStream(t, s, "/sessions/"+sid+"/events/stream", cookie)
	childStream, childDone := privateStream(t, s, "/sessions/"+sid+"/threads/"+child+"/stream", cookie)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for range 2 {
		blocker := blockPrivateSessions(t, s, ctx)
		observePrivateSQL(t, s, ctx, 2, true)
		if err := blocker.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		observePrivateSQL(t, s, ctx, 0, true)
	}
	eventID := appendOn(t, s.tserver, sid, domain.ID(child), true, domain.EventAgentToolUse, allowBashCall)
	for name, stream := range map[string]*sseStream{"parent": parent, "child": childStream} {
		if f := stream.next(t); f.data["id"] != eventID {
			t.Fatalf("%s lost developer authority after successful recheck: %+v", name, f)
		}
		stream.close()
	}
	for _, done := range []<-chan error{parentDone, childDone} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("client cancellation did not finish stream")
		}
	}
	observePrivateSQL(t, s, ctx, 0, false)
}

// Removing cadence/deadline/current-role checks or losing Flusher/cancellation must fail these real streams.
func TestPrivateProxyStreamsAndRevocation(t *testing.T) {
	for _, withdrawal := range []string{"revoke", "expire", "deactivate", "demote", "store-error", "blocked-store", "client-cancel"} {
		t.Run(withdrawal, func(t *testing.T) {
			s := newPrivateServer(t)
			caller := seedMember(t, s, "stream-reader", "admin", "console")
			cookie, _ := s.issue(caller)
			sid := eventsFixture(t, s.tserver)
			child := insertChild(t, s.tserver, sid, "running")
			parent, parentDone := privateStream(t, s, "/sessions/"+sid+"/events/stream?event_deltas%5B%5D=agent.message", cookie)
			childStream, childDone := privateStream(t, s, "/sessions/"+sid+"/threads/"+child+"/stream", cookie)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			observePrivateSQL(t, s, ctx, 0, true)
			// Both connections must carry a genuinely live, flushed event, not a buffered success response.
			eventID := appendOn(t, s.tserver, sid, domain.ID(child), true, domain.EventAgentToolUse, allowBashCall)
			for name, stream := range map[string]*sseStream{"parent": parent, "child": childStream} {
				f := stream.next(t)
				if f.name != "agent.tool_use" || f.data["id"] != eventID {
					t.Fatalf("%s live frame=%+v; want %s", name, f, eventID)
				}
			}
			var before int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id=$1`, sid).Scan(&before); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			var blocker pgx.Tx
			switch withdrawal {
			case "blocked-store", "client-cancel":
				blocker = blockPrivateSessions(t, s, ctx)
				// Observe BOTH watcher queries blocked in PostgreSQL before asking for cancellation.
				observePrivateSQL(t, s, ctx, 2, true)
				if withdrawal == "client-cancel" {
					parent.close()
					childStream.close()
				}
			case "store-error":
				if _, err := s.pool.Exec(ctx, `ALTER TABLE browser_sessions RENAME TO unavailable_sessions`); err != nil {
					t.Fatal(err)
				}
			default:
				query := map[string]string{
					"revoke":     `DELETE FROM browser_sessions WHERE principal_id=$1`,
					"expire":     `UPDATE browser_sessions SET created_at=clock_timestamp()-interval '13 hours',expires_at=clock_timestamp()-interval '1 second' WHERE principal_id=$1`,
					"deactivate": `UPDATE principals SET status='deactivated' WHERE id=$1`,
					"demote":     `UPDATE principals SET role='user' WHERE id=$1`,
				}[withdrawal]
				if _, err := s.pool.Exec(ctx, query, caller); err != nil {
					t.Fatal(err)
				}
			}
			for name, done := range map[string]<-chan error{"parent": parentDone, "child": childDone} {
				select {
				case err := <-done:
					// Withdrawal may abort a blocked write and truncate HTTP's final
					// chunk. A client deadline is not evidence of server withdrawal.
					if withdrawal != "client-cancel" && err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("%s stream did not withdraw: %v", name, err)
					}
				case <-ctx.Done():
					t.Fatalf("%s stream outlived 15s withdrawal budget (%s)", name, time.Since(started))
				}
			}
			elapsed := time.Since(started)
			if elapsed >= 15*time.Second {
				t.Fatalf("withdrawal took %s", elapsed)
			}
			t.Logf("both flushed streams withdrew in %s", elapsed)
			for name, stream := range map[string]*sseStream{"parent": parent, "child": childStream} {
				for f := range stream.frames {
					if f.name != "ping" {
						t.Errorf("%s invented a frame on authority loss: %+v", name, f)
					}
				}
			}
			// Keep the exclusive lock held until the timed-out/cancelled lookups and listener are gone.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cleanupCancel()
			if withdrawal == "store-error" {
				if _, err := s.pool.Exec(cleanupCtx, `ALTER TABLE unavailable_sessions RENAME TO browser_sessions`); err != nil {
					t.Fatal(err)
				}
			}
			observePrivateSQL(t, s, cleanupCtx, 0, false)
			if blocker != nil {
				if err := blocker.Rollback(cleanupCtx); err != nil {
					t.Fatal(err)
				}
			}
			var after int
			if err := s.pool.QueryRow(cleanupCtx, `SELECT count(*) FROM events WHERE session_id=$1`, sid).Scan(&after); err != nil || after != before {
				t.Fatalf("withdrawal changed resource events: before=%d after=%d err=%v", before, after, err)
			}
			want, marker := 200, ""
			switch withdrawal {
			case "revoke", "expire":
				want, marker = 401, "1"
			case "deactivate":
				want, marker = 403, "1"
			case "demote":
				want = 403
			}
			res, raw := s.request("GET", privateProxy+"/agents", "", nativeHeaders(cookie, ""))
			if res.StatusCode != want || res.Header.Get("x-platform-signed-out") != marker {
				t.Fatalf("subsequent admission=%d %s marker=%q want=%d/%q", res.StatusCode, raw, res.Header.Get("x-platform-signed-out"), want, marker)
			}
		})
	}
}
