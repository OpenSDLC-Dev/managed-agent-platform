package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesStream converts a Messages stream to a Responses one, event by
// event, in the order the Responses API streams (OpenAI's streaming guide,
// and github.com/openai/openai-go/v3 v3.73.0's ResponseStreamEventUnion
// for each event's fields): response.created and response.in_progress
// first; each block an output item, opened by response.output_item.added
// and closed by response.output_item.done before the next opens — text
// with its output_text part's added, deltas, done and part done between,
// thinking with its one summary part's, tool_use with its arguments'
// deltas and done — and response.completed or response.incomplete last,
// carrying the whole Response. Every event carries the next sequence_number from zero, and
// each is framed with an event line naming its type. No data carries a
// top-level error key, which openai-go reads as the stream failing: a
// failure is response.failed, its error inside the response.
type ResponsesStream struct {
	m      ResponseMeta
	seq    int64
	output []any           // each block's item, nil while it is open
	open   map[int64]*item // the open blocks, by their Messages index
	usage  map[string]int64
	stop   string
	begun  bool
	done   bool
}

// item is an open block.
type item struct {
	typ   string // the Messages block's
	n     int    // its output index
	id    string
	text  strings.Builder // text or thinking
	sig   strings.Builder // a thinking block's signature, a redacted one's data
	args  strings.Builder // a tool_use block's arguments
	input string          // and the input it opened with
	call  string          // its call_id
	name  string
}

// NewResponsesStream converts a stream whose Response m describes.
func NewResponsesStream(m ResponseMeta) *ResponsesStream {
	return &ResponsesStream{m: m, open: map[int64]*item{}, usage: map[string]int64{}}
}

// Done reports whether the stream has sent its last event.
func (s *ResponsesStream) Done() bool { return s.done }

// Event is the events one Messages event becomes, none once the stream is
// done. It fails on an event it cannot carry: a block with no Responses
// counterpart, which only a server tool, never asked for, makes, or a delta
// or stop for a block not open. An error event ends the stream as
// response.failed (Failure).
func (s *ResponsesStream) Event(name string, data []byte) ([]byte, error) {
	if s.done {
		return nil, nil
	}
	var out bytes.Buffer
	var obj map[string]json.RawMessage
	if len(data) > 0 && (json.Unmarshal(data, &obj) != nil || obj == nil) {
		return nil, fmt.Errorf("a %s event's data is not a JSON object", name)
	}
	switch name {
	case "message_start":
		var msg map[string]json.RawMessage
		_ = json.Unmarshal(obj["message"], &msg)
		usageCounts(s.usage, msg["usage"])
		s.begin(&out)
	case "content_block_start":
		s.begin(&out)
		if err := s.start(&out, obj); err != nil {
			return nil, err
		}
	case "content_block_delta":
		if err := s.delta(&out, obj); err != nil {
			return nil, err
		}
	case "content_block_stop":
		if err := s.finish(&out, obj); err != nil {
			return nil, err
		}
	case "message_delta":
		var d map[string]json.RawMessage
		_ = json.Unmarshal(obj["delta"], &d)
		if stop, ok := text(d, "stop_reason"); ok {
			s.stop = stop
		}
		usageCounts(s.usage, obj["usage"])
	case "message_stop":
		// A block still open has content the caller was streamed and the
		// Response would leave out.
		if len(s.open) > 0 {
			return nil, fmt.Errorf("message_stop arrived with %d block(s) still open", len(s.open))
		}
		s.begin(&out)
		status, incomplete := responseStatus(s.stop)
		s.emit(&out, "response."+status, map[string]any{"response": s.m.response(status, s.items(), responseUsage(s.usage), incomplete, nil)})
		s.done = true
	case "error":
		var e map[string]json.RawMessage
		_ = json.Unmarshal(obj["error"], &e)
		typ, _ := text(e, "type")
		msg, _ := text(e, "message")
		return s.Failure(typ, msg), nil
	}
	return out.Bytes(), nil
}

// Failure is the event ending the stream as failed, for an error of the
// Messages API's type typ: response.failed, its Response carrying the items
// finished so far and the error, by the code a Response's error takes.
func (s *ResponsesStream) Failure(typ, msg string) []byte {
	if s.done {
		return nil
	}
	var out bytes.Buffer
	s.begin(&out)
	code := "server_error"
	switch typ {
	case "rate_limit_error":
		code = "rate_limit_exceeded"
	case "invalid_request_error":
		code = "invalid_prompt"
	}
	s.emit(&out, "response.failed", map[string]any{"response": s.m.response("failed", s.items(), responseUsage(s.usage), nil,
		map[string]string{"code": code, "message": msg})})
	s.done = true
	return out.Bytes()
}

// items is the output so far: the finished items, in order.
func (s *ResponsesStream) items() []any {
	out := make([]any, 0, len(s.output))
	for _, it := range s.output {
		if it != nil {
			out = append(out, it)
		}
	}
	return out
}

// begin sends the stream's first two events, once.
func (s *ResponsesStream) begin(out *bytes.Buffer) {
	if s.begun {
		return
	}
	s.begun = true
	r := s.m.response("in_progress", []any{}, nil, nil, nil)
	s.emit(out, "response.created", map[string]any{"response": r})
	s.emit(out, "response.in_progress", map[string]any{"response": r})
}

func (s *ResponsesStream) start(out *bytes.Buffer, obj map[string]json.RawMessage) error {
	var index int64
	var blk map[string]json.RawMessage
	if json.Unmarshal(obj["index"], &index) != nil || json.Unmarshal(obj["content_block"], &blk) != nil {
		return fmt.Errorf("a content_block_start has no index or content_block")
	}
	if s.open[index] != nil {
		return fmt.Errorf("block %d started twice", index)
	}
	typ, _ := text(blk, "type")
	it := &item{typ: typ, n: len(s.output)}
	at := map[string]any{"output_index": it.n}
	switch typ {
	case "text":
		it.id = s.m.itemID("msg", it.n)
		s.emit(out, "response.output_item.added", with(at, "item", messageItem(it.id, "in_progress", []any{})))
		s.emit(out, "response.content_part.added", with(at, "item_id", it.id, "content_index", 0, "part", outputText("")))
		if t, _ := text(blk, "text"); t != "" {
			it.text.WriteString(t)
			s.emit(out, "response.output_text.delta", with(at, "item_id", it.id, "content_index", 0, "delta", t, "logprobs", []any{}))
		}
	case "thinking":
		it.id = s.m.itemID("rs", it.n)
		s.emit(out, "response.output_item.added", with(at, "item", reasoningItem(it.id, "in_progress", []any{}, "")))
		s.emit(out, "response.reasoning_summary_part.added", with(at, "item_id", it.id, "summary_index", 0, "part", summaryText("")))
		if t, _ := text(blk, "thinking"); t != "" {
			it.text.WriteString(t)
			s.emit(out, "response.reasoning_summary_text.delta", with(at, "item_id", it.id, "summary_index", 0, "delta", t))
		}
		sig, _ := text(blk, "signature")
		it.sig.WriteString(sig)
	case "redacted_thinking":
		it.id = s.m.itemID("rs", it.n)
		data, _ := text(blk, "data")
		it.sig.WriteString(data)
		s.emit(out, "response.output_item.added", with(at, "item", reasoningItem(it.id, "in_progress", []any{}, data)))
	case "tool_use":
		it.id = s.m.itemID("fc", it.n)
		it.call, _ = text(blk, "id")
		it.name, _ = text(blk, "name")
		if in := blk["input"]; !null(in) && !bytes.Equal(compact(in), []byte("{}")) {
			it.input = string(compact(in))
		}
		s.emit(out, "response.output_item.added", with(at, "item", callItem(it.id, "in_progress", it.call, it.name, "")))
	default:
		return fmt.Errorf("a %q block has no Responses counterpart", typ)
	}
	s.open[index] = it
	s.output = append(s.output, nil)
	return nil
}

func (s *ResponsesStream) delta(out *bytes.Buffer, obj map[string]json.RawMessage) error {
	it, d, err := s.block(obj, "delta")
	if err != nil {
		return err
	}
	at := map[string]any{"output_index": it.n, "item_id": it.id}
	switch typ, _ := text(d, "type"); {
	case typ == "text_delta" && it.typ == "text":
		t, _ := text(d, "text")
		it.text.WriteString(t)
		s.emit(out, "response.output_text.delta", with(at, "content_index", 0, "delta", t, "logprobs", []any{}))
	case typ == "thinking_delta" && it.typ == "thinking":
		t, _ := text(d, "thinking")
		it.text.WriteString(t)
		s.emit(out, "response.reasoning_summary_text.delta", with(at, "summary_index", 0, "delta", t))
	case typ == "signature_delta" && it.typ == "thinking":
		sig, _ := text(d, "signature")
		it.sig.WriteString(sig)
	case typ == "input_json_delta" && it.typ == "tool_use":
		t, _ := text(d, "partial_json")
		it.args.WriteString(t)
		s.emit(out, "response.function_call_arguments.delta", with(at, "delta", t))
	case typ == "citations_delta":
	default:
		return fmt.Errorf("a %q delta for a %s block has no Responses counterpart", typ, it.typ)
	}
	return nil
}

func (s *ResponsesStream) finish(out *bytes.Buffer, obj map[string]json.RawMessage) error {
	it, _, err := s.block(obj, "")
	if err != nil {
		return err
	}
	at := map[string]any{"output_index": it.n}
	var done map[string]any
	switch it.typ {
	case "text":
		t := it.text.String()
		part := outputText(t)
		s.emit(out, "response.output_text.done", with(at, "item_id", it.id, "content_index", 0, "text", t, "logprobs", []any{}))
		s.emit(out, "response.content_part.done", with(at, "item_id", it.id, "content_index", 0, "part", part))
		done = messageItem(it.id, "completed", []any{part})
	case "thinking":
		t := it.text.String()
		s.emit(out, "response.reasoning_summary_text.done", with(at, "item_id", it.id, "summary_index", 0, "text", t))
		s.emit(out, "response.reasoning_summary_part.done", with(at, "item_id", it.id, "summary_index", 0, "part", summaryText(t)))
		done = reasoningItem(it.id, "completed", []any{summaryText(t)}, it.sig.String())
	case "redacted_thinking":
		done = reasoningItem(it.id, "completed", []any{}, it.sig.String())
	case "tool_use":
		args := it.args.String()
		switch {
		case args == "" && it.input != "":
			args = it.input
		case args == "":
			args = "{}"
		}
		s.emit(out, "response.function_call_arguments.done", with(at, "item_id", it.id, "arguments", args))
		done = callItem(it.id, "completed", it.call, it.name, args)
	}
	s.emit(out, "response.output_item.done", with(at, "item", done))
	s.output[it.n] = done
	var index int64
	_ = json.Unmarshal(obj["index"], &index)
	delete(s.open, index)
	return nil
}

// block is the open block an event names, and the object at key in it.
func (s *ResponsesStream) block(obj map[string]json.RawMessage, key string) (*item, map[string]json.RawMessage, error) {
	var index int64
	if json.Unmarshal(obj["index"], &index) != nil || s.open[index] == nil {
		return nil, nil, fmt.Errorf("an event names a block that is not open")
	}
	var d map[string]json.RawMessage
	if key != "" && json.Unmarshal(obj[key], &d) != nil {
		return nil, nil, fmt.Errorf("an event's %s is not an object", key)
	}
	return s.open[index], d, nil
}

// emit writes one event, typ its type, numbered next.
func (s *ResponsesStream) emit(out *bytes.Buffer, typ string, fields map[string]any) {
	fields["type"], fields["sequence_number"] = typ, s.seq
	s.seq++
	out.WriteString("event: " + typ + "\ndata: ")
	out.Write(encode(fields))
	out.WriteString("\n\n")
}

// with is base and the key-value pairs after it, in a new map.
func with(base map[string]any, kv ...any) map[string]any {
	m := make(map[string]any, len(base)+len(kv)/2+2)
	for k, v := range base {
		m[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// compact is raw JSON with its insignificant space removed; raw that does
// not parse is returned as it is.
func compact(raw json.RawMessage) []byte {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}
