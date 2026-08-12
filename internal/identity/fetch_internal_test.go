package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fetchXServe starts a loopback server running h and returns it with a client
// that reaches it. The client is the httptest server's own — the production
// client refuses loopback by design, which is the property
// TestProductionClientRefusesLoopback pins and every other test here has to work
// around.
func fetchXServe(t *testing.T, h http.HandlerFunc) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, srv.Client()
}

// fetchXBodyOfSize returns a well-formed JSON object whose encoding is exactly n
// bytes, so the body cap can be driven from both sides of its boundary rather
// than from one.
func fetchXBodyOfSize(t *testing.T, n int) []byte {
	t.Helper()
	const wrapper = `{"pad":""}`
	if n < len(wrapper) {
		t.Fatalf("size %d is below the smallest object this builds (%d bytes)", n, len(wrapper))
	}
	return []byte(`{"pad":"` + strings.Repeat("a", n-len(wrapper)) + `"}`)
}

// fetchXRoundTripFunc is a transport a test can assert is never reached.
type fetchXRoundTripFunc func(*http.Request) (*http.Response, error)

func (f fetchXRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRequireHTTPS(t *testing.T) {
	t.Parallel()
	// The scheme rule, mirroring the reference SDK's rule for its own credential
	// endpoints. It is deliberately NOT the dial guard: this decides which URLs
	// may be configured, the guard decides which addresses may be dialed, and
	// the loopback rows below are exactly why the two must stay separate — a URL
	// this accepts is still refused at dial time in production.
	for _, tc := range []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "https", raw: "https://idp.example"},
		{name: "https with port and path", raw: "https://idp.example:8443/.well-known/openid-configuration"},
		{name: "http to localhost", raw: "http://localhost:9/keys"},
		{name: "http to 127.0.0.1", raw: "http://127.0.0.1:9/keys"},
		{name: "http to ::1", raw: "http://[::1]:9/keys"},
		{name: "http to a case-folded loopback name", raw: "http://LocalHost:9/keys"},

		{name: "http to a named host", raw: "http://idp.example/keys", wantErr: true},
		{name: "http to cloud metadata", raw: "http://169.254.169.254/keys", wantErr: true},
		// A host that merely starts with a loopback name is not loopback: the
		// rule is the whole host, not a prefix of it.
		{name: "http to a loopback lookalike", raw: "http://localhost.evil.example/keys", wantErr: true},
		{name: "another scheme", raw: "ftp://idp.example/keys", wantErr: true},
		{name: "no scheme and so no host", raw: "idp.example/keys", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
		{name: "https with no host", raw: "https:///keys", wantErr: true},
		// A credential smuggled into a key URL is never a legitimate
		// configuration, with or without the password half.
		{name: "userinfo with a password", raw: "https://user:pass@idp.example/keys", wantErr: true},
		{name: "userinfo without a password", raw: "https://user@idp.example/keys", wantErr: true},
		{name: "unparseable", raw: "://idp.example", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := requireHTTPS(tc.raw)
			if tc.wantErr && err == nil {
				t.Errorf("requireHTTPS(%q) = nil, want an error", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("requireHTTPS(%q) = %v, want nil", tc.raw, err)
			}
		})
	}
}

func TestGetJSONSizeCapBoundary(t *testing.T) {
	t.Parallel()
	// The cap is read with a +1 probe, so the boundary is asserted from both
	// sides: a body of exactly maxIdPBytes is a working provider and must parse,
	// one byte more is refused.
	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "exactly at the cap", size: maxIdPBytes},
		{name: "one byte over the cap", size: maxIdPBytes + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := fetchXBodyOfSize(t, tc.size)
			srv, client := fetchXServe(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(body)
			})

			var got map[string]any
			err := getJSON(context.Background(), client, srv.URL, time.Minute, &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("getJSON accepted a %d-byte body, want a refusal above %d", tc.size, maxIdPBytes)
				}
				return
			}
			if err != nil {
				t.Fatalf("getJSON: %v", err)
			}
			pad, _ := got["pad"].(string)
			if want := tc.size - len(`{"pad":""}`); len(pad) != want {
				t.Errorf("decoded pad is %d bytes, want %d — the body was truncated", len(pad), want)
			}
		})
	}
}

func TestGetJSONNonOK(t *testing.T) {
	t.Parallel()
	// Anything but 200 is an error naming the status: an operator reading a boot
	// failure needs to know whether the provider answered 404 (wrong URL) or 500
	// (provider broken), and the two are one log line apart.
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			srv, client := fetchXServe(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			})

			var got map[string]any
			err := getJSON(context.Background(), client, srv.URL, time.Minute, &got)
			if err == nil {
				t.Fatalf("getJSON accepted status %d", status)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Errorf("error %q does not name status %d", err, status)
			}
		})
	}
}

func TestGetJSONMalformed(t *testing.T) {
	t.Parallel()
	// Content-Type is deliberately not enforced — too many providers get it
	// wrong — so the body having to parse is the whole check. Both bodies below
	// arrive as 200s.
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "truncated JSON", body: `{"keys":`},
		{name: "an HTML error page", body: "<html><body>sign in to continue</body></html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, client := fetchXServe(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})

			var got map[string]any
			if err := getJSON(context.Background(), client, srv.URL, time.Minute, &got); err == nil {
				t.Fatalf("getJSON accepted %s as a document", tc.name)
			}
		})
	}
}

func TestGetJSONRefusesRedirect(t *testing.T) {
	t.Parallel()
	// Following a redirect would move the fetch off the address the guard vetted
	// and onto one it never approved, so the production client turns a 3xx into
	// the non-200 branch. The policy under test is production's own: the client
	// below borrows productionClient.CheckRedirect and only swaps the transport,
	// because the guarded transport cannot reach a loopback fixture at all.
	var elsewhereHits atomic.Int64
	elsewhere, _ := fetchXServe(t, func(w http.ResponseWriter, _ *http.Request) {
		elsewhereHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jwks_uri":"https://attacker.example/keys"}`))
	})
	redirector, transportOwner := fetchXServe(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	})

	client := &http.Client{
		Transport:     transportOwner.Transport,
		CheckRedirect: productionClient.CheckRedirect,
	}
	var got map[string]any
	err := getJSON(context.Background(), client, redirector.URL, time.Minute, &got)
	if err == nil {
		t.Fatal("getJSON followed a redirect and accepted the second server's document")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(http.StatusFound)) {
		t.Errorf("error %q does not name the redirect status", err)
	}
	// The redirect being refused is the claim; that the other server was never
	// reached is the proof. Without it a client that followed the redirect and
	// then failed for some unrelated reason would pass this test.
	if n := elsewhereHits.Load(); n != 0 {
		t.Errorf("the redirect target served %d requests, want 0", n)
	}
}

func TestGetJSONDeadline(t *testing.T) {
	t.Parallel()
	// The per-fetch deadline is the shared fetch's own, and it is the one branch
	// no fake clock can reach. Driven with a gated handler and a 50ms timeout,
	// so this costs milliseconds rather than the production five seconds.
	release := make(chan struct{})
	srv, client := fetchXServe(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{}`))
	})
	// Registered after the server's own cleanup, so it runs first (LIFO) and
	// srv.Close never waits on a handler nothing will release.
	t.Cleanup(func() { close(release) })

	var got map[string]any
	err := getJSON(context.Background(), client, srv.URL, 50*time.Millisecond, &got)
	if err == nil {
		t.Fatal("getJSON returned before the handler answered")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("getJSON = %v, want a deadline error", err)
	}
}

func TestGetJSONHonoursCallerCancellation(t *testing.T) {
	t.Parallel()
	// getJSON itself honours the context it is handed — which is what makes a
	// caller's deadline real at startup, where New fetches directly and an
	// unreachable IdP must fail the boot rather than hang past it.
	//
	// Detaching the SHARED refresh from whichever caller happened to lead it is a
	// separate property, and it lives one layer up in leadFlight, where the
	// sharing is: TestLeadFlightOutlivesTheLeadersCancellation covers it.
	started := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	srv, client := fetchXServe(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jwks_uri":"https://idp.example/keys"}`))
	})
	t.Cleanup(releaseOnce)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		var doc map[string]any
		done <- getJSON(ctx, client, srv.URL, time.Minute, &doc)
	}()

	<-started // the request is in flight and the handler is holding it open
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("getJSON = %v, want the caller's cancellation to reach the fetch", err)
	}
	releaseOnce()
}

func TestGetJSONErrorsDropTheURLButKeepTheChain(t *testing.T) {
	t.Parallel()
	// The two halves of one rule. A key-set URL can be a signed URL whose query
	// string IS the credential, so no error may quote it; but an error nobody can
	// match with errors.Is is a debugging dead end, so the cause must survive.
	release := make(chan struct{})
	srv, client := fetchXServe(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{}`))
	})
	t.Cleanup(func() { close(release) })

	target := srv.URL + "/keys?access_token=super-secret"
	var got map[string]any
	err := getJSON(context.Background(), client, target, 50*time.Millisecond, &got)
	if err == nil {
		t.Fatal("getJSON returned before the handler answered")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("getJSON = %v, want the deadline to survive errors.Is", err)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Errorf("getJSON error quotes the query credential: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "redacted") {
		t.Errorf("getJSON error = %q, want the query replaced with the redaction marker", err.Error())
	}
}

func TestGetJSONRequestConstructionError(t *testing.T) {
	t.Parallel()
	// A URL carrying a control byte cannot be built into a request. The branch is
	// unreachable from configuration — requireHTTPS parses first — but it is the
	// error path a jwks_uri arriving inside a discovery document takes, and it
	// must return rather than dial.
	client := &http.Client{Transport: fetchXRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("getJSON dialed a URL it could not build a request for")
		return nil, fmt.Errorf("unexpected round trip")
	})}

	var got map[string]any
	if err := getJSON(context.Background(), client, "https://idp.example/keys\x7f", time.Minute, &got); err == nil {
		t.Fatal("getJSON accepted a URL holding a control byte")
	}
}

func TestProductionClientRefusesLoopback(t *testing.T) {
	t.Parallel()
	// The one test that does not swap the client out: it drives the client a
	// Config with no HTTPClient selects, against a fixture on 127.0.0.1 — the
	// address class the dial guard exists to refuse. It proves the guard is the
	// default rather than an aspiration, and it is why every other test in this
	// package supplies its own client.
	var hits atomic.Int64
	srv, _ := fetchXServe(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"jwks_uri":"https://idp.example/keys"}`))
	})

	var got map[string]any
	err := getJSON(context.Background(), ProductionClientForTest(), srv.URL, time.Minute, &got)
	if err == nil {
		t.Fatal("the production client reached a loopback address")
	}
	// Asserting only that it failed proves nothing about the guard: a dial that
	// never happened and a dial the guard refused look the same from here. The
	// failure has to be the guard's own refusal.
	if !strings.Contains(err.Error(), "disallowed address") {
		t.Errorf("getJSON = %v, want the dial guard's refusal", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the fixture served %d requests, want 0 — the dial was not refused", n)
	}
}
