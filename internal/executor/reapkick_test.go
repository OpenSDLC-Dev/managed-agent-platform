package executor

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// awaitReap polls the fake provider until it has reaped sid. The poll surface
// is the only race-safe one — Run's reap goroutine appends concurrently — and
// the deadline is generous because what the caller is really asserting is the
// difference between seconds and an hour, not a tight bound.
func awaitReap(t *testing.T, h *harness, sid domain.ID, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if slices.Contains(h.prov.reapedSnapshot(), sid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s was never reaped", what, sid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunReapsOnAKickRatherThanTheInterval: a session that ends publishes a
// wake, and the reaper sweeps on it instead of waiting for its tick (#354).
// The interval is an hour, so every reap this test observes is one the ticker
// cannot account for.
//
// Two sessions, because the listener sweeps once whenever it establishes — it
// has to, since LISTEN delivers only to connections already listening and a
// kick fired while it was down was queued nowhere. That startup sweep would
// reap a deleted session all by itself, so it cannot be the thing under test.
// The first session is its target and its barrier: seeing it reaped is how the
// test knows the LISTEN is covering. Only then does the second session end and
// a kick go out, and only that second reap can be attributed to the kick.
func TestRunReapsOnAKickRatherThanTheInterval(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: time.Hour})
	h.prov = h.exec.provider.(*fakeProvider)
	h.exec.cfg.ReapKickDSN = h.pool.Config().ConnString()

	// The barrier session is deleted before Run starts; the kicked session is
	// alive, so the startup sweep classifies it as nothing to do. Both are
	// owned from the start, so the fixture is never written while the reap
	// goroutine is reading it.
	barrier := h.sid
	kicked := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	deleteSessionRowByID(t, h, barrier)
	h.prov.owned = []domain.ID{barrier, kicked}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = h.exec.Run(ctx); close(runDone) }()
	defer func() { cancel(); <-runDone }()

	awaitReap(t, h, barrier, "the sweep the listener runs when it establishes")
	if got := h.prov.reapedSnapshot(); slices.Contains(got, kicked) {
		t.Fatalf("the live session was reaped by the startup sweep: %v", got)
	}

	deleteSessionRowByID(t, h, kicked)
	if err := events.NotifyReapKick(ctx, h.pool); err != nil {
		t.Fatalf("publish the kick: %v", err)
	}
	awaitReap(t, h, kicked, "the kick")
}

// TestReapKickWithoutADSNStillReapsOnTheInterval: the listener is optional —
// a deployment that cannot spare the connection sets no DSN and gets the
// teardown latency it had before the kick, not a broken reaper. The same rung
// covers the listener that never establishes, since both leave the ticker as
// the only wake.
func TestReapKickWithoutADSNStillReapsOnTheInterval(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: 20 * time.Millisecond})
	h.prov = h.exec.provider.(*fakeProvider)
	if h.exec.cfg.ReapKickDSN != "" {
		t.Fatal("the harness set a kick DSN; this rung needs none")
	}
	deleteSessionRow(t, h)
	h.prov.owned = []domain.ID{h.sid}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = h.exec.Run(ctx); close(runDone) }()
	defer func() { cancel(); <-runDone }()

	awaitReap(t, h, h.sid, "the interval, with no listener")
}

// shortenReapKickBackoff spends the reconnect pause in test time. The value is
// a package var and the executor suite does not run its tests in parallel, the
// same terms reapHookAfterClassify is used on.
func shortenReapKickBackoff(t *testing.T) {
	t.Helper()
	prev := reapKickBackoff
	reapKickBackoff = 20 * time.Millisecond
	t.Cleanup(func() { reapKickBackoff = prev })
}

// killListener ends the backend holding the kick's LISTEN, the way a failover
// or an idle-connection reaper would. It finds it by the statement it last
// ran, which for that connection is the LISTEN itself and nothing since.
func killListener(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var pid int
		err := h.pool.QueryRow(ctx,
			`SELECT pid FROM pg_stat_activity
			  WHERE datname = current_database() AND pid <> pg_backend_pid()
			    AND query = $1`, "LISTEN "+events.ChannelReapKick).Scan(&pid)
		if err == nil {
			if _, err := h.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
				t.Fatalf("terminate the listening backend: %v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend is holding %s: %v", events.ChannelReapKick, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReapKickListenerRecoversFromALostConnection: the listener is not allowed
// to give up. Its connection dies the way a failover kills one, and the reaper
// must both catch up on what it missed and go on hearing new kicks — the first
// because LISTEN queues nothing for a connection that is gone, so a wake fired
// during the outage is simply not there to collect, and the sweep on every
// establish is what stands in for it.
//
// The interval is an hour throughout, so nothing here can be the ticker.
func TestReapKickListenerRecoversFromALostConnection(t *testing.T) {
	shortenReapKickBackoff(t)
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: time.Hour})
	h.prov = h.exec.provider.(*fakeProvider)
	h.exec.cfg.ReapKickDSN = h.pool.Config().ConnString()

	barrier := h.sid
	duringOutage := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	afterRecovery := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	deleteSessionRowByID(t, h, barrier)
	h.prov.owned = []domain.ID{barrier, duringOutage, afterRecovery}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = h.exec.Run(ctx); close(runDone) }()
	defer func() { cancel(); <-runDone }()

	awaitReap(t, h, barrier, "the sweep the listener runs when it establishes")

	// The outage. Ending a session while it lasts publishes into nothing, so
	// only the sweep the reconnect runs can account for this reap.
	killListener(t, h)
	deleteSessionRowByID(t, h, duringOutage)
	awaitReap(t, h, duringOutage, "the sweep after reconnecting")

	// And it is still listening afterwards. The payload is deliberately not
	// the empty one the producer sends: the listener reads no payload at all,
	// and a future reader of one would fail here rather than in production.
	deleteSessionRowByID(t, h, afterRecovery)
	if _, err := h.pool.Exec(ctx, `SELECT pg_notify($1, $2)`,
		events.ChannelReapKick, "not-a-payload-anyone-reads"); err != nil {
		t.Fatalf("publish the kick: %v", err)
	}
	awaitReap(t, h, afterRecovery, "a kick delivered after the reconnect")
}

// TestReapKickListenerSurvivesAnUnusableDSN: a DSN that never connects must
// cost teardown latency and nothing else. Two things are asserted, and the
// second is the one worth having — that the interval still reaps is true even
// of a listener that gave up, so on its own it would pin nothing. Shutdown is
// the real guard: a retry pause that waits out its backoff instead of the
// context would hold Run open behind a connection that is never coming, and
// this is where that shows.
func TestReapKickListenerSurvivesAnUnusableDSN(t *testing.T) {
	// Deliberately not shortened: the point is that a pause far longer than
	// the test's patience still yields to the cancellation.
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: 20 * time.Millisecond})
	h.prov = h.exec.provider.(*fakeProvider)
	h.exec.cfg.ReapKickDSN = "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1"
	deleteSessionRow(t, h)
	h.prov.owned = []domain.ID{h.sid}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = h.exec.Run(ctx); close(runDone) }()

	awaitReap(t, h, h.sid, "the interval, with a listener that cannot connect")

	// The deadline has to be shorter than the backoff, or a pause that waits
	// the backoff out instead of yielding to the cancellation still lands
	// inside it and the rung proves nothing. Returning takes microseconds, so
	// the margin is in the right place.
	cancel()
	select {
	case <-runDone:
	case <-time.After(reapKickBackoff / 2):
		t.Fatalf("Run did not return within %s while the listener was retrying every %s: "+
			"the retry pause is waiting out its backoff rather than the context",
			reapKickBackoff/2, reapKickBackoff)
	}
}

// TestReapKicksCoalesce: the wake channel holds one request, so a burst of
// endings costs the sweep that sees all of them rather than one sweep each.
// Asserted on the channel itself because the alternative — counting passes
// under a live Run — races the sweep it is counting.
func TestReapKicksCoalesce(t *testing.T) {
	e := &Executor{kick: make(chan struct{}, 1)}
	for range 5 {
		e.wake()
	}
	if n := len(e.kick); n != 1 {
		t.Fatalf("five wakes queued %d sweeps, want 1", n)
	}
	<-e.kick
	// And a wake after the sweep took the request arms the next one: coalescing
	// must not swallow an ending that arrived while the previous sweep ran.
	e.wake()
	if n := len(e.kick); n != 1 {
		t.Fatalf("a wake after the sweep queued %d, want 1", n)
	}
}
