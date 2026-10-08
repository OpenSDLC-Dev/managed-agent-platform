package modelgateway_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/anthropics/anthropic-sdk-go"
)

// The gateway's live tier (docs/plan/59_model-gateway.md, "Live tier"): the
// official Anthropic SDK drives the gateway over HTTP, as any SDK caller
// would, to the real vendors RUN_LIVE_MODELGATEWAY names. Each model's
// provider points at a proxy that forwards to the vendor and records both
// directions, so the checks see what the vendor signed and what the gateway
// sent it back: neither vendor checks a signature, so its 200 alone proves
// nothing.
//
// Consent is the environment's alone; the keys come from the environment or
// the repo-root .env. A named vendor whose configuration is missing fails,
// and an unnamed one is never called.
const liveEnv = "RUN_LIVE_MODELGATEWAY"

type liveVendor struct {
	name     string // the profile, and the name RUN_LIVE_MODELGATEWAY uses
	keyEnv   string
	baseEnv  string // configures the base URL where the vendor has more than one
	base     string
	models   []string
	thinking anthropic.ThinkingConfigParamUnion // what asks its models to think
	quiet    []string                           // its models that return no thinking even so
}

var liveVendors = []liveVendor{
	{name: "deepseek", keyEnv: "DEEPSEEK_API_KEY", base: "https://api.deepseek.com/anthropic",
		models: []string{"deepseek-flash", "deepseek-v4-pro"}, thinking: anthropic.ThinkingConfigParamOfEnabled(1024)},
	// A MiniMax key works on its own region's host only, and MiniMax-M3
	// thinks only when asked adaptively (the plan's ground truth).
	{name: "minimax", keyEnv: "MINIMAX_API_KEY", baseEnv: "MINIMAX_BASE_URL",
		models:   []string{"MiniMax-M3", "MiniMax-M3.1-Flash-Preview"},
		thinking: anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
		// It returned no thinking block to this tier's request on 2026-10-07,
		// asked enabled or adaptive, called directly or through the gateway;
		// its round trip is checked without one, and any it returns is
		// checked like any other.
		quiet: []string{"MiniMax-M3.1-Flash-Preview"}},
}

// liveKeys is the keys of the vendors consented to, which a failure's
// message never prints.
var liveKeys []string

// namedVendors is the vendors consented to, each with its key and base URL.
func namedVendors(t *testing.T) []liveVendor {
	t.Helper()
	named := os.Getenv(liveEnv)
	if named == "" {
		t.Skipf("%s is not set: skipping the gateway's live tier (no vendor is called)", liveEnv)
	}
	var out []liveVendor
	for _, n := range strings.Split(named, ",") {
		i := slices.IndexFunc(liveVendors, func(v liveVendor) bool { return v.name == strings.TrimSpace(n) })
		if i < 0 {
			t.Fatalf("%s names %q, which this tier does not cover", liveEnv, n)
		}
		v := liveVendors[i]
		if v.baseEnv != "" {
			v.base = liveSetting(v.baseEnv)
		}
		key := liveSetting(v.keyEnv)
		if key == "" || v.base == "" {
			t.Fatalf("%s names %s, but %s or its base URL is not configured", liveEnv, v.name, v.keyEnv)
		}
		v.keyEnv = key // from here on, the key itself
		liveKeys = append(liveKeys, key)
		out = append(out, v)
	}
	return out
}

// liveSetting is key from the environment, or else from the repo-root .env,
// two directories up from the package directory go test runs in. The file
// supplies only the settings this tier names, its values parsed as
// internal/modeltest parses its own — modeltest serves MODEL_* alone, so this
// tier reads its file itself, as webtooltest does.
func liveSetting(key string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return liveDotEnv()[key]
}

var liveDotEnv = sync.OnceValue(func() map[string]string {
	out := map[string]string{}
	f, err := os.Open(filepath.Join("..", "..", ".env")) // go test runs in the package directory
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || strings.HasPrefix(line, "#") || !slices.ContainsFunc(liveVendors, func(lv liveVendor) bool { return k == lv.keyEnv || k == lv.baseEnv }) {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
				v = v[1 : 1+end]
			}
		} else if i := strings.IndexFunc(v, func(r rune) bool { return r == '#' }); i > 0 && (v[i-1] == ' ' || v[i-1] == '\t') {
			v = strings.TrimSpace(v[:i]) // a trailing comment; a '#' inside the value is kept
		}
		out[k] = v
	}
	return out
})

// exchange is one request the gateway sent a vendor and what came back.
type exchange struct {
	mu     sync.Mutex
	path   string
	sent   []byte
	status int
	got    bytes.Buffer
}

func (x *exchange) answer() (int, []byte) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.status, bytes.Clone(x.got.Bytes())
}

// tee records an answer as it passes, before the gateway can read it.
type tee struct {
	http.ResponseWriter
	x *exchange
}

func (w tee) WriteHeader(code int) {
	w.x.mu.Lock()
	w.x.status = code
	w.x.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w tee) Write(p []byte) (int, error) {
	w.x.mu.Lock()
	w.x.got.Write(p)
	w.x.mu.Unlock()
	return w.ResponseWriter.Write(p)
}

func (w tee) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type recorder struct {
	mu  sync.Mutex
	log []*exchange
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.log)
}

func (r *recorder) since(n int) []*exchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.log[n:])
}

// recordingProxy forwards to base and records each exchange in rec.
func recordingProxy(t *testing.T, base string, rec *recorder) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	rp := &httputil.ReverseProxy{FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(u)
		pr.Out.Host = u.Host
		// Asked for plainly, the answer is recorded as text; the transport
		// still compresses on the wire.
		pr.Out.Header.Del("Accept-Encoding")
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		x := &exchange{path: r.URL.Path, sent: body.Bytes()}
		rec.mu.Lock()
		rec.log = append(rec.log, x)
		rec.mu.Unlock()
		r.Body = http.NoBody
		if body.Len() > 0 {
			r.Body = readCloser{bytes.NewReader(body.Bytes())}
		}
		rp.ServeHTTP(tee{w, x}, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

// vendorThinking is the thinking values of an answer as the vendor sent it,
// and whether a stream interleaved its thinking blocks (vendorAnswer).
func vendorThinking(raw []byte) (values []string, interleaved bool) {
	m, interleaved := vendorAnswer(raw)
	return provenance(&m), interleaved
}

// vendorAnswer is an answer as the vendor sent it, whole or streamed,
// assembled by the SDK's own types, and whether a stream interleaved its
// thinking blocks — a signature for one arriving after a later one started —
// which the gateway leaves unwrapped by design and expected does not model.
func vendorAnswer(raw []byte) (m anthropic.Message, interleaved bool) {
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		_ = json.Unmarshal(raw, &m)
		return m, false
	}
	last := int64(-1) // the thinking block that started last
	for _, line := range strings.Split(string(raw), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var ev anthropic.MessageStreamEventUnion
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "content_block_start" && strings.Contains(ev.ContentBlock.Type, "thinking"):
			last = ev.Index
		case ev.Type == "content_block_delta" && ev.Delta.Type == "signature_delta" && ev.Index != last:
			interleaved = true
		}
		_ = m.Accumulate(ev)
	}
	return m, interleaved
}

// sameUsage reports whether the SDK read the token counts the vendor
// reported, each assembled the same way.
func sameUsage(got, vendor anthropic.Usage) bool {
	return got.InputTokens == vendor.InputTokens && got.OutputTokens == vendor.OutputTokens &&
		got.CacheCreationInputTokens == vendor.CacheCreationInputTokens && got.CacheReadInputTokens == vendor.CacheReadInputTokens
}

// counts prints usage's token counts: input, output, cache write, cache read.
func counts(u anthropic.Usage) string {
	return fmt.Sprintf("%d/%d/%d/%d", u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens)
}

// expected is what the gateway must return for the thinking values the
// vendor sent, and what a continuation must send the vendor back: each value
// wrapped, in block order, until the first empty one, which — like every
// value after it, whose signature covers it — comes back as the vendor sent
// it and goes back to no upstream.
func expected(vend []string, dep string) (returned, sentBack []string) {
	ended := false
	for _, v := range vend {
		ended = ended || v == ""
		if ended {
			returned = append(returned, v)
			continue
		}
		returned = append(returned, "mapgw1."+dep+"."+v)
		sentBack = append(sentBack, v)
	}
	return returned, sentBack
}

// sentThinking is the thinking values a request carried.
func sentThinking(body []byte) []string {
	var c fakeCall
	_ = json.Unmarshal(body, &c.Body)
	return thinkingIn(c)
}

// liveAskText asks for the tier's one tool, liveTool.
const liveAskText = "What time is it now? Call the get_time tool first, then answer in one short sentence."

func liveAsk() anthropic.MessageParam {
	return anthropic.NewUserMessage(anthropic.NewTextBlock(liveAskText))
}

var liveTool = anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: "get_time",
	Description: anthropic.String("Returns the current time."), InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{}}}}

// liveCallTimeout bounds one live call; MiniMax-M3 has taken 142 seconds.
const liveCallTimeout = 3 * time.Minute

// liveTurn sends history to r's alias, whole or streamed, with thinking on as
// its vendor takes it and one tool offered.
func liveTurn(t *testing.T, cl *anthropic.Client, r liveRoute, history []anthropic.MessageParam, stream bool) (*anthropic.Message, error) {
	t.Helper()
	return liveSend(cl, anthropic.MessageNewParams{Model: anthropic.Model(r.alias), MaxTokens: 2048, Messages: history,
		Thinking: r.thinking, Tools: []anthropic.ToolUnionParam{liveTool}}, stream)
}

// liveSend sends p whole, or streamed and assembled by the SDK's Accumulate;
// a stream it leaves early is closed, and the vendor stops generating.
func liveSend(cl *anthropic.Client, p anthropic.MessageNewParams, stream bool) (*anthropic.Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	if !stream {
		return cl.Messages.New(ctx, p)
	}
	s := cl.Messages.NewStreaming(ctx, p)
	defer s.Close()
	var m anthropic.Message
	for s.Next() {
		if err := m.Accumulate(s.Current()); err != nil {
			return nil, err
		}
	}
	return &m, s.Err()
}

// liveText asks r's alias, whole or streamed, a question with a one-word
// answer and no tool offered: the answer comes back as text, under the
// alias, with the usage the vendor reported.
func liveText(t *testing.T, cl *anthropic.Client, r liveRoute, stream bool) {
	t.Helper()
	mode := map[bool]string{false: "whole", true: "streamed"}[stream]
	n := r.rec.count()
	m, err := liveSend(cl, anthropic.MessageNewParams{Model: anthropic.Model(r.alias), MaxTokens: 2048, Thinking: r.thinking,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Which planet is the largest in the solar system? Answer in one word."))}}, stream)
	if err != nil {
		liveFatalf(t, "%s text: %v", mode, err)
	}
	_, raw := one(t, r.rec, n).answer()
	up, _ := vendorAnswer(raw)
	if m.StopReason != anthropic.StopReasonEndTurn || !strings.Contains(strings.ToLower(textOf(m)), "jupiter") || string(m.Model) != r.alias ||
		m.Usage.InputTokens == 0 || m.Usage.OutputTokens == 0 || !sameUsage(m.Usage, up.Usage) {
		liveFatalf(t, "%s text: %q (stop %q) from model %q, usage %s, the vendor's %s", mode, masked(textOf(m)), m.StopReason, m.Model, counts(m.Usage), counts(up.Usage))
	}
}

// one is the single exchange a call made, or fails the test.
func one(t *testing.T, rec *recorder, n int) *exchange {
	t.Helper()
	xs := rec.since(n)
	if len(xs) != 1 {
		t.Fatalf("%d upstream calls, want 1", len(xs))
	}
	return xs[0]
}

type liveRoute struct {
	alias, dep string
	rec        *recorder
	thinking   anthropic.ThinkingConfigParamUnion
	thinks     bool // the model is expected to return signed thinking
}

// TestLiveThinkingRoundTrip runs, for every model of every named vendor,
// whole and streamed: a text answer; a thinking tool call whose signatures come back wrapped
// around the vendor's own values, then the continuation a client makes by
// returning the answer as the SDK gives it, which must send the vendor exactly
// its values; and one refusal relayed with the vendor's status. An alias whose
// first choice is down falls back to the first named vendor's first model,
// and the continuation goes straight to the deployment that produced its
// thinking. With both vendors named, a conversation crosses from DeepSeek to
// MiniMax and back, and each is sent only its own thinking; and a tool loop
// MiniMax opens, continued at DeepSeek, gets DeepSeek's refusal relayed. And the SDK lists,
// a page at a time, every chat alias a key may use and no other, and gets each.
func TestLiveThinkingRoundTrip(t *testing.T) {
	vendors := namedVendors(t)
	e := newEnv(t)
	routes := map[string][]liveRoute{}
	var fallback liveRoute
	down := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		writeBody(w, 503, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	})
	for _, v := range vendors {
		for _, model := range v.models {
			rec := &recorder{}
			p := e.provider(recordingProxy(t, v.base, rec), func(p *store.Provider) { p.Name, p.Profile = model, v.name })
			e.credential(p, v.keyEnv, 1)
			d := e.deployment(p, model)
			e.alias(model, target(d, 0))
			thinks := !slices.Contains(v.quiet, model)
			routes[v.name] = append(routes[v.name], liveRoute{model, d.ID, rec, v.thinking, thinks})
			// The fallback's continuation finds its way back by its thinking,
			// so it falls back to a model that thinks.
			if fallback.alias == "" && thinks {
				dp := e.provider(down.URL, func(p *store.Provider) { p.Name = "down" })
				e.credential(dp, "sk-down-key1", 1)
				e.alias("fallback", target(e.deployment(dp, "down"), 0), target(d, 1))
				fallback = liveRoute{"fallback", d.ID, rec, v.thinking, true}
			}
		}
	}
	// The list: the chat aliases, an embedding alias beside them, and a key
	// that may use every alias but the first.
	var chat []string
	for _, v := range vendors {
		for _, r := range routes[v.name] {
			chat = append(chat, r.alias)
		}
	}
	if fallback.alias != "" {
		chat = append(chat, fallback.alias)
	}
	vp := e.provider(down.URL, func(p *store.Provider) { p.Name = "vectors" })
	e.credential(vp, "sk-vectors-key1", 1)
	e.alias("vectors", target(e.deployment(vp, "vectors", func(d *store.Deployment) { d.Kind = store.KindEmbedding }), 0))
	narrow := e.key(append(slices.Clone(chat[1:]), "vectors"))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	t.Run("model list", func(t *testing.T) {
		want := slices.Clone(chat[1:])
		slices.Sort(want)
		nc := e.client(narrow)
		pager := nc.Models.ListAutoPaging(liveCtx(t), anthropic.ModelListParams{Limit: anthropic.Int(1)})
		var got []string
		for pager.Next() {
			m := pager.Current()
			got = append(got, m.ID)
			for name, ok := range map[string]bool{"id": m.JSON.ID.Valid(), "capabilities": m.JSON.Capabilities.Valid(),
				"created_at": m.JSON.CreatedAt.Valid(), "display_name": m.JSON.DisplayName.Valid(),
				"max_input_tokens": m.JSON.MaxInputTokens.Valid(), "max_tokens": m.JSON.MaxTokens.Valid()} {
				if !ok {
					t.Errorf("%s: %s missing", m.ID, name)
				}
			}
		}
		if err := pager.Err(); err != nil || !slices.Equal(got, want) {
			t.Fatalf("listed %q (%v), want %q", got, err, want)
		}
		if page, err := nc.Models.List(liveCtx(t), anthropic.ModelListParams{Limit: anthropic.Int(1)}); err != nil ||
			len(page.Data) != 1 || page.Data[0].ID != want[0] || !page.HasMore || page.LastID != want[0] {
			t.Errorf("the first page: %+v, %v", page, err)
		}
		for _, id := range want {
			if m, err := nc.Models.Get(liveCtx(t), id, anthropic.ModelGetParams{}); err != nil || m.ID != id {
				t.Errorf("get %s: %v", id, err)
			}
		}
		for _, id := range []string{chat[0], "vectors", "nope"} {
			_, err := nc.Models.Get(liveCtx(t), id, anthropic.ModelGetParams{})
			var apiErr *anthropic.Error
			if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
				t.Errorf("get %s: %v, want a 404", id, err)
			}
		}
	})

	t.Run("models", func(t *testing.T) {
		for _, v := range vendors {
			for _, r := range routes[v.name] {
				t.Run(r.alias, func(t *testing.T) {
					t.Parallel()
					for _, stream := range []bool{false, true} {
						liveText(t, cl, r, stream)
						liveRoundTrip(t, cl, e.s, r, stream)
					}
					n := r.rec.count()
					_, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: anthropic.Model(r.alias), MaxTokens: 64})
					status, _ := one(t, r.rec, n).answer()
					var apiErr *anthropic.Error
					if !errors.As(err, &apiErr) || apiErr.StatusCode != status || status < 400 || status >= 500 {
						liveErrorf(t, "a request with no messages: error %v, the vendor's status %d", err, status)
					}
				})
			}
		}
	})

	t.Run("fallback", func(t *testing.T) {
		tries := liveRoundTrip(t, cl, e.s, fallback, false)
		if n := len(down.recorded()); n != tries {
			liveErrorf(t, "the first choice was called %d times, want %d: once by each first turn, for its one credential, and never by the continuation", n, tries)
		}
	})

	if len(routes["deepseek"]) == 0 || len(routes["minimax"]) == 0 {
		return
	}
	t.Run("across vendors", func(t *testing.T) {
		ds, mm := routes["deepseek"][0], routes["minimax"][0]
		n := ds.rec.count()
		m1, err := liveTurn(t, cl, ds, []anthropic.MessageParam{liveAsk()}, false)
		if err != nil {
			liveFatalf(t, "%v", err)
		}
		_, raw := one(t, ds.rec, n).answer()
		vend, _ := vendorThinking(raw)
		_, own := expected(vend, ds.dep)
		// MiniMax closes the tool loop it may start before the conversation
		// returns: DeepSeek refuses a loop that reaches it without its thinking
		// under another vendor's tool ids, and the gateway, sending each vendor
		// only its own thinking, leaves that refusal to fail like any other
		// (plan 59, "Retry and fallback happen before the first byte only").
		h := next([]anthropic.MessageParam{liveAsk()}, m1)
		var mmOwn []string
		for calls := 0; ; calls++ {
			if calls == 3 {
				liveFatalf(t, "MiniMax called the tool three times without answering")
			}
			n = mm.rec.count()
			m2, err := liveTurn(t, cl, mm, h, false)
			if err != nil {
				liveFatalf(t, "%v", err)
			}
			x := one(t, mm.rec, n)
			if sent := sentThinking(x.sent); !slices.Equal(sent, mmOwn) {
				liveErrorf(t, "MiniMax was sent %q, want only its own %q", abbreviate(sent), abbreviate(mmOwn))
			}
			_, raw := x.answer()
			vend, _ := vendorThinking(raw)
			_, back := expected(vend, mm.dep)
			mmOwn = append(mmOwn, back...)
			h = next(h, m2)
			if !hasToolUse(m2) {
				break
			}
		}
		n = ds.rec.count()
		if _, err := liveTurn(t, cl, ds, h, false); err != nil {
			liveFatalf(t, "%v", err)
		}
		if sent := sentThinking(one(t, ds.rec, n).sent); !slices.Equal(sent, own) {
			liveErrorf(t, "back at DeepSeek, sent %d values, want its own %d", len(sent), len(own))
		}
	})

	t.Run("an open loop across vendors", func(t *testing.T) {
		// A loop MiniMax opens carries MiniMax's tool ids and none of
		// DeepSeek's thinking, which DeepSeek refuses; the gateway relays the
		// refusal as DeepSeek gave it.
		ds, mm := routes["deepseek"][0], routes["minimax"][0]
		var m1 *anthropic.Message
		for try := 0; try < 3 && !hasToolUse(m1); try++ {
			var err error
			if m1, err = liveTurn(t, cl, mm, []anthropic.MessageParam{liveAsk()}, false); err != nil {
				liveFatalf(t, "%v", err)
			}
		}
		if !hasToolUse(m1) {
			liveFatalf(t, "MiniMax answered three times without calling the tool")
		}
		n := ds.rec.count()
		_, err := liveTurn(t, cl, ds, next([]anthropic.MessageParam{liveAsk()}, m1), false)
		xs := ds.rec.since(n)
		if len(xs) != 1 {
			liveFatalf(t, "DeepSeek was called %d times, want once: %v", len(xs), err)
		}
		vs, raw := xs[0].answer()
		var apiErr *anthropic.Error
		if vs != 400 || !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || !strings.Contains(err.Error(), "must be passed back") {
			liveFatalf(t, "the open loop: %v; DeepSeek answered %d %s; want its 400 relayed", err, vs, masked(string(raw)))
		}
	})
}

// liveRoundTrip returns how many times it asked the first turn.
func liveRoundTrip(t *testing.T, cl *anthropic.Client, s *store.Store, r liveRoute, stream bool) (tries int) {
	t.Helper()
	mode := map[bool]string{false: "whole", true: "streamed"}[stream]
	// A model thinking adaptively decides for itself whether the question
	// deserves thinking, which is not what this checks, so a model expected
	// to think is asked up to three times for an answer that carries some.
	var m *anthropic.Message
	var up anthropic.Message
	var vend []string
	var interleaved bool
	for tries < 3 {
		tries++
		n := r.rec.count()
		var err error
		if m, err = liveTurn(t, cl, r, []anthropic.MessageParam{liveAsk()}, stream); err != nil {
			liveFatalf(t, "%s: %v", mode, err)
		}
		_, raw := one(t, r.rec, n).answer()
		up, interleaved = vendorAnswer(raw)
		vend = provenance(&up)
		if _, back := expected(vend, r.dep); len(back) > 0 || !r.thinks {
			break
		}
	}
	if interleaved {
		liveFatalf(t, "%s: the vendor interleaved its thinking blocks, which this tier's oracle does not model", mode)
	}
	want, back := expected(vend, r.dep)
	if len(back) == 0 && r.thinks {
		liveFatalf(t, "%s: the vendor returned no signed thinking in %d answers (%q), so there is nothing to round-trip", mode, tries, abbreviate(vend))
	}
	if got := provenance(m); !slices.Equal(got, want) || string(m.Model) != r.alias || m.Usage.OutputTokens == 0 {
		liveFatalf(t, "%s: model %q, %d output tokens, thinking %q, want %q", mode, m.Model, m.Usage.OutputTokens, abbreviate(got), abbreviate(want))
	}
	if m.StopReason != anthropic.StopReasonToolUse {
		liveFatalf(t, "%s: the model answered without calling the tool (stop %q)", mode, m.StopReason)
	}
	if !sameUsage(m.Usage, up.Usage) {
		liveFatalf(t, "%s: the SDK read usage %s, the vendor reported %s", mode, counts(m.Usage), counts(up.Usage))
	}
	// The ledger holds what the caller's SDK added up, written before the
	// answer ended.
	rows, _, err := s.ListUsage(context.Background(), store.UsageFilter{Alias: r.alias}, 0, 1)
	sdk := store.Tokens{Input: m.Usage.InputTokens, Output: m.Usage.OutputTokens,
		CacheWrite: m.Usage.CacheCreationInputTokens, CacheRead: m.Usage.CacheReadInputTokens}
	if err != nil || len(rows) != 1 || rows[0].Tokens == nil || *rows[0].Tokens != sdk || rows[0].Status != 200 ||
		rows[0].DeploymentID != r.dep || stream != (rows[0].TTFT > 0) {
		liveFatalf(t, "%s: the ledger holds %+v (%v), want a 200 from %s with the SDK's %+v", mode, rows, err, r.dep, sdk)
	}
	n := r.rec.count()
	if _, err := liveTurn(t, cl, r, next([]anthropic.MessageParam{liveAsk()}, m), stream); err != nil {
		liveFatalf(t, "%s continuation: %v", mode, err)
	}
	if sent := sentThinking(one(t, r.rec, n).sent); !slices.Equal(sent, back) {
		liveFatalf(t, "%s continuation sent %q, want the vendor's own %q", mode, abbreviate(sent), abbreviate(back))
	}
	t.Logf("%s: %d of %d thinking block(s) round-tripped, from answer %d", mode, len(back), len(want), tries)
	return tries
}

// liveCtx bounds one live call whose answer is read whole, and is
// cancelled with the test.
func liveCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	t.Cleanup(cancel)
	return ctx
}

// textOf is an answer's text, its blocks joined; empty for the nil answer
// an error leaves.
func textOf(m *anthropic.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// liveFatalf and liveErrorf fail with a message that prints no key: a
// vendor's output can echo one anywhere — a value, a stop reason, an error.
func liveFatalf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatal(masked(fmt.Sprintf(format, args...)))
}

func liveErrorf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Error(masked(fmt.Sprintf(format, args...)))
}

func masked(s string) string {
	for _, k := range liveKeys {
		s = strings.ReplaceAll(s, k, "***")
	}
	return s
}

// abbreviate keeps a failure readable: signatures run to kilobytes. Each
// value is masked before it is cut, so a cut cannot leave part of a key.
func abbreviate(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		v = masked(v)
		if len(v) > 48 {
			v = fmt.Sprintf("%s…(%d)", v[:48], len(v))
		}
		out[i] = v
	}
	return out
}
