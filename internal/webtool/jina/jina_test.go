package jina_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	// Jina Reader takes the target URL verbatim as the request path.
	if uri != "/"+target {
		t.Errorf("request URI = %q, want %q", uri, "/"+target)
	}
	if auth != "Bearer jina-k1" {
		t.Errorf("Authorization = %q, want Bearer jina-k1", auth)
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
	// An empty base URL wires the default in; nothing is called here.
	if c := jina.New("", "k"); c == nil {
		t.Fatal("New returned nil")
	}
}

func TestFetchRejectsUnusableBaseURL(t *testing.T) {
	if _, err := jina.New(":not-a-url", "k").Fetch(context.Background(), "https://example.com/"); err == nil {
		t.Fatal("an unusable base URL did not surface as an error")
	}
}
