package toolset_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// delegationDef is one definition decoded far enough to assert its shape: the
// name the model calls back, and the schema it is told to call under.
type delegationDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	} `json:"input_schema"`
}

func decodeDefs(t *testing.T, raws []json.RawMessage) []delegationDef {
	t.Helper()
	out := make([]delegationDef, len(raws))
	for i, raw := range raws {
		if err := json.Unmarshal(raw, &out[i]); err != nil {
			t.Fatalf("definition %d: %v", i, err)
		}
	}
	return out
}

// The two sets are the topology: a coordinator addresses its children, a child
// reports to its coordinator, and neither is offered the other's tools — which
// is what keeps the roster one level deep.
func TestDelegationToolsSplitByRole(t *testing.T) {
	coord := names(t, toolset.CoordinatorTools())
	want := []string{"create_agent", "send_to_agent", "list_agents", "wait_for_agents"}
	if !equal(coord, want) {
		t.Errorf("CoordinatorTools = %v, want %v", coord, want)
	}
	worker := names(t, toolset.WorkerTools())
	want = []string{"submit_result", "send_to_parent"}
	if !equal(worker, want) {
		t.Errorf("WorkerTools = %v, want %v", worker, want)
	}
	for _, c := range coord {
		for _, w := range worker {
			if c == w {
				t.Errorf("%q is offered to both roles", c)
			}
		}
	}
}

// Every turn of a thread reassembles its request from the log, so these bytes
// are a contract rather than an implementation detail: a reworded description
// moves the request prefix of every coordinator and every child, on every
// replay. The digests pin exactly that. Failing here is not a bug to route
// around — it means a definition moved, and the pin is updated by the change
// that meant to move it.
func TestDelegationDefinitionsArePinned(t *testing.T) {
	want := map[string]string{
		"create_agent":    "0f04173d57d8849ea8966dfb3a61c89260e564b8027591bb0686cc843f5d4d77",
		"send_to_agent":   "c09ae72c8d327e136f0aa078e886c352263f472ddfaee1ff1fac3dee779dd38f",
		"list_agents":     "a6ead7fc11c20f93927400b80d9068a35a751ccc1de9237d602f1d8d4f6e1cbc",
		"wait_for_agents": "3f7be7105079dd123fd3c8176d8aadb99078ce77e8b91395e58ac1f277e0fac4",
		"submit_result":   "8fa5068c12d5eafb12547e29135342b78a54f2264e1c4ff4e17da5258e710dd8",
		"send_to_parent":  "778f3507085ad1441601dfc0d13ca5854c8e483f292dfa603de84e08e035c7f9",
	}
	raws := append(toolset.CoordinatorTools(), toolset.WorkerTools()...)
	if len(raws) != len(want) {
		t.Fatalf("%d definitions, want %d pinned", len(raws), len(want))
	}
	for i, d := range decodeDefs(t, raws) {
		got := fmt.Sprintf("%x", sha256.Sum256(raws[i]))
		if got != want[d.Name] {
			t.Errorf("%s moved: sha256 %s, pinned %s\n%s", d.Name, got, want[d.Name], raws[i])
		}
	}
	// And each caller gets its own slice: one that rewrote what it was handed
	// would otherwise move every later request's prefix from inside the
	// package.
	for _, tc := range []struct {
		name string
		get  func() []json.RawMessage
	}{
		{"CoordinatorTools", toolset.CoordinatorTools},
		{"WorkerTools", toolset.WorkerTools},
	} {
		mine := tc.get()
		mine[0] = json.RawMessage(`{"name":"clobbered"}`)
		if bytes.Equal(tc.get()[0], mine[0]) {
			t.Errorf("%s: the returned slice aliases the package's own", tc.name)
		}
	}
}

// The schemas are ours (INFERRED, docs/DIVERGENCES.md): the reference documents
// what the six tools do and not what they take, so this pins what the model is
// told to send.
func TestDelegationSchemas(t *testing.T) {
	want := map[string]struct {
		props    []string
		required []string
	}{
		"create_agent":    {props: []string{"agent_name", "message"}, required: []string{"agent_name", "message"}},
		"send_to_agent":   {props: []string{"session_thread_id", "agent_name", "message"}, required: []string{"message"}},
		"list_agents":     {},
		"wait_for_agents": {},
		"submit_result":   {props: []string{"result"}, required: []string{"result"}},
		"send_to_parent":  {props: []string{"message"}, required: []string{"message"}},
	}
	defs := decodeDefs(t, append(toolset.CoordinatorTools(), toolset.WorkerTools()...))
	if len(defs) != len(want) {
		t.Fatalf("%d definitions, want %d", len(defs), len(want))
	}
	for _, d := range defs {
		w, ok := want[d.Name]
		if !ok {
			t.Errorf("unexpected tool %q", d.Name)
			continue
		}
		if d.Description == "" {
			t.Errorf("%s: no description", d.Name)
		}
		if d.InputSchema.Type != "object" {
			t.Errorf("%s: input_schema type = %q, want object", d.Name, d.InputSchema.Type)
		}
		if len(d.InputSchema.Properties) != len(w.props) {
			t.Errorf("%s: properties = %v, want %v", d.Name, d.InputSchema.Properties, w.props)
		}
		for _, p := range w.props {
			if _, ok := d.InputSchema.Properties[p]; !ok {
				t.Errorf("%s: no %q property", d.Name, p)
			}
		}
		if !equal(d.InputSchema.Required, w.required) {
			t.Errorf("%s: required = %v, want %v", d.Name, d.InputSchema.Required, w.required)
		}
	}
}

// The predicate is what the API's tool-result validation and the workers' scans
// consult, so it must admit exactly the tools the brain injects — no more, and
// no fewer.
func TestIsDelegationTool(t *testing.T) {
	for _, d := range decodeDefs(t, append(toolset.CoordinatorTools(), toolset.WorkerTools()...)) {
		if !toolset.IsDelegationTool(d.Name) {
			t.Errorf("IsDelegationTool(%q) = false, want true", d.Name)
		}
	}
	for _, name := range []string{"bash", "read", "web_fetch", "mcp__srv__create_agent", "", "create_agents"} {
		if toolset.IsDelegationTool(name) {
			t.Errorf("IsDelegationTool(%q) = true, want false", name)
		}
	}
	// The whole built-in set, not a sample of it: the runnable classification
	// excludes every name this predicate admits, so a built-in that ever
	// collided with a delegation name would be classed as nobody's work and
	// its calls would hang unanswered instead of failing loudly.
	builtins, err := toolset.Tools(json.RawMessage(`{"type":"agent_toolset_20260401"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names(t, builtins) {
		if toolset.IsDelegationTool(name) {
			t.Errorf("built-in %q is also a delegation name: the runnable classification would skip it", name)
		}
	}
}
