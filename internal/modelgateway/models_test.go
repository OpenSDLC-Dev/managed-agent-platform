package modelgateway_test

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/anthropics/anthropic-sdk-go"
)

func catalogFor(t *testing.T, e *env) *catalog.Catalog {
	t.Helper()
	c, err := catalog.New(e.ctx, e.pool, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	d := e.deployment(p, "m")
	e.alias("fast", target(d, 0))
	e.alias("slow", target(d, 0))
	granted := e.key(everyAlias)
	narrow := e.key([]string{"slow"})
	ungranted := e.key(noGrant)
	// The control plane registers the bootstrap key's row; no policy names it.
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO api_keys (id, name, key_hash) VALUES ('key_boot', 'bootstrap', $1)`, apikey.Hash(bootstrap)); err != nil {
		t.Fatal(err)
	}
	e.start()
	body := `{"model":"fast","max_tokens":8,"messages":[]}`
	cases := []struct {
		name   string
		header map[string]string
		status int
		typ    string
	}{
		{"no key", nil, 401, "authentication_error"},
		{"unknown key", map[string]string{"x-api-key": "sk-map-api01-unknown"}, 401, "authentication_error"},
		{"no grant", map[string]string{"x-api-key": ungranted}, 403, "permission_error"},
		{"another alias's grant", map[string]string{"x-api-key": narrow}, 403, "permission_error"},
		{"granted", map[string]string{"x-api-key": granted}, 200, ""},
		{"as a Bearer", map[string]string{"Authorization": "Bearer " + granted}, 200, ""},
		{"x-api-key wins", map[string]string{"x-api-key": granted, "Authorization": "Bearer sk-wrong"}, 200, ""},
		{"bootstrap, no policy", map[string]string{"x-api-key": bootstrap}, 200, ""},
	}
	for _, c := range cases {
		resp, b := e.do("POST", "/v1/messages", body, c.header)
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, b)
			continue
		}
		if c.typ != "" {
			if typ, _, _ := errorOf(t, b); typ != c.typ {
				t.Errorf("%s: %s", c.name, typ)
			}
		}
	}
	// A repeated credential header is ambiguous, even beside a good one.
	for name, hdr := range map[string][][2]string{
		"repeated x-api-key":     {{"x-api-key", "sk-a"}, {"x-api-key", "sk-b"}, {"Authorization", "Bearer " + granted}},
		"repeated Authorization": {{"x-api-key", granted}, {"Authorization", "Bearer sk-a"}, {"Authorization", "Bearer sk-b"}},
	} {
		req, _ := http.NewRequest("POST", e.url+"/v1/messages", strings.NewReader(body))
		for _, h := range hdr {
			req.Header.Add(h[0], h[1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	// An unauthenticated caller learns nothing about paths.
	if resp, _ := e.do("GET", "/v1/anything", "", nil); resp.StatusCode != 401 {
		t.Errorf("unknown path, no key: %d", resp.StatusCode)
	}
}

// The bootstrap key needs no policy, but one written for it applies; a key
// an application issued under the name bootstrap is an ordinary key.
func TestTheBootstrapKey(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	d := e.deployment(p, "m")
	e.alias("fast", target(d, 0))
	e.alias("slow", target(d, 0))
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO api_keys (id, name, key_hash) VALUES ('key_boot', 'bootstrap', $1)`, apikey.Hash(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.PutKeyPolicy(e.ctx, store.KeyPolicy{APIKeyID: "key_boot", Aliases: []string{"slow"}}); err != nil {
		t.Fatal(err)
	}
	impostor := "sk-map-api01-impostor"
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO api_keys (id, name, key_hash) VALUES ('key_imp', 'bootstrap-ish', $1)`, apikey.Hash(impostor)); err != nil {
		t.Fatal(err)
	}
	e.start()
	for _, c := range []struct {
		key, model string
		status     int
	}{
		{bootstrap, "slow", 200},
		{bootstrap, "fast", 403},
		{impostor, "slow", 403},
	} {
		resp, b := e.do("POST", "/v1/messages", `{"model":"`+c.model+`","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": c.key})
		if resp.StatusCode != c.status {
			t.Errorf("%s on %s: %d %s", c.key, c.model, resp.StatusCode, b)
		}
	}
}

// The brain's key is the bootstrap key's twin on the inference routes: known
// by its configured value, it needs no policy, but one written for it
// applies, and it authenticates by its row. With no brain key configured the
// same value is an ordinary key without a grant.
func TestTheBrainKey(t *testing.T) {
	const brain = "sk-map-brain-test-key"
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	d := e.deployment(p, "m")
	e.alias("fast", target(d, 0))
	e.alias("slow", target(d, 0))
	call := func(model string) int {
		resp, _ := e.do("POST", "/v1/messages", `{"model":"`+model+`","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": brain})
		return resp.StatusCode
	}
	adminAPI, err := admin.New(admin.Config{Store: e.s, Cipher: e.cipher, BootstrapKey: bootstrap})
	if err != nil {
		t.Fatal(err)
	}
	e.start(func(c *modelgateway.Config) { c.BrainKey = brain; c.Admin = adminAPI })
	if s := call("fast"); s != 401 {
		t.Errorf("no row: %d", s)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO api_keys (id, name, key_hash) VALUES ('key_brain', 'brain', $1)`, apikey.Hash(brain)); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"fast", "slow"} {
		if s := call(model); s != 200 {
			t.Errorf("no policy, %s: %d", model, s)
		}
	}
	// The admin API answers the bootstrap key alone.
	for key, want := range map[string]int{bootstrap: 200, brain: 401} {
		if resp, b := e.do("GET", "/admin/v1/profiles", "", map[string]string{"x-api-key": key}); resp.StatusCode != want {
			t.Errorf("admin with %s: %d %s, want %d", key, resp.StatusCode, b, want)
		}
	}

	e.start()
	if s := call("fast"); s != 403 {
		t.Errorf("no brain key configured: %d", s)
	}

	if _, err := e.s.PutKeyPolicy(e.ctx, store.KeyPolicy{APIKeyID: "key_brain", Aliases: []string{"slow"}}); err != nil {
		t.Fatal(err)
	}
	e.start(func(c *modelgateway.Config) { c.BrainKey = brain })
	if s := call("slow"); s != 200 {
		t.Errorf("policy, slow: %d", s)
	}
	if s := call("fast"); s != 403 {
		t.Errorf("policy, fast: %d", s)
	}
}

// The bootstrap key authenticates by its row like any key, so the platform
// retiring it — archived, or past an expiry — ends its calls here too, and its
// value alone opens nothing.
func TestARetiredBootstrapKeyCallsNothing(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	e.start()
	call := func() int {
		resp, _ := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": bootstrap})
		return resp.StatusCode
	}
	if s := call(); s != 401 {
		t.Errorf("no row: %d", s)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO api_keys (id, name, key_hash) VALUES ('key_boot', 'bootstrap', $1)`, apikey.Hash(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if s := call(); s != 200 {
		t.Errorf("active: %d", s)
	}
	for name, q := range map[string]string{
		"archived": `UPDATE api_keys SET status = 'archived' WHERE id = 'key_boot'`,
		"expired":  `UPDATE api_keys SET status = 'active', expires_at = now() - interval '1 hour' WHERE id = 'key_boot'`,
	} {
		if _, err := e.pool.Exec(e.ctx, q); err != nil {
			t.Fatal(err)
		}
		if s := call(); s != 401 {
			t.Errorf("%s: %d", name, s)
		}
	}
}

func TestModels(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	big := e.deployment(p, "big", func(d *store.Deployment) {
		d.Capabilities = store.Capabilities{Thinking: true, Vision: true, MaxInputTokens: 200000, MaxTokens: 64000}
	})
	small := e.deployment(p, "small", func(d *store.Deployment) {
		d.Capabilities = store.Capabilities{Thinking: true, MaxInputTokens: 128000, MaxTokens: 32000}
	})
	unknown := e.deployment(p, "unknown", func(d *store.Deployment) { d.Capabilities = store.Capabilities{Thinking: true} })
	var all []string
	for i := range 25 {
		name := fmt.Sprintf("chat-%02d", i)
		all = append(all, name)
		e.alias(name, target(big, 0))
	}
	e.alias("both", target(big, 0), target(small, 1))
	e.alias("partly-known", target(big, 0), target(unknown, 1))
	e.alias("Qwen/Qwen3-Coder", target(big, 0))
	all = append(all, "Qwen/Qwen3-Coder", "both", "partly-known")
	e.alias("*", target(big, 0))
	e.alias("vectors", target(e.deployment(p, "e", func(d *store.Deployment) { d.Kind = store.KindEmbedding }), 0))
	key := e.key(everyAlias)
	narrow := e.key([]string{"chat-03", "vectors", "*"})
	e.start()
	slices.Sort(all)

	pager := e.client(key).Models.ListAutoPaging(e.ctx, anthropic.ModelListParams{Limit: anthropic.Int(10)})
	var got []string
	for pager.Next() {
		m := pager.Current()
		got = append(got, m.ID)
		for name, f := range map[string]bool{"id": m.JSON.ID.Valid(), "capabilities": m.JSON.Capabilities.Valid(),
			"created_at": m.JSON.CreatedAt.Valid(), "display_name": m.JSON.DisplayName.Valid(),
			"max_input_tokens": m.JSON.MaxInputTokens.Valid(), "max_tokens": m.JSON.MaxTokens.Valid(), "type": m.JSON.Type.Valid()} {
			if !f {
				t.Errorf("%s: %s missing", m.ID, name)
			}
		}
	}
	if err := pager.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(all) {
		t.Errorf("listed %v\nwant %v", got, all)
	}

	// Capabilities: what every target offers.
	for id, want := range map[string][4]any{
		"chat-00":      {true, true, int64(200000), int64(64000)},
		"both":         {false, true, int64(128000), int64(32000)},
		"partly-known": {false, true, int64(0), int64(0)},
	} {
		m, err := e.client(key).Models.Get(e.ctx, id, anthropic.ModelGetParams{})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		gotCaps := [4]any{m.Capabilities.ImageInput.Supported, m.Capabilities.Thinking.Supported, m.MaxInputTokens, m.MaxTokens}
		if gotCaps != want || m.DisplayName != id || m.CreatedAt.IsZero() || !m.Capabilities.Thinking.Types.Enabled.Supported {
			t.Errorf("%s: %v, display %q", id, gotCaps, m.DisplayName)
		}
	}
	if m, err := e.client(key).Models.Get(e.ctx, "Qwen/Qwen3-Coder", anthropic.ModelGetParams{}); err != nil || m.ID != "Qwen/Qwen3-Coder" {
		t.Errorf("a name with a slash: %v", err)
	}

	// A narrow key sees only its chat aliases; the wildcard and an
	// embedding alias are no Anthropic model.
	page, err := e.client(narrow).Models.List(e.ctx, anthropic.ModelListParams{})
	if err != nil || len(page.Data) != 1 || page.Data[0].ID != "chat-03" || page.HasMore {
		t.Errorf("narrow list: %v %v", page, err)
	}
	for _, id := range []string{"chat-04", "vectors", "*", "nope"} {
		_, err := e.client(narrow).Models.Get(e.ctx, id, anthropic.ModelGetParams{})
		var aerr *anthropic.Error
		if !errors.As(err, &aerr) || aerr.StatusCode != 404 {
			t.Errorf("get %s: %v", id, err)
		}
	}

	// Paging backwards, and the refusals.
	_, b := e.do("GET", "/v1/models?before_id=chat-05&limit=2", "", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	if !strings.Contains(string(b), `"first_id":"chat-03"`) || !strings.Contains(string(b), `"last_id":"chat-04"`) || !strings.Contains(string(b), `"has_more":true`) {
		t.Errorf("before_id page: %s", b)
	}
	_, b = e.do("GET", "/v1/models?after_id=zzz", "", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	if !strings.Contains(string(b), `"first_id":null`) || !strings.Contains(string(b), `"data":[]`) {
		t.Errorf("empty page: %s", b)
	}
	for q, want := range map[string]int{"limit=0": 400, "limit=1001": 400, "limit=x": 400, "after_id=a&before_id=b": 400} {
		if resp, _ := e.do("GET", "/v1/models?"+q, "", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}); resp.StatusCode != want {
			t.Errorf("%s: %d", q, resp.StatusCode)
		}
	}
	// The root answers Anthropic's shape only to an Anthropic caller, and
	// OpenAI's to any other (TestModelsInOpenAIsShape); the prefix asks for
	// Anthropic's explicitly.
	if resp, b := e.do("GET", "/v1/models", "", map[string]string{"x-api-key": key}); resp.StatusCode != 200 ||
		!strings.Contains(string(b), `"object":"list"`) || strings.Contains(string(b), "has_more") {
		t.Errorf("root without anthropic-version: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do("GET", "/anthropic/v1/models", "", map[string]string{"x-api-key": key}); resp.StatusCode != 200 || !strings.Contains(string(b), "chat-00") {
		t.Errorf("prefixed: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/v1/models", "", map[string]string{"x-api-key": key}); resp.StatusCode != 405 {
		t.Errorf("POST /v1/models: %d", resp.StatusCode)
	}
}

func TestTheAdminAPIIsMounted(t *testing.T) {
	e := newEnv(t)
	e.start(func(c *modelgateway.Config) {
		c.Admin = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(299) })
	})
	if resp, _ := e.do("GET", "/admin/v1/profiles", "", nil); resp.StatusCode != 299 {
		t.Errorf("admin: %d", resp.StatusCode)
	}
	e2 := newEnv(t)
	e2.start()
	if resp, _ := e2.do("GET", "/admin/v1/profiles", "", nil); resp.StatusCode != 404 {
		t.Errorf("no admin: %d", resp.StatusCode)
	}
}
