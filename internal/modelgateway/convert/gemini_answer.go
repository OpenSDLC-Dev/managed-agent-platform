package convert

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// GeminiAnswer converts a whole generateContent answer to a Messages one, for
// a caller that named alias as its model. Its content is the first
// candidate's parts, in order: text as text blocks, adjacent ones joined and
// empty ones dropped, and each functionCall as a tool_use block under its id,
// or under one of geminiCallID's where Gemini names none. Ahead of them, when
// the answer has thought summaries or a signed call, goes one thinking block
// (docs/plan/62_gemini-upstream-protocol.md, decision 6): the summaries as
// its text, and as its signature sign applied to GeminiSignaturePrefix and the
// first call's signature — the one Gemini checks when the call comes back —
// which GeminiRequest sets on that call again. A text part's signature is not
// carried. The stop reason is geminiStop's; usage is the upstream's, as the
// caller reads it, nil reporting zeros; the id is the upstream's responseId,
// or id when it names none.
//
// A prompt Gemini blocked, answered with no candidate and a blockReason, is
// an empty refusal. Otherwise it fails on an answer it cannot carry: no
// candidate, a finish reason geminiStop has none for, a part carrying
// content Messages has no block for (inline data, a file, code or its
// result), a functionCall with no name or with args that are no JSON object,
// or a text that is not a string. Every field is read by its exact key.
func GeminiAnswer(b []byte, alias, id string, usage *Usage, sign func(value string) string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("the answer is not a JSON object")
	}
	if rid, ok := text(obj, "responseId"); ok && rid != "" {
		id = rid
	}
	if usage == nil {
		usage = &Usage{}
	}
	message := func(content []any, stop string) []byte {
		return encode(map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": alias, "content": content,
			"stop_reason": stop, "stop_sequence": nil, "usage": usage,
		})
	}
	var cands []map[string]json.RawMessage
	if !null(obj["candidates"]) && json.Unmarshal(obj["candidates"], &cands) != nil {
		return nil, fmt.Errorf("the answer's candidates is not an array of objects")
	}
	if len(cands) == 0 {
		var feedback map[string]json.RawMessage
		_ = json.Unmarshal(obj["promptFeedback"], &feedback)
		if reason, _ := text(feedback, "blockReason"); reason != "" {
			return message([]any{}, "refusal"), nil
		}
		return nil, fmt.Errorf("the answer has no candidate")
	}
	var content map[string]json.RawMessage
	_ = json.Unmarshal(cands[0]["content"], &content)
	var parts []map[string]json.RawMessage
	if !null(content["parts"]) && json.Unmarshal(content["parts"], &parts) != nil {
		return nil, fmt.Errorf("the answer's parts is not an array of objects")
	}
	var (
		blocks    []any
		summaries []string
		signature string
		calls     int
		joined    *strings.Builder // the text block the next text part joins, if the last block is one
	)
	for i, p := range parts {
		for _, k := range []string{"inlineData", "fileData", "executableCode", "codeExecutionResult"} {
			if !null(p[k]) {
				return nil, fmt.Errorf("the answer's part %d: %s has no Messages counterpart", i, k)
			}
		}
		if !null(p["functionCall"]) {
			call, err := geminiCallOf(p["functionCall"], id, calls)
			if err != nil {
				return nil, fmt.Errorf("the answer's part %d: %w", i, err)
			}
			if calls == 0 {
				signature, _ = text(p, "thoughtSignature")
			}
			calls++
			blocks, joined = append(blocks, call), nil
			continue
		}
		t, ok := text(p, "text")
		if !ok && !null(p["text"]) {
			return nil, fmt.Errorf("the answer's part %d: text is not a string", i)
		}
		switch {
		case isTrue(p["thought"]):
			summaries = append(summaries, t)
		case t == "":
		case joined != nil:
			joined.WriteString(t)
		default:
			joined = &strings.Builder{}
			joined.WriteString(t)
			blocks = append(blocks, joined)
		}
	}
	reason, _ := text(cands[0], "finishReason")
	stop, err := geminiStop(reason, calls > 0)
	if err != nil {
		if msg, _ := text(cands[0], "finishMessage"); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	out := make([]any, 0, len(blocks)+1)
	if len(summaries) > 0 || signature != "" {
		out = append(out, map[string]string{"type": "thinking", "thinking": strings.Join(summaries, ""),
			"signature": sign(GeminiSignaturePrefix + signature)})
	}
	for _, blk := range blocks {
		if t, ok := blk.(*strings.Builder); ok {
			blk = map[string]string{"type": "text", "text": t.String()}
		}
		out = append(out, blk)
	}
	return message(out, stop), nil
}

// geminiCallOf is a functionCall as a tool_use block, the n-th call of the
// answer answerID names.
func geminiCallOf(raw json.RawMessage, answerID string, n int) (map[string]any, error) {
	var fc map[string]json.RawMessage
	if json.Unmarshal(raw, &fc) != nil {
		return nil, fmt.Errorf("functionCall is not an object")
	}
	name, _ := text(fc, "name")
	if name == "" {
		return nil, fmt.Errorf("functionCall has no name")
	}
	input := json.RawMessage("{}")
	if !null(fc["args"]) {
		var args map[string]json.RawMessage
		if json.Unmarshal(fc["args"], &args) != nil {
			return nil, fmt.Errorf("functionCall args is not a JSON object")
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, fc["args"]); err != nil {
			return nil, err
		}
		input = buf.Bytes()
	}
	callID, _ := text(fc, "id")
	if callID == "" {
		callID = geminiCallID(answerID, n)
	}
	return map[string]any{"type": "tool_use", "id": callID, "name": name, "input": input}, nil
}

// geminiCallID is the id the n-th call of an answer gets where Gemini names
// none: derived from the answer's id, so the same answer converts the same
// way, and no two answers' calls share one.
func geminiCallID(answerID string, n int) string {
	sum := sha256.Sum256([]byte(answerID + "\x00" + strconv.Itoa(n)))
	return "toolu_" + hex.EncodeToString(sum[:12])
}

// geminiStop is a finishReason as a Messages stop_reason
// (google.golang.org/genai 1.73.0's FinishReason, and bifrost's two more).
// STOP, a stop sequence's included, is tool_use for an answer that called a
// tool, as Gemini ends a tool turn so, and otherwise end_turn, as is an
// answer naming no reason; MAX_TOKENS is
// max_tokens, and so is CONTINUATION, an answer the server's own limit cut
// short, which the caller may ask to continue. Every reason a safety or
// policy check stopped the answer for is refusal. The rest — a malformed or
// unexpected call, too many calls, a missing signature, a malformed answer,
// OTHER, and any reason Gemini adds later — have no stop_reason a caller
// could act on, and fail, naming the reason.
func geminiStop(reason string, called bool) (string, error) {
	switch reason {
	case "", "STOP":
		if called {
			return "tool_use", nil
		}
		return "end_turn", nil
	case "MAX_TOKENS", "CONTINUATION":
		return "max_tokens", nil
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "IMAGE_OTHER":
		return "refusal", nil
	}
	return "", fmt.Errorf("the answer finished %s", reason)
}

// isTrue reports whether v is JSON true.
func isTrue(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("true")) }
