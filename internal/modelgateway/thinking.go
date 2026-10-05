package modelgateway

import (
	"bytes"
	"encoding/json"
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
// block returns to its producer under the prefix it was produced under.
//
// Strip mode is the backstop for an upstream that refuses the history's
// thinking anyway: the request goes once more with every thinking block
// removed, and the answer's first thinking block carries resetPrefix, so later
// requests drop every thinking block ahead of it, as it was produced without
// them.
const (
	wrapPrefix  = "mapgw1."
	resetPrefix = "mapgw1r."
)

// unwrap reads a wrapped value: its producer, the upstream's own value, and
// whether it carries the reset mark. Deployment ids hold no dot.
func unwrap(v string) (dep, value string, reset, ok bool) {
	rest, reset := strings.CutPrefix(v, resetPrefix)
	if !reset {
		if rest, ok = strings.CutPrefix(v, wrapPrefix); !ok {
			return "", "", false, false
		}
	}
	dep, value, ok = strings.Cut(rest, ".")
	return dep, value, reset, ok && dep != ""
}

// thinkingField names the field that carries a block's provenance, or "" for
// a block that is not thinking.
func thinkingField(typ string) string {
	switch typ {
	case "thinking":
		return "signature"
	case "redacted_thinking":
		return "data"
	}
	return ""
}

// history is a request's messages as provenance reads them. A message holding
// no thinking block is never decoded past its role and goes upstream as the
// caller sent it.
type history struct{ msgs []histMsg }

type histMsg struct {
	raw    json.RawMessage
	role   string
	obj    map[string]json.RawMessage // set with blocks
	blocks []histBlock                // set only for a message with a thinking block
}

type histBlock struct {
	raw   json.RawMessage
	field string // thinkingField of its type; "" for any other block
	dep   string // its producer; "" when it carries no wrapper
	value string // the upstream's value under the wrapper
	reset bool   // it carries the reset mark
	// stale is a thinking block ahead of the newest reset mark, which no
	// deployment is sent.
	stale bool
}

// parseHistory reads a request's messages. It returns nil when provenance has
// nothing to do — no thinking block anywhere — and when the messages are not
// an array of objects, which the upstream answers itself.
func parseHistory(raw json.RawMessage) *history {
	if !bytes.Contains(raw, []byte("thinking")) {
		return nil
	}
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil
	}
	h := &history{msgs: make([]histMsg, len(msgs))}
	found := false
	for i, m := range msgs {
		var head struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &head) != nil {
			return nil
		}
		h.msgs[i] = histMsg{raw: m, role: head.Role}
		var content []json.RawMessage
		if json.Unmarshal(head.Content, &content) != nil {
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
		_ = json.Unmarshal(m, &h.msgs[i].obj)
		h.msgs[i].blocks = blocks
	}
	if !found {
		return nil
	}
	h.markStale()
	return h
}

func parseBlock(b json.RawMessage) histBlock {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(b, &obj)
	var typ, v string
	_ = json.Unmarshal(obj["type"], &typ)
	blk := histBlock{raw: b, field: thinkingField(typ)}
	if blk.field != "" && json.Unmarshal(obj[blk.field], &v) == nil {
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
	var out []histMsg
	emptied := false
	for _, m := range h.msgs {
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
			m.raw, _ = json.Marshal(obj)
		}
		if n := len(out); emptied && n > 0 && m.role == "user" && out[n-1].role == "user" {
			out[n-1].raw = joined(out[n-1].raw, m.raw)
		} else {
			out = append(out, m)
		}
		emptied = false
	}
	raws := make([]json.RawMessage, len(out))
	for i, m := range out {
		raws[i] = m.raw
	}
	b, _ := json.Marshal(raws)
	return b
}

// joined is two user turns as one: the first's fields, with both contents in
// order, a string content being one text block.
func joined(a, b json.RawMessage) json.RawMessage {
	var ao, bo map[string]json.RawMessage
	_ = json.Unmarshal(a, &ao)
	_ = json.Unmarshal(b, &bo)
	ao["content"], _ = json.Marshal(append(contentBlocks(ao["content"]), contentBlocks(bo["content"])...))
	out, _ := json.Marshal(ao)
	return out
}

func contentBlocks(c json.RawMessage) []json.RawMessage {
	var s string
	if bytes.HasPrefix(bytes.TrimSpace(c), []byte(`"`)) && json.Unmarshal(c, &s) == nil {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
		return []json.RawMessage{b}
	}
	var bs []json.RawMessage
	_ = json.Unmarshal(c, &bs)
	return bs
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

// thinkingRefusal reports whether an upstream's 400 refuses the history's
// thinking, so that one more attempt without it may cure the request: its
// message names a signature, a redacted_thinking block, or a thinking block
// (by that word or by its place in messages). A refusal naming only the
// thinking parameter — its type, its budget — names no block, and DeepSeek's
// "must be passed back" asks for thinking the request lacks, so removing
// thinking cures neither. Anthropic's "cannot be modified" is one: removing
// every block is valid where editing one is not. Only the message is read: a
// vendor that echoes the request in its error would otherwise make any
// refusal of a request carrying thinking read as one.
func thinkingRefusal(body []byte) bool {
	msg := strings.ToLower(errorMessage(body))
	switch {
	case strings.Contains(msg, "must be passed back"):
		return false
	case strings.Contains(msg, "signature"), strings.Contains(msg, "redacted_thinking"):
		return true
	}
	return strings.Contains(msg, "thinking") && (strings.Contains(msg, "block") || strings.Contains(msg, "messages."))
}

// errorMessage is an error body's message: error.message in Anthropic's
// envelope, a top-level message in others, and otherwise the body itself.
func errorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	switch {
	case json.Unmarshal(body, &e) != nil:
	case e.Error.Message != "":
		return e.Error.Message
	case e.Message != "":
		return e.Message
	}
	return string(body)
}

// wrapping marks one answer's thinking blocks with their producer, the first
// of a strip-mode answer with the reset mark.
type wrapping struct {
	dep   string
	reset bool           // the next thinking block takes resetPrefix
	open  map[int]string // a streamed thinking block whose signature has not begun: its prefix, by index
}

func newWrapping(dep string, strip bool) *wrapping {
	return &wrapping{dep: dep, reset: strip, open: map[int]string{}}
}

// prefix is the wrapper the next thinking block takes.
func (w *wrapping) prefix() string {
	if w.reset {
		w.reset = false
		return resetPrefix + w.dep + "."
	}
	return wrapPrefix + w.dep + "."
}

// content wraps a whole answer's thinking blocks — an empty or absent value
// too, so that every block returns with its producer — or returns nil when
// there are none.
func (w *wrapping) content(raw json.RawMessage) json.RawMessage {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	changed := false
	for i, b := range blocks {
		var obj map[string]json.RawMessage
		var typ, v string
		_ = json.Unmarshal(b, &obj)
		_ = json.Unmarshal(obj["type"], &typ)
		if field := thinkingField(typ); field != "" {
			_ = json.Unmarshal(obj[field], &v)
			blocks[i] = withString(b, field, w.prefix()+v)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	out, _ := json.Marshal(blocks)
	return out
}

// start wraps a streamed content_block_start's value, or holds a thinking
// block whose signature is still empty open for its first fragment. It
// returns nil when the event is unchanged.
func (w *wrapping) start(data []byte) []byte {
	var ev map[string]json.RawMessage
	var index int
	var block map[string]json.RawMessage
	if json.Unmarshal(data, &ev) != nil || json.Unmarshal(ev["index"], &index) != nil ||
		json.Unmarshal(ev["content_block"], &block) != nil {
		return nil
	}
	var typ, v string
	_ = json.Unmarshal(block["type"], &typ)
	field := thinkingField(typ)
	if field == "" {
		return nil
	}
	prefix := w.prefix()
	_ = json.Unmarshal(block[field], &v)
	if v == "" && field == "signature" {
		w.open[index] = prefix
		return nil
	}
	ev["content_block"] = withString(ev["content_block"], field, prefix+v)
	out, _ := json.Marshal(ev)
	return out
}

// delta wraps an open block's first non-empty signature fragment; the rest
// pass unchanged, so the fragments concatenated, as the SDKs assemble them,
// are the wrapped value. It returns nil when the event is unchanged.
func (w *wrapping) delta(data []byte) []byte {
	if len(w.open) == 0 || !bytes.Contains(data, []byte("signature_delta")) {
		return nil
	}
	var ev struct {
		Index int `json:"index"`
		Delta struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Delta.Type != "signature_delta" || ev.Delta.Signature == "" {
		return nil
	}
	prefix, ok := w.open[ev.Index]
	if !ok {
		return nil
	}
	delete(w.open, ev.Index)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(data, &obj)
	obj["delta"] = withString(obj["delta"], "signature", prefix+ev.Delta.Signature)
	out, _ := json.Marshal(obj)
	return out
}

// stop is the event that wraps a thinking block ending with no signature,
// sent ahead of its content_block_stop, or nil: an unsigned block still
// returns with its producer, as it does from a whole answer.
func (w *wrapping) stop(data []byte) []byte {
	var ev struct {
		Index *int `json:"index"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Index == nil {
		return nil
	}
	prefix, ok := w.open[*ev.Index]
	if !ok {
		return nil
	}
	delete(w.open, *ev.Index)
	d, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": *ev.Index,
		"delta": map[string]string{"type": "signature_delta", "signature": prefix}})
	return []byte("event: content_block_delta\ndata: " + string(d) + "\n\n")
}
