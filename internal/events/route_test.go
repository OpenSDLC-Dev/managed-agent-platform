package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// RouteInbound tells the client's mistakes from its own faults by type, so the
// API answers the first with a 4xx and the second with a 500 that carries none
// of the fault's text (#841): an interrupt naming no thread of the session is a
// *ThreadNotFoundError, an answer naming no call a *RouteRefusal, and a read
// the database cannot finish — here under a cancelled context — neither.
func TestRouteInboundTellsRefusalsFromFaults(t *testing.T) {
	pool := pgtest.NewPool(t)
	sid, _ := pgtest.NewSession(t, pool, "cloud")
	route := func(ctx context.Context, ev string) error {
		t.Helper()
		evs, err := events.NormalizeInbound("cloud", events.ManagementCredential, []json.RawMessage{json.RawMessage(ev)})
		if err != nil {
			t.Fatalf("normalize %s: %v", ev, err)
		}
		_, err = events.RouteInbound(ctx, pool, sid, evs)
		return err
	}
	absentThread := `{"type":"user.interrupt","session_thread_id":"` + domain.NewID(domain.PrefixSessionThread).String() + `"}`

	var noThread *events.ThreadNotFoundError
	if err := route(context.Background(), absentThread); !errors.As(err, &noThread) {
		t.Errorf("an absent thread: err = %v (%T), want a *ThreadNotFoundError", err, err)
	}
	var refusal *events.RouteRefusal
	noCall := `{"type":"user.tool_confirmation","result":"allow","tool_use_id":"` + domain.NewID(domain.PrefixEvent).String() + `"}`
	if err := route(context.Background(), noCall); !errors.As(err, &refusal) {
		t.Errorf("a confirmation naming no call: err = %v (%T), want a *RouteRefusal", err, err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ev := range []string{absentThread, noCall} {
		err := route(cancelled, ev)
		if err == nil || errors.As(err, &noThread) || errors.As(err, &refusal) {
			t.Errorf("%s under a cancelled context: err = %v (%T), want a fault, not a refusal", ev, err, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s under a cancelled context: err = %v, want it to wrap context.Canceled", ev, err)
		}
	}
}
