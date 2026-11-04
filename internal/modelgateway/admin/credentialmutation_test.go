package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
)

const widenGrantBody = `{"aliases":null,"rpm":null,"tpm":null}`

// Pause only after the assembled handler consumes a valid prefix. Direct
// invocation makes this an authorization/body boundary, not transport buffering.
type grantBody struct {
	ctx                      context.Context
	prefix, tail             *strings.Reader
	entered, release, eof    chan struct{}
	releaseOnce, enteredOnce sync.Once
	eofOnce                  sync.Once
}

func newGrantBody(ctx context.Context) *grantBody {
	return &grantBody{ctx: ctx, prefix: strings.NewReader(widenGrantBody[:len(widenGrantBody)-1]), tail: strings.NewReader("}"),
		entered: make(chan struct{}), release: make(chan struct{}), eof: make(chan struct{})}
}

func (b *grantBody) Read(p []byte) (int, error) {
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

func (b *grantBody) Close() error { b.unblock(); return nil }
func (b *grantBody) unblock()     { b.releaseOnce.Do(func() { close(b.release) }) }

type grantRequest struct {
	ctx  context.Context
	done chan struct{}
	rr   *httptest.ResponseRecorder
}

func grantCall(ctx context.Context, h http.Handler, method, path string, body io.Reader, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, cookieOrigin+path, body).WithContext(ctx)
	r.Header = headers.Clone()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func startGrantRequest(t *testing.T, ctx context.Context, h http.Handler, method, path string, body io.ReadCloser, headers http.Header) *grantRequest {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	call := &grantRequest{ctx: ctx, done: make(chan struct{})}
	go func() {
		defer close(call.done)
		call.rr = grantCall(ctx, h, method, path, body, headers)
	}()
	t.Cleanup(func() {
		cancel()
		if body != nil {
			body.Close()
		}
		select {
		case <-call.done:
		case <-time.After(5 * time.Second):
			t.Error("grant request did not stop after cancellation and body release")
		}
	})
	return call
}

func (r *grantRequest) await(t *testing.T, signal <-chan struct{}, what string) {
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

func (r *grantRequest) result(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case <-r.done:
		return r.rr
	case <-r.ctx.Done():
		t.Fatalf("grant request did not finish: %v", r.ctx.Err())
		return nil
	}
}

type grantFixture struct {
	e               *env
	handler, logout http.Handler
	caller          string
	issued          login.IssuedSession
}

func newGrantFixture(t *testing.T, ctx context.Context) *grantFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: pool, store: store.New(pool), cipher: cipher, prober: &stubProber{}}
	m := login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: cookieOrigin, Development: true})
	h, err := admin.New(admin.Config{Store: e.store, Cipher: cipher, BootstrapKey: bootstrap, Prober: e.prober, Login: m})
	if err != nil {
		t.Fatal(err)
	}
	caller := domain.NewID(domain.PrefixPrincipal).String()
	if _, err := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject,role,role_source) VALUES ($1,'https://cookie.example','grant-caller','admin','console')`, caller); err != nil {
		t.Fatal(err)
	}
	issued, err := m.Issue(ctx, caller)
	if err != nil {
		t.Fatal(err)
	}
	// Logout is the actual control-plane endpoint over the shared session store.
	return &grantFixture{e: e, handler: h, logout: api.NewHandler(pool, nil, nil, nil, api.WithLogin(m, nil)), caller: caller, issued: issued}
}

func (f *grantFixture) withdraw(t *testing.T, ctx context.Context, withdrawal string) (int, string) {
	t.Helper()
	if withdrawal == "logout" {
		rr := grantCall(ctx, f.logout, "POST", "/api/auth/logout", nil, cookieAdminHeaders(f.issued))
		if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"ok":true}` {
			t.Fatalf("logout did not commit: status=%d body=%s", rr.Code, rr.Body.String())
		}
		var count int
		if err := f.e.pool.QueryRow(ctx, `SELECT count(*) FROM browser_sessions WHERE principal_id=$1`, f.caller).Scan(&count); err != nil || count != 0 {
			t.Fatalf("logout left %d sessions: %v", count, err)
		}
		return 401, "1"
	}
	query := map[string]string{
		"demote":     `UPDATE principals SET role='developer' WHERE id=$1`,
		"deactivate": `UPDATE principals SET status='deactivated' WHERE id=$1`,
		"expire":     `UPDATE browser_sessions SET created_at=clock_timestamp()-interval '13 hours',expires_at=clock_timestamp()-interval '1 second' WHERE principal_id=$1`,
	}[withdrawal]
	tag, err := f.e.pool.Exec(ctx, query, f.caller)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("%s affected %d rows: %v", withdrawal, tag.RowsAffected(), err)
	}
	var committed bool
	verify := map[string]string{
		"demote":     `SELECT role='developer' FROM principals WHERE id=$1`,
		"deactivate": `SELECT status='deactivated' FROM principals WHERE id=$1`,
		"expire":     `SELECT expires_at<clock_timestamp() FROM browser_sessions WHERE principal_id=$1`,
	}[withdrawal]
	if err := f.e.pool.QueryRow(ctx, verify, f.caller).Scan(&committed); err != nil || !committed {
		t.Fatalf("%s was not committed: effect=%v err=%v", withdrawal, committed, err)
	}
	want, marker := 403, ""
	if withdrawal == "deactivate" {
		marker = "1"
	}
	if withdrawal == "expire" {
		want, marker = 401, "1"
	}
	return want, marker
}

type grantTarget struct {
	id, secret string
	before     *store.KeyPolicy
}

func prepareGrantTarget(t *testing.T, ctx context.Context, f *grantFixture, state string) grantTarget {
	t.Helper()
	expires := time.Now().Add(time.Hour)
	secret, row, err := api.IssueManagementKey(ctx, f.e.pool, "grant-target", &expires, f.caller)
	if err != nil {
		t.Fatal(err)
	}
	target := grantTarget{id: row.ID, secret: secret}
	if state == "limited" {
		rpm, tpm := int32(1), int64(10)
		before, err := f.e.store.PutKeyPolicy(ctx, store.KeyPolicy{APIKeyID: row.ID, RPM: &rpm, TPM: &tpm})
		if err != nil {
			t.Fatal(err)
		}
		target.before = &before
	}
	return target
}

func (target grantTarget) assertState(t *testing.T, ctx context.Context, f *grantFixture, rr *httptest.ResponseRecorder, changed bool) {
	t.Helper()
	got, err := f.e.store.GetKeyPolicy(ctx, target.id)
	wantRows := 0
	switch {
	case changed:
		wantRows = 1
		if err != nil || got.APIKeyID != target.id || got.Aliases != nil || got.RPM != nil || got.TPM != nil {
			t.Errorf("successful grant did not restore/widen the actual row: %+v err=%v", got, err)
		}
		if rr.Code == 200 {
			var body map[string]any
			if json.Unmarshal(rr.Body.Bytes(), &body) != nil || body["type"] != "key_policy" || body["api_key_id"] != target.id {
				t.Errorf("grant reply does not describe the widened target: %s", rr.Body.String())
			}
			for _, key := range []string{"aliases", "rpm", "tpm"} {
				if value, present := body[key]; !present || value != nil {
					t.Errorf("widened grant reply requires explicit null %q: present=%t value=%v body=%s", key, present, value, rr.Body.String())
				}
			}
		}
	case target.before != nil:
		wantRows = 1
		if err != nil || !reflect.DeepEqual(got, *target.before) {
			t.Errorf("withdrawn caller changed the limited grant: got=%+v before=%+v err=%v", got, *target.before, err)
		}
	default:
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("withdrawn caller restored an absent grant: %+v err=%v", got, err)
		}
	}
	var rows, keys int
	var valid bool
	if err := f.e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM modelgateway.key_policies),(SELECT count(*) FROM api_keys),status='active' AND expires_at>clock_timestamp() AND key_hash=$2 FROM api_keys WHERE id=$1`, target.id, apikey.Hash(target.secret)).Scan(&rows, &keys, &valid); err != nil || rows != wantRows || keys != 1 || !valid {
		t.Errorf("durable grant/key state: grants=%d want=%d keys=%d valid_target=%v err=%v", rows, wantRows, keys, valid, err)
	}
}

// A copied admission-time admin role must not restore an absent grant or lift
// limits after logout, role loss, deactivation or absolute session expiry.
func TestGatewayCredentialMutationDelayedBody(t *testing.T) {
	for _, state := range []string{"missing", "limited"} {
		for _, withdrawal := range []string{"unchanged", "logout", "demote", "deactivate", "expire"} {
			t.Run(state+"/"+withdrawal, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				f := newGrantFixture(t, ctx)
				target := prepareGrantTarget(t, ctx, f, state)
				body := newGrantBody(ctx)
				call := startGrantRequest(t, ctx, f.handler, "POST", "/admin/v1/key_policies/"+target.id, body, cookieAdminHeaders(f.issued))
				call.await(t, body.entered, "partial body after admin admission")
				want, marker := 200, ""
				if withdrawal != "unchanged" {
					want, marker = f.withdraw(t, ctx, withdrawal)
				}
				body.unblock()
				rr := call.result(t)
				select {
				case <-body.eof:
				default:
					t.Error("admitted grant request did not consume the complete valid body")
				}
				if rr.Code != want || rr.Header().Get("x-platform-signed-out") != marker {
					t.Errorf("post-body grant authority: status=%d marker=%q want=%d/%q body=%s", rr.Code, rr.Header().Get("x-platform-signed-out"), want, marker, rr.Body.String())
				}
				target.assertState(t, ctx, f, rr, withdrawal == "unchanged")
				if withdrawal == "unchanged" && !t.Failed() {
					fmt.Printf("CONTROL_ASSERTIONS_PASSED %s\n", t.Name())
				}
			})
		}
	}
}

// Bootstrap admission remains independent of the cookie, including when that
// cookie is withdrawn during the same upload. Ordinary issued keys stay refused
// by TestAuthByKey and malformed offered headers by TestGatewayCookieLegacyPrecedence.
func TestGatewayCredentialMutationLegacyPrecedence(t *testing.T) {
	for _, state := range []string{"missing", "limited"} {
		for _, field := range []string{"X-Api-Key", "Authorization"} {
			t.Run(state+"/"+field, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				f := newGrantFixture(t, ctx)
				target := prepareGrantTarget(t, ctx, f, state)
				headers := cookieAdminHeaders(f.issued)
				credential := bootstrap
				if field == "Authorization" {
					credential = "Bearer " + credential
				}
				headers.Set(field, credential)
				body := newGrantBody(ctx)
				call := startGrantRequest(t, ctx, f.handler, "POST", "/admin/v1/key_policies/"+target.id, body, headers)
				call.await(t, body.entered, "partial body on the legacy bootstrap lane")
				f.withdraw(t, ctx, "logout")
				body.unblock()
				rr := call.result(t)
				select {
				case <-body.eof:
				default:
					t.Error("legacy grant request did not consume the complete valid body")
				}
				if rr.Code != 200 || rr.Header().Get("x-platform-signed-out") != "" {
					t.Errorf("bootstrap credential lost precedence: status=%d marker=%q body=%s", rr.Code, rr.Header().Get("x-platform-signed-out"), rr.Body.String())
				}
				target.assertState(t, ctx, f, rr, true)
				if !t.Failed() {
					fmt.Printf("CONTROL_ASSERTIONS_PASSED %s\n", t.Name())
				}
			})
		}
	}
}
