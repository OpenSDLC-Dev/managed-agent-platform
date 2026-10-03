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

// The checks a send runs against the log tell the client's mistakes from
// their own faults by type, so the API answers the first with a 4xx and the
// second with a 500 that carries none of the fault's text (#841): an interrupt
// naming no thread of the session is a *ThreadNotFoundError, a reference
// naming no call or an outcome naming no rubric file a *Refusal, and a read
// the database cannot finish — here under a cancelled context — neither.
func TestSendChecksTellRefusalsFromFaults(t *testing.T) {
	pool := pgtest.NewPool(t)
	sid, _ := pgtest.NewSession(t, pool, "self_hosted")
	normalize := func(ev string) []events.NewEvent {
		t.Helper()
		evs, err := events.NormalizeInbound("self_hosted", events.EnvironmentCredential, []json.RawMessage{json.RawMessage(ev)})
		if err != nil {
			t.Fatalf("normalize %s: %v", ev, err)
		}
		return evs
	}
	noCall := domain.NewID(domain.PrefixEvent).String()
	route := func(ctx context.Context, ev string) error {
		_, err := events.RouteInbound(ctx, pool, sid, normalize(ev))
		return err
	}
	outcomes := func(ctx context.Context, ev string) error {
		t.Helper()
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		defs, err := events.DefineOutcomes(normalize(ev))
		if err != nil {
			t.Fatalf("parse %s: %v", ev, err)
		}
		return events.ValidateDefineOutcomes(ctx, tx, sid, defs, false)
	}
	checks := []struct {
		name string
		run  func(ctx context.Context) error
	}{
		{"an interrupt naming no thread", func(ctx context.Context) error {
			return route(ctx, `{"type":"user.interrupt","session_thread_id":"`+domain.NewID(domain.PrefixSessionThread).String()+`"}`)
		}},
		{"a confirmation naming no call, routed", func(ctx context.Context) error {
			return route(ctx, `{"type":"user.tool_confirmation","result":"allow","tool_use_id":"`+noCall+`"}`)
		}},
		{"a tool result naming no call", func(ctx context.Context) error {
			return events.ValidateToolResults(ctx, pool, sid, normalize(`{"type":"user.tool_result","tool_use_id":"`+noCall+`"}`), nil)
		}},
		{"a confirmation naming no call", func(ctx context.Context) error {
			return events.ValidateToolConfirmations(ctx, pool, sid, normalize(`{"type":"user.tool_confirmation","result":"allow","tool_use_id":"`+noCall+`"}`))
		}},
		{"an outcome naming no rubric file", func(ctx context.Context) error {
			return outcomes(ctx, `{"type":"user.define_outcome","description":"d","rubric":{"type":"file","file_id":"`+domain.NewID(domain.PrefixFile).String()+`"}}`)
		}},
	}

	var noThread *events.ThreadNotFoundError
	var refusal *events.Refusal
	var noPending *events.NoPendingConfirmationError
	for _, c := range checks {
		err := c.run(context.Background())
		if !errors.As(err, &noThread) && !errors.As(err, &refusal) && !errors.As(err, &noPending) {
			t.Errorf("%s: err = %v (%T), want a client refusal", c.name, err, err)
		}
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range checks {
		err := c.run(cancelled)
		if err == nil || errors.As(err, &noThread) || errors.As(err, &refusal) || errors.As(err, &noPending) {
			t.Errorf("%s under a cancelled context: err = %v (%T), want a fault, not a refusal", c.name, err, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s under a cancelled context: err = %v, want it to wrap context.Canceled", c.name, err)
		}
	}
}

// A define_outcome payload that does not parse is the client's batch: a
// *Refusal carrying the parse error's text, which is what
// ValidateDefineOutcomes answered when it parsed the payloads itself, and what
// a send and a create still answer in its place, ahead of its checks. The
// normalizer writes every payload it admits, so only a payload it did not
// write reaches this.
func TestDefineOutcomesRefusesAPayloadThatDoesNotParse(t *testing.T) {
	bad := json.RawMessage(`{"description":"d","max_iterations":"three"}`)
	_, parseErr := events.ParseDefineOutcome(bad)
	if parseErr == nil {
		t.Fatal("the payload parsed; pick one that does not")
	}
	_, err := events.DefineOutcomes([]events.NewEvent{
		{Type: domain.EventUserMessage, Payload: json.RawMessage(`{}`)},
		{Type: domain.EventUserDefineOutcome, Payload: bad},
	})
	var refusal *events.Refusal
	if !errors.As(err, &refusal) || err.Error() != parseErr.Error() {
		t.Errorf("DefineOutcomes => %v (%T), want a *Refusal reading %q", err, err, parseErr)
	}
}
