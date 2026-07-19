package events_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// Direct coverage for toolflow.go — the checks the control plane runs over an
// inbound batch before it is appended. internal/api exercises them through the
// send handler, which normalizes payloads first; calling them directly is what
// reaches the branches the handler cannot present: each arm of the answered
// subquery's COALESCE, the session predicates on both sides of every EXISTS,
// and the nil-vs-empty binding of the extraRefs arrays.

// appendEvent appends one event with a caller-chosen id (empty = generated) and
// a literal payload, returning the id. The raw payload is the point: these
// tests plant shapes the API's NormalizeInbound rejects upstream.
func appendEvent(t *testing.T, log *events.Log, sid, id domain.ID, typ domain.EventType, payload string) domain.ID {
	t.Helper()
	got, err := log.Append(context.Background(), sid, []events.NewEvent{
		{ID: id, Type: typ, Payload: json.RawMessage(payload)},
	})
	if err != nil {
		t.Fatalf("append %s: %v", typ, err)
	}
	return got[0].ID
}

// toolUse appends a tool-use event carrying evaluated_permission perm; an empty
// perm omits the key entirely, which is a distinct case from an explicit null.
func toolUse(t *testing.T, log *events.Log, sid domain.ID, typ domain.EventType, perm string) domain.ID {
	t.Helper()
	payload := "{}"
	if perm != "" {
		payload = fmt.Sprintf(`{"evaluated_permission":%s}`, perm)
	}
	return appendEvent(t, log, sid, "", typ, payload)
}

func ask(t *testing.T, log *events.Log, sid domain.ID) domain.ID {
	t.Helper()
	return toolUse(t, log, sid, domain.EventAgentToolUse, `"ask"`)
}

// answerWith appends a result event carrying ref under payload key. key is
// explicit so each arm of the COALESCE can be driven on its own.
func answerWith(t *testing.T, log *events.Log, sid domain.ID, typ domain.EventType, key string, ref domain.ID) {
	t.Helper()
	appendEvent(t, log, sid, "", typ, fmt.Sprintf(`{%q:%q}`, key, ref.String()))
}

func confirmOnLog(t *testing.T, log *events.Log, sid, ref domain.ID) {
	t.Helper()
	appendEvent(t, log, sid, "", domain.EventUserToolConfirm, fmt.Sprintf(`{"tool_use_id":%q}`, ref.String()))
}

// inResult/inConfirm/inRaw build inbound events that are never appended:
// validation runs over the batch before the insert, and an un-appended value is
// the only way to present a payload AppendInTx would rewrite (it substitutes {}
// for an empty one).
func inResult(typ domain.EventType, key, ref string) events.NewEvent {
	return inRaw(typ, fmt.Sprintf(`{%q:%q}`, key, ref))
}

func inConfirm(ref string) events.NewEvent {
	return inResult(domain.EventUserToolConfirm, "tool_use_id", ref)
}

func inRaw(typ domain.EventType, payload string) events.NewEvent {
	return events.NewEvent{Type: typ, Payload: json.RawMessage(payload)}
}

func wantErrIs(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %q", want)
	}
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func wantErrHas(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want one containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want one containing %q", err, want)
	}
}

func TestToolResultRefs(t *testing.T) {
	// Batch order, preserved as given; non-result types contribute nothing.
	got := events.ToolResultRefs([]events.NewEvent{
		inResult(domain.EventUserToolResult, "tool_use_id", "b"),
		inRaw(domain.EventUserMessage, `{"content":[]}`),
		inResult(domain.EventUserCustomToolRes, "custom_tool_use_id", "a"),
		inRaw(domain.EventAgentToolUse, `{}`),
	})
	if want := []string{"b", "a"}; !slices.Equal(got, want) {
		t.Errorf("refs = %v, want %v (batch order, not sorted)", got, want)
	}

	// An unreadable ref drops silently. This function feeds the resume trigger,
	// not validation; ValidateToolResults rejects these same shapes instead
	// (TestValidateToolResults/malformed_payload), and the divergence is
	// deliberate.
	got = events.ToolResultRefs([]events.NewEvent{
		inResult(domain.EventUserToolResult, "tool_use_id", "V"),
		inRaw(domain.EventUserToolResult, `[1,2]`),
		inRaw(domain.EventUserToolResult, `{}`),
		inRaw(domain.EventUserToolResult, `{"tool_use_id":5}`),
		inRaw(domain.EventUserToolResult, `{"tool_use_id":null}`),
	})
	if want := []string{"V", ""}; !slices.Equal(got, want) {
		t.Errorf(`refs = %#v, want %#v (a JSON-null ref decodes to "", the rest drop)`, got, want)
	}

	// Nil, never an allocated empty slice. Downstream the two are equivalent —
	// the callers normalize nil before binding it (see the extra-refs cases) —
	// so this pins the representation the collectors actually return, which is
	// what makes that normalization the only thing standing between a nil and
	// the != ALL(NULL) trap.
	if got := events.ToolResultRefs(nil); got != nil {
		t.Errorf("ToolResultRefs(nil) = %#v, want nil", got)
	}
	if got := events.ToolResultRefs([]events.NewEvent{}); got != nil {
		t.Errorf("ToolResultRefs(empty) = %#v, want nil", got)
	}
}

func TestToolConfirmationRefs(t *testing.T) {
	// A non-confirmation event carrying a tool_use_id must not be collected.
	got := events.ToolConfirmationRefs([]events.NewEvent{
		inConfirm("b"),
		inResult(domain.EventUserMessage, "tool_use_id", "x"),
		inConfirm("a"),
	})
	if want := []string{"b", "a"}; !slices.Equal(got, want) {
		t.Errorf("refs = %v, want %v", got, want)
	}

	// The same event this function skips silently is a client error to
	// ValidateToolConfirmations. The nil Querier is the assertion that no query
	// ran: the payload decode fails before q is ever touched.
	bad := inRaw(domain.EventUserToolConfirm, `{"tool_use_id":5}`)
	if got := events.ToolConfirmationRefs([]events.NewEvent{inConfirm("a"), bad}); !slices.Equal(got, []string{"a"}) {
		t.Errorf("refs = %#v, want [a] (an unreadable ref drops)", got)
	}
	wantErrIs(t, events.ValidateToolConfirmations(context.Background(), nil, "sesn_x", []events.NewEvent{bad}),
		"events[0]: tool_use_id must be a string")

	if got := events.ToolConfirmationRefs(nil); got != nil {
		t.Errorf("ToolConfirmationRefs(nil) = %#v, want nil", got)
	}
}

func TestHasUnansweredToolUse(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()

	check := func(t *testing.T, sid domain.ID, extra []string) bool {
		t.Helper()
		got, err := events.HasUnansweredToolUse(ctx, pool, sid, extra)
		if err != nil {
			t.Fatalf("HasUnansweredToolUse: %v", err)
		}
		return got
	}

	t.Run("empty session", func(t *testing.T) {
		sid := newSession(t, pool)
		if check(t, sid, nil) {
			t.Error("empty session reports an unanswered tool use")
		}
		appendEvent(t, log, sid, "", domain.EventUserMessage, `{"content":[]}`)
		if check(t, sid, nil) {
			t.Error("a session with only messages reports an unanswered tool use")
		}
	})

	// All three tool-use kinds count, and each arm of the result COALESCE
	// resolves its own. The intermediate assertions matter: without them a
	// final false could come from one arm matching everything.
	t.Run("every kind, every coalesce arm", func(t *testing.T) {
		for _, tc := range []struct {
			use    domain.EventType
			result domain.EventType
			key    string
		}{
			{domain.EventAgentToolUse, domain.EventUserToolResult, "tool_use_id"},
			{domain.EventAgentCustomToolUse, domain.EventUserCustomToolRes, "custom_tool_use_id"},
			{domain.EventAgentMCPToolUse, domain.EventAgentMCPToolResult, "mcp_tool_use_id"},
			// The platform's own denial synthesis answers a built-in tool use
			// with an agent.tool_result rather than a user.tool_result.
			{domain.EventAgentToolUse, domain.EventAgentToolResult, "tool_use_id"},
		} {
			t.Run(string(tc.use)+"/"+string(tc.result), func(t *testing.T) {
				sid := newSession(t, pool)
				id := toolUse(t, log, sid, tc.use, "")
				if !check(t, sid, nil) {
					t.Fatalf("%s with no result reports answered", tc.use)
				}
				answerWith(t, log, sid, tc.result, tc.key, id)
				if check(t, sid, nil) {
					t.Errorf("%s answered by %s/%s still reports unanswered", tc.use, tc.result, tc.key)
				}
			})
		}
	})

	t.Run("only result types answer", func(t *testing.T) {
		sid := newSession(t, pool)
		id := toolUse(t, log, sid, domain.EventAgentToolUse, "")
		// Right key, wrong event type.
		answerWith(t, log, sid, domain.EventUserMessage, "tool_use_id", id)
		// Right event type, a key the COALESCE does not read.
		answerWith(t, log, sid, domain.EventUserToolResult, "some_other_id", id)
		if !check(t, sid, nil) {
			t.Error("a non-result event or an unread key answered a tool use")
		}
	})

	// The COALESCE returns its FIRST non-null arm, so one event carrying two
	// keys answers one tool use, not both. Adjacent pairs are driven separately
	// because a swap of arms two and three is invisible to a first-vs-third
	// fixture.
	t.Run("coalesce arm precedence", func(t *testing.T) {
		for _, tc := range []struct {
			name            string
			winner, loser   domain.EventType
			winKey, loseKey string
			loserResultType domain.EventType
		}{
			{"tool_use_id over custom_tool_use_id", domain.EventAgentToolUse, domain.EventAgentCustomToolUse,
				"tool_use_id", "custom_tool_use_id", domain.EventUserCustomToolRes},
			{"tool_use_id over mcp_tool_use_id", domain.EventAgentToolUse, domain.EventAgentMCPToolUse,
				"tool_use_id", "mcp_tool_use_id", domain.EventAgentMCPToolResult},
			{"custom_tool_use_id over mcp_tool_use_id", domain.EventAgentCustomToolUse, domain.EventAgentMCPToolUse,
				"custom_tool_use_id", "mcp_tool_use_id", domain.EventAgentMCPToolResult},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sid := newSession(t, pool)
				win := toolUse(t, log, sid, tc.winner, "")
				lose := toolUse(t, log, sid, tc.loser, "")
				appendEvent(t, log, sid, "", domain.EventAgentToolResult,
					fmt.Sprintf(`{%q:%q,%q:%q}`, tc.winKey, win, tc.loseKey, lose))
				// Either ordering leaves one use outstanding, so this says only
				// that the two-key event did not answer both.
				if !check(t, sid, nil) {
					t.Error("a result carrying two keys answered both tool uses")
				}
				// Answering the losing arm on its own is what separates them:
				// in arm order the session is now settled; swapped, the winner
				// was never answered and is still outstanding.
				answerWith(t, log, sid, tc.loserResultType, tc.loseKey, lose)
				if check(t, sid, nil) {
					t.Errorf("%s did not win the COALESCE — the %s is still unanswered", tc.winKey, tc.winner)
				}
			})
		}
	})

	// The query constrains neither the pairing of result kind to use kind nor
	// which key names which, so a custom result answers a built-in tool use.
	t.Run("kinds need not pair", func(t *testing.T) {
		sid := newSession(t, pool)
		id := toolUse(t, log, sid, domain.EventAgentToolUse, "")
		answerWith(t, log, sid, domain.EventUserCustomToolRes, "custom_tool_use_id", id)
		if check(t, sid, nil) {
			t.Error("a custom result did not answer a built-in tool use")
		}
	})

	// ->> over a JSON null yields SQL NULL, so the first arm falls through
	// rather than matching the empty string.
	t.Run("null first arm falls through", func(t *testing.T) {
		sid := newSession(t, pool)
		c := toolUse(t, log, sid, domain.EventAgentCustomToolUse, "")
		appendEvent(t, log, sid, "", domain.EventUserCustomToolRes,
			fmt.Sprintf(`{"tool_use_id":null,"custom_tool_use_id":%q}`, c))
		if check(t, sid, nil) {
			t.Error("a null first arm blocked the custom_tool_use_id arm")
		}
	})

	// Nothing correlates on seq: a result that arrives before its tool use
	// still answers it. A plausible-looking r.seq > tu.seq clause breaks here.
	t.Run("order independent", func(t *testing.T) {
		sid := newSession(t, pool)
		id := domain.NewID("sevt")
		answerWith(t, log, sid, domain.EventUserToolResult, "tool_use_id", id)
		appendEvent(t, log, sid, id, domain.EventAgentToolUse, `{}`)
		if check(t, sid, nil) {
			t.Error("a result appended before its tool use did not answer it")
		}
	})

	t.Run("session scoped", func(t *testing.T) {
		a, b, c := newSession(t, pool), newSession(t, pool), newSession(t, pool)
		idA := toolUse(t, log, a, domain.EventAgentToolUse, "")
		toolUse(t, log, b, domain.EventAgentToolUse, "")
		// B answers A's id: the result subquery is scoped, so A stays blocked.
		answerWith(t, log, b, domain.EventUserToolResult, "tool_use_id", idA)
		if !check(t, a, nil) {
			t.Error("a result in another session answered this one's tool use")
		}
		if check(t, c, nil) {
			t.Error("another session's outstanding tool use leaked into a clean one")
		}
	})

	// extraRefs is the batch's own not-yet-inserted references. nil and an
	// empty slice must behave alike: pgx binds nil as SQL NULL, and
	// `id != ALL(NULL)` is NULL, so dropping toolflow.go's nil normalization
	// makes this return a silent false — a premature resume, never an error.
	t.Run("extra refs", func(t *testing.T) {
		sid := newSession(t, pool)
		id := toolUse(t, log, sid, domain.EventAgentToolUse, "")
		for _, tc := range []struct {
			name  string
			extra []string
			want  bool
		}{
			{"nil", nil, true},
			{"empty", []string{}, true},
			{"names the outstanding use", []string{id.String()}, false},
			{"names nothing on the log", []string{"sevt_nope"}, true},
		} {
			if got := check(t, sid, tc.extra); got != tc.want {
				t.Errorf("extraRefs %s (%#v): unanswered = %v, want %v", tc.name, tc.extra, got, tc.want)
			}
		}
	})
}

// The platform variant must see only agent.tool_use: the executor runs nothing
// else, so a remainder of client-executed tools has no platform work and
// enqueuing a tool_exec for it would provision a sandbox for nothing.
func TestHasUnansweredPlatformToolUse(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()
	sid := newSession(t, pool)

	both := func(t *testing.T, extra []string) (platform, any_ bool) {
		t.Helper()
		p, err := events.HasUnansweredPlatformToolUse(ctx, pool, sid, extra)
		if err != nil {
			t.Fatalf("HasUnansweredPlatformToolUse: %v", err)
		}
		a, err := events.HasUnansweredToolUse(ctx, pool, sid, extra)
		if err != nil {
			t.Fatalf("HasUnansweredToolUse: %v", err)
		}
		return p, a
	}

	toolUse(t, log, sid, domain.EventAgentCustomToolUse, "")
	toolUse(t, log, sid, domain.EventAgentMCPToolUse, "")
	if platform, any_ := both(t, nil); platform || !any_ {
		t.Errorf("custom+mcp outstanding: platform = %v, any = %v; want false, true", platform, any_)
	}

	id := toolUse(t, log, sid, domain.EventAgentToolUse, "")
	if platform, any_ := both(t, nil); !platform || !any_ {
		t.Errorf("built-in outstanding: platform = %v, any = %v; want true, true", platform, any_)
	}

	// extraRefs reaches this variant too — the confirmation-deny resume passes
	// the denied ids through it, and a version hard-coding nil passes every
	// other case here.
	if platform, _ := both(t, []string{id.String()}); platform {
		t.Error("extraRefs did not clear the built-in tool use on the platform variant")
	}
}

func TestValidateToolResults(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()

	// Validation is sequential and complete per event: for an error to surface
	// at index n, events 0..n-1 must be entirely valid.
	validate := func(sid domain.ID, evs ...events.NewEvent) error {
		return events.ValidateToolResults(ctx, pool, sid, evs)
	}

	t.Run("unknown reference", func(t *testing.T) {
		sid := newSession(t, pool)
		wantErrIs(t, validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", "sevt_nope")),
			`events[0]: tool_use_id "sevt_nope" does not name a tool use in this session`)
	})

	t.Run("reference in another session", func(t *testing.T) {
		a, b := newSession(t, pool), newSession(t, pool)
		id := toolUse(t, log, b, domain.EventAgentToolUse, "")
		ev := inResult(domain.EventUserToolResult, "tool_use_id", id.String())
		wantErrIs(t, validate(a, ev),
			fmt.Sprintf(`events[0]: tool_use_id %q does not name a tool use in this session`, id))
		// The same batch against its own session passes, so only scoping rejected it.
		if err := validate(b, ev); err != nil {
			t.Errorf("valid result in its own session: %v", err)
		}
	})

	t.Run("kind mismatch", func(t *testing.T) {
		sid := newSession(t, pool)
		builtin := toolUse(t, log, sid, domain.EventAgentToolUse, "")
		custom := toolUse(t, log, sid, domain.EventAgentCustomToolUse, "")
		mcp := toolUse(t, log, sid, domain.EventAgentMCPToolUse, "")
		msg := appendEvent(t, log, sid, "", domain.EventAgentMessage, `{"content":[]}`)

		wantErrIs(t, validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", custom.String())),
			fmt.Sprintf(`events[0]: tool_use_id %q references a agent.custom_tool_use event, not agent.tool_use`, custom))
		wantErrIs(t, validate(sid, inResult(domain.EventUserCustomToolRes, "custom_tool_use_id", builtin.String())),
			fmt.Sprintf(`events[0]: custom_tool_use_id %q references a agent.tool_use event, not agent.custom_tool_use`, builtin))
		// agent.mcp_tool_use counts as outstanding but is never a valid target,
		// so it is rejected as a mismatch rather than as a missing reference.
		wantErrIs(t, validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", mcp.String())),
			fmt.Sprintf(`events[0]: tool_use_id %q references a agent.mcp_tool_use event, not agent.tool_use`, mcp))

		// The lookup has no type predicate, so a reference to any event at all
		// is FOUND and reported as a mismatch — not as "does not name", despite
		// that message reading as the more natural fit.
		err := validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", msg.String()))
		wantErrIs(t, err, fmt.Sprintf(`events[0]: tool_use_id %q references a agent.message event, not agent.tool_use`, msg))
	})

	t.Run("already answered", func(t *testing.T) {
		for _, tc := range []struct {
			result domain.EventType
			key    string
		}{
			{domain.EventUserToolResult, "tool_use_id"},
			{domain.EventUserCustomToolRes, "custom_tool_use_id"},
			{domain.EventAgentToolResult, "tool_use_id"},
			{domain.EventAgentMCPToolResult, "mcp_tool_use_id"},
		} {
			t.Run(string(tc.result)+"/"+tc.key, func(t *testing.T) {
				sid := newSession(t, pool)
				id := toolUse(t, log, sid, domain.EventAgentToolUse, "")
				answerWith(t, log, sid, tc.result, tc.key, id)
				wantErrIs(t, validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", id.String())),
					fmt.Sprintf(`events[0]: tool use %q already has a result`, id))
			})
		}

		// One event answers one tool use: the COALESCE stops at its first
		// non-null arm. Both adjacent pairs are driven — a swap of arms two and
		// three does not move a first-vs-second fixture.
		t.Run("coalesce arm precedence", func(t *testing.T) {
			// tool_use_id wins over custom_tool_use_id: the built-in is
			// answered, the custom one stays open.
			t.Run("first over second", func(t *testing.T) {
				sid := newSession(t, pool)
				u := toolUse(t, log, sid, domain.EventAgentToolUse, "")
				c := toolUse(t, log, sid, domain.EventAgentCustomToolUse, "")
				appendEvent(t, log, sid, "", domain.EventAgentToolResult,
					fmt.Sprintf(`{"tool_use_id":%q,"custom_tool_use_id":%q}`, u, c))
				wantErrHas(t, validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", u.String())),
					"already has a result")
				if err := validate(sid, inResult(domain.EventUserCustomToolRes, "custom_tool_use_id", c.String())); err != nil {
					t.Errorf("a later COALESCE arm answered the custom tool use: %v", err)
				}
			})

			// custom_tool_use_id wins over mcp_tool_use_id. Only this pair
			// notices the two later arms trading places.
			t.Run("second over third", func(t *testing.T) {
				sid := newSession(t, pool)
				c := toolUse(t, log, sid, domain.EventAgentCustomToolUse, "")
				m := toolUse(t, log, sid, domain.EventAgentMCPToolUse, "")
				appendEvent(t, log, sid, "", domain.EventAgentToolResult,
					fmt.Sprintf(`{"custom_tool_use_id":%q,"mcp_tool_use_id":%q}`, c, m))
				wantErrHas(t, validate(sid, inResult(domain.EventUserCustomToolRes, "custom_tool_use_id", c.String())),
					"already has a result")
			})
		})

		// A key the COALESCE does not read leaves the tool use answerable.
		t.Run("unread key does not answer", func(t *testing.T) {
			sid := newSession(t, pool)
			id := toolUse(t, log, sid, domain.EventAgentToolUse, "")
			answerWith(t, log, sid, domain.EventUserToolResult, "some_other_id", id)
			if err := validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", id.String())); err != nil {
				t.Errorf("unread key counted as an answer: %v", err)
			}
		})

		// Dropping r.session_id from the answered subquery turns this into
		// "already has a result", and nothing else would notice.
		t.Run("session scoped", func(t *testing.T) {
			a, b := newSession(t, pool), newSession(t, pool)
			id := toolUse(t, log, a, domain.EventAgentToolUse, "")
			answerWith(t, log, b, domain.EventUserToolResult, "tool_use_id", id)
			if err := validate(a, inResult(domain.EventUserToolResult, "tool_use_id", id.String())); err != nil {
				t.Errorf("a result in another session counted as the answer: %v", err)
			}
		})
	})

	t.Run("ask gate", func(t *testing.T) {
		t.Run("blocks until confirmed", func(t *testing.T) {
			sid := newSession(t, pool)
			id := ask(t, log, sid)
			ev := inResult(domain.EventUserToolResult, "tool_use_id", id.String())
			wantErrIs(t, validate(sid, ev),
				fmt.Sprintf(`events[0]: tool use %q is awaiting confirmation and cannot be answered yet`, id))
			confirmOnLog(t, log, sid, id)
			if err := validate(sid, ev); err != nil {
				t.Errorf("confirmed ask still gated: %v", err)
			}
		})

		// The gate asks only whether a confirmation exists, never what it
		// decided: a denial opens it exactly as an approval does, because the
		// denial's own synthesized result is what follows.
		t.Run("outcome is not read", func(t *testing.T) {
			sid := newSession(t, pool)
			id := ask(t, log, sid)
			appendEvent(t, log, sid, "", domain.EventUserToolConfirm,
				fmt.Sprintf(`{"tool_use_id":%q,"result":"deny"}`, id))
			if err := validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", id.String())); err != nil {
				t.Errorf("a deny confirmation did not open the gate: %v", err)
			}
		})

		t.Run("confirmation is per tool use and session scoped", func(t *testing.T) {
			a, b := newSession(t, pool), newSession(t, pool)
			x, y := ask(t, log, a), ask(t, log, a)
			confirmOnLog(t, log, a, x)
			if err := validate(a, inResult(domain.EventUserToolResult, "tool_use_id", x.String())); err != nil {
				t.Errorf("confirmed x still gated: %v", err)
			}
			wantErrHas(t, validate(a, inResult(domain.EventUserToolResult, "tool_use_id", y.String())),
				"is awaiting confirmation")

			// A confirmation in another session must not open this gate.
			z := ask(t, log, b)
			confirmOnLog(t, log, a, z)
			wantErrHas(t, validate(b, inResult(domain.EventUserToolResult, "tool_use_id", z.String())),
				"is awaiting confirmation")
		})

		// Only a user.tool_confirmation opens the gate. Another event carrying
		// the same tool_use_id must not — without the type predicate, any event
		// keyed by the id would let a result answer a tool the user never
		// approved, and the append-only log would record it permanently.
		t.Run("only a confirmation opens the gate", func(t *testing.T) {
			sid := newSession(t, pool)
			id := ask(t, log, sid)
			appendEvent(t, log, sid, "", domain.EventUserMessage,
				fmt.Sprintf(`{"tool_use_id":%q,"content":[]}`, id))
			wantErrHas(t, validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", id.String())),
				"is awaiting confirmation")
		})

		// The gate reads evaluated_permission on any tool-use kind, while only
		// agent.tool_use is confirmable. An ask-stamped custom tool use is
		// therefore unanswerable from both sides at once. Pinned as current
		// behavior, not endorsed: the brain stamps a policy on built-ins only,
		// so nothing reaches this state today.
		t.Run("gates kinds that cannot be confirmed", func(t *testing.T) {
			sid := newSession(t, pool)
			id := toolUse(t, log, sid, domain.EventAgentCustomToolUse, `"ask"`)
			wantErrHas(t, validate(sid, inResult(domain.EventUserCustomToolRes, "custom_tool_use_id", id.String())),
				"is awaiting confirmation")
			wantErrHas(t, events.ValidateToolConfirmations(ctx, pool, sid, []events.NewEvent{inConfirm(id.String())}),
				"does not name a tool use in this session")
		})

		// Only the literal "ask" gates. The absent and null legs reach the
		// COALESCE(...,'') as the empty string.
		t.Run("only ask gates", func(t *testing.T) {
			for _, perm := range []string{`"allow"`, "", "null"} {
				sid := newSession(t, pool)
				id := toolUse(t, log, sid, domain.EventAgentToolUse, perm)
				if err := validate(sid, inResult(domain.EventUserToolResult, "tool_use_id", id.String())); err != nil {
					t.Errorf("evaluated_permission %q gated the result: %v", perm, err)
				}
			}
		})
	})

	// The decode failure is the client's own field-type error and must be named
	// as such: swallowed, ref becomes "" and the caller is told instead that an
	// empty id names no tool use, pointing them at the wrong mistake.
	t.Run("malformed payload", func(t *testing.T) {
		sid := newSession(t, pool)
		for _, payload := range []string{`{}`, `{"tool_use_id":5}`, `null`} {
			wantErrIs(t, validate(sid, inRaw(domain.EventUserToolResult, payload)),
				"events[0]: tool_use_id must be a string")
		}
		// The key reported is the one that event's own type calls for.
		wantErrIs(t, validate(sid, inRaw(domain.EventUserCustomToolRes, `{}`)),
			"events[0]: custom_tool_use_id must be a string")
		// The decoder's own wording is an implementation detail.
		for _, payload := range []string{`[1,2]`, ``} {
			wantErrHas(t, validate(sid, inRaw(domain.EventUserToolResult, payload)), "events[0]: ")
		}
	})

	// Non-result types are skipped before the payload is decoded at all, so a
	// garbage payload on one is not this function's error to report.
	t.Run("non-result types skipped", func(t *testing.T) {
		sid := newSession(t, pool)
		if err := validate(sid,
			inRaw(domain.EventAgentToolUse, `[1,2]`),
			inRaw(domain.EventUserToolConfirm, `[1,2]`),
		); err != nil {
			t.Errorf("a non-result event was decoded: %v", err)
		}
	})

	t.Run("intra-batch duplicate", func(t *testing.T) {
		sid := newSession(t, pool)
		id := toolUse(t, log, sid, domain.EventAgentToolUse, "")
		ev := inResult(domain.EventUserToolResult, "tool_use_id", id.String())
		wantErrIs(t, validate(sid, ev, ev),
			fmt.Sprintf(`events[1]: duplicate result for tool_use_id %q in one request`, id))

		// seen is keyed by the referenced id alone, not by id+kind, and the
		// message reports the second event's own key.
		wantErrIs(t, validate(sid, ev, inResult(domain.EventUserCustomToolRes, "custom_tool_use_id", id.String())),
			fmt.Sprintf(`events[1]: duplicate result for custom_tool_use_id %q in one request`, id))
	})

	// The index is the client's own batch position, counting the events this
	// function skips.
	t.Run("index counts skipped events", func(t *testing.T) {
		sid := newSession(t, pool)
		u := toolUse(t, log, sid, domain.EventAgentToolUse, "")
		c := toolUse(t, log, sid, domain.EventAgentCustomToolUse, "")
		wantErrHas(t, validate(sid,
			inResult(domain.EventUserToolResult, "tool_use_id", u.String()),
			inRaw(domain.EventUserMessage, `{"content":[]}`),
			inResult(domain.EventUserCustomToolRes, "custom_tool_use_id", c.String()),
			inResult(domain.EventUserToolResult, "tool_use_id", "sevt_nope"),
		), "events[3]:")
	})

	t.Run("valid batch", func(t *testing.T) {
		sid := newSession(t, pool)
		u := toolUse(t, log, sid, domain.EventAgentToolUse, "")
		c := toolUse(t, log, sid, domain.EventAgentCustomToolUse, "")
		if err := validate(sid,
			inResult(domain.EventUserToolResult, "tool_use_id", u.String()),
			inRaw(domain.EventUserMessage, `{"content":[]}`),
			inResult(domain.EventUserCustomToolRes, "custom_tool_use_id", c.String()),
		); err != nil {
			t.Errorf("valid batch rejected: %v", err)
		}
		if err := validate(sid); err != nil {
			t.Errorf("empty batch rejected: %v", err)
		}
	})
}

func TestValidateToolConfirmations(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()

	validate := func(sid domain.ID, evs ...events.NewEvent) error {
		return events.ValidateToolConfirmations(ctx, pool, sid, evs)
	}

	t.Run("unknown reference", func(t *testing.T) {
		sid := newSession(t, pool)
		wantErrIs(t, validate(sid, inConfirm("sevt_nope")),
			`events[0]: tool_use_id "sevt_nope" does not name a tool use in this session`)
	})

	t.Run("reference in another session", func(t *testing.T) {
		a, b := newSession(t, pool), newSession(t, pool)
		id := ask(t, log, b)
		wantErrIs(t, validate(a, inConfirm(id.String())),
			fmt.Sprintf(`events[0]: tool_use_id %q does not name a tool use in this session`, id))
	})

	// Only agent.tool_use is confirmable, and the restriction lives in the
	// WHERE clause — so an ask-gated custom or MCP tool use falls out as
	// ErrNoRows and is reported as missing, not as ungated. Counter-intuitive
	// but correct; a refactor moving the predicate would change the message.
	t.Run("non-confirmable kinds report missing, not ungated", func(t *testing.T) {
		for _, typ := range []domain.EventType{domain.EventAgentCustomToolUse, domain.EventAgentMCPToolUse} {
			sid := newSession(t, pool)
			id := toolUse(t, log, sid, typ, `"ask"`)
			wantErrIs(t, validate(sid, inConfirm(id.String())),
				fmt.Sprintf(`events[0]: tool_use_id %q does not name a tool use in this session`, id))
		}
	})

	t.Run("not gated for ask", func(t *testing.T) {
		for _, perm := range []string{`"allow"`, "", "null"} {
			sid := newSession(t, pool)
			id := toolUse(t, log, sid, domain.EventAgentToolUse, perm)
			wantErrIs(t, validate(sid, inConfirm(id.String())),
				fmt.Sprintf(`events[0]: tool use %q was not gated for confirmation`, id))
		}
	})

	t.Run("already confirmed", func(t *testing.T) {
		sid := newSession(t, pool)
		id := ask(t, log, sid)
		confirmOnLog(t, log, sid, id)
		wantErrIs(t, validate(sid, inConfirm(id.String())),
			fmt.Sprintf(`events[0]: tool use %q is already confirmed`, id))
	})

	// Only a user.tool_confirmation counts as one. Without the type predicate,
	// any other event carrying the id would make the human's genuine first
	// approval be rejected as a repeat — leaving the ask permanently
	// unresolvable and the session stuck in requires_action.
	t.Run("only a confirmation counts as already confirmed", func(t *testing.T) {
		sid := newSession(t, pool)
		id := ask(t, log, sid)
		appendEvent(t, log, sid, "", domain.EventUserMessage,
			fmt.Sprintf(`{"tool_use_id":%q,"content":[]}`, id))
		if err := validate(sid, inConfirm(id.String())); err != nil {
			t.Errorf("a non-confirmation event carrying the id blocked the first confirmation: %v", err)
		}
	})

	// That check is session-scoped: a confirmation of the same id in another
	// session must not resolve this one.
	t.Run("already-confirmed check is session scoped", func(t *testing.T) {
		a, b := newSession(t, pool), newSession(t, pool)
		id := ask(t, log, a)
		confirmOnLog(t, log, b, id)
		if err := validate(a, inConfirm(id.String())); err != nil {
			t.Errorf("a confirmation in another session resolved this one: %v", err)
		}
	})

	// A contradictory state — already confirmed, but not gated — reports the
	// gating diagnosis. Which of the two the client is told is a real choice.
	t.Run("ungated outranks already-confirmed", func(t *testing.T) {
		sid := newSession(t, pool)
		id := toolUse(t, log, sid, domain.EventAgentToolUse, `"allow"`)
		confirmOnLog(t, log, sid, id)
		wantErrIs(t, validate(sid, inConfirm(id.String())),
			fmt.Sprintf(`events[0]: tool use %q was not gated for confirmation`, id))
	})

	// The whole-payload null leg is the interesting one: the first decode
	// succeeds and leaves a nil map, so this is not a decode error.
	t.Run("malformed payload", func(t *testing.T) {
		sid := newSession(t, pool)
		for _, payload := range []string{`{}`, `{"tool_use_id":5}`, `null`} {
			wantErrIs(t, validate(sid, inRaw(domain.EventUserToolConfirm, payload)),
				"events[0]: tool_use_id must be a string")
		}
		// The decoder's own wording is an implementation detail; only the
		// batch-index prefix is pinned for these.
		for _, payload := range []string{`[1,2]`, ``} {
			wantErrHas(t, validate(sid, inRaw(domain.EventUserToolConfirm, payload)), "events[0]: ")
		}
	})

	t.Run("intra-batch duplicate", func(t *testing.T) {
		sid := newSession(t, pool)
		id := ask(t, log, sid)
		ev := inConfirm(id.String())
		wantErrIs(t, validate(sid, ev, ev),
			fmt.Sprintf(`events[1]: duplicate confirmation for tool_use_id %q in one request`, id))
	})

	// Non-confirmation events are skipped without a query — the nil Querier is
	// the assertion.
	t.Run("non-confirmations skipped", func(t *testing.T) {
		if err := events.ValidateToolConfirmations(ctx, nil, "sesn_x", []events.NewEvent{
			inRaw(domain.EventUserMessage, `{"content":[]}`),
			inRaw(domain.EventAgentToolUse, `[1,2]`),
			inResult(domain.EventUserToolResult, "tool_use_id", "sevt_x"),
		}); err != nil {
			t.Errorf("a non-confirmation event was validated: %v", err)
		}
		if err := events.ValidateToolConfirmations(ctx, nil, "sesn_x", nil); err != nil {
			t.Errorf("nil batch: %v", err)
		}
	})

	t.Run("valid batch", func(t *testing.T) {
		sid := newSession(t, pool)
		x, y := ask(t, log, sid), ask(t, log, sid)
		if err := validate(sid,
			inConfirm(x.String()),
			inRaw(domain.EventUserMessage, `{"content":[]}`),
			inConfirm(y.String()),
		); err != nil {
			t.Errorf("valid batch rejected: %v", err)
		}
	})
}

func TestUnconfirmedAskEvents(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()

	list := func(t *testing.T, sid domain.ID, extra []string) []string {
		t.Helper()
		got, err := events.UnconfirmedAskEvents(ctx, pool, sid, extra)
		if err != nil {
			t.Fatalf("UnconfirmedAskEvents: %v", err)
		}
		return got
	}

	t.Run("nothing outstanding", func(t *testing.T) {
		sid := newSession(t, pool)
		if got := list(t, sid, nil); got != nil {
			t.Errorf("empty session = %#v, want nil", got)
		}
		appendEvent(t, log, sid, "", domain.EventUserMessage, `{"content":[]}`)
		if got := list(t, sid, nil); got != nil {
			t.Errorf("messages only = %#v, want nil", got)
		}
	})

	// Log order, not id order: the ids here descend lexicographically as seq
	// ascends, so ORDER BY tu.id would return them reversed.
	t.Run("log order", func(t *testing.T) {
		sid := newSession(t, pool)
		want := []string{"sevt_zzz", "sevt_mmm", "sevt_aaa"}
		for _, id := range want {
			appendEvent(t, log, sid, domain.ID(id), domain.EventAgentToolUse, `{"evaluated_permission":"ask"}`)
		}
		if got := list(t, sid, nil); !slices.Equal(got, want) {
			t.Errorf("ids = %v, want %v (log/seq order, not id order)", got, want)
		}
	})

	// Absent and null are excluded along with allow and deny. Only the row set
	// is pinned, not the mechanism: WHERE discards SQL UNKNOWN and FALSE alike,
	// so this cannot tell the bare equality from a COALESCE(...,'').
	t.Run("only ask qualifies", func(t *testing.T) {
		sid := newSession(t, pool)
		for _, perm := range []string{`"allow"`, `"deny"`, "", "null"} {
			toolUse(t, log, sid, domain.EventAgentToolUse, perm)
		}
		id := ask(t, log, sid)
		if got := list(t, sid, nil); !slices.Equal(got, []string{id.String()}) {
			t.Errorf("ids = %v, want just the ask-gated %s", got, id)
		}
	})

	t.Run("only built-in tool uses qualify", func(t *testing.T) {
		sid := newSession(t, pool)
		toolUse(t, log, sid, domain.EventAgentCustomToolUse, `"ask"`)
		toolUse(t, log, sid, domain.EventAgentMCPToolUse, `"ask"`)
		id := ask(t, log, sid)
		if got := list(t, sid, nil); !slices.Equal(got, []string{id.String()}) {
			t.Errorf("ids = %v, want just the agent.tool_use %s", got, id)
		}
	})

	t.Run("confirmations clear them", func(t *testing.T) {
		sid := newSession(t, pool)
		x, y := ask(t, log, sid), ask(t, log, sid)
		confirmOnLog(t, log, sid, x)
		if got := list(t, sid, nil); !slices.Equal(got, []string{y.String()}) {
			t.Errorf("ids = %v, want just %s", got, y)
		}
		confirmOnLog(t, log, sid, y)
		if got := list(t, sid, nil); got != nil {
			t.Errorf("ids = %#v, want nil once every ask is confirmed", got)
		}
	})

	// The query is confirmation-aware, not result-aware: an ask that somehow
	// carries a result but no confirmation is still blocking. The API refuses
	// to create that state; this pins which signal the query reads.
	t.Run("a result does not clear an ask", func(t *testing.T) {
		sid := newSession(t, pool)
		id := ask(t, log, sid)
		answerWith(t, log, sid, domain.EventAgentToolResult, "tool_use_id", id)
		if got := list(t, sid, nil); !slices.Equal(got, []string{id.String()}) {
			t.Errorf("ids = %v, want %s still blocking", got, id)
		}
	})

	// nil and empty must agree. Without toolflow.go's nil normalization pgx
	// binds nil as SQL NULL, `tu.id != ALL(NULL)` is NULL, and this returns
	// nil — every ask read as confirmed, resuming a gated session with no
	// human approval, silently and without an error.
	t.Run("extra confirmed", func(t *testing.T) {
		sid := newSession(t, pool)
		x, y := ask(t, log, sid), ask(t, log, sid)
		both := []string{x.String(), y.String()}
		for _, tc := range []struct {
			name  string
			extra []string
			want  []string
		}{
			{"nil", nil, both},
			{"empty", []string{}, both},
			{"names one ask", []string{x.String()}, []string{y.String()}},
			{"names nothing on the log", []string{"sevt_nope"}, both},
		} {
			if got := list(t, sid, tc.extra); !slices.Equal(got, tc.want) {
				t.Errorf("extraConfirmed %s (%#v): ids = %v, want %v", tc.name, tc.extra, got, tc.want)
			}
		}
	})

	t.Run("session scoped", func(t *testing.T) {
		a, b := newSession(t, pool), newSession(t, pool)
		askA, askB := ask(t, log, a), ask(t, log, b)
		// B carries a confirmation naming A's ask: scoped on the confirmation
		// side, so it resolves neither.
		confirmOnLog(t, log, b, askA)
		if got := list(t, a, nil); !slices.Equal(got, []string{askA.String()}) {
			t.Errorf("session A = %v, want %s (a foreign confirmation resolved it)", got, askA)
		}
		if got := list(t, b, nil); !slices.Equal(got, []string{askB.String()}) {
			t.Errorf("session B = %v, want just %s", got, askB)
		}
	})
}

// The Querier contract: the checks run inside the caller's transaction, over
// rows no other connection can see yet. The API's send handler depends on
// exactly this — it validates a batch against the log under the session row
// lock, in the same transaction that will append it.
func TestToolflowChecksSeeCallerTransaction(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()
	sid := newSession(t, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id := domain.NewID("sevt")
	if _, err := log.AppendInTx(ctx, tx, sid, []events.NewEvent{
		{ID: id, Type: domain.EventAgentToolUse, Payload: json.RawMessage(`{"evaluated_permission":"ask"}`)},
	}, events.AppendOptions{}); err != nil {
		t.Fatalf("append in tx: %v", err)
	}

	if got, err := events.HasUnansweredToolUse(ctx, tx, sid, nil); err != nil || !got {
		t.Errorf("in tx: unanswered = %v, %v; want true", got, err)
	}
	if got, err := events.UnconfirmedAskEvents(ctx, tx, sid, nil); err != nil || !slices.Equal(got, []string{id.String()}) {
		t.Errorf("in tx: asks = %v, %v; want [%s]", got, err, id)
	}
	if err := events.ValidateToolConfirmations(ctx, tx, sid, []events.NewEvent{inConfirm(id.String())}); err != nil {
		t.Errorf("in tx: confirmation rejected: %v", err)
	}

	// The same moment, from outside the transaction: nothing is visible.
	if got, err := events.HasUnansweredToolUse(ctx, pool, sid, nil); err != nil || got {
		t.Errorf("outside tx: unanswered = %v, %v; want false", got, err)
	}
	if got, err := events.UnconfirmedAskEvents(ctx, pool, sid, nil); err != nil || got != nil {
		t.Errorf("outside tx: asks = %#v, %v; want nil", got, err)
	}
	wantErrHas(t, events.ValidateToolConfirmations(ctx, pool, sid, []events.NewEvent{inConfirm(id.String())}),
		"does not name a tool use in this session")
}

// A driver failure on the query itself must surface as a wrapped query error,
// never as a client diagnosis: without the err check after Scan,
// ValidateToolResults would read an empty useType and report a kind mismatch
// for what is really a dead pool. This covers each function's query error only
// — UnconfirmedAskEvents returns a mid-iteration rows.Scan failure unwrapped
// (toolflow.go:278), which a closed pool cannot reach because it fails at
// Query first.
func TestToolflowQueryErrorsAreWrapped(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	ctx := context.Background()
	sid := newSession(t, pool)
	id := toolUse(t, log, sid, domain.EventAgentToolUse, "")

	pool.Close()

	if _, err := events.HasUnansweredToolUse(ctx, pool, sid, nil); err == nil {
		t.Error("HasUnansweredToolUse on a closed pool should error")
	} else {
		wantErrHas(t, err, "unanswered tool_use check:")
	}
	if _, err := events.HasUnansweredPlatformToolUse(ctx, pool, sid, nil); err == nil {
		t.Error("HasUnansweredPlatformToolUse on a closed pool should error")
	} else {
		wantErrHas(t, err, "unanswered tool_use check:")
	}
	err := events.ValidateToolResults(ctx, pool, sid, []events.NewEvent{
		inResult(domain.EventUserToolResult, "tool_use_id", id.String()),
	})
	wantErrHas(t, err, "validate tool result:")
	if err != nil && (strings.Contains(err.Error(), "references a") || strings.Contains(err.Error(), "does not name")) {
		t.Errorf("a driver failure was reported as a client error: %v", err)
	}
	wantErrHas(t, events.ValidateToolConfirmations(ctx, pool, sid, []events.NewEvent{inConfirm(id.String())}),
		"validate tool confirmation:")
	if _, err := events.UnconfirmedAskEvents(ctx, pool, sid, nil); err == nil {
		t.Error("UnconfirmedAskEvents on a closed pool should error")
	} else {
		wantErrHas(t, err, "unconfirmed ask events:")
	}
}
