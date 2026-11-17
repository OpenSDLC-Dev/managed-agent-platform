package admin_test

import (
	"bytes"
	"context"

	"io"
	"net/http"
	"net/http/httptest"

	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"

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
	h, err := admin.New(admin.Config{Store: e.store, Cipher: cipher, Prober: e.prober, Login: m})
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
