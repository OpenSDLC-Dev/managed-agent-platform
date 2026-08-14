package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveStatus is a server that answers every request with one status and no MCP
// at all — the shape a gateway in front of an MCP endpoint produces when it
// refuses the credential before the server ever sees the request.
func serveStatus(t *testing.T, status int) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// The wire splits a refused credential from a server that could not be reached,
// and only the status can tell them apart: the go-sdk renders a non-2xx into
// prose and wraps no sentinel, so this package watches the response itself.
func TestConnectMarksARefusedCredential(t *testing.T) {
	for _, row := range []struct {
		name       string
		status     int
		refused    bool
		unexpected string
	}{
		{name: "401 unauthorized", status: http.StatusUnauthorized, refused: true},
		{name: "403 forbidden", status: http.StatusForbidden, refused: true},
		// Every other failure is a connection that did not work, which is what
		// the other error type is for. 407 in particular is an authentication
		// status the reference does not name — a proxy's, not the server's.
		{name: "500 server error", status: http.StatusInternalServerError},
		{name: "404 not found", status: http.StatusNotFound},
		{name: "429 too many requests", status: http.StatusTooManyRequests},
		{name: "407 proxy authentication required", status: http.StatusProxyAuthRequired},
	} {
		t.Run(row.name, func(t *testing.T) {
			conn, err := mcp.Connect(context.Background(), mcp.Config{
				URL: serveStatus(t, row.status), HTTPClient: &http.Client{}, BearerToken: "tok"})
			if err == nil {
				_ = conn.Close()
				t.Fatal("expected the handshake to fail")
			}
			if got := errors.Is(err, mcp.ErrUnauthorized); got != row.refused {
				t.Errorf("errors.Is(err, ErrUnauthorized) = %v, want %v (err: %v)", got, row.refused, err)
			}
		})
	}
}

// The marked error still says what it said before: the sentinel is added to the
// chain, not substituted for the diagnosis, and the endpoint stays redacted to
// scheme://host because this string reaches a stored column.
func TestARefusedConnectionStillNamesTheServerAndNothingElse(t *testing.T) {
	url := serveStatus(t, http.StatusUnauthorized)
	_, err := mcp.Connect(context.Background(), mcp.Config{
		URL: url + "/mcp?api_key=SECRET-QUERY", HTTPClient: &http.Client{}, BearerToken: "tok"})
	if err == nil {
		t.Fatal("expected the handshake to fail")
	}
	if !errors.Is(err, mcp.ErrUnauthorized) {
		t.Fatalf("a 401 handshake was not marked: %v", err)
	}
	if !strings.Contains(err.Error(), "mcp: connect to "+url) {
		t.Errorf("the error no longer names the server it failed against: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-QUERY") {
		t.Errorf("the error quotes the endpoint's query: %v", err)
	}
}

// A server that works is never marked, however many requests the handshake and
// the listing take.
func TestAWorkingConnectionIsNotMarkedRefused(t *testing.T) {
	url, _ := serveToolCall(t, func(json.RawMessage) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
	})
	conn := connect(t, url)
	if _, err := conn.CallTool(context.Background(), "echo", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	// Success, not "did not fail with ErrUnauthorized": mark wraps only a
	// non-nil error, so an assertion guarded on err != nil holds whatever the
	// marking does and pins nothing at all.
	if _, err := conn.ListTools(context.Background()); err != nil {
		t.Fatalf("list on a working server: %v", err)
	}
}

// The listing's other half: a failure that is not a refusal must not be marked
// as one. 500 is the nearest thing to it — a server that answered, and answered
// badly — and it reaches the listing path, which the connect-time table cannot.
func TestAListingThatFailsForAnotherReasonIsNotMarkedRefused(t *testing.T) {
	conn, err := mcp.Connect(context.Background(), mcp.Config{
		URL:        serveThenFailing(t, map[string]int{"tools/list": http.StatusInternalServerError}),
		HTTPClient: &http.Client{}, BearerToken: "tok"})
	if err != nil {
		t.Fatalf("the handshake was expected to succeed: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ListTools(context.Background()); err == nil {
		t.Fatal("expected the failed listing to fail")
	} else if errors.Is(err, mcp.ErrUnauthorized) {
		t.Errorf("a 500 on the listing was marked as a refused credential: %v", err)
	}
}

// An endpoint may carry userinfo, from which net/http derives a Basic header of
// its own. Both directions of that are decisions rather than accidents: without
// a vault credential the dial is not anonymous, and with one the vault's token
// replaces the URL's — the configured, rotatable credential wins.
func TestAResolvedTokenReplacesTheURLsOwnCredential(t *testing.T) {
	// Guarded: the handler runs on the server's goroutine and the assertions on
	// the test's, and nothing between them synchronises.
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	withUserinfo := (&url.URL{Scheme: "http", Host: strings.TrimPrefix(ts.URL, "http://"),
		User: url.UserPassword("bob", "pw"), Path: "/"}).String()

	for _, row := range []struct{ name, token, want string }{
		{name: "no credential resolved", want: "Basic Ym9iOnB3"},
		{name: "a credential resolved", token: "VAULT-TOKEN", want: "Bearer VAULT-TOKEN"},
	} {
		t.Run(row.name, func(t *testing.T) {
			mu.Lock()
			seen = nil
			mu.Unlock()

			conn, err := mcp.Connect(context.Background(), mcp.Config{
				URL: withUserinfo, HTTPClient: &http.Client{}, BearerToken: row.token})
			if err == nil {
				_ = conn.Close()
				t.Fatal("expected the handshake to fail against a 500")
			}

			mu.Lock()
			defer mu.Unlock()
			if len(seen) == 0 {
				t.Fatal("the server saw no request at all")
			}
			for _, got := range seen {
				if got != row.want {
					t.Errorf("the server saw Authorization %q, want %q", got, row.want)
				}
			}
		})
	}
}

// serveThenFailing serves MCP but answers each named JSON-RPC method with the
// status `rules` gives it — a token that expires mid-session, a server that
// authenticates the work rather than the connection, or one that refuses the
// discovery probe alone. The failures arrive on a Conn that already exists,
// which is the half a connect-time test cannot reach.
func serveThenFailing(t *testing.T, rules map[string]int) string {
	t.Helper()
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		s := sdk.NewServer(&sdk.Implementation{Name: "refusing-server", Version: "1"}, nil)
		sdk.AddTool(s, &sdk.Tool{Name: "echo", Description: "echoes"},
			func(context.Context, *sdk.CallToolRequest, map[string]any) (
				*sdk.CallToolResult, map[string]any, error) {
				return &sdk.CallToolResult{}, nil, nil
			})
		return s
	}, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		if status, ok := rules[msg.Method]; ok {
			w.WriteHeader(status)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// A refusal that lands after the handshake is the same authentication failure,
// and it has to reach the caller through the Conn — which is what carrying the
// watch on the connection is for.
func TestARefusalAfterTheHandshakeIsMarkedToo(t *testing.T) {
	t.Run("on a tool call", func(t *testing.T) {
		conn, err := mcp.Connect(context.Background(), mcp.Config{
			URL: serveThenFailing(t, map[string]int{"tools/call": http.StatusUnauthorized}), HTTPClient: &http.Client{}, BearerToken: "tok"})
		if err != nil {
			t.Fatalf("the handshake was expected to succeed: %v", err)
		}
		defer conn.Close()
		if _, err := conn.CallTool(context.Background(), "echo", nil); err == nil {
			t.Fatal("expected the refused call to fail")
		} else if !errors.Is(err, mcp.ErrUnauthorized) {
			t.Errorf("a 401 on the call was not marked: %v", err)
		} else if errors.Is(err, mcp.ErrServerAnswered) {
			t.Errorf("a refused credential must not read as a server refusing the call: %v", err)
		}
	})

	t.Run("on a listing", func(t *testing.T) {
		conn, err := mcp.Connect(context.Background(), mcp.Config{
			URL: serveThenFailing(t, map[string]int{"tools/list": http.StatusUnauthorized}), HTTPClient: &http.Client{}, BearerToken: "tok"})
		if err != nil {
			t.Fatalf("the handshake was expected to succeed: %v", err)
		}
		defer conn.Close()
		if _, err := conn.ListTools(context.Background()); err == nil {
			t.Fatal("expected the refused listing to fail")
		} else if !errors.Is(err, mcp.ErrUnauthorized) {
			t.Errorf("a 401 on the listing was not marked: %v", err)
		}
	})
}

// A 401 the SDK itself shrugs off must not answer for every later failure on the
// connection. go-sdk opens with `server/discover` and carries on when that one is
// refused, so a server requiring authentication for discovery alone hands back a
// working connection that has already seen a 401 — and every subsequent error
// would read as an authentication failure the operator cannot find.
func TestARecoveredRefusalDoesNotSpeakForALaterFailure(t *testing.T) {
	t.Run("on a tool call", func(t *testing.T) {
		conn := connectRefusingDiscovery(t, nil)
		defer conn.Close()

		// The server answers this one itself, in full: nothing about it is an
		// authentication failure, and the credential it accepted is the same one.
		_, err := conn.CallTool(context.Background(), "no-such-tool", nil)
		if err == nil {
			t.Fatal("expected the unknown tool to fail")
		}
		if errors.Is(err, mcp.ErrUnauthorized) {
			t.Errorf("a refusal the handshake recovered from was reported as this call's: %v", err)
		}
		if !errors.Is(err, mcp.ErrServerAnswered) {
			t.Errorf("a server refusing a call it answered = %v, want ErrServerAnswered", err)
		}
	})

	t.Run("on a listing", func(t *testing.T) {
		conn := connectRefusingDiscovery(t, map[string]int{"tools/list": http.StatusInternalServerError})
		defer conn.Close()

		_, err := conn.ListTools(context.Background())
		if err == nil {
			t.Fatal("expected the failed listing to fail")
		}
		if errors.Is(err, mcp.ErrUnauthorized) {
			t.Errorf("a refusal the handshake recovered from was reported as this listing's: %v", err)
		}
	})

	// The one an operation-scoped reset cannot reach: both exchanges are inside
	// the same Connect, so nothing clears the flag between them.
	t.Run("inside the handshake that recovered from it", func(t *testing.T) {
		conn, err := mcp.Connect(context.Background(), mcp.Config{
			URL: serveThenFailing(t, map[string]int{
				"server/discover": http.StatusUnauthorized,
				"initialize":      http.StatusInternalServerError,
			}),
			HTTPClient: &http.Client{}, BearerToken: "tok"})
		if err == nil {
			_ = conn.Close()
			t.Fatal("expected the handshake to fail")
		}
		if errors.Is(err, mcp.ErrUnauthorized) {
			t.Errorf("a 500 that followed a recovered 401 was reported as a refused credential: %v", err)
		}
	})

	// And its response-less twin: the exchange after the refusal never answered
	// at all, which says as little about the credential as a 500 does.
	t.Run("when the exchange after it never answered", func(t *testing.T) {
		conn, err := mcp.Connect(context.Background(), mcp.Config{
			URL: serveRefusingDiscoveryThenHangingUp(t), HTTPClient: &http.Client{}, BearerToken: "tok"})
		if err == nil {
			_ = conn.Close()
			t.Fatal("expected the handshake to fail")
		}
		if errors.Is(err, mcp.ErrUnauthorized) {
			t.Errorf("a dropped connection after a recovered 401 was reported as a refusal: %v", err)
		}
	})
}

// serveRefusingDiscoveryThenHangingUp answers the discovery probe 401 and then
// closes the connection on the exchange the SDK falls back to, so that one
// produces a transport error and no response at all.
func serveRefusingDiscoveryThenHangingUp(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Method == "server/discover" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// No status, no body: hijack and drop, which is what a middlebox that
		// resets the connection looks like to the client.
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// connectRefusingDiscovery opens a connection to a server that refuses the
// discovery probe go-sdk leads with and serves everything else, plus whatever
// `also` adds. The handshake stands — the SDK falls back to the legacy
// initialize on any discovery error — so the connection starts life having
// already seen a 401.
func connectRefusingDiscovery(t *testing.T, also map[string]int) *mcp.Conn {
	t.Helper()
	rules := map[string]int{"server/discover": http.StatusUnauthorized}
	for m, s := range also {
		rules[m] = s
	}
	conn, err := mcp.Connect(context.Background(), mcp.Config{
		URL: serveThenFailing(t, rules), HTTPClient: &http.Client{}, BearerToken: "tok"})
	if err != nil {
		t.Fatalf("the SDK recovers from a refused discovery, so the handshake should stand: %v", err)
	}
	return conn
}

// serveDrainingThenRefusing answers the handshake normally, spends almost the
// whole connection budget on one tools/list page, then refuses the next page with
// a 401 whose header block alone is larger than what is left.
//
// That combination is the only way to reach the response limit's discard path
// with a status worth reading: the limit answers (nil, error) and closes the
// response, so a status watcher above it would be handed neither.
func serveDrainingThenRefusing(t *testing.T) string {
	t.Helper()
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		return sdk.NewServer(&sdk.Implementation{Name: "draining-server", Version: "1"}, nil)
	}, nil)
	// Sized to leave the budget with more than a plain 401's header block and
	// less than the padded one below, so neither margin depends on counting the
	// handshake's own bytes exactly.
	pad := strings.Repeat("d", mcp.MaxResponseBytes-(16<<10))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil || req.Method != "tools/list" {
			r.Body = io.NopCloser(bytes.NewReader(body))
			inner.ServeHTTP(w, r)
			return
		}
		if req.Params.Cursor != "" {
			w.Header().Set("X-Padding", strings.Repeat("p", 32<<10))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{
				"nextCursor": "page-2",
				"tools": []any{map[string]any{
					"name": "bulky", "description": pad,
					"inputSchema": map[string]any{"type": "object"},
				}},
			},
		})
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// The status has to be read below the response limit, not above it: the limit
// refuses an oversized response by discarding it and returning an error of its
// own, and a 401 refused that way is a 401 nothing above the limit ever sees.
func TestARefusalTheResponseLimitDiscardsIsStillMarked(t *testing.T) {
	conn, err := mcp.Connect(context.Background(), mcp.Config{
		URL: serveDrainingThenRefusing(t), HTTPClient: &http.Client{}, BearerToken: "tok"})
	if err != nil {
		t.Fatalf("the handshake was expected to succeed: %v", err)
	}
	defer conn.Close()

	_, err = conn.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected the listing to fail")
	}
	if !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("the fixture did not reach the response limit, so this proves nothing: %v", err)
	}
	if !errors.Is(err, mcp.ErrUnauthorized) {
		t.Errorf("a 401 the limit discarded was not marked: %v", err)
	}
}
