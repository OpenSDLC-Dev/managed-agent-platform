package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if _, err := conn.ListTools(context.Background()); err != nil && errors.Is(err, mcp.ErrUnauthorized) {
		t.Errorf("a working server was marked as refusing the credential: %v", err)
	}
}

// serveThenRefuse completes the handshake and answers 401 to the named JSON-RPC
// method — a token that expires mid-session, or a server that authenticates the
// work rather than the connection. The refusal arrives on a Conn that already
// exists, which is the half a connect-time test cannot reach.
func serveThenRefuse(t *testing.T, refuse string) string {
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
		if msg.Method == refuse {
			w.WriteHeader(http.StatusUnauthorized)
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
			URL: serveThenRefuse(t, "tools/call"), HTTPClient: &http.Client{}, BearerToken: "tok"})
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
			URL: serveThenRefuse(t, "tools/list"), HTTPClient: &http.Client{}, BearerToken: "tok"})
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
