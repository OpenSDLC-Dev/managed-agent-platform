package modelgateway

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
)

// Thinking provenance (docs/plan/59_model-gateway.md, "Routing, retries,
// limits"). A thinking block is valid only at the deployment that produced it,
// so every block the gateway returns names its producer — a thinking block's
// signature and a redacted_thinking block's data become wrapPrefix, the
// deployment id, a dot, then the upstream's value — and each deployment is
// sent its own blocks back, unwrapped, and no other. Both fields are opaque to
// a client, which returns them verbatim. Every removal is deterministic, so a
// block returns to its producer under the prefix it was produced under. A
// block whose value is empty has nothing to wrap: it names no producer and is
// sent to no upstream, and neither is any thinking block after it in the same
// answer, whose signature covers it — as the brain keeps no thinking past an
// unsigned block either.
//
// Strip mode is the backstop for an upstream that refuses the history's
// thinking anyway: the request goes once more with every thinking block
// removed, and the answer's first thinking block, when wrapped, carries
// resetPrefix, so later requests drop every thinking block ahead of it, as it
// was produced without them.
const (
	wrapPrefix  = "mapgw1."
	resetPrefix = "mapgw1r."
)

// unwrap reads a wrapped value: its producer, the upstream's own value, and
// whether it carries the reset mark. Deployment ids hold no dot, so anything
// that is not a prefix, an id and a dot is no wrapper at all.
func unwrap(v string) (dep, value string, reset, ok bool) {
	rest, reset := strings.CutPrefix(v, resetPrefix)
	if !reset {
		if rest, ok = strings.CutPrefix(v, wrapPrefix); !ok {
			return "", "", false, false
		}
	}
	dep, value, ok = strings.Cut(rest, ".")
	if !ok || dep == "" {
		return "", "", false, false
	}
	return dep, value, reset, true
}

// provenanceOf reads a content block: for a thinking block, the field that
// carries its provenance — a thinking block's signature, a redacted_thinking
// block's data — and that field's value; for any other block, "". Fields are
// read by their exact keys, as the upstream reads them: a struct would take
// "Type" for "type", and a block the upstream reads as thinking would pass as
// text.
func provenanceOf(b json.RawMessage) (field, value string) {
	var obj map[string]json.RawMessage
	var typ string
	if json.Unmarshal(b, &obj) != nil || json.Unmarshal(obj["type"], &typ) != nil {
		return "", ""
	}
	switch typ {
	case "thinking":
		field = "signature"
	case "redacted_thinking":
		field = "data"
	default:
		return "", ""
	}
	_ = json.Unmarshal(obj[field], &value)
	return field, value
}

// history is a request's messages as provenance reads them. A message holding
// no thinking block goes upstream as the caller sent it.
type history struct{ msgs []histMsg }

type histMsg struct {
	raw    json.RawMessage
	role   string
	obj    map[string]json.RawMessage // set with blocks
	blocks []histBlock                // set only for a message with a thinking block
}

type histBlock struct {
	raw   json.RawMessage
	field string // provenanceOf's field; "" for a block that is not thinking
	dep   string // its producer; "" when it carries no wrapper
	value string // the upstream's value under the wrapper
	reset bool   // it carries the reset mark
	// stale is a thinking block ahead of the newest reset mark, which no
	// deployment is sent.
	stale bool
}

// parseHistory reads a request's messages. It returns nil when provenance has
// nothing to do — no thinking block anywhere — and when the messages are not
// an array of objects, which the upstream answers itself. A block's type can
// name thinking only by those letters or through a \u escape, so messages
// holding neither are not decoded at all.
func parseHistory(raw json.RawMessage) *history {
	if !bytes.Contains(raw, []byte("thinking")) && !bytes.Contains(raw, []byte(`\u`)) {
		return nil
	}
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil
	}
	h := &history{msgs: make([]histMsg, len(msgs))}
	found := false
	for i, m := range msgs {
		var obj map[string]json.RawMessage
		if json.Unmarshal(m, &obj) != nil {
			return nil
		}
		var role string
		_ = json.Unmarshal(obj["role"], &role) // by its exact key, as provenanceOf reads a block
		h.msgs[i] = histMsg{raw: m, role: role}
		var content []json.RawMessage
		if json.Unmarshal(obj["content"], &content) != nil {
			continue // a string, or something the upstream refuses itself
		}
		blocks := make([]histBlock, len(content))
		thinking := false
		for j, b := range content {
			blocks[j] = parseBlock(b)
			thinking = thinking || blocks[j].field != ""
		}
		if !thinking {
			continue
		}
		found = true
		h.msgs[i].obj = obj
		h.msgs[i].blocks = blocks
	}
	if !found {
		return nil
	}
	h.markStale()
	return h
}

func parseBlock(b json.RawMessage) histBlock {
	field, v := provenanceOf(b)
	blk := histBlock{raw: b, field: field}
	if field != "" {
		blk.dep, blk.value, blk.reset, _ = unwrap(v)
	}
	return blk
}

// markStale marks every thinking block ahead of the newest reset-marked one.
func (h *history) markStale() {
	found := false
	for i := len(h.msgs) - 1; i >= 0; i-- {
		bs := h.msgs[i].blocks
		for j := len(bs) - 1; j >= 0; j-- {
			if bs[j].field == "" {
				continue
			}
			if found {
				bs[j].stale = true
			}
			found = found || bs[j].reset
		}
	}
}

// producer is the deployment among attempts that produced the newest block a
// request keeps, or "" when no such block names one of them.
func (h *history) producer(attempts []catalog.Attempt) string {
	if h == nil {
		return ""
	}
	for i := len(h.msgs) - 1; i >= 0; i-- {
		bs := h.msgs[i].blocks
		for j := len(bs) - 1; j >= 0; j-- {
			b := bs[j]
			if b.field == "" || b.stale || b.dep == "" {
				continue
			}
			for _, at := range attempts {
				if at.Deployment.ID == b.dep {
					return b.dep
				}
			}
		}
	}
	return ""
}

// carries reports whether dep is sent any thinking block, which is whether
// strip mode would change its request.
func (h *history) carries(dep string) bool {
	if h == nil {
		return false
	}
	for _, m := range h.msgs {
		for _, b := range m.blocks {
			if b.keptFor(dep) {
				return true
			}
		}
	}
	return false
}

func (b histBlock) keptFor(dep string) bool {
	return b.field != "" && !b.stale && b.dep != "" && b.dep == dep
}

// messagesFor is the messages dep is sent: its own thinking blocks unwrapped,
// every other thinking block removed — all of them in strip mode — and an
// assistant message the removal empties removed with it, the user turns it
// separated joined into one, as the Messages API itself combines consecutive
// same-role turns.
func (h *history) messagesFor(dep string, strip bool) json.RawMessage {
	var out []outMsg
	emptied := false
	for _, m := range h.msgs {
		raw := m.raw
		if m.blocks != nil {
			var kept []json.RawMessage
			for _, b := range m.blocks {
				switch {
				case b.field == "":
					kept = append(kept, b.raw)
				case !strip && b.keptFor(dep):
					kept = append(kept, withString(b.raw, b.field, b.value))
				}
			}
			if len(kept) == 0 {
				emptied = true
				continue
			}
			obj := make(map[string]json.RawMessage, len(m.obj))
			for k, v := range m.obj {
				obj[k] = v
			}
			obj["content"], _ = json.Marshal(kept)
			raw, _ = json.Marshal(obj)
		}
		n := len(out)
		if !(emptied && n > 0 && m.role == "user" && out[n-1].role == "user" && out[n-1].join(raw)) {
			out = append(out, outMsg{raw: raw, role: m.role})
		}
		emptied = false
	}
	raws := make([]json.RawMessage, len(out))
	for i, o := range out {
		raws[i] = o.encode()
	}
	b, _ := json.Marshal(raws)
	return b
}

// outMsg is one message messagesFor sends. A user turn that later turns join
// collects their content as blocks and is encoded once, at the end.
type outMsg struct {
	raw    json.RawMessage
	role   string
	obj    map[string]json.RawMessage // set once a turn has joined it
	blocks []json.RawMessage          // its content then, the joined turns' after it
}

// join appends a user turn's content to this one's, a string content being
// one text block. It joins nothing, and reports false, when either content is
// neither a string nor an array, which the upstream answers as sent.
func (o *outMsg) join(raw json.RawMessage) bool {
	var next map[string]json.RawMessage
	if json.Unmarshal(raw, &next) != nil {
		return false
	}
	more, ok := contentBlocks(next["content"])
	if !ok {
		return false
	}
	if o.obj == nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal(o.raw, &obj) != nil {
			return false
		}
		blocks, ok := contentBlocks(obj["content"])
		if !ok {
			return false
		}
		o.obj, o.blocks = obj, blocks
	}
	o.blocks = append(o.blocks, more...)
	return true
}

func (o outMsg) encode() json.RawMessage {
	if o.obj == nil {
		return o.raw
	}
	o.obj["content"], _ = json.Marshal(o.blocks)
	out, _ := json.Marshal(o.obj)
	return out
}

// contentBlocks is a message's content as blocks, when it is a string or an
// array of blocks.
func contentBlocks(c json.RawMessage) ([]json.RawMessage, bool) {
	c = bytes.TrimSpace(c)
	var s string
	if bytes.HasPrefix(c, []byte(`"`)) && json.Unmarshal(c, &s) == nil {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
		return []json.RawMessage{b}, true
	}
	var bs []json.RawMessage
	if !bytes.HasPrefix(c, []byte("[")) || json.Unmarshal(c, &bs) != nil {
		return nil, false
	}
	return bs, true
}

// withString is a JSON object with one field set to a string.
func withString(raw json.RawMessage, field, value string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return raw
	}
	obj[field], _ = json.Marshal(value)
	out, _ := json.Marshal(obj)
	return out
}

// preferring moves the attempts at dep ahead of the others, each keeping its
// order: the newest block's producer is tried first, whatever its priority,
// since it is the one deployment sent the request's newest thinking.
func preferring(attempts []catalog.Attempt, dep string) []catalog.Attempt {
	out := make([]catalog.Attempt, 0, len(attempts))
	for _, at := range attempts {
		if at.Deployment.ID == dep {
			out = append(out, at)
		}
	}
	for _, at := range attempts {
		if at.Deployment.ID != dep {
			out = append(out, at)
		}
	}
	return out
}

// thinkingRefusal reports whether an upstream's 400 refuses the thinking the
// request carried, so that one more attempt without it may cure the request:
// its message names a thinking block's signature, a redacted_thinking block,
// a thinking block, or a thinking field of one of the request's blocks.
// Removing thinking cannot cure three refusals that do so, which are not:
// DeepSeek's "must be passed back" and Anthropic's "must start with a thinking
// block", which ask for thinking the request lacks, and one about a parameter
// that configures thinking (thinkingParams) rather than a block, which strip
// mode leaves as it is. Anthropic's "cannot be modified" is one: removing
// every block is valid where editing one is not. Only the message is read,
// since a vendor that echoes the request in its error would otherwise make any
// refusal of a request carrying thinking read as one.
func thinkingRefusal(body []byte) bool {
	msg := strings.ReplaceAll(strings.ToLower(errorMessage(body)), "`", "")
	switch {
	case strings.Contains(msg, "must be passed back"), strings.Contains(msg, "must start with a thinking block"):
		return false
	case strings.Contains(msg, "signature") && strings.Contains(msg, "thinking"):
		return true
	}
	for _, p := range thinkingParams {
		if strings.Contains(msg, p) {
			return false
		}
	}
	return strings.Contains(msg, "redacted_thinking") || strings.Contains(msg, "thinking block") || blockField.MatchString(msg)
}

// thinkingParams name what configures thinking, as Anthropic's configuration
// refusals do (platform.claude.com/docs/en/build-with-claude/thinking-troubleshooting,
// read 2026-10-05).
var thinkingParams = []string{"thinking.type", "between_tools", "budget_tokens", "output_config", "adaptive thinking"}

// blockField is a thinking field of one of the request's blocks, by its path.
var blockField = regexp.MustCompile(`content\.\d+\.(thinking|signature)\b`)

// errorMessage is an error body's message: error.message in Anthropic's
// envelope, else a top-level message; the text of a body that is not JSON at
// all; and "" for JSON that holds no message, whatever its shape — an error
// field that is a string included.
func errorMessage(body []byte) string {
	if !json.Valid(body) {
		return string(body)
	}
	var top, inner map[string]json.RawMessage
	var msg string
	_ = json.Unmarshal(body, &top)
	if json.Unmarshal(top["error"], &inner) == nil && json.Unmarshal(inner["message"], &msg) == nil && msg != "" {
		return msg
	}
	msg = ""
	_ = json.Unmarshal(top["message"], &msg)
	return msg
}

// wrapping marks one answer's thinking blocks with their producer, in block
// order, the first in a strip-mode answer with the reset mark. A signature
// covers the blocks ahead of it, so once a thinking block goes unwrapped —
// an unsigned one, or in a stream one still unsigned when the next thinking
// block starts, which Anthropic's sequential blocks make the same thing —
// none after it is wrapped either: sent back without it, they would be
// refused.
type wrapping struct {
	dep   string
	reset bool // the next value wrapped takes resetPrefix
	ended bool // a thinking block went unwrapped, and so does every one after it
	open  *int // the streamed thinking block whose signature has not begun
}

func newWrapping(dep string, strip bool) *wrapping {
	return &wrapping{dep: dep, reset: strip}
}

func (w *wrapping) wrap(v string) string {
	if w.reset {
		w.reset = false
		return resetPrefix + w.dep + "." + v
	}
	return wrapPrefix + w.dep + "." + v
}

// content wraps a whole answer's thinking values, or returns nil when there
// are none to wrap.
func (w *wrapping) content(raw json.RawMessage) json.RawMessage {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	changed := false
	for i, b := range blocks {
		field, v := provenanceOf(b)
		switch {
		case field == "" || w.ended:
		case v == "":
			w.ended = true
		default:
			blocks[i] = withString(b, field, w.wrap(v))
			changed = true
		}
	}
	if !changed {
		return nil
	}
	out, _ := json.Marshal(blocks)
	return out
}

// start wraps a streamed content_block_start's thinking value, or holds a
// thinking block whose signature is still empty open for its first fragment.
// A thinking block that starts while one is open, an empty redacted block,
// which no fragment fills, and an empty one that names no index, whose
// fragments cannot be told apart, end the wrapping. It returns nil when the
// event is unchanged.
func (w *wrapping) start(data []byte) []byte {
	var ev struct {
		Index        *int            `json:"index"`
		ContentBlock json.RawMessage `json:"content_block"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return nil
	}
	field, v := provenanceOf(ev.ContentBlock)
	switch {
	case field == "" || w.ended:
		return nil
	case w.open != nil:
		w.ended = true
		return nil
	case v == "":
		if field == "signature" && ev.Index != nil {
			w.open = ev.Index
		} else {
			w.ended = true
		}
		return nil
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(data, &obj)
	obj["content_block"] = withString(ev.ContentBlock, field, w.wrap(v))
	out, _ := json.Marshal(obj)
	return out
}

// delta wraps the open block's first non-empty signature fragment; the rest
// pass unchanged, so the fragments concatenated, as the SDKs assemble them,
// are the wrapped value. It returns nil when the event is unchanged.
func (w *wrapping) delta(data []byte) []byte {
	if w.open == nil || w.ended {
		return nil
	}
	var ev struct {
		Index *int `json:"index"`
		Delta struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Index == nil || *ev.Index != *w.open ||
		ev.Delta.Type != "signature_delta" || ev.Delta.Signature == "" {
		return nil
	}
	w.open = nil
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(data, &obj)
	obj["delta"] = withString(obj["delta"], "signature", w.wrap(ev.Delta.Signature))
	out, _ := json.Marshal(obj)
	return out
}
