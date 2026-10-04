package brain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// replayThinking is what replay needs to send a model's thinking back (#67):
// the session's stored blocks by thinking-event id, and the model id the
// request goes to, which a block must have been produced by.
type replayThinking struct {
	model  string
	blocks map[domain.ID]events.ThinkingBlock
}

// prefixChain hashes what a model reads ahead of a point in a request — the
// system prompt, the tool definitions, then every content block with its role
// — so a thinking block goes back only under the prefix it was produced under,
// the rule Anthropic's API enforces (docs/plan/60_thinking-replay.md decision
// 5). Blocks are hashed one at a time with their role, not message by message,
// so a run of same-role blocks hashes alike whether replay merged it into one
// message or the request it was produced in ended mid-run. Each block is
// hashed in its canonical form, the bytes json.Marshal gives it inside a
// message's content (compacted, HTML-escaped): that is the form both a request
// as sent and a request being built can produce.
type prefixChain struct{ h hash.Hash }

func newPrefixChain(system string, tools []json.RawMessage) (*prefixChain, error) {
	c := &prefixChain{h: sha256.New()}
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

// requestChain is the chain over a whole request as sent: the point the first
// block of its response is produced at.
func requestChain(req provider.Request) (*prefixChain, error) {
	c, err := newPrefixChain(req.System, req.Tools)
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
// under. It runs last because the system prompt is only final once every
// system.message has been read, and a system prompt change moves every
// block's prefix. A kept block joins the prefix later blocks are checked
// against and a removed one does not, so a removal drops exactly the blocks
// produced before the change: each one produced since was produced without it.
func admitThinking(system string, tools []json.RawMessage, model string, msgs []replayMessage) error {
	chain, err := newPrefixChain(system, tools)
	if err != nil {
		return err
	}
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
			if tb, ok := m.thinking[i]; ok && (tb.Model != model || tb.PrefixDigest != chain.sum()) {
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
