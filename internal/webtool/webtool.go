// Package webtool is the seam for the web_fetch / web_search built-in tools
// (docs/plan/15_web-tools.md, #47): the Searcher and Fetcher interfaces the
// executor drives, with one adapter package per backend (tavily/, jina/).
//
// The tools execute in the executor's own process on both deployment modes —
// never in the sandbox, never on the BYOC worker, never through the
// per-session egress gate: the environment's networking policy deliberately
// does not govern them (the reference documents exactly that), and the
// official worker implements only the six sandbox tools. Backend variability
// lives behind these interfaces with one shared contract suite (webtooltest),
// as provider/, sandbox/ and blob/ do for theirs.
package webtool

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// SearchResult is one search hit — the fields the wire's search_result block
// needs: a title, the source URL, and text content.
type SearchResult struct {
	Title   string
	URL     string
	Content string
}

// Searcher answers a web_search call.
type Searcher interface {
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// FetchResult is a fetched page. Truncated reports that Content stopped at
// MaxContentBytes rather than at the page's own end.
type FetchResult struct {
	Content   string
	Truncated bool
}

// Fetcher answers a web_fetch call.
type Fetcher interface {
	Fetch(ctx context.Context, url string) (FetchResult, error)
}

// MaxContentBytes caps what an adapter reads from a response body. The body is
// untrusted-length input (a fetched page is whatever the site serves), so the
// read is capped the way sandbox file reads are. A fetch truncates at the cap
// and says so; a search response past it is refused — a hits payload that
// large is a broken or hostile endpoint, and truncated JSON decodes as
// nothing.
const MaxContentBytes = 4 << 20

// maxSnippet bounds the body excerpt an HTTPError carries.
const maxSnippet = 200

// ReadCapped reads r up to MaxContentBytes and reports whether the source held
// more. The one-past-the-cap read is how "more" is detected without draining
// an unbounded stream.
func ReadCapped(r io.Reader) (data []byte, truncated bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, MaxContentBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > MaxContentBytes {
		return data[:MaxContentBytes], true, nil
	}
	return data, false, nil
}

// HTTPError is the error for a non-2xx backend response: the operation, the
// status, and a short body excerpt with the credential redacted — redacted
// before the excerpt is cut, so a truncation can never expose what a full
// occurrence would have hidden. An endpoint that echoes its Authorization
// header back must not land the key in an error that becomes a tool result
// event.
func HTTPError(op, status string, body []byte, secret string) error {
	snippet := string(body)
	if secret != "" {
		snippet = strings.ReplaceAll(snippet, secret, "[redacted]")
	}
	if len(snippet) > maxSnippet {
		snippet = snippet[:maxSnippet]
	}
	return fmt.Errorf("%s: %s: %s", op, status, snippet)
}
