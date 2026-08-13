package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"unicode/utf8"

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
type toolClass struct {
	kind   domain.EventType
	policy domain.PermissionPolicyType
	server string
	tool   string
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

func mcpModelName(server, tool string) string {
	return mcpNamePrefix + server + mcpNameSeparator + tool
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
	cut := maxNoteLabel
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "[truncated]"
}

// offerable reports whether a name can be sent as a tool definition. A name
// outside the pinned shape is not one tool's problem: the endpoint rejects the
// request, so it costs every tool in it, every turn, on a log that keeps
// replaying the same agent.
func offerable(name string) bool {
	if name == "" || len(name) > maxModelToolName {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// resolveTools turns the agent's tools[] and the session's MCP catalog into the
// two halves of a turn's tool surface: the definitions the model is offered, and
// the class of each name it may call back. The third return is the notes worth
// telling an operator — every tool an agent declared and the model was not
// offered, and why.
//
// Custom tools are Messages-API tool definitions minus the union discriminator;
// an agent_toolset entry expands to the built-in tools it enables (bash, read,
// write, edit, glob, grep, web_fetch, web_search), which the executor runs in
// the sandbox; an mcp_toolset expands to the tools its server reported, resolved
// against the entry's default_config and configs[].
//
// The agent's own tools are laid down first and the MCP ones after, so a name
// declared by the agent's author always beats a name a third-party server chose
// — whatever order tools[] lists them in. That is the only ordering rule here;
// within each half the declaration order stands.
//
// A note is not a failure. An MCP server that could not be reached, a tool whose
// prefixed name the endpoint would reject, a name already taken, a configs[]
// entry naming a tool the server does not report: each costs its own tool and
// nothing else, because the alternative — failing the turn — takes down an agent
// whose other tools work over a third party's listing it does not control. The
// one hard error is a permission policy this platform cannot evaluate, which is
// the #26 fail-open: defaulting it would run an unconfirmed tool.
func resolveTools(agent domain.ResolvedAgent, cat mcpCatalog) ([]json.RawMessage, map[string]toolClass, []string, error) {
	var defs []json.RawMessage
	class := map[string]toolClass{}
	var notes []string

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
			notes = append(notes, fmt.Sprintf(
				"no tools were offered from MCP server %q: it has no listing this turn",
				noteLabel(probe.Server)))
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
			notes = append(notes, fmt.Sprintf(
				"MCP server %q does not report a tool named %q, which its toolset configures",
				noteLabel(probe.Server), noteLabel(name)))
		}
		for _, r := range resolved {
			name := mcpModelName(probe.Server, r.Name)
			if !offerable(name) {
				notes = append(notes, fmt.Sprintf(
					"MCP tool %q on server %q was not offered: together they do not compose a name "+
						"a model request can carry (letters, digits, underscore and hyphen, at most %d)",
					noteLabel(r.Name), noteLabel(probe.Server), maxModelToolName))
				continue
			}
			if _, taken := class[name]; taken {
				notes = append(notes, fmt.Sprintf(
					"MCP tool %q on server %q was not offered: another tool is already named %q",
					noteLabel(r.Name), noteLabel(probe.Server), noteLabel(name)))
				continue
			}
			def, err := json.Marshal(map[string]any{
				"name": name, "description": r.Description, "input_schema": r.InputSchema,
			})
			if err != nil {
				return nil, nil, nil, err
			}
			defs = append(defs, def)
			class[name] = toolClass{
				kind: domain.EventAgentMCPToolUse, policy: r.Policy,
				server: probe.Server, tool: r.Name,
			}
		}
	}
	return defs, class, notes, nil
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
// An entry missing its name or url is skipped rather than reported: there is
// nothing to dial, so suspending for it would never end. The API rejects both at
// the boundary; this mirrors the discovery driver's own skip so the two sides
// cannot disagree about what is discoverable.
func (b *Brain) loadMCPCatalog(ctx context.Context, sid domain.ID, agent domain.ResolvedAgent) (mcpCatalog, []string, error) {
	declared, err := declaredMCPServers(agent)
	if err != nil {
		return nil, nil, err
	}
	if len(declared) == 0 {
		return nil, nil, nil
	}

	rows, err := b.pool.Query(ctx,
		`SELECT server_name, url, status, tools FROM mcp_catalogs WHERE session_id = $1`,
		sid.String())
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
	slog.InfoContext(ctx, "brain: turn suspended for MCP discovery",
		"session_id", sid.String(), "servers", servers)
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
