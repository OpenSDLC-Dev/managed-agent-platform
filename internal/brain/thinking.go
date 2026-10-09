package brain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// replayThinking is what replay needs to send a model's thinking back (#67):
// the session's stored blocks by thinking-event id, the model id the request
// goes to, which a block must have been produced by, and the route it goes
// over (provider.Descriptor.Route), which opens every block's prefix.
type replayThinking struct {
	model  string
	route  string
	blocks map[domain.ID]events.ThinkingBlock
}

// prefixChain hashes what a model reads ahead of a point in a request — the
// route the request goes over, the system prompt, the tool definitions, then
// every content block with its role — so a thinking block goes back only under
// the prefix it was produced under,
// the rule Anthropic's API enforces (docs/plan/60_thinking-replay.md decision
// 5). Blocks are hashed one at a time with their role, not message by message,
// so a run of same-role blocks hashes alike whether replay merged it into one
// message or the request it was produced in ended mid-run. Each block is
// hashed in its canonical form, the bytes json.Marshal gives it inside a
// message's content (compacted, HTML-escaped): that is the form both a request
// as sent and a request being built can produce. The route comes first because
// the adapter renders the request on its way out (flatten_search_results) and
// the endpoint decides whose signatures it can read: a request the brain builds
// alike can reach a model as another prefix over another route.
type prefixChain struct{ h hash.Hash }

func newPrefixChain(route, system string, tools []json.RawMessage) (*prefixChain, error) {
	c := &prefixChain{h: sha256.New()}
	c.write("route", []byte(route))
	c.write("system", []byte(system))
	for _, t := range tools {
		if err := c.add("tool", t); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// add hashes one block, read by role.
func (c *prefixChain) add(role string, block json.RawMessage) error {
	canonical, err := json.Marshal(block)
	if err != nil {
		return err
	}
	c.write(role, canonical)
	return nil
}

// write frames one item as its tag and its bytes, each length-prefixed, so no
// two different sequences of items hash alike.
func (c *prefixChain) write(tag string, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(tag)))
	c.h.Write(n[:])
	c.h.Write([]byte(tag))
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	c.h.Write(n[:])
	c.h.Write(b)
}

// sum is the digest of everything added so far; adding may continue after it.
func (c *prefixChain) sum() string { return hex.EncodeToString(c.h.Sum(nil)) }

// anyPrefixDigest is the digest a block is stored under in place of its
// prefix's when the endpoint said its thinking may go back under any prefix
// (provider.Chunk.ThinkingAnyPrefix): the model gateway's word for a vendor
// that checks none (#883, docs/plan/61_thinking-replay-via-gateway.md). It
// binds the block to its route alone; its "any:" head keeps it from ever
// equalling a chain's digest, which is bare hex.
func anyPrefixDigest(route string) string {
	c := &prefixChain{h: sha256.New()}
	c.write("route", []byte(route))
	return "any:" + c.sum()
}

// requestChain is the chain over a whole request as sent over route: the
// point the first block of its response is produced at.
func requestChain(route string, req provider.Request) (*prefixChain, error) {
	c, err := newPrefixChain(route, req.System, req.Tools)
	if err != nil {
		return nil, err
	}
	for _, m := range req.Messages {
		blocks, err := contentBlocks(m.Content)
		if err != nil {
			return nil, err
		}
		for _, b := range blocks {
			if err := c.add(m.Role, b); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// replayMessage is one message of a request being built, before its content is
// marshaled: thinking marks the blocks that came from stored thinking, which
// admitThinking keeps or removes once the whole request is known.
type replayMessage struct {
	role     string
	blocks   []json.RawMessage
	thinking map[int]events.ThinkingBlock
}

// admitThinking walks a built request in order and removes each stored
// thinking block whose model or prefix differs from the one it was produced
// under — its prefix unless it was stored under its route's anyPrefixDigest,
// whose producer checks none, and which goes back over that route under any
// prefix (#883). It runs last because the system prompt is only final once every
// system.message has been read, and a system prompt change moves every
// block's prefix. A kept block joins the prefix later blocks are checked
// against and a removed one does not, so a removal drops exactly the blocks
// produced before the change: each one produced since was produced without it.
func admitThinking(t replayThinking, system string, tools []json.RawMessage, msgs []replayMessage) error {
	if !slices.ContainsFunc(msgs, func(m replayMessage) bool { return len(m.thinking) > 0 }) {
		return nil // nothing to admit, so no prefix to hash
	}
	chain, err := newPrefixChain(t.route, system, tools)
	if err != nil {
		return err
	}
	anyPrefix := anyPrefixDigest(t.route)
	for mi := range msgs {
		m := &msgs[mi]
		if len(m.thinking) == 0 {
			for _, b := range m.blocks {
				if err := chain.add(m.role, b); err != nil {
					return err
				}
			}
			continue
		}
		kept := make([]json.RawMessage, 0, len(m.blocks))
		for i, b := range m.blocks {
			if tb, ok := m.thinking[i]; ok && (tb.Model != t.model || (tb.PrefixDigest != chain.sum() && tb.PrefixDigest != anyPrefix)) {
				continue
			}
			if err := chain.add(m.role, b); err != nil {
				return err
			}
			kept = append(kept, b)
		}
		m.blocks = kept
	}
	return nil
}

// urlMedia reports whether req shows the model an image or document fetched by
// URL, anywhere in its messages, tool results included. The bytes behind a URL
// can change while the request stays the same, and a block replayed over
// changed bytes is one under another prefix, so a response to such a request
// keeps no thinking. Only the stream needs to ask: a stored block's digest
// matches only a prefix identical to one that had no URL media.
func urlMedia(req provider.Request) (bool, error) {
	for _, m := range req.Messages {
		var v any
		if err := json.Unmarshal(m.Content, &v); err != nil {
			return false, err
		}
		if hasURLSource(v) {
			return true, nil
		}
	}
	return false, nil
}

func hasURLSource(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if t, _ := x["type"].(string); t == "image" || t == "document" {
			if src, ok := x["source"].(map[string]any); ok && src["type"] == "url" {
				return true
			}
		}
		for _, e := range x {
			if hasURLSource(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasURLSource(e) {
				return true
			}
		}
	}
	return false
}
