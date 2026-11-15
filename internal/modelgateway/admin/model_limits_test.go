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

func TestModelLimitsBehindProxyUseOnlyCookieAuthority(t *testing.T) {
	e, m := newCookieAdmin(t, login.ModeTrustedProxy, cookieOrigin, true)
	ctx := t.Context()
	if _, err := e.pool.Exec(ctx, `INSERT INTO principals (id,issuer,subject,kind,role)
		VALUES ('principal_budget','cookie','admin','user','admin')`); err != nil {
		t.Fatal(err)
	}
	issued, err := m.Issue(ctx, "principal_budget")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"requests_per_minute":5,"input_tokens_per_minute":null,"output_tokens_per_minute":null}`
	assertion := e.token("admins")
	for _, token := range []string{assertion, "invalid-proxy-assertion"} {
		headers := cookieAdminHeaders(issued)
		headers.Set("X-Goog-IAP-JWT-Assertion", token)
		for _, method := range []string{"GET", "POST"} {
			path := "/admin/v1/rate_limits"
			if method == "POST" {
				path += "/fast"
			}
			status, _, raw := cookieAdminRequest(t, e, method, path, body, headers)
			if status != http.StatusOK {
				t.Errorf("cookie with incidental assertion: %s %d %s", method, status, raw)
			}
		}
	}
	headers := cookieAdminHeaders(issued)
	headers.Set("X-Goog-IAP-JWT-Assertion", assertion)
	for _, tc := range []struct {
		name   string
		change func(http.Header)
		want   int
	}{
		{"assertion-only", func(h http.Header) { h.Del("Cookie") }, http.StatusUnauthorized},
		{"invalid-cookie", func(h http.Header) { h.Set("Cookie", "map_session=invalid") }, http.StatusUnauthorized},
		{"missing-csrf", func(h http.Header) { h.Del("X-Csrf-Token") }, http.StatusForbidden},
		{"foreign-origin", func(h http.Header) { h.Set("Origin", "https://other.example") }, http.StatusForbidden},
		{"explicit-key", func(h http.Header) { h.Set("X-Api-Key", bootstrap) }, http.StatusForbidden},
		{"explicit-bearer", func(h http.Header) { h.Set("Authorization", "Bearer "+bootstrap) }, http.StatusForbidden},
		{"operator-bearer", func(h http.Header) { h.Set("Authorization", "Bearer "+assertion) }, http.StatusUnauthorized},
	} {
		h := headers.Clone()
		tc.change(h)
		status, _, raw := cookieAdminRequest(t, e, "POST", "/admin/v1/rate_limits/fast", body, h)
		if status != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, status, raw, tc.want)
		}
	}
	if _, err := e.pool.Exec(ctx, `UPDATE principals SET role='developer' WHERE id='principal_budget'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/admin/v1/rate_limits", http.StatusOK}, {"POST", "/admin/v1/rate_limits/fast", http.StatusForbidden}} {
		status, _, raw := cookieAdminRequest(t, e, tc.method, tc.path, body, headers)
		if status != tc.want {
			t.Errorf("current developer with admin assertion: %d %s, want %d", status, raw, tc.want)
		}
	}
	var count int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM principals`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("incidental assertion signed in a principal: count=%d err=%v", count, err)
	}
}

func TestModelLimitRedirectsDoNotReplayProxySignIn(t *testing.T) {
	e, m := newCookieAdmin(t, login.ModeTrustedProxy, cookieOrigin, true)
	ctx := t.Context()
	if _, err := e.pool.Exec(ctx, `INSERT INTO principals (id,issuer,subject,kind,role)
		VALUES ('principal_budget','cookie','admin','user','admin')`); err != nil {
		t.Fatal(err)
	}
	issued, err := m.Issue(ctx, "principal_budget")
	if err != nil {
		t.Fatal(err)
	}
	assertion := e.token("admins")
	for _, path := range []string{
		"/admin//v1/rate_limits", "/admin/v1/x/../rate_limits",
		"/admin/v1/%72ate_limits", "/admin/v1/x/../%72ate_limits",
		"/admin/v1/rate_limits/../providers",
	} {
		headers := http.Header{"X-Goog-IAP-JWT-Assertion": {assertion}}
		status, _, raw := cookieAdminRequest(t, e, "GET", path, "", headers)
		if status != http.StatusUnauthorized {
			t.Errorf("assertion-only %s: %d %s", path, status, raw)
		}
		headers.Set("Cookie", "map_session="+issued.Token)
		status, _, raw = cookieAdminRequest(t, e, "GET", path, "", headers)
		if status != http.StatusOK && status != http.StatusTemporaryRedirect {
			t.Errorf("cookie %s: %d %s", path, status, raw)
		}
	}
	var count int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM principals`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("redirect replayed proxy sign-in: count=%d err=%v", count, err)
	}
}

func TestModelLimitReplacementRejectsAmbiguousFields(t *testing.T) {
	f := newGrantFixture(t, t.Context())
	path := "/admin/v1/rate_limits/fast"
	body := `{"requests_per_minute":5,"input_tokens_per_minute":null,"output_tokens_per_minute":null}`
	if rr := grantCall(t.Context(), f.handler, "POST", path, strings.NewReader(body), cookieAdminHeaders(f.issued)); rr.Code != http.StatusOK {
		t.Fatalf("control: %d %s", rr.Code, rr.Body.String())
	}
	for _, field := range []string{"REQUESTS_PER_MINUTE", "requests_per_minute", `requests_per_min\u0075te`} {
		bad := strings.TrimSuffix(body, "}") + `,"` + field + `":null}`
		rr := grantCall(t.Context(), f.handler, "POST", path, strings.NewReader(bad), cookieAdminHeaders(f.issued))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("ambiguous %s: %d %s", field, rr.Code, rr.Body.String())
		}
	}
	limits, err := f.e.store.ListModelLimits(t.Context())
	if err != nil || len(limits) != 1 || limits[0].RPM == nil || *limits[0].RPM != 5 {
		t.Fatalf("ambiguous replacement changed the budget: %+v %v", limits, err)
	}
}
