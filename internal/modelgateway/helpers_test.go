package modelgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

const bootstrap = "sk-map-bootstrap-test-key"

// env is a gateway over a real database and cipher, with its configuration
// written through the store before start.
type env struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	s       *store.Store
	cipher  secrets.Cipher
	url     string
	keys    int
	handler http.Handler
	h2      bool         // serve the gateway over TLS, and do's requests over HTTP/2
	httpc   *http.Client // do's client when h2 is set
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.NewPool(t)
	cipher, err := local.New(local.Config{KeyID: "k", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: context.Background(), pool: pool, s: store.New(pool), cipher: cipher}
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// provider is an anthropic-generic provider at url.
func (e *env) provider(url string, mod ...func(*store.Provider)) store.Provider {
	e.t.Helper()
	p := store.Provider{Name: "p", Profile: "anthropic-generic", Enabled: true,
		Endpoints: map[profile.Protocol]string{profile.Anthropic: url}}
	for _, m := range mod {
		m(&p)
	}
	p, err := e.s.CreateProvider(e.ctx, p)
	e.must(err)
	return p
}

// credential seals key under the cipher, as the admin API does.
func (e *env) credential(p store.Provider, key string, weight int) store.Credential {
	e.t.Helper()
	ct, kid, err := e.cipher.Encrypt(e.ctx, []byte(key))
	e.must(err)
	c, err := e.s.CreateCredential(e.ctx, store.Credential{ProviderID: p.ID, Ciphertext: ct, KeyID: kid,
		LastFour: key[len(key)-4:], Weight: weight, Enabled: true})
	e.must(err)
	return c
}

func (e *env) deployment(p store.Provider, upstream string, mod ...func(*store.Deployment)) store.Deployment {
	e.t.Helper()
	d := store.Deployment{ProviderID: p.ID, UpstreamModel: upstream, Kind: store.KindChat, Enabled: true}
	for _, m := range mod {
		m(&d)
	}
	d, err := e.s.CreateDeployment(e.ctx, d)
	e.must(err)
	return d
}

func (e *env) alias(name string, targets ...store.Target) {
	e.t.Helper()
	_, err := e.s.CreateAlias(e.ctx, store.Alias{Name: name, Targets: targets})
	e.must(err)
}

func target(d store.Deployment, priority int) store.Target {
	return store.Target{DeploymentID: d.ID, Priority: priority, Weight: 1}
}

// everyAlias grants a key every alias; noGrant writes no policy at all.
var (
	everyAlias = []string(nil)
	noGrant    = []string{"\x00none"}
)

// key issues a platform API key and grants it aliases.
func (e *env) key(aliases []string) string {
	e.t.Helper()
	e.keys++
	key := fmt.Sprintf("sk-map-api01-test-%d", e.keys)
	id := fmt.Sprintf("key_%d", e.keys)
	_, err := e.pool.Exec(e.ctx, `INSERT INTO api_keys (id, name, key_hash) VALUES ($1, $2, $3)`, id, id, apikey.Hash(key))
	e.must(err)
	if len(aliases) == 1 && aliases[0] == noGrant[0] {
		return key
	}
	_, err = e.s.PutKeyPolicy(e.ctx, store.KeyPolicy{APIKeyID: id, Aliases: aliases})
	e.must(err)
	return key
}

// start loads the configuration written so far and serves the gateway.
func (e *env) start(mod ...func(*modelgateway.Config)) {
	e.t.Helper()
	cat, err := catalog.New(e.ctx, e.pool, time.Hour)
	e.must(err)
	cfg := modelgateway.Config{Catalog: cat, Store: e.s, Keys: e.pool, Cipher: e.cipher, BootstrapKey: bootstrap, Backoff: time.Millisecond}
	for _, m := range mod {
		m(&cfg)
	}
	h, err := modelgateway.New(cfg)
	e.must(err)
	e.handler = h
	srv := httptest.NewUnstartedServer(h)
	if e.h2 {
		srv.EnableHTTP2 = true
		srv.StartTLS()
		e.httpc = srv.Client()
	} else {
		srv.Start()
	}
	e.t.Cleanup(srv.Close)
	e.url = srv.URL
}

// client is an Anthropic SDK client of the gateway, with its own retries off
// so a test sees the gateway's.
func (e *env) client(key string) *anthropic.Client {
	c := anthropic.NewClient(option.WithBaseURL(e.url), option.WithAPIKey(key), option.WithMaxRetries(0))
	return &c
}

// do sends a raw request to the gateway.
func (e *env) do(method, path, body string, header map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.url+path, bytes.NewBufferString(body))
	e.must(err)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	c := http.DefaultClient
	if e.httpc != nil {
		c = e.httpc
	}
	resp, err := c.Do(req)
	e.must(err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	e.must(err)
	return resp, b
}

// errorOf decodes an Anthropic error envelope.
func errorOf(t *testing.T, b []byte) (typ, msg, rid string) {
	t.Helper()
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(b, &env); err != nil || env.Type != "error" {
		t.Fatalf("not an error envelope: %s", b)
	}
	return env.Error.Type, env.Error.Message, env.RequestID
}

func hello() []anthropic.MessageParam {
	return []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))}
}

// fake is an upstream speaking Anthropic Messages, whose behavior each test
// sets; it records every call it receives.
type fake struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []fakeCall
	answer func(w http.ResponseWriter, r *http.Request, c fakeCall)
}

type fakeCall struct {
	Path  string
	Query string
	// EscapedPath is the path as sent, its escapes kept.
	EscapedPath string
	Key         string
	Model       string
	Header      http.Header
	Body        map[string]json.RawMessage
	Raw         []byte // the body as received
}

func newFake(t *testing.T, answer func(w http.ResponseWriter, r *http.Request, c fakeCall)) *fake {
	t.Helper()
	f := &fake{answer: answer}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c := fakeCall{Path: r.URL.Path, Query: r.URL.RawQuery, EscapedPath: r.URL.EscapedPath(), Key: r.Header.Get("x-api-key"), Header: r.Header.Clone(), Raw: b}
		_ = json.Unmarshal(b, &c.Body)
		_ = json.Unmarshal(c.Body["model"], &c.Model)
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		f.answer(w, r, c)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) recorded() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// message answers a whole message from the model the call named.
func message(text string) func(http.ResponseWriter, *http.Request, fakeCall) {
	return func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		if c.Path == "/v1/messages/count_tokens" {
			writeBody(w, 200, `{"input_tokens":42}`)
			return
		}
		var stream bool
		_ = json.Unmarshal(c.Body["stream"], &stream)
		if stream {
			streamText(w, c.Model, text)
			return
		}
		writeBody(w, 200, fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":2}}`, c.Model, text))
	}
}

func writeBody(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// events is an Anthropic stream of text from model, with a ping and a
// keep-alive comment in the middle.
func events(model, text string) []string {
	return []string{
		fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n", model),
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: ping\ndata: {\"type\": \"ping\"}\n\n",
		": keep-alive\n\n",
		fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text),
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
}

func streamText(w http.ResponseWriter, model, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	for _, e := range events(model, text) {
		_, _ = io.WriteString(w, e)
		w.(http.Flusher).Flush()
	}
}

func status(code int, body string) func(http.ResponseWriter, *http.Request, fakeCall) {
	return func(w http.ResponseWriter, _ *http.Request, _ fakeCall) { writeBody(w, code, body) }
}
