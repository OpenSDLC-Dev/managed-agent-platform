package brain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// web_search's description carries the day's date (#682), so the brain renders
// the tool definitions per request, from its clock at assembly — not once at
// startup. Two turns on one UTC day therefore offer the same bytes, and a turn
// after the next UTC midnight offers different ones: the date line and nothing
// else. That turnover is the cost the owner accepted — the cached prompt
// prefix, which the tool list heads, moves once a day.
func TestWebSearchDateIsRenderedPerRequest(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{agentReply("one"), agentReply("two"), agentReply("three")}, nil)
	h.builtins(t)
	clock := time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC)
	h.brain.SetNow(func() time.Time { return clock })

	h.wake(t, "first")
	h.runOnce(t)
	clock = clock.Add(59 * time.Minute) // 23:59, the same UTC day
	h.wake(t, "second")
	h.runOnce(t)
	clock = clock.Add(2 * time.Minute) // 00:01, the next one
	h.wake(t, "third")
	h.runOnce(t)

	calls := h.provider.calls
	if len(calls) != 3 {
		t.Fatalf("%d model calls, want 3", len(calls))
	}
	sameDay, nextDay := toolsByName(t, calls[0].Tools), toolsByName(t, calls[2].Tools)
	if second := toolsByName(t, calls[1].Tools); len(second) != len(sameDay) {
		t.Fatalf("same-day turns offered %d and %d tools", len(sameDay), len(second))
	} else {
		for name, def := range sameDay {
			if second[name] != def {
				t.Errorf("%s moved within one UTC day:\n%s\n%s", name, def, second[name])
			}
		}
	}

	if len(nextDay) != len(sameDay) {
		t.Fatalf("turns a day apart offered %d and %d tools", len(sameDay), len(nextDay))
	}
	for name, def := range sameDay {
		if name == "web_search" {
			continue
		}
		if nextDay[name] != def {
			t.Errorf("%s moved across a UTC midnight; only web_search's date line should:\n%s\n%s", name, def, nextDay[name])
		}
	}
	for _, tc := range []struct{ def, date string }{
		{sameDay["web_search"], "2026-09-01"},
		{nextDay["web_search"], "2026-09-02"},
	} {
		var d struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal([]byte(tc.def), &d); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(d.Description, "\n- Today's date is "+tc.date+"\n") {
			t.Errorf("web_search description carries no date line for %s:\n%s", tc.date, d.Description)
		}
	}
	if sameDay["web_search"] == nextDay["web_search"] {
		t.Error("web_search's definition did not move across a UTC midnight")
	}
}

// The test above swaps the clock out, so it cannot see the one New wires in:
// a brain built with a frozen clock would pass it. This turn runs on the
// production clock and its date line must be the real UTC date — the date
// before the turn or the date after it, so a run that straddles a UTC
// midnight still passes.
func TestWebSearchDateComesFromTheWallClock(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{agentReply("one")}, nil)
	h.builtins(t)

	before := time.Now().UTC().Format(time.DateOnly)
	h.wake(t, "first")
	h.runOnce(t)
	after := time.Now().UTC().Format(time.DateOnly)

	if len(h.provider.calls) != 1 {
		t.Fatalf("%d model calls, want 1", len(h.provider.calls))
	}
	def, ok := toolsByName(t, h.provider.calls[0].Tools)["web_search"]
	if !ok {
		t.Fatal("no web_search was offered")
	}
	var d struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(def), &d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Description, "\n- Today's date is "+before+"\n") &&
		!strings.Contains(d.Description, "\n- Today's date is "+after+"\n") {
		t.Errorf("web_search's date line is not today's UTC date (%s):\n%s", before, d.Description)
	}
}

// toolsByName indexes one request's tool definitions by name, as raw text.
func toolsByName(t *testing.T, tools []json.RawMessage) map[string]string {
	t.Helper()
	out := make(map[string]string, len(tools))
	for _, raw := range tools {
		var d struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("tool definition: %v", err)
		}
		out[d.Name] = string(raw)
	}
	return out
}
