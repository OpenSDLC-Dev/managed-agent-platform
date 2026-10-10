package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// GeminiStream converts a streamGenerateContent stream (alt=sse), chunk by
// chunk, each a GenerateContentResponse, to a Messages one, in Stream's
// event flow (docs/plan/62_gemini-upstream-protocol.md, decision 9). Each
// chunk's first candidate's parts become events as GeminiAnswer reads them:
// thought summaries as thinking_delta, text as text_delta, empty text
// dropped, and each functionCall, which Gemini streams whole, as one tool_use
// block with a single input_json_delta.
//
// The leading thinking block opens on the first thought, or on a signed
// call that comes before any text or call, and closes at the first text,
// call or finish: its signature, sign applied to GeminiSignaturePrefix and the first
// call's signature when that call is what closes it, is its signature_delta,
// so the stream accumulates to the message GeminiAnswer makes of the whole
// answer. What comes after the block closed cannot reach it: a later
// thought is dropped, and so is the signature of a call that follows text,
// for which GeminiRequest sends the sentinel on replay.
//
// A candidate's finishReason finishes the stream, until a chunk carries
// more of the answer, and End makes message_delta of it and of the usage SetUsage recorded, which Gemini
// reports in full on its last chunk alone; Gemini sends no [DONE], so a
// stream ends when its upstream closes it. A prompt blocked, with no
// candidate, before any answer finishes it as an empty refusal.
type GeminiStream struct {
	s        Stream
	leading  bool   // the leading thinking block may still open
	callSig  string // the signature of the call that closed it
	calls    int
	reason   string // the finish reason
	blocked  bool
	answered bool // a chunk has carried some of the answer
}

// NewGeminiStream converts a stream for a caller that named alias as its
// model; id is the message's when the upstream names no responseId, and sign
// wraps the leading thinking block's signature, as GeminiAnswer's does.
func NewGeminiStream(alias, id string, sign func(value string) string) *GeminiStream {
	g := &GeminiStream{s: Stream{alias: alias, id: id}, leading: true}
	g.s.signature = func() string { return sign(GeminiSignaturePrefix + g.callSig) }
	return g
}

// SetUsage records the usage a chunk reported, which message_delta reports.
func (g *GeminiStream) SetUsage(u Usage) { g.s.SetUsage(u) }

// Finished reports whether the stream has said all it will.
func (g *GeminiStream) Finished() bool { return g.s.finished }

// Chunk is the events one chunk, a JSON object, becomes. It fails on a chunk
// GeminiAnswer would fail on as a whole answer.
func (g *GeminiStream) Chunk(data []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("a chunk is not a JSON object")
	}
	var out bytes.Buffer
	g.s.begin(&out, map[string]json.RawMessage{"id": obj["responseId"]})
	var cands []map[string]json.RawMessage
	if !null(obj["candidates"]) && json.Unmarshal(obj["candidates"], &cands) != nil {
		return nil, fmt.Errorf("a chunk's candidates is not an array of objects")
	}
	if len(cands) == 0 {
		var feedback map[string]json.RawMessage
		_ = json.Unmarshal(obj["promptFeedback"], &feedback)
		if reason, _ := text(feedback, "blockReason"); reason != "" && !g.answered {
			g.blocked, g.s.finished = true, true
		}
		return out.Bytes(), nil
	}
	var content map[string]json.RawMessage
	_ = json.Unmarshal(cands[0]["content"], &content)
	var parts []map[string]json.RawMessage
	if !null(content["parts"]) && json.Unmarshal(content["parts"], &parts) != nil {
		return nil, fmt.Errorf("a chunk's parts is not an array of objects")
	}
	before := out.Len()
	for i, p := range parts {
		if err := g.part(&out, p); err != nil {
			return nil, fmt.Errorf("a chunk's part %d: %w", i, err)
		}
	}
	if out.Len() > before { // an answer, or more of one after its finish or a block, has not finished
		g.answered, g.blocked, g.s.finished = true, false, false
	}
	if reason, _ := text(cands[0], "finishReason"); reason != "" {
		if _, err := geminiStop(reason, g.calls > 0); err != nil {
			if msg, _ := text(cands[0], "finishMessage"); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
			return nil, err
		}
		g.s.close(&out)
		g.reason, g.s.finished, g.leading = reason, true, false
	}
	return out.Bytes(), nil
}

func (g *GeminiStream) part(out *bytes.Buffer, p map[string]json.RawMessage) error {
	if err := geminiUnsupported(p); err != nil {
		return err
	}
	if !null(p["functionCall"]) {
		call, err := geminiCallOf(p["functionCall"], g.s.id, g.calls)
		if err != nil {
			return err
		}
		if g.leading {
			if sig, _ := text(p, "thoughtSignature"); sig != "" {
				g.callSig = sig
				g.s.into(out, "thinking", nil)
			}
			g.leading = false
		}
		g.calls++
		g.s.into(out, "tool_use", map[string]any{"type": "tool_use", "id": call["id"], "name": call["name"], "input": map[string]any{}})
		if input := call["input"].(json.RawMessage); string(input) != "{}" {
			g.s.delta(out, map[string]string{"type": "input_json_delta", "partial_json": string(input)})
		}
		return nil
	}
	t, ok := text(p, "text")
	if !ok && !null(p["text"]) {
		return fmt.Errorf("text is not a string")
	}
	switch {
	case t == "":
	case isTrue(p["thought"]):
		if g.leading {
			g.s.into(out, "thinking", nil)
			g.s.delta(out, map[string]string{"type": "thinking_delta", "thinking": t})
		}
	default:
		g.leading = false
		g.s.into(out, "text", nil)
		g.s.delta(out, map[string]string{"type": "text_delta", "text": t})
	}
	return nil
}

// End is the events that end the message: message_start, if no chunk came
// to send it, the open block's end, message_delta and message_stop.
func (g *GeminiStream) End() []byte {
	var out bytes.Buffer
	g.s.begin(&out, nil)
	g.s.close(&out)
	stop := "refusal"
	if !g.blocked {
		stop, _ = geminiStop(g.reason, g.calls > 0)
	}
	event(&out, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": g.s.usage,
	})
	event(&out, "message_stop", map[string]string{"type": "message_stop"})
	return out.Bytes()
}
