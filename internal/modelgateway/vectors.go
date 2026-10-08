package modelgateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// relayVectors relays an embeddings or rerank answer as it arrives, its
// top-level model the name the caller sent and every other byte the
// upstream's, its usage read on the way (answerRewriter). Such an answer runs
// to tens of megabytes of vectors, which are neither held nor decoded, so no
// bound applies to it but the stall guard's. Nothing is written until its
// first bytes arrive, so an upstream that answers nothing is retried as any
// is. One that breaks off after them leaves the caller a cut-off body, the
// only way left to say so, and is recorded as the failure it was; one whose
// caller has gone is still read to its end, its usage counted.
func relayVectors(w http.ResponseWriter, resp *http.Response, c call, guard *provider.StallGuard, red provider.Redactor) (*failure, bool) {
	buf := make([]byte, 32<<10)
	n, err := io.ReadAtLeast(resp.Body, buf, 1)
	if n == 0 && !errors.Is(err, io.EOF) {
		return noAnswer(guard, red, err)
	}
	c.out.status = resp.StatusCode
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	rw := &answerRewriter{alias: encodeJSON(c.alias)}
	cw := newCallerWriter(w, c.out.at, guard, false)
	send := func(b []byte) {
		if out := rw.rewrite(b); len(out) > 0 {
			cw.write(out)
		}
	}
	send(buf[:n])
	for err == nil {
		n, err = resp.Body.Read(buf)
		send(buf[:n])
	}
	c.out.tokens = vectorUsageOf(rw.usage)
	if !errors.Is(err, io.EOF) {
		c.out.errType = "api_error"
		if errors.Is(guard.Cause(err), provider.ErrStalled) {
			c.out.errType = "timeout_error"
		}
	}
	return nil, false
}

// vectorUsageOf reads an embeddings or rerank usage object by its exact
// keys: prompt_tokens, which OpenAI's embeddings report, or else
// total_tokens, a call whose tokens are all input reporting only that. A
// usage holding neither, null, or a count countOf would not read, is nil —
// Gitee's rerank reports zeros under camelCase keys, which are not read
// (probed 2026-10-08).
func vectorUsageOf(raw json.RawMessage) *store.Tokens {
	var u map[string]json.RawMessage
	if json.Unmarshal(raw, &u) != nil || u == nil {
		return nil
	}
	n, ok := countOf(u["prompt_tokens"])
	if !ok {
		n, ok = countOf(u["total_tokens"])
	}
	if !ok {
		return nil
	}
	return &store.Tokens{Input: n}
}

// The rewriter's bounds: a key longer than maxKey is no key it looks for, and
// a usage longer than maxUsage is not kept.
const (
	maxKey   = 256
	maxUsage = 64 << 10
)

// The rewriter's states.
const (
	rwStart   = iota // before the answer's first byte that is not whitespace
	rwPass           // passing what is left as it is
	rwKey            // in the top-level object, before a key
	rwInKey          // in a top-level key
	rwColon          // after a top-level key
	rwValue          // after a top-level key's colon
	rwInValue        // in a top-level value
	rwAfter          // after a top-level value
)

// answerRewriter rewrites a JSON answer fed to it piece by piece, as it
// arrives: the value of each top-level member whose key, unescaped, is model
// becomes alias, whatever the value holds, and the last top-level usage value
// is kept, without the whitespace between its tokens, up to maxUsage bytes;
// every other byte passes as it came. It reads the answer only as far as
// telling strings, nesting and top-level members apart, so an answer that is
// not an object, or is malformed, passes as it came from the point it stops
// making sense — but for a model value, replaced however malformed.
type answerRewriter struct {
	alias []byte          // the alias, encoded as a JSON string
	usage json.RawMessage // the last top-level usage value read whole

	state    int
	inStr    bool
	esc      bool
	nest     int    // the nesting within the top-level value being read
	key      []byte // the top-level key being read, quotes included
	member   string // the key of the top-level value being read
	capture  []byte // the usage value being read
	overflow bool   // whether the key or the usage being read outgrew its bound
}

// rewrite is in as the caller gets it.
func (r *answerRewriter) rewrite(in []byte) []byte {
	out := make([]byte, 0, len(in)+len(r.alias))
	for i := 0; i < len(in); i++ {
		b := in[i]
		switch r.state {
		case rwPass:
			return append(out, in[i:]...)
		case rwStart:
			out = append(out, b)
			switch {
			case b == '{':
				r.state = rwKey
			case !isSpace(b):
				r.state = rwPass
			}
		case rwKey:
			out = append(out, b)
			switch {
			case b == '"':
				r.state, r.key, r.overflow, r.esc = rwInKey, append(r.key[:0], b), false, false
			case b == '}' || !isSpace(b):
				r.state = rwPass
			}
		case rwInKey:
			out = append(out, b)
			if len(r.key) < maxKey {
				r.key = append(r.key, b)
			} else {
				r.overflow = true
			}
			switch {
			case r.esc:
				r.esc = false
			case b == '\\':
				r.esc = true
			case b == '"':
				r.member = ""
				var k string
				if !r.overflow && json.Unmarshal(r.key, &k) == nil {
					r.member = k
				}
				r.state = rwColon
			}
		case rwColon:
			out = append(out, b)
			switch {
			case b == ':':
				r.state = rwValue
			case !isSpace(b):
				r.state = rwPass
			}
		case rwValue:
			if isSpace(b) {
				out = append(out, b)
				continue
			}
			r.state, r.nest, r.inStr, r.esc = rwInValue, 0, false, false
			switch r.member {
			case "model":
				out = append(out, r.alias...)
			case "usage":
				r.capture, r.overflow = r.capture[:0], false
			}
			i-- // the value's first byte
		case rwInValue:
			part, ended := r.valueByte(b)
			if part {
				switch r.member {
				case "model":
				case "usage":
					switch {
					case !r.inStr && isSpace(b): // between tokens, so no part of the usage
					case len(r.capture) < maxUsage:
						r.capture = append(r.capture, b)
					default:
						r.overflow = true
					}
					out = append(out, b)
				default:
					out = append(out, b)
				}
			}
			if ended {
				if r.member == "usage" {
					r.usage = nil
					if !r.overflow {
						r.usage = append(json.RawMessage(nil), r.capture...)
					}
				}
				r.state = rwAfter
				if !part {
					i-- // the byte after the value
				}
			}
		case rwAfter:
			out = append(out, b)
			switch {
			case b == ',':
				r.state = rwKey
			case !isSpace(b):
				r.state = rwPass // the object's close, or what makes no sense
			}
		}
	}
	return out
}

// valueByte reads b as the next byte of a top-level value: whether it is part
// of the value, and whether the value has ended — with b, or, for a number,
// a literal or anything else not quoted or nested, before it.
func (r *answerRewriter) valueByte(b byte) (part, ended bool) {
	if r.inStr {
		switch {
		case r.esc:
			r.esc = false
		case b == '\\':
			r.esc = true
		case b == '"':
			r.inStr = false
			return true, r.nest == 0
		}
		return true, false
	}
	switch {
	case b == '"':
		r.inStr = true
	case b == '{' || b == '[':
		r.nest++
	case b == '}' || b == ']':
		if r.nest == 0 {
			return false, true
		}
		r.nest--
		return true, r.nest == 0
	case b == ',' || isSpace(b):
		if r.nest == 0 {
			return false, true
		}
	}
	return true, false
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
