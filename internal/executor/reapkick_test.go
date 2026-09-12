package executor

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// ownSession puts one more session into the provider's holding under the
// mutex, and is how these tests attribute a reap to the wake that caused it.
// A pass lists Owned once and then iterates that answer (reapPass), so a
// session added after a pass began cannot be reaped by it — which makes the
// session the test adds here reachable only by the next wake, and there is
// only one of those with the interval set to an hour.
func ownSession(t *testing.T, h *harness, sid domain.ID) {
	t.Helper()
	h.prov.mu.Lock()
	defer h.prov.mu.Unlock()
	h.prov.owned = append(h.prov.owned, sid)
}

// runExecutor starts Run and returns a stop function that cancels it and waits
// — bounded, because a Run that will not return is the failure some of these
// rungs exist to catch, and a cleanup that waits forever for it would hang the
// binary rather than fail the test.
func runExecutor(t *testing.T, h *harness) (context.Context, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.exec.Run(ctx); close(done) }()
	return ctx, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
	}
}

// kickConn is the listener's connection config, taken the way production takes
// it: from the pool that is already open, whose parse has consumed whatever
// pool_* options the DSN carried.
func kickConn(t *testing.T, h *harness) *pgx.ConnConfig {
	t.Helper()
	return h.pool.Config().ConnConfig.Copy()
}

// TestRunReapsOnAKickRatherThanTheInterval: a session that ends publishes a
// wake, and the reaper sweeps on it instead of waiting for its tick (#354).
// The interval is an hour, so every reap this test observes is one the ticker
// cannot account for.
//
// Two sessions, because a sweep runs at startup whatever the kick does: the
// loop passes before its first wait (#709), and the listener sweeps again
// whenever it establishes — it has to, since LISTEN delivers only to
// connections already listening and a kick fired while it was down was queued
// nowhere. Either sweep would reap a deleted session all by itself, so neither
// can be the thing under test.
// The first session is its target and its barrier: seeing it reaped is how the
// test knows the LISTEN is covering. The second is owned only afterwards, so
// the startup sweep — which listed its holding before that — can never be what
// reaps it, however long it is still running.
func TestRunReapsOnAKickRatherThanTheInterval(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: time.Hour})
	h.prov = h.exec.provider.(*fakeProvider)
	h.exec.cfg.ReapKickConn = kickConn(t, h)

	barrier := h.sid
	kicked := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	deleteSessionRowByID(t, h, barrier)
	h.prov.owned = []domain.ID{barrier}

	ctx, stop := runExecutor(t, h)
	defer stop()

	awaitReap(t, h, barrier, "the startup sweeps")
	// Both startup sweeps have to be spent before the subject exists, or one of
	// them reaps it and the notification proves nothing: the loop's own boot
	// pass is one, the listener's establish is the other. And the LISTEN has to
	// be up before the NOTIFY, which is delivered only to connections already
	// listening.
	awaitListening(t, h)
	awaitPasses(t, h, 2, "the boot pass and the listener's establish")

	deleteSessionRowByID(t, h, kicked)
	ownSession(t, h, kicked)
	if err := events.NotifyReapKick(ctx, h.pool); err != nil {
		t.Fatalf("publish the kick: %v", err)
	}
	awaitReap(t, h, kicked, "the kick")
}

// TestReapKickWithoutAConnStillReapsOnTheInterval: the listener is optional —
// a deployment that cannot spare the connection configures none and gets the
// teardown latency it had before the kick, not a broken reaper. The same rung
// covers the listener that never establishes, since both leave the ticker as
// the only wake.
//
// Two sessions, for the reason the kick rung above needs two: the loop takes a
// pass before its first wait (#709), so a session owned before it starts is
// reaped by that pass and says nothing about the ticker. The second is owned
// only once the first is gone — after the boot pass listed the holding — so a
// tick is the only thing left that can reap it.
func TestReapKickWithoutAConnStillReapsOnTheInterval(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: 20 * time.Millisecond})
	h.prov = h.exec.provider.(*fakeProvider)
	if h.exec.cfg.ReapKickConn != nil {
		t.Fatal("the harness configured a kick connection; this rung needs none")
	}
	barrier := h.sid
	ticked := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	deleteSessionRowByID(t, h, barrier)
	h.prov.owned = []domain.ID{barrier}

	_, stop := runExecutor(t, h)
	defer stop()

	awaitReap(t, h, barrier, "a pass — the boot one or the tick after it, which this rung need not tell apart — with no listener")

	deleteSessionRowByID(t, h, ticked)
	ownSession(t, h, ticked)
	awaitReap(t, h, ticked, "the interval, with no listener")
}

// TestRunReapsBeforeItsFirstTick: the loop passes before it waits, so an
// executor that restarts more often than ReapInterval still tears down its
// predecessor's leftovers. The interval here is an hour and no listener is
// configured, so nothing but that pass can reap anything — and until #709 the
// wake that made this look covered belonged to the listener, which two
// supported configurations never start.
func TestRunReapsBeforeItsFirstTick(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: time.Hour})
	h.prov = h.exec.provider.(*fakeProvider)
	if h.exec.cfg.ReapKickConn != nil {
		t.Fatal("the harness configured a kick connection; this rung needs none")
	}
	held := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	deleteSessionRow(t, h)
	h.prov.owned = []domain.ID{h.sid}

	_, stop := runExecutor(t, h)
	defer stop()

	awaitReap(t, h, h.sid, "the pass the loop takes before its first wait")

	// And then it waits. A session owned after that pass must stand until the
	// tick an hour away — a loop that passed without ever waiting would reap it
	// too and look identical from the reap above.
	deleteSessionRowByID(t, h, held)
	ownSession(t, h, held)
	time.Sleep(500 * time.Millisecond)
	if slices.Contains(h.prov.reapedSnapshot(), held) {
		t.Error("a session owned after the boot pass was reaped within the interval: the loop is not waiting between passes")
	}
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

// listenerPID waits for the backend holding the kick's LISTEN and returns it.
// It finds it by the statement it last ran, which for that connection is the
// LISTEN itself and nothing since; the match is scoped to this test's own
// database, which pgtest creates fresh, so it cannot reach a parallel suite's
// listener.
func listenerPID(t *testing.T, h *harness) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var pid int
		err := h.pool.QueryRow(context.Background(),
			`SELECT pid FROM pg_stat_activity
			  WHERE datname = current_database() AND pid <> pg_backend_pid()
			    AND query = $1`, "LISTEN "+events.ChannelReapKick).Scan(&pid)
		if err == nil {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend is holding %s: %v", events.ChannelReapKick, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitListening waits for the LISTEN to exist, which is what a rung has to
// know before it publishes anything: a NOTIFY sent before the LISTEN is
// delivered to nobody and queued nowhere.
func awaitListening(t *testing.T, h *harness) {
	t.Helper()
	_ = listenerPID(t, h)
}

// awaitPasses waits until n reap passes have listed this endpoint's holding.
// A session owned after that cannot have been seen by any of them, which is
// what lets a rung say which wake reaped it: the loop passes once before its
// first wait (#709) and the listener sweeps again when it establishes, so two
// sweeps happen at startup for reasons that have nothing to do with the wake
// under test.
func awaitPasses(t *testing.T, h *harness, n int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.prov.ownedCallsSnapshot() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d reap pass(es) ran, want %d", what, h.prov.ownedCallsSnapshot(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killListener ends the backend holding the kick's LISTEN, the way a failover
// or an idle-connection reaper would.
func killListener(t *testing.T, h *harness) {
	t.Helper()
	pid := listenerPID(t, h)
	if _, err := h.pool.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate the listening backend: %v", err)
	}
}

// TestReapKickListenerRecoversFromALostConnection: the listener is not allowed
// to give up. Its connection dies the way a failover kills one, and the reaper
// must both catch up on what it missed and go on hearing new kicks — the first
// because LISTEN queues nothing for a connection that is gone, so a wake fired
// during the outage is simply not there to collect, and the sweep on every
// establish is what stands in for it.
//
// The interval is an hour throughout, so nothing here can be the ticker, and
// each session is owned only once the wake that must reap it is the next one.
func TestReapKickListenerRecoversFromALostConnection(t *testing.T) {
	shortenReapKickBackoff(t)
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: time.Hour})
	h.prov = h.exec.provider.(*fakeProvider)
	h.exec.cfg.ReapKickConn = kickConn(t, h)

	barrier := h.sid
	duringOutage := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	afterRecovery := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	deleteSessionRowByID(t, h, barrier)
	h.prov.owned = []domain.ID{barrier}

	ctx, stop := runExecutor(t, h)
	defer stop()

	awaitReap(t, h, barrier, "the startup sweeps")
	awaitPasses(t, h, 2, "the boot pass and the listener's establish")

	// Ended before the outage rather than during it, and reaped by nothing
	// until after it: no helper here publishes a kick, and both startup sweeps
	// are spent, so with the interval at an hour there is no wake left that
	// could take this session — until the reconnect's own sweep. Doing it in
	// this order costs the rung nothing and closes a window that a reconnect
	// racing a five-statement delete would otherwise open.
	deleteSessionRowByID(t, h, duringOutage)
	ownSession(t, h, duringOutage)
	killListener(t, h)
	awaitReap(t, h, duringOutage, "the sweep after reconnecting")

	// And it is still listening afterwards. The payload is deliberately not
	// the empty one the producer sends: the listener reads no payload at all,
	// and a future reader of one would fail here rather than in production.
	deleteSessionRowByID(t, h, afterRecovery)
	ownSession(t, h, afterRecovery)
	if _, err := h.pool.Exec(ctx, `SELECT pg_notify($1, $2)`,
		events.ChannelReapKick, "not-a-payload-anyone-reads"); err != nil {
		t.Fatalf("publish the kick: %v", err)
	}
	awaitReap(t, h, afterRecovery, "a kick delivered after the reconnect")
}

// TestReapKickListenerSurvivesAnUnusableTarget: a listener that can never
// connect must cost teardown latency and nothing else. Two things are
// asserted, and the second is the one worth having — that the interval still
// reaps is true even of a listener that gave up, so on its own it would pin
// nothing. Shutdown is the real guard: a retry pause that waits out its
// backoff instead of the context would hold Run open behind a connection that
// is never coming, and this is where that shows.
func TestReapKickListenerSurvivesAnUnusableTarget(t *testing.T) {
	// Deliberately not shortened: the point is that a pause far longer than
	// the test's patience still yields to the cancellation.
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: 20 * time.Millisecond})
	h.prov = h.exec.provider.(*fakeProvider)
	unusable, err := pgx.ParseConfig("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("parse the unusable config: %v", err)
	}
	h.exec.cfg.ReapKickConn = unusable
	barrier := h.sid
	ticked := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	deleteSessionRowByID(t, h, barrier)
	h.prov.owned = []domain.ID{barrier}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = h.exec.Run(ctx); close(runDone) }()

	// The barrier is reaped by a pass — at this interval the boot one and the
	// tick after it are twenty milliseconds apart and the rung need not tell
	// them apart. What the second subject buys is the claim in the name: this
	// listener never establishes, so it never wakes anything, and a session
	// owned after a pass has listed can only be reaped by a later tick.
	// TestRunReapsBeforeItsFirstTick is where the boot pass itself is pinned,
	// at an interval no tick can reach inside a test.
	awaitReap(t, h, barrier, "a pass, with a listener that cannot connect")
	deleteSessionRowByID(t, h, ticked)
	ownSession(t, h, ticked)
	awaitReap(t, h, ticked, "the interval, with a listener that cannot connect")

	// The deadline has to be shorter than the backoff, or a pause that waits
	// the backoff out instead of yielding to the cancellation still lands
	// inside it and the rung proves nothing. Returning takes microseconds, so
	// the margin is in the right place. Bounded rather than deferred, for the
	// reason runExecutor's stop gives: the failure here is a Run that does not
	// return, and waiting for it forever would hang the binary.
	cancel()
	select {
	case <-runDone:
	case <-time.After(reapKickBackoff / 2):
		t.Fatalf("Run did not return within %s while the listener was retrying every %s: "+
			"the retry pause is waiting out its backoff rather than the context",
			reapKickBackoff/2, reapKickBackoff)
	}
}

// TestReapKickDialsThePoolsConfigNotTheDSN: a DATABASE_URL may carry pgxpool's
// own pool_* options — cmd/executor's documentation tells operators to size
// pool_max_conns, so this is a supported shape, not an exotic one — and only
// pgxpool's parse consumes them. Hand the same string to pgx and they stay in
// the startup packet as settings the server has never heard of, which it
// refuses the whole connection over: the listener would never establish, the
// warning would repeat every backoff forever, and every teardown would quietly
// fall back to the interval this change exists to stop waiting for.
//
// Both halves are asserted, because the second is why the first is written the
// way it is: the raw parse must fail to connect, and the pool's config — the
// line cmd/executor runs — must work.
func TestReapKickDialsThePoolsConfigNotTheDSN(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: time.Hour})
	h.prov = h.exec.provider.(*fakeProvider)

	tuned := h.pool.Config().ConnConfig.Config.Database
	dsn := h.pool.Config().ConnString() + "?pool_max_conns=8"
	if tuned == "" {
		t.Fatal("the fixture pool reports no database")
	}

	raw, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgx.ParseConfig accepted no pool option at all: %v", err)
	}
	dctx, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDial()
	if conn, err := pgx.ConnectConfig(dctx, raw); err == nil {
		_ = conn.Close(context.Background())
		t.Fatal("pgx connected with a pool_max_conns DSN; the hazard this design avoids is gone, " +
			"and the reasoning in reapkick.go and cmd/executor is now stale")
	}

	tunedPool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open a pool over the same database with pool options: %v", err)
	}
	defer tunedPool.Close()
	h.exec.cfg.ReapKickConn = tunedPool.Config().ConnConfig.Copy()

	_, stop := runExecutor(t, h)
	defer stop()
	// The LISTEN itself, not a reap: since #709 the loop sweeps at startup
	// whatever the listener does, so a reap would be satisfied by a config that
	// never connects at all.
	awaitListening(t, h)
}

// TestReapKicksCoalesce: a burst of endings costs the sweep that sees all of
// them rather than one sweep each. Asserted against a live reapLoop on the
// count of sweeps it actually ran — one Owned listing per pass, by
// construction — rather than on the wake channel's length, which would only
// restate the constructor.
//
// The burst arrives while a pass is already running, which is the case the
// capacity exists for and the one a channel-length check cannot reach. Both
// mistakes fail it: an unbuffered wake would drop all five, since the loop is
// inside the pass and not at its select, leaving one sweep; an unbounded one
// would queue five more, leaving six.
//
// wake is called directly rather than published through Postgres so that the
// burst is complete before the pass is released — a NOTIFY arrives when it
// arrives, and a rung that raced that would be counting arrivals rather than
// coalescing. The NOTIFY path has its own rungs above.
func TestReapKicksCoalesce(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: time.Hour})
	h.prov = h.exec.provider.(*fakeProvider)
	if h.exec.cfg.ReapKickConn != nil {
		t.Fatal("the harness configured a kick connection; this rung counts sweeps and must have no wake it did not raise")
	}
	deleteSessionRow(t, h)
	h.prov.owned = []domain.ID{h.sid}

	// Hold the first pass open at the seam between classification and the
	// session lock. Closed rather than signalled once, so every later pass runs
	// straight through it.
	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	reapHookAfterClassify = func(domain.ID) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-release
	}
	t.Cleanup(func() { reapHookAfterClassify = nil })
	// Released however this rung ends: a t.Fatal below would otherwise leave
	// the pass parked in the hook, where no cancellation can reach it, and the
	// real failure would be buried under Run's refusal to return.
	// TestRunWaitsForTheReaperToStop guards its own hook the same way.
	var releaseOnce sync.Once
	releaseReaper := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseReaper()

	_, stop := runExecutor(t, h)
	defer stop()

	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass the loop takes before its first wait never reached the classify seam")
	}
	for range 5 {
		h.exec.wake()
	}
	releaseReaper()

	awaitReap(t, h, h.sid, "the pass that was already running")
	// One sweep for the five, so two in all — the boot pass and the burst's. Settled rather than sampled: the
	// second sweep is still starting when the first one's reap lands, and with
	// the interval at an hour nothing else can add to this.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n := h.prov.ownedCallsSnapshot(); n > 2 {
			t.Fatalf("five wakes during one pass cost %d sweeps, want 2", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := h.prov.ownedCallsSnapshot(); n != 2 {
		t.Fatalf("five wakes during one pass cost %d sweeps, want 2 "+
			"(the one that was running, and one for the whole burst)", n)
	}
}
