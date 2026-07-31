// Package jina is the Jina Reader backend for the web_fetch built-in tool:
// one GET per call with the target URL as the request path, answered as
// markdown text. It passes webtooltest's Fetcher contract.
package jina

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool"
)

// DefaultBaseURL is Jina Reader's public endpoint. A deployment may point the
// adapter elsewhere (a proxy, a self-hosted reader) — never hard-code the
// public host in a caller.
const DefaultBaseURL = "https://r.jina.ai"

// requestTimeout backstops a caller that arrives without a deadline; the
// executor's per-tool deadline, when shorter, wins through the context.
const requestTimeout = 60 * time.Second

// Client is a webtool.Fetcher backed by Jina Reader.
type Client struct {
	baseURL string
	key     string
	hc      *http.Client
}

// New returns a Client for baseURL (empty means DefaultBaseURL). An empty key
// sends no Authorization header — Jina's rate-limited free tier takes none.
func New(baseURL, key string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		hc:      &http.Client{Timeout: requestTimeout},
	}
}

// Fetch reads one page through the reader. The target is model-supplied
// input, so it is validated before anything touches the network: absolute
// http(s) URLs only.
func (c *Client) Fetch(ctx context.Context, target string) (webtool.FetchResult, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return webtool.FetchResult{}, fmt.Errorf("web_fetch: not an http(s) URL: %q", target)
	}
	// Jina Reader's shape: the full target URL rides as the request path —
	// percent-encoded as one segment, because concatenated raw, everything
	// from the target's first '#' would parse as the OUTER URL's fragment and
	// silently never be sent (the reader decodes the segment and keeps the
	// fragment; verified against the live endpoint).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+url.PathEscape(target), nil)
	if err != nil {
		return webtool.FetchResult{}, fmt.Errorf("jina fetch: %w", err)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return webtool.FetchResult{}, fmt.Errorf("jina fetch: %w", err)
	}
	defer resp.Body.Close()

	data, truncated, err := webtool.ReadCapped(resp.Body)
	if err != nil {
		return webtool.FetchResult{}, fmt.Errorf("jina fetch: reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return webtool.FetchResult{}, webtool.HTTPError("jina fetch", resp.Status, data, c.key)
	}
	return webtool.FetchResult{Content: string(data), Truncated: truncated}, nil
}
