package queue_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

func TestEnqueueClaimComplete(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	created, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !created {
		t.Fatal("first enqueue reported not created")
	}

	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if item == nil {
		t.Fatal("Claim returned nil with queued work")
	}
	if item.SessionID != sessionID || item.EnvironmentID != envID || item.Kind != queue.ModelTurn {
		t.Errorf("item = %+v", item)
	}
	if item.Reclaimed {
		t.Error("fresh claim reported as reclaim")
	}
	if !domain.ID(item.ID).HasPrefix("work") {
		t.Errorf("item id %q not work_-prefixed", item.ID)
	}

	if err := q.Complete(ctx, pool, item); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	again, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil {
		t.Fatalf("Claim after complete: %v", err)
	}
	if again != nil {
		t.Errorf("claimed completed work: %+v", again)
	}
}

func TestEnqueueIdempotentWhileLive(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	created, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn)
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if created {
		t.Error("second enqueue created a duplicate live item")
	}

	// A different kind is independent.
	created, err = q.Enqueue(ctx, pool, envID, sessionID, queue.ToolExec)
	if err != nil {
		t.Fatalf("tool_exec Enqueue: %v", err)
	}
	if !created {
		t.Error("tool_exec enqueue suppressed by live model_turn")
	}

	// While the item is claimed (active) it still suppresses duplicates …
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("Claim: %v %v", item, err)
	}
	created, err = q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("enqueue while active created a duplicate")
	}

	// … and completion frees the slot.
	if err := q.Complete(ctx, pool, item); err != nil {
		t.Fatal(err)
	}
	created, err = q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("enqueue after completion suppressed")
	}
}

func TestClaimIsolatesKindsAndOrdersOldestFirst(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	s1, e1 := pgtest.NewSession(t, pool, "cloud")
	s2, e2 := pgtest.NewSession(t, pool, "cloud")
	if _, err := q.Enqueue(ctx, pool, e1, s1, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, pool, e1, s1, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, pool, e2, s2, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}

	first, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim: %+v %v", first, err)
	}
	second, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second claim: %+v %v", second, err)
	}
	if first.SessionID != s1 || second.SessionID != s2 {
		t.Errorf("claim order: got %s then %s, want %s then %s", first.SessionID, second.SessionID, s1, s2)
	}
	third, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if third != nil {
		t.Errorf("model_turn claim returned tool_exec work: %+v", third)
	}
}

func TestParallelClaimsNeverShareAnItem(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	const n = 8
	for range n {
		s, e := pgtest.NewSession(t, pool, "cloud")
		if _, err := q.Enqueue(ctx, pool, e, s, queue.ModelTurn); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[domain.ID]bool{}
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
			if err != nil || item == nil {
				t.Errorf("parallel claim: %+v %v", item, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[item.ID] {
				t.Errorf("item %s claimed twice", item.ID)
			}
			seen[item.ID] = true
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Errorf("claimed %d distinct items, want %d", len(seen), n)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, 50*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}

	// Not expired yet: nothing to claim.
	if got, err := q.Claim(ctx, queue.ModelTurn, time.Minute); err != nil || got != nil {
		t.Fatalf("claim before expiry: %+v %v", got, err)
	}

	time.Sleep(60 * time.Millisecond)
	re, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || re == nil {
		t.Fatalf("claim after expiry: %+v %v", re, err)
	}
	if re.ID != item.ID {
		t.Errorf("reclaimed a different item: %s vs %s", re.ID, item.ID)
	}
	if !re.Reclaimed {
		t.Error("expired-lease claim not flagged as reclaim")
	}

	// The first claimant lost the lease: its Complete must fail loudly.
	if err := q.Complete(ctx, pool, item); err == nil {
		t.Error("Complete after losing the lease succeeded silently")
	} else {
		// The new claimant still owns it.
		if err := q.Complete(ctx, pool, re); err != nil {
			t.Errorf("new claimant Complete: %v", err)
		}
	}
}

func TestExtendRenewsTheLease(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, 50*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	if err := q.Extend(ctx, item, time.Minute); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if got, err := q.Claim(ctx, queue.ModelTurn, time.Minute); err != nil || got != nil {
		t.Errorf("extended lease was reclaimed: %+v %v", got, err)
	}

	if err := q.Complete(ctx, pool, item); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Extending a finished item is a lost lease, surfaced as an error.
	if err := q.Extend(ctx, item, time.Minute); err == nil {
		t.Error("Extend after completion succeeded silently")
	}
}

func TestAssertProvesOwnership(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, 50*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	if err := q.Assert(ctx, pool, item); err != nil {
		t.Fatalf("Assert while owning the lease: %v", err)
	}

	// After expiry and a reclaim, the original claimant's proof is dead.
	time.Sleep(60 * time.Millisecond)
	re, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || re == nil {
		t.Fatalf("reclaim: %+v %v", re, err)
	}
	if err := q.Assert(ctx, pool, item); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("Assert after losing the lease = %v, want ErrLeaseLost", err)
	}
	if err := q.Assert(ctx, pool, re); err != nil {
		t.Errorf("new claimant Assert: %v", err)
	}
}

// A claimant that asserts its lease and then writes must not find the item
// reclaimed underneath the open transaction: the proof has to hold until the
// commit, not just at the instant it is read. The stalled executor's partial
// commit is where this bites — it settles with nothing renewing its lease, so
// the row is reclaimable the whole time it is writing (#383).
func TestAssertHoldsTheItemAgainstAConcurrentReclaim(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, 50*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	// Let the lease lapse with no renewal, then settle under it.
	time.Sleep(60 * time.Millisecond)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := q.Assert(ctx, tx, item); err != nil {
		t.Fatalf("Assert inside the settling transaction: %v", err)
	}

	re, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil {
		t.Fatalf("concurrent claim: %v", err)
	}
	if re != nil {
		t.Fatalf("reclaimed %s while its holder was settling: two claimants own it", re.ID)
	}

	// Once the settlement commits, the lapsed lease is reclaimable again — the
	// lock defers the reclaim, it does not cancel it.
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if re, err = q.Claim(ctx, queue.ModelTurn, time.Minute); err != nil || re == nil {
		t.Fatalf("reclaim after the settlement committed: %+v %v", re, err)
	}
}

func TestEnqueueUnknownSessionFails(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)
	if _, err := q.Enqueue(ctx, pool, domain.NewID("env"), domain.NewID("sesn"), queue.ModelTurn); err == nil {
		t.Error("enqueue against missing session/environment succeeded")
	}
}

func TestRequeueHandsTheItemBack(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}

	// While active, a fresh enqueue is suppressed — Requeue is the only way
	// to chain follow-on work under the live slot.
	if err := q.Requeue(ctx, pool, item); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	re, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || re == nil {
		t.Fatalf("claim after requeue: %+v %v", re, err)
	}
	if re.ID != item.ID {
		t.Errorf("requeued claim = %s, want %s", re.ID, item.ID)
	}
	if re.Reclaimed {
		t.Error("requeued item flagged as a reclaim (it was handed back cleanly)")
	}

	// The old claimant's lease died with the requeue.
	if err := q.Complete(ctx, pool, item); err == nil {
		t.Error("Complete with the pre-requeue lease succeeded")
	}
	if err := q.Complete(ctx, pool, re); err != nil {
		t.Errorf("Complete with the fresh lease: %v", err)
	}
}

// TestPollReservesWithoutTransition pins the wire work API's poll: it hands out
// the oldest queued tool_exec item for one environment as a soft reservation —
// the item stays queued (ack transitions it) and a second poll inside the
// reclaim window will not re-hand it out, but once the reservation lapses a
// later poll reclaims it. Poll is environment-scoped and tool_exec-only.
func TestPollReservesWithoutTransition(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "self_hosted")
	q := queue.New(pool)

	// A model_turn item on the same environment is never handed to a worker.
	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	// Empty of tool_exec work: poll returns nothing.
	if w, err := q.Poll(ctx, envID, time.Minute); err != nil || w != nil {
		t.Fatalf("poll with only model_turn work: %+v %v", w, err)
	}

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, err := q.Poll(ctx, envID, 50*time.Millisecond)
	if err != nil || w == nil {
		t.Fatalf("poll: %+v %v", w, err)
	}
	if w.SessionID != sessionID || w.EnvironmentID != envID {
		t.Errorf("work = %+v", w)
	}
	if w.State != "queued" {
		t.Errorf("polled work state = %q, want queued (ack transitions it)", w.State)
	}
	if !domain.ID(w.ID).HasPrefix("work") {
		t.Errorf("work id %q not work_-prefixed", w.ID)
	}

	// Reserved: a second poll inside the window hands out nothing.
	if got, err := q.Poll(ctx, envID, time.Minute); err != nil || got != nil {
		t.Fatalf("poll inside reclaim window: %+v %v", got, err)
	}
	// Reservation lapses: a later poll re-offers the same still-queued item — the
	// same row, under a fresh identity (TestEveryReHandOutMintsAFreshWorkIdentity).
	time.Sleep(60 * time.Millisecond)
	re, err := q.Poll(ctx, envID, time.Minute)
	if err != nil || re == nil {
		t.Fatalf("poll after reclaim window: %+v %v", re, err)
	}
	if re.SessionID != w.SessionID || !re.CreatedAt.Equal(w.CreatedAt) {
		t.Errorf("re-offered a different item: %+v vs %+v", re, w)
	}
	if re.ID == w.ID {
		t.Errorf("re-offered under the same id %s, want a fresh one", re.ID)
	}
}

// TestEnqueueCapturesTraceContext pins the enqueue→poll leg of cross-process
// tracing: an item enqueued under an active span carries that span's W3C trace
// context, so a worker that later polls it can parent its tool-execution spans
// on the enqueuing turn — one trace across the control-plane→worker boundary. An
// item enqueued with no active span carries no trace context at all.
func TestEnqueueCapturesTraceContext(t *testing.T) {
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	tracedSess, tracedEnv := pgtest.NewSession(t, pool, "self_hosted")
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	if _, err := q.Enqueue(ctx, pool, tracedEnv, tracedSess, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	w, err := q.Poll(context.Background(), tracedEnv, time.Minute)
	if err != nil || w == nil {
		t.Fatalf("poll: %+v %v", w, err)
	}
	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
	if got := w.TraceContext["traceparent"]; got != want {
		t.Errorf("polled trace_context[traceparent] = %q, want %q", got, want)
	}
	// The stored form re-extracts to the same trace — exactly what the worker does.
	if got := trace.SpanContextFromContext(telemetry.Extract(context.Background(), w.TraceContext)); got.TraceID() != sc.TraceID() {
		t.Errorf("extracted trace id = %s, want %s", got.TraceID(), sc.TraceID())
	}

	// No active span → no trace context (SQL NULL → nil map), not an empty object.
	plainSess, plainEnv := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := q.Enqueue(context.Background(), pool, plainEnv, plainSess, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	pw, err := q.Poll(context.Background(), plainEnv, time.Minute)
	if err != nil || pw == nil {
		t.Fatalf("poll plain: %+v %v", pw, err)
	}
	if len(pw.TraceContext) != 0 {
		t.Errorf("untraced enqueue carried trace context: %v", pw.TraceContext)
	}

	// A model_turn enqueued under the same span carries NO trace context: the
	// brain opens its own model_request span per turn and never reads this back,
	// so only tool_exec (the worker/executor's tool run) captures it.
	if _, err := q.Enqueue(ctx, pool, tracedEnv, tracedSess, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	var mtTrace *string
	if err := pool.QueryRow(context.Background(),
		`SELECT trace_context->>'traceparent' FROM work_items WHERE session_id=$1 AND kind='model_turn'`,
		tracedSess.String()).Scan(&mtTrace); err != nil {
		t.Fatalf("read model_turn trace_context: %v", err)
	}
	if mtTrace != nil {
		t.Errorf("model_turn carried trace context %q, want none", *mtTrace)
	}
}

// TestPollIsEnvironmentScoped pins that a poll only ever hands out its own
// environment's work — the Bearer key is environment-scoped, so one
// environment's worker must never see another's queue.
func TestPollIsEnvironmentScoped(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessA, envA := pgtest.NewSession(t, pool, "self_hosted")
	_, envB := pgtest.NewSession(t, pool, "self_hosted")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envA, sessA, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if w, err := q.Poll(ctx, envB, time.Minute); err != nil || w != nil {
		t.Fatalf("poll env B saw env A's work: %+v %v", w, err)
	}
	w, err := q.Poll(ctx, envA, time.Minute)
	if err != nil || w == nil {
		t.Fatalf("poll env A: %+v %v", w, err)
	}
	if w.EnvironmentID != envA {
		t.Errorf("work environment = %s, want %s", w.EnvironmentID, envA)
	}
}

// TestClaimAndPollAreExclusiveByEnvironmentKind pins the cloud/self_hosted
// split at the queue: the executor's Claim(tool_exec) serves only cloud
// environments and Poll serves only self_hosted, so a self_hosted item a worker
// polls is never also run by the executor. model_turn is claimed for every
// environment — the brain runs on the platform regardless of sandbox location.
func TestClaimAndPollAreExclusiveByEnvironmentKind(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	cloudSess, cloudEnv := pgtest.NewSession(t, pool, "cloud")
	selfSess, selfEnv := pgtest.NewSession(t, pool, "self_hosted")
	q := queue.New(pool)

	// A self_hosted tool_exec item: Poll serves it, Claim never does.
	if _, err := q.Enqueue(ctx, pool, selfEnv, selfSess, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if it, err := q.Claim(ctx, queue.ToolExec, time.Minute); err != nil || it != nil {
		t.Fatalf("executor claimed a self_hosted tool_exec item: %+v %v", it, err)
	}
	if w, err := q.Poll(ctx, selfEnv, time.Minute); err != nil || w == nil {
		t.Fatalf("poll did not serve the self_hosted item: %+v %v", w, err)
	}

	// A cloud tool_exec item: Claim serves it, Poll never does.
	if _, err := q.Enqueue(ctx, pool, cloudEnv, cloudSess, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if w, err := q.Poll(ctx, cloudEnv, time.Minute); err != nil || w != nil {
		t.Fatalf("poll served a cloud tool_exec item: %+v %v", w, err)
	}
	it, err := q.Claim(ctx, queue.ToolExec, time.Minute)
	if err != nil || it == nil {
		t.Fatalf("executor did not claim the cloud item: %+v %v", it, err)
	}
	if it.EnvironmentID != cloudEnv {
		t.Errorf("claimed item environment = %s, want the cloud env %s", it.EnvironmentID, cloudEnv)
	}

	// model_turn is claimed regardless of environment kind (the brain is not
	// split): a self_hosted session's model turn still runs on the platform.
	if _, err := q.Enqueue(ctx, pool, selfEnv, selfSess, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	mt, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || mt == nil {
		t.Fatalf("brain did not claim a self_hosted model_turn: %+v %v", mt, err)
	}
	if mt.EnvironmentID != selfEnv {
		t.Errorf("model_turn env = %s, want %s", mt.EnvironmentID, selfEnv)
	}
}

// TestWebExecIsClaimedForEveryEnvironmentAndNeverPolled pins web_exec's
// routing (docs/plan/15_web-tools.md): web_fetch/web_search run in the
// platform executor's own process for cloud AND self_hosted sessions alike,
// so Claim(web_exec) serves both environment kinds — and Poll never serves
// it, because the official worker toolset implements only the six sandbox
// tools and would fail an unknown tool. Like tool_exec, it carries the
// enqueuing turn's trace context for the web driver's span to parent on.
func TestWebExecIsClaimedForEveryEnvironmentAndNeverPolled(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	cloudSess, cloudEnv := pgtest.NewSession(t, pool, "cloud")
	selfSess, selfEnv := pgtest.NewSession(t, pool, "self_hosted")
	q := queue.New(pool)

	// A self_hosted web_exec item: Poll must never hand it to a BYOC worker;
	// the platform executor claims it despite the environment kind.
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f},
		SpanID:     trace.SpanID{0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38},
		TraceFlags: trace.FlagsSampled,
	})
	if _, err := q.Enqueue(trace.ContextWithSpanContext(ctx, sc), pool, selfEnv, selfSess, queue.WebExec); err != nil {
		t.Fatal(err)
	}
	if w, err := q.Poll(ctx, selfEnv, time.Minute); err != nil || w != nil {
		t.Fatalf("poll served a web_exec item to a worker: %+v %v", w, err)
	}
	it, err := q.Claim(ctx, queue.WebExec, time.Minute)
	if err != nil || it == nil {
		t.Fatalf("executor did not claim the self_hosted web_exec item: %+v %v", it, err)
	}
	if it.EnvironmentID != selfEnv || it.Kind != queue.WebExec {
		t.Errorf("claimed item = %+v, want the self_hosted web_exec item", it)
	}
	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
	if got := it.TraceContext["traceparent"]; got != want {
		t.Errorf("claimed trace_context[traceparent] = %q, want %q", got, want)
	}

	// A cloud web_exec item claims the same way.
	if _, err := q.Enqueue(ctx, pool, cloudEnv, cloudSess, queue.WebExec); err != nil {
		t.Fatal(err)
	}
	ci, err := q.Claim(ctx, queue.WebExec, time.Minute)
	if err != nil || ci == nil {
		t.Fatalf("executor did not claim the cloud web_exec item: %+v %v", ci, err)
	}
	if ci.EnvironmentID != cloudEnv {
		t.Errorf("claimed item environment = %s, want the cloud env %s", ci.EnvironmentID, cloudEnv)
	}
}

// TestOutputsHarvestIsClaimedAndNeverPolled pins the deliverables harvest's
// routing (docs/plan/21_outcomes.md, Decision 8): outputs_harvest is
// internal-only work the platform executor claims, invisible to the wire work
// API — Poll must never hand it to a BYOC worker, whose toolset has no file
// lane. Like tool_exec it carries the enqueuing turn's trace context, so the
// harvest's consumer span parents on the settlement that scheduled it.
func TestOutputsHarvestIsClaimedAndNeverPolled(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sess, env := pgtest.NewSession(t, pool, "cloud")
	selfSess, selfEnv := pgtest.NewSession(t, pool, "self_hosted")
	q := queue.New(pool)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f},
		SpanID:     trace.SpanID{0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58},
		TraceFlags: trace.FlagsSampled,
	})
	if _, err := q.Enqueue(trace.ContextWithSpanContext(ctx, sc), pool, env, sess, queue.OutputsHarvest); err != nil {
		t.Fatal(err)
	}
	if w, err := q.Poll(ctx, env, time.Minute); err != nil || w != nil {
		t.Fatalf("poll served an outputs_harvest item to a worker: %+v %v", w, err)
	}
	it, err := q.Claim(ctx, queue.OutputsHarvest, time.Minute)
	if err != nil || it == nil {
		t.Fatalf("executor did not claim the outputs_harvest item: %+v %v", it, err)
	}
	if it.SessionID != sess || it.Kind != queue.OutputsHarvest {
		t.Errorf("claimed item = %+v, want the session's outputs_harvest item", it)
	}
	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
	if got := it.TraceContext["traceparent"]; got != want {
		t.Errorf("claimed trace_context[traceparent] = %q, want %q", got, want)
	}

	// Only the brain's cloud settlement enqueues the kind, but the queue does
	// not enforce that — pin that a self_hosted item, were one ever enqueued,
	// still never reaches a worker's poll.
	if _, err := q.Enqueue(ctx, pool, selfEnv, selfSess, queue.OutputsHarvest); err != nil {
		t.Fatal(err)
	}
	if w, err := q.Poll(ctx, selfEnv, time.Minute); err != nil || w != nil {
		t.Fatalf("poll served a self_hosted outputs_harvest item: %+v %v", w, err)
	}
}

// TestMCPExecIsClaimedForEveryEnvironmentAndNeverPolled pins mcp_exec's
// routing (docs/plan/29_mcp-toolset.md): MCP tool discovery and execution are
// server-side on every environment kind — the SDK's session tool runner says so
// three times, the work API has no MCP surface at all, and the BYOC worker's
// contract is agent.tool_use + agent.custom_tool_use only. So Claim serves both
// environment kinds, Poll serves neither, and the item carries the enqueuing
// turn's trace context for the MCP driver's consumer span to parent on — the
// web_exec precedent exactly.
func TestMCPExecIsClaimedForEveryEnvironmentAndNeverPolled(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	cloudSess, cloudEnv := pgtest.NewSession(t, pool, "cloud")
	selfSess, selfEnv := pgtest.NewSession(t, pool, "self_hosted")
	q := queue.New(pool)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f},
		SpanID:     trace.SpanID{0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78},
		TraceFlags: trace.FlagsSampled,
	})
	if _, err := q.Enqueue(trace.ContextWithSpanContext(ctx, sc), pool, selfEnv, selfSess, queue.MCPExec); err != nil {
		t.Fatal(err)
	}
	if w, err := q.Poll(ctx, selfEnv, time.Minute); err != nil || w != nil {
		t.Fatalf("poll served an mcp_exec item to a worker: %+v %v", w, err)
	}
	it, err := q.Claim(ctx, queue.MCPExec, time.Minute)
	if err != nil || it == nil {
		t.Fatalf("executor did not claim the self_hosted mcp_exec item: %+v %v", it, err)
	}
	if it.EnvironmentID != selfEnv || it.Kind != queue.MCPExec {
		t.Errorf("claimed item = %+v, want the self_hosted mcp_exec item", it)
	}
	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
	if got := it.TraceContext["traceparent"]; got != want {
		t.Errorf("claimed trace_context[traceparent] = %q, want %q", got, want)
	}

	if _, err := q.Enqueue(ctx, pool, cloudEnv, cloudSess, queue.MCPExec); err != nil {
		t.Fatal(err)
	}
	ci, err := q.Claim(ctx, queue.MCPExec, time.Minute)
	if err != nil || ci == nil {
		t.Fatalf("executor did not claim the cloud mcp_exec item: %+v %v", ci, err)
	}
	if ci.EnvironmentID != cloudEnv {
		t.Errorf("claimed item environment = %s, want the cloud env %s", ci.EnvironmentID, cloudEnv)
	}
}

func TestCancelSessionTakesEveryLiveItem(t *testing.T) {
	// What a user.interrupt does to the turn it ends: every item the session
	// still has in flight is stopped, whoever holds it. A claimant's lease proof
	// dies with the item, which is what stops a brain or executor still working
	// the interrupted turn from committing anything, and the freed slot lets the
	// redirect's own turn be enqueued in the same transaction.
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	other, otherEnv := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	// Every kind a session can have in flight, mcp_exec included: cancel is
	// written against the session rather than a kind list, and a test that
	// enqueued a subset would stay green for a cancel that had grown one.
	for _, kind := range []queue.Kind{queue.ModelTurn, queue.ToolExec, queue.WebExec, queue.OutputsHarvest, queue.MCPExec} {
		if _, err := q.Enqueue(ctx, pool, envID, sessionID, kind); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Enqueue(ctx, pool, otherEnv, other, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	// One claimed (active), one still queued.
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	// Two sessions have a model_turn queued, so pin which one this claim holds:
	// the lost-lease assertions below only mean anything about the cancel if the
	// claimed item is the cancelled session's.
	if item.SessionID != sessionID {
		t.Fatalf("claimed item session = %s, want the session about to be cancelled %s", item.SessionID, sessionID)
	}

	if err := q.CancelSession(ctx, pool, sessionID); err != nil {
		t.Fatalf("CancelSession: %v", err)
	}

	if err := q.Complete(ctx, pool, item); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("Complete after cancel = %v, want ErrLeaseLost", err)
	}
	if err := q.Assert(ctx, pool, item); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("Assert after cancel = %v, want ErrLeaseLost", err)
	}
	// Requeue is the third lease-asserted write — a brain chaining its own item
	// into the next turn — and must fail with the other two, or a cancelled turn
	// could hand itself back to the queue and run on.
	if err := q.Requeue(ctx, pool, item); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("Requeue after cancel = %v, want ErrLeaseLost", err)
	}
	if got := liveItems(t, pool, sessionID); got != 0 {
		t.Errorf("live items after cancel = %d, want 0", got)
	}
	// The slot is free again, so the redirect turn schedules immediately.
	created, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("enqueue after cancel was suppressed, so no turn would run")
	}
	// Another session's work is untouched.
	if got := liveItems(t, pool, other); got != 1 {
		t.Errorf("other session's live items = %d, want 1", got)
	}
}

// TestEnqueueNotifiesWorkChannelOnCommit pins the long-poll wake producer
// (#74): a transactional tool_exec Enqueue rides the work-items NOTIFY on the
// caller's transaction, so an environment-keyed subscriber (the work API's
// long poll) wakes only once the item is committed — never for a row a
// re-poll cannot yet see.
func TestEnqueueNotifiesWorkChannelOnCommit(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "self_hosted")
	q := queue.New(pool)

	b := events.NewBroker(pool)
	sub := b.Subscribe(envID)
	defer sub.Close()
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := b.Ready(readyCtx); err != nil {
		t.Fatalf("broker never became ready: %v", err)
	}
	// The listener wakes every subscriber once when LISTEN becomes active
	// (broker.listen calls setReady then wakeAll), and Ready can return between
	// those two steps — so this coverage-start wake may still be in flight here.
	// Wait for it rather than draining non-blocking: a non-blocking drain that
	// misses it leaves it to arrive inside the pre-commit window below, where it
	// trips the "woken before the enqueue committed" assertion under load (#486).
	// After it is drained the 1-buffered channel is empty, so the only wake that
	// can follow is the enqueue's own commit NOTIFY.
	select {
	case <-sub.Wake():
	case <-time.After(10 * time.Second):
		t.Fatal("coverage-start wake never arrived to be drained")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	created, err := q.Enqueue(ctx, tx, envID, sessionID, queue.ToolExec)
	if err != nil || !created {
		t.Fatalf("enqueue in tx: created=%v err=%v", created, err)
	}
	// Postgres delivers NOTIFY only on commit; a wake now would mean the
	// producer bypassed the caller's transaction.
	select {
	case <-sub.Wake():
		t.Fatal("woken before the enqueue committed")
	case <-time.After(150 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Wake():
	case <-time.After(10 * time.Second):
		t.Fatal("no wake after the enqueue committed")
	}
}

func liveItems(t *testing.T, pool *pgxpool.Pool, sessionID domain.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE session_id = $1 AND state <> 'stopped'`,
		sessionID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// RequeueSettlement is Requeue plus the consecutive-settlement count the
// delegation chain is bounded by (#442). It is a fourth lease-asserted write,
// so it gets the same two tests Requeue has, plus the two things only it does:
// carry the count forward, and — through plain Requeue — clear it again.
func TestRequeueSettlementCarriesAndClearsTheCount(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	if item.Chain != 0 {
		t.Errorf("fresh item Chain = %d, want 0", item.Chain)
	}

	// The count rides on the item, so the next claimant — which may be a
	// different brain — sees the same run.
	if err := q.RequeueSettlement(ctx, pool, item, item.Chain+1); err != nil {
		t.Fatalf("RequeueSettlement: %v", err)
	}
	second, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || second == nil {
		t.Fatalf("claim after settlement requeue: %+v %v", second, err)
	}
	if second.ID != item.ID || second.Chain != 1 {
		t.Fatalf("claim = %s chain %d, want %s chain 1", second.ID, second.Chain, item.ID)
	}
	if err := q.RequeueSettlement(ctx, pool, second, second.Chain+1); err != nil {
		t.Fatalf("RequeueSettlement: %v", err)
	}
	third, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || third == nil {
		t.Fatalf("claim: %+v %v", third, err)
	}
	if third.Chain != 2 {
		t.Errorf("Chain = %d, want 2", third.Chain)
	}

	// Plain Requeue is every other reason to hand an item back, and every
	// other reason is progress: it clears the run.
	if err := q.Requeue(ctx, pool, third); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	fourth, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || fourth == nil {
		t.Fatalf("claim: %+v %v", fourth, err)
	}
	if fourth.Chain != 0 {
		t.Errorf("Chain after a plain requeue = %d, want 0", fourth.Chain)
	}
}

// The lease proof is the same one Requeue asserts: a claimant that lost the
// item cannot hand it back, or two brains would run the same turn.
func TestRequeueSettlementRequiresTheLease(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	stale := *item
	stale.Lease = stale.Lease.Add(-time.Hour)

	if err := q.RequeueSettlement(ctx, pool, &stale, 1); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("RequeueSettlement with a stale lease = %v, want ErrLeaseLost", err)
	}
}

// work_items.metadata is a client-visible field on a tool_exec item, and the
// only reason the settlement count may live there is that nothing writes it
// onto one: the executor's requeue passes no count, and a zero count removes
// the key rather than writing it. Asserted on the executor's own kind — a
// cloud tool_exec, the only tool_exec Claim serves — because a wire-visible
// self_hosted item is polled, never claimed, so no Requeue can reach it at
// all. Work.Metadata is map[string]string, so an integer landing here would
// not merely leak: it would fail the scan.
func TestRequeueLeavesAToolExecItemsMetadataAlone(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sessionID, envID := pgtest.NewSession(t, pool, "cloud")
	q := queue.New(pool)

	if _, err := q.Enqueue(ctx, pool, envID, sessionID, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	item, err := q.Claim(ctx, queue.ToolExec, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE work_items SET metadata = '{"worker":"w-1"}'::jsonb WHERE id = $1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Requeue(ctx, pool, item); err != nil {
		t.Fatalf("Requeue: %v", err)
	}

	var got map[string]any
	if err := pool.QueryRow(ctx, `SELECT metadata FROM work_items WHERE id = $1`, item.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got["worker"] != "w-1" {
		t.Errorf("metadata = %v, want the client's own key kept", got)
	}
	if _, ok := got["settlement_chain"]; ok {
		t.Errorf("metadata = %v, want no settlement_chain on a client-visible item", got)
	}
}
