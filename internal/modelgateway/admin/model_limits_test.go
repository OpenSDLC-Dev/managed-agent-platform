package admin_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

func TestModelLimitsRequireCurrentCookieAuthority(t *testing.T) {
	e, m := newCookieAdmin(t, login.ModePassword, cookieOrigin, false)
	ctx := context.Background()
	_, err := e.pool.Exec(ctx, `INSERT INTO principals (id,issuer,subject,kind,role) VALUES
		('principal_admin','limits','admin','user','admin'),
		('principal_dev','limits','dev','user','developer')`)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := m.Issue(ctx, "principal_admin")
	if err != nil {
		t.Fatal(err)
	}
	cookie := cookieAdminHeaders(issued)
	body := `{"requests_per_minute":5,"input_tokens_per_minute":1000,"output_tokens_per_minute":null}`
	status, _, raw := cookieAdminRequest(t, e, "POST", "/admin/v1/rate_limits/vendor/fast", body, cookie)
	if status != 200 || !strings.Contains(raw, `"model":"vendor/fast"`) {
		t.Fatalf("write: %d %s", status, raw)
	}
	for _, bad := range []string{`{}`, `{"requests_per_minute":0,"input_tokens_per_minute":null,"output_tokens_per_minute":null}`, `{"requests_per_minute":null,"input_tokens_per_minute":null,"output_tokens_per_minute":null,"unknown":1}`} {
		status, _, raw = cookieAdminRequest(t, e, "POST", "/admin/v1/rate_limits/fast", bad, cookie)
		if status != 400 {
			t.Fatalf("invalid write: %d %s", status, raw)
		}
	}
	dev, err := m.Issue(ctx, "principal_dev")
	if err != nil {
		t.Fatal(err)
	}
	status, _, raw = cookieAdminRequest(t, e, "GET", "/admin/v1/rate_limits", "", cookieAdminHeaders(dev))
	if status != 200 || !strings.Contains(raw, `"requests_per_minute":5`) {
		t.Fatalf("developer read: %d %s", status, raw)
	}
	status, _, raw = cookieAdminRequest(t, e, "POST", "/admin/v1/rate_limits/fast", body, cookieAdminHeaders(dev))
	if status != http.StatusForbidden {
		t.Fatalf("developer write: %d %s", status, raw)
	}
	status, _, raw = cookieAdminRequest(t, e, "POST", "/admin/v1/rate_limits/fast", body, http.Header{"X-Api-Key": {bootstrap}})
	if status != http.StatusForbidden {
		t.Fatalf("legacy credential: %d %s", status, raw)
	}
	_, err = e.pool.Exec(ctx, `UPDATE principals SET role='developer' WHERE id='principal_admin'`)
	if err != nil {
		t.Fatal(err)
	}
	status, _, raw = cookieAdminRequest(t, e, "POST", "/admin/v1/rate_limits/fast", body, cookie)
	if status != http.StatusForbidden {
		t.Fatalf("demoted admin: %d %s", status, raw)
	}
}

func TestModelLimitMutationRechecksAfterBody(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	f := newGrantFixture(t, ctx)
	body := newGrantBody(ctx)
	body.prefix = strings.NewReader(`{"requests_per_minute":null,"input_tokens_per_minute":null,"output_tokens_per_minute":null`)
	call := startGrantRequest(t, ctx, f.handler, "POST", "/admin/v1/rate_limits/fast", body, cookieAdminHeaders(f.issued))
	call.await(t, body.entered, "body after admin admission")
	if _, err := f.e.pool.Exec(ctx, `UPDATE principals SET role='developer' WHERE id=$1`, f.caller); err != nil {
		t.Fatal(err)
	}
	body.unblock()
	rr := call.result(t)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("demotion during body: %d %s", rr.Code, rr.Body.String())
	}
	limits, err := f.e.store.ListModelLimits(ctx)
	if err != nil || len(limits) != 0 {
		t.Fatalf("unauthorized limit committed: %+v %v", limits, err)
	}
}

func TestModelLimitNamesMatchPublicAliases(t *testing.T) {
	f := newGrantFixture(t, t.Context())
	body := `{"requests_per_minute":5,"input_tokens_per_minute":null,"output_tokens_per_minute":null}`
	for _, tc := range []struct {
		name string
		want int
	}{
		{strings.Repeat("界", 256), http.StatusOK},
		{strings.Repeat("界", 257), http.StatusBadRequest},
		{"has space", http.StatusBadRequest},
	} {
		rr := grantCall(t.Context(), f.handler, "POST", "/admin/v1/rate_limits/"+strings.ReplaceAll(tc.name, " ", "%20"), strings.NewReader(body), cookieAdminHeaders(f.issued))
		if rr.Code != tc.want {
			t.Errorf("%d-rune alias: status=%d want=%d body=%s", len([]rune(tc.name)), rr.Code, tc.want, rr.Body.String())
		}
	}
	rr := grantCall(t.Context(), f.handler, "GET", "/admin/v1/rate_limits", nil, cookieAdminHeaders(f.issued))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), strings.Repeat("界", 256)) {
		t.Fatalf("Unicode alias round trip: %d %s", rr.Code, rr.Body.String())
	}
}
