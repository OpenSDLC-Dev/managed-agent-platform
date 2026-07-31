// Package webtooltest is test support for the webtool seam: the shared
// contract suite every Searcher/Fetcher backend must pass (CLAUDE.md: backend
// variability lives behind an interface with one shared suite, as
// providertest, sandboxtest and blobtest do for theirs), and the opt-in gate
// for the live tier that calls the real backends.
//
// The .env handling deliberately mirrors internal/modeltest rather than
// importing it: modeltest's contract is scoped to the model endpoint — it
// reads MODEL_* keys only, by design — and widening that scope for another
// package's keys would trade two small parsers for one leaky contract. Same
// rules, restated: consent to spend money is the RUN_LIVE_WEB_TESTS
// environment variable, never the file; the environment always wins over the
// file, an empty environment value included; only the two web keys are ever
// read from the file; not opted in, the file is never opened. Opted in but
// unconfigured FAILS rather than skips — a safety net that skips itself when
// its credentials rot is not a safety net. Production code must never import
// this package.
package webtooltest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool"
)

// contractKey is the credential the suites hand every backend; the
// failure-path subtests assert it never surfaces in an error.
const contractKey = "contract-key-3f9c"

// SearcherBackend describes one Searcher implementation to the contract suite.
type SearcherBackend struct {
	// New returns a Searcher pointed at baseURL, authenticating with key.
	New func(baseURL, key string) webtool.Searcher
	// Render renders hits into the backend's successful wire response.
	Render func(hits []webtool.SearchResult) (body, contentType string)
}

// RunSearcherContract asserts the backend-agnostic invariants every Searcher
// owes the executor. Protocol specifics — paths, headers, request bodies —
// stay in each adapter's own tests.
func RunSearcherContract(t *testing.T, b SearcherBackend) {
	t.Run("maps the endpoint's hits in order", func(t *testing.T) {
		hits := []webtool.SearchResult{
			{Title: "one", URL: "https://a.example/1", Content: "first hit"},
			{Title: "two", URL: "https://b.example/2", Content: "second hit"},
		}
		body, ct := b.Render(hits)
		srv := serve(t, http.StatusOK, body, ct)
		got, err := b.New(srv.URL, contractKey).Search(context.Background(), "q")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if !reflect.DeepEqual(got, hits) {
			t.Errorf("Search = %+v, want %+v", got, hits)
		}
	})

	t.Run("an empty result set is an answer, not an error", func(t *testing.T) {
		body, ct := b.Render(nil)
		srv := serve(t, http.StatusOK, body, ct)
		got, err := b.New(srv.URL, contractKey).Search(context.Background(), "q")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("Search = %+v, want no hits", got)
		}
	})

	t.Run("an endpoint failure is an error without the credential", func(t *testing.T) {
		srv := serve(t, http.StatusInternalServerError,
			"upstream exploded holding "+contractKey, "text/plain")
		_, err := b.New(srv.URL, contractKey).Search(context.Background(), "q")
		if err == nil {
			t.Fatal("an endpoint 500 did not surface as an error")
		}
		if strings.Contains(err.Error(), contractKey) {
			t.Errorf("credential leaked into the error: %v", err)
		}
	})

	t.Run("an oversized response is refused", func(t *testing.T) {
		srv := serve(t, http.StatusOK,
			strings.Repeat("x", webtool.MaxContentBytes+1), "application/json")
		if _, err := b.New(srv.URL, contractKey).Search(context.Background(), "q"); err == nil {
			t.Fatal("a response past MaxContentBytes did not surface as an error")
		}
	})

	t.Run("a cancelled context is an error", func(t *testing.T) {
		body, ct := b.Render(nil)
		srv := serve(t, http.StatusOK, body, ct)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := b.New(srv.URL, contractKey).Search(ctx, "q"); err == nil {
			t.Fatal("a cancelled context did not surface as an error")
		}
	})

	t.Run("a torn response body is an error", func(t *testing.T) {
		srv := serveTorn(t)
		if _, err := b.New(srv.URL, contractKey).Search(context.Background(), "q"); err == nil {
			t.Fatal("a mid-body connection close did not surface as an error")
		}
	})
}

// FetcherBackend describes one Fetcher implementation to the contract suite.
type FetcherBackend struct {
	// New returns a Fetcher pointed at baseURL, authenticating with key.
	New func(baseURL, key string) webtool.Fetcher
	// Render renders page content into the backend's successful wire response.
	Render func(content string) (body, contentType string)
}

// RunFetcherContract asserts the backend-agnostic invariants every Fetcher
// owes the executor.
func RunFetcherContract(t *testing.T, b FetcherBackend) {
	t.Run("returns the endpoint's content", func(t *testing.T) {
		const content = "# Title\n\nA page body."
		body, ct := b.Render(content)
		srv := serve(t, http.StatusOK, body, ct)
		got, err := b.New(srv.URL, contractKey).Fetch(context.Background(), "https://example.com/")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if got.Content != content {
			t.Errorf("Content = %q, want %q", got.Content, content)
		}
		if got.Truncated {
			t.Error("a small page reported truncated")
		}
	})

	t.Run("truncates an oversized body and says so", func(t *testing.T) {
		body, ct := b.Render(strings.Repeat("a", webtool.MaxContentBytes+1))
		srv := serve(t, http.StatusOK, body, ct)
		got, err := b.New(srv.URL, contractKey).Fetch(context.Background(), "https://example.com/")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if !got.Truncated {
			t.Error("an oversized page did not report truncated")
		}
		if len(got.Content) > webtool.MaxContentBytes {
			t.Errorf("len(Content) = %d, past the cap", len(got.Content))
		}
	})

	t.Run("an endpoint failure is an error without the credential", func(t *testing.T) {
		srv := serve(t, http.StatusBadGateway,
			"upstream exploded holding "+contractKey, "text/plain")
		_, err := b.New(srv.URL, contractKey).Fetch(context.Background(), "https://example.com/")
		if err == nil {
			t.Fatal("an endpoint 502 did not surface as an error")
		}
		if strings.Contains(err.Error(), contractKey) {
			t.Errorf("credential leaked into the error: %v", err)
		}
	})

	t.Run("rejects a non-http url without touching the network", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			requests.Add(1)
		}))
		t.Cleanup(srv.Close)
		f := b.New(srv.URL, contractKey)
		for _, target := range []string{"ftp://example.com/x", "example.com/no-scheme", ""} {
			if _, err := f.Fetch(context.Background(), target); err == nil {
				t.Errorf("Fetch(%q) did not error", target)
			}
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("a rejected URL reached the network %d times", n)
		}
	})

	t.Run("a cancelled context is an error", func(t *testing.T) {
		body, ct := b.Render("page")
		srv := serve(t, http.StatusOK, body, ct)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := b.New(srv.URL, contractKey).Fetch(ctx, "https://example.com/"); err == nil {
			t.Fatal("a cancelled context did not surface as an error")
		}
	})

	t.Run("a torn response body is an error", func(t *testing.T) {
		srv := serveTorn(t)
		if _, err := b.New(srv.URL, contractKey).Fetch(context.Background(), "https://example.com/"); err == nil {
			t.Fatal("a mid-body connection close did not surface as an error")
		}
	})
}

// serveTorn starts a server that promises a body and closes mid-stream, so
// the adapter's read fails after a clean 200.
func serveTorn(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, "short")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serve starts a server answering every request with one staged response.
func serve(t *testing.T, status int, body, contentType string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// LiveEnv opts into the live web-backend tier: one real Tavily search and one
// real Jina fetch (cents at most). Any non-empty value opts in.
const LiveEnv = "RUN_LIVE_WEB_TESTS"

// The backend credential keys, read from the environment or the repo-root
// .env. Exactly these two — nothing else ever reaches the file.
const (
	TavilyKeyEnv = "TAVILY_API_KEY"
	JinaKeyEnv   = "JINA_API_KEY"
)

// LiveKey gates a live test and returns one backend's key. Not opted in: the
// test skips and the credential file is never opened. Opted in with the key
// missing: the test FAILS — each backend's key is judged on its own, so a
// tier with only one backend configured fails exactly the tests that need the
// other.
func LiveKey(t *testing.T, keyEnv string) string {
	t.Helper()
	key, skip, err := liveKey(resolve, keyEnv)
	if skip != "" {
		t.Skip(skip)
	}
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// liveKey is the rule LiveKey applies, split out for its own tests.
func liveKey(getenv func(string) string, keyEnv string) (key, skip string, err error) {
	if getenv(LiveEnv) == "" {
		return "", fmt.Sprintf("%s is not set: skipping the live web tier (no backend is called)", LiveEnv), nil
	}
	key = getenv(keyEnv)
	if key == "" {
		return "", "", fmt.Errorf("%s opted into the live web tier but %s is unset: "+
			"set it in the environment or the repo-root .env", LiveEnv, keyEnv)
	}
	return key, "", nil
}

// resolve reads one key for the production gate.
func resolve(key string) string { return lookup(os.LookupEnv, dotEnv, key) }

// lookup resolves a key from the environment, falling back to the repo-root
// .env for the two web keys only. The environment always wins — including
// when it sets a key to the empty string, which is an answer ("this is
// unset") and not an invitation for the file to supply one. The tier variable
// lives outside the web keys, so consent can never come from disk.
func lookup(lookupEnv func(string) (string, bool), file func() map[string]string, key string) string {
	if v, ok := lookupEnv(key); ok {
		return v
	}
	if key != TavilyKeyEnv && key != JinaKeyEnv {
		return ""
	}
	return file()[key]
}

// dotEnv parses the repo-root .env once, on first use. The values stay here
// rather than being pushed into the process environment: an os.Setenv would
// outlive the test that triggered it (modeltest's rationale, mirrored). A
// missing file is not an error — the environment may carry everything.
var dotEnv = sync.OnceValue(func() map[string]string {
	f, err := os.Open(filepath.Join(repoRoot(), ".env"))
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseDotEnv(f)
})

// repoRoot derives the checkout root from this file's compile-time path, so
// every caller reaches the same .env regardless of its own package directory.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func parseDotEnv(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		key = strings.TrimSpace(key)
		if key != TavilyKeyEnv && key != JinaKeyEnv {
			continue
		}
		out[key] = parseValue(value)
	}
	return out
}

// parseValue takes the value side of one .env line, exactly as modeltest
// reads the same file: a quoted value is whatever the quotes enclose, so a
// '#' inside them is content and anything after the closing quote is not; an
// unquoted one runs to a '#' that follows whitespace, which is a trailing
// comment, and keeps a '#' that does not — some credentials contain one.
func parseValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		if end := strings.IndexByte(s[1:], s[0]); end >= 0 {
			return s[1 : 1+end]
		}
	}
	if i := commentStart(s); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func commentStart(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}
