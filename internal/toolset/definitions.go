package toolset

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// The permission policy each toolset kind resolves to when an entry sets none.
// Both are documented rather than inferred: "Each toolset kind has its own
// default: the agent toolset defaults to `always_allow`, and MCP toolsets
// default to `always_ask`", and, for the built-in kind again, "`default_config`
// is optional. If you omit it, the agent toolset is enabled with the default
// permission policy, `always_allow`" (the reference's permission-policies
// guide). MCP asks by default so that a tool newly appearing on someone else's
// server cannot execute without approval.
const (
	DefaultAgentToolsetPolicy = domain.PolicyAlwaysAllow
	DefaultMCPToolsetPolicy   = domain.PolicyAlwaysAsk
)

// The two tool types that share the default_config / configs shape. They name
// themselves in every error this package raises, so a client reading a 400
// knows which entry of tools[] to fix.
const (
	agentToolsetType = "agent_toolset_20260401"
	mcpToolsetType   = "mcp_toolset"
)

// definitions are the eight built-in tools in the order the reference lists
// them, each already in the Messages-API tool shape the provider request
// carries (name / description / input_schema). The six sandbox tools' schemas
// are the wire's, field for field — anthropic-sdk-go's
// BetaManagedAgentsAgentToolset20260401*Input types are what the model's tool
// calls are validated against on the other side, so a property this platform
// invents is a property no reference client would send. The two web tools have
// no such Input types (see their own comment below).
var definitions = []toolDef{
	{
		name: "bash",
		// The redirection sentence is ours, and it is behavioural rather than
		// cosmetic: a backgrounded process inherits the call's output stream and
		// holds it open after the command itself has exited, so an unredirected
		// straggler costs about two seconds per call while the daemon force-closes
		// it. Telling the model up front is cheaper than paying it every call.
		description: "Run a bash command in a persistent shell. State (cwd, env vars) persists across calls. " +
			"Redirect the output of any process you background (`cmd > /tmp/out 2>&1 &`): a straggler still " +
			"holding this call's output stream open delays the result.",
		props: map[string]any{
			"command":    prop("string", "The command to run."),
			"restart":    prop("boolean", "Restart the persistent shell before running."),
			"timeout_ms": prop("integer", "Per-call timeout in milliseconds."),
		},
	},
	{
		name:        "read",
		description: "Read a UTF-8 text file. Relative paths resolve against the workdir.",
		props: map[string]any{
			"file_path": prop("string", "Path of the file to read."),
			"view_range": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"description": "[start_line, end_line] 1-indexed inclusive. end_line of 0 or less means to end of file.",
			},
		},
		required: []string{"file_path"},
	},
	{
		name:        "write",
		description: "Write a UTF-8 text file, creating parent directories as needed. Relative paths resolve against the workdir.",
		props: map[string]any{
			"file_path": prop("string", "Path of the file to write."),
			"content":   prop("string", "Full file contents to write."),
		},
		required: []string{"file_path", "content"},
	},
	{
		name:        "edit",
		description: "Replace a unique occurrence of old_string with new_string in a file (set replace_all to replace every occurrence).",
		props: map[string]any{
			"file_path":   prop("string", "Path of the file to edit."),
			"old_string":  prop("string", "Substring to find and replace."),
			"new_string":  prop("string", "Replacement text."),
			"replace_all": prop("boolean", "Replace every occurrence instead of requiring a unique match."),
		},
		required: []string{"file_path", "old_string", "new_string"},
	},
	{
		name:        "glob",
		description: "List paths matching a glob pattern (e.g. **/*.go), newest first.",
		props: map[string]any{
			"pattern": prop("string", "Glob pattern, e.g. **/*.go (** matches any depth)."),
			"path":    prop("string", "Directory to search in. Defaults to the workdir."),
		},
		required: []string{"pattern"},
	},
	{
		name:        "grep",
		description: "Search file contents for a regular expression, returning matching lines as path:line:text.",
		props: map[string]any{
			"pattern": prop("string", "Regular expression to search for."),
			"path":    prop("string", "Directory to search in. Defaults to the workdir."),
		},
		required: []string{"pattern"},
	},
	// The two web tools execute in the executor's own process, not the sandbox
	// (docs/plan/15_web-tools.md) — web:true is what routes their work to the
	// web_exec item and keeps their names out of every sandbox-tool scan.
	// Unlike the six above, the wire carries no Input type for them (the
	// official client toolset implements only the six), so these schemas are
	// this platform's minimal reading, recorded INFERRED in docs/DIVERGENCES.md.
	{
		name:        "web_fetch",
		description: "Fetch a web page by absolute http(s) URL, returning its content as markdown text.",
		props: map[string]any{
			"url": prop("string", "Absolute http(s) URL of the page to fetch."),
		},
		required: []string{"url"},
		web:      true,
	},
	{
		name:        "web_search",
		description: "Search the web, returning the top results — title, source URL, and a content snippet for each.",
		props: map[string]any{
			"query": prop("string", "The search query."),
		},
		required: []string{"query"},
		web:      true,
	},
}

// IsWebTool reports whether name is a built-in tool that executes in the
// executor's process rather than the sandbox. The executor's sandbox scan, the
// BYOC worker's, and the queue-kind decisions all consult this one predicate,
// so the split cannot drift between them.
func IsWebTool(name string) bool {
	for _, d := range definitions {
		if d.name == name {
			return d.web
		}
	}
	return false
}

type toolDef struct {
	name        string
	description string
	props       map[string]any
	required    []string
	// web marks a tool that executes in the executor's process (web_exec
	// work), never in the sandbox.
	web bool
}

func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

// marshal renders the definition as the provider request's tool shape.
func (d toolDef) marshal() (json.RawMessage, error) {
	schema := map[string]any{"type": "object", "properties": d.props}
	if len(d.required) > 0 {
		schema["required"] = d.required
	}
	return json.Marshal(map[string]any{
		"name": d.name, "description": d.description, "input_schema": schema,
	})
}

// entry is an agent's agent_toolset_20260401 tool, as stored on the resolved
// agent. Every field is optional here though the reference renders them on every
// resolved agent: this platform stores the client's tools verbatim, so a bare
// entry is what a bare entry arrives as. Omitted enabled means the reference's
// resolved default — the toolset is on; omitted permission_policy means
// DefaultAgentToolsetPolicy.
type entry struct {
	DefaultConfig *struct {
		Enabled          *bool         `json:"enabled"`
		PermissionPolicy *policyConfig `json:"permission_policy"`
	} `json:"default_config"`
	Configs []struct {
		Name             string        `json:"name"`
		Enabled          *bool         `json:"enabled"`
		PermissionPolicy *policyConfig `json:"permission_policy"`
	} `json:"configs"`
}

// policyConfig is the wire's {"type":…} permission_policy object.
type policyConfig struct {
	Type string `json:"type"`
}

// resolved pairs an enabled built-in tool's definition with its resolved
// permission policy.
type resolved struct {
	def    toolDef
	policy domain.PermissionPolicyType
}

// resolveToolset applies an agent_toolset_20260401 entry's default_config and
// per-tool configs onto the built-in definitions, returning the enabled tools in
// the wire's order with each tool's resolved enable state dropped and its policy
// kept. Enable and policy resolve independently: a per-tool config overrides
// default_config, which overrides the toolset default (on / DefaultAgentToolsetPolicy).
func resolveToolset(raw json.RawMessage) ([]resolved, error) {
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", agentToolsetType, err)
	}
	if err := rejectUnknownToolsetKeys(agentToolsetType, raw); err != nil {
		return nil, err
	}

	enabled := true
	var defaultPolicy *policyConfig
	if e.DefaultConfig != nil {
		if e.DefaultConfig.Enabled != nil {
			enabled = *e.DefaultConfig.Enabled
		}
		defaultPolicy = e.DefaultConfig.PermissionPolicy
	}

	type override struct {
		enabled *bool
		policy  *policyConfig
	}
	overrides := make(map[string]override, len(e.Configs))
	for _, c := range e.Configs {
		o := overrides[c.Name]
		if c.Enabled != nil {
			o.enabled = c.Enabled
		}
		if c.PermissionPolicy != nil {
			o.policy = c.PermissionPolicy
		}
		overrides[c.Name] = o
	}

	var out []resolved
	for _, d := range definitions {
		o := overrides[d.name]
		on := enabled
		if o.enabled != nil {
			on = *o.enabled
		}
		if !on {
			continue
		}
		// Resolve and validate the policy only for tools that are actually
		// enabled: a per-tool config's policy overrides default_config's, which
		// overrides DefaultAgentToolsetPolicy. An unevaluable policy is a hard
		// error, but only when a live tool would carry it — a malformed policy
		// on a disabled or overridden-away tool has no effect and is ignored.
		pc := defaultPolicy
		if o.policy != nil {
			pc = o.policy
		}
		policy := DefaultAgentToolsetPolicy
		if pc != nil {
			p, err := policyType(agentToolsetType, pc.Type)
			if err != nil {
				return nil, err
			}
			policy = p
		}
		out = append(out, resolved{def: d, policy: policy})
	}
	return out, nil
}

// rejectUnknownToolsetKeys fails on any key outside the pinned toolset wire
// schema — at the toolset object and every nested default_config, configs[]
// entry, and permission_policy. kind names the entry's type in the error and
// selects the keys the toolset object itself accepts, the one place the two
// kinds differ: an mcp_toolset carries mcp_server_name where an
// agent_toolset_20260401 carries nothing extra.
// encoding/json silently drops unknown object keys,
// so without this a misspelled permission_policy (the issue's permission_polciy) is
// discarded, PermissionPolicy stays nil, and the tool resolves to the always_allow
// default rather than the intended gate — a fail-open at the human-in-the-loop
// boundary (issue #26). It is eager, not lazy like policyType: a malformed stored
// object is the defect, so a typo on a disabled tool — a latent fail-open that
// activates when the tool is enabled — is rejected too. Errors name the field's
// path so a client can find the typo. It runs after resolveToolset's typed
// unmarshal, so every object it revisits has already parsed as the right JSON shape.
//
// The accepted keys are anthropic-sdk-go v1.66.0's request (*Params) types in
// betaagent.go: BetaManagedAgentsAgentToolset20260401Params (type/configs/
// default_config), AgentToolsetDefaultConfigParams (enabled/permission_policy),
// the eight BetaManagedAgents<Tool>ToolConfigParams variants of
// AgentToolConfigParamsUnion (name/type/enabled/permission_policy), and the
// always_allow/always_ask policy params (type only). The two web variants carry
// allowed_domains / blocked_domains and max_content_tokens / user_location as
// well; those four stay refused, because this platform's egress allow-list is
// operator-side configuration (WEBTOOL_ALLOWED_DOMAINS) and accepting the field
// would promise a per-agent policy nothing enforces (docs/DIVERGENCES.md).
func rejectUnknownToolsetKeys(kind string, raw json.RawMessage) error {
	top, ok := jsonObject(raw)
	if !ok {
		return nil // not an object: the typed unmarshal already reported it
	}
	allowed := []string{"type", "configs", "default_config"}
	if kind == mcpToolsetType {
		allowed = append(allowed, "mcp_server_name")
	}
	if err := rejectKeysOutside(kind, top, "", allowed...); err != nil {
		return err
	}
	if dc, ok := jsonObject(top["default_config"]); ok {
		if err := rejectConfigKeys(kind, dc, "default_config", false); err != nil {
			return err
		}
	}
	for i, item := range jsonArray(top["configs"]) {
		if c, ok := jsonObject(item); ok {
			if err := rejectConfigKeys(kind, c, fmt.Sprintf("configs[%d]", i), true); err != nil {
				return err
			}
		}
	}
	return nil
}

// rejectConfigKeys checks a default_config or configs[] object and its nested
// permission_policy. perTool adds "name", accepted only on a configs[] entry,
// and — on the built-in kind alone — "type": v1.66.0 split AgentToolConfigParams
// into a union whose eight variants each tag themselves with the tool's own name
// in both keys. It is optional on the request and required on the response, so a
// v1.66.0 client may or may not send it. Neither kind's default_config gained
// one, and BetaManagedAgentsMCPToolConfigParams did not either.
func rejectConfigKeys(kind string, obj map[string]json.RawMessage, path string, perTool bool) error {
	allowed := []string{"enabled", "permission_policy"}
	builtinTool := perTool && kind == agentToolsetType
	if perTool {
		allowed = append(allowed, "name")
	}
	if builtinTool {
		allowed = append(allowed, "type")
	}
	if err := rejectKeysOutside(kind, obj, path, allowed...); err != nil {
		return err
	}
	if builtinTool {
		if err := checkToolType(kind, obj, path); err != nil {
			return err
		}
	}
	if pp, ok := jsonObject(obj["permission_policy"]); ok {
		return rejectKeysOutside(kind, pp, path+".permission_policy", "type")
	}
	return nil
}

// checkToolType requires a built-in configs[] entry's discriminator to name the
// tool the entry configures. The union tags each variant with one constant for
// both name and type, so an entry whose two disagree names two variants at once
// and there is no way to tell which the client meant: rejecting it beats
// silently configuring whichever key this platform happens to read (INFERRED —
// the reference's own answer is unrecorded; docs/DIVERGENCES.md). An absent type
// is fine, the request marking it omitzero. Both are read as *string because
// encoding/json accepts a JSON null into a plain string as its zero value, and
// {"name":null,"type":""} would otherwise pass as two equal empty strings.
func checkToolType(kind string, obj map[string]json.RawMessage, path string) error {
	raw, ok := obj["type"]
	if !ok {
		return nil
	}
	var typ *string
	if err := json.Unmarshal(raw, &typ); err != nil || typ == nil {
		return fmt.Errorf("%s: %s.type must be a string", kind, path)
	}
	var name *string
	if err := json.Unmarshal(obj["name"], &name); err != nil || name == nil {
		return fmt.Errorf("%s: %s.type needs a string name to equal", kind, path)
	}
	if *typ != *name {
		return fmt.Errorf("%s: %s.type is %q but must equal name %q", kind, path, *typ, *name)
	}
	return nil
}

// rejectKeysOutside fails on the first key of obj not in allowed, naming its path
// (the toolset object itself for the empty path).
func rejectKeysOutside(kind string, obj map[string]json.RawMessage, path string, allowed ...string) error {
	for k := range obj {
		if slices.Contains(allowed, k) {
			continue
		}
		if path == "" {
			return fmt.Errorf("%s: unknown field %q", kind, k)
		}
		return fmt.Errorf("%s: unknown field %q in %s", kind, k, path)
	}
	return nil
}

// jsonObject decodes raw as a JSON object, reporting false for null, a non-object,
// or absent input — the cases resolveToolset's typed unmarshal has already accepted
// or rejected, so they carry no unknown-key check of their own.
func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// jsonArray decodes raw as a JSON array, returning nil for null / non-array /
// absent input (again already handled by the typed unmarshal).
func jsonArray(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil
	}
	return a
}

// policyType validates a permission_policy's type against the wire enum. An
// unknown value (including empty) is a rejection, never a silent default: a
// policy this platform cannot evaluate must not resolve to "run it anyway".
func policyType(kind, s string) (domain.PermissionPolicyType, error) {
	switch p := domain.PermissionPolicyType(s); p {
	case domain.PolicyAlwaysAllow, domain.PolicyAlwaysAsk:
		return p, nil
	default:
		return "", fmt.Errorf("%s: unknown permission_policy type %q", kind, s)
	}
}

// Validate checks that an agent_toolset_20260401 entry resolves — its enable
// flags and the permission policies of its enabled tools are well-formed. It is
// the create-time counterpart to Tools/Policies: an entry that fails here would
// otherwise be stored on the agent and wedge every turn when the brain resolves
// it, so the API validates at agent creation to make a malformed toolset a 400
// instead.
func Validate(raw json.RawMessage) error {
	_, err := resolveToolset(raw)
	return err
}

// Tools returns the model-facing definitions of the built-in tools an
// agent_toolset_20260401 entry enables, in the wire's order.
func Tools(raw json.RawMessage) ([]json.RawMessage, error) {
	rs, err := resolveToolset(raw)
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, r := range rs {
		def, err := r.def.marshal()
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, nil
}

// Policies resolves the permission policy of every built-in tool an
// agent_toolset_20260401 entry enables, keyed by tool name. It mirrors Tools'
// enable resolution, so disabled tools are absent; the brain reads it to stamp
// evaluated_permission on each tool_use and to decide whether a turn's calls
// suspend for human confirmation.
func Policies(raw json.RawMessage) (map[string]domain.PermissionPolicyType, error) {
	rs, err := resolveToolset(raw)
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.PermissionPolicyType, len(rs))
	for _, r := range rs {
		out[r.def.name] = r.policy
	}
	return out, nil
}
