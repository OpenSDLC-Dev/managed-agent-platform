package brain

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// mcpTool is one listing entry, as the catalog stores it.
func mcpTool(name string) toolset.MCPTool {
	return toolset.MCPTool{
		Name: name, Description: "Does " + name + ".",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

// listingOf renders tools the way a catalog row stores them.
func listingOf(t *testing.T, tools ...toolset.MCPTool) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// defNames pulls the model-facing names out of the assembled definitions.
func defNames(t *testing.T, defs []json.RawMessage) []string {
	t.Helper()
	out := make([]string, len(defs))
	for i, raw := range defs {
		var d struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("definition %d: %v", i, err)
		}
		out[i] = d.Name
	}
	return out
}

func hasNote(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// modelToolName is the documented rule, written out so this test pins it
// literally rather than re-deriving whatever the brain happens to compose.
var modelToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Both edges of that rule matter. A bound one short refuses names the endpoint
// accepts and the loss is silent — a tool the agent declared simply never
// reaches the model — while a name one byte too long, or one byte outside the
// class, costs the whole request and every tool in it, on every turn of a
// session that replays.
func TestAComposedMCPNameMatchesTheDocumentedRule(t *testing.T) {
	// mcp__ and __ frame the pair, so the two names together may spend
	// maxModelToolName less that frame.
	frame := len(mcpNamePrefix) + len(mcpNameSeparator)
	exact := strings.Repeat("t", maxModelToolName-frame-len("docs"))

	cases := []struct {
		server, tool string
		fits         bool
		want         string
	}{
		{"docs", "search", true, "mcp__docs__search"},
		{"docs", "Aa0_-", true, "mcp__docs__Aa0_-"},
		// A listing entry with no name is not a tool, and nothing composes for it.
		{"docs", "", false, ""},
		{"docs", exact, true, "mcp__docs__" + exact},
		{"docs", exact + "t", false, ""},
		// Bytes outside the class are mangled rather than dropped: internal/mcp
		// admits the SDK's own tool-name class, which includes '.', so a server
		// publishing dotted names would otherwise offer nothing at all.
		{"git.hub", "create.issue", true, "mcp__git_hub__create_issue"},
		{"docs", "search files", true, "mcp__docs__search_files"},
		// One rune, four bytes. The bound counts bytes and the class replaces
		// bytes, so the two agree on what this name costs.
		{"docs", "🔎", true, "mcp__docs__" + strings.Repeat("_", 4)},
	}
	for _, tc := range cases {
		if got := mcpNameFits(tc.server, tc.tool); got != tc.fits {
			t.Errorf("mcpNameFits(%q, %q) = %v, want %v", tc.server, tc.tool, got, tc.fits)
			continue
		}
		if !tc.fits {
			continue
		}
		got := mcpModelName(tc.server, tc.tool)
		if got != tc.want {
			t.Errorf("mcpModelName(%q, %q) = %q, want %q", tc.server, tc.tool, got, tc.want)
		}
		if !modelToolName.MatchString(got) {
			t.Errorf("mcpModelName(%q, %q) = %q, which the documented rule rejects", tc.server, tc.tool, got)
		}
	}
}

// noteLabel cuts on a rune boundary: a server name is customer-supplied text,
// and a label cut mid-rune reaches the log as replacement characters that name
// nothing.
func TestNoteLabelCutsOnARuneBoundary(t *testing.T) {
	// Three-byte runes over the cap: the cut lands mid-rune unless backed off.
	long := strings.Repeat("界", maxNoteLabel)
	got := noteLabel(long)
	if !strings.HasSuffix(got, "[truncated]") {
		t.Fatalf("label = %q, want it marked truncated", got)
	}
	head := strings.TrimSuffix(got, "[truncated]")
	if !utf8.ValidString(head) {
		t.Errorf("label head is not valid UTF-8: %q", head)
	}
	if len(head) > maxNoteLabel {
		t.Errorf("label head is %d bytes, want at most %d", len(head), maxNoteLabel)
	}
	if short := noteLabel("界"); short != "界" {
		t.Errorf("noteLabel(%q) = %q, want it untouched", "界", short)
	}
}

// An MCP tool reaches the model under mcp__{server}__{tool}: this brain sends
// one flat tools[] to an ordinary Messages endpoint, which carries no server
// field to keep the two names apart. The event the call commits under carries
// them apart again, which is what the class records.
func TestMCPToolsAreOfferedUnderAPrefixedName(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"custom","name":"ask_user","description":"Ask.","input_schema":{"type":"object"}}`),
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`),
	}}}
	cat := mcpCatalog{"docs": listingOf(t, mcpTool("search"), mcpTool("fetch"))}

	defs, class, notes, err := resolveTools(agent, cat, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none", notes)
	}
	want := []string{"ask_user", "mcp__docs__search", "mcp__docs__fetch"}
	if got := defNames(t, defs); !slicesEqual(got, want) {
		t.Fatalf("offered = %v, want %v", got, want)
	}
	c, ok := class["mcp__docs__search"]
	if !ok {
		t.Fatal("the prefixed name has no class")
	}
	if c.kind != domain.EventAgentMCPToolUse {
		t.Errorf("kind = %q, want %q", c.kind, domain.EventAgentMCPToolUse)
	}
	if c.server != "docs" || c.tool != "search" {
		t.Errorf("class = %+v, want server docs and tool search", c)
	}
	// The MCP toolset's default is ask, not the built-in toolset's allow.
	if c.policy != domain.PolicyAlwaysAsk {
		t.Errorf("policy = %q, want %q", c.policy, domain.PolicyAlwaysAsk)
	}
}

// The Messages API pins a tool's name to ^[a-zA-Z0-9_-]{1,64}$, and an MCP
// server chooses its own names under no such rule — 255 characters of server
// name and 128 of tool name both fit the MCP wire. A name the endpoint would
// reject costs the whole request, every turn, on a log that keeps replaying it,
// so the tool is dropped and the rest of the listing still reaches the model.
func TestAnOverlongMCPToolNameCostsOnlyItsOwnTool(t *testing.T) {
	long := strings.Repeat("n", 64)
	cat := mcpCatalog{"docs": listingOf(t,
		mcpTool(long), // mcp__docs__ + 64 is past the ceiling
		mcpTool("search files"),
		mcpTool("fetch"),
	)}
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`),
	}}}

	defs, class, notes, err := resolveTools(agent, cat, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	// The space is outside the class but the name still fits, so that tool is
	// mangled into the request rather than lost with the overlong one.
	want := []string{"mcp__docs__search_files", "mcp__docs__fetch"}
	if got := defNames(t, defs); !slicesEqual(got, want) {
		t.Fatalf("offered = %v, want %v", got, want)
	}
	if len(class) != 2 {
		t.Errorf("class = %v, want only the offered tools", class)
	}
	if c := class["mcp__docs__search_files"]; c.tool != "search files" {
		t.Errorf("class tool = %q, want the server's own name kept for the wire", c.tool)
	}
	if len(notes) != 1 || !hasNote(notes, long) {
		t.Errorf("notes = %v, want only the overlong tool named", notes)
	}
}

// Two names that mangle alike contest the one they compose to, exactly as two
// that composed to it directly would: the mangling is what the model sees, and
// a request cannot carry two definitions under one name.
func TestTwoToolNamesThatSanitizeAlikeContestOneName(t *testing.T) {
	cat := mcpCatalog{"docs": listingOf(t, mcpTool("find.files"), mcpTool("find files"))}
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`),
	}}}

	defs, class, notes, err := resolveTools(agent, cat, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if got := defNames(t, defs); !slicesEqual(got, []string{"mcp__docs__find_files"}) {
		t.Fatalf("offered = %v, want one definition", got)
	}
	if c := class["mcp__docs__find_files"]; c.tool != "find.files" {
		t.Errorf("class tool = %q, want the first-listed name to hold it", c.tool)
	}
	if len(notes) != 1 || !hasNote(notes, "find files") {
		t.Errorf("notes = %v, want the loser named", notes)
	}
}

// Every skip is written down, but their number is not bounded by anything an
// operator controls: one catalog row lists thousands of tools inside its own
// cap, and a session replays the same listing on every turn. Past a handful the
// count is the news and the names are not.
func TestTheNotesOneTurnWritesAreBounded(t *testing.T) {
	long := strings.Repeat("n", 64) // past the ceiling under any server name
	var tools []toolset.MCPTool
	for i := 0; i < maxToolNotes+4; i++ {
		tools = append(tools, mcpTool(long+string(rune('a'+i))))
	}
	cat := mcpCatalog{"docs": listingOf(t, tools...)}
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`),
	}}}

	defs, _, notes, err := resolveTools(agent, cat, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("offered %d tools under names no request can carry", len(defs))
	}
	if len(notes) != maxToolNotes+1 {
		t.Fatalf("notes = %d, want %d and one summary", len(notes), maxToolNotes)
	}
	if !strings.Contains(notes[len(notes)-1], "4 further declared tools") {
		t.Errorf("last note = %q, want it to count the rest", notes[len(notes)-1])
	}
}

// The per-server catalog cap bounds one listing; an agent may declare twenty.
// What one request carries has to be bounded on its own, and by declaration
// order, so which tools survive is the agent author's ordering and not a
// server's.
func TestOneRequestCarriesABoundedSetOfMCPDefinitions(t *testing.T) {
	// A schema large enough that a handful of tools exhaust the budget.
	fat := toolset.MCPTool{
		Name:        "search",
		InputSchema: json.RawMessage(`{"type":"object","description":"` + strings.Repeat("d", maxMCPToolBytes*3/4) + `"}`),
	}
	second := fat
	second.Name = "fetch"
	third := fat
	third.Name = "list"
	cat := mcpCatalog{"docs": listingOf(t, fat, second, third)}
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`),
	}}}

	defs, class, notes, err := resolveTools(agent, cat, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if got := defNames(t, defs); !slicesEqual(got, []string{"mcp__docs__search"}) {
		t.Fatalf("offered = %v, want the budget to stop after the first", got)
	}
	if len(class) != 1 {
		t.Errorf("class = %v, want a dropped tool to be uncallable too", class)
	}
	spent := 0
	for _, d := range defs {
		spent += len(d)
	}
	if spent > maxMCPToolBytes {
		t.Errorf("offered %d bytes of definitions, want at most %d", spent, maxMCPToolBytes)
	}
	if len(notes) != 1 || !hasNote(notes, "2 MCP tools were not offered") {
		t.Errorf("notes = %v, want the dropped tools counted", notes)
	}
}

// A note is written per tool and per turn, and neither half of an MCP tool's
// name is capped where it is stored: a server name rides the agent spec, which
// the API bounds at megabytes rather than at the reference's documented 255. An
// uncapped note multiplies that into the log for as long as the session lives.
func TestANoteQuotesABoundedName(t *testing.T) {
	huge := strings.Repeat("s", 100_000)
	other := strings.Repeat("o", 100_000)

	cases := []struct {
		name  string
		tools string
		cat   mcpCatalog
		want  int // notes expected
	}{{
		// A server with no listing: the "no listing" note.
		name:  "no listing",
		tools: `{"type":"mcp_toolset","mcp_server_name":"` + huge + `"}`,
		cat:   mcpCatalog{},
		want:  1,
	}, {
		// A listing whose every composed name is unusable, plus a configs[]
		// entry naming a tool the server does not report: the other two notes.
		name: "unusable names and an unknown config",
		tools: `{"type":"mcp_toolset","mcp_server_name":"` + huge + `",
		         "configs":[{"name":"` + other + `","enabled":true}]}`,
		cat:  mcpCatalog{huge: nil},
		want: 3,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cat[huge] == nil && len(tc.cat) > 0 {
				tc.cat[huge] = listingOf(t, mcpTool("search"), mcpTool("fetch"))
			}
			agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{
				Tools: []json.RawMessage{json.RawMessage(tc.tools)},
			}}
			defs, _, notes, err := resolveTools(agent, tc.cat, delegationNone)
			if err != nil {
				t.Fatalf("resolveTools: %v", err)
			}
			if len(defs) != 0 {
				t.Fatalf("offered %d tools under a name no request can carry", len(defs))
			}
			if len(notes) != tc.want {
				t.Fatalf("notes = %d (%v), want %d", len(notes), notes, tc.want)
			}
			for _, n := range notes {
				if len(n) > 4*maxNoteLabel {
					t.Errorf("a note is %d bytes, want the quoted names bounded", len(n))
				}
			}
		})
	}
}

// A tool name is one namespace. A custom tool the agent's author named
// mcp__docs__search wins it — the author's own definition is not displaced by a
// third party's listing — and the MCP tool is dropped rather than sent as a
// second definition under a name the endpoint already has.
func TestAnMCPToolLosesAContestedName(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`),
		json.RawMessage(`{"type":"custom","name":"mcp__docs__search","description":"Mine.","input_schema":{"type":"object"}}`),
	}}}
	cat := mcpCatalog{"docs": listingOf(t, mcpTool("search"), mcpTool("fetch"))}

	defs, class, notes, err := resolveTools(agent, cat, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if got := defNames(t, defs); !slicesEqual(got, []string{"mcp__docs__search", "mcp__docs__fetch"}) {
		t.Fatalf("offered = %v, want the custom tool and the uncontested MCP one", got)
	}
	if c := class["mcp__docs__search"]; c.kind != domain.EventAgentCustomToolUse {
		t.Errorf("kind = %q, want the custom tool to keep the name", c.kind)
	}
	if !hasNote(notes, "mcp__docs__search") {
		t.Errorf("notes = %v, want the dropped MCP tool named", notes)
	}
}

// Flattening two names into one string is not injective: a server named a__b
// offering c and a server named a offering b__c compose to mcp__a__b__c. The
// contest is settled the same way as one with a custom tool — declaration
// order, with the loser noted — rather than by escaping a separator the
// reference never publishes.
func TestTwoServersCanContestOneComposedName(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"a__b"}`),
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"a"}`),
	}}}
	cat := mcpCatalog{
		"a__b": listingOf(t, mcpTool("c")),
		"a":    listingOf(t, mcpTool("b__c"), mcpTool("d")),
	}

	defs, class, notes, err := resolveTools(agent, cat, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if got := defNames(t, defs); !slicesEqual(got, []string{"mcp__a__b__c", "mcp__a__d"}) {
		t.Fatalf("offered = %v, want the first claim on the contested name plus the uncontested one", got)
	}
	if c := class["mcp__a__b__c"]; c.server != "a__b" || c.tool != "c" {
		t.Errorf("class = %+v, want the first-declared server to keep the name", c)
	}
	if !hasNote(notes, `"b__c"`) {
		t.Errorf("notes = %v, want the dropped tool named", notes)
	}
}

// A server whose discovery failed has no listing, so its toolset offers
// nothing. That is not a failed turn — the session keeps running without that
// server's tools, and the next discovery pass re-attempts it.
func TestAServerWithNoListingOffersNothing(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"agent_toolset_20260401","configs":[{"name":"bash"}]}`),
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`),
	}}}

	defs, class, notes, err := resolveTools(agent, mcpCatalog{}, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("the built-in toolset was dropped along with the MCP one")
	}
	for _, name := range defNames(t, defs) {
		if strings.HasPrefix(name, "mcp__") {
			t.Errorf("offered %q from a server with no listing", name)
		}
	}
	if _, ok := class["mcp__docs__search"]; ok {
		t.Error("a tool from an undiscovered server has a class")
	}
	if !hasNote(notes, "docs") {
		t.Errorf("notes = %v, want the server named", notes)
	}
}

// An MCP server's tools are unknowable when the agent is written, so a
// configs[] entry naming one the server does not report is a note and never an
// error — the reference's documented dynamic-availability semantics.
func TestUnknownConfigNamesAreNotedNotFatal(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs",
		                  "configs":[{"name":"summarise","permission_policy":{"type":"always_allow"}}]}`),
	}}}
	cat := mcpCatalog{"docs": listingOf(t, mcpTool("search"))}

	defs, _, notes, err := resolveTools(agent, cat, delegationNone)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if got := defNames(t, defs); !slicesEqual(got, []string{"mcp__docs__search"}) {
		t.Fatalf("offered = %v, want the one reported tool", got)
	}
	if !hasNote(notes, "summarise") {
		t.Errorf("notes = %v, want the unconfigurable name reported", notes)
	}
}

// A policy this platform cannot evaluate still fails the turn, MCP or not: the
// alternative is running a tool nobody confirmed (#26).
func TestAnUnevaluableMCPPolicyFailsAssembly(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs",
		                  "configs":[{"name":"search","permission_policy":{"type":"always_deny"}}]}`),
	}}}
	cat := mcpCatalog{"docs": listingOf(t, mcpTool("search"))}

	if _, _, _, err := resolveTools(agent, cat, delegationNone); err == nil {
		t.Fatal("resolveTools accepted a policy it cannot evaluate")
	}
}

// A catalog row this platform wrote and cannot read back is corrupt state, not
// a third party's answer, and it fails the turn where a failed listing would
// only cost its server's tools. The distinction is which side wrote the bytes.
func TestACorruptListingFailsAssembly(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"docs"}`),
	}}}
	cat := mcpCatalog{"docs": json.RawMessage(`{"not":"a listing"}`)}

	if _, _, _, err := resolveTools(agent, cat, delegationNone); err == nil {
		t.Fatal("resolveTools accepted a listing it could not decode")
	}
}

// A declared server with no address is nothing to dial, so it is not something
// to wait for either — a turn suspended for one would never resume. The API
// rejects both fields empty at the boundary and the discovery driver skips the
// same shape; this keeps the two sides agreeing on what is discoverable.
func TestADeclaredServerWithNoAddressIsNotWaitedFor(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{MCPServers: []json.RawMessage{
		json.RawMessage(`{"type":"url","name":"docs","url":""}`),
		json.RawMessage(`{"type":"url","name":"","url":"https://x.test/mcp"}`),
		json.RawMessage(`{"type":"url","name":"real","url":"https://y.test/mcp"}`),
	}}}
	got, err := declaredMCPServers(agent)
	if err != nil {
		t.Fatalf("declaredMCPServers: %v", err)
	}
	if len(got) != 1 || got[0].name != "real" {
		t.Errorf("declared = %+v, want only the server with both fields", got)
	}

	bad := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{MCPServers: []json.RawMessage{
		json.RawMessage(`"not an object"`),
	}}}
	if _, err := declaredMCPServers(bad); err == nil {
		t.Error("declaredMCPServers accepted a malformed mcp_servers entry")
	}
}

// A tools[] entry that is not an object at all is corrupt session state, not a
// third party's listing: nothing can be offered and nothing can be classified,
// so the turn fails visibly rather than silently losing a tool.
func TestAMalformedToolEntryFailsAssembly(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Tools: []json.RawMessage{
		json.RawMessage(`"not an object"`),
	}}}
	if _, _, _, err := resolveTools(agent, nil, delegationNone); err == nil {
		t.Fatal("resolveTools accepted a malformed tools[] entry")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
