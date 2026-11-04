package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

// The handler receives a valid JSON prefix before the barrier. Only its own
// second Read signals admission; no transport can buffer the body ahead of auth.
type credentialBody struct {
	ctx                      context.Context
	prefix, tail             *strings.Reader
	entered, release, eof    chan struct{}
	releaseOnce, enteredOnce sync.Once
	eofOnce                  sync.Once
}

func newCredentialBody(ctx context.Context, body string) *credentialBody {
	return &credentialBody{ctx: ctx, prefix: strings.NewReader(body[:len(body)-1]), tail: strings.NewReader(body[len(body)-1:]),
		entered: make(chan struct{}), release: make(chan struct{}), eof: make(chan struct{})}
}

func (b *credentialBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	b.enteredOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
	n, err := b.tail.Read(p)
	if err == io.EOF {
		b.eofOnce.Do(func() { close(b.eof) })
	}
	return n, err
}

func (b *credentialBody) Close() error { b.unblock(); return nil }
func (b *credentialBody) unblock()     { b.releaseOnce.Do(func() { close(b.release) }) }

type credentialRequest struct {
	ctx  context.Context
	done chan struct{}
	rr   *httptest.ResponseRecorder
}

func credentialCall(ctx context.Context, h http.Handler, method, path string, body io.Reader, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, nativeOrigin+path, body).WithContext(ctx)
	r.Header = headers.Clone()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func startCredentialRequest(t *testing.T, ctx context.Context, h http.Handler, method, path string, body io.ReadCloser, headers http.Header) *credentialRequest {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	call := &credentialRequest{ctx: ctx, done: make(chan struct{})}
	go func() {
		defer close(call.done)
		call.rr = credentialCall(ctx, h, method, path, body, headers)
	}()
	t.Cleanup(func() {
		cancel()
		if body != nil {
			body.Close()
		}
		select {
		case <-call.done:
		case <-time.After(5 * time.Second):
			t.Error("credential request did not stop after cancellation and body release")
		}
	})
	return call
}

func (r *credentialRequest) await(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
		return
	case <-r.done:
		t.Fatalf("request ended before %s: status=%d body=%s", what, r.rr.Code, r.rr.Body.String())
	case <-r.ctx.Done():
		t.Fatalf("request did not reach %s: %v", what, r.ctx.Err())
	}
}

func (r *credentialRequest) result(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case <-r.done:
		return r.rr
	case <-r.ctx.Done():
		t.Fatalf("credential request did not finish: %v", r.ctx.Err())
		return nil
	}
}

func newCredentialHandler(t *testing.T) (*nativeServer, http.Handler) {
	t.Helper()
	pool := newPoolWithKey(t)
	m := login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: nativeOrigin, Development: true})
	s := &nativeServer{tserver: &tserver{t: t, pool: pool}, manager: m}
	return s, api.NewHandler(pool, nil, nil, nil, api.WithLogin(m, nil))
}

// Every withdrawal is committed while the admitted mutation is still in Read.
func withdrawCredentialCaller(t *testing.T, ctx context.Context, s *nativeServer, h http.Handler, caller, withdrawal string, headers http.Header) (int, string) {
	t.Helper()
	want, marker := 403, ""
	if withdrawal == "logout" {
		rr := credentialCall(ctx, h, "POST", "/api/auth/logout", nil, headers)
		if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"ok":true}` {
			t.Fatalf("logout did not commit: status=%d body=%s", rr.Code, rr.Body.String())
		}
		var rows int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM browser_sessions WHERE principal_id=$1`, caller).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("logout left %d sessions: %v", rows, err)
		}
		return 401, "1"
	}
	query := map[string]string{
		"demote":     `UPDATE principals SET role='developer' WHERE id=$1`,
		"deactivate": `UPDATE principals SET status='deactivated' WHERE id=$1`,
		"expire":     `UPDATE browser_sessions SET created_at=clock_timestamp()-interval '13 hours',expires_at=clock_timestamp()-interval '1 second' WHERE principal_id=$1`,
	}[withdrawal]
	// Exec completes the autocommit before a separate read proves its effect.
	tag, err := s.pool.Exec(ctx, query, caller)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("%s affected %d rows: %v", withdrawal, tag.RowsAffected(), err)
	}
	var committed bool
	verify := map[string]string{
		"demote":     `SELECT role='developer' FROM principals WHERE id=$1`,
		"deactivate": `SELECT status='deactivated' FROM principals WHERE id=$1`,
		"expire":     `SELECT expires_at<clock_timestamp() FROM browser_sessions WHERE principal_id=$1`,
	}[withdrawal]
	if err := s.pool.QueryRow(ctx, verify, caller).Scan(&committed); err != nil || !committed {
		t.Fatalf("%s was not committed: effect=%v err=%v", withdrawal, committed, err)
	}
	if withdrawal == "deactivate" {
		marker = "1"
	}
	if withdrawal == "expire" {
		want, marker = 401, "1"
	}
	return want, marker
}

type credentialTarget struct {
	operation, path, body, keyID, secret, envID string
	managementRows                              int
}

func prepareCredentialTarget(t *testing.T, ctx context.Context, s *nativeServer, h http.Handler, caller, operation string) credentialTarget {
	t.Helper()
	target := credentialTarget{operation: operation, path: consoleAPIKeysPath, body: `{"name":"delayed-issued"}`}
	switch operation {
	case "reactivate-org", "reactivate-workspace":
		expires := time.Now().Add(time.Hour)
		secret, row, err := api.IssueManagementKey(ctx, s.pool, "delayed-target", &expires, caller)
		if err != nil {
			t.Fatal(err)
		}
		target.keyID, target.secret, target.body = row.ID, secret, `{"status":"active"}`
		target.path = consoleOrgAPIKey(row.ID)
		if operation == "reactivate-workspace" {
			target.path = consoleAPIKey(row.ID)
		}
		if tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET status='inactive' WHERE id=$1`, row.ID); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("disable target: rows=%d err=%v", tag.RowsAffected(), err)
		}
		var valid bool
		if err := s.pool.QueryRow(ctx, `SELECT status='inactive' AND created_by IS NOT NULL AND expires_at>clock_timestamp() FROM api_keys WHERE id=$1`, row.ID).Scan(&valid); err != nil || !valid {
			t.Fatalf("reactivation target is not issued, inactive and unexpired: valid=%v err=%v", valid, err)
		}
		assertCredentialLane(t, ctx, h, secret, "", 401)
	case "worker-issue":
		rr := credentialCall(ctx, h, "POST", "/v1/environments", strings.NewReader(`{"name":"credential-target","config":{"type":"self_hosted"}}`), http.Header{"X-Api-Key": {testKey}, "Content-Type": {"application/json"}})
		var env struct {
			ID string `json:"id"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &env) != nil || env.ID == "" {
			t.Fatalf("create worker environment: status=%d body=%s", rr.Code, rr.Body.String())
		}
		target.envID, target.path, target.body = env.ID, consoleTokens(env.ID), `{"name":"delayed-worker"}`
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&target.managementRows); err != nil {
		t.Fatal(err)
	}
	return target
}

func assertCredentialLane(t *testing.T, ctx context.Context, h http.Handler, secret, envID string, want int) {
	t.Helper()
	path, headers := "/v1/agents", http.Header{"X-Api-Key": {secret}}
	if envID != "" {
		path, headers = "/v1/environments/"+envID+"/work/stats", http.Header{"Authorization": {"Bearer " + secret}}
	}
	rr := credentialCall(ctx, h, "GET", path, nil, headers)
	if rr.Code != want {
		t.Errorf("credential lane %s: status=%d want=%d body=%s", path, rr.Code, want, rr.Body.String())
	}
}

func (target credentialTarget) assertState(t *testing.T, ctx context.Context, s *nativeServer, h http.Handler, rr *httptest.ResponseRecorder, changed bool, issuer string) {
	t.Helper()
	var managementRows, workerRows int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM api_keys),(SELECT count(*) FROM environment_keys)`).Scan(&managementRows, &workerRows); err != nil {
		t.Fatal(err)
	}
	wantManagement, wantWorkers := target.managementRows, 0
	if changed && target.operation == "management-issue" {
		wantManagement++
	}
	if changed && target.operation == "worker-issue" {
		wantWorkers = 1
	}
	if managementRows != wantManagement || workerRows != wantWorkers {
		t.Errorf("durable rows: management=%d worker=%d; want management=%d worker=%d", managementRows, workerRows, wantManagement, wantWorkers)
	}
	if target.keyID != "" {
		want, lane := "inactive", 401
		if changed {
			want, lane = "active", 200
		}
		var status string
		if err := s.pool.QueryRow(ctx, `SELECT status FROM api_keys WHERE id=$1`, target.keyID).Scan(&status); err != nil || status != want {
			t.Errorf("reactivation target=%q want=%q err=%v", status, want, err)
		}
		assertCredentialLane(t, ctx, h, target.secret, "", lane)
		return
	}
	if !changed || rr.Code != 200 {
		return
	}
	var issued struct {
		ID          string `json:"id"`
		RawKey      string `json:"raw_key"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	var stored bool
	if target.operation == "management-issue" {
		if issued.ID == "" || issued.RawKey == "" {
			t.Fatal("successful management issuance returned no id or synthetic key")
		}
		if err := s.pool.QueryRow(ctx, `SELECT name='delayed-issued' AND status='active' AND created_by=$2 AND key_hash=$3 FROM api_keys WHERE id=$1`, issued.ID, issuer, apikey.Hash(issued.RawKey)).Scan(&stored); err != nil || !stored {
			t.Fatalf("issued management row=%v err=%v", stored, err)
		}
		assertCredentialLane(t, ctx, h, issued.RawKey, "", 200)
	} else {
		if issued.AccessToken == "" {
			t.Fatal("successful worker issuance returned no synthetic access token")
		}
		if err := s.pool.QueryRow(ctx, `SELECT name='delayed-worker' AND environment_id=$1 AND expires_at>clock_timestamp() FROM environment_keys WHERE key_hash=$2`, target.envID, apikey.Hash(issued.AccessToken)).Scan(&stored); err != nil || !stored {
			t.Fatalf("issued worker row=%v err=%v", stored, err)
		}
		assertCredentialLane(t, ctx, h, issued.AccessToken, target.envID, 200)
	}
}

// An admission-time role, or a missing post-upload session check, fails every
// withdrawal row. The identical unchanged request proves the body and target work.
func TestNativeCredentialMutationDelayedBody(t *testing.T) {
	for _, operation := range []string{"management-issue", "reactivate-org", "reactivate-workspace", "worker-issue"} {
		for _, withdrawal := range []string{"unchanged", "logout", "demote", "deactivate", "expire"} {
			t.Run(operation+"/"+withdrawal, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				s, h := newCredentialHandler(t)
				caller := seedMember(t, s, "credential-caller", "admin", "console")
				cookie, session := s.issue(caller)
				headers := nativeHeaders(cookie, session.CSRFToken)
				target := prepareCredentialTarget(t, ctx, s, h, caller, operation)
				body := newCredentialBody(ctx, target.body)
				call := startCredentialRequest(t, ctx, h, "POST", target.path, body, headers)
				call.await(t, body.entered, "partial body after admin admission")
				want, marker := 200, ""
				if withdrawal != "unchanged" {
					want, marker = withdrawCredentialCaller(t, ctx, s, h, caller, withdrawal, headers)
				}
				body.unblock()
				rr := call.result(t)
				select {
				case <-body.eof:
				default:
					t.Error("admitted request did not consume the complete valid body")
				}
				if rr.Code != want || rr.Header().Get("x-platform-signed-out") != marker {
					t.Errorf("post-body authority: status=%d marker=%q want=%d/%q body=%s", rr.Code, rr.Header().Get("x-platform-signed-out"), want, marker, rr.Body.String())
				}
				target.assertState(t, ctx, s, h, rr, withdrawal == "unchanged", caller)
				if withdrawal == "unchanged" && !t.Failed() {
					fmt.Printf("CONTROL_ASSERTIONS_PASSED %s\n", t.Name())
				}
			})
		}
	}
}

// A cookie-only fix on these legacy-capable routes would reject the legitimate
// x-api-key machine issuer, whose independently offered credential outranks cookies.
func TestNativeCredentialMutationLegacyPrecedence(t *testing.T) {
	for _, operation := range []string{"management-issue", "reactivate-org", "reactivate-workspace", "worker-issue"} {
		t.Run(operation+"/X-Api-Key", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			s, h := newCredentialHandler(t)
			caller := seedMember(t, s, "legacy-cookie", "admin", "console")
			cookie, session := s.issue(caller)
			headers := nativeHeaders(cookie, session.CSRFToken)
			target := prepareCredentialTarget(t, ctx, s, h, caller, operation)
			offered := headers.Clone()
			offered.Set("X-Api-Key", testKey)
			body := newCredentialBody(ctx, target.body)
			call := startCredentialRequest(t, ctx, h, "POST", target.path, body, offered)
			call.await(t, body.entered, "partial body on the legacy machine lane")
			withdrawCredentialCaller(t, ctx, s, h, caller, "logout", headers)
			body.unblock()
			rr := call.result(t)
			select {
			case <-body.eof:
			default:
				t.Error("legacy request did not consume the complete valid body")
			}
			if rr.Code != 200 || rr.Header().Get("x-platform-signed-out") != "" {
				t.Errorf("legacy credential lost precedence: status=%d marker=%q body=%s", rr.Code, rr.Header().Get("x-platform-signed-out"), rr.Body.String())
			}
			var issuer string
			if err := s.pool.QueryRow(ctx, `SELECT id FROM api_keys WHERE key_hash=$1`, apikey.Hash(testKey)).Scan(&issuer); err != nil {
				t.Fatal(err)
			}
			target.assertState(t, ctx, s, h, rr, true, issuer)
			if !t.Failed() {
				fmt.Printf("CONTROL_ASSERTIONS_PASSED %s\n", t.Name())
			}
		})
	}
}

// A management key is not a generic Bearer credential. Offering it must prevent
// cookie fallback without reading a valid mutation body or signing out the browser.
func TestNativeCredentialMutationRejectsManagementBearer(t *testing.T) {
	for _, operation := range []string{"management-issue", "reactivate-org", "reactivate-workspace", "worker-issue"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			s, h := newCredentialHandler(t)
			caller := seedMember(t, s, "bearer-cookie", "admin", "console")
			cookie, session := s.issue(caller)
			headers := nativeHeaders(cookie, session.CSRFToken)
			target := prepareCredentialTarget(t, ctx, s, h, caller, operation)
			cookieOnly := credentialCall(ctx, h, "GET", "/api/auth/session", nil, headers)
			if cookieOnly.Code != 200 || cookieOnly.Header().Get("x-platform-signed-out") != "" {
				t.Fatalf("cookie-only control was not authenticated: status=%d body=%s", cookieOnly.Code, cookieOnly.Body.String())
			}
			offered := headers.Clone()
			offered.Set("Authorization", "Bearer "+testKey)
			body := newCredentialBody(ctx, target.body)
			call := startCredentialRequest(t, ctx, h, "POST", target.path, body, offered)
			select {
			case <-call.done:
			case <-body.entered:
				t.Error("unsupported management Bearer reached the handler body")
			case <-ctx.Done():
				t.Fatalf("management Bearer rejection did not finish: %v", ctx.Err())
			}
			body.unblock()
			rr := call.result(t)
			if body.prefix.Len() != len(target.body)-1 || body.tail.Len() != 1 {
				t.Error("unsupported management Bearer consumed the request body")
			}
			if rr.Code != 401 || rr.Header().Get("x-platform-signed-out") != "" {
				t.Errorf("management Bearer was not rejected independently of its cookie: status=%d marker=%q body=%s", rr.Code, rr.Header().Get("x-platform-signed-out"), rr.Body.String())
			}
			target.assertState(t, ctx, s, h, rr, false, "")
			cookieOnly = credentialCall(ctx, h, "GET", "/api/auth/session", nil, headers)
			if cookieOnly.Code != 200 || cookieOnly.Header().Get("x-platform-signed-out") != "" {
				t.Errorf("management Bearer rejection invalidated its cookie: status=%d body=%s", cookieOnly.Code, cookieOnly.Body.String())
			}
			if !t.Failed() {
				fmt.Printf("CONTROL_ASSERTIONS_PASSED %s\n", t.Name())
			}
		})
	}
}
