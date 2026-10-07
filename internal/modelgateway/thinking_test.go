package modelgateway_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// signer is an upstream that holds thinking to what Anthropic's API does
// (docs/plan/59_model-gateway.md, "Verification"): each block it returns is
// signed over the request ahead of it — the messages it was sent, then the
// blocks before it in its own answer — and a request carrying a block whose
// signature is not the one it would sign over that same prefix, because
// another signer made it or the prefix has changed, is refused. It refuses an
// empty message too, as the Messages API does. A signature holds a dot, as an
// opaque value may.
type signer struct {
	*fake
	name string
	down atomic.Bool
	mu   sync.Mutex
	// reply is the shape of its answers.
	reply reply
	// intercept, when set, may answer a call itself before any check.
	intercept func(w http.ResponseWriter, c fakeCall) bool
}

type reply struct {
	redacted bool // a redacted_thinking block first
	thinking int  // thinking blocks
	unsigned bool // whose signatures are empty
	// unsignedAt is the one thinking block, counted from 1, whose signature
	// is empty; 0 for none.
	unsignedAt int
	tools      int // tool calls after them
}

func newSigner(t *testing.T, name string) *signer {
	t.Helper()
	s := &signer{name: name, reply: reply{thinking: 1, tools: 1}}
	s.fake = newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.intercept != nil && s.intercept(w, c) {
			return
		}
		if s.down.Load() {
			writeBody(w, 503, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		prefix, refusal := s.check(c)
		switch {
		case refusal != "":
			writeBody(w, 400, invalidRequest(refusal))
		case c.Path == "/v1/messages/count_tokens":
			writeBody(w, 200, `{"input_tokens":1}`)
		default:
			s.answer(w, c, prefix)
		}
	})
	return s
}

// set changes what s answers, between requests.
func (s *signer) set(r reply, intercept func(w http.ResponseWriter, c fakeCall) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply, s.intercept = r, intercept
}

func invalidRequest(msg string) string {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": msg}})
	return string(b)
}

// signedBy configures s as a deployment of its own provider.
func (e *env) signedBy(s *signer) store.Deployment {
	e.t.Helper()
	p := e.provider(s.URL)
	e.credential(p, "sk-"+s.name+"-key1", 1)
	return e.deployment(p, s.name+"-model")
}

func (s *signer) sign(prefix, before []string, text string) string {
	h := sha256.Sum256([]byte(s.name + "\x00" + strings.Join(prefix, "\x01") + "\x00" + strings.Join(before, "\x01") + "\x00" + text))
	return s.name + "." + hex.EncodeToString(h[:10])
}

// check verifies every thinking block in a call and returns the prefix its
// answer is signed over, or the refusal.
func (s *signer) check(c fakeCall) (prefix []string, refusal string) {
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(c.Body["messages"], &msgs)
	for i, m := range msgs {
		blocks := blocksOf(m.Content)
		if len(blocks) == 0 {
			return nil, fmt.Sprintf("messages.%d: all messages must have non-empty content except for the optional final assistant message", i)
		}
		var before []string
		for j, b := range blocks {
			var f struct{ Type, Thinking, Signature, Data string }
			_ = json.Unmarshal(b, &f)
			switch {
			case f.Type == "thinking" && f.Signature != s.sign(prefix, before, f.Thinking):
				return nil, fmt.Sprintf("messages.%d.content.%d: Invalid `signature` in `thinking` block", i, j)
			case f.Type == "redacted_thinking" && f.Data != s.sign(prefix, before, "redacted"):
				return nil, fmt.Sprintf("messages.%d.content.%d: Invalid `data` in `redacted_thinking` block", i, j)
			}
			before = append(before, canon(b))
		}
		prefix = append(prefix, m.Role+":"+strings.Join(before, ","))
	}
	return prefix, ""
}

// blocksOf is a message's content as blocks, a string being one text block.
func blocksOf(content json.RawMessage) []json.RawMessage {
	var text string
	if json.Unmarshal(content, &text) == nil {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		return []json.RawMessage{b}
	}
	var blocks []json.RawMessage
	_ = json.Unmarshal(content, &blocks)
	return blocks
}

// canon is a block as signed: its fields in order, nulls left out, so that
// the same block encoded by the SDK or by the gateway signs alike.
func canon(b json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for k, v := range m {
		if v == nil {
			delete(m, k)
		}
	}
	out, _ := json.Marshal(m)
	return string(out)
}

// canonJSON is a JSON document with its keys in order, for comparing what two
// encoders wrote.
func canonJSON(b []byte) string {
	var v any
	_ = json.Unmarshal(b, &v)
	out, _ := json.Marshal(v)
	return string(out)
}

func (s *signer) answer(w http.ResponseWriter, c fakeCall, prefix []string) {
	var blocks []map[string]any
	var before []string
	add := func(b map[string]any) {
		raw, _ := json.Marshal(b)
		blocks = append(blocks, b)
		before = append(before, canon(raw))
	}
	r := s.reply
	if r.redacted {
		add(map[string]any{"type": "redacted_thinking", "data": s.sign(prefix, before, "redacted")})
	}
	for k := range r.thinking {
		text := fmt.Sprintf("%s thought %d at %d", s.name, k, len(prefix))
		sig := ""
		if !r.unsigned && r.unsignedAt != k+1 {
			sig = s.sign(prefix, before, text)
		}
		add(map[string]any{"type": "thinking", "thinking": text, "signature": sig})
	}
	for k := range r.tools {
		add(map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%s_%d_%d", s.name, len(prefix), k), "name": "probe", "input": map[string]any{}})
	}
	if len(blocks) == 0 {
		add(map[string]any{"type": "text", "text": "done"})
	}
	stop := "end_turn"
	if r.tools > 0 {
		stop = "tool_use"
	}
	var stream bool
	_ = json.Unmarshal(c.Body["stream"], &stream)
	if !stream {
		b, _ := json.Marshal(map[string]any{"id": "msg_s", "type": "message", "role": "assistant", "model": c.Model,
			"content": blocks, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
		writeBody(w, 200, string(b))
		return
	}
	ev := func(name string, data any) string {
		b, _ := json.Marshal(data)
		return "event: " + name + "\ndata: " + string(b) + "\n\n"
	}
	delta := func(i int, d map[string]any) string {
		return ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": d})
	}
	out := []string{ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_s", "type": "message",
		"role": "assistant", "model": c.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}})}
	for i, b := range blocks {
		start := b
		var deltas []string
		switch b["type"] {
		case "thinking":
			// The signature arrives after an empty start, in fragments after
			// an empty one, as the gateway must wrap only the first that
			// holds anything.
			start = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
			deltas = append(deltas, delta(i, map[string]any{"type": "thinking_delta", "thinking": b["thinking"]}))
			if sig := b["signature"].(string); sig != "" {
				n := len(sig) / 3
				for _, frag := range []string{"", sig[:n], sig[n : 2*n], sig[2*n:]} {
					deltas = append(deltas, delta(i, map[string]any{"type": "signature_delta", "signature": frag}))
				}
			}
		case "text":
			start = map[string]any{"type": "text", "text": ""}
			deltas = append(deltas, delta(i, map[string]any{"type": "text_delta", "text": b["text"]}))
		}
		out = append(out, ev("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": start}))
		out = append(out, deltas...)
		out = append(out, ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": i}))
	}
	out = append(out,
		ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}}),
		ev("message_stop", map[string]any{"type": "message_stop"}))
	sse(w, out...)
}

// thinkingIn is the provenance values — signatures and redacted data — of
// every thinking block a call carried.
func thinkingIn(c fakeCall) []string {
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(c.Body["messages"], &msgs)
	var out []string
	for _, m := range msgs {
		for _, b := range blocksOf(m.Content) {
			var f struct{ Type, Signature, Data string }
			_ = json.Unmarshal(b, &f)
			switch f.Type {
			case "thinking":
				out = append(out, f.Signature)
			case "redacted_thinking":
				out = append(out, f.Data)
			}
		}
	}
	return out
}

func (s *signer) last() fakeCall {
	calls := s.recorded()
	return calls[len(calls)-1]
}

// provenance is the provenance values of an answer's thinking blocks.
func provenance(m *anthropic.Message) []string {
	var out []string
	for _, b := range m.Content {
		switch b.Type {
		case "thinking":
			out = append(out, b.Signature)
		case "redacted_thinking":
			out = append(out, b.Data)
		}
	}
	return out
}

func params(history []anthropic.MessageParam) anthropic.MessageNewParams {
	return anthropic.MessageNewParams{Model: "m", MaxTokens: 64, Messages: history}
}

func talk(t *testing.T, cl *anthropic.Client, history []anthropic.MessageParam, opts ...option.RequestOption) *anthropic.Message {
	t.Helper()
	m, err := cl.Messages.New(context.Background(), params(history), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// next is history with an answer returned verbatim, as a client returns it,
// and a user turn answering each of its tool calls, or text when it made
// none.
func next(history []anthropic.MessageParam, m *anthropic.Message) []anthropic.MessageParam {
	history = append(history, m.ToParam())
	var turn []anthropic.ContentBlockParamUnion
	for _, b := range m.Content {
		if b.Type == "tool_use" {
			turn = append(turn, anthropic.NewToolResultBlock(b.ID, "ok", false))
		}
	}
	if len(turn) == 0 {
		turn = append(turn, anthropic.NewTextBlock("go on"))
	}
	return append(history, anthropic.NewUserMessage(turn...))
}

// edited is history with the first thinking block of message i rewritten.
func edited(history []anthropic.MessageParam, i int) []anthropic.MessageParam {
	out := append([]anthropic.MessageParam(nil), history...)
	content := append([]anthropic.ContentBlockParamUnion(nil), out[i].Content...)
	for j, b := range content {
		if b.OfThinking != nil {
			blk := *b.OfThinking
			blk.Thinking = "edited"
			content[j] = anthropic.ContentBlockParamUnion{OfThinking: &blk}
			break
		}
	}
	out[i].Content = content
	return out
}

func wantPrefixes(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("thinking values = %q, want prefixes %q", got, want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("thinking value %d = %q, want prefix %q", i, got[i], want[i])
		}
	}
}

// A tool loop whose first deployment fails reaches the second with none of
// the first's blocks and stays there after the first recovers, the second
// having produced the newest block; when the second fails in turn, the first
// gets back only its own blocks, each under the prefix it produced it under —
// which the signers enforce.
func TestThinkingReturnsToItsProducerAlone(t *testing.T) {
	e := newEnv(t)
	a, b := newSigner(t, "a"), newSigner(t, "b")
	da, db := e.signedBy(a), e.signedBy(b)
	e.alias("m", target(da, 0), target(db, 1))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	h := hello()
	m := talk(t, cl, h)
	wantPrefixes(t, provenance(m), "mapgw1."+da.ID+".a.")
	h = next(h, m)
	h = next(h, talk(t, cl, h))

	a.down.Store(true)
	m = talk(t, cl, h)
	wantPrefixes(t, provenance(m), "mapgw1."+db.ID+".b.")
	if got := thinkingIn(b.last()); len(got) != 0 {
		t.Fatalf("b was sent a's thinking: %q", got)
	}
	h = next(h, m)

	a.down.Store(false)
	tried := len(a.recorded())
	h = next(h, talk(t, cl, h))
	if len(a.recorded()) != tried {
		t.Fatal("a was tried first though b produced the newest block")
	}
	wantPrefixes(t, thinkingIn(b.last()), "b.")

	b.down.Store(true)
	talk(t, cl, h)
	wantPrefixes(t, thinkingIn(a.last()), "a.", "a.")
}

// A reply that held only thinking leaves no empty assistant message when its
// thinking goes elsewhere: the message goes, and the user turns either side
// of it become one. Consecutive user turns the caller sent stay as sent.
func TestAThinkingOnlyReplyLeavesNoEmptyMessage(t *testing.T) {
	e := newEnv(t)
	a, b := newSigner(t, "a"), newSigner(t, "b")
	da, db := e.signedBy(a), e.signedBy(b)
	e.alias("m", target(db, 0))
	key := e.key(everyAlias)
	e.start()

	body := fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"hello"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"mapgw1.%s.a.x"}]},
		{"role":"user","content":[{"type":"text","text":"again"}]}]}`, da.ID)
	resp, out := e.do("POST", "/v1/messages", body, map[string]string{"x-api-key": key})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	if got, want := canonJSON(b.last().Body["messages"]), canonJSON([]byte(`[{"role":"user","content":"hello"},{"role":"user","content":[{"type":"text","text":"hi"},{"type":"text","text":"again"}]}]`)); got != want {
		t.Fatalf("b was sent %s, want %s", got, want)
	}

	// A turn whose content is neither a string nor an array joins nothing:
	// both go as sent, for the upstream to answer.
	body = fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"hello"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"mapgw1.%s.a.x"}]},
		{"role":"user","content":{"type":"text","text":"kept"}}]}`, da.ID)
	e.do("POST", "/v1/messages", body, map[string]string{"x-api-key": key})
	if got, want := canonJSON(b.last().Body["messages"]), canonJSON([]byte(`[{"role":"user","content":"hello"},{"role":"user","content":{"type":"text","text":"kept"}}]`)); got != want {
		t.Fatalf("b was sent %s, want %s", got, want)
	}

	// A role is read by its exact key, as the upstream reads it: "Role" does
	// not keep a user turn from joining.
	body = fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"hello"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"mapgw1.%s.a.x"}]},
		{"role":"user","Role":"assistant","content":"next"}]}`, da.ID)
	e.do("POST", "/v1/messages", body, map[string]string{"x-api-key": key})
	if got, want := canonJSON(b.last().Body["messages"]), canonJSON([]byte(`[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"next"}]}]`)); got != want {
		t.Fatalf("b was sent %s, want %s", got, want)
	}
}

// A caller that has left by the time its attempt's thinking is refused is
// not retried without it: the retry, like any other, is not made for a
// caller gone, which is still written the refusal.
func TestACallerGoneIsNotRetriedWithoutThinking(t *testing.T) {
	e := newEnv(t)
	a := newSigner(t, "a")
	e.alias("m", target(e.signedBy(a), 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)
	h := next(hello(), talk(t, cl, hello()))

	arrived, release := make(chan struct{}), make(chan struct{})
	refused := false
	a.set(reply{thinking: 1, tools: 1}, func(w http.ResponseWriter, _ fakeCall) bool {
		if refused {
			return false
		}
		refused = true
		close(arrived)
		<-release
		writeBody(w, 400, invalidRequest("messages.1.content.0: Invalid `signature` in `thinking` block"))
		return true
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = cl.Messages.New(ctx, params(h))
		close(done)
	}()
	<-arrived
	cancel()
	<-done
	time.Sleep(200 * time.Millisecond) // the gateway sees its caller's connection close
	close(release)
	time.Sleep(500 * time.Millisecond)
	if n := len(a.recorded()); n != 2 {
		t.Fatalf("%d calls after the caller left, want 2", n)
	}
}

// A streamed answer whose signature arrives in fragments after an empty start
// assembles, as the SDK accumulates it, the same wrapped values as the whole
// answer, a redacted block's data included; a block with no signature has
// nothing to wrap and comes back as it came, either way.
func TestAStreamedAnswerWrapsAsTheWholeAnswerDoes(t *testing.T) {
	e := newEnv(t)
	a := newSigner(t, "a")
	da := e.signedBy(a)
	e.alias("m", target(da, 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	for _, r := range []reply{{redacted: true, thinking: 2, tools: 1}, {thinking: 1, unsigned: true}} {
		a.set(r, nil)
		whole := talk(t, cl, hello())
		stream := cl.Messages.NewStreaming(context.Background(), params(hello()))
		var acc anthropic.Message
		for stream.Next() {
			if err := acc.Accumulate(stream.Current()); err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		got, want := provenance(&acc), provenance(whole)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("stream assembled %q, whole answer %q", got, want)
		}
		for _, v := range want {
			if r.unsigned && v != "" || !r.unsigned && !strings.HasPrefix(v, "mapgw1."+da.ID+".") {
				t.Fatalf("value %q came back from a reply with unsigned=%v", v, r.unsigned)
			}
		}
	}

	// On the wire the start keeps its empty signature and the first fragment
	// that holds anything carries the wrapper, so a client that keeps only
	// the latest fragment, as some SDKs do, still has it from an upstream
	// that sends its signature whole.
	a.set(reply{thinking: 1}, nil)
	resp, raw := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`,
		map[string]string{"x-api-key": key})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var sigs []string
	for _, block := range strings.Split(string(raw), "\n\n") {
		_, data, ok := strings.Cut(block, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			ContentBlock *struct{ Signature *string } `json:"content_block"`
			Delta        *struct{ Type, Signature string }
		}
		_ = json.Unmarshal([]byte(data), &ev)
		switch {
		case ev.ContentBlock != nil && ev.ContentBlock.Signature != nil:
			sigs = append(sigs, "start:"+*ev.ContentBlock.Signature)
		case ev.Delta != nil && ev.Delta.Type == "signature_delta":
			sigs = append(sigs, ev.Delta.Signature)
		}
	}
	if len(sigs) != 5 || sigs[0] != "start:" || sigs[1] != "" || !strings.HasPrefix(sigs[2], "mapgw1."+da.ID+".a.") ||
		strings.Contains(sigs[3]+sigs[4], "mapgw1") {
		t.Fatalf("the signature went out as %q", sigs)
	}
}

// A thinking block an upstream sends whole inside message_start is wrapped
// like any other.
func TestAThinkingBlockInMessageStartIsWrapped(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		sse(w, fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[{\"type\":\"thinking\",\"thinking\":\"t\",\"signature\":\"s\"}],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", c.Model),
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	p := e.provider(f.URL)
	e.credential(p, "sk-start-key1", 1)
	d := e.deployment(p, "up")
	e.alias("m", target(d, 0))
	key := e.key(everyAlias)
	e.start()

	resp, raw := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`,
		map[string]string{"x-api-key": key})
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"signature":"mapgw1.`+d.ID+`.s"`) {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
}

// The deployment tried first is the newest block's producer among the alias's
// targets, whatever its priority: a newer block naming a deployment outside
// the alias, which no upstream is sent, does not hide it.
func TestTheNewestReachableProducerIsPreferred(t *testing.T) {
	e := newEnv(t)
	a, b, c := newSigner(t, "a"), newSigner(t, "b"), newSigner(t, "c")
	da, db, dc := e.signedBy(a), e.signedBy(b), e.signedBy(c)
	e.alias("m", target(db, 0), target(da, 1))
	e.alias("ma", target(da, 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	m, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: "ma", MaxTokens: 64, Messages: hello()})
	if err != nil {
		t.Fatal(err)
	}
	h := append(next(hello(), m),
		anthropic.NewAssistantMessage(anthropic.NewThinkingBlock("mapgw1."+dc.ID+".c.x", "elsewhere"), anthropic.NewTextBlock("c said")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("and?")))
	talk(t, cl, h)
	if len(b.recorded()) != 0 || len(a.recorded()) != 2 {
		t.Fatalf("calls: a %d, b %d", len(a.recorded()), len(b.recorded()))
	}
	wantPrefixes(t, thinkingIn(a.last()), "a.")
}

// A block the gateway did not wrap, whose wrapper names a deployment outside
// the alias or one no longer configured, or whose wrapper is malformed,
// reaches no upstream, on count_tokens as on messages; the messages it was
// in keep the rest.
func TestForeignThinkingNeverReachesAnUpstream(t *testing.T) {
	e := newEnv(t)
	a, c := newSigner(t, "a"), newSigner(t, "c")
	da, dc := e.signedBy(a), e.signedBy(c)
	e.alias("m", target(da, 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	h := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("hello")),
		anthropic.NewAssistantMessage(anthropic.NewThinkingBlock("raw.sig", "unwrapped"), anthropic.NewTextBlock("one")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("next")),
		anthropic.NewAssistantMessage(anthropic.NewThinkingBlock("mapgw1."+dc.ID+".c.x", "outside"),
			anthropic.NewRedactedThinkingBlock("mapgw1.gwdep_gone.y"), anthropic.NewThinkingBlock("mapgw1."+da.ID, "malformed"),
			anthropic.NewTextBlock("two")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("last")),
	}
	talk(t, cl, h)
	if _, err := cl.Messages.CountTokens(context.Background(), anthropic.MessageCountTokensParams{Model: "m", Messages: h}); err != nil {
		t.Fatal(err)
	}
	calls := a.recorded()
	if len(calls) != 2 || calls[1].Path != "/v1/messages/count_tokens" {
		t.Fatalf("a was called %d times", len(calls))
	}
	for _, call := range calls {
		var msgs []json.RawMessage
		_ = json.Unmarshal(call.Body["messages"], &msgs)
		if got := thinkingIn(call); len(got) != 0 || len(msgs) != 5 {
			t.Fatalf("%s was sent thinking %q in %d messages", call.Path, got, len(msgs))
		}
	}
	if len(c.recorded()) != 0 {
		t.Fatal("a deployment outside the alias was called")
	}

	// A type spelled with an escape is the same type, though the bytes never
	// say "thinking"; and a type is read by its exact key, as the upstream
	// reads it, so "Type" does not make a thinking block text.
	for _, block := range []string{`{"type":"redacted_\u0074hinking","data":"mapgw1.%s.c.x"}`,
		`{"type":"thinking","thinking":"t","signature":"mapgw1.%s.c.y","Type":"text"}`} {
		body := fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"},
			{"role":"assistant","content":[`+block+`,{"type":"text","text":"one"}]},
			{"role":"user","content":"next"}]}`, dc.ID)
		if resp, out := e.do("POST", "/v1/messages", body, map[string]string{"x-api-key": key}); resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, out)
		}
		var msgs []struct {
			Content []json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(a.last().Body["messages"], &msgs)
		if len(msgs) != 3 || len(msgs[1].Content) != 1 {
			t.Fatalf("%s reached the upstream: %s", fmt.Sprintf(block, dc.ID), a.last().Body["messages"])
		}
	}

	// A message that is not an object goes as sent, for the upstream to
	// refuse, and the others are filtered all the same.
	body := fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"mapgw1.%s.c.z"},{"type":"text","text":"one"}]},
		"oops"]}`, dc.ID)
	e.do("POST", "/v1/messages", body, map[string]string{"x-api-key": key})
	if sent := string(a.last().Body["messages"]); strings.Contains(sent, "mapgw1.") || !strings.Contains(sent, `"oops"`) {
		t.Fatalf("a was sent %s", sent)
	}
}

// The retry strip mode makes is an attempt like any other, held to the
// deployment's budget: with two attempts per deployment, a refusal and its
// stripped retry leave none for the deployment's second credential.
func TestStripModeStaysWithinTheBudget(t *testing.T) {
	e := newEnv(t)
	a := newSigner(t, "a")
	p := e.provider(a.URL)
	e.credential(p, "sk-a-key1", 1)
	e.credential(p, "sk-a-key2", 1)
	e.alias("m", target(e.deployment(p, "a-model"), 0))
	key := e.key(everyAlias)
	e.start(func(c *modelgateway.Config) { c.MaxAttempts = 2 })
	cl := e.client(key)
	h := next(hello(), talk(t, cl, hello()))

	a.set(reply{thinking: 1, tools: 1}, func(w http.ResponseWriter, c fakeCall) bool {
		if len(thinkingIn(c)) > 0 {
			writeBody(w, 400, invalidRequest("messages.1.content.0: Invalid `signature` in `thinking` block"))
		} else {
			writeBody(w, 503, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		}
		return true
	})
	n := len(a.recorded())
	_, err := cl.Messages.New(context.Background(), params(h))
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 || len(a.recorded())-n != 2 {
		t.Fatalf("%d calls, error %v; want 2 and the stripped attempt's 503", len(a.recorded())-n, err)
	}
}

// A stream's signatures are wrapped in block order, as a whole answer's are:
// only the open block's first fragment, read by its value — a type spelled
// with an escape is a signature fragment, and one that names no index is
// none — and nothing once a thinking block ahead has gone unwrapped.
func TestAStreamWrapsInBlockOrder(t *testing.T) {
	var events atomic.Pointer[[]string]
	ev := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		start := ev("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`, c.Model))
		sse(w, append(append([]string{start}, *events.Load()...), ev("message_stop", `{"type":"message_stop"}`))...)
	})
	p := e.provider(f.URL)
	e.credential(p, "sk-order-key1", 1)
	d := e.deployment(p, "up")
	e.alias("m", target(d, 0))
	key := e.key(everyAlias)
	e.start()

	thinking := `{"type":"thinking","thinking":"","signature":""}`
	start := func(index, block string) string {
		return ev("content_block_start", `{"type":"content_block_start"`+index+`,"content_block":`+block+`}`)
	}
	sig := func(index, typ, v string) string {
		return ev("content_block_delta", `{"type":"content_block_delta"`+index+`,"delta":{"type":"`+typ+`","signature":"`+v+`"}}`)
	}
	stop := func(index string) string { return ev("content_block_stop", `{"type":"content_block_stop"`+index+`}`) }
	i0, i1 := `,"index":0`, `,"index":1`
	wrapped := func(v string) string { return "mapgw1." + d.ID + "." + v }
	for _, tc := range []struct {
		name   string
		events []string
		want   []string
	}{
		{"in order", []string{start(i0, thinking), sig(i0, "signature_delta", "s0"), stop(i0),
			start(i1, `{"type":"redacted_thinking","data":"d1"}`), stop(i1)}, []string{wrapped("s0"), wrapped("d1")}},
		{"an escaped type and a fragment with no index", []string{start(i0, thinking), sig("", "signature_delta", "zz"),
			sig(i0, `signature_\u0064elta`, "s1"), stop(i0)}, []string{"zz", wrapped("s1")}},
		{"a fragment of another block", []string{start(i0, thinking), sig(i1, "signature_delta", "x"),
			sig(i0, "signature_delta", "s0"), stop(i0)}, []string{"x", wrapped("s0")}},
		{"interleaved", []string{start(i0, thinking), start(i1, thinking), sig(i1, "signature_delta", "s1"),
			sig(i0, "signature_delta", "s0")}, []string{"s1", "s0"}},
		{"an unsigned block ahead", []string{start(i0, thinking), stop(i0), start(i1, thinking),
			sig(i1, "signature_delta", "s1")}, []string{"s1"}},
		{"an empty redacted block ahead", []string{start(i0, `{"type":"redacted_thinking","data":""}`), stop(i0),
			start(i1, `{"type":"redacted_thinking","data":"d1"}`), stop(i1)}, []string{"d1"}},
		{"an empty start with no index ahead", []string{start("", thinking), start(i1, thinking),
			sig(i1, "signature_delta", "s1")}, []string{"s1"}},
		{"a start read by its exact keys", []string{start(i0, thinking+`,"Content_Block":{"type":"text","text":""}`), stop(i0),
			start(i1, thinking), sig(i1, "signature_delta", "s1")}, []string{"s1"}},
		{"a delta read by its exact keys", []string{start(i0, thinking),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","Type":"signature_delta","signature":"zz"}}`),
			sig(i0, "signature_delta", "s0")}, []string{"zz", wrapped("s0")}},
	} {
		events.Store(&tc.events)
		resp, raw := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			map[string]string{"x-api-key": key})
		var got []string
		for _, line := range strings.Split(string(raw), "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var ev struct {
				ContentBlock struct{ Signature, Data string } `json:"content_block"`
				Delta        struct{ Signature string }       `json:"delta"`
			}
			_ = json.Unmarshal([]byte(data), &ev)
			for _, v := range []string{ev.ContentBlock.Signature, ev.ContentBlock.Data, ev.Delta.Signature} {
				if v != "" {
					got = append(got, v)
				}
			}
		}
		if resp.StatusCode != 200 || strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: status %d, values %q, want %q", tc.name, resp.StatusCode, got, tc.want)
		}
	}
}

// A thinking block after an unsigned one in the same answer goes unwrapped
// too, whole or streamed: its signature covers the unsigned block, which no
// upstream is sent back, so the next request sends only the block ahead of
// it, and the conversation goes on with no refusal. A strip-mode answer whose
// first thinking block is unsigned wraps nothing, so carries no reset mark.
func TestAnUnsignedBlockEndsWhatAnAnswerWraps(t *testing.T) {
	e := newEnv(t)
	a := newSigner(t, "a")
	da := e.signedBy(a)
	e.alias("m", target(da, 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)
	both := func(h []anthropic.MessageParam) []*anthropic.Message {
		whole := talk(t, cl, h)
		stream := cl.Messages.NewStreaming(context.Background(), params(h))
		var acc anthropic.Message
		for stream.Next() {
			if err := acc.Accumulate(stream.Current()); err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		return []*anthropic.Message{whole, &acc}
	}

	a.set(reply{thinking: 3, unsignedAt: 2, tools: 1}, nil)
	for _, m := range both(hello()) {
		got := provenance(m)
		if len(got) != 3 || !strings.HasPrefix(got[0], "mapgw1."+da.ID+".a.") || got[1] != "" || !strings.HasPrefix(got[2], "a.") {
			t.Fatalf("an answer came back as %q", got)
		}
		n := len(a.recorded())
		talk(t, cl, next(hello(), m))
		if calls, sent := len(a.recorded())-n, thinkingIn(a.last()); calls != 1 || strings.Join(sent, " ") != strings.TrimPrefix(got[0], "mapgw1."+da.ID+".") {
			t.Fatalf("returning it took %d calls, sending %q", calls, sent)
		}
	}

	h := edited(next(hello(), talk(t, cl, hello())), 1)
	a.set(reply{thinking: 2, unsignedAt: 1, tools: 1}, nil)
	for _, m := range both(h) {
		if got := provenance(m); len(got) != 2 || got[0] != "" || !strings.HasPrefix(got[1], "a.") {
			t.Fatalf("a strip-mode answer came back as %q", got)
		}
	}
}

// A block the upstream refuses earns one attempt without any thinking, whose
// answer's first thinking block carries the reset mark; the next request then
// keeps every block of that answer — two thinking blocks and two parallel
// tool calls — and none ahead of it, and the conversation goes on with no
// further refusal.
func TestARefusedThinkingIsStrippedOnce(t *testing.T) {
	e := newEnv(t)
	a := newSigner(t, "a")
	da := e.signedBy(a)
	e.alias("m", target(da, 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	h := next(hello(), talk(t, cl, hello()))
	h = edited(h, 1)
	a.set(reply{thinking: 2, tools: 2}, nil)
	n := len(a.recorded())
	m := talk(t, cl, h)
	calls := a.recorded()[n:]
	if len(calls) != 2 || len(thinkingIn(calls[0])) != 1 || len(thinkingIn(calls[1])) != 0 {
		t.Fatalf("the refusal took %d calls", len(calls))
	}
	wantPrefixes(t, provenance(m), "mapgw1r."+da.ID+".a.", "mapgw1."+da.ID+".a.")

	h = next(h, m)
	after := len(a.recorded())
	for range 2 {
		n = len(a.recorded())
		m = talk(t, cl, h)
		if calls := a.recorded()[n:]; len(calls) != 1 {
			t.Fatalf("a later request took %d calls", len(calls))
		}
		h = next(h, m)
	}
	call := a.recorded()[after]
	var msgs []struct {
		Content []json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(call.Body["messages"], &msgs)
	if len(msgs[1].Content) != 1 || len(msgs[3].Content) != 4 || len(thinkingIn(call)) != 2 {
		t.Fatalf("the next request was sent %d blocks of the refused answer and %d of the reset-marked one", len(msgs[1].Content), len(msgs[3].Content))
	}
}

// Strip mode holds for every attempt after it: an attempt without thinking
// that fails before its first byte falls back still stripped — the fallback
// is sent none of its own blocks either — and a second refusal earns no
// second retry.
func TestStripModeHoldsThroughFallback(t *testing.T) {
	e := newEnv(t)
	a, b := newSigner(t, "a"), newSigner(t, "b")
	da, db := e.signedBy(a), e.signedBy(b)
	e.alias("m", target(da, 0), target(db, 1))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	a.down.Store(true)
	h := next(hello(), talk(t, cl, hello()))
	a.down.Store(false)
	b.down.Store(true)
	m := talk(t, cl, h)
	wantPrefixes(t, provenance(m), "mapgw1."+da.ID+".a.")
	h = edited(next(h, m), 3)
	b.down.Store(false)

	a.set(reply{thinking: 1, tools: 1}, func(w http.ResponseWriter, c fakeCall) bool {
		if len(thinkingIn(c)) > 0 {
			return false
		}
		writeBody(w, 503, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		return true
	})
	m = talk(t, cl, h)
	if got := thinkingIn(b.last()); len(got) != 0 {
		t.Fatalf("a stripped request's fallback was sent %q", got)
	}
	wantPrefixes(t, provenance(m), "mapgw1r."+db.ID+".b.")

	// The fallback refusing too — though its own block is in the history —
	// earns it no retry of its own.
	b.set(reply{thinking: 1, tools: 1}, func(w http.ResponseWriter, c fakeCall) bool {
		writeBody(w, 400, invalidRequest("messages.1.content.0: Invalid `signature` in `thinking` block"))
		return true
	})
	na, nb := len(a.recorded()), len(b.recorded())
	_, err := cl.Messages.New(context.Background(), params(h))
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("a second refusal answered %v", err)
	}
	if len(a.recorded())-na != 2 || len(b.recorded())-nb != 1 {
		t.Fatalf("a second refusal took %d calls to a and %d to b", len(a.recorded())-na, len(b.recorded())-nb)
	}
}

// A strip-mode answer with no thinking has nothing to carry the reset mark,
// so the next request on that history pays the refusal and one retry again,
// and no more.
func TestAStripModeAnswerWithoutThinkingPaysAgain(t *testing.T) {
	e := newEnv(t)
	a := newSigner(t, "a")
	e.alias("m", target(e.signedBy(a), 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	h := edited(next(hello(), talk(t, cl, hello())), 1)
	a.set(reply{tools: 1}, nil)
	for range 2 {
		n := len(a.recorded())
		m := talk(t, cl, h)
		if calls := len(a.recorded()) - n; calls != 2 {
			t.Fatalf("a request took %d calls", calls)
		}
		h = next(h, m)
	}
}

// Only a refusal of the thinking a request carried earns the retry: one that
// names no thinking does not, nor DeepSeek's "must be passed back", nor one
// about a thinking parameter; Anthropic's "cannot be modified" does; and a
// request that carried no thinking — none in its history, or only another
// deployment's — is never retried for it.
func TestOnlyAThinkingRefusalEarnsTheRetry(t *testing.T) {
	e := newEnv(t)
	a := newSigner(t, "a")
	e.alias("m", target(e.signedBy(a), 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)
	h := next(hello(), talk(t, cl, hello()))
	foreign := []anthropic.MessageParam{hello()[0],
		anthropic.NewAssistantMessage(anthropic.NewThinkingBlock("raw.sig", "unwrapped"), anthropic.NewTextBlock("one")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("next"))}

	for _, tc := range []struct {
		history []anthropic.MessageParam
		status  int
		message string
		calls   int
	}{
		{h, 400, "messages.1.content.0: Invalid `signature` in `thinking` block", 2},
		{h, 400, "messages.1.content.0: `thinking` or `redacted_thinking` blocks in the latest assistant message cannot be modified. These blocks must remain as they were in the original response.", 2},
		{h, 400, "The `content[].thinking` in the thinking mode must be passed back to the API.", 1},
		{h, 400, "messages.1.content.0.type: Expected `thinking` or `redacted_thinking`, but found `tool_use`. When `thinking` is enabled, a final `assistant` message must start with a thinking block (preceeding the lastmost set of `tool_use` and `tool_result` blocks). We recommend you include thinking blocks from previous turns. To avoid this requirement, disable `thinking`.", 1},
		{h, 400, "max_tokens: Field required", 1},
		{h, 400, "thinking.budget_tokens: Input should be greater than or equal to 1024", 1},
		{h, 500, "messages.1.content.0: Invalid `signature` in `thinking` block", 1},
		{hello(), 400, "messages.1.content.0: Invalid `signature` in `thinking` block", 1},
		{foreign, 400, "messages.1.content.0: Invalid `signature` in `thinking` block", 1},
	} {
		refused := false
		a.set(reply{thinking: 1, tools: 1}, func(w http.ResponseWriter, _ fakeCall) bool {
			if refused {
				return false
			}
			refused = true
			writeBody(w, tc.status, invalidRequest(tc.message))
			return true
		})
		n := len(a.recorded())
		_, err := cl.Messages.New(context.Background(), params(tc.history))
		if calls := len(a.recorded()) - n; calls != tc.calls || (err == nil) != (tc.calls == 2) {
			t.Errorf("%q took %d calls (error %v), want %d", tc.message, calls, err, tc.calls)
		}
	}
}

// A session's requests keep one deployment when they carry no thinking, and
// yield to the producer of the newest block when they do.
func TestASessionYieldsToTheNewestProducer(t *testing.T) {
	e := newEnv(t)
	a, b := newSigner(t, "a"), newSigner(t, "b")
	e.alias("m", target(e.signedBy(a), 0), target(e.signedBy(b), 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)
	session := option.WithHeader(modelgateway.SessionHeader, "sesn_1")

	talk(t, cl, hello(), session)
	first, other := a, b
	if len(a.recorded()) == 0 {
		first, other = b, a
	}
	first.down.Store(true)
	h := next(hello(), talk(t, cl, hello(), session))
	first.down.Store(false)

	n := len(first.recorded())
	talk(t, cl, h, session)
	if len(first.recorded()) != n {
		t.Fatal("the session's deployment was tried ahead of the newest block's producer")
	}
	talk(t, cl, hello(), session)
	if len(first.recorded()) != n+1 || len(other.recorded()) != 2 {
		t.Fatalf("a request without thinking left the session's deployment: %d, %d calls", len(first.recorded())-n, len(other.recorded()))
	}
}

func TestThinkingRefusal(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{invalidRequest("messages.1.content.0: Invalid `signature` in `thinking` block"), true},
		{invalidRequest("messages.1.content.0: Invalid `data` in `redacted_thinking` block"), true},
		{invalidRequest("messages.3.content.0.thinking: each thinking block must contain thinking"), true},
		{invalidRequest("thinking.type: Input should be 'enabled', 'disabled' or 'adaptive'"), false},
		{invalidRequest("`max_tokens` must be greater than `thinking.budget_tokens`"), false},
		{invalidRequest("thinking is not supported by this model"), false},
		{invalidRequest("The `content[].thinking` in the thinking mode must be passed back to the API."), false},
		{invalidRequest("messages.1.content.0.thinking: Field required"), true},
		{`{"message":"invalid signature in thinking block"}`, true},
		{`{"message":"invalid thinking block"}`, true},
		{`invalid thinking signature`, true},
		{invalidRequest("Invalid redacted_thinking data"), true},
		{invalidRequest("The thinking block must be passed back to the API."), false},
		{invalidRequest("messages.0.content.0: unknown block type"), false},
		{invalidRequest("prompt is too long"), false},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"},"echo":"a thinking block, signed"}`, false},
		{`{"message":"max_tokens: too large","echo":"a thinking block, signed"}`, false},
		{`{"type":"error","error":{"type":"invalid_request_error"},"request":{"messages":"a thinking block, signed"}}`, false},
		{`{"detail":"max_tokens too large","input":"a thinking block, signed"}`, false},
		{`{"message":"max_tokens: too large","error":"bad request","echo":"a thinking block, signed"}`, false},
		{`"invalid signature in thinking block"`, false},
		{`{"error":"Invalid signature in thinking block"}`, true},
		// Anthropic's own wordings (platform.claude.com/docs/en/build-with-claude/thinking-troubleshooting).
		{invalidRequest("messages.1.content.0: Invalid `signature` in `thinking` block. The block is bound to a different conversation. Remove the block, or set `thinking.block_binding.prefix_mismatch_behavior` to \"drop_block\"."), true},
		{invalidRequest("messages.1.content.0.type: Expected `thinking` or `redacted_thinking`, but found `tool_use`. When `thinking` is enabled, a final `assistant` message must start with a thinking block (preceeding the lastmost set of `tool_use` and `tool_result` blocks). We recommend you include thinking blocks from previous turns. To avoid this requirement, disable `thinking`."), false},
		{invalidRequest("To turn thinking off on this model, send \"thinking\": {\"type\": \"between_tools\"} instead of {\"type\": \"disabled\"}. The model does not think before responding. The short updates it writes between tool calls come back as thinking blocks."), false},
		{invalidRequest("messages.3: output_config.effort 'low' differs from the 'high' in effect before it; effort cannot change when thinking is disabled on this model. Use effort 'high', or enable thinking."), false},
		{invalidRequest("\"thinking.type.disabled\" is not supported for this model. Use \"thinking.type.adaptive\" and \"output_config.effort\" to control thinking behavior."), false},
		{invalidRequest("adaptive thinking is not supported on this model"), false},
		{invalidRequest("messages.1.content.0: the `thinking` block is empty"), true},
		// Refusals that mention thinking or a signature but no block of the request.
		{invalidRequest("2 validation errors: messages.0.content: Field required; thinking.budget_tokens: too small"), false},
		{invalidRequest("invalid request signature"), false},
		{invalidRequest("messages.3.content.0.text: invalid value \"I was thinking...\""), false},
	} {
		if got := modelgateway.ThinkingRefusal([]byte(tc.body)); got != tc.want {
			t.Errorf("ThinkingRefusal(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}
