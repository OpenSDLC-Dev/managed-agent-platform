package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

const (
	bootstrap = "sk-map-api01-bootstrap-for-tests"
	audience  = "modelgateway-admin"
)

type env struct {
	t      *testing.T
	url    string
	pool   *pgxpool.Pool
	store  *store.Store
	cipher *local.Cipher
	idp    *identitytest.IdP
	clock  *identitytest.Clock
}

func newEnv(t *testing.T, withIdentity bool) *env {
	t.Helper()
	pool := pgtest.NewPool(t)
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: pool, store: store.New(pool), cipher: cipher}
	cfg := admin.Config{Store: e.store, Cipher: cipher, BootstrapKey: bootstrap}
	if withIdentity {
		e.idp = identitytest.NewIdP(t)
		e.clock = identitytest.NewClock(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
		v, err := identity.New(context.Background(), identity.Config{
			Mode: identity.ModeOIDC, Issuer: e.idp.Issuer(), Audience: audience, JWKSURL: e.idp.JWKSURL(),
			RoleMap: map[string]identity.Role{
				"admins": identity.RoleAdmin, "devs": identity.RoleDeveloper, "readers": identity.RoleViewer,
			},
			HTTPClient: e.idp.Client(), Now: e.clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Verifier = v
	}
	h, err := admin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

func (e *env) token(roles ...string) string {
	e.t.Helper()
	c := e.idp.Claims(audience, e.clock.Now())
	values := make([]any, len(roles))
	for i, r := range roles {
		values[i] = r
	}
	c["roles"] = values
	return e.idp.Mint(e.t, c)
}

type reply struct {
	status int
	body   map[string]any
	raw    string
}

func (r reply) errType() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		s, _ := e["type"].(string)
		return s
	}
	return ""
}

func (r reply) errMessage() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		s, _ := e["message"].(string)
		return s
	}
	return ""
}

func (r reply) list() []map[string]any {
	var out []map[string]any
	for _, v := range r.body["data"].([]any) {
		out = append(out, v.(map[string]any))
	}
	return out
}

func (e *env) do(method, path string, body any, headers map[string]string) reply {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, e.url+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, raw: string(raw)}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r.body); err != nil {
			e.t.Fatalf("%s %s: body is not JSON: %s", method, path, raw)
		}
	}
	return r
}

// admin calls with the bootstrap key.
func (e *env) admin(method, path string, body any) reply {
	e.t.Helper()
	return e.do(method, path, body, map[string]string{"x-api-key": bootstrap})
}

func (e *env) ok(r reply, what string) map[string]any {
	e.t.Helper()
	if r.status != http.StatusOK {
		e.t.Fatalf("%s: status %d, body %s", what, r.status, r.raw)
	}
	return r.body
}

func (e *env) refused(r reply, status int, errType, contains string) {
	e.t.Helper()
	if r.status != status || r.errType() != errType || !strings.Contains(r.errMessage(), contains) {
		e.t.Fatalf("got %d %s %q, want %d %s naming %q", r.status, r.errType(), r.errMessage(), status, errType, contains)
	}
}

func (e *env) provider(profile string, endpoints map[string]string) string {
	e.t.Helper()
	b := e.ok(e.admin("POST", "/admin/v1/providers", map[string]any{"name": "p", "profile": profile, "endpoints": endpoints}), "create provider")
	return b["id"].(string)
}

func (e *env) deployment(providerID, kind string) string {
	e.t.Helper()
	b := e.ok(e.admin("POST", "/admin/v1/deployments", map[string]any{
		"provider_id": providerID, "upstream_model": "deepseek-v4-pro", "kind": kind,
	}), "create deployment")
	return b["id"].(string)
}

var deepseekBoth = map[string]string{"anthropic": "https://api.deepseek.com/anthropic", "openai": "https://api.deepseek.com"}

// The bootstrap key, by either header, may do anything; no other platform
// key may do anything, since a key that could edit key policies could lift
// its own limits; and a request with no credential, or a wrong one, is
// unauthenticated.
func TestAuthByKey(t *testing.T) {
	e := newEnv(t, false)
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO api_keys (id, name, key_hash) VALUES ('key_app', 'bootstrap', $1)`, apikey.Hash("sk-map-api01-app")); err != nil {
		t.Fatal(err)
	}
	e.ok(e.admin("GET", "/admin/v1/providers", nil), "x-api-key bootstrap")
	e.ok(e.do("GET", "/admin/v1/providers", nil, map[string]string{"Authorization": "Bearer " + bootstrap}), "bearer bootstrap")
	for name, headers := range map[string]map[string]string{
		"none":                          nil,
		"wrong key":                     {"x-api-key": "sk-map-api01-wrong"},
		"an issued key named bootstrap": {"x-api-key": "sk-map-api01-app"},
		"an issued key as bearer":       {"Authorization": "Bearer sk-map-api01-app"},
		"a JWT with identity off":       {"Authorization": "Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"},
	} {
		if r := e.do("GET", "/admin/v1/providers", nil, headers); r.status != http.StatusUnauthorized || r.errType() != "authentication_error" {
			t.Errorf("%s: %d %s, want 401", name, r.status, r.raw)
		}
	}
	req, _ := http.NewRequest("GET", e.url+"/admin/v1/providers", nil)
	req.Header.Add("x-api-key", bootstrap)
	req.Header.Add("x-api-key", bootstrap)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("two x-api-key headers: %d, want 401", resp.StatusCode)
	}
}

// An operator token reads with viewer or developer and writes with admin.
func TestAuthByRole(t *testing.T) {
	e := newEnv(t, true)
	body := map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth}
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	for _, role := range []string{"readers", "devs", "admins"} {
		e.ok(e.do("GET", "/admin/v1/providers", nil, bearer(e.token(role))), role+" reads")
	}
	for _, role := range []string{"readers", "devs"} {
		e.refused(e.do("POST", "/admin/v1/providers", body, bearer(e.token(role))), http.StatusForbidden, "permission_error", "admin")
	}
	e.ok(e.do("POST", "/admin/v1/providers", body, bearer(e.token("admins"))), "admin writes")
	e.refused(e.do("GET", "/admin/v1/providers", nil, bearer(e.token("strangers"))), http.StatusForbidden, "permission_error", "role")
	e.refused(e.do("GET", "/admin/v1/nothing", nil, bearer(e.token("strangers"))), http.StatusForbidden, "permission_error", "role")
	e.refused(e.do("PUT", "/admin/v1/providers", nil, bearer(e.token("strangers"))), http.StatusForbidden, "permission_error", "role")
	if r := e.do("GET", "/admin/v1/providers", nil, bearer("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln")); r.status != http.StatusUnauthorized {
		t.Errorf("forged token: %d %s", r.status, r.raw)
	}
}

func TestProfiles(t *testing.T) {
	e := newEnv(t, false)
	list := e.admin("GET", "/admin/v1/profiles", nil).list()
	if len(list) != 6 || list[0]["type"] != "profile" {
		t.Fatalf("profiles = %v", list)
	}
	p := e.ok(e.admin("GET", "/admin/v1/profiles/minimax", nil), "get minimax")
	if p["name"] != "minimax" || len(p["hosts"].([]any)) != 4 {
		t.Errorf("minimax = %v", p)
	}
	e.refused(e.admin("GET", "/admin/v1/profiles/nope", nil), http.StatusNotFound, "not_found_error", "nope")
}

// A provider's endpoints are checked against its profile and refused when
// they could carry a secret; so are its headers.
func TestProviders(t *testing.T) {
	e := newEnv(t, false)
	created := e.ok(e.admin("POST", "/admin/v1/providers", map[string]any{
		"name": "deepseek prod", "profile": "deepseek", "endpoints": map[string]string{"anthropic": "https://api.deepseek.com/anthropic/"},
		"headers": map[string]string{"X-Gateway-Route": "pool-7"}, "stall_timeout_ms": 90000,
	}), "create")
	id := created["id"].(string)
	if created["type"] != "provider" || created["enabled"] != true || created["stall_timeout_ms"] != 90000.0 || created["propagate_trace"] != false ||
		created["endpoints"].(map[string]any)["anthropic"] != "https://api.deepseek.com/anthropic" {
		t.Fatalf("created = %v", created)
	}
	for name, tc := range map[string]struct {
		body     map[string]any
		contains string
	}{
		"unknown profile":      {map[string]any{"name": "p", "profile": "openrouter", "endpoints": deepseekBoth}, "openrouter"},
		"protocol off profile": {map[string]any{"name": "p", "profile": "anthropic-generic", "endpoints": map[string]string{"openai": "https://x.example"}}, "openai"},
		"no endpoint":          {map[string]any{"name": "p", "profile": "deepseek", "endpoints": map[string]string{}}, "endpoint"},
		"unknown protocol":     {map[string]any{"name": "p", "profile": "deepseek", "endpoints": map[string]string{"gemini": "https://x.example"}}, "gemini"},
		"userinfo":             {map[string]any{"name": "p", "profile": "anthropic-generic", "endpoints": map[string]string{"anthropic": "https://u:p@x.example"}}, "userinfo"},
		"query":                {map[string]any{"name": "p", "profile": "anthropic-generic", "endpoints": map[string]string{"anthropic": "https://x.example/?key=s"}}, "query"},
		"fragment":             {map[string]any{"name": "p", "profile": "anthropic-generic", "endpoints": map[string]string{"anthropic": "https://x.example/#f"}}, "fragment"},
		"scheme":               {map[string]any{"name": "p", "profile": "anthropic-generic", "endpoints": map[string]string{"anthropic": "ftp://x.example"}}, "http"},
		"relative":             {map[string]any{"name": "p", "profile": "anthropic-generic", "endpoints": map[string]string{"anthropic": "/v1"}}, "http"},
		"no host":              {map[string]any{"name": "p", "profile": "anthropic-generic", "endpoints": map[string]string{"anthropic": "https://:443"}}, "http"},
		"port out of range":    {map[string]any{"name": "p", "profile": "anthropic-generic", "endpoints": map[string]string{"anthropic": "http://model.svc:99999"}}, "port"},
		"port zero":            {map[string]any{"name": "p", "profile": "anthropic-generic", "endpoints": map[string]string{"anthropic": "http://model.svc:0/v1"}}, "port"},
		"credential header":    {map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth, "headers": map[string]string{"X-Upstream-Token": "s"}}, "X-Upstream-Token"},
		"authorization header": {map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth, "headers": map[string]string{"Authorization": "Bearer s"}}, "Authorization"},
		"bad header name":      {map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth, "headers": map[string]string{"X Route": "a"}}, "X Route"},
		"tracestate header":    {map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth, "headers": map[string]string{"Tracestate": "v=1"}}, "Tracestate"},
		"traceparent header":   {map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth, "headers": map[string]string{"traceparent": "00-x"}}, "traceparent"},
		"header line break":    {map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth, "headers": map[string]string{"X-Route": "a\r\nX-Evil: b"}}, "X-Route"},
		"headers alike":        {map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth, "headers": map[string]string{"X-Route": "a", "x-route": "b"}}, "x-route"},
		"no name":              {map[string]any{"profile": "deepseek", "endpoints": deepseekBoth}, "name"},
		"stall zero":           {map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth, "stall_timeout_ms": 0}, "stall_timeout_ms"},
		"unknown field":        {map[string]any{"name": "p", "profile": "deepseek", "endpoints": deepseekBoth, "region": "cn"}, "region"},
	} {
		t.Run(name, func(t *testing.T) {
			e.refused(e.admin("POST", "/admin/v1/providers", tc.body), http.StatusBadRequest, "invalid_request_error", tc.contains)
		})
	}
	e.refused(e.admin("POST", "/admin/v1/providers", "{"), http.StatusBadRequest, "invalid_request_error", "JSON")
	e.refused(e.admin("POST", "/admin/v1/providers", `{"name":"p","profile":"deepseek","endpoints":{"openai":"https://api.deepseek.com"}}}`),
		http.StatusBadRequest, "invalid_request_error", "after")
	if traced := e.ok(e.admin("POST", "/admin/v1/providers", map[string]any{
		"name": "in-cluster", "profile": "anthropic-generic", "endpoints": map[string]string{"anthropic": "http://vllm.models.svc:8000"},
		"propagate_trace": true,
	}), "an http endpoint"); traced["propagate_trace"] != true {
		t.Errorf("created with propagate_trace = %v", traced["propagate_trace"])
	}

	up := e.ok(e.admin("POST", "/admin/v1/providers/"+id, map[string]any{"name": "renamed", "stall_timeout_ms": nil, "enabled": false, "headers": nil, "propagate_trace": true}), "update")
	if up["name"] != "renamed" || up["stall_timeout_ms"] != nil || up["enabled"] != false || len(up["headers"].(map[string]any)) != 0 || up["propagate_trace"] != true {
		t.Fatalf("updated = %v", up)
	}
	e.refused(e.admin("POST", "/admin/v1/providers/"+id, map[string]any{"endpoints": deepseekBoth}), http.StatusBadRequest, "invalid_request_error", "fixed")
	e.refused(e.admin("POST", "/admin/v1/providers/"+id, map[string]any{"profile": "minimax"}), http.StatusBadRequest, "invalid_request_error", "fixed")
	e.refused(e.admin("POST", "/admin/v1/providers/"+id, map[string]any{"headers": map[string]string{"Cookie": "s"}}), http.StatusBadRequest, "invalid_request_error", "Cookie")
	e.refused(e.admin("POST", "/admin/v1/providers/"+id, map[string]any{"name": ""}), http.StatusBadRequest, "invalid_request_error", "name")
	e.refused(e.admin("POST", "/admin/v1/providers/"+id, map[string]any{"colour": "red"}), http.StatusBadRequest, "invalid_request_error", "colour")
	for _, field := range []string{"enabled", "name", "propagate_trace"} {
		e.refused(e.admin("POST", "/admin/v1/providers/"+id, map[string]any{field: nil}), http.StatusBadRequest, "invalid_request_error", "null")
	}
	e.refused(e.admin("POST", "/admin/v1/providers/gwprov_missing", map[string]any{"name": "x"}), http.StatusNotFound, "not_found_error", "gwprov_missing")
	if got := e.ok(e.admin("GET", "/admin/v1/providers/"+id, nil), "get"); got["name"] != "renamed" {
		t.Errorf("get = %v", got)
	}
	if n := len(e.admin("GET", "/admin/v1/providers", nil).list()); n != 2 {
		t.Errorf("list has %d providers, want 2", n)
	}
	dep := e.deployment(id, "chat")
	e.refused(e.admin("DELETE", "/admin/v1/providers/"+id, nil), http.StatusConflict, "invalid_request_error", dep)
	e.ok(e.admin("DELETE", "/admin/v1/deployments/"+dep, nil), "delete deployment")
	if got := e.ok(e.admin("DELETE", "/admin/v1/providers/"+id, nil), "delete"); got["type"] != "provider_deleted" || got["id"] != id {
		t.Errorf("delete = %v", got)
	}
	e.refused(e.admin("GET", "/admin/v1/providers/"+id, nil), http.StatusNotFound, "not_found_error", id)
}

// A credential's key is sealed and never returned; only its last four
// characters are shown.
func TestCredentials(t *testing.T) {
	e := newEnv(t, false)
	pid := e.provider("deepseek", map[string]string{"anthropic": "https://api.deepseek.com/anthropic"})
	const key = "sk-deepseek-0123456789abcdef"
	r := e.admin("POST", "/admin/v1/providers/"+pid+"/credentials", map[string]any{"key": key})
	c := e.ok(r, "create")
	if strings.Contains(r.raw, key) || strings.Contains(r.raw, "ciphertext") {
		t.Fatalf("the response carries the key: %s", r.raw)
	}
	if c["type"] != "credential" || c["last_four"] != "cdef" || c["weight"] != 1.0 || c["enabled"] != true ||
		len(c["protocols"].([]any)) != 1 || c["kind"] != "api_key" {
		t.Fatalf("created = %v", c)
	}
	cid := c["id"].(string)
	stored, err := e.store.GetCredential(context.Background(), pid, cid)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := e.cipher.Decrypt(context.Background(), stored.Ciphertext, stored.KeyID)
	if err != nil || string(plain) != key {
		t.Fatalf("sealed key = %q, %v", plain, err)
	}
	if short := e.ok(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials", map[string]any{"key": "sk-short"}), "short key"); short["last_four"] != "" {
		t.Errorf("a short key shows %q of itself", short["last_four"])
	}
	e.refused(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials", map[string]any{"key": key, "protocols": []string{"openai"}}),
		http.StatusBadRequest, "invalid_request_error", "openai")
	e.refused(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials", map[string]any{"key": "has space"}), http.StatusBadRequest, "invalid_request_error", "key")
	e.refused(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials", map[string]any{"key": key, "protocols": []string{}}),
		http.StatusBadRequest, "invalid_request_error", "empty")
	e.refused(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials", map[string]any{}), http.StatusBadRequest, "invalid_request_error", "key")
	e.refused(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials", map[string]any{"key": key, "weight": 0}), http.StatusBadRequest, "invalid_request_error", "weight")
	e.refused(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials", map[string]any{"key": key, "protocols": []string{"anthropic", "anthropic"}}),
		http.StatusBadRequest, "invalid_request_error", "twice")
	e.refused(e.admin("POST", "/admin/v1/providers/gwprov_missing/credentials", map[string]any{"key": key}), http.StatusNotFound, "not_found_error", "gwprov_missing")
	e.refused(e.admin("GET", "/admin/v1/providers/gwprov_missing/credentials", nil), http.StatusNotFound, "not_found_error", "gwprov_missing")

	up := e.ok(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials/"+cid, map[string]any{"weight": 3, "enabled": false}), "update")
	if up["weight"] != 3.0 || up["enabled"] != false {
		t.Fatalf("updated = %v", up)
	}
	e.refused(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials/"+cid, map[string]any{"key": "sk-new"}), http.StatusBadRequest, "invalid_request_error", "rotate")
	for _, field := range []string{"enabled", "weight"} {
		e.refused(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials/"+cid, map[string]any{field: nil}), http.StatusBadRequest, "invalid_request_error", "null")
	}
	e.refused(e.admin("POST", "/admin/v1/providers/"+pid+"/credentials/"+cid, map[string]any{"protocols": []string{}}),
		http.StatusBadRequest, "invalid_request_error", "empty")
	if n := len(e.admin("GET", "/admin/v1/providers/"+pid+"/credentials", nil).list()); n != 2 {
		t.Errorf("list has %d credentials, want 2", n)
	}
	if got := e.ok(e.admin("GET", "/admin/v1/providers/"+pid+"/credentials/"+cid, nil), "get"); got["id"] != cid {
		t.Errorf("get = %v", got)
	}
	e.ok(e.admin("DELETE", "/admin/v1/providers/"+pid+"/credentials/"+cid, nil), "delete")
	e.refused(e.admin("DELETE", "/admin/v1/providers/"+pid+"/credentials/"+cid, nil), http.StatusNotFound, "not_found_error", cid)
}

func TestDeployments(t *testing.T) {
	e := newEnv(t, false)
	pid := e.provider("deepseek", deepseekBoth)
	d := e.ok(e.admin("POST", "/admin/v1/deployments", map[string]any{
		"provider_id": pid, "upstream_model": "deepseek-v4-pro", "kind": "chat", "display_name": "V4 Pro",
		"capabilities": map[string]any{"tools": true, "thinking": true, "max_input_tokens": 128000},
		"prices":       map[string]any{"input": 0.28, "output": 1.1, "cache_write": 0},
	}), "create")
	id := d["id"].(string)
	if d["type"] != "deployment" || d["capabilities"].(map[string]any)["thinking"] != true ||
		d["prices"].(map[string]any)["input"] != 0.28 || d["prices"].(map[string]any)["cache_read"] != nil ||
		d["prices"].(map[string]any)["cache_write"] != 0.0 {
		t.Fatalf("created = %v", d)
	}
	for name, tc := range map[string]struct {
		body     map[string]any
		contains string
	}{
		"unknown provider":   {map[string]any{"provider_id": "gwprov_missing", "upstream_model": "m", "kind": "chat"}, "gwprov_missing"},
		"bad kind":           {map[string]any{"provider_id": pid, "upstream_model": "m", "kind": "image"}, "image"},
		"no model":           {map[string]any{"provider_id": pid, "kind": "chat"}, "upstream_model"},
		"negative price":     {map[string]any{"provider_id": pid, "upstream_model": "m", "kind": "chat", "prices": map[string]any{"output": -1}}, "output"},
		"price too high":     {map[string]any{"provider_id": pid, "upstream_model": "m", "kind": "chat", "prices": map[string]any{"input": 2e9}}, "input"},
		"price too small":    {map[string]any{"provider_id": pid, "upstream_model": "m", "kind": "chat", "prices": map[string]any{"cache_read": 1e-12}}, "cache_read"},
		"negative limit":     {map[string]any{"provider_id": pid, "upstream_model": "m", "kind": "chat", "capabilities": map[string]any{"max_tokens": -1}}, "max_tokens"},
		"unknown capability": {map[string]any{"provider_id": pid, "upstream_model": "m", "kind": "chat", "capabilities": map[string]any{"audio": true}}, "audio"},
	} {
		t.Run(name, func(t *testing.T) {
			e.refused(e.admin("POST", "/admin/v1/deployments", tc.body), http.StatusBadRequest, "invalid_request_error", tc.contains)
		})
	}
	up := e.ok(e.admin("POST", "/admin/v1/deployments/"+id, map[string]any{"display_name": "renamed", "prices": map[string]any{"input": 0.3}, "enabled": false}), "update")
	if up["display_name"] != "renamed" || up["prices"].(map[string]any)["output"] != nil || up["enabled"] != false {
		t.Fatalf("updated = %v", up)
	}
	for _, field := range []string{"provider_id", "upstream_model", "kind"} {
		e.refused(e.admin("POST", "/admin/v1/deployments/"+id, map[string]any{field: "x"}), http.StatusBadRequest, "invalid_request_error", "fixed")
	}
	for _, field := range []string{"enabled", "display_name", "capabilities", "prices"} {
		e.refused(e.admin("POST", "/admin/v1/deployments/"+id, map[string]any{field: nil}), http.StatusBadRequest, "invalid_request_error", "null")
	}
	e.ok(e.admin("POST", "/admin/v1/aliases", map[string]any{"name": "fast", "targets": []map[string]any{{"deployment_id": id}}}), "alias")
	e.refused(e.admin("DELETE", "/admin/v1/deployments/"+id, nil), http.StatusConflict, "invalid_request_error", "fast")
	if n := len(e.admin("GET", "/admin/v1/deployments", nil).list()); n != 1 {
		t.Errorf("list has %d deployments", n)
	}
	e.ok(e.admin("GET", "/admin/v1/deployments/"+id, nil), "get")
}

func TestAliases(t *testing.T) {
	e := newEnv(t, false)
	pid := e.provider("deepseek", deepseekBoth)
	chat1, chat2, emb := e.deployment(pid, "chat"), e.deployment(pid, "chat"), e.deployment(pid, "embedding")
	a := e.ok(e.admin("POST", "/admin/v1/aliases", map[string]any{
		"name": "Qwen/Qwen3-Coder", "display_name": "Coder",
		"targets": []map[string]any{{"deployment_id": chat1, "weight": 3}, {"deployment_id": chat2, "priority": 1}},
	}), "create")
	if a["type"] != "alias" || a["kind"] != "chat" || len(a["targets"].([]any)) != 2 ||
		a["targets"].([]any)[1].(map[string]any)["weight"] != 1.0 {
		t.Fatalf("created = %v", a)
	}
	if got := e.ok(e.admin("GET", "/admin/v1/aliases/Qwen/Qwen3-Coder", nil), "get by a name with a slash"); got["name"] != "Qwen/Qwen3-Coder" {
		t.Errorf("get = %v", got)
	}
	for name, tc := range map[string]struct {
		body     map[string]any
		status   int
		contains string
	}{
		"duplicate":      {map[string]any{"name": "Qwen/Qwen3-Coder", "targets": []map[string]any{{"deployment_id": chat1}}}, http.StatusConflict, "already exists"},
		"mixed kinds":    {map[string]any{"name": "m", "targets": []map[string]any{{"deployment_id": chat1}, {"deployment_id": emb}}}, http.StatusBadRequest, "kind"},
		"no targets":     {map[string]any{"name": "m"}, http.StatusBadRequest, "target"},
		"blank name":     {map[string]any{"name": "has space", "targets": []map[string]any{{"deployment_id": chat1}}}, http.StatusBadRequest, "name"},
		"empty segment":  {map[string]any{"name": "a//b", "targets": []map[string]any{{"deployment_id": chat1}}}, http.StatusBadRequest, "segment"},
		"dot segments":   {map[string]any{"name": "x/../y", "targets": []map[string]any{{"deployment_id": chat1}}}, http.StatusBadRequest, "segment"},
		"leading slash":  {map[string]any{"name": "/lead", "targets": []map[string]any{{"deployment_id": chat1}}}, http.StatusBadRequest, "segment"},
		"trailing slash": {map[string]any{"name": "trail/", "targets": []map[string]any{{"deployment_id": chat1}}}, http.StatusBadRequest, "segment"},
		"just dots":      {map[string]any{"name": "..", "targets": []map[string]any{{"deployment_id": chat1}}}, http.StatusBadRequest, "segment"},
		"ghost":          {map[string]any{"name": "m", "targets": []map[string]any{{"deployment_id": "gwdep_missing"}}}, http.StatusBadRequest, "gwdep_missing"},
		"weight zero":    {map[string]any{"name": "m", "targets": []map[string]any{{"deployment_id": chat1, "weight": 0}}}, http.StatusBadRequest, "weight"},
		"priority neg":   {map[string]any{"name": "m", "targets": []map[string]any{{"deployment_id": chat1, "priority": -1}}}, http.StatusBadRequest, "priority"},
	} {
		t.Run(name, func(t *testing.T) {
			e.refused(e.admin("POST", "/admin/v1/aliases", tc.body), tc.status, "invalid_request_error", tc.contains)
		})
	}
	e.ok(e.admin("POST", "/admin/v1/aliases", map[string]any{"name": "*", "targets": []map[string]any{{"deployment_id": chat2}}}), "wildcard")
	e.ok(e.admin("POST", "/admin/v1/aliases", map[string]any{"name": "embed", "targets": []map[string]any{{"deployment_id": emb}}}), "embedding")
	e.refused(e.admin("POST", "/admin/v1/aliases/embed", map[string]any{"targets": []map[string]any{{"deployment_id": e.deployment(pid, "embedding")}}}),
		http.StatusBadRequest, "invalid_request_error", "fixed")
	up := e.ok(e.admin("POST", "/admin/v1/aliases/Qwen/Qwen3-Coder", map[string]any{"display_name": "renamed", "targets": []map[string]any{{"deployment_id": chat2}}}), "update")
	if up["display_name"] != "renamed" || len(up["targets"].([]any)) != 1 {
		t.Fatalf("updated = %v", up)
	}
	e.refused(e.admin("POST", "/admin/v1/aliases/Qwen/Qwen3-Coder", map[string]any{"kind": "embedding"}), http.StatusBadRequest, "invalid_request_error", "fixed")
	e.refused(e.admin("POST", "/admin/v1/aliases/Qwen/Qwen3-Coder", map[string]any{"targets": nil}), http.StatusBadRequest, "invalid_request_error", "null")
	e.refused(e.admin("POST", "/admin/v1/aliases/Qwen/Qwen3-Coder", map[string]any{"name": "other"}), http.StatusBadRequest, "invalid_request_error", "fixed")
	if n := len(e.admin("GET", "/admin/v1/aliases", nil).list()); n != 3 {
		t.Errorf("list has %d aliases", n)
	}
	if got := e.ok(e.admin("DELETE", "/admin/v1/aliases/Qwen/Qwen3-Coder", nil), "delete"); got["type"] != "alias_deleted" {
		t.Errorf("delete = %v", got)
	}
	e.refused(e.admin("GET", "/admin/v1/aliases/Qwen/Qwen3-Coder", nil), http.StatusNotFound, "not_found_error", "Qwen/Qwen3-Coder")
}

func TestKeyPolicies(t *testing.T) {
	e := newEnv(t, false)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO api_keys (id, name, key_hash) VALUES ('key_app', 'app', 'h')`); err != nil {
		t.Fatal(err)
	}
	pid := e.provider("deepseek", deepseekBoth)
	e.ok(e.admin("POST", "/admin/v1/aliases", map[string]any{"name": "fast", "targets": []map[string]any{{"deployment_id": e.deployment(pid, "chat")}}}), "alias")
	put := func(body map[string]any) reply { return e.admin("POST", "/admin/v1/key_policies/key_app", body) }
	kp := e.ok(put(map[string]any{"aliases": []string{"fast"}, "rpm": 60, "tpm": 100000}), "put")
	if kp["type"] != "key_policy" || kp["api_key_id"] != "key_app" || kp["rpm"] != 60.0 || len(kp["aliases"].([]any)) != 1 {
		t.Fatalf("put = %v", kp)
	}
	// A grant is written whole, so a body that leaves a field out would
	// widen it silently: every field is named, null being every alias or no
	// limit.
	e.refused(put(map[string]any{"rpm": 120}), http.StatusBadRequest, "invalid_request_error", "aliases")
	e.refused(put(map[string]any{"aliases": []string{"fast"}, "rpm": 120}), http.StatusBadRequest, "invalid_request_error", "tpm")
	if got := e.ok(e.admin("GET", "/admin/v1/key_policies/key_app", nil), "get"); got["tpm"] != 100000.0 {
		t.Fatalf("a refused write changed the grant: %v", got)
	}
	all := e.ok(put(map[string]any{"aliases": nil, "rpm": nil, "tpm": nil}), "replace")
	if all["aliases"] != nil || all["rpm"] != nil || all["tpm"] != nil {
		t.Fatalf("replaced = %v", all)
	}
	whole := func(aliases any, rpm, tpm any) map[string]any {
		return map[string]any{"aliases": aliases, "rpm": rpm, "tpm": tpm}
	}
	e.refused(e.admin("POST", "/admin/v1/key_policies/key_missing", whole(nil, nil, nil)), http.StatusNotFound, "not_found_error", "key_missing")
	e.refused(put(whole([]string{}, nil, nil)), http.StatusBadRequest, "invalid_request_error", "DELETE")
	e.refused(put(whole([]string{"typo"}, nil, nil)), http.StatusBadRequest, "invalid_request_error", "typo")
	e.refused(put(whole([]string{"fast", "fast"}, nil, nil)), http.StatusBadRequest, "invalid_request_error", "twice")
	e.refused(put(whole(nil, 0, nil)), http.StatusBadRequest, "invalid_request_error", "rpm")
	e.refused(put(whole(nil, nil, -5)), http.StatusBadRequest, "invalid_request_error", "tpm")
	if n := len(e.admin("GET", "/admin/v1/key_policies", nil).list()); n != 1 {
		t.Errorf("list has %d policies", n)
	}
	e.ok(put(whole([]string{"fast"}, nil, nil)), "grant fast")
	e.refused(e.admin("DELETE", "/admin/v1/aliases/fast", nil), http.StatusConflict, "invalid_request_error", "key_app")
	e.ok(e.admin("DELETE", "/admin/v1/key_policies/key_app", nil), "delete")
	e.refused(e.admin("GET", "/admin/v1/key_policies/key_app", nil), http.StatusNotFound, "not_found_error", "key_app")
	e.ok(e.admin("DELETE", "/admin/v1/aliases/fast", nil), "an alias no grant names")
}

// Paths and methods the API does not serve answer in its error envelope.
func TestUnknownPathsAndMethods(t *testing.T) {
	e := newEnv(t, false)
	e.refused(e.admin("GET", "/admin/v1/nothing", nil), http.StatusNotFound, "not_found_error", "/admin/v1/nothing")
	e.refused(e.admin("PUT", "/admin/v1/providers", nil), http.StatusMethodNotAllowed, "invalid_request_error", "PUT")
	e.refused(e.admin("PATCH", "/admin/v1/aliases/a/b", nil), http.StatusMethodNotAllowed, "invalid_request_error", "PATCH")
	if r := e.do("GET", "/admin/v1/nothing", nil, nil); r.status != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request to an unknown path: %d, want 401 before anything about the path", r.status)
	}
}

// New refuses a configuration it could not serve safely.
func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher, _ := local.New(local.Config{KeyID: "k", Key: bytes.Repeat([]byte{1}, 32)})
	for name, cfg := range map[string]admin.Config{
		"no store":         {Cipher: cipher, BootstrapKey: bootstrap},
		"no cipher":        {Store: store.New(pool), BootstrapKey: bootstrap},
		"no bootstrap key": {Store: store.New(pool), Cipher: cipher},
	} {
		if _, err := admin.New(cfg); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}

// Behind an identity-aware proxy the assertion header is the operator's
// credential, a Bearer never is, and a repeated assertion is none.
func TestAuthBehindAProxy(t *testing.T) {
	pool := pgtest.NewPool(t)
	cipher, _ := local.New(local.Config{KeyID: "k", Key: bytes.Repeat([]byte{3}, 32)})
	idp := identitytest.NewIdP(t)
	clock := identitytest.NewClock(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	const header = "X-Goog-Iap-Jwt-Assertion"
	v, err := identity.New(context.Background(), identity.Config{
		Mode: identity.ModeTrustedProxy, AssertionHeader: header, Issuer: idp.Issuer(), Audience: audience,
		JWKSURL: idp.JWKSURL(), RoleMap: map[string]identity.Role{"admins": identity.RoleAdmin},
		HTTPClient: idp.Client(), Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := admin.New(admin.Config{Store: store.New(pool), Cipher: cipher, Verifier: v, BootstrapKey: bootstrap})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	claims := idp.Claims(audience, clock.Now())
	claims["roles"] = []any{"admins"}
	tok := idp.Mint(t, claims)
	get := func(set func(http.Header)) int {
		req, _ := http.NewRequest("GET", srv.URL+"/admin/v1/providers", nil)
		set(req.Header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(func(h http.Header) { h.Set(header, tok) }); got != http.StatusOK {
		t.Errorf("assertion header: %d, want 200", got)
	}
	if got := get(func(h http.Header) { h.Set("Authorization", "Bearer "+tok) }); got != http.StatusUnauthorized {
		t.Errorf("the same token as a Bearer: %d, want 401", got)
	}
	if got := get(func(h http.Header) { h.Add(header, tok); h.Add(header, tok) }); got != http.StatusUnauthorized {
		t.Errorf("a repeated assertion: %d, want 401", got)
	}
}
