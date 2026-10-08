package modelgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/openai/openai-go/v3"
	"go.opentelemetry.io/otel/trace"
)

// dikw-core's three request bodies (docs/plan/59_model-gateway.md, Ground
// truth): text embeddings as the openai Python SDK sends them, with
// dimensions and its base64 default; Gitee's multimodal objects; and a
// rerank batch scoring every document.
const (
	dikwText       = `{"input":["Jupiter is the largest planet.","Octopuses have three hearts."],"model":"text","dimensions":1024,"encoding_format":"base64"}`
	dikwMultimodal = `{"model":"vision","input":[{"text":"a red square"},{"image":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}]}`
	dikwRerank     = `{"model":"ranker","query":"Which planet is the largest? <b>&</b>","documents":["Octopuses have three hearts.","Jupiter is the <i>largest</i> planet.","Paris is in France."],"top_n":3}`
)

// vectors answers an embeddings request: base64 vectors where it asked for
// them and floats otherwise, with usage unless its input holds objects, as
// dikw-core found Gitee's multimodal answers; and a rerank request in
// Gitee's shape, its usage in camelCase.
func vectors(w http.ResponseWriter, _ *http.Request, c fakeCall) {
	if c.Path == "/rerank" {
		writeBody(w, 200, fmt.Sprintf(`{"model":%q,"usage":{"totalTokens":0,"promptTokens":0},"results":[{"index":1,"document":{"text":"Jupiter is the <i>largest</i> planet."},"relevance_score":0.9982522142273162},{"index":0,"document":{"text":"Octopuses have three hearts."},"relevance_score":1.7366719305164412E-5}]}`, c.Model))
		return
	}
	data := `[{"object":"embedding","embedding":[0.5,1.7366719305164412E-5,-2],"index":0},{"object":"embedding","embedding":[-0.25,0,1e-7],"index":1}]`
	if string(c.Body["encoding_format"]) == `"base64"` {
		data = `[{"object":"embedding","embedding":"AAAAPwAAgD8AAADA","index":0},{"object":"embedding","embedding":"AACAvgAAAAA=","index":1}]`
	}
	usage := `,"usage":{"prompt_tokens":13,"total_tokens":13}`
	if bytes.HasPrefix(c.Body["input"], []byte(`[{`)) {
		usage = ""
	}
	writeBody(w, 200, fmt.Sprintf(`{"object":"list","data":%s,"model":%q%s}`, data, c.Model, usage))
}

// row is the ledger's row for a request id.
func (e *env) row(rid string) store.Usage {
	e.t.Helper()
	for _, u := range e.ledger() {
		if u.RequestID == rid {
			return u
		}
	}
	e.t.Fatalf("no ledger row for %q", rid)
	return store.Usage{}
}

// Embeddings and rerank pass through on their own routes, at the root and
// under /openai: the body goes upstream as sent but for model, the answer
// comes back as sent but for model, its vectors untouched whether float or
// base64, and the ledger counts the usage the upstream reported — none where
// it reported none, or reported it in a shape no OpenAI usage takes.
func TestEmbeddingsAndRerankPassThrough(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, vectors)
	p := onOpenAI(e, "gitee", f.URL)
	embedding := func(d *store.Deployment) { d.Kind = store.KindEmbedding }
	e.alias("text", target(e.deployment(p, "Qwen3-Embedding-8B", embedding), 0))
	e.alias("vision", target(e.deployment(p, "Qwen3-VL-Embedding-8B", embedding), 0))
	e.alias("ranker", target(e.deployment(p, "bge-reranker-v2-m3", func(d *store.Deployment) { d.Kind = store.KindRerank }), 0))
	key := e.key(everyAlias)
	e.start()

	float := strings.Replace(dikwText, `"base64"`, `"float"`, 1)
	for _, tc := range []struct {
		name, path, body, upstreamPath, upstreamModel, endpoint string
		tokens                                                  *store.Tokens
	}{
		{"base64", "/v1/embeddings", dikwText, "/embeddings", "Qwen3-Embedding-8B", "embeddings", &store.Tokens{Input: 13}},
		{"float", "/openai/v1/embeddings", float, "/embeddings", "Qwen3-Embedding-8B", "embeddings", &store.Tokens{Input: 13}},
		{"multimodal", "/v1/embeddings", dikwMultimodal, "/embeddings", "Qwen3-VL-Embedding-8B", "embeddings", nil},
		{"rerank", "/v1/rerank", dikwRerank, "/rerank", "bge-reranker-v2-m3", "rerank", nil},
		{"rerank, prefixed", "/openai/v1/rerank", dikwRerank, "/rerank", "bge-reranker-v2-m3", "rerank", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := len(f.recorded())
			resp, b := e.do("POST", tc.path, tc.body, map[string]string{"Authorization": "Bearer " + key, "anthropic-version": "2023-06-01", "X-Caller": "app"})
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("%d %s: %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
			}
			calls := f.recorded()[n:]
			if len(calls) != 1 {
				t.Fatalf("%d upstream calls", len(calls))
			}
			up := calls[0]
			if up.Path != tc.upstreamPath || up.Model != tc.upstreamModel || up.Header.Get("Authorization") != "Bearer sk-gitee-key1" ||
				up.Header.Get("anthropic-version") != "" || up.Header.Get("X-Caller") != "" {
				t.Fatalf("upstream got %s, model %q, headers %v", up.Path, up.Model, up.Header)
			}
			var sent map[string]json.RawMessage
			_ = json.Unmarshal([]byte(tc.body), &sent)
			if len(up.Body) != len(sent) {
				t.Errorf("upstream got %s for %s", up.Raw, tc.body)
			}
			for k, v := range sent {
				if k != "model" && !bytes.Equal(up.Body[k], v) {
					t.Errorf("upstream got %s = %s, sent %s", k, up.Body[k], v)
				}
			}
			// The answer is the upstream's, member for member, but for model.
			var got, want map[string]json.RawMessage
			_ = json.Unmarshal(b, &got)
			upstream := httptest.NewRecorder()
			vectors(upstream, nil, up)
			_ = json.Unmarshal(upstream.Body.Bytes(), &want)
			if !bytes.Equal(got["model"], sent["model"]) || len(got) != len(want) {
				t.Errorf("answered %s for %s", b, upstream.Body.Bytes())
			}
			for k, v := range want {
				if k != "model" && !bytes.Equal(got[k], v) {
					t.Errorf("answered %s = %s, upstream sent %s", k, got[k], v)
				}
			}
			u := e.row(resp.Header.Get("request-id"))
			if u.Protocol != "openai" || u.Endpoint != tc.endpoint || u.Status != 200 || !reflect.DeepEqual(u.Tokens, tc.tokens) {
				t.Errorf("ledger %+v, tokens %+v, want %s and %+v", u, u.Tokens, tc.endpoint, tc.tokens)
			}
		})
	}

	// openai-go reads the float answer as any OpenAI caller would.
	cl := e.oaClient(key, "/v1")
	res, err := cl.Embeddings.New(context.Background(), openai.EmbeddingNewParams{Model: "text", Dimensions: openai.Int(1024),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: []string{"Jupiter is the largest planet.", "Octopuses have three hearts."}}})
	if err != nil || res.Model != "text" || len(res.Data) != 2 || !reflect.DeepEqual(res.Data[0].Embedding, []float64{0.5, 1.7366719305164412e-05, -2}) ||
		res.Usage.PromptTokens != 13 {
		t.Fatalf("openai-go: %v %+v", err, res)
	}
}

// Each route serves its own kind of alias and no other, and only on the
// OpenAI protocol: no caller reaches an upstream through a route whose
// answer it could not read. Neither API streams, so a stream asked for is
// refused, as is a stream flag the gateway cannot read: an upstream that
// streamed would answer in a shape whose usage goes uncounted. What else a
// chat request must get right — stream_options, the fields a chat vendor
// ignores — is the upstream's business here.
func TestEmbeddingsAndRerankKeepToTheirKind(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, vectors)
	other := newFake(t, chatAnswer("Jupiter", deepseekStyle))
	p := onOpenAI(e, "gitee", f.URL)
	e.alias("text", target(e.deployment(p, "Qwen3-Embedding-8B", func(d *store.Deployment) { d.Kind = store.KindEmbedding }), 0))
	e.alias("ranker", target(e.deployment(p, "bge-reranker-v2-m3", func(d *store.Deployment) { d.Kind = store.KindRerank }), 0))
	e.alias("talk", target(e.deployment(onOpenAI(e, "openai-generic", other.URL), "up"), 0))
	e.alias("anthropic-vectors", target(e.deployment(onProfile(e, "anthropic-generic", f.URL), "emb", func(d *store.Deployment) { d.Kind = store.KindEmbedding }), 0))
	mm := newFake(t, vectors)
	e.alias("minimax-vectors", target(e.deployment(onOpenAI(e, "minimax", mm.URL), "emb", func(d *store.Deployment) { d.Kind = store.KindEmbedding }), 0))
	key := e.key(everyAlias)
	e.start()
	bearer := map[string]string{"Authorization": "Bearer " + key}

	for _, tc := range []struct {
		method, path, body string
		status             int
		openAI             bool
		msg                string
	}{
		{"POST", "/v1/embeddings", `{"model":"talk","input":"x"}`, 400, true, "model: talk serves chat, not embedding"},
		{"POST", "/v1/embeddings", `{"model":"ranker","input":"x"}`, 400, true, "model: ranker serves rerank, not embedding"},
		{"POST", "/v1/rerank", `{"model":"text","query":"q","documents":["a"]}`, 400, true, "model: text serves embedding, not rerank"},
		{"POST", "/v1/chat/completions", `{"model":"ranker","messages":[]}`, 400, true, "model: ranker serves rerank, not chat"},
		{"POST", "/v1/messages", `{"model":"text","max_tokens":1,"messages":[]}`, 400, false, "model: text serves embedding, not chat"},
		{"POST", "/v1/embeddings", `{"model":"nope","input":"x"}`, 404, true, "model: nope"},
		{"POST", "/v1/embeddings", `{"input":"x"}`, 400, true, "model: Field required"},
		{"POST", "/v1/embeddings", `{"model":"anthropic-vectors","input":"x"}`, 503, true, "no enabled upstream on the OpenAI protocol"},
		{"GET", "/v1/embeddings", "", 405, true, "POST"},
		{"GET", "/openai/v1/rerank", "", 405, true, "POST"},
		{"POST", "/anthropic/v1/embeddings", `{"model":"text","input":"x"}`, 404, false, "no such path"},
		{"POST", "/anthropic/v1/rerank", `{"model":"ranker","query":"q","documents":["a"]}`, 404, false, "no such path"},
		{"POST", "/v1/embeddings", `{"model":"text","input":"x","stream":true}`, 400, true, "stream: /v1/embeddings does not stream"},
		{"POST", "/openai/v1/rerank", `{"model":"ranker","query":"q","documents":["a"],"stream":true}`, 400, true, "stream: /v1/rerank does not stream"},
		{"POST", "/v1/embeddings", `{"model":"text","input":"x","stream":"yes"}`, 400, true, "stream: must be a boolean"},
		{"POST", "/v1/embeddings", `{"model":"text","input":"x","Stream":true}`, 400, true, "Stream: the field is stream"},
	} {
		resp, b := e.do(tc.method, tc.path, tc.body, bearer)
		if resp.StatusCode != tc.status || !strings.Contains(string(b), tc.msg) {
			t.Errorf("%s %s %s: %d %s, want %d %q", tc.method, tc.path, tc.body, resp.StatusCode, b, tc.status, tc.msg)
		}
		if _, _, ok := openAIEnvelope(b); ok != tc.openAI {
			t.Errorf("%s %s: answered %s, in OpenAI's envelope %t", tc.method, tc.path, b, ok)
		}
	}
	if n := len(f.recorded()) + len(other.recorded()) + len(mm.recorded()); n != 0 {
		t.Fatalf("%d upstream calls for requests the gateway refuses", n)
	}

	// What would fail a chat request goes upstream as sent. A key that
	// differs from model only in case goes before model, so a decoder that
	// matches keys regardless of case, keeping the last, reads the
	// deployment's id rather than the caller's.
	body := `{"model":"text","input":"x","stream":false,"stream_options":5,"Model":"other"}`
	if resp, b := e.do("POST", "/v1/embeddings", body, bearer); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if calls := f.recorded(); len(calls) != 1 || string(calls[0].Body["stream"]) != "false" || string(calls[0].Body["stream_options"]) != "5" ||
		bytes.LastIndex(calls[0].Raw, []byte(`"model":"Qwen3-Embedding-8B"`)) < bytes.Index(calls[0].Raw, []byte(`"Model":"other"`)) {
		t.Fatalf("upstream got %s", calls[0].Raw)
	}
	// What MiniMax's chat ignores refuses no embeddings request.
	body = `{"model":"minimax-vectors","input":"x","parallel_tool_calls":false,"stop":["x"],"tool_choice":"required"}`
	if resp, b := e.do("POST", "/v1/embeddings", body, bearer); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if calls := mm.recorded(); len(calls) != 1 || string(calls[0].Body["parallel_tool_calls"]) != "false" {
		t.Fatalf("upstream got %v", calls)
	}
}

// An upstream's batch cap answers in the upstream's own words — its status
// and body, the call's credential removed — and is not tried again: the
// gateway splits no batch.
func TestAnUpstreamsBatchCapIsRelayed(t *testing.T) {
	e := newEnv(t)
	refusal := `{"error":{"code":"400","message":"无效的参数数组长度: 'documents' 长度是 '1' 到 '25' (sk-gitee-key1)","type":"server_error"}}`
	f := newFake(t, status(400, refusal))
	e.alias("ranker", target(e.deployment(onOpenAI(e, "gitee", f.URL), "bge-reranker-v2-m3", func(d *store.Deployment) { d.Kind = store.KindRerank }), 0))
	key := e.key(everyAlias)
	e.start()
	docs, _ := json.Marshal(make([]string, 26))
	resp, b := e.do("POST", "/v1/rerank", fmt.Sprintf(`{"model":"ranker","query":"q","documents":%s}`, docs), map[string]string{"Authorization": "Bearer " + key})
	if want := strings.Replace(refusal, "sk-gitee-key1", "[REDACTED]", 1); resp.StatusCode != 400 || len(f.recorded()) != 1 ||
		strings.Contains(string(b), "sk-gitee-key1") || !strings.Contains(string(b), "'documents' 长度是 '1' 到 '25'") {
		t.Fatalf("%d after %d calls: %s, want the upstream's %s", resp.StatusCode, len(f.recorded()), b, want)
	}
	if u := e.row(resp.Header.Get("request-id")); u.Endpoint != "rerank" || u.Status != 400 || u.Tokens != nil {
		t.Errorf("ledger %+v", u)
	}
}

// connections records the client connection each request arrived on.
type connections struct {
	*httptest.Server
	mu     sync.Mutex
	remote []string
	closed []bool
}

func newConnections(t *testing.T) *connections {
	c := &connections{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.remote, c.closed = append(c.remote, r.RemoteAddr), append(c.closed, r.Close)
		c.mu.Unlock()
		vectors(w, r, fakeCall{Path: r.URL.Path, Model: "m"})
	}))
	t.Cleanup(c.Close)
	return c
}

// Gitee drops idle connections mid-batch, so each request to it goes on a
// connection no other request has used: not one of its own, nor one another
// provider on the same host left idle. That provider's are reused.
func TestGiteeIsSentEachRequestOnAFreshConnection(t *testing.T) {
	e := newEnv(t)
	host := newConnections(t)
	embedding := func(d *store.Deployment) { d.Kind = store.KindEmbedding }
	e.alias("generic", target(e.deployment(onOpenAI(e, "openai-generic", host.URL), "m", embedding), 0))
	e.alias("gitee", target(e.deployment(onOpenAI(e, "gitee", host.URL), "m", embedding), 0))
	key := e.key(everyAlias)
	e.start()
	for range 3 {
		for _, alias := range []string{"generic", "gitee"} {
			if resp, b := e.do("POST", "/v1/embeddings", fmt.Sprintf(`{"model":%q,"input":"x"}`, alias), map[string]string{"Authorization": "Bearer " + key}); resp.StatusCode != 200 {
				t.Fatalf("%s: %d %s", alias, resp.StatusCode, b)
			}
		}
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	seen := map[string]int{}
	for i, remote := range host.remote {
		seen[remote]++
		if gitee := i%2 == 1; host.closed[i] != gitee {
			t.Errorf("request %d (gitee %t) asked to close its connection: %t", i, gitee, host.closed[i])
		}
	}
	// The generic provider's three requests share one connection; each of
	// Gitee's has one to itself.
	if len(host.remote) != 6 || len(seen) != 4 || seen[host.remote[0]] != 3 {
		t.Errorf("6 requests on connections %v", host.remote)
	}
}

// An embedding and a rerank are traced as their own operations, by route.
func TestEmbeddingsAndRerankAreTracedAsTheirOperations(t *testing.T) {
	rec, _ := observed(t)
	e := newEnv(t)
	f := newFake(t, vectors)
	p := onOpenAI(e, "gitee", f.URL)
	e.alias("text", target(e.deployment(p, "Qwen3-Embedding-8B", func(d *store.Deployment) { d.Kind = store.KindEmbedding }), 0))
	e.alias("ranker", target(e.deployment(p, "bge-reranker-v2-m3", func(d *store.Deployment) { d.Kind = store.KindRerank }), 0))
	key := e.key(everyAlias)
	e.start()
	for i, tc := range []struct{ path, body, route, op, client, input string }{
		{"/openai/v1/embeddings", dikwText, "POST /v1/embeddings", "embeddings", "embeddings Qwen3-Embedding-8B", "13"},
		{"/v1/rerank", dikwRerank, "POST /v1/rerank", "rerank", "rerank bge-reranker-v2-m3", ""},
	} {
		if resp, b := e.do("POST", tc.path, tc.body, map[string]string{"Authorization": "Bearer " + key}); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
		servers, clients := ended(t, rec, trace.SpanKindServer, i+1), ended(t, rec, trace.SpanKindClient, i+1)
		s, c := servers[i], clients[i]
		if s.Name() != tc.route || c.Name() != tc.client {
			t.Errorf("spans %q and %q, want %q and %q", s.Name(), c.Name(), tc.route, tc.client)
		}
		for _, kvs := range [][]string{{"gen_ai.operation.name", tc.op}} {
			if got, _ := attr(s.Attributes(), kvs[0]); got != kvs[1] {
				t.Errorf("request span %s = %q, want %q", kvs[0], got, kvs[1])
			}
			if got, _ := attr(c.Attributes(), kvs[0]); got != kvs[1] {
				t.Errorf("attempt span %s = %q, want %q", kvs[0], got, kvs[1])
			}
		}
		if got, _ := attr(s.Attributes(), "gen_ai.usage.input_tokens"); got != tc.input {
			t.Errorf("request span's input tokens %q, want %q", got, tc.input)
		}
	}
}
