package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// maxBodyBytes bounds request bodies; agent specs are small configuration
// documents, not payloads.
const maxBodyBytes = 4 << 20

// nulEscape is the six bytes of the JSON escape for U+0000, spelled as a byte
// slice so no editor or tool can rewrite the sequence into the byte it denotes.
var nulEscape = []byte{'\\', 'u', '0', '0', '0', '0'}

// decodeObject reads the request body as a single JSON object, keyed by raw
// field so handlers can distinguish omitted / null / value — the reference
// updates are patches where that distinction is semantic.
func decodeObject(r *http.Request) (map[string]json.RawMessage, error) {
	// Read one byte past the limit so oversize bodies are detected as such
	// instead of being truncated into a misleading JSON parse error.
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, errInvalid("could not read request body")
	}
	if len(raw) > maxBodyBytes {
		return nil, &apiError{http.StatusRequestEntityTooLarge, errTypeRequestTooLarge,
			fmt.Sprintf("request body larger than %d bytes", maxBodyBytes)}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errInvalid("request body must be a JSON object")
	}
	if obj == nil {
		return map[string]json.RawMessage{}, nil
	}
	if err := rejectNULBody(raw); err != nil {
		return nil, err
	}
	return obj, nil
}

// rejectNULBody rejects U+0000 anywhere in the request body. It is valid JSON
// (the \u0000 escape) but Postgres can store it in neither a text column
// (SQLSTATE 22021) nor a jsonb value (22P05), nor bind it in the text[] the
// work endpoint's metadata deletes use — so letting it through turns a
// well-formed request into a 500 at insert time.
//
// The guard sits here, on the decode every JSON *object* body passes through,
// rather than on the individual field parsers: the unstorable byte is a property
// of the request, not of any one field, and the nested raw-JSON payloads (the
// agent spec's tools/mcp_servers/skills, the environment config's package lists
// and allowed_hosts) never reach stringField at all. One chokepoint is also what
// keeps the endpoints from diverging — the Go-side metadata merge behind
// agents/environments/sessions would otherwise drop an unstorable delete key as
// a silent no-op while the identical patch against the work endpoint's SQL-side
// merge failed. internal/events applies the same rule to inbound event payloads.
//
// Two body reads are deliberately not covered, neither of which can reach
// Postgres: parseStopForce reads the stop body's single bool directly, and the
// archive/ack/heartbeat handlers read no body at all. Path IDs and query
// parameters are a separate surface carrying the same defect — see #135.
func rejectNULBody(raw []byte) error {
	// A raw 0x00 byte in a JSON string is itself invalid JSON, so the object
	// decode above has already rejected it: the six-byte escape is the only way
	// a NUL survives into a decoded string. Its absence therefore proves the
	// body clean without a second parse, which on the events endpoint — the one
	// carrying megabyte tool output — is the difference between ~14ms and ~0.04ms.
	if !bytes.Contains(raw, nulEscape) {
		return nil
	}
	// UseNumber keeps number literals as their source text. Decoding into the
	// default float64 would reject an out-of-range literal such as 1e400 — legal
	// JSON that Postgres stores happily, and that reaches here through any
	// passthrough field (a custom tool's input_schema, the environment config) —
	// turning a working request into a 400 on a body this guard should only ever
	// have inspected. json.Number is a distinct type, so the walk ignores it.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return errInvalid("request body must be a JSON object")
	}
	return rejectNUL(v, "")
}

// rejectNUL walks every decoded string — object keys and values alike — and
// names the offending path, so a client can find the value instead of hunting
// through its own request.
func rejectNUL(v any, path string) error {
	const rule = `must not contain U+0000 (the \u0000 escape): it cannot be stored`
	switch x := v.(type) {
	case string:
		if strings.ContainsRune(x, 0) {
			return errInvalid("%s %s", fieldPath(path), rule)
		}
	case []any:
		for i, elem := range x {
			if err := rejectNUL(elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, elem := range x {
			// A NUL-bearing key is reported by its parent: echoing the key back
			// would put the byte we just rejected into the error message.
			if strings.ContainsRune(k, 0) {
				return errInvalid("%s keys %s", fieldPath(path), rule)
			}
			child := k
			if path != "" {
				child = path + "." + k
			}
			if err := rejectNUL(elem, child); err != nil {
				return err
			}
		}
	}
	return nil
}

// fieldPath names the request itself when the offending value sits at the root
// (a top-level key), where there is no field path to quote.
func fieldPath(path string) string {
	if path == "" {
		return "the request body"
	}
	return path
}

// checkID rejects a malformed path id with the 404 an unknown id already gets.
// Path IDs and id-shaped query parameters are the separate surface rejectNULBody
// flags: http.ServeMux decodes %00 into a real NUL in PathValue / URL.Query, and
// — like any non-alphabet or invalid-UTF-8 byte — it binds straight into
// Postgres and fails as a 500 (SQLSTATE 22021). A server-minted id never carries
// such a byte, so validating the id's shape before it reaches a bind parameter
// closes the whole class. resource names the resource in the wire message, so a
// malformed id is indistinguishable from a merely-absent one (see #135).
func checkID(id, resource string) error {
	if !domain.ID(id).Valid() {
		return errNotFound("%s %s not found", resource, id)
	}
	return nil
}

// checkWorkID is checkID for the work API, whose not-found message omits the id
// (matching mapWorkErr's ErrWorkNotFound) so a worker cannot tell a malformed
// work_id from an item it is not allowed to see.
func checkWorkID(id domain.ID) error {
	if !id.Valid() {
		return errNotFound("work item not found")
	}
	return nil
}

// storableText reports whether s can be stored as Postgres text: valid UTF-8
// with no U+0000. An id-shaped query value is validated with domain.ID.Valid
// instead; this covers a free-form filter such as the event types[] list, whose
// members are compared against stored event names — an unknown-but-storable
// value filters to empty, so only the unstorable byte is rejected (with a 400).
func storableText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// rejectUnknownKeys mirrors the reference API's strict parameter validation:
// unrecognized body fields are an error, not a silent no-op — a typo'd field
// name must not vanish into accepted-but-ignored input.
func rejectUnknownKeys(obj map[string]json.RawMessage, allowed ...string) error {
	for key := range obj {
		if !slices.Contains(allowed, key) {
			return errInvalid("unknown field %q", key)
		}
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// stringField parses an optional string field. Returns set=false when the key
// is absent; null=true when explicitly null.
func stringField(obj map[string]json.RawMessage, key string) (val string, set, null bool, err error) {
	raw, ok := obj[key]
	if !ok {
		return "", false, false, nil
	}
	if isNull(raw) {
		return "", true, true, nil
	}
	if e := json.Unmarshal(raw, &val); e != nil {
		return "", true, false, errInvalid("%s must be a string", key)
	}
	return val, true, false, nil
}

// requiredString parses a required, non-empty string field.
func requiredString(obj map[string]json.RawMessage, key string) (string, error) {
	val, set, null, err := stringField(obj, key)
	if err != nil {
		return "", err
	}
	if !set || null || val == "" {
		return "", errInvalid("%s is required", key)
	}
	return val, nil
}

// parseMetadata parses a full metadata object (create semantics: all values
// must be strings). Unstorable U+0000 in a key or value is already gone:
// rejectNULBody rejects it for the whole body, before any parser runs.
func parseMetadata(obj map[string]json.RawMessage) (map[string]string, error) {
	md := map[string]string{}
	raw, ok := obj["metadata"]
	if !ok || isNull(raw) {
		return md, nil
	}
	if err := json.Unmarshal(raw, &md); err != nil {
		return nil, errInvalid("metadata must be an object of string values")
	}
	if md == nil {
		md = map[string]string{}
	}
	return md, nil
}

// splitMetadataPatch partitions a metadata patch object into per-key upserts and
// deletes: a string value upserts the key, an explicit null deletes it, and when
// emptyDeletes is set an empty string also deletes (the reference's environment
// rule). It is the shared string-or-null parse behind both patchMetadata (the
// Go-side merge for agents/environments/sessions) and the work endpoint's
// SQL-side merge, so the two can never drift.
func splitMetadataPatch(raw json.RawMessage, emptyDeletes bool) (upserts map[string]string, deletes []string, err error) {
	var patch map[string]*string
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, nil, errInvalid("metadata must be an object of string-or-null values")
	}
	upserts = map[string]string{}
	for k, v := range patch {
		if v == nil || (emptyDeletes && *v == "") {
			deletes = append(deletes, k)
			continue
		}
		upserts[k] = *v
	}
	return upserts, deletes, nil
}

// patchMetadata applies update semantics onto existing metadata: a string
// value upserts the key, an explicit null deletes it. emptyDeletes matches
// the environment-update rule, where an empty string also deletes (the
// reference documents this for environments only).
func patchMetadata(existing map[string]string, raw json.RawMessage, emptyDeletes bool) (map[string]string, error) {
	if isNull(raw) {
		return existing, nil
	}
	upserts, deletes, err := splitMetadataPatch(raw, emptyDeletes)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range existing {
		out[k] = v
	}
	for _, k := range deletes {
		delete(out, k)
	}
	for k, v := range upserts {
		out[k] = v
	}
	return out, nil
}

// parseModel parses the wire's string-or-object model form and validates it.
func parseModel(raw json.RawMessage) (domain.Model, error) {
	var m domain.Model
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, errInvalid("model must be a model id string or a {id, speed} object")
	}
	if m.ID == "" {
		return m, errInvalid("model.id is required")
	}
	if m.Speed != "" && m.Speed != "standard" && m.Speed != "fast" {
		return m, errInvalid(`model.speed must be "standard" or "fast"`)
	}
	return m, nil
}

// rawList parses a JSON array field into its raw elements. Explicit null is
// treated as an empty list (the reference's "send empty/null to clear").
func rawList(raw json.RawMessage, key string) ([]json.RawMessage, error) {
	if isNull(raw) {
		return []json.RawMessage{}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errInvalid("%s must be an array", key)
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	return items, nil
}

// parseTools validates the tools[] union: each entry must carry a known type
// discriminator and that variant's required fields. The full raw objects are
// preserved for storage so configs round-trip byte-for-byte.
func parseTools(raw json.RawMessage) ([]json.RawMessage, error) {
	items, err := rawList(raw, "tools")
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		var probe struct {
			Type          string          `json:"type"`
			Name          string          `json:"name"`
			Description   string          `json:"description"`
			MCPServerName string          `json:"mcp_server_name"`
			InputSchema   json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, errInvalid("tools entries must be objects")
		}
		switch probe.Type {
		case "agent_toolset_20260401":
			// configs/default_config are optional, but a malformed enable flag
			// or permission_policy must be a 400 here rather than a toolset that
			// wedges every turn when the brain resolves it.
			if err := toolset.Validate(item); err != nil {
				return nil, errInvalid("%s", err)
			}
		case "custom":
			if probe.Name == "" || probe.Description == "" || len(probe.InputSchema) == 0 || isNull(probe.InputSchema) {
				return nil, errInvalid("custom tools require name, description, and input_schema")
			}
		case "mcp_toolset":
			if probe.MCPServerName == "" {
				return nil, errInvalid("mcp_toolset tools require mcp_server_name")
			}
		default:
			return nil, errInvalid("unknown tool type %q", probe.Type)
		}
	}
	return items, nil
}

// parseMCPServers validates mcp_servers[]: only {type:"url", name, url}.
func parseMCPServers(raw json.RawMessage) ([]json.RawMessage, error) {
	items, err := rawList(raw, "mcp_servers")
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		var probe struct {
			Type string `json:"type"`
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, errInvalid("mcp_servers entries must be objects")
		}
		if probe.Type != "url" || probe.Name == "" || probe.URL == "" {
			return nil, errInvalid(`mcp_servers entries require type "url", name, and url`)
		}
	}
	return items, nil
}

// maxSkillsPerSession is the reference's published cap, counted across every
// agent — a v1 session resolves exactly one agent, so it binds on that
// agent's skills[] (base spec and session override alike).
const maxSkillsPerSession = 500

// parseSkills validates skills[]: {type:"anthropic"|"custom", skill_id,
// version?}. The response-side skill carries a required version, and the
// reference defaults an omitted one to the latest — we normalize to the
// literal "latest"; resolution happens at use time (materialization,
// injection), never at create.
func parseSkills(raw json.RawMessage) ([]json.RawMessage, error) {
	items, err := rawList(raw, "skills")
	if err != nil {
		return nil, err
	}
	if len(items) > maxSkillsPerSession {
		return nil, errInvalid("skills lists at most %d entries per session", maxSkillsPerSession)
	}
	for i, item := range items {
		var probe struct {
			Type    string `json:"type"`
			SkillID string `json:"skill_id"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, errInvalid("skills entries must be objects")
		}
		if (probe.Type != "anthropic" && probe.Type != "custom") || probe.SkillID == "" {
			return nil, errInvalid(`skills entries require type "anthropic" or "custom" and skill_id`)
		}
		if probe.Version == "" {
			probe.Version = "latest"
		}
		normalized, err := json.Marshal(map[string]string{
			"type": probe.Type, "skill_id": probe.SkillID, "version": probe.Version,
		})
		if err != nil {
			return nil, err
		}
		items[i] = normalized
	}
	return items, nil
}

// utc normalizes a nullable timestamp for rendering: the wire carries UTC
// ("Z"), never a server-local offset.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// agentSpec is the stored (and rendered) agent configuration — the domain
// wire shape (always-present fields, raw collection entries).
type agentSpec = domain.AgentSpec
