package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Stream converts a Chat Completions stream to a Messages one, chunk by
// chunk, in the event flow the Messages API streams
// (platform.claude.com/docs/en/build-with-claude/streaming): message_start
// before anything else; each block's content_block_start, its deltas and
// its content_block_stop before the next block starts, indices counting up
// from zero, as an SDK accumulating the stream requires (checked against
// anthropic-sdk-go v1.70.1 — messageutil.go Message.Accumulate); a thinking
// block's signature_delta just before its stop; a tool_use block opened
// with an empty input, its arguments following as input_json_delta; then
// message_delta, with the stop reason and the usage, and message_stop.
// Chat Completions reports usage after the finish, if at all, so
// message_start reports zeros and message_delta every count, which the SDK
// takes over message_start's.
type Stream struct {
	alias, id string
	signature func() string
	usage     Usage

	started  bool
	finished bool   // the choice has finished, and not gone on
	next     int    // the next block's index
	open     string // the open block's type, "" for none
	call     int64  // the open tool_use block's call index
	callID   string // and the id and name it opened with
	callName string
	callArgs int            // its arguments' fragments so far
	callObj  bool           // and whether one was a JSON object, which no other may join
	calls    map[int64]bool // the call indices a tool_use block has opened for
	called   bool           // a tool call has been seen
	refused  bool           // a refusal has been seen
	stop     string         // the choice's finish_reason
}

// NewStream converts a stream for a caller that named alias as its model.
// id is the message's when the upstream's first chunk names none; signature
// signs each thinking block, as Answer's does.
func NewStream(alias, id string, signature func() string) *Stream {
	return &Stream{alias: alias, id: id, signature: signature}
}

// SetUsage records the usage a chunk reported, which message_delta reports.
func (s *Stream) SetUsage(u Usage) { s.usage = u }

// Finished reports whether the choice has finished, so a stream the
// upstream ends now has said all it will.
func (s *Stream) Finished() bool { return s.finished }

// Chunk is the events one chunk, a JSON object, becomes. It fails on a chunk
// it cannot carry: a second choice, the deprecated function_call, a fragment
// of a tool call whose block an interleaved call already ended, or a tool
// call's name or id arriving, or changing, once its block has opened —
// Chat Completions may stream a name in pieces, which openai-go joins, where
// Messages names the tool once, at content_block_start. An open call's name
// or id sent again unchanged is a repeat, and carries on.
func (s *Stream) Chunk(data []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("a chunk is not a JSON object")
	}
	var out bytes.Buffer
	s.begin(&out, obj)
	var choices []map[string]json.RawMessage
	if !null(obj["choices"]) && json.Unmarshal(obj["choices"], &choices) != nil {
		return nil, fmt.Errorf("a chunk's choices is not an array of objects")
	}
	for _, ch := range choices {
		var index int64
		if !null(ch["index"]) && (json.Unmarshal(ch["index"], &index) != nil || index != 0) {
			return nil, fmt.Errorf("the upstream answered a choice other than the first")
		}
		if err := s.choice(&out, ch); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

func (s *Stream) choice(out *bytes.Buffer, ch map[string]json.RawMessage) error {
	var delta map[string]json.RawMessage
	_ = json.Unmarshal(ch["delta"], &delta)
	if !null(delta["function_call"]) {
		return fmt.Errorf("the upstream streamed the deprecated function_call; it must stream tool_calls")
	}
	for _, key := range []string{"reasoning_content", "content", "refusal"} {
		if _, ok := text(delta, key); !ok && !null(delta[key]) {
			return fmt.Errorf("a chunk's %s is not a string", key)
		}
	}
	generated := false
	if r, _ := text(delta, "reasoning_content"); r != "" {
		s.into(out, "thinking", nil)
		s.delta(out, map[string]string{"type": "thinking_delta", "thinking": r})
		generated = true
	}
	for _, key := range []string{"content", "refusal"} {
		if t, _ := text(delta, key); t != "" {
			s.into(out, "text", nil)
			s.delta(out, map[string]string{"type": "text_delta", "text": t})
			generated, s.refused = true, s.refused || key == "refusal"
		}
	}
	var calls []map[string]json.RawMessage
	if !null(delta["tool_calls"]) && json.Unmarshal(delta["tool_calls"], &calls) != nil {
		return fmt.Errorf("a chunk's tool_calls is not an array of objects")
	}
	for _, c := range calls {
		var index int64
		if i := c["index"]; !null(i) && json.Unmarshal(i, &index) != nil {
			return fmt.Errorf("a tool call's index is not a whole number")
		}
		var fn map[string]json.RawMessage
		_ = json.Unmarshal(c["function"], &fn)
		args, object, err := Arguments(fn["arguments"])
		if err != nil {
			return fmt.Errorf("tool call %d: %w", index, err)
		}
		id, _ := text(c, "id")
		name, _ := text(fn, "name")
		switch {
		case s.open == "tool_use" && s.call == index:
			if id != "" && id != s.callID || name != "" && name != s.callName {
				return fmt.Errorf("the upstream streamed tool call %d's name or id after starting it", index)
			}
		case s.calls[index]:
			return fmt.Errorf("the upstream went back to tool call %d after starting another", index)
		default:
			s.into(out, "tool_use", map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
			s.call, s.callID, s.callName, s.callArgs, s.callObj, s.called = index, id, name, 0, false, true
			if s.calls == nil {
				s.calls = map[int64]bool{}
			}
			s.calls[index] = true
		}
		if args != "" {
			if s.callObj || object && s.callArgs > 0 {
				return fmt.Errorf("tool call %d's arguments came as a JSON object beside other fragments", index)
			}
			s.callArgs, s.callObj = s.callArgs+1, object
			s.delta(out, map[string]string{"type": "input_json_delta", "partial_json": args})
		}
		generated = true
	}
	if generated { // a choice that goes on after its finish has not finished
		s.finished = false
	}
	if finish, _ := text(ch, "finish_reason"); finish != "" {
		s.close(out)
		s.stop, s.finished = finish, true
	}
	return nil
}

// End is the events that end the message: message_start, if no chunk came
// to send it, the open block's end, message_delta and message_stop.
func (s *Stream) End() []byte {
	var out bytes.Buffer
	s.begin(&out, nil)
	s.close(&out)
	event(&out, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": StopReason(s.stop, s.called, s.refused), "stop_sequence": nil},
		"usage": s.usage,
	})
	event(&out, "message_stop", map[string]string{"type": "message_stop"})
	return out.Bytes()
}

// Ping is the event that holds a Messages stream open and says nothing.
func Ping() []byte {
	return []byte("event: ping\ndata: {\"type\": \"ping\"}\n\n")
}

func (s *Stream) begin(out *bytes.Buffer, chunk map[string]json.RawMessage) {
	if s.started {
		return
	}
	s.started = true
	if id, ok := text(chunk, "id"); ok && id != "" {
		s.id = id
	}
	event(out, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "model": s.alias, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": Usage{},
	}})
}

// into makes typ the open block, ending the one before unless it is that
// block already: a tool_use block is opened anew for each call (start, its
// content_block_start's block).
func (s *Stream) into(out *bytes.Buffer, typ string, start map[string]any) {
	if s.open == typ && typ != "tool_use" {
		return
	}
	s.close(out)
	switch typ {
	case "thinking":
		start = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	case "text":
		start = map[string]any{"type": "text", "text": ""}
	}
	s.open = typ
	event(out, "content_block_start", map[string]any{"type": "content_block_start", "index": s.next, "content_block": start})
}

func (s *Stream) delta(out *bytes.Buffer, d map[string]string) {
	event(out, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.next, "delta": d})
}

// close ends the open block, a thinking block with its signature first.
func (s *Stream) close(out *bytes.Buffer) {
	if s.open == "" {
		return
	}
	if s.open == "thinking" {
		s.delta(out, map[string]string{"type": "signature_delta", "signature": s.signature()})
	}
	event(out, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.next})
	s.open = ""
	s.next++
}

// event writes one server-sent event.
func event(out *bytes.Buffer, name string, data any) {
	out.WriteString("event: " + name + "\ndata: ")
	out.Write(encode(data))
	out.WriteString("\n\n")
}
