package modelgateway

import (
	"net/http"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// modelInfo is Anthropic's ModelInfo (checked against anthropic-sdk-go
// v1.70.1 — model.go ModelInfo), every field the SDK marks required present.
type modelInfo struct {
	Type           string       `json:"type"`
	ID             string       `json:"id"`
	DisplayName    string       `json:"display_name"`
	CreatedAt      time.Time    `json:"created_at"`
	MaxInputTokens int64        `json:"max_input_tokens"`
	MaxTokens      int64        `json:"max_tokens"`
	Capabilities   capabilities `json:"capabilities"`
}

type support struct {
	Supported bool `json:"supported"`
}

// capabilities is ModelCapabilities (model.go ModelCapabilities). A
// deployment records tools, thinking, vision and two token limits, so image
// input and thinking answer from those and every capability it does not
// record answers false.
type capabilities struct {
	Batch             support `json:"batch"`
	Citations         support `json:"citations"`
	CodeExecution     support `json:"code_execution"`
	ContextManagement struct {
		ClearThinking20251015 support `json:"clear_thinking_20251015"`
		ClearToolUses20250919 support `json:"clear_tool_uses_20250919"`
		Compact20260112       support `json:"compact_20260112"`
		Supported             bool    `json:"supported"`
	} `json:"context_management"`
	Effort struct {
		High      support `json:"high"`
		Low       support `json:"low"`
		Max       support `json:"max"`
		Medium    support `json:"medium"`
		Supported bool    `json:"supported"`
		Xhigh     support `json:"xhigh"`
	} `json:"effort"`
	ImageInput        support `json:"image_input"`
	PDFInput          support `json:"pdf_input"`
	StructuredOutputs support `json:"structured_outputs"`
	Thinking          struct {
		Supported bool `json:"supported"`
		Types     struct {
			Adaptive support `json:"adaptive"`
			Enabled  support `json:"enabled"`
		} `json:"types"`
	} `json:"thinking"`
}

// describe answers for an alias what every one of its targets offers: a
// capability only when each records it, a token limit the smallest each
// records, and zero when one records none — a caller relying on the answer
// is right whichever target serves it.
func describe(snap *catalog.Snapshot, a store.Alias) modelInfo {
	m := modelInfo{Type: "model", ID: a.Name, DisplayName: a.DisplayName, CreatedAt: a.CreatedAt}
	if m.DisplayName == "" {
		m.DisplayName = a.Name
	}
	vision, thinking := len(a.Targets) > 0, len(a.Targets) > 0
	var maxIn, maxOut int64 = -1, -1
	smallest := func(have, v int64) int64 {
		switch {
		case v == 0 || have == 0:
			return 0
		case have < 0 || v < have:
			return v
		}
		return have
	}
	for _, t := range a.Targets {
		d, _ := snap.Deployment(t.DeploymentID)
		vision = vision && d.Capabilities.Vision
		thinking = thinking && d.Capabilities.Thinking
		maxIn = smallest(maxIn, d.Capabilities.MaxInputTokens)
		maxOut = smallest(maxOut, d.Capabilities.MaxTokens)
	}
	m.MaxInputTokens, m.MaxTokens = max(maxIn, 0), max(maxOut, 0)
	m.Capabilities.ImageInput.Supported = vision
	m.Capabilities.Thinking.Supported = thinking
	m.Capabilities.Thinking.Types.Enabled.Supported = thinking
	return m
}

// listable reports whether the caller sees a in Anthropic's list: a chat
// alias its grant covers. The wildcard is a fallback, not a model.
func listable(c caller, a store.Alias) bool {
	return a.Kind == store.KindChat && a.Name != "*" && c.may(a.Name)
}

// listModels pages through the aliases by name. after_id and before_id are
// names rather than positions, so a page stays well defined when an alias is
// added or removed between two requests.
func (h *handler) listModels(w http.ResponseWriter, r *http.Request, c caller) {
	q := r.URL.Query()
	limit := 20
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, r, invalid("limit: must be between 1 and 1000"))
			return
		}
		limit = n
	}
	after, before := q.Get("after_id"), q.Get("before_id")
	if after != "" && before != "" {
		writeError(w, r, invalid("after_id and before_id cannot both be set"))
		return
	}
	snap := h.cfg.Catalog.Snapshot()
	var names []store.Alias
	for _, a := range snap.Aliases() {
		if listable(c, a) && (after == "" || a.Name > after) && (before == "" || a.Name < before) {
			names = append(names, a)
		}
	}
	more := len(names) > limit
	switch {
	case !more:
	case before != "":
		names = names[len(names)-limit:]
	default:
		names = names[:limit]
	}
	data := make([]modelInfo, 0, len(names))
	for _, a := range names {
		data = append(data, describe(snap, a))
	}
	var first, last *string
	if len(data) > 0 {
		first, last = &data[0].ID, &data[len(data)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "has_more": more, "first_id": first, "last_id": last})
}

func (h *handler) getModel(w http.ResponseWriter, r *http.Request, c caller, id string) {
	snap := h.cfg.Catalog.Snapshot()
	// Alias answers a name it does not hold with the wildcard, which listable
	// refuses, so only the exact name is found.
	if a, ok := snap.Alias(id); ok && listable(c, a) {
		writeJSON(w, http.StatusOK, describe(snap, a))
		return
	}
	writeError(w, r, notFound("model: %s", id))
}
