// Package tavily is the Tavily search backend for the web_search built-in
// tool: one POST /search per call, Bearer-authenticated, mapped onto
// webtool.SearchResult. It passes webtooltest's Searcher contract.
package tavily

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool"
)

// DefaultBaseURL is Tavily's public endpoint. A deployment may point the
// adapter elsewhere (a proxy, a gateway) — never hard-code the public host in
// a caller.
const DefaultBaseURL = "https://api.tavily.com"

// MaxResults is the fixed hit count requested per search. The tool's input
// schema is deliberately just {query}; a bounded, deterministic result size
// belongs to the platform, not the model.
const MaxResults = 5

// requestTimeout backstops a caller that arrives without a deadline; the
// executor's per-tool deadline, when shorter, wins through the context.
const requestTimeout = 60 * time.Second

// Client is a webtool.Searcher backed by Tavily.
type Client struct {
	baseURL string
	key     string
	hc      *http.Client
}

// New returns a Client for baseURL (empty means DefaultBaseURL),
// authenticating with key.
func New(baseURL, key string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		hc: &http.Client{Timeout: requestTimeout,
			// Never follow a redirect: this client talks only to the
			// operator-configured backend endpoint, so a 3xx means that
			// configuration is stale (point the base URL at the
			// post-redirect address). Chasing it would resend a request
			// carrying model-controlled data to a host nobody vetted;
			// refusing surfaces it as an HTTPError naming the drift.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Search runs one Tavily search and maps its hits in order.
func (c *Client) Search(ctx context.Context, query string) ([]webtool.SearchResult, error) {
	body, err := json.Marshal(map[string]any{"query": query, "max_results": MaxResults})
	if err != nil {
		return nil, fmt.Errorf("tavily search: encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tavily search: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily search: %w", err)
	}
	defer resp.Body.Close()

	data, truncated, err := webtool.ReadCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tavily search: reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, webtool.HTTPError("tavily search", resp.Status, data, c.key)
	}
	if truncated {
		return nil, fmt.Errorf("tavily search: response exceeds %d bytes", webtool.MaxContentBytes)
	}

	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("tavily search: decoding response: %w", err)
	}
	// max_results is a request, not a guarantee — the answer's count is the
	// backend's word, and the bounded result size the constant promises must
	// not depend on it.
	if len(out.Results) > MaxResults {
		out.Results = out.Results[:MaxResults]
	}
	hits := make([]webtool.SearchResult, len(out.Results))
	for i, r := range out.Results {
		hits[i] = webtool.SearchResult{Title: r.Title, URL: r.URL, Content: r.Content}
	}
	return hits, nil
}
