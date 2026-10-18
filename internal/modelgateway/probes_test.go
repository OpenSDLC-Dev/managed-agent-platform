package modelgateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// startProbing serves the gateway with the admin API mounted on it, its
// prober the gateway's own request path, as cmd/modelgateway wires them.
func (e *env) startProbing(mod ...func(*modelgateway.Config)) {
	e.t.Helper()
	e.start(append(mod, func(c *modelgateway.Config) {
		pr, err := modelgateway.NewProber(*c)
		e.must(err)
		c.Admin, err = admin.New(admin.Config{Store: e.s, Cipher: e.cipher, BootstrapKey: bootstrap, Prober: pr})
		e.must(err)
	})...)
}

// check asks the admin API to check a credential.
func (e *env) check(providerID, credentialID string) []admin.CredentialCheck {
	e.t.Helper()
	resp, b := e.do("POST", "/admin/v1/providers/"+providerID+"/credentials/"+credentialID+"/check", "", map[string]string{"x-api-key": bootstrap})
	var out struct {
		Type    string                  `json:"type"`
		Results []admin.CredentialCheck `json:"results"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &out) != nil || out.Type != "credential_check" {
		e.t.Fatalf("check: %d %s", resp.StatusCode, b)
	}
	return out.Results
}

// test asks the admin API to test a deployment, and returns the results and
// the request id the ledger rows carry.
func (e *env) test(deploymentID, body string) ([]admin.DeploymentTest, string) {
	e.t.Helper()
	resp, b := e.do("POST", "/admin/v1/deployments/"+deploymentID+"/test", body, map[string]string{"x-api-key": bootstrap})
	var out struct {
		Type    string                 `json:"type"`
		Results []admin.DeploymentTest `json:"results"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &out) != nil || out.Type != "deployment_test" {
		e.t.Fatalf("test: %d %s", resp.StatusCode, b)
	}
	return out.Results, resp.Header.Get("request-id")
}

func credentialsOf(e *env, p store.Provider) []store.Credential {
	e.t.Helper()
	cs, err := e.s.ListCredentials(e.ctx, p.ID)
	e.must(err)
	return cs
}

func intp(n int) *int { return &n }

// A check lists the vendor's models once on each protocol the credential may
// use, with the provider's headers and the key in the header its profile
// takes — MiniMax's Bearer on both — asking Anthropic's route for its
// largest page, and answers what each listed.
func TestACheckListsModelsOnEachProtocol(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		switch {
		case r.Method != http.MethodGet:
			writeBody(w, 405, `{}`)
		case c.Path == "/anthropic/v1/models":
			writeBody(w, 200, `{"data":[{"type":"model","id":"MiniMax-M3"},{"type":"model","id":"MiniMax-M2.7"}],"has_more":false,"first_id":"MiniMax-M3","last_id":"MiniMax-M2.7"}`)
		case c.Path == "/models":
			writeBody(w, 200, `{"object":"list","data":[{"id":"MiniMax-M3","object":"model"},{"object":"model"}]}`)
		default:
			writeBody(w, 404, `404 page not found`)
		}
	})
	p := e.provider(f.URL, func(p *store.Provider) {
		p.Profile = "minimax"
		p.Endpoints = map[profile.Protocol]string{profile.Anthropic: f.URL + "/anthropic", profile.OpenAI: f.URL}
		p.Headers = map[string]string{"X-Route": "pool-7"}
	})
	c := e.credential(p, "sk-minimax-key1", 1)
	e.startProbing()

	got := e.check(p.ID, c.ID)
	if len(got) != 2 || got[0].Protocol != profile.Anthropic || got[1].Protocol != profile.OpenAI {
		t.Fatalf("results = %+v", got)
	}
	if !got[0].OK || !slices.Equal(got[0].Models, []string{"MiniMax-M3", "MiniMax-M2.7"}) || *got[0].Status != 200 || got[0].Error != nil || got[0].LatencyMS == nil {
		t.Errorf("anthropic = %+v", got[0])
	}
	if !got[1].OK || !slices.Equal(got[1].Models, []string{"MiniMax-M3"}) {
		t.Errorf("openai = %+v", got[1])
	}
	calls := f.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	for _, call := range calls {
		if call.Header.Get("Authorization") != "Bearer sk-minimax-key1" || call.Key != "" || call.Header.Get("X-Route") != "pool-7" {
			t.Errorf("%s headers = %v", call.Path, call.Header)
		}
	}
	if calls[0].Query != "limit=1000" || calls[0].Header.Get("anthropic-version") != "2023-06-01" || calls[1].Query != "" {
		t.Errorf("queries %q %q, anthropic-version %q", calls[0].Query, calls[1].Query, calls[0].Header.Get("anthropic-version"))
	}
}

// A protocol whose list-models route the profile records the vendor does not
// serve is not called — DeepSeek's Anthropic endpoint — and the answer says
// so; a generic profile's route is tried, with x-api-key, and its 404 read as
// the route's absence; Gemini's lists its models[].name, without models/,
// under x-goog-api-key.
func TestACheckFollowsTheProfile(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		switch c.Path {
		case "/models":
			writeBody(w, 200, `{"object":"list","data":[{"id":"deepseek-flash","object":"model"}]}`)
		case "/v1beta/models":
			writeBody(w, 200, `{"models":[{"name":"models/gemini-2.5-flash"},{"name":"models/gemini-2.5-pro"}],"nextPageToken":""}`)
		default:
			writeBody(w, 404, `{"error":{"message":"no route /generic/v1/models"}}`)
		}
	})
	deepseek := onBoth(e, "deepseek", f.URL)
	generic := e.provider(f.URL+"/generic", func(p *store.Provider) { p.Headers = map[string]string{"anthropic-version": "2099-01-01"} })
	gc := e.credential(generic, "sk-generic-key1", 1)
	gemini := onGemini(e, f.URL)
	e.startProbing()

	got := e.check(deepseek.ID, credentialsOf(e, deepseek)[0].ID)
	if len(got) != 2 || got[0].OK || got[0].Status != nil || got[0].LatencyMS != nil || got[0].Error.Type != "not_found_error" ||
		!strings.Contains(got[0].Error.Message, "DeepSeek serves no list-models route on its Anthropic endpoint") {
		t.Errorf("deepseek anthropic = %+v %+v", got[0], got[0].Error)
	}
	if !got[1].OK || !slices.Equal(got[1].Models, []string{"deepseek-flash"}) {
		t.Errorf("deepseek openai = %+v", got[1])
	}

	got = e.check(generic.ID, gc.ID)
	if len(got) != 1 || got[0].OK || *got[0].Status != 404 || got[0].Error.Type != "not_found_error" ||
		!strings.Contains(got[0].Error.Message, "may not serve") || !strings.Contains(got[0].Error.Message, "no route /generic/v1/models") {
		t.Errorf("generic = %+v %+v", got, got[0].Error)
	}

	got = e.check(gemini.ID, credentialsOf(e, gemini)[0].ID)
	if len(got) != 1 || !got[0].OK || !slices.Equal(got[0].Models, []string{"gemini-2.5-flash", "gemini-2.5-pro"}) {
		t.Errorf("gemini = %+v", got)
	}

	var paths []string
	for _, c := range f.recorded() {
		paths = append(paths, c.Path+"?"+c.Query)
		switch c.Path {
		case "/generic/v1/models":
			if c.Key != "sk-generic-key1" || c.Header.Get("Authorization") != "" || c.Header.Get("anthropic-version") != "2099-01-01" {
				t.Errorf("generic headers = %v", c.Header)
			}
		case "/v1beta/models":
			if c.Header.Get("x-goog-api-key") != "AIza-gemini-key1" {
				t.Errorf("gemini headers = %v", c.Header)
			}
		}
	}
	if want := []string{"/models?", "/generic/v1/models?limit=1000", "/v1beta/models?pageSize=1000"}; !slices.Equal(paths, want) {
		t.Errorf("calls = %v, want %v", paths, want)
	}
}

// A vendor's refusal is reported as it gave it, its key removed: its status,
// its type or the one its status has, and its message — or its body, where
// it names none — and a 200 holding no list is no listing, as Zhipu's
// Anthropic host answers a bad key. A redirect is not followed, and a
// vendor that cannot be reached, or does not answer in time, is reported as
// such.
func TestACheckReportsTheVendorsAnswer(t *testing.T) {
	defer modelgateway.SetProbeTimeouts(200*time.Millisecond, time.Minute)()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	for _, c := range []struct {
		name, body string
		status     int
		url        string // instead of the fake
		typ, msg   string
		noStatus   bool
	}{
		{name: "typed", status: 401, body: `{"error":{"message":"Authentication Fails, Your api key: sk-openai-generic-key1 is invalid","type":"authentication_error"}}`,
			typ: "authentication_error", msg: "Your api key: [redacted] is invalid"},
		{name: "untyped 402", status: 402, body: `{"error":{"message":"Insufficient Balance"}}`, typ: "billing_error", msg: "Insufficient Balance"},
		{name: "untyped 403", status: 403, body: `{"message":"forbidden"}`, typ: "permission_error", msg: "forbidden"},
		{name: "vendor type", status: 401, body: `{"type":"error","error":{"type":"authorized_error","message":"login fail"}}`, typ: "authorized_error", msg: "login fail"},
		{name: "gemini key", status: 400, body: `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID"}]}}`,
			typ: "authentication_error", msg: "API key not valid"},
		{name: "empty", status: 401, body: ``, typ: "authentication_error", msg: "answered 401 with an empty body"},
		{name: "long type", status: 400, body: `{"error":{"type":"` + strings.Repeat("x", 2000) + `","message":"m"}}`, typ: strings.Repeat("x", 1024) + "…", msg: "m"},
		{name: "key in type", status: 401, body: `{"error":{"type":"bad key sk-openai-generic-key1","message":"denied"}}`, typ: "bad key [redacted]", msg: "denied"},
		{name: "rate limited", status: 429, body: `{"error":{"message":"slow down"}}`, typ: "rate_limit_error", msg: "slow down"},
		{name: "no list", status: 200, body: `{"code":401,"msg":"令牌已过期或验证不正确","success":false}`, typ: "api_error", msg: `no model list: {"code":401,"msg":"令牌已过期或验证不正确","success":false}`},
		{name: "no list, a key", status: 200, body: `{"code":401,"msg":"bad key sk-openai-generic-key1"}`, typ: "api_error", msg: "bad key [redacted]"},
		{name: "null list", status: 200, body: `{"object":"list","data":null}`, typ: "api_error", msg: "no model list"},
		{name: "html", status: 502, body: "<html><body>" + strings.Repeat("bad gateway ", 200) + "</body></html>", typ: "api_error", msg: "<html><body>bad gateway"},
		{name: "redirect", status: 302, body: ``, typ: "api_error", msg: "redirect (302)"},
		{name: "unreachable", url: gone.URL, typ: "api_error", msg: "the request failed", noStatus: true},
		{name: "silent", url: slow.URL, typ: "timeout_error", msg: "the request failed", noStatus: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
				if c.status == 302 {
					w.Header().Set("Location", "https://elsewhere.example/models")
				}
				writeBody(w, c.status, c.body)
			})
			url := f.URL
			if c.url != "" {
				url = c.url
			}
			p := onOpenAI(e, "openai-generic", url)
			e.startProbing()
			got := e.check(p.ID, credentialsOf(e, p)[0].ID)
			if len(got) != 1 || got[0].OK || got[0].Models != nil || got[0].Error == nil || got[0].LatencyMS == nil {
				t.Fatalf("results = %+v", got)
			}
			r := got[0]
			if r.Error.Type != c.typ || !strings.Contains(r.Error.Message, c.msg) || strings.Contains(r.Error.Message, "sk-openai-generic-key1") {
				t.Errorf("error = %+v, want %s naming %q", r.Error, c.typ, c.msg)
			}
			if c.noStatus != (r.Status == nil) || !c.noStatus && *r.Status != c.status {
				t.Errorf("status = %v, want %d", r.Status, c.status)
			}
			if len(r.Error.Message) > 1100 {
				t.Errorf("message of %d bytes", len(r.Error.Message))
			}
		})
	}
}

// A test makes one attempt at the deployment with each credential it names
// — once, with no retry, whatever the vendor answers — through the request
// path a call takes: here a passthrough, the one-token request a Messages
// caller would send. Each is written to the ledger as a test, under no key
// and no alias.
func TestADeploymentTestMakesOneAttemptWithEachCredential(t *testing.T) {
	defer modelgateway.SetProbeTimeouts(time.Minute, time.Second)()
	e := newEnv(t)
	var arrived sync.WaitGroup // the attempts are made at once: each waits for the other, past the test's bound
	arrived.Add(2)
	all := make(chan struct{})
	go func() { arrived.Wait(); close(all) }()
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		arrived.Done()
		select {
		case <-all:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
		}
		if c.Key == "sk-upstream-3" {
			writeBody(w, 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		message("p")(w, r, c)
	})
	p := e.provider(f.URL)
	c1 := e.credential(p, "sk-upstream-1", 1)
	off := e.credential(p, "sk-upstream-2", 1)
	_, err := e.s.UpdateCredential(e.ctx, p.ID, off.ID, func(c *store.Credential) error { c.Enabled = false; return nil })
	e.must(err)
	c3 := e.credential(p, "sk-upstream-3", 1)
	d := e.deployment(p, "up-model")
	e.startProbing()

	got, rid := e.test(d.ID, "")
	anthropicProto := profile.Anthropic
	want := []admin.DeploymentTest{
		{CredentialID: c1.ID, Protocol: &anthropicProto, OK: true, Status: intp(200), Usage: &admin.TestUsage{InputTokens: 5, OutputTokens: 2}},
		{CredentialID: c3.ID, Protocol: &anthropicProto, Status: intp(529), Error: &admin.ProbeError{Type: "overloaded_error", Message: "Overloaded"}},
	}
	for i := range got {
		if got[i].LatencyMS == nil {
			t.Errorf("result %d has no latency", i)
		}
		got[i].LatencyMS = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("results = %s, want %s", encodeJSON(got), encodeJSON(want))
	}
	calls := f.recorded()
	slices.SortFunc(calls, func(a, b fakeCall) int { return strings.Compare(a.Key, b.Key) })
	if len(calls) != 2 || calls[0].Key != "sk-upstream-1" || calls[1].Key != "sk-upstream-3" {
		t.Fatalf("calls = %+v", calls)
	}
	var body any
	e.must(json.Unmarshal(calls[0].Raw, &body))
	if !reflect.DeepEqual(body, map[string]any{"model": "up-model", "max_tokens": float64(1),
		"messages": []any{map[string]any{"role": "user", "content": "ping"}}}) || calls[0].Path != "/v1/messages" ||
		calls[0].Header.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("sent %s to %s, anthropic-version %q", calls[0].Raw, calls[0].Path, calls[0].Header.Get("anthropic-version"))
	}

	rows := e.ledger()
	if len(rows) != 2 {
		t.Fatalf("ledger = %+v", rows)
	}
	slices.SortFunc(rows, func(a, b store.Usage) int { return strings.Compare(a.CredentialID, b.CredentialID) })
	if c1.ID > c3.ID {
		slices.Reverse(rows)
	}
	for i, row := range rows {
		if row.RequestID != rid || row.APIKeyID != "admin" || row.Endpoint != "deployment_test" || row.Alias != "" ||
			row.Model != "up-model" || row.DeploymentID != d.ID || row.Protocol != "anthropic" {
			t.Errorf("row %d = %+v", i, row)
		}
	}
	if rows[0].CredentialID != c1.ID || rows[0].Status != 200 || rows[0].ErrorType != "" || rows[0].Tokens == nil || rows[0].Tokens.Input != 5 {
		t.Errorf("first row = %+v", rows[0])
	}
	if rows[1].CredentialID != c3.ID || rows[1].Status != 529 || rows[1].ErrorType != "overloaded_error" || rows[1].Tokens != nil {
		t.Errorf("second row = %+v", rows[1])
	}
}

// A vendor refusing the credential is reported as it refused it, where a
// caller is given the gateway's 502: its status, its type, its message, its
// key removed. The ledger keeps what a call would have recorded.
func TestADeploymentTestReportsARefusedCredential(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, status(402, `{"error":{"message":"Insufficient Balance for sk-upstream-1","type":"unknown_error"}}`))
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	d := e.deployment(p, "up-model")
	e.startProbing()

	got, _ := e.test(d.ID, "")
	if len(got) != 1 || got[0].OK || *got[0].Status != 402 || got[0].Error.Type != "unknown_error" ||
		got[0].Error.Message != "Insufficient Balance for [redacted]" || got[0].Usage != nil {
		t.Errorf("results = %s", encodeJSON(got))
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].Status != 502 || rows[0].ErrorType != "api_error" {
		t.Errorf("ledger = %+v", rows)
	}

	f.answer = status(401, `{"message":"bad key"}`)
	if got, _ := e.test(d.ID, ""); got[0].Error.Type != "authentication_error" || got[0].Error.Message != "bad key" {
		t.Errorf("an untyped 401: %s", encodeJSON(got))
	}
	f.answer = status(403, ``)
	if got, _ := e.test(d.ID, ""); got[0].Error.Type != "permission_error" || got[0].Error.Message != "the vendor answered 403 with an empty body" {
		t.Errorf("an empty 403: %s", encodeJSON(got))
	}

	// A refusal cut short at its bound is not quoted: its key may be cut in
	// half, or escaped in a JSON string no longer whole enough to decode.
	q := e.provider(f.URL)
	e.credential(q, "sk-up/stream-2", 1)
	long := e.deployment(q, "up-model")
	f.answer = status(401, `{"error":{"message":"bad key sk-up\/stream-2 `+strings.Repeat("x", 5000)+`"}}`)
	if got, _ := e.test(long.ID, ""); *got[0].Status != 401 || got[0].Error.Type != "api_error" ||
		got[0].Error.Message != "upstream refused the gateway's credential (HTTP 401)" {
		t.Errorf("a long 401: %s", encodeJSON(got))
	}
}

// A converted attempt's error keeps the vendor's own type and message in a
// test's result, though a caller is given the type converted; whatever the
// vendor says, its credential is removed.
func TestADeploymentTestReportsTheVendorsOwnError(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, status(429, `{"error":{"type":"insufficient_quota","message":"quota exceeded for sk-openai-generic-key1"}}`))
	chat := e.deployment(onOpenAI(e, "openai-generic", f.URL), "up-model")
	g := newFake(t, status(200, `{"candidates":[{"content":{"role":"model","parts":[{"text":"p"}]},"finishReason":"OTHER","finishMessage":"bad key AIza-gemini-key1 `+strings.Repeat("x", 2000)+`"}]}`))
	gem := e.deployment(onGemini(e, g.URL), "gemini-2.5-flash")
	e.startProbing()

	got, _ := e.test(chat.ID, "")
	if len(got) != 1 || got[0].OK || *got[0].Status != 429 || got[0].Error.Type != "insufficient_quota" ||
		got[0].Error.Message != "quota exceeded for [redacted]" {
		t.Errorf("converted: %s", encodeJSON(got))
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].Status != 429 || rows[0].ErrorType != "rate_limit_error" {
		t.Errorf("ledger = %+v", rows)
	}

	got, _ = e.test(gem.ID, "")
	if len(got) != 1 || got[0].OK || *got[0].Status != 200 || got[0].Error.Type != "api_error" ||
		!strings.Contains(got[0].Error.Message, "finished OTHER: bad key [redacted]") || len(got[0].Error.Message) > 1100 {
		t.Errorf("gemini: %s", encodeJSON(got))
	}
}

// A test is bounded as a whole: a vendor that sends a byte at a time, each
// inside the stall timeout, is cut off at the test's bound.
func TestADeploymentTestIsBoundedWhileTheVendorDribbles(t *testing.T) {
	defer modelgateway.SetProbeTimeouts(time.Minute, 300*time.Millisecond)()
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ fakeCall) {
		w.WriteHeader(200)
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	d := e.deployment(p, "up-model")
	e.startProbing()

	start := time.Now()
	got, _ := e.test(d.ID, "")
	if len(got) != 1 || got[0].OK || *got[0].Status != 200 || got[0].Error.Type != "timeout_error" || time.Since(start) > 3*time.Second {
		t.Errorf("after %s: %s", time.Since(start), encodeJSON(got))
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].Status != 504 || rows[0].ErrorType != "timeout_error" {
		t.Errorf("ledger = %+v", rows)
	}
}

// A chat deployment reached on an OpenAI credential alone is tested through
// the conversion a Messages call would take; one on Gemini through its
// generateContent; an embedding and a rerank deployment with one short input
// on their own routes.
func TestADeploymentTestTakesTheCallsPath(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		switch {
		case strings.HasSuffix(c.Path, "/chat/completions"):
			chatAnswer("p", deepseekStyle)(w, r, c)
		case strings.HasSuffix(c.Path, ":generateContent"):
			geminiText("p")(w, r, c)
		default:
			vectors(w, r, c)
		}
	})
	openai := onOpenAI(e, "deepseek", f.URL)
	chat := e.deployment(openai, "deepseek-flash")
	embed := e.deployment(openai, "embedder", func(d *store.Deployment) { d.Kind = store.KindEmbedding })
	rerank := e.deployment(openai, "ranker", func(d *store.Deployment) { d.Kind = store.KindRerank })
	gem := e.deployment(onGemini(e, f.URL), "gemini-2.5-flash")
	e.startProbing()

	oa, gp := profile.OpenAI, profile.Gemini
	for _, c := range []struct {
		d     store.Deployment
		proto *profile.Protocol
		usage *admin.TestUsage
		path  string
		sent  map[string]any
	}{
		{chat, &oa, &admin.TestUsage{InputTokens: 6, OutputTokens: 3, CacheReadTokens: 4}, "/chat/completions",
			map[string]any{"model": "deepseek-flash", "max_tokens": float64(1), "messages": []any{map[string]any{"role": "user", "content": "ping"}}}},
		{embed, &oa, &admin.TestUsage{InputTokens: 13}, "/embeddings", map[string]any{"model": "embedder", "input": "ping"}},
		{rerank, &oa, nil, "/rerank", map[string]any{"model": "ranker", "query": "ping", "documents": []any{"pong"}}},
		{gem, &gp, &admin.TestUsage{InputTokens: 6, OutputTokens: 8, CacheReadTokens: 4}, "/v1beta/models/gemini-2.5-flash:generateContent", nil},
	} {
		got, _ := e.test(c.d.ID, "")
		if len(got) != 1 || !got[0].OK || !reflect.DeepEqual(got[0].Protocol, c.proto) || !reflect.DeepEqual(got[0].Usage, c.usage) {
			t.Errorf("%s: %s", c.d.UpstreamModel, encodeJSON(got))
		}
		calls := f.recorded()
		last := calls[len(calls)-1]
		var sent map[string]any
		e.must(json.Unmarshal(last.Raw, &sent))
		if last.Path != c.path || c.sent != nil && !reflect.DeepEqual(sent, c.sent) {
			t.Errorf("%s: sent %s to %s", c.d.UpstreamModel, last.Raw, last.Path)
		}
	}
	if n := len(f.recorded()); n != 4 {
		t.Errorf("%d calls, want 4", n)
	}
}

// A credential no protocol of the request reaches the deployment on is not
// called, nor recorded; a test whose vendor stays silent past the test's
// bound ends there, whatever the provider's stall timeout.
func TestWhatADeploymentTestCannotReach(t *testing.T) {
	defer modelgateway.SetProbeTimeouts(time.Minute, 200*time.Millisecond)()
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ fakeCall) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	both := onBoth(e, "minimax", f.URL, profile.Anthropic)
	embed := e.deployment(both, "embedder", func(d *store.Deployment) { d.Kind = store.KindEmbedding })
	chat := e.deployment(both, "MiniMax-M3")
	patient := e.provider(f.URL, func(p *store.Provider) { p.StallTimeout = 10 * time.Minute })
	e.credential(patient, "sk-patient-key1", 1)
	long := e.deployment(patient, "up-model")
	e.startProbing()

	got, _ := e.test(embed.ID, "")
	if len(got) != 1 || got[0].OK || got[0].Protocol != nil || got[0].Status != nil || got[0].LatencyMS != nil ||
		got[0].Error.Type != "invalid_request_error" || !strings.Contains(got[0].Error.Message, "that OpenAI requests reach") {
		t.Errorf("results = %s", encodeJSON(got))
	}
	if n := len(f.recorded()); n != 0 {
		t.Errorf("%d calls", n)
	}
	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("ledger = %+v", rows)
	}

	for i, d := range []store.Deployment{chat, long} {
		start := time.Now()
		got, _ = e.test(d.ID, "")
		if len(got) != 1 || got[0].OK || got[0].Status != nil || got[0].Error.Type != "timeout_error" || time.Since(start) > 3*time.Second {
			t.Errorf("%s, after %s: %s", d.UpstreamModel, time.Since(start), encodeJSON(got))
		}
		if rows := e.ledger(); len(rows) != i+1 || rows[0].Status != 504 || rows[0].ErrorType != "timeout_error" {
			t.Errorf("%s: ledger = %+v", d.UpstreamModel, rows)
		}
	}
}

// An embeddings answer cut off past the gateway's bound fails its test,
// though the vendor answered 200, and is recorded as the failure it was.
func TestACutOffVectorsAnswerFailsATest(t *testing.T) {
	defer modelgateway.SetMaxVectorAnswer(64)()
	e := newEnv(t)
	f := newFake(t, vectors)
	embed := e.deployment(onOpenAI(e, "openai-generic", f.URL), "embedder", func(d *store.Deployment) { d.Kind = store.KindEmbedding })
	e.startProbing()

	got, _ := e.test(embed.ID, "")
	if len(got) != 1 || got[0].OK || got[0].Status == nil || *got[0].Status != 200 || got[0].Error == nil || got[0].Error.Type != "api_error" {
		t.Errorf("results = %s", encodeJSON(got))
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].Status != 200 || rows[0].ErrorType != "api_error" {
		t.Errorf("ledger = %+v", rows)
	}
}
