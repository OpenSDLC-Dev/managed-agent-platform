package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

// mcpCatalog is the session's usable MCP listings, keyed by server name: what
// the executor's discovery driver reached and stored, narrowed to the servers
// the agent declares at the endpoints it declares them at. A server that has no
// entry offers no tools this turn.
//
// The listing stays as the row holds it and is decoded where it is used, so that
// a value this platform wrote and cannot read back fails the turn visibly
// (resolveTools' error reaches failTurn) rather than being returned from a
// loader whose other failures are transient and settle by reclaim — which for a
// deterministic decode is a loop that grinds forever without telling anyone.
type mcpCatalog map[string]json.RawMessage

// toolClass says what one model-facing tool name means: the event a call on it
// commits as, the permission policy that gates it, and — for an MCP tool — the
// server and bare name the prefixed model-facing name stands in for, which the
// wire event carries apart.
//
// settlement marks a delegation tool, which commits as an ordinary
// agent.tool_use and is answered inside the settlement transaction rather than
// by any driver (plan 35 decision 6). It has to be a property of the class
// rather than a test on the name, because a single-agent session's agent may
// declare a custom tool called create_agent and that call is the agent's own:
// only the class knows whether this thread's role is what put the name there.
type toolClass struct {
	kind       domain.EventType
	policy     domain.PermissionPolicyType
	server     string
	tool       string
	settlement bool
}

// delegationRole is which of the delegation tools a thread is offered, decided
// by the thread's place in the session rather than by anything its agent
// declares (plan 35 decision 6). A child is a child whatever its own snapshot
// says, which is what holds the topology to one level.
type delegationRole int

const (
	delegationNone delegationRole = iota
	delegationCoordinator
	delegationChild
)

// delegationTools is the definitions a role is offered, and nothing for a
// single-agent session.
func delegationTools(role delegationRole) []json.RawMessage {
	switch role {
	case delegationCoordinator:
		return toolset.CoordinatorTools()
	case delegationChild:
		return toolset.WorkerTools()
	}
	return nil
}

// hasRoster reports whether a resolved agent's snapshot carries a non-empty
// roster — the test that makes a primary thread a coordinator.
//
// It decodes rather than measuring: the column stores an explicit JSON null for
// a single agent, which arrives as a four-byte RawMessage, so a length test
// would class every single-agent primary as a coordinator. A snapshot that will
// not decode is treated as no roster, which costs a coordinator its delegation
// tools rather than letting it spawn against a roster nothing can read — and
// the roster is written by this platform's own resolution (internal/api/roster.go)
// and validated there, so an unreadable one is a defect rather than a client's
// input.
func hasRoster(raw json.RawMessage) bool {
	var p struct {
		Agents []json.RawMessage `json:"agents"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	return len(p.Agents) > 0
}

// The model-facing name of an MCP tool, and the shape a Messages endpoint takes
// a tool name in.
//
// The prefix is ours and it is forced by architecture rather than chosen. The
// reference's own MCP protocol carries a call's server in its own field
// (`mcp_server_name` on agent.mcp_tool_use, `server_name` on the connector's
// block), so the name it shows the model is bare. This brain has no such field:
// it assembles one flat tools[] for an ordinary Messages endpoint, where two
// servers offering `search` would be two definitions under one name. Recorded in
// docs/DIVERGENCES.md.
//
// maxModelToolName and the character class are the documented Messages
// constraint — "Must match the regex ^[a-zA-Z0-9_-]{1,64}$" (the tool-use
// guide's parameter table; the API reference and the SDK types state neither).
// An MCP server is bound by nothing of the sort: 255 characters of server name
// and 128 of tool name are both within what its own wire allows.
const (
	mcpNamePrefix    = "mcp__"
	mcpNameSeparator = "__"
	maxModelToolName = 64
)

// The two tools[] entry types that expand to more than themselves.
const (
	agentToolsetType = "agent_toolset_20260401"
	mcpToolsetType   = "mcp_toolset"
)

// mcpNameFits reports whether a server and a tool name compose into a name a
// model request can carry — and answers before anything is composed. A name
// outside the pinned shape is not one tool's problem: the endpoint rejects the
// whole request, so it costs every tool in it, every turn, on a log that keeps
// replaying the same agent.
//
// The length is settled first because neither half is bounded where it is
// stored: a server name rides the agent spec, bounded only by the API's 4 MiB
// body, and a listing inside one catalog row can hold thousands of tools, so
// composing first would allocate a name per tool only to throw it away.
func mcpNameFits(server, tool string) bool {
	if tool == "" {
		return false
	}
	return len(mcpNamePrefix)+len(server)+len(mcpNameSeparator)+len(tool) <= maxModelToolName
}

// mcpModelName composes the name an MCP tool is offered to the model under. A
// caller deciding whether to offer one asks mcpNameFits first; replay composes
// unconditionally, because the assistant block it rebuilds has to name the tool
// that call was committed under.
//
// Bytes outside the documented class become '_', one for one, so the length
// mcpNameFits settled is the length produced. Mangling rather than dropping is
// what keeps a whole server's tools from vanishing over a naming convention:
// internal/mcp admits the SDK's own tool-name class, which includes '.', so a
// server publishing `github.create_issue` would otherwise offer nothing at all.
// The model's name is not the wire's — agent.mcp_tool_use carries the server and
// the bare tool in two fields of its own, and the class map keeps the pair — so
// a mangled model-facing name costs nothing a reader of the log can see, while
// two tools that sanitize to one name contest it exactly as two that composed to
// one do.
func mcpModelName(server, tool string) string {
	b := make([]byte, 0, len(mcpNamePrefix)+len(server)+len(mcpNameSeparator)+len(tool))
	b = append(b, mcpNamePrefix...)
	b = appendModelName(b, server)
	b = append(b, mcpNameSeparator...)
	b = appendModelName(b, tool)
	return string(b)
}

func appendModelName(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return b
}

// maxNoteLabel bounds a name a note quotes. Nothing caps either half of an MCP
// tool's name where it is stored — a server name rides the agent spec, bounded
// only by the API's 4 MiB body, and the reference's documented 1–255 is not
// enforced (docs/DIVERGENCES.md #66) — while a note is written per tool and per
// turn, so an uncapped one multiplies into the log for as long as the session
// lives. A name past this is a name being used as a payload.
const maxNoteLabel = 256

func noteLabel(s string) string {
	if len(s) <= maxNoteLabel {
		return s
	}
	return toolset.TruncateRunes(s, maxNoteLabel) + "[truncated]"
}

// maxToolNotes bounds how many of a turn's skips are written down. Each note is
// capped on its own, but their number is not: a catalog row holds thousands of
// tools inside its own 256 KiB, a configs[] array is bounded only by the API's
// body limit, and every entry of either can be a skip — on every turn, for as
// long as the session lives. Past a handful the count is the news and the names
// are not, so the rest are summarized in one line.
const maxToolNotes = 16

type toolNotes struct {
	lines   []string
	dropped int
}

func (n *toolNotes) add(format string, args ...any) {
	if len(n.lines) >= maxToolNotes {
		n.dropped++
		return
	}
	n.lines = append(n.lines, fmt.Sprintf(format, args...))
}

func (n *toolNotes) render() []string {
	if n.dropped == 0 {
		return n.lines
	}
	return append(n.lines, fmt.Sprintf(
		"%d further declared tools were not offered this turn", n.dropped))
}

// maxMCPToolBytes bounds what a session's MCP toolsets may add to one provider
// request. Each stored listing is capped where it is written (maxCatalogTools,
// internal/executor/mcpwork.go), but that cap is per server and an agent may
// declare twenty, so without this the request is bounded only by their sum. The
// figure is that per-server cap, for the reason it was chosen: a hundred tools
// averaging a kilobyte fit inside it, and a request carrying more definitions
// than that is an unusable request whatever an endpoint's own limit turns out to
// be. Tools are charged in declaration order, so which ones survive a session
// that overruns is the agent author's ordering rather than a server's.
const maxMCPToolBytes = 256 << 10

// resolveTools turns the thread's role, the agent's tools[] and the session's
// MCP catalog into the two halves of a turn's tool surface: the definitions the
// model is offered, and the class of each name it may call back. The third
// return is the notes worth telling an operator — every tool an agent declared
// and the model was not offered, and why.
//
// The role's delegation tools are the platform's own and are laid down before
// anything an agent declared: a coordinator's four on the primary thread of a
// session with a roster, a child's two on any child, none otherwise (plan 35
// decision 6). They class as settlement-executed, and being first is what makes
// an agent's custom tool of one of those names the one that is dropped.
//
// Custom tools are Messages-API tool definitions minus the union discriminator;
// an agent_toolset entry expands to the built-in tools it enables (bash, read,
// write, edit, glob, grep, web_fetch, web_search), which the executor runs in
// the sandbox; an mcp_toolset expands to the tools its server reported, resolved
// against the entry's default_config and configs[].
//
// The agent's own tools follow, and the MCP ones last, so a name declared by the
// agent's author always beats a name a third-party server chose — whatever order
// tools[] lists them in. Those are the only ordering rules here; within each
// group the declaration order stands.
//
// A note is not a failure. An MCP server that could not be reached, a tool whose
// prefixed name the endpoint would reject, a name already taken, a configs[]
// entry naming a tool the server does not report, a definition past the budget
// one request carries: each costs its own tool and nothing else, because the
// alternative — failing the turn — takes down an agent whose other tools work
// over a third party's listing it does not control. The one hard error is a
// permission policy this platform cannot evaluate, which is the #26 fail-open:
// defaulting it would run an unconfirmed tool.
func resolveTools(agent domain.ResolvedAgent, cat mcpCatalog, role delegationRole) ([]json.RawMessage, map[string]toolClass, []string, error) {
	var defs []json.RawMessage
	class := map[string]toolClass{}
	var notes toolNotes

	// The name is read back out of the definition rather than listed beside it,
	// so the set the model is offered and the set it may call back cannot drift
	// apart: there are at most four of them and they are static.
	injected := map[string]bool{}
	for _, def := range delegationTools(role) {
		var probe struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(def, &probe); err != nil {
			return nil, nil, nil, fmt.Errorf("delegation tool: %w", err)
		}
		defs = append(defs, def)
		class[probe.Name] = toolClass{kind: domain.EventAgentToolUse, settlement: true}
		injected[probe.Name] = true
	}
	// Inside a session that delegates, the settlement claims all six names —
	// but offers only this thread's half. The other half is classed and not
	// defined, which is what earns a model reaching for it wrongRole's answer
	// — naming the tool it should have reached for instead — rather than
	// #567's generic "unknown tool": an unclassed name is answered safely
	// either way now, just less usefully, since the settlement would have no
	// way to know it was one of the six at all.
	//
	// Classed before the agent's own tools are read, so an agent that really
	// declares a custom tool of one of these names still wins it back below,
	// exactly where the name was not offered to this thread.
	if role != delegationNone {
		for _, name := range toolset.AllDelegationTools() {
			if _, taken := class[name]; !taken {
				class[name] = toolClass{kind: domain.EventAgentToolUse, settlement: true}
			}
		}
	}

	for _, raw := range agent.Tools {
		var probe struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, nil, nil, fmt.Errorf("agent tool: %w", err)
		}
		switch probe.Type {
		case "custom":
			if injected[probe.Name] {
				// A delegation tool is the platform's, and the model must not
				// be able to reach an agent's own code through one of their
				// names. Only this arm can contest one: no built-in is named
				// like a delegation tool, and every MCP name is mcp__-prefixed,
				// so neither of the other two expansions can collide.
				notes.add("the agent's custom tool %q was not offered: the platform's delegation tool of that name shadows it",
					noteLabel(probe.Name))
				continue
			}
			def, err := json.Marshal(map[string]any{
				"name": probe.Name, "description": probe.Description, "input_schema": probe.InputSchema,
			})
			if err != nil {
				return nil, nil, nil, err
			}
			defs = append(defs, def)
			class[probe.Name] = toolClass{kind: domain.EventAgentCustomToolUse}
		case agentToolsetType:
			builtins, err := toolset.Tools(raw)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("agent tool: %w", err)
			}
			policies, err := toolset.Policies(raw)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("agent tool: %w", err)
			}
			defs = append(defs, builtins...)
			for name, p := range policies {
				class[name] = toolClass{kind: domain.EventAgentToolUse, policy: p}
			}
		}
	}

	spent, overBudget := 0, 0
	for _, raw := range agent.Tools {
		var probe struct {
			Type   string `json:"type"`
			Server string `json:"mcp_server_name"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, nil, nil, fmt.Errorf("agent tool: %w", err)
		}
		if probe.Type != mcpToolsetType {
			continue
		}
		stored, ok := cat[probe.Server]
		if !ok {
			notes.add("no tools were offered from MCP server %q: it has no listing this turn",
				noteLabel(probe.Server))
			continue
		}
		var listing []toolset.MCPTool
		if err := json.Unmarshal(stored, &listing); err != nil {
			return nil, nil, nil, fmt.Errorf("mcp catalog for %q: %w", probe.Server, err)
		}
		resolved, unknown, err := toolset.ResolveMCP(raw, listing)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("agent tool: %w", err)
		}
		for _, name := range unknown {
			notes.add("MCP server %q does not report a tool named %q, which its toolset configures",
				noteLabel(probe.Server), noteLabel(name))
		}
		for _, r := range resolved {
			if !mcpNameFits(probe.Server, r.Name) {
				notes.add("MCP tool %q on server %q was not offered: together they do not compose a "+
					"name a model request can carry (at most %d bytes)",
					noteLabel(r.Name), noteLabel(probe.Server), maxModelToolName)
				continue
			}
			name := mcpModelName(probe.Server, r.Name)
			if _, taken := class[name]; taken {
				// Nothing here needs noteLabel: this branch is past
				// mcpNameFits, so the composed name is at most
				// maxModelToolName and both halves of it are shorter still. A
				// contest is not only with the agent's own tools — two servers
				// can compose to one string (a server named a__b with tool c,
				// and a server named a with tool b__c), as can two names that
				// sanitize to one — and declaration order settles all of them.
				notes.add("MCP tool %q on server %q was not offered: another tool is already named %q",
					r.Name, probe.Server, name)
				continue
			}
			def, err := json.Marshal(map[string]any{
				"name": name, "description": r.Description, "input_schema": r.InputSchema,
			})
			if err != nil {
				return nil, nil, nil, err
			}
			if spent+len(def) > maxMCPToolBytes {
				overBudget++
				continue
			}
			spent += len(def)
			defs = append(defs, def)
			class[name] = toolClass{
				kind: domain.EventAgentMCPToolUse, policy: r.Policy,
				server: probe.Server, tool: r.Name,
			}
		}
	}
	if overBudget > 0 {
		notes.add("%d MCP tools were not offered: this session's listings hold more than the %d bytes "+
			"of tool definitions one request carries", overBudget, maxMCPToolBytes)
	}
	return defs, class, notes.render(), nil
}

// loadMCPCatalog reads the session's catalog into the listings request assembly
// may use, and names the declared servers that have no row at all.
//
// The three answers are deliberately distinct. A server with a usable row
// contributes its tools. A server with a *failed* row contributes none and is
// not reported undiscovered: its failure is already recorded, the session keeps
// running without it, and reporting it would suspend the turn to re-dial an
// endpoint that just refused — every turn, forever, for a server that is simply
// down. A server with no row has never been attempted, and that is the one the
// caller suspends for.
//
// A row whose url no longer matches what the agent declares counts as no row.
// The mid-session patch that repointed the server deletes the rows it
// invalidates in its own transaction, so this is a second line rather than the
// first — but a listing attributed to the wrong endpoint would surface as a
// model calling tools that do not exist, and the check costs a string compare.
//
// The declared servers are the caller's, already parsed: a spec this platform
// stored and cannot read back is a permanent failure of this session rather than
// a transient one, and the caller is where the two are told apart.
func (b *Brain) loadMCPCatalog(ctx context.Context, sid, threadID domain.ID, declared []mcpServerRef) (mcpCatalog, []string, error) {
	if len(declared) == 0 {
		return nil, nil, nil
	}

	// The thread's own listings (plan 35 decision 14): each thread declares
	// its agent's servers and is discovered separately, NULL the primary's.
	rows, err := b.pool.Query(ctx,
		`SELECT server_name, url, status, tools FROM mcp_catalogs
		  WHERE session_id = $1 AND thread_id IS NOT DISTINCT FROM $2`,
		sid.String(), events.NullableThread(threadID))
	if err != nil {
		return nil, nil, fmt.Errorf("read mcp catalog: %w", err)
	}
	defer rows.Close()
	type row struct {
		url, status string
		tools       []byte
	}
	stored := map[string]row{}
	for rows.Next() {
		var name string
		var r row
		if err := rows.Scan(&name, &r.url, &r.status, &r.tools); err != nil {
			return nil, nil, fmt.Errorf("read mcp catalog: %w", err)
		}
		stored[name] = r
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read mcp catalog: %w", err)
	}

	cat := mcpCatalog{}
	var undiscovered []string
	for _, s := range declared {
		r, ok := stored[s.name]
		if !ok || r.url != s.url {
			undiscovered = append(undiscovered, s.name)
			continue
		}
		if r.status != "ready" {
			continue
		}
		cat[s.name] = r.tools
	}
	return cat, undiscovered, nil
}

// mcpServerRef is one entry of the agent's mcp_servers array, reduced to the
// two fields the catalog is keyed on.
type mcpServerRef struct {
	name, url string
}

// declaredMCPServers reads those two fields out of the agent's spec.
//
// An entry missing either is skipped rather than reported: there is nothing to
// dial, so waiting for its listing would never end. The API rejects both at the
// boundary; this mirrors the discovery driver's own skip so the two sides cannot
// disagree about what is discoverable.
func declaredMCPServers(agent domain.ResolvedAgent) ([]mcpServerRef, error) {
	var out []mcpServerRef
	for _, raw := range agent.MCPServers {
		var probe struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("agent mcp_servers: %w", err)
		}
		if probe.Name == "" || probe.URL == "" {
			continue
		}
		out = append(out, mcpServerRef{name: probe.Name, url: probe.URL})
	}
	return out, nil
}

// suspendForDiscovery hands the turn back as MCP work: nothing is assembled,
// nothing is sent to the model, and the session stays running while the
// discovery driver reaches the servers this session has never listed. The
// driver's own settlement chains the model turn once the listings are in.
//
// The two halves commit together, under the session lock, for the reason every
// other settlement here does: a completed item with no successor leaves the
// session running with nothing scheduled, which archive and delete both refuse
// and only a user.interrupt gets out of. It appends no event — the log's job is
// the conversation, and waiting for a listing is not part of one — but goes
// through the log all the same, because that is what refuses an archived
// session under the same lock.
//
// The enqueue may find an mcp_exec already live and do nothing, which is
// correct: Enqueue is keyed (session_id, kind) over the live states, and the
// item already there will chain the turn when it settles.
func (b *Brain) suspendForDiscovery(ctx context.Context, sid domain.ID, item *queue.Item, servers []string) error {
	// Bounded for the reason a note's names are: a server name is agent-supplied
	// and capped by nothing before the API's 4 MiB body, and this line is written
	// once per undiscovered server on the first turn of every session.
	labels := make([]string, len(servers))
	for i, s := range servers {
		labels[i] = noteLabel(s)
	}
	slog.InfoContext(ctx, "brain: turn suspended for MCP discovery",
		"session_id", sid.String(), "servers", labels)
	_, err := b.log.AppendWith(ctx, sid, nil, events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			if err := b.queue.Complete(ctx, tx, item); err != nil {
				return err
			}
			_, err := b.queue.Enqueue(ctx, tx, item.EnvironmentID, sid, queue.MCPExec)
			return err
		},
	})
	if err != nil {
		return fmt.Errorf("suspend for mcp discovery: %w", err)
	}
	return nil
}

// logToolNotes puts every tool an agent declared and the model was not offered
// where an operator can find it. It is the skills- and files-injection
// precedent: a reference this platform cannot resolve is a logged miss, not a
// failed turn, and the log is where a miss goes because no wire event describes
// one — session.error names connection and authentication failures, and this is
// neither.
func logToolNotes(ctx context.Context, sid domain.ID, notes []string) {
	for _, n := range notes {
		slog.WarnContext(ctx, "brain: "+n, "session_id", sid.String())
	}
}
