package tavily_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/tavily"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/webtooltest"
)

// renderHits builds the Tavily /search response body for the contract suite.
func renderHits(hits []webtool.SearchResult) (string, string) {
	type hit struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	}
	out := struct {
		Results []hit `json:"results"`
	}{Results: []hit{}}
	for _, h := range hits {
		out.Results = append(out.Results, hit(h))
	}
	body, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return string(body), "application/json"
}

func TestSearcherContract(t *testing.T) {
	webtooltest.RunSearcherContract(t, webtooltest.SearcherBackend{
		New: func(baseURL, key string) webtool.Searcher {
			return tavily.New(baseURL, key)
		},
		Render: renderHits,
	})
}

func TestSearchRequestShape(t *testing.T) {
	var (
		method, path, auth, ct string
		body                   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		auth, ct = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(srv.Close)

	// The trailing slash on the base URL must not double up in the path.
	if _, err := tavily.New(srv.URL+"/", "tvly-k1").Search(context.Background(), "how do magnets work"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if method != http.MethodPost || path != "/search" {
		t.Errorf("request = %s %s, want POST /search", method, path)
	}
	if auth != "Bearer tvly-k1" {
		t.Errorf("Authorization = %q, want Bearer tvly-k1", auth)
	}
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body["query"] != "how do magnets work" {
		t.Errorf("query = %v, want the search query", body["query"])
	}
	if body["max_results"] != float64(tavily.MaxResults) {
		t.Errorf("max_results = %v, want %d", body["max_results"], tavily.MaxResults)
	}
}

func TestSearchCapsTheHitCount(t *testing.T) {
	// max_results is a request; a backend answering with more must not have
	// its word taken for the bounded result size.
	var over []webtool.SearchResult
	for i := 0; i < tavily.MaxResults+2; i++ {
		over = append(over, webtool.SearchResult{Title: "t", URL: "https://a.example/", Content: "c"})
	}
	body, ct := renderHits(over)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ct)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	hits, err := tavily.New(srv.URL, "k").Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != tavily.MaxResults {
		t.Errorf("len(hits) = %d, want capped at %d", len(hits), tavily.MaxResults)
	}
}

func TestSearchRejectsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)
	if _, err := tavily.New(srv.URL, "k").Search(context.Background(), "q"); err == nil {
		t.Fatal("a malformed response body did not surface as an error")
	}
}

func TestDefaultBaseURL(t *testing.T) {
	if tavily.DefaultBaseURL != "https://api.tavily.com" {
		t.Errorf("DefaultBaseURL = %q", tavily.DefaultBaseURL)
	}
	// An empty base URL wires the default in; nothing is called here.
	if c := tavily.New("", "k"); c == nil {
		t.Fatal("New returned nil")
	}
}

func TestSearchRejectsUnusableBaseURL(t *testing.T) {
	if _, err := tavily.New(":not-a-url", "k").Search(context.Background(), "q"); err == nil {
		t.Fatal("an unusable base URL did not surface as an error")
	}
}
