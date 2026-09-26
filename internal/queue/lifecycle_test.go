package queue_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"
)

func TestAckTransitionsQueuedToStarting(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, err := q.Poll(ctx, env, time.Minute)
	if err != nil || w == nil {
		t.Fatalf("poll: %+v %v", w, err)
	}

	acked, err := q.Ack(ctx, env, w.ID)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked.State != "starting" {
		t.Errorf("state after ack = %q, want starting", acked.State)
	}
	if acked.AcknowledgedAt == nil {
		t.Error("acknowledged_at not set by ack")
	}
	// Idempotent: re-ack keeps starting and does not re-stamp acknowledged_at.
	first := *acked.AcknowledgedAt
	again, err := q.Ack(ctx, env, w.ID)
	if err != nil {
		t.Fatalf("re-ack: %v", err)
	}
	if again.State != "starting" || again.AcknowledgedAt == nil || !again.AcknowledgedAt.Equal(first) {
		t.Errorf("re-ack not idempotent: state=%q acked=%v (want starting, %v)", again.State, again.AcknowledgedAt, first)
	}

	// Unknown work id, and a real id under the wrong environment, are not-found.
	if _, err := q.Ack(ctx, env, domain.NewID("work")); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("ack unknown = %v, want ErrWorkNotFound", err)
	}
	_, otherEnv := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Ack(ctx, otherEnv, w.ID); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("ack wrong env = %v, want ErrWorkNotFound", err)
	}
}

func TestHeartbeatClaimsAndExtendsWithOptimisticConcurrency(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)

	// A heartbeat before ack (still queued) cannot claim: mismatch.
	if _, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Fatalf("heartbeat before ack = %v, want ErrHeartbeatMismatch", err)
	}
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatal(err)
	}

	// First heartbeat claims the lease: starting → active.
	hb1, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30)
	if err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	if hb1.State != "active" || !hb1.LeaseExtended || hb1.TTLSeconds != 30 {
		t.Errorf("first heartbeat = %+v, want active/extended/ttl 30", hb1)
	}
	// started_at is the enqueue stamp (#542): the claim leaves it where it was.
	got, _ := q.GetWork(ctx, env, w.ID)
	if got.StartedAt == nil || w.StartedAt == nil || !got.StartedAt.Equal(*w.StartedAt) {
		t.Errorf("started_at after first heartbeat = %v, want the enqueue stamp %v", got.StartedAt, w.StartedAt)
	}

	// A second NO_HEARTBEAT is rejected — the lease is already claimed.
	if _, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("re-claim = %v, want ErrHeartbeatMismatch", err)
	}
	// A stale/wrong expected value is rejected.
	if _, err := q.Heartbeat(ctx, env, w.ID, "2000-01-01T00:00:00Z", 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("wrong expected = %v, want ErrHeartbeatMismatch", err)
	}
	// Echoing the server's prior last_heartbeat extends the lease and rolls it.
	hb2, err := q.Heartbeat(ctx, env, w.ID, hb1.LastHeartbeat.Format(time.RFC3339Nano), 45)
	if err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}
	if hb2.State != "active" || !hb2.LeaseExtended || hb2.LastHeartbeat == nil || !hb2.LastHeartbeat.After(*hb1.LastHeartbeat) {
		t.Errorf("second heartbeat = %+v (prev %v), want active/extended/rolled", hb2, hb1.LastHeartbeat)
	}
	// The old value no longer matches (optimistic concurrency).
	if _, err := q.Heartbeat(ctx, env, w.ID, hb1.LastHeartbeat.Format(time.RFC3339Nano), 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("replay of superseded heartbeat = %v, want ErrHeartbeatMismatch", err)
	}
}

func TestHeartbeatOnStoppingLearnsWithoutExtending(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatal(err)
	}
	hb, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30)
	if err != nil {
		t.Fatal(err)
	}
	// Control plane requests a graceful stop.
	if _, _, err := q.Stop(ctx, env, w.ID, false); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	// The worker's next heartbeat (echoing the prior value) still matches, but
	// the item is stopping: it learns the state and the lease is not extended.
	after, err := q.Heartbeat(ctx, env, w.ID, hb.LastHeartbeat.Format(time.RFC3339Nano), 30)
	if err != nil {
		t.Fatalf("heartbeat on stopping: %v", err)
	}
	if after.State != "stopping" || after.LeaseExtended {
		t.Errorf("heartbeat on stopping = %+v, want stopping/not-extended", after)
	}
}

// TestAckAndClaimHealAStamplessItem: during a rolling upgrade a replica still
// running the code that cleared started_at at every hand-out can poll an item
// whose ack and claim then land on an upgraded one. Neither may carry the null
// on to the rest of the item's life (#542): each takes created_at for a
// missing stamp and leaves an existing one alone.
func TestAckAndClaimHealAStamplessItem(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)
	clear := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE work_items SET started_at = NULL WHERE id = $1`, w.ID); err != nil {
			t.Fatal(err)
		}
	}
	healed := func(step string) {
		t.Helper()
		got, err := q.GetWork(ctx, env, w.ID)
		if err != nil || got.StartedAt == nil || !got.StartedAt.Equal(got.CreatedAt) {
			t.Errorf("after %s started_at = %v (%v), want created_at", step, got, err)
		}
	}
	clear()
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatal(err)
	}
	healed("ack")
	clear()
	if _, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatal(err)
	}
	healed("claim heartbeat")
}

// claimedItem drives a fresh tool_exec item through the worker handshake —
// enqueue, poll, ack, first heartbeat — leaving it active under a claimed lease,
// and returns its environment and id: the shared setup of the tests of a
// wind-down whose worker already beats.
func claimedItem(t *testing.T, pool *pgxpool.Pool, q *queue.Queue) (domain.ID, domain.ID) {
	t.Helper()
	env, id := ackedItem(t, pool, q)
	if _, err := q.Heartbeat(context.Background(), env, id, queue.NoHeartbeat, 30); err != nil {
		t.Fatalf("claim heartbeat: %v", err)
	}
	return env, id
}

// ackedItem is claimedItem short of the claim: the item is starting, under
// the ack's startup lease, and no beat has reached it — the state a graceful
// stop reaches before the worker's claim does.
func ackedItem(t *testing.T, pool *pgxpool.Pool, q *queue.Queue) (domain.ID, domain.ID) {
	t.Helper()
	ctx := context.Background()
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	w, err := q.Poll(ctx, env, time.Minute)
	if err != nil || w == nil {
		t.Fatalf("poll: %+v %v", w, err)
	}
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	return env, w.ID
}

// leaseHeld reports whether the item still carries a lease. The wire work object
// has no lease field, so Work does not project one and a stop's lease clearing is
// observable only on the row.
func leaseHeld(t *testing.T, pool *pgxpool.Pool, id domain.ID) bool {
	t.Helper()
	return leaseOf(t, pool, id) != nil
}

// leaseOf reads the item's lease expiry, nil when it carries none.
func leaseOf(t *testing.T, pool *pgxpool.Pool, id domain.ID) *time.Time {
	t.Helper()
	var lease *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM work_items WHERE id = $1`, id).Scan(&lease); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	return lease
}

// rowSnapshot renders the item's whole row, the columns the wire never carries
// (the lease, updated_at) included, so a test can hold a call to having
// written nothing at all.
func rowSnapshot(t *testing.T, pool *pgxpool.Pool, id domain.ID) string {
	t.Helper()
	var row string
	if err := pool.QueryRow(context.Background(),
		`SELECT to_jsonb(w)::text FROM work_items w WHERE id = $1`, id).Scan(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return row
}

func TestStopForceAndGraceful(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	// Graceful stop of an item a worker holds → stopping; force then escalates →
	// stopped.
	env, id := claimedItem(t, pool, q)
	held, err := q.GetWork(ctx, env, id)
	if err != nil || held.StartedAt == nil {
		t.Fatalf("claimed item = %+v %v, want its enqueue started_at", held, err)
	}
	// Stop returns the updated item, which the wire answers with (#804).
	stopped, moved, err := q.Stop(ctx, env, id, false)
	if err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	if !moved || stopped.State != "stopping" || stopped.StopRequestedAt == nil || stopped.StoppedAt != nil {
		t.Errorf("graceful stop returned %+v (moved %v), want stopping with stop_requested_at and no stopped_at", stopped, moved)
	}
	// Neither stop moves started_at, as no reference stop does (#542).
	if stopped.StartedAt == nil || !stopped.StartedAt.Equal(*held.StartedAt) {
		t.Errorf("graceful stop started_at = %v, want %v", stopped.StartedAt, held.StartedAt)
	}
	// The lease stays: the worker holds it while it winds down, and its lapsing
	// past WindDown is what tells the control plane the wind-down was abandoned.
	if !leaseHeld(t, pool, id) {
		t.Error("graceful stop cleared the lease the winding-down worker still holds")
	}
	// Re-graceful-stopping a stopping item moves nothing, and says so: it
	// returns the item as it stands.
	if again, moved, err := q.Stop(ctx, env, id, false); err != nil || moved || !reflect.DeepEqual(again, stopped) {
		t.Errorf("re-graceful-stop = %+v, moved %v, %v; want the unchanged %+v, not moved", again, moved, err, stopped)
	}
	// force escalates stopping → stopped.
	stopped, moved, err = q.Stop(ctx, env, id, true)
	if err != nil {
		t.Fatalf("force stop: %v", err)
	}
	if !moved || stopped.State != "stopped" || stopped.StoppedAt == nil {
		t.Errorf("force stop returned %+v (moved %v), want stopped with stopped_at", stopped, moved)
	}
	if stopped.StartedAt == nil || !stopped.StartedAt.Equal(*held.StartedAt) {
		t.Errorf("force stop started_at = %v, want %v", stopped.StartedAt, held.StartedAt)
	}
	if leaseHeld(t, pool, id) {
		t.Error("force stop left a lease behind, want it cleared")
	}
	// Stopping an already-stopped item, forced or not, moves nothing either.
	for _, force := range []bool{true, false} {
		if again, moved, err := q.Stop(ctx, env, id, force); err != nil || moved || !reflect.DeepEqual(again, stopped) {
			t.Errorf("stop (force %v) of stopped = %+v, moved %v, %v; want the unchanged %+v, not moved", force, again, moved, err, stopped)
		}
	}
	// A missing item is not-found.
	if _, _, err := q.Stop(ctx, env, domain.NewID("work"), true); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("stop unknown = %v, want ErrWorkNotFound", err)
	}
}

// TestGracefulStopOfQueuedWorkStopsOutright: a graceful stop asks the item's
// worker to wind down, and a queued item has no worker to ask — nobody acked
// it. Parking one in stopping would leave it non-terminal with a null
// stopped_at until a poll finalized it, and one never handed out has no lease
// to lapse at all (#25). A graceful stop of one therefore finalizes it
// outright — there is nothing in flight to wind down. No recording reaches
// this case; docs/DIVERGENCES.md registers it as an inference.
func TestGracefulStopOfQueuedWorkStopsOutright(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	for _, tc := range []struct {
		name string
		poll bool
	}{
		// Never handed out is the starkest case: no worker has even seen the item,
		// and with no lease to lapse not even a timeout could rescue it later.
		{"queued, never handed out", false},
		{"queued, polled but not acked", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
			if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
				t.Fatal(err)
			}
			// An un-polled item's id is the one Enqueue minted; a client that listed
			// the environment's queue can address it, and so stop it.
			var id domain.ID
			if err := pool.QueryRow(ctx,
				`SELECT id FROM work_items WHERE session_id = $1 AND kind = 'tool_exec'`,
				sessionID).Scan(&id); err != nil {
				t.Fatalf("read enqueued id: %v", err)
			}
			if tc.poll {
				w, err := q.Poll(ctx, env, time.Minute)
				if err != nil || w == nil {
					t.Fatalf("poll: %+v %v", w, err)
				}
				id = w.ID
			}
			stopped, moved, err := q.Stop(ctx, env, id, false)
			if err != nil {
				t.Fatalf("graceful stop: %v", err)
			}
			if !moved || stopped.State != "stopped" || stopped.StoppedAt == nil || stopped.StopRequestedAt == nil {
				t.Errorf("graceful stop of a %s item returned %+v, want stopped with both timestamps", tc.name, stopped)
			}
			// The poll reservation, where there is one, is released with it.
			if leaseHeld(t, pool, id) {
				t.Error("the finalized item still carries a lease, want it cleared")
			}
		})
	}
}

// TestGracefulStopOfStartingWorkWindsDown: an acked item has a worker — the
// one that acked it — before any beat of its lands, and that worker's claim is
// the channel it is told on (TestClaimOnNeverClaimedStoppingWorkLearnsTheStop).
// So a graceful stop parks it in stopping, as the reference does (2026-09-19
// custom-mixed-tools #22 and #23, self-hosted-latest-cli #2 and #5):
// stop_requested_at stamped, stopped_at and the last heartbeat still null, the
// ack's stamps untouched. The startup lease its ack installed is kept, and is
// load-bearing: cleared, the item would meet the finalizer's null-lease arm at
// the very next poll and be settled before a claim as late as the recorded one
// (59 s after the stop) could arrive.
func TestGracefulStopOfStartingWorkWindsDown(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	env, id := ackedItem(t, pool, q)
	acked, err := q.GetWork(ctx, env, id)
	if err != nil {
		t.Fatal(err)
	}
	lease := leaseOf(t, pool, id)
	if lease == nil {
		t.Fatal("the ack installed no startup lease")
	}

	stopping, moved, err := q.Stop(ctx, env, id, false)
	if err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	if !moved || stopping.State != "stopping" || stopping.StopRequestedAt == nil || stopping.StoppedAt != nil || stopping.LastHeartbeat != nil {
		t.Errorf("graceful stop of starting work = %+v (moved %v), want stopping with stop_requested_at, no stopped_at, no heartbeat",
			stopping, moved)
	}
	if !reflect.DeepEqual(stopping.AcknowledgedAt, acked.AcknowledgedAt) || !reflect.DeepEqual(stopping.StartedAt, acked.StartedAt) {
		t.Errorf("graceful stop moved acknowledged_at %v -> %v or started_at %v -> %v",
			acked.AcknowledgedAt, stopping.AcknowledgedAt, acked.StartedAt, stopping.StartedAt)
	}
	if got := leaseOf(t, pool, id); got == nil || !got.Equal(*lease) {
		t.Errorf("lease after the graceful stop = %v, want the ack's startup lease %v kept", got, lease)
	}
	// A graceful stop of it again moves nothing, as for any stopping item.
	if again, moved, err := q.Stop(ctx, env, id, false); err != nil || moved || !reflect.DeepEqual(again, stopping) {
		t.Errorf("repeat graceful stop = %+v, moved %v, %v; want the unchanged %+v, not moved", again, moved, err, stopping)
	}
}

// TestClaimOnNeverClaimedStoppingWorkLearnsTheStop: when a graceful stop
// reaches acked work before its worker's claim does, the claim is the one beat
// that worker still sends, so it is where the worker learns of the stop. The
// reference answers it 200 — lease_extended false, state stopping, and no last
// heartbeat (2026-09-19 custom-mixed-tools #44) — and its worker then
// force-stops the item (#47). The claim writes nothing: it records no beat
// (the force stop the recording answers next still carries a null
// latest_heartbeat_at), extends no lease, and repeating it answers the same.
// ttl_seconds echoes the TTL the claim asked for, as every non-extending beat
// here does.
func TestClaimOnNeverClaimedStoppingWorkLearnsTheStop(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	env, id := ackedItem(t, pool, q)
	if _, _, err := q.Stop(ctx, env, id, false); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	before := rowSnapshot(t, pool, id)

	for i, ttl := range []int64{30, 45} {
		hb, err := q.Heartbeat(ctx, env, id, queue.NoHeartbeat, ttl)
		if err != nil {
			t.Fatalf("claim %d on never-claimed stopping work = %v, want the stop reported", i, err)
		}
		if hb.State != "stopping" || hb.LeaseExtended || hb.LastHeartbeat != nil || hb.TTLSeconds != ttl {
			t.Errorf("claim %d = %+v, want stopping, not extended, no last heartbeat, ttl %d", i, hb, ttl)
		}
		if after := rowSnapshot(t, pool, id); after != before {
			t.Errorf("claim %d wrote the row:\nbefore %s\nafter  %s", i, before, after)
		}
	}
}

// TestClaimRefusalsAroundAStop: a claim answers a stop only on never-claimed
// stopping work. Every neighbour keeps the 412 of a claim whose preconditions
// fail, and writes nothing: once-claimed stopping work, whose worker learns of
// the stop from its next echo beat instead, and stopped work, whether a beat
// ever reached it or not. No recording reaches these; docs/DIVERGENCES.md
// registers each as an inference. An item the work API cannot see stays a 404.
func TestClaimRefusalsAroundAStop(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	for _, tc := range []struct {
		name         string
		claimed      bool // a beat claimed the item before the stop
		force        bool
		wantStopping bool
	}{
		{"once-claimed stopping work", true, false, true},
		{"stopped work no beat reached", false, true, false},
		{"stopped work once claimed", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, id := ackedItem(t, pool, q)
			if tc.claimed {
				if _, err := q.Heartbeat(ctx, env, id, queue.NoHeartbeat, 30); err != nil {
					t.Fatalf("claim: %v", err)
				}
			}
			w, _, err := q.Stop(ctx, env, id, tc.force)
			if err != nil {
				t.Fatalf("stop: %v", err)
			}
			if (w.State == "stopping") != tc.wantStopping {
				t.Fatalf("the stop left %s, which is not the case under test", w.State)
			}
			before := rowSnapshot(t, pool, id)
			if _, err := q.Heartbeat(ctx, env, id, queue.NoHeartbeat, 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
				t.Errorf("claim on %s = %v, want ErrHeartbeatMismatch", tc.name, err)
			}
			if after := rowSnapshot(t, pool, id); after != before {
				t.Errorf("the refused claim wrote the row:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
	env, _ := ackedItem(t, pool, q)
	if _, err := q.Heartbeat(ctx, env, domain.NewID("work"), queue.NoHeartbeat, 30); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("claim on a missing item = %v, want ErrWorkNotFound", err)
	}
}

// TestPollFinalizesANeverClaimedWindDown: a worker that acked an item and died
// before its claim never learns of a graceful stop, so only the finalizer ends
// that wind-down, on the signal an abandoned claimed one gives: the lease —
// here the ack's startup lease, which the stop kept — lapsed AND WindDown
// passed since the request. Until both hold, a poll neither finalizes the item
// nor re-offers it (a stopping item never is), so a claim as late as the
// recorded one, 59 s after the stop, still finds it stopping. Past both, the
// next poll settles it, and a claim that arrives after that meets stopped
// work, which is refused. The re-arm the settlement owes is the work API's,
// pinned by internal/api's TestWorkGracefulStopOfStartingWorkReArmsOnce.
func TestPollFinalizesANeverClaimedWindDown(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	env, id := ackedItem(t, pool, q)
	if _, _, err := q.Stop(ctx, env, id, false); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	set := func(assign string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE work_items SET `+assign+` WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	leftAlone := func(when string) {
		t.Helper()
		if done := finalizeAbandoned(t, pool, q, env); len(done) != 0 {
			t.Fatalf("finalize %s = %v, want nothing", when, done)
		}
		if next, err := q.Poll(ctx, env, time.Minute); err != nil || next != nil {
			t.Fatalf("poll %s = %+v %v, want no work (a stopping item is never re-offered)", when, next, err)
		}
		if w, err := q.GetWork(ctx, env, id); err != nil || w.State != "stopping" {
			t.Fatalf("item %s = %+v %v, want it left stopping", when, w, err)
		}
	}

	leftAlone("under the startup lease")
	set(`lease_expires_at = now() - interval '1 second', stop_requested_at = now() - interval '59 seconds'`)
	leftAlone("59 s into the wind-down, its lease lapsed")
	if hb, err := q.Heartbeat(ctx, env, id, queue.NoHeartbeat, 30); err != nil || hb.State != "stopping" {
		t.Fatalf("claim 59 s after the stop = %+v %v, want the stop reported", hb, err)
	}

	set(`stop_requested_at = now() - interval '61 seconds'`)
	if done := finalizeAbandoned(t, pool, q, env); len(done) != 1 {
		t.Fatalf("finalize past WindDown and the lease = %v, want the item's session", done)
	}
	w, err := q.GetWork(ctx, env, id)
	if err != nil || w.State != "stopped" || w.StoppedAt == nil || w.LastHeartbeat != nil {
		t.Fatalf("settled wind-down = %+v %v, want stopped with stopped_at and no heartbeat", w, err)
	}
	if leaseHeld(t, pool, id) {
		t.Error("the settled item still carries a lease, want it cleared")
	}
	if next, err := q.Poll(ctx, env, time.Minute); err != nil || next != nil {
		t.Fatalf("poll after the settle = %+v %v, want no work", next, err)
	}
	if _, err := q.Heartbeat(ctx, env, id, queue.NoHeartbeat, 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("claim after the settle = %v, want ErrHeartbeatMismatch", err)
	}
}

// TestPollFinalizesAnAbandonedWindDown: the worker asked to wind down normally
// stops the item itself, but one that dies mid-wind-down (SIGKILL, a panic, an
// unreachable control plane) never does — and Poll deliberately never re-offers a
// stopping item, so nothing else would move it. Its lease still lapses, and —
// once WindDown has passed since the request — that is the signal: the next poll
// of the environment finalizes the abandoned item rather than leaving it
// non-terminal forever (#25). It is finalized, never
// handed back as work — the caller asked for it to stop.
func TestPollFinalizesAnAbandonedWindDown(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	env, id := claimedItem(t, pool, q)
	if _, _, err := q.Stop(ctx, env, id, false); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}

	// While the lease is live the wind-down is still its worker's to finish, so a
	// poll must leave the item alone. This is what makes the finalize below a
	// consequence of the lapsed lease rather than of the stopping state.
	if done := finalizeAbandoned(t, pool, q, env); len(done) != 0 {
		t.Fatalf("finalize during a live wind-down = %v, want nothing", done)
	}
	if next, err := q.Poll(ctx, env, time.Minute); err != nil || next != nil {
		t.Fatalf("poll during a live wind-down = %+v %v, want no work", next, err)
	}
	if w, err := q.GetWork(ctx, env, id); err != nil || w.State != "stopping" {
		t.Fatalf("item during a live wind-down = %+v %v, want it left stopping", w, err)
	}

	// The worker dies without finishing the wind-down: its lease lapses. Inside
	// WindDown that proves nothing — a live worker stops heartbeating the moment
	// it learns of the stop and flushes what it holds — so the item is left alone.
	if _, err := pool.Exec(ctx,
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if done := finalizeAbandoned(t, pool, q, env); len(done) != 0 {
		t.Fatalf("finalize inside the wind-down's minute = %v, want nothing", done)
	}
	// Forty-five seconds in — the worker's whole notice-and-flush — still nothing.
	if _, err := pool.Exec(ctx,
		`UPDATE work_items SET stop_requested_at = now() - interval '45 seconds' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if done := finalizeAbandoned(t, pool, q, env); len(done) != 0 {
		t.Fatalf("finalize 45 s into the wind-down = %v, want nothing", done)
	}
	// Past it, the lapsed lease is the signal.
	if _, err := pool.Exec(ctx,
		`UPDATE work_items SET stop_requested_at = now() - interval '61 seconds' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if done := finalizeAbandoned(t, pool, q, env); len(done) != 1 {
		t.Fatalf("finalize = %v, want the one abandoned item's session", done)
	}
	next, err := q.Poll(ctx, env, time.Minute)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if next != nil {
		t.Fatalf("poll handed back %+v, want no work (an abandoned stop is finalized, not re-offered)", next)
	}
	w, err := q.GetWork(ctx, env, id)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if w.State != "stopped" || w.StoppedAt == nil {
		t.Errorf("abandoned wind-down = %+v, want stopped with stopped_at", w)
	}
	if leaseHeld(t, pool, id) {
		t.Error("the finalized item still carries a lease, want it cleared")
	}
}

// TestPollFinalizesALeaselessWindDown covers the one stopping row the current
// state machine never writes but a rolling upgrade can still produce: until #25 a
// graceful stop parked a never-polled queued item — which carries no lease at all
// — in stopping, so a not-yet-upgraded replica can keep writing one after
// migration 0014 has repaired the rows that predate it. With no lease to lapse,
// only the finalizer's null arm can ever reach it.
func TestPollFinalizesALeaselessWindDown(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	// What the old state machine wrote: stopping, never handed out, no lease.
	var id domain.ID
	if err := pool.QueryRow(ctx,
		`UPDATE work_items SET state = 'stopping', stop_requested_at = now()
		 WHERE session_id = $1 AND kind = 'tool_exec' RETURNING id`, sessionID).Scan(&id); err != nil {
		t.Fatalf("stage the legacy row: %v", err)
	}

	if done := finalizeAbandoned(t, pool, q, env); len(done) != 1 || done[0] != sessionID {
		t.Fatalf("finalize = %v, want the legacy row's session", done)
	}
	next, err := q.Poll(ctx, env, time.Minute)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if next != nil {
		t.Fatalf("poll handed back %+v, want no work", next)
	}
	w, err := q.GetWork(ctx, env, id)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if w.State != "stopped" || w.StoppedAt == nil {
		t.Errorf("leaseless wind-down = %+v, want stopped with stopped_at", w)
	}
}

func TestGetWorkScopingAndFields(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)

	got, err := q.GetWork(ctx, env, w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SessionID != sessionID || got.State != "queued" {
		t.Errorf("got = %+v", got)
	}
	// A queued item has reached no lifecycle timestamp — started_at aside, which
	// enqueue stamps, as the reference renders it on a still-queued item (#542).
	if got.AcknowledgedAt != nil || got.StopRequestedAt != nil || got.StoppedAt != nil || got.LastHeartbeat != nil {
		t.Errorf("queued item has non-null lifecycle timestamps: %+v", got)
	}
	if got.StartedAt == nil || got.StartedAt.Before(got.CreatedAt) {
		t.Errorf("queued item started_at = %v, want stamped at enqueue (created_at %v)", got.StartedAt, got.CreatedAt)
	}
	if _, err := q.GetWork(ctx, env, domain.NewID("work")); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("get unknown = %v, want ErrWorkNotFound", err)
	}
	_, otherEnv := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.GetWork(ctx, otherEnv, w.ID); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("get wrong env = %v, want ErrWorkNotFound", err)
	}
}

// TestUpdateMetadataPatches pins the work-item metadata patch: a string value
// upserts a key, an explicit delete removes it, absent keys are preserved, and
// the merge is atomic (a single UPDATE, no read-modify-write). It is scoped like
// the rest of the work API and leaves lifecycle state and timestamps untouched.
func TestUpdateMetadataPatches(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)

	// Upsert two keys onto the default-empty metadata.
	got, err := q.UpdateMetadata(ctx, env, w.ID, map[string]string{"a": "1", "b": "2"}, nil)
	if err != nil {
		t.Fatalf("update upsert: %v", err)
	}
	if len(got.Metadata) != 2 || got.Metadata["a"] != "1" || got.Metadata["b"] != "2" {
		t.Errorf("after upsert metadata = %v, want a=1 b=2", got.Metadata)
	}
	// The patch must not transition the item: still queued after a poll.
	if got.State != "queued" || got.AcknowledgedAt != nil || got.StartedAt == nil || w.StartedAt == nil || !got.StartedAt.Equal(*w.StartedAt) {
		t.Errorf("metadata update disturbed lifecycle: %+v", got)
	}

	// A mixed patch: upsert a, delete b, add c.
	got, err = q.UpdateMetadata(ctx, env, w.ID, map[string]string{"a": "9", "c": "3"}, []string{"b"})
	if err != nil {
		t.Fatalf("update mixed: %v", err)
	}
	if len(got.Metadata) != 2 || got.Metadata["a"] != "9" || got.Metadata["c"] != "3" {
		t.Errorf("after mixed patch metadata = %v, want a=9 c=3 (b deleted)", got.Metadata)
	}

	// An empty patch is a no-op that still returns the item.
	got, err = q.UpdateMetadata(ctx, env, w.ID, map[string]string{}, nil)
	if err != nil || len(got.Metadata) != 2 {
		t.Errorf("empty patch = %+v %v, want unchanged", got.Metadata, err)
	}

	// A nil patch (nil upserts AND nil deletes) is also a no-op, not corruption:
	// the guards turn nil upserts into {} (else `metadata || 'null'` coerces the
	// object into a JSON array) and nil deletes into an empty text[] (else
	// `metadata - NULL` nulls the NOT NULL column). This exercises those guards,
	// which no other caller reaches.
	got, err = q.UpdateMetadata(ctx, env, w.ID, nil, nil)
	if err != nil {
		t.Fatalf("nil patch: %v", err)
	}
	if len(got.Metadata) != 2 || got.Metadata["a"] != "9" || got.Metadata["c"] != "3" {
		t.Errorf("nil patch corrupted metadata = %v, want a=9 c=3 unchanged", got.Metadata)
	}

	// Scoping: an unknown id and a wrong-env id are both ErrWorkNotFound.
	if _, err := q.UpdateMetadata(ctx, env, domain.NewID("work"), map[string]string{"x": "1"}, nil); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("update unknown = %v, want ErrWorkNotFound", err)
	}
	_, otherEnv := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.UpdateMetadata(ctx, otherEnv, w.ID, map[string]string{"x": "1"}, nil); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("update wrong env = %v, want ErrWorkNotFound", err)
	}
}

// TestPollReclaimsExpiredLeases pins the reclaim scope: a still-queued (un-acked)
// reservation whose window lapsed is re-offered, AND an acked (`starting`) or
// heartbeating (`active`) item whose lease has lapsed (a dead worker) is
// reclaimed — reset to a fresh queued reservation so the next worker can re-poll,
// re-ack, and re-claim it (NO_HEARTBEAT needs last_heartbeat cleared). An item
// whose lease is still LIVE is never reclaimed: its worker still owns it.
func TestPollReclaimsExpiredLeases(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}

	// A queued reservation whose window lapsed is re-offered on the next poll —
	// under a fresh identity (see TestEveryReHandOutMintsAFreshWorkIdentity).
	first, _ := q.Poll(ctx, env, -time.Second) // reservation already expired
	if first == nil {
		t.Fatal("first poll returned nil")
	}
	again, err := q.Poll(ctx, env, time.Minute)
	if err != nil || again == nil || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("lapsed queued reservation not re-offered: %+v %v", again, err)
	}
	if again.ID == first.ID {
		t.Errorf("re-offered under the same id %s, want a fresh one", again.ID)
	}
	// started_at is stamped at enqueue and survives every hand-out of a
	// still-queued item: no worker has taken it back out of the queue (#542).
	if first.StartedAt == nil || again.StartedAt == nil || !again.StartedAt.Equal(*first.StartedAt) {
		t.Errorf("re-offered queued item started_at = %v, want the enqueue stamp %v", again.StartedAt, first.StartedAt)
	}
	// A queued row with no stamp — enqueued before the upgrade, or handed out by
	// a replica still running the code that cleared it — takes its created_at at
	// its next hand-out rather than staying null.
	if _, err := pool.Exec(ctx,
		`UPDATE work_items SET started_at = NULL, lease_expires_at = now() - interval '1 second' WHERE id = $1`, again.ID); err != nil {
		t.Fatal(err)
	}
	healed, err := q.Poll(ctx, env, time.Minute)
	if err != nil || healed == nil || healed.StartedAt == nil || !healed.StartedAt.Equal(healed.CreatedAt) {
		t.Fatalf("stampless queued row re-offered as %+v %v, want started_at = created_at", healed, err)
	}
	again = healed

	expireLease := func(id domain.ID) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	// Ack it (queued→starting) and give it a live lease via a first heartbeat
	// (starting→active). While the lease is LIVE, poll must not reclaim it.
	if _, err := q.Ack(ctx, env, again.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Heartbeat(ctx, env, again.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatal(err)
	}
	if w, err := q.Poll(ctx, env, time.Minute); err != nil || w != nil {
		t.Errorf("poll reclaimed a live-leased active item = %+v %v, want nil (still owned)", w, err)
	}

	// The worker dies: its lease lapses. Poll now reclaims the active item,
	// resetting it so it re-enters the poll→ack→NO_HEARTBEAT-claim flow — again
	// under a fresh identity.
	expireLease(again.ID)
	reclaimed, err := q.Poll(ctx, env, time.Minute)
	if err != nil || reclaimed == nil || !reclaimed.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("expired active item not reclaimed: %+v %v", reclaimed, err)
	}
	if reclaimed.ID == again.ID {
		t.Errorf("reclaimed under the same id %s, want a fresh one", reclaimed.ID)
	}
	// A reclaim puts the item back in the queue, and re-entering the queue
	// re-stamps started_at, as the reference's re-queue does (#542).
	if reclaimed.StartedAt == nil || again.StartedAt == nil || !reclaimed.StartedAt.After(*again.StartedAt) {
		t.Errorf("reclaimed active item started_at = %v, want re-stamped after %v", reclaimed.StartedAt, again.StartedAt)
	}
	if _, err := q.Ack(ctx, env, reclaimed.ID); err != nil {
		t.Fatalf("reclaimed item cannot be re-acked: %v", err)
	}
	if _, err := q.Heartbeat(ctx, env, reclaimed.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatalf("reclaimed item cannot be re-claimed with NO_HEARTBEAT: %v", err)
	}
	// The re-stamp is later than created_at, so here — unlike on a fresh item —
	// an ack or claim that overwrote the stamp rather than healing a missing one
	// would show.
	if got, err := q.GetWork(ctx, env, reclaimed.ID); err != nil || got.StartedAt == nil || !got.StartedAt.Equal(*reclaimed.StartedAt) {
		t.Errorf("re-acked, re-claimed item = %+v %v, want started_at still the reclaim's %v", got, err, reclaimed.StartedAt)
	}

	// A freshly-acked `starting` item (a worker that has not sent its first
	// heartbeat yet) must NOT be reclaimed just because its un-acked poll
	// reservation lapsed: Ack installs a startup lease, so a slow-but-live worker
	// keeps its item. Poll it with an already-expired reservation, ack, and
	// confirm the next poll does not steal it.
	sid2, env2 := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env2, sid2, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w2, _ := q.Poll(ctx, env2, -time.Second) // reservation already expired
	if _, err := q.Ack(ctx, env2, w2.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := q.Poll(ctx, env2, time.Minute); err != nil || got != nil {
		t.Errorf("poll reclaimed a freshly-acked starting item = %+v %v, want nil (startup lease protects it)", got, err)
	}

	// Once that startup lease lapses — a worker that died between ack and its
	// first heartbeat — the starting item is reclaimed, under a fresh identity.
	expireLease(w2.ID)
	got, err := q.Poll(ctx, env2, time.Minute)
	if err != nil || got == nil || !got.CreatedAt.Equal(w2.CreatedAt) {
		t.Fatalf("expired starting item not reclaimed: %+v %v", got, err)
	}
	if got.ID == w2.ID {
		t.Errorf("reclaimed under the same id %s, want a fresh one", got.ID)
	}
	if got.StartedAt == nil || w2.StartedAt == nil || !got.StartedAt.After(*w2.StartedAt) {
		t.Errorf("reclaimed starting item started_at = %v, want re-stamped after %v", got.StartedAt, w2.StartedAt)
	}
}

// TestEveryReHandOutMintsAFreshWorkIdentity pins the fix for the identity-blind
// hung-worker race (#62). The wire's lifecycle calls carry no ownership proof —
// stop's body is {force} only and the work object has no generation field — so
// while a reclaim re-offered the SAME work id, a hung-then-revived worker's
// force-stop landed on whatever worker held the item next, re-stranding the
// session. Every re-hand-out now mints a fresh id: the stale worker's ack,
// heartbeat, and stop all address an id that no longer exists (not-found → 404),
// and the replacement's item is untouched. The row is the same row, so the
// client-visible metadata rides through the rotation.
//
// The FIRST hand-out keeps the id enqueue minted — no worker has ever held it,
// so there is nothing to invalidate, and a client that listed the queued item
// can still address it.
func TestEveryReHandOutMintsAFreshWorkIdentity(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	// Enqueued under an active span, so the rotation can be shown to carry the
	// trace context over: a reclaimed run that lost it would silently detach from
	// the turn that produced the work — and the reclaim is the run an operator
	// most needs traced.
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	if _, err := q.Enqueue(trace.ContextWithSpanContext(ctx, sc), pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	var enqueued string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM work_items WHERE session_id = $1 AND kind = 'tool_exec'`,
		sessionID.String()).Scan(&enqueued); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateMetadata(ctx, env, domain.ID(enqueued), map[string]string{"k": "v"}, nil); err != nil {
		t.Fatal(err)
	}

	// The hung worker's hand-out: the id is the one enqueue minted.
	stale, err := q.Poll(ctx, env, time.Minute)
	if err != nil || stale == nil {
		t.Fatalf("poll: %+v %v", stale, err)
	}
	if stale.ID.String() != enqueued {
		t.Errorf("first hand-out id = %s, want the enqueued %s (nothing has held it)", stale.ID, enqueued)
	}
	if _, err := q.Ack(ctx, env, stale.ID); err != nil {
		t.Fatal(err)
	}
	beat, err := q.Heartbeat(ctx, env, stale.ID, queue.NoHeartbeat, 30)
	if err != nil {
		t.Fatal(err)
	}
	staleEcho := beat.LastHeartbeat.Format(time.RFC3339Nano) // what its next beat would send
	// It hangs; its lease lapses.
	if _, err := pool.Exec(ctx,
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}

	// The replacement reclaims it under a fresh identity and takes the lease.
	fresh, err := q.Poll(ctx, env, time.Minute)
	if err != nil || fresh == nil {
		t.Fatalf("reclaim poll: %+v %v", fresh, err)
	}
	if fresh.ID == stale.ID {
		t.Fatalf("reclaimed under the stale id %s, want a fresh one", fresh.ID)
	}
	if !fresh.ID.HasPrefix("work") {
		t.Errorf("reclaimed id %q not work_-prefixed", fresh.ID)
	}
	if fresh.SessionID != sessionID || fresh.EnvironmentID != env || fresh.Metadata["k"] != "v" {
		t.Errorf("reclaim did not carry the item over: %+v", fresh)
	}
	if !fresh.CreatedAt.Equal(stale.CreatedAt) {
		t.Errorf("reclaim created_at = %s, want the enqueued %s (same row)", fresh.CreatedAt, stale.CreatedAt)
	}
	if want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID()); fresh.TraceContext["traceparent"] != want {
		t.Errorf("reclaim trace_context[traceparent] = %q, want %q", fresh.TraceContext["traceparent"], want)
	}
	if _, err := q.Ack(ctx, env, fresh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Heartbeat(ctx, env, fresh.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatal(err)
	}

	// The stale worker revives. Every lifecycle call it makes under the identity
	// it was handed is not-found — none of them reaches the replacement's item.
	if _, _, err := q.Stop(ctx, env, stale.ID, true); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("stale force-stop = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Ack(ctx, env, stale.ID); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("stale ack = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Heartbeat(ctx, env, stale.ID, staleEcho, 30); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("stale heartbeat = %v, want ErrWorkNotFound (it was a 412 while the id was reused)", err)
	}

	// The replacement's item is still live and still its own.
	live, err := q.GetWork(ctx, env, fresh.ID)
	if err != nil || live.State != "active" {
		t.Fatalf("replacement item = %+v %v, want a live active item", live, err)
	}
}

// TestLifecycleEndpointsRejectModelTurn pins that a worker's environment-key
// endpoints cannot see or mutate the brain's model_turn rows (which share the
// work_items table): acking one would wedge the brain's turn.
func TestLifecycleEndpointsRejectModelTurn(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	var mtID domain.ID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM work_items WHERE session_id = $1 AND kind = 'model_turn'`, sessionID).Scan(&mtID); err != nil {
		t.Fatal(err)
	}

	if _, err := q.GetWork(ctx, env, mtID); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("GetWork(model_turn) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Ack(ctx, env, mtID); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Ack(model_turn) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Heartbeat(ctx, env, mtID, queue.NoHeartbeat, 30); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Heartbeat(model_turn) = %v, want ErrWorkNotFound", err)
	}
	if _, _, err := q.Stop(ctx, env, mtID, true); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Stop(model_turn) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.UpdateMetadata(ctx, env, mtID, map[string]string{"x": "1"}, nil); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("UpdateMetadata(model_turn) = %v, want ErrWorkNotFound", err)
	}
	// The brain's item is untouched: still queued and claimable by the brain.
	it, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || it == nil || it.ID != mtID {
		t.Fatalf("model_turn item was disturbed by the work API: claim=%+v err=%v", it, err)
	}
}

// TestLifecycleEndpointsRejectCloudToolExec pins that a cloud environment's
// tool_exec item — the platform executor's work, not a worker's — is invisible
// to the work API even though it is tool_exec, so an errant cloud env key cannot
// yank it from the executor mid-run.
func TestLifecycleEndpointsRejectCloudToolExec(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, cloudEnv := pgtest.NewSession(t, pool, "cloud")
	if _, err := q.Enqueue(ctx, pool, cloudEnv, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	var id domain.ID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM work_items WHERE session_id = $1 AND kind = 'tool_exec'`, sessionID).Scan(&id); err != nil {
		t.Fatal(err)
	}

	if _, err := q.GetWork(ctx, cloudEnv, id); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("GetWork(cloud tool_exec) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.Ack(ctx, cloudEnv, id); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Ack(cloud tool_exec) = %v, want ErrWorkNotFound", err)
	}
	if _, _, err := q.Stop(ctx, cloudEnv, id, true); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("Stop(cloud tool_exec) = %v, want ErrWorkNotFound", err)
	}
	if _, err := q.UpdateMetadata(ctx, cloudEnv, id, map[string]string{"x": "1"}, nil); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("UpdateMetadata(cloud tool_exec) = %v, want ErrWorkNotFound", err)
	}
	// Poll serves only self_hosted, so a cloud environment yields nothing.
	if w, err := q.Poll(ctx, cloudEnv, time.Minute); err != nil || w != nil {
		t.Errorf("Poll(cloud env) = %+v %v, want nil (cloud is the executor's)", w, err)
	}
	// ListWork is scoped the same way: a cloud env's tool_exec never lists.
	if items, err := q.ListWork(ctx, cloudEnv, false, time.Time{}, "", 10); err != nil || len(items) != 0 {
		t.Errorf("ListWork(cloud env) = %d items %v, want 0 (cloud is the executor's)", len(items), err)
	}
	// The executor can still claim it — it was never disturbed.
	if it, err := q.Claim(ctx, queue.ToolExec, time.Minute); err != nil || it == nil || it.ID != id {
		t.Fatalf("cloud tool_exec disturbed by the work API: claim=%+v err=%v", it, err)
	}
}

// TestHeartbeatMissingAndMalformed pins two error mappings: a heartbeat on an
// absent item is not-found (404), distinct from a mismatch, and a malformed
// expected value is a mismatch (412), never a 500 from a failed SQL cast.
func TestHeartbeatMissingAndMalformed(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, _ := q.Poll(ctx, env, time.Minute)
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 30); err != nil {
		t.Fatal(err)
	}

	// An absent item is not-found (so a worker can tell "stale, retry" from "gone").
	if _, err := q.Heartbeat(ctx, env, domain.NewID("work"), "2026-01-01T00:00:00Z", 30); !errors.Is(err, queue.ErrWorkNotFound) {
		t.Errorf("heartbeat on missing item = %v, want ErrWorkNotFound", err)
	}
	// A malformed expected value is a mismatch, never a cast-error 500.
	if _, err := q.Heartbeat(ctx, env, w.ID, "not-a-timestamp", 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("heartbeat with malformed expected = %v, want ErrHeartbeatMismatch", err)
	}
	// A valid-but-stale value on a live item is still a mismatch.
	if _, err := q.Heartbeat(ctx, env, w.ID, "2000-01-01T00:00:00Z", 30); !errors.Is(err, queue.ErrHeartbeatMismatch) {
		t.Errorf("heartbeat with stale expected = %v, want ErrHeartbeatMismatch", err)
	}
}

// finalizeAbandoned drives the two-step the work API runs ahead of a poll:
// list the candidates, settle each under its own transaction. Returns the
// sessions of the items this call settled.
func finalizeAbandoned(t *testing.T, pool *pgxpool.Pool, q *queue.Queue, env domain.ID) []domain.ID {
	t.Helper()
	ctx := context.Background()
	items, err := q.ListAbandoned(ctx, env)
	if err != nil {
		t.Fatalf("list abandoned: %v", err)
	}
	var done []domain.ID
	for _, it := range items {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := q.FinalizeAbandoned(ctx, tx, env, it.ID)
		if err != nil {
			t.Fatalf("finalize %s: %v", it.ID, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if ok {
			done = append(done, it.SessionID)
		}
	}
	return done
}

// The settle re-checks the row under its transaction, so a stale candidate —
// here one another poll settled between the list and the settle — reports
// false instead of stopping (and re-arming) the session twice.
func TestFinalizeAbandonedRechecksUnderTheTransaction(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	env, id := claimedItem(t, pool, q)
	if _, _, err := q.Stop(ctx, env, id, false); err != nil {
		t.Fatal(err)
	}
	// The flip re-checks the window itself, whatever a listing said: a lapsed
	// lease 45 s into the wind-down is refused under the transaction.
	if _, err := pool.Exec(ctx,
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second', stop_requested_at = now() - interval '45 seconds' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	early, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := q.FinalizeAbandoned(ctx, early, env, id); err != nil || ok {
		t.Fatalf("finalize inside the window = %v %v, want refused", ok, err)
	}
	_ = early.Rollback(ctx)
	if _, err := pool.Exec(ctx,
		`UPDATE work_items SET stop_requested_at = now() - interval '61 seconds' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	items, err := q.ListAbandoned(ctx, env)
	if err != nil || len(items) != 1 {
		t.Fatalf("list = %v %v, want the one abandoned item", items, err)
	}
	// Two polls listed the same candidate; the first settles it.
	if done := finalizeAbandoned(t, pool, q, env); len(done) != 1 {
		t.Fatalf("first settle = %v, want the item's session", done)
	}
	// The second's guarded flip finds it already stopped and stands down.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ok, err := q.FinalizeAbandoned(ctx, tx, env, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a stale candidate was settled twice, want the recheck to refuse it")
	}
}

// HasUndrainedWork answers the environment delete's refusal (#546), so it is
// scoped exactly as the rest of the work API is — and each clause of that scope
// gets its own case, since a single queued self_hosted tool_exec satisfies them
// all at once and would pin none of them.
func TestHasUndrainedWork(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	// Nothing enqueued, and an environment that does not exist.
	sessionID, env := pgtest.NewSession(t, pool, "self_hosted")
	assertUndrained(t, q, ctx, env, false, "an empty queue")
	assertUndrained(t, q, ctx, domain.NewID("env"), false, "an environment that does not exist")

	// A queued tool_exec is work a worker could be handed.
	if _, err := q.Enqueue(ctx, pool, env, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	assertUndrained(t, q, ctx, env, true, "a queued tool_exec")

	// Still undrained while a worker holds it: polled, acked, then heartbeating.
	w, err := q.Poll(ctx, env, time.Minute)
	if err != nil || w == nil {
		t.Fatalf("poll: %+v %v", w, err)
	}
	if _, err := q.Ack(ctx, env, w.ID); err != nil {
		t.Fatal(err)
	}
	assertUndrained(t, q, ctx, env, true, "an acked (starting) item")
	if _, err := q.Heartbeat(ctx, env, w.ID, queue.NoHeartbeat, 60); err != nil {
		t.Fatal(err)
	}
	assertUndrained(t, q, ctx, env, true, "an active item a worker is running")

	// Stopped is drained. Stop's force arm is what draws that boundary, so it is
	// what the test uses to reach it.
	if _, _, err := q.Stop(ctx, env, w.ID, true); err != nil {
		t.Fatal(err)
	}
	assertUndrained(t, q, ctx, env, false, "a stopped item")

	// A model_turn is the brain's own queue: no worker polls it, so it is not
	// work the delete's refusal speaks for.
	turnSession, turnEnv := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(ctx, pool, turnEnv, turnSession, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	assertUndrained(t, q, ctx, turnEnv, false, "a queued model_turn")

	// A cloud tool_exec is the platform executor's queue, not a worker's.
	cloudSession, cloudEnv := pgtest.NewSession(t, pool, "cloud")
	if _, err := q.Enqueue(ctx, pool, cloudEnv, cloudSession, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	assertUndrained(t, q, ctx, cloudEnv, false, "a cloud tool_exec")
}

func assertUndrained(t *testing.T, q *queue.Queue, ctx context.Context, envID domain.ID, want bool, what string) {
	t.Helper()
	got, err := q.HasUndrainedWork(ctx, envID)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got != want {
		t.Errorf("%s: undrained = %v, want %v", what, got, want)
	}
}
