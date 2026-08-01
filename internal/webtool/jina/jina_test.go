package jina_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/jina"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/webtooltest"
)

func TestFetcherContract(t *testing.T) {
	webtooltest.RunFetcherContract(t, webtooltest.FetcherBackend{
		New: func(baseURL, key string) webtool.Fetcher {
			return jina.New(baseURL, key)
		},
		// Jina Reader answers with the page as markdown text, verbatim.
		Render: func(content string) (string, string) {
			return content, "text/plain; charset=utf-8"
		},
	})
}

func TestFetchRequestShape(t *testing.T) {
	var uri, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri, auth = r.RequestURI, r.Header.Get("Authorization")
		w.Write([]byte("# page"))
	}))
	t.Cleanup(srv.Close)

	const target = "https://example.com/a?x=1"
	// The trailing slash on the base URL must not double up in the path.
	if _, err := jina.New(srv.URL+"/", "jina-k1").Fetch(context.Background(), target); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// The target URL rides percent-encoded as one path segment.
	if want := "/" + url.PathEscape(target); uri != want {
		t.Errorf("request URI = %q, want %q", uri, want)
	}
	if auth != "Bearer jina-k1" {
		t.Errorf("Authorization = %q, want Bearer jina-k1", auth)
	}
}

func TestFetchKeepsTheFragment(t *testing.T) {
	// Concatenated raw, everything from the target's '#' would become the
	// outer URL's fragment and silently never reach the reader — a
	// hash-routed page would fetch as its landing page.
	var uri string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri = r.RequestURI
		w.Write([]byte("page"))
	}))
	t.Cleanup(srv.Close)

	if _, err := jina.New(srv.URL, "k").Fetch(context.Background(), "https://docs.example.com/#/api/auth"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(uri, "%23") {
		t.Errorf("request URI %q lost the target's fragment", uri)
	}
}

func TestFetchWithoutKeyOmitsAuthorization(t *testing.T) {
	var auth string
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, sawAuth = r.Header.Get("Authorization"), r.Header.Get("Authorization") != ""
		w.Write([]byte("page"))
	}))
	t.Cleanup(srv.Close)

	if _, err := jina.New(srv.URL, "").Fetch(context.Background(), "https://example.com/"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if sawAuth {
		t.Errorf("keyless fetch sent Authorization %q; Jina's free tier takes none", auth)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	if jina.DefaultBaseURL != "https://r.jina.ai" {
		t.Errorf("DefaultBaseURL = %q", jina.DefaultBaseURL)
	}
}

func TestFetchRejectsUnusableBaseURL(t *testing.T) {
	if _, err := jina.New(":not-a-url", "k").Fetch(context.Background(), "https://example.com/"); err == nil {
		t.Fatal("an unusable base URL did not surface as an error")
	}
}

// A redirecting backend is an error, never followed: the executor's allowlist
// judged the URL it was asked to fetch, and silently following a 3xx would
// move the request somewhere it never judged.
func TestFetchDoesNotFollowRedirects(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	_, err := jina.New(srv.URL, "").Fetch(context.Background(), "https://example.com/page")
	if err == nil {
		t.Fatal("Fetch followed a redirect to success, want an HTTP error")
	}
	if hits != 1 {
		t.Errorf("backend requests = %d, want 1 (the redirect not followed)", hits)
	}
}
