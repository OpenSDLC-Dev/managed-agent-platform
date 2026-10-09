package brain

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// keptUnder is the stored block for thinking event id as streamTurn keeps it:
// produced by model "m" over route "r" at the end of the request built from
// tools and history.
func keptUnder(t *testing.T, id domain.ID, tools []json.RawMessage, history []domain.Event) events.ThinkingBlock {
	t.Helper()
	req, _, err := buildRequest("sys", tools, history, "", "", "", "", replayThinking{})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := requestChain("r", req)
	if err != nil {
		t.Fatal(err)
	}
	return events.ThinkingBlock{EventID: id, Model: "m", PrefixDigest: chain.sum(),
		Block: json.RawMessage(`{"type":"thinking","thinking":"t","signature":"s"}`)}
}

// assistantTypes is the block types of the assistant messages buildRequest
// renders from tools and history with kept stored, for model "m" over route.
func assistantTypes(t *testing.T, route string, tools []json.RawMessage, history []domain.Event, kept events.ThinkingBlock) []string {
	t.Helper()
	req, _, err := buildRequest("sys", tools, history, "", "", "", "",
		replayThinking{model: "m", route: route, blocks: map[domain.ID]events.ThinkingBlock{kept.EventID: kept}})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		var blocks []struct{ Type string }
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			types = append(types, b.Type)
		}
	}
	return types
}

// A tool added since the block was produced changes the prefix every block
// begins with: the block drops, and the request is otherwise unchanged.
func TestThinkingDropsWhenTheToolSetChanges(t *testing.T) {
	asked := ev(1, domain.EventUserMessage, `{"content":"q"}`)
	think := ev(2, domain.EventAgentThinking, `{}`)
	history := []domain.Event{asked, think,
		ev(3, domain.EventAgentMessage, `{"content":[{"type":"text","text":"a"}]}`),
		ev(4, domain.EventUserMessage, `{"content":"next"}`)}
	before := []json.RawMessage{json.RawMessage(`{"name":"a","input_schema":{"type":"object"}}`)}
	after := []json.RawMessage{before[0], json.RawMessage(`{"name":"b","input_schema":{"type":"object"}}`)}
	kept := keptUnder(t, think.ID, before, history[:1])

	if got := assistantTypes(t, "r", before, history, kept); !slices.Equal(got, []string{"thinking", "text"}) {
		t.Fatalf("same tools: assistant blocks = %v, want the block kept", got)
	}
	if got := assistantTypes(t, "r", after, history, kept); !slices.Equal(got, []string{"text"}) {
		t.Errorf("tool added: assistant blocks = %v, want the block dropped", got)
	}
}

// Another route — another endpoint, or the adapter rendering search results
// otherwise — is another prefix even when the request the brain builds is the
// same: the block drops.
func TestThinkingDropsWhenTheRouteChanges(t *testing.T) {
	asked := ev(1, domain.EventUserMessage, `{"content":"q"}`)
	think := ev(2, domain.EventAgentThinking, `{}`)
	history := []domain.Event{asked, think,
		ev(3, domain.EventAgentMessage, `{"content":[{"type":"text","text":"a"}]}`),
		ev(4, domain.EventUserMessage, `{"content":"next"}`)}
	kept := keptUnder(t, think.ID, nil, history[:1])

	if got := assistantTypes(t, "r", nil, history, kept); !slices.Equal(got, []string{"thinking", "text"}) {
		t.Fatalf("same route: assistant blocks = %v, want the block kept", got)
	}
	if got := assistantTypes(t, "elsewhere", nil, history, kept); !slices.Equal(got, []string{"text"}) {
		t.Errorf("another route: assistant blocks = %v, want the block dropped", got)
	}
}

// Earlier messages that render in another order than the one the block was
// produced after are another prefix: the block drops.
func TestThinkingDropsWhenEarlierMessagesReorder(t *testing.T) {
	think := ev(3, domain.EventAgentThinking, `{}`)
	answer := ev(4, domain.EventAgentMessage, `{"content":[{"type":"text","text":"a"}]}`)
	inOrder := []domain.Event{
		ev(1, domain.EventUserMessage, `{"content":"one"}`),
		ev(2, domain.EventUserMessage, `{"content":"two"}`),
	}
	kept := keptUnder(t, think.ID, nil, inOrder)

	if got := assistantTypes(t, "r", nil, append(slices.Clone(inOrder), think, answer), kept); !slices.Equal(got, []string{"thinking", "text"}) {
		t.Fatalf("same order: assistant blocks = %v, want the block kept", got)
	}
	reordered := []domain.Event{
		ev(1, domain.EventUserMessage, `{"content":"two"}`),
		ev(2, domain.EventUserMessage, `{"content":"one"}`),
		think, answer,
	}
	if got := assistantTypes(t, "r", nil, reordered, kept); !slices.Equal(got, []string{"text"}) {
		t.Errorf("reordered: assistant blocks = %v, want the block dropped", got)
	}
}

// A block stored under its route's any: digest — its producer checks no
// prefix (#883, docs/plan/61_thinking-replay-via-gateway.md) — goes back after
// a tool set change and a reorder of earlier messages, and still drops when
// the route or the model changes.
func TestUncheckedThinkingIgnoresThePrefixButNotTheRouteOrModel(t *testing.T) {
	think := ev(3, domain.EventAgentThinking, `{}`)
	answer := ev(4, domain.EventAgentMessage, `{"content":[{"type":"text","text":"a"}]}`)
	inOrder := []domain.Event{
		ev(1, domain.EventUserMessage, `{"content":"one"}`),
		ev(2, domain.EventUserMessage, `{"content":"two"}`),
	}
	kept := keptUnder(t, think.ID, nil, inOrder)
	kept.PrefixDigest = anyPrefixDigest("r")
	history := append(slices.Clone(inOrder), think, answer)
	tools := []json.RawMessage{json.RawMessage(`{"name":"b","input_schema":{"type":"object"}}`)}
	reordered := []domain.Event{
		ev(1, domain.EventUserMessage, `{"content":"two"}`),
		ev(2, domain.EventUserMessage, `{"content":"one"}`),
		think, answer,
	}
	keptTypes, dropped := []string{"thinking", "text"}, []string{"text"}

	if got := assistantTypes(t, "r", tools, history, kept); !slices.Equal(got, keptTypes) {
		t.Errorf("tool added: assistant blocks = %v, want the block kept", got)
	}
	if got := assistantTypes(t, "r", nil, reordered, kept); !slices.Equal(got, keptTypes) {
		t.Errorf("reordered: assistant blocks = %v, want the block kept", got)
	}
	if got := assistantTypes(t, "elsewhere", nil, history, kept); !slices.Equal(got, dropped) {
		t.Errorf("another route: assistant blocks = %v, want the block dropped", got)
	}
	other := kept
	other.Model = "another-model"
	if got := assistantTypes(t, "r", nil, history, other); !slices.Equal(got, dropped) {
		t.Errorf("another model: assistant blocks = %v, want the block dropped", got)
	}
}

// The any: digest can never be a chain's, which is bare hex, and differs by
// route.
func TestTheAnyPrefixDigestIsNoChainsDigest(t *testing.T) {
	a, b := anyPrefixDigest("r"), anyPrefixDigest("elsewhere")
	if a == b || !strings.HasPrefix(a, "any:") {
		t.Fatalf("any-prefix digests %q and %q", a, b)
	}
	chain, err := newPrefixChain("r", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(chain.sum(), ":") {
		t.Fatalf("a chain digest %q holds the any: separator", chain.sum())
	}
}
