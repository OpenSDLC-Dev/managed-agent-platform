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
	name    string // the profile, and the name RUN_LIVE_MODELGATEWAY uses
	keyEnv  string
	baseEnv string // configures the base URL where the vendor has more than one
	base    string
	models  []string
}

var liveVendors = []liveVendor{
	{name: "deepseek", keyEnv: "DEEPSEEK_API_KEY", base: "https://api.deepseek.com/anthropic",
		models: []string{"deepseek-flash", "deepseek-v4-pro"}},
	// A MiniMax key works on its own region's host only.
	{name: "minimax", keyEnv: "MINIMAX_API_KEY", baseEnv: "MINIMAX_BASE_URL",
		models: []string{"MiniMax-M3", "MiniMax-M3.1-Flash-Preview"}},
}

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
		out = append(out, v)
	}
	return out
}

// liveSetting is key from the environment, or else from the repo-root .env,
// read as internal/modeltest reads its own keys (webtooltest mirrors it the
// same way, for the same reason: modeltest serves MODEL_* alone).
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
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
				v = v[1 : 1+end]
			}
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
})

// exchange is one request the gateway sent a vendor and what came back.
type exchange struct {
	mu     sync.Mutex
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
		x := &exchange{sent: body.Bytes()}
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
// whole or streamed, assembled by the SDK's own types.
func vendorThinking(raw []byte) []string {
	var m anthropic.Message
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		_ = json.Unmarshal(raw, &m)
		return provenance(&m)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var ev anthropic.MessageStreamEventUnion
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &ev) == nil {
			_ = m.Accumulate(ev)
		}
	}
	return provenance(&m)
}

// sentThinking is the thinking values a request carried.
func sentThinking(body []byte) []string {
	var c fakeCall
	_ = json.Unmarshal(body, &c.Body)
	return thinkingIn(c)
}

func liveAsk() anthropic.MessageParam {
	return anthropic.NewUserMessage(anthropic.NewTextBlock("What time is it now? Call the get_time tool first, then answer in one short sentence."))
}

// liveTurn sends history to alias, whole or streamed, with thinking on and
// one tool offered.
func liveTurn(t *testing.T, cl *anthropic.Client, alias string, history []anthropic.MessageParam, stream bool) (*anthropic.Message, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p := anthropic.MessageNewParams{Model: anthropic.Model(alias), MaxTokens: 2048, Messages: history,
		Thinking: anthropic.ThinkingConfigParamOfEnabled(1024),
		Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{Name: "get_time",
			Description: anthropic.String("Returns the current time."), InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{}}}}}}
	if !stream {
		return cl.Messages.New(ctx, p)
	}
	s := cl.Messages.NewStreaming(ctx, p)
	var m anthropic.Message
	for s.Next() {
		if err := m.Accumulate(s.Current()); err != nil {
			return nil, err
		}
	}
	return &m, s.Err()
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
}

// TestLiveThinkingRoundTrip runs, for every model of every named vendor,
// whole and streamed: a thinking tool call whose signatures come back wrapped
// around the vendor's own values, then the continuation a client makes by
// returning the answer as the SDK gives it, which must send the vendor exactly
// its values; and one refusal relayed with the vendor's status. With both
// vendors named, a conversation crosses from DeepSeek to MiniMax and back, and
// each is sent only its own thinking.
func TestLiveThinkingRoundTrip(t *testing.T) {
	vendors := namedVendors(t)
	e := newEnv(t)
	routes := map[string][]liveRoute{}
	for _, v := range vendors {
		for _, model := range v.models {
			rec := &recorder{}
			p := e.provider(recordingProxy(t, v.base, rec), func(p *store.Provider) { p.Name, p.Profile = model, v.name })
			e.credential(p, v.keyEnv, 1)
			d := e.deployment(p, model)
			e.alias(model, target(d, 0))
			routes[v.name] = append(routes[v.name], liveRoute{model, d.ID, rec})
		}
	}
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	t.Run("models", func(t *testing.T) {
		for _, v := range vendors {
			for _, r := range routes[v.name] {
				t.Run(r.alias, func(t *testing.T) {
					t.Parallel()
					for _, stream := range []bool{false, true} {
						liveRoundTrip(t, cl, r, stream)
					}
					n := r.rec.count()
					_, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: anthropic.Model(r.alias), MaxTokens: 64})
					status, _ := one(t, r.rec, n).answer()
					var apiErr *anthropic.Error
					if !errors.As(err, &apiErr) || apiErr.StatusCode != status || status < 400 || status >= 500 {
						t.Errorf("a request with no messages: error %v, the vendor's status %d", err, status)
					}
				})
			}
		}
	})

	if len(routes["deepseek"]) == 0 || len(routes["minimax"]) == 0 {
		return
	}
	t.Run("across vendors", func(t *testing.T) {
		ds, mm := routes["deepseek"][0], routes["minimax"][0]
		n := ds.rec.count()
		m1, err := liveTurn(t, cl, ds.alias, []anthropic.MessageParam{liveAsk()}, false)
		if err != nil {
			t.Fatal(err)
		}
		_, raw := one(t, ds.rec, n).answer()
		own := nonEmpty(vendorThinking(raw))
		h := next([]anthropic.MessageParam{liveAsk()}, m1)
		n = mm.rec.count()
		m2, err := liveTurn(t, cl, mm.alias, h, false)
		if err != nil {
			t.Fatal(err)
		}
		if sent := sentThinking(one(t, mm.rec, n).sent); len(sent) != 0 {
			t.Errorf("MiniMax was sent DeepSeek's thinking: %q", sent)
		}
		h = append(h, m2.ToParam(), anthropic.NewUserMessage(anthropic.NewTextBlock("Thanks. Call get_time once more.")))
		n = ds.rec.count()
		if _, err := liveTurn(t, cl, ds.alias, h, false); err != nil {
			t.Fatal(err)
		}
		if sent := sentThinking(one(t, ds.rec, n).sent); !slices.Equal(sent, own) {
			t.Errorf("back at DeepSeek, sent %d values, want its own %d", len(sent), len(own))
		}
	})
}

func liveRoundTrip(t *testing.T, cl *anthropic.Client, r liveRoute, stream bool) {
	t.Helper()
	mode := map[bool]string{false: "whole", true: "streamed"}[stream]
	n := r.rec.count()
	m, err := liveTurn(t, cl, r.alias, []anthropic.MessageParam{liveAsk()}, stream)
	if err != nil {
		t.Fatalf("%s: %v", mode, err)
	}
	_, raw := one(t, r.rec, n).answer()
	vend := vendorThinking(raw)
	var want []string
	for _, v := range vend {
		if v != "" {
			v = "mapgw1." + r.dep + "." + v
		}
		want = append(want, v)
	}
	if got := provenance(m); !slices.Equal(got, want) || string(m.Model) != r.alias || m.Usage.OutputTokens == 0 {
		t.Fatalf("%s: model %q, %d output tokens, thinking %q, want %q", mode, m.Model, m.Usage.OutputTokens, abbreviate(got), abbreviate(want))
	}
	if m.StopReason != anthropic.StopReasonToolUse {
		t.Fatalf("%s: the model answered without calling the tool (stop %q)", mode, m.StopReason)
	}
	n = r.rec.count()
	if _, err := liveTurn(t, cl, r.alias, next([]anthropic.MessageParam{liveAsk()}, m), stream); err != nil {
		t.Fatalf("%s continuation: %v", mode, err)
	}
	if sent := sentThinking(one(t, r.rec, n).sent); !slices.Equal(sent, nonEmpty(vend)) {
		t.Fatalf("%s continuation sent %q, want the vendor's own %q", mode, abbreviate(sent), abbreviate(nonEmpty(vend)))
	}
	t.Logf("%s: %d thinking block(s) round-tripped", mode, len(want))
}

func nonEmpty(vs []string) []string {
	return slices.DeleteFunc(slices.Clone(vs), func(v string) bool { return v == "" })
}

// abbreviate keeps a failure readable: signatures run to kilobytes.
func abbreviate(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		if len(v) > 48 {
			v = fmt.Sprintf("%s…(%d)", v[:48], len(v))
		}
		out[i] = v
	}
	return out
}
