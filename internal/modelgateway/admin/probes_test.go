package admin_test

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// stubProber stands in for the gateway's request path, recording what each
// route handed it.
type stubProber struct {
	mu      sync.Mutex
	checked [][]string // per call: the provider, then the credential
	tested  [][]string // per call: the deployment, its provider, then each credential
}

func (s *stubProber) CheckCredential(_ context.Context, p store.Provider, c store.Credential) []admin.CredentialCheck {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = append(s.checked, []string{p.ID, c.ID})
	return []admin.CredentialCheck{{Protocol: c.Protocols[0], OK: true, Models: []string{"m"}}}
}

func (s *stubProber) TestDeployment(_ context.Context, d store.Deployment, p store.Provider, cs []store.Credential) []admin.DeploymentTest {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := []string{d.ID, p.ID}
	out := []admin.DeploymentTest{}
	for _, c := range cs {
		ids = append(ids, c.ID)
		out = append(out, admin.DeploymentTest{CredentialID: c.ID, OK: true})
	}
	s.tested = append(s.tested, ids)
	return out
}

func (s *stubProber) calls() (checked, tested [][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.checked...), append([][]string(nil), s.tested...)
}

func (e *env) credential(providerID string, enabled bool) string {
	e.t.Helper()
	b := e.ok(e.admin("POST", "/admin/v1/providers/"+providerID+"/credentials",
		map[string]any{"key": "sk-upstream-0123456789", "enabled": enabled}), "create credential")
	return b["id"].(string)
}

// A check hands the prober the provider and the credential its path names,
// disabled or not, and answers what the prober found under the two ids.
func TestTheCheckRoute(t *testing.T) {
	e := newEnv(t, false)
	p := e.provider("deepseek", deepseekBoth)
	c := e.credential(p, false)
	other := e.credential(e.provider("deepseek", deepseekBoth), true)
	path := "/admin/v1/providers/" + p + "/credentials/" + c + "/check"

	for _, body := range []any{nil, "{}", " \n"} {
		got := e.ok(e.admin("POST", path, body), "check")
		if got["type"] != "credential_check" || got["provider_id"] != p || got["credential_id"] != c {
			t.Fatalf("check = %v", got)
		}
		res := got["results"].([]any)
		if len(res) != 1 || res[0].(map[string]any)["ok"] != true {
			t.Errorf("results = %v", res)
		}
	}
	if checked, _ := e.prober.calls(); len(checked) != 3 || !reflect.DeepEqual(checked[0], []string{p, c}) {
		t.Errorf("the prober was handed %v", checked)
	}

	e.refused(e.admin("POST", "/admin/v1/providers/"+p+"/credentials/"+other+"/check", nil), http.StatusNotFound, "not_found_error", other)
	e.refused(e.admin("POST", "/admin/v1/providers/gwprov_nope/credentials/"+c+"/check", nil), http.StatusNotFound, "not_found_error", "gwprov_nope")
	e.refused(e.admin("POST", path, `{"protocol":"openai"}`), http.StatusBadRequest, "invalid_request_error", "protocol")
	e.refused(e.admin("POST", path, `[]`), http.StatusBadRequest, "invalid_request_error", "JSON")
	e.refused(e.admin("GET", path, nil), http.StatusMethodNotAllowed, "invalid_request_error", "POST")
	if checked, _ := e.prober.calls(); len(checked) != 3 {
		t.Errorf("a refused check reached the prober: %v", checked)
	}
}

// A test hands the prober the deployment, its provider and the credential
// the body names, disabled or not, or else every enabled credential of the
// provider, in the order they were added.
func TestTheTestRoute(t *testing.T) {
	e := newEnv(t, false)
	p := e.provider("deepseek", deepseekBoth)
	d := e.deployment(p, "chat")
	c1 := e.credential(p, true)
	off := e.credential(p, false)
	c3 := e.credential(p, true)
	path := "/admin/v1/deployments/" + d + "/test"

	got := e.ok(e.admin("POST", path, nil), "test")
	if got["type"] != "deployment_test" || got["deployment_id"] != d || len(got["results"].([]any)) != 2 {
		t.Fatalf("test = %v", got)
	}
	e.ok(e.admin("POST", path, map[string]any{"credential_id": off}), "test the disabled one")
	e.ok(e.admin("POST", path, `{"credential_id":null}`), "a null credential_id is none")
	_, tested := e.prober.calls()
	want := [][]string{{d, p, c1, c3}, {d, p, off}, {d, p, c1, c3}}
	if !reflect.DeepEqual(tested, want) {
		t.Errorf("the prober was handed %v, want %v", tested, want)
	}

	other := e.credential(e.provider("deepseek", deepseekBoth), true)
	e.refused(e.admin("POST", path, map[string]any{"credential_id": other}), http.StatusBadRequest, "invalid_request_error", other)
	e.refused(e.admin("POST", path, map[string]any{"credential_id": ""}), http.StatusBadRequest, "invalid_request_error", "credential_id")
	e.refused(e.admin("POST", path, `{"credential":"x"}`), http.StatusBadRequest, "invalid_request_error", "credential")
	e.refused(e.admin("POST", "/admin/v1/deployments/gwdep_nope/test", nil), http.StatusNotFound, "not_found_error", "gwdep_nope")
	e.refused(e.admin("GET", path, nil), http.StatusMethodNotAllowed, "invalid_request_error", "POST")
	lonely := e.deployment(e.provider("deepseek", deepseekBoth), "chat")
	e.refused(e.admin("POST", "/admin/v1/deployments/"+lonely+"/test", nil), http.StatusBadRequest, "invalid_request_error", "no enabled credential")
	if _, tested := e.prober.calls(); len(tested) != 3 {
		t.Errorf("a refused test reached the prober: %v", tested)
	}
}

// Both routes call the vendor with the provider's key, so they need the
// admin role, as the writes do.
func TestTheProbeRoutesNeedTheAdminRole(t *testing.T) {
	e := newEnv(t, true)
	p := e.provider("deepseek", deepseekBoth)
	c := e.credential(p, true)
	d := e.deployment(p, "chat")
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	for _, path := range []string{"/admin/v1/providers/" + p + "/credentials/" + c + "/check", "/admin/v1/deployments/" + d + "/test"} {
		// A user is turned away before routing, so its refusal names the floor.
		e.refused(e.do("POST", path, nil, bearer(e.token("readers"))), http.StatusForbidden, "permission_error", "developer")
		e.refused(e.do("POST", path, nil, bearer(e.token("devs"))), http.StatusForbidden, "permission_error", "admin")
		e.ok(e.do("POST", path, nil, bearer(e.token("admins"))), "admin on "+path)
	}
	if checked, tested := e.prober.calls(); len(checked) != 1 || len(tested) != 1 {
		t.Errorf("checked %v, tested %v: want one each", checked, tested)
	}
}
