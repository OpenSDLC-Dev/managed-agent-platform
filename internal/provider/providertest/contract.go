// Package providertest is the shared contract suite every provider.Provider
// adapter must pass (CLAUDE.md: backend variability lives behind an interface
// with one shared suite, as internal/sandbox/sandboxtest and internal/blob/
// blobtest already do for their backends). It asserts the protocol-agnostic
// invariants of the Provider/Stream contract — what every adapter owes its
// caller, whether the turn was carried over Anthropic Messages or OpenAI Chat
// Completions: a stream terminates with exactly one done chunk carrying a stop
// reason; a well-formed tool turn's done chunk carries stop_reason tool_use —
// which the OpenAI adapter forces from the tool calls it saw while the
// Anthropic adapter passes the endpoint's label straight through, so what this
// pins is the two adapters agreeing on a well-formed turn, NOT a promise about
// either endpoint behind them: the brain classifies a turn on its tool blocks
// rather than its label (#181); a tool call's input accumulates and defaults to {} when empty; a usage
// reading is nil only when the endpoint reported none (not when it reported
// zeroes, #90); a cancelled context surfaces as a stream error rather than a
// silent completion; an endpoint that goes silent — before its response headers
// or in the middle of its stream — ends the turn within the configured stall
// budget, while one that sends nothing but keepalives is left to think (#121);
// and Close releases the stream both after a completed turn and before draining
// one.
//
// Protocol-specific behavior stays in each adapter's own package: the wire
// request shape, credential redaction, the OpenAI lossy conversions, and the
// finish_reason -> stop_reason mapping table are NOT here.
//
// It is test support; production code must never import it. A backend supplies
// a Backend that renders the suite's abstract Script into its own streaming
// wire protocol on a fake upstream — the providertest analogue of
// sandboxtest.Harness / blobtest's newStore.
package providertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// ToolCall is the single tool call a Script's turn makes.
type ToolCall struct {
	ID   string
	Name string
	// Input is a compact JSON object the accumulated tool input must equal,
	// e.g. `{"command":"ls"}` or `{}` for an argument-less call.
	Input string
}

// Script is an abstract model turn the suite asks a backend to stage. A backend
// renders it into its own streaming wire protocol; only the fields a given
// subtest sets are meaningful.
type Script struct {
	Text  string             // assistant text streamed before any tool call
	Tool  *ToolCall          // an optional tool call the turn makes
	Usage *domain.ModelUsage // usage the upstream reports, or nil for "reported none"
}

// Backend renders Scripts into running providers for one protocol.
type Backend struct {
	// Turn stages a fake upstream that plays s to completion — streaming its
	// text and tool call, reporting its usage (or none when Script.Usage is
	// nil), and closing the turn the way this protocol naturally ends one — and
	// returns a provider wired to it. A non-empty Tool.Input MUST be streamed
	// across at least two wire frames so the suite exercises input
	// accumulation rather than a single-frame input.
	Turn func(t *testing.T, s Script) provider.Provider

	// Hang stages an upstream that streams exactly enough to yield one text
	// chunk and then blocks without completing the turn, so the suite can
	// cancel the request context mid-stream — or let the provider's own stall
	// budget end it. It returns a provider wired to it, configured with stall as
	// its Config.StallTimeout (zero for the default).
	Hang func(t *testing.T, stall time.Duration) provider.Provider

	// Wedge stages an upstream that accepts the connection and then answers
	// nothing at all — no response headers, ever — the wedged proxy of #121. It
	// returns a provider wired to it with stall as its Config.StallTimeout.
	Wedge func(t *testing.T, stall time.Duration) provider.Provider

	// Keepalive stages an upstream that sends nothing but this protocol's
	// content-free liveness frames (an Anthropic ping, an SSE comment) every
	// stall/8 for 3*stall, and only then completes a one-word turn. It returns a
	// provider wired to it with stall as its Config.StallTimeout, so the suite
	// can hold an adapter to the other half of the stall contract: a quiet but
	// living endpoint must not be killed. The frames are eight to a budget so
	// that a loaded machine has to lose seven in a row before this reads as a
	// stall — the margin is the difference between a contract test and a flake.
	Keepalive func(t *testing.T, stall time.Duration) provider.Provider
}

// Run exercises the provider.Provider contract against one backend.
func Run(t *testing.T, b Backend) {
	t.Helper()

	req := func() provider.Request {
		return provider.Request{
			Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		}
	}
	// drain reads a stream to the end, returning every chunk and the terminal
	// error (nil on a clean turn).
	drain := func(s provider.Stream) ([]provider.Chunk, error) {
		var cs []provider.Chunk
		for s.Next() {
			cs = append(cs, s.Chunk())
		}
		return cs, s.Err()
	}

	t.Run("TextTurnTerminatesWithStopAndUsage", func(t *testing.T) {
		p := b.Turn(t, Script{Text: "Hello", Usage: &domain.ModelUsage{InputTokens: 25, OutputTokens: 17}})
		stream, err := p.Generate(context.Background(), req())
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		// Closed like a real caller closes it: the brain always does, and a
		// stream left open holds its stall guard's watcher until the budget.
		defer stream.Close()
		chunks, err := drain(stream)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		var text strings.Builder
		var doneCount int
		for _, c := range chunks {
			if c.Kind == provider.KindTextDelta {
				text.WriteString(c.Text)
			}
			if c.Kind == provider.KindDone {
				doneCount++
			}
		}
		if text.String() != "Hello" {
			t.Errorf("streamed text = %q, want %q", text.String(), "Hello")
		}
		if doneCount != 1 {
			t.Errorf("done chunks = %d, want exactly one", doneCount)
		}
		done := lastChunk(t, chunks)
		if done.Kind != provider.KindDone {
			t.Fatalf("last chunk = %+v, want a done chunk to terminate the turn", done)
		}
		if done.StopReason != "end_turn" {
			t.Errorf("stop_reason = %q, want end_turn", done.StopReason)
		}
		if done.Usage == nil {
			t.Fatal("done carried no usage, but the turn reported one")
		}
		if done.Usage.InputTokens != 25 || done.Usage.OutputTokens != 17 {
			t.Errorf("usage = %+v, want in=25 out=17", *done.Usage)
		}
	})

	t.Run("ToolCallYieldsToolUseStop", func(t *testing.T) {
		p := b.Turn(t, Script{
			Tool:  &ToolCall{ID: "call_1", Name: "bash", Input: `{"command":"ls -la /tmp"}`},
			Usage: &domain.ModelUsage{InputTokens: 3, OutputTokens: 4},
		})
		stream, err := p.Generate(context.Background(), req())
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		// Closed like a real caller closes it: the brain always does, and a
		// stream left open holds its stall guard's watcher until the budget.
		defer stream.Close()
		chunks, err := drain(stream)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		var tools []provider.Chunk
		for _, c := range chunks {
			if c.Kind == provider.KindToolUse {
				tools = append(tools, c)
			}
		}
		if len(tools) != 1 {
			t.Fatalf("tool_use chunks = %d, want exactly one (%+v)", len(tools), chunks)
		}
		tu := tools[0].ToolUse
		if tu == nil || tu.ID != "call_1" || tu.Name != "bash" {
			t.Fatalf("tool_use = %+v, want id=call_1 name=bash", tu)
		}
		// The input arrived across multiple wire frames (Backend.Turn's
		// contract) and must reassemble to the whole object.
		assertJSONEqual(t, string(tu.Input), `{"command":"ls -la /tmp"}`)
		done := lastChunk(t, chunks)
		if done.Kind != provider.KindDone || done.StopReason != "tool_use" {
			t.Errorf("done = %+v, want stop_reason tool_use whenever the turn made a tool call", done)
		}
		// The usage reported on a tool-call turn must reach the done chunk too:
		// a regression that dropped usage only on tool turns would otherwise
		// slip past a suite that checks usage on text turns alone.
		if done.Usage == nil {
			t.Fatal("done carried no usage, but the tool-call turn reported one")
		}
		if done.Usage.InputTokens != 3 || done.Usage.OutputTokens != 4 {
			t.Errorf("usage = %+v, want in=3 out=4", *done.Usage)
		}
	})

	t.Run("EmptyToolInputBecomesEmptyObject", func(t *testing.T) {
		p := b.Turn(t, Script{Tool: &ToolCall{ID: "call_0", Name: "noop", Input: "{}"}})
		stream, err := p.Generate(context.Background(), req())
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		// Closed like a real caller closes it: the brain always does, and a
		// stream left open holds its stall guard's watcher until the budget.
		defer stream.Close()
		chunks, err := drain(stream)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		var tu *provider.ToolUse
		for _, c := range chunks {
			if c.Kind == provider.KindToolUse {
				tu = c.ToolUse
			}
		}
		if tu == nil {
			t.Fatalf("no tool_use chunk emitted (%+v)", chunks)
		}
		assertJSONEqual(t, string(tu.Input), "{}")
	})

	// #90: silence is not a zero reading. An endpoint that reports no usage at
	// all must leave the done chunk's usage nil, so the token metric records
	// nothing rather than a free turn.
	t.Run("NoUsageReportedIsNil", func(t *testing.T) {
		p := b.Turn(t, Script{Text: "hi", Usage: nil})
		stream, err := p.Generate(context.Background(), req())
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		// Closed like a real caller closes it: the brain always does, and a
		// stream left open holds its stall guard's watcher until the budget.
		defer stream.Close()
		chunks, err := drain(stream)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		done := lastChunk(t, chunks)
		if done.Kind != provider.KindDone {
			t.Fatalf("last chunk = %+v, want done", done)
		}
		if done.Usage != nil {
			t.Errorf("usage = %+v, want nil: the upstream reported none (#90)", *done.Usage)
		}
	})

	// The mirror of the case above: a usage object full of zeroes is a reading
	// like any other — the endpoint answered, and the answer is zero.
	t.Run("ZeroUsageIsStillAReading", func(t *testing.T) {
		p := b.Turn(t, Script{Text: "hi", Usage: &domain.ModelUsage{}})
		stream, err := p.Generate(context.Background(), req())
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		// Closed like a real caller closes it: the brain always does, and a
		// stream left open holds its stall guard's watcher until the budget.
		defer stream.Close()
		chunks, err := drain(stream)
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		done := lastChunk(t, chunks)
		if done.Usage == nil {
			t.Fatal("usage = nil, want a zeroed reading: the upstream sent a usage object (#90)")
		}
		if done.Usage.InputTokens != 0 || done.Usage.OutputTokens != 0 {
			t.Errorf("usage = %+v, want zeroes", *done.Usage)
		}
	})

	// A context cancelled mid-turn must surface as a stream error, never as a
	// silent completion — the brain must be able to tell an aborted turn from a
	// finished one.
	t.Run("ContextCancellationSurfacesAsError", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := b.Hang(t, 0)
		stream, err := p.Generate(ctx, req())
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		defer stream.Close()
		if !stream.Next() {
			t.Fatalf("stream ended before the first chunk: %v", stream.Err())
		}
		if k := stream.Chunk().Kind; k != provider.KindTextDelta {
			t.Fatalf("first chunk kind = %s, want a text delta before cancel", k)
		}
		cancel()
		start := time.Now()
		var sawDone bool
		for stream.Next() {
			if stream.Chunk().Kind == provider.KindDone {
				sawDone = true
			}
		}
		elapsed := time.Since(start)
		if sawDone {
			t.Error("a cancelled turn produced a done chunk — it must not complete")
		}
		if stream.Err() == nil {
			t.Error("a cancelled turn must surface a stream error, not a silent stop")
		}
		// The upstream hangs until a 10s backstop; a stream that observes the
		// cancelled context aborts in milliseconds. Requiring the error to
		// surface well inside that window is what proves cancellation was
		// propagated rather than the backstop closing the connection.
		if elapsed > 5*time.Second {
			t.Errorf("cancellation took %s to surface — the stream did not observe the cancelled context", elapsed)
		}
	})

	// #121: an endpoint that accepts the connection and then answers nothing must
	// end the turn on its own. Nothing else will: the brain runs one turn at a
	// time and its lease keeper renews the work item for as long as the stream is
	// open, so an unbounded wait takes a replica out of service for every tenant,
	// with no error, no metric and no log.
	t.Run("WedgedEndpointFailsWithinTheStallBudget", func(t *testing.T) {
		const budget = 250 * time.Millisecond
		p := b.Wedge(t, budget)
		start := time.Now()
		err := failWithin(t, 5*time.Second, func() error {
			stream, err := p.Generate(context.Background(), req())
			if err != nil {
				return err
			}
			defer stream.Close()
			_, err = drain(stream)
			return err
		})
		if !errors.Is(err, provider.ErrStalled) {
			t.Errorf("error = %v, want one matching provider.ErrStalled", err)
		}
		// The lower bound is what makes this a statement about the configured
		// budget rather than about finiteness: a turn that failed instantly
		// failed for some other reason — a refused connection, a bad URL — and
		// would satisfy the ceiling alone while proving nothing.
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("the turn failed in %s, inside its own %s budget — something other than the stall bound ended it", elapsed, budget)
		}
	})

	// The other half of the same hang: response headers arrive, content starts,
	// and the endpoint then dies mid-stream. A header timeout would not catch
	// this one — the bound has to be the silence, wherever in the turn it falls.
	t.Run("MidStreamSilenceFailsWithinTheStallBudget", func(t *testing.T) {
		const budget = 250 * time.Millisecond
		p := b.Hang(t, budget)
		var chunks []provider.Chunk
		start := time.Now()
		err := failWithin(t, 5*time.Second, func() error {
			stream, err := p.Generate(context.Background(), req())
			if err != nil {
				return err
			}
			defer stream.Close()
			chunks, err = drain(stream)
			return err
		})
		if !errors.Is(err, provider.ErrStalled) {
			t.Errorf("error = %v, want one matching provider.ErrStalled", err)
		}
		if len(chunks) == 0 {
			t.Error("the stall discarded the chunk the upstream did send before going quiet")
		}
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("the turn failed in %s, inside its own %s budget — something other than the stall bound ended it", elapsed, budget)
		}
	})

	// A quiet endpoint is not a dead one. Keepalives buy the budget back, so a
	// model that thinks for longer than one budget before its first token still
	// gets to answer — the failure mode a bound this fix must not introduce.
	t.Run("KeepalivesHoldTheStallBudgetOpen", func(t *testing.T) {
		const budget = 400 * time.Millisecond
		p := b.Keepalive(t, budget)
		start := time.Now()
		var chunks []provider.Chunk
		err := failWithin(t, 10*time.Second, func() error {
			stream, err := p.Generate(context.Background(), req())
			if err != nil {
				return err
			}
			defer stream.Close()
			chunks, err = drain(stream)
			return err
		})
		if err != nil {
			t.Fatalf("a keepalive-only upstream failed the turn: %v", err)
		}
		if lastChunk(t, chunks).Kind != provider.KindDone {
			t.Error("the keepalive turn did not terminate with a done chunk")
		}
		// Without the reset, the turn would have died at one budget. Two is the
		// margin that keeps this a statement about resets rather than about how
		// promptly a loaded CI machine schedules a timer.
		if elapsed := time.Since(start); elapsed < 2*budget {
			t.Errorf("the turn completed in %s, inside two %s budgets — the upstream never went quiet long enough to test anything", elapsed, budget)
		}
	})

	t.Run("CloseAfterCompletionReturnsNil", func(t *testing.T) {
		p := b.Turn(t, Script{Text: "done"})
		stream, err := p.Generate(context.Background(), req())
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, err := drain(stream); err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if err := stream.Close(); err != nil {
			t.Errorf("Close after a completed turn = %v, want nil", err)
		}
	})

	// Closing an undrained stream whose upstream is still hanging must release
	// it promptly, not block trying to drain bytes that will never arrive — the
	// brain closes the stream in a defer before releasing the turn's lease, so a
	// blocking Close would wedge the session on a hung endpoint. The context is
	// cancelled only at subtest end, so the timing below measures Close alone.
	t.Run("CloseDoesNotBlockOnAHungUpstream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := b.Hang(t, 0)
		stream, err := p.Generate(ctx, req())
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		start := time.Now()
		closeErr := stream.Close()
		elapsed := time.Since(start)
		if closeErr != nil {
			t.Errorf("Close on a hung upstream = %v, want nil", closeErr)
		}
		if elapsed > 5*time.Second {
			t.Errorf("Close blocked %s on a hung upstream — it must not drain a stream that never completed", elapsed)
		}
	})
}

// failWithin runs op in the background and returns its error, failing the test
// if op has not returned by limit. A bound that does not bind would otherwise
// hang this suite for the whole test timeout instead of failing it, which is
// the exact shape of the defect under test.
func failWithin(t *testing.T, limit time.Duration, op func() error) error {
	t.Helper()
	res := make(chan error, 1)
	go func() { res <- op() }()
	select {
	case err := <-res:
		return err
	case <-time.After(limit):
		t.Fatalf("the turn had not ended after %s — nothing bounds the wait on the endpoint (#121)", limit)
		return nil
	}
}

// lastChunk returns the final chunk of a drained turn. A turn that produced no
// chunks at all fails readably here rather than panicking on an empty slice —
// this suite's audience is the author of a not-yet-working backend.
func lastChunk(t *testing.T, chunks []provider.Chunk) provider.Chunk {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("the turn produced no chunks at all")
	}
	return chunks[len(chunks)-1]
}

// assertJSONEqual compares two JSON documents by their compact form, so
// insignificant whitespace never fails the assertion.
func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var cg, cw bytes.Buffer
	if err := json.Compact(&cg, []byte(got)); err != nil {
		t.Fatalf("tool input is not valid JSON: %q: %v", got, err)
	}
	if err := json.Compact(&cw, []byte(want)); err != nil {
		t.Fatalf("expected value is not valid JSON: %q: %v", want, err)
	}
	if cg.String() != cw.String() {
		t.Errorf("tool input = %s, want %s", cg.String(), cw.String())
	}
}
