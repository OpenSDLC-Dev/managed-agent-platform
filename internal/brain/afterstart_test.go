package brain_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// onQuery runs hook the first time the brain sends a query whose SQL holds
// match — the one seam into the stretch between the span start's commit and
// the model call, which no fake provider can reach. hook may return a context
// to run the query under (a cancelled one fails it), or nil to leave it be.
type onQuery struct {
	match []string // every one of these must be in the SQL
	once  sync.Once
	hook  func(ctx context.Context) context.Context
}

func (o *onQuery) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	for _, m := range o.match {
		if !strings.Contains(d.SQL, m) {
			return ctx
		}
	}
	out := ctx
	o.once.Do(func() {
		if c := o.hook(ctx); c != nil {
			out = c
		}
	})
	return out
}

func (o *onQuery) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// tracedBrain rebuilds the harness's brain over a pool of its own whose
// queries o watches; the harness keeps its own pool for the test's writes.
func (h *harness) tracedBrain(t *testing.T, o *onQuery) {
	t.Helper()
	cfg := h.pool.Config()
	cfg.ConnConfig.Tracer = o
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h.brain = brain.New(pool, h.registry, nil, brain.Config{})
}

// requestHistoryQuery is what only the request's own history read sends: a
// list of the thread's rows bounded below the span start.
var requestHistoryQuery = []string{"SELECT id, seq, type, payload", "AND seq < $"}

// The span start proves the claimant holds the item, but the history read and
// the request build come after it, and an interrupt can stop the item in
// between (queue.CancelSession). The claimant must find that out before it
// calls the model: a call made anyway is billed for a turn nobody may commit.
// Ownership is proven again right before the call.
func TestAClaimantThatLostItsItemAfterTheStartNeverCallsTheModel(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "must not run"), done("end_turn", 1)}}, nil)
	h.wake(t, "one")
	h.tracedBrain(t, &onQuery{match: requestHistoryQuery, hook: func(context.Context) context.Context {
		// What an interrupt's commit does to the turn it ends.
		if err := h.queue.CancelSession(context.Background(), h.pool, h.sessionID); err != nil {
			t.Errorf("cancel: %v", err)
		}
		return nil
	}})

	found, err := h.brain.RunOnce(context.Background())
	if !found || !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("RunOnce = %v, %v; want the turn abandoned on a lost lease", found, err)
	}
	if n := len(h.provider.calls); n != 0 {
		t.Errorf("provider called %d times by a claimant that had lost its item", n)
	}
}

// A history read that fails after the span start must not strand the turn
// behind its lease: the start is already committed, its inputs stamped, so the
// request is closed with an errored span.model_request_end and the item goes
// straight back to the queue — the retry's own start stamps nothing new, and
// no session.error is written for a fault that was the platform's, not the
// model's.
func TestAHistoryReadFailingAfterTheStartReleasesTheItemAtOnce(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "answered"), done("end_turn", 1)}}, nil)
	h.wake(t, "one")
	h.tracedBrain(t, &onQuery{match: requestHistoryQuery, hook: func(ctx context.Context) context.Context {
		failed, cancel := context.WithCancel(ctx)
		cancel()
		return failed
	}})

	if _, err := h.brain.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v; want the fault handled by releasing the item", err)
	}
	if n := len(h.provider.calls); n != 0 {
		t.Fatalf("provider called %d times without a history", n)
	}
	ends := h.eventsOfType(t, "span.model_request_end")
	var end struct {
		IsError bool `json:"is_error"`
	}
	if len(ends) != 1 || json.Unmarshal(ends[0].Body, &end) != nil || !end.IsError {
		t.Fatalf("span ends = %d, want one errored end closing the start", len(ends))
	}
	if n := h.countType(t, "session.error"); n != 0 {
		t.Errorf("session.error written %d times for a platform fault", n)
	}
	// Claimable now, not after the lease: the retry runs the request.
	h.runOnce(t)
	if n := len(h.provider.calls); n != 1 {
		t.Fatalf("provider calls after the retry = %d, want 1", n)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle after the retried turn", got)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("%d model_turn items live, want 0", n)
	}
}
