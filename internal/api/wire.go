package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// maxBodyBytes bounds request bodies; agent specs are small configuration
// documents, not payloads.
const maxBodyBytes = 4 << 20

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
	return obj, nil
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
// must be strings).
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

// patchMetadata applies update semantics onto existing metadata: a string
// value upserts the key, an explicit null deletes it. emptyDeletes matches
// the environment-update rule, where an empty string also deletes (the
// reference documents this for environments only).
func patchMetadata(existing map[string]string, raw json.RawMessage, emptyDeletes bool) (map[string]string, error) {
	if isNull(raw) {
		return existing, nil
	}
	var patch map[string]*string
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, errInvalid("metadata must be an object of string-or-null values")
	}
	out := map[string]string{}
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil || (emptyDeletes && *v == "") {
			delete(out, k)
		} else {
			out[k] = *v
		}
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
			// configs/default_config are optional.
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

// parseSkills validates skills[]: {type:"anthropic"|"custom", skill_id,
// version?}. The response-side skill carries a required version, and the
// reference defaults an omitted one to the latest — we normalize to the
// literal "latest" (nothing resolves skill versions yet).
func parseSkills(raw json.RawMessage) ([]json.RawMessage, error) {
	items, err := rawList(raw, "skills")
	if err != nil {
		return nil, err
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

// agentSpec is the stored (and rendered) agent configuration. Every field is
// always present so responses satisfy the wire's api:"required" surface.
type agentSpec struct {
	Model       domain.Model      `json:"model"`
	System      string            `json:"system"`
	Description string            `json:"description"`
	Tools       []json.RawMessage `json:"tools"`
	MCPServers  []json.RawMessage `json:"mcp_servers"`
	Skills      []json.RawMessage `json:"skills"`
}

// normalize guarantees non-nil collections so JSON renders [] rather than null.
func (s *agentSpec) normalize() {
	if s.Tools == nil {
		s.Tools = []json.RawMessage{}
	}
	if s.MCPServers == nil {
		s.MCPServers = []json.RawMessage{}
	}
	if s.Skills == nil {
		s.Skills = []json.RawMessage{}
	}
}
