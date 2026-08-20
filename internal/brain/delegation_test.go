package brain

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// agentWithACustomTool is the surface every role test resolves against: one
// custom tool, so the injected tools' position among the agent's own is
// visible.
func agentWithACustomTool() domain.ResolvedAgent {
	return domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"custom","name":"lookup","description":"Look up.","input_schema":{"type":"object"}}`),
	}}}
}

// The role decides the tool surface: a coordinator's primary thread addresses
// its children, a child reports back, and a single-agent session sees neither
// set. The injected tools come first so an agent's own tools[] can never
// reorder them, and every one of them classes as settlement-executed — the
// commit answers it, no driver runs it.
func TestDelegationToolsAreInjectedByRole(t *testing.T) {
	for _, tc := range []struct {
		name     string
		role     delegationRole
		injected []string
	}{
		{"single agent", delegationNone, nil},
		{"coordinator", delegationCoordinator,
			[]string{"create_agent", "send_to_agent", "list_agents", "wait_for_agents"}},
		{"child", delegationChild, []string{"submit_result", "send_to_parent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defs, class, notes, err := resolveTools(agentWithACustomTool(), nil, tc.role)
			if err != nil {
				t.Fatalf("resolveTools: %v", err)
			}
			if len(notes) != 0 {
				t.Errorf("notes = %v, want none", notes)
			}
			want := append(append([]string{}, tc.injected...), "lookup")
			if got := defNames(t, defs); !slicesEqual(got, want) {
				t.Fatalf("offered = %v, want %v", got, want)
			}
			for _, name := range tc.injected {
				c, ok := class[name]
				if !ok {
					t.Fatalf("%s has no class", name)
				}
				if !c.settlement {
					t.Errorf("%s: settlement = false, want true", name)
				}
				if c.kind != domain.EventAgentToolUse {
					t.Errorf("%s: kind = %q, want %q", name, c.kind, domain.EventAgentToolUse)
				}
				if c.policy != "" {
					t.Errorf("%s: policy = %q, want none — a platform tool is never gated", name, c.policy)
				}
			}
			if _, spawns := class["create_agent"]; spawns && tc.role != delegationCoordinator {
				t.Error("create_agent reached a thread that is not a coordinator's primary")
			}
		})
	}
}

// The stability of the injected tools[] is pinned where it can actually move:
// the definitions' bytes and the no-aliasing rule in
// TestDelegationDefinitionsArePinned (internal/toolset), and two real turns'
// requests in TestTwoCoordinatorTurnsShareARequestPrefix. Comparing two
// resolveTools calls here could not fail — both clone one package-level slice.

// An injected name is the platform's, and the model must not be able to reach
// an agent's own code through it: the custom tool is dropped, with a note, and
// only where the name was actually injected — the same agent on a single-agent
// session keeps its tool.
func TestACustomToolNamedLikeADelegationToolIsShadowed(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"custom","name":"create_agent","description":"Not ours.","input_schema":{"type":"object"}}`),
	}}}

	defs, class, notes, err := resolveTools(agent, nil, delegationCoordinator)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if got := defNames(t, defs); !slicesEqual(got,
		[]string{"create_agent", "send_to_agent", "list_agents", "wait_for_agents"}) {
		t.Fatalf("offered = %v, want the four injected tools alone", got)
	}
	if !class["create_agent"].settlement {
		t.Error("the agent's own definition took the name")
	}
	if len(notes) != 1 || !hasNote(notes, "create_agent") {
		t.Errorf("notes = %v, want the shadowed tool named", notes)
	}

	defs, class, notes, err = resolveTools(agent, nil, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if got := defNames(t, defs); !slicesEqual(got, []string{"create_agent"}) {
		t.Fatalf("offered = %v, want the agent's own tool", got)
	}
	if class["create_agent"].kind != domain.EventAgentCustomToolUse {
		t.Errorf("kind = %q, want the agent's own custom tool", class["create_agent"].kind)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none where nothing was injected", notes)
	}
}

// The roster is stored as an explicit JSON null for a single agent, so a length
// test alone would class every single-agent primary as a coordinator.
func TestRosterDetection(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"absent", "", false},
		{"json null", "null", false},
		{"empty roster", `{"type":"coordinator","agents":[]}`, false},
		{"one member", `{"type":"coordinator","agents":[{"type":"agent","name":"researcher"}]}`, true},
		{"unreadable", `{`, false},
	} {
		if got := hasRoster(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("hasRoster(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A delegation call commits as an ordinary agent.tool_use — stamped allow, like
// every platform call — but escalates no work item, because no driver can run
// it. Its id is minted here so the answer this same commit appends can name it.
func TestTurnEventsRendersADelegationCall(t *testing.T) {
	_, class, _, err := resolveTools(agentWithACustomTool(), nil, delegationCoordinator)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	turn := &turnResult{toolUses: []provider.ToolUse{
		{ID: "toolu_1", Name: "create_agent", Input: json.RawMessage(`{"agent_name":"researcher","message":"go"}`)},
		{ID: "toolu_2", Name: "list_agents", Input: json.RawMessage(`{}`)},
	}}

	batch, kind, askIDs, delegated, err := turnEvents(turn, class)
	if err != nil {
		t.Fatalf("turnEvents: %v", err)
	}
	if kind != "" {
		t.Errorf("work kind = %q, want none — no driver runs a delegation call", kind)
	}
	if len(askIDs) != 0 {
		t.Errorf("askIDs = %v, want none", askIDs)
	}
	if len(batch) != 2 {
		t.Fatalf("%d events, want one per call", len(batch))
	}
	if len(delegated) != 2 {
		t.Fatalf("delegated = %v, want both calls in model order", delegated)
	}
	for i, want := range []string{"create_agent", "list_agents"} {
		ev := batch[i]
		if ev.Type != domain.EventAgentToolUse {
			t.Errorf("event %d type = %q, want %q", i, ev.Type, domain.EventAgentToolUse)
		}
		if ev.ID == "" {
			t.Errorf("event %d has no id: the answer could not name it", i)
		}
		var p struct {
			Name                string          `json:"name"`
			Input               json.RawMessage `json:"input"`
			EvaluatedPermission string          `json:"evaluated_permission"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("event %d payload: %v", i, err)
		}
		if p.Name != want {
			t.Errorf("event %d name = %q, want %q", i, p.Name, want)
		}
		if p.EvaluatedPermission != string(domain.EvalPermAllow) {
			t.Errorf("event %d evaluated_permission = %q, want allow", i, p.EvaluatedPermission)
		}
		if delegated[i].eventID != ev.ID {
			t.Errorf("delegated %d names %q, want the event's own id %q", i, delegated[i].eventID, ev.ID)
		}
		if delegated[i].name != want {
			t.Errorf("delegated %d name = %q, want %q", i, delegated[i].name, want)
		}
		if !bytes.Equal(delegated[i].input, p.Input) {
			t.Errorf("delegated %d input = %s, want the committed %s", i, delegated[i].input, p.Input)
		}
	}
}

// A child's first turn is this arm and nothing else: the spawn writes the task
// on the child's log as one agent.thread_message_received, so without it the
// child's opening request carries no messages at all. The rendering is pinned
// byte for byte because it is the head of every later request that thread
// assembles — a prefix that moves costs the whole cached prefix.
func TestReplayRendersAReceivedMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			"from a named agent",
			`{"content":[{"type":"text","text":"found three papers"}],"from_session_thread_id":"sthr_1","from_agent_name":"researcher"}`,
			`[{"text":"[message from researcher]\n\nfound three papers","type":"text"}]`,
		},
		{
			// from_agent_name is null when the sender is the primary agent,
			// which has a role rather than a roster name.
			"from the primary",
			`{"content":[{"type":"text","text":"summarize them"}],"from_session_thread_id":"sthr_0","from_agent_name":null}`,
			`[{"text":"[message from your coordinator]\n\nsummarize them","type":"text"}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _, err := buildRequest("", nil,
				[]domain.Event{ev(1, domain.EventAgentThreadMessageReceived, tc.body)}, "", "", "")
			if err != nil {
				t.Fatalf("buildRequest: %v", err)
			}
			if len(req.Messages) != 1 {
				t.Fatalf("%d messages, want one", len(req.Messages))
			}
			if req.Messages[0].Role != "user" {
				t.Errorf("role = %q, want user", req.Messages[0].Role)
			}
			if string(req.Messages[0].Content) != tc.want {
				t.Errorf("content =\n%s\nwant\n%s", req.Messages[0].Content, tc.want)
			}
		})
	}
}

// The sender's own delegation tool_use and the result answering it already
// carry the message into its conversation; rendering the projection too would
// say it twice (Design C).
func TestReplaySkipsASentMessage(t *testing.T) {
	req, _, err := buildRequest("", nil, []domain.Event{
		ev(1, domain.EventAgentThreadMessageSent,
			`{"content":[{"type":"text","text":"go and look"}],"to_session_thread_id":"sthr_1","to_agent_name":"researcher"}`),
		ev(2, domain.EventAgentThreadMessageReceived,
			`{"content":[{"type":"text","text":"done"}],"from_session_thread_id":"sthr_1","from_agent_name":"researcher"}`),
	}, "", "", "")
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("%d messages, want the received one alone", len(req.Messages))
	}
	if string(req.Messages[0].Content) != `[{"text":"[message from researcher]\n\ndone","type":"text"}]` {
		t.Errorf("content = %s, want the received message alone", req.Messages[0].Content)
	}
}

// A turn may mix the two families. The exec calls still escalate their work
// item and an ask still gates them; the delegation call rides along answered by
// the settlement, and never joins askIDs.
func TestTurnEventsMixesDelegationWithExecCalls(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"always_ask"}}}`),
	}}}
	_, class, _, err := resolveTools(agent, nil, delegationCoordinator)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	turn := &turnResult{toolUses: []provider.ToolUse{
		{ID: "toolu_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		{ID: "toolu_2", Name: "create_agent", Input: json.RawMessage(`{"agent_name":"researcher","message":"go"}`)},
	}}

	batch, kind, askIDs, delegated, err := turnEvents(turn, class)
	if err != nil {
		t.Fatalf("turnEvents: %v", err)
	}
	if kind != queue.ToolExec {
		t.Errorf("work kind = %q, want %q — the bash call still needs a driver", kind, queue.ToolExec)
	}
	if len(askIDs) != 1 || askIDs[0] != batch[0].ID {
		t.Errorf("askIDs = %v, want the bash call alone", askIDs)
	}
	if len(delegated) != 1 || delegated[0].name != "create_agent" {
		t.Fatalf("delegated = %v, want the spawn alone", delegated)
	}
	if delegated[0].eventID != batch[1].ID {
		t.Errorf("delegated names %q, want the committed event %q", delegated[0].eventID, batch[1].ID)
	}
}
