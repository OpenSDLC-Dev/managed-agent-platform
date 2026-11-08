package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// DefaultRepo is this repository. The pointers are issue numbers in one
// repository and nowhere else, so there is nothing to derive and nothing to
// configure; -repo exists for the test that drives this against a fake.
const DefaultRepo = "OpenSDLC-Dev/managed-agent-platform"

// issueState is the sliver of GitHub's issue representation this needs. The
// list endpoint returns pull requests too, which is wanted: the registry cites
// PR #198 as provenance, and a PR shares the issue number space.
type issueState struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// fetchStates returns number -> open for every issue and pull request in the
// repository, then fills in by number anything the listing did not carry.
//
// It lists rather than looping over the ~70 numbers the file cites because the
// unauthenticated ceiling is 60 requests per hour per IP — a per-number loop
// would exhaust it and turn a real check into a rate-limit error. A token
// raises the ceiling and is used when the environment offers one, but is not
// required: this repository is public, and a scheduled run that could only work
// with a credential is one more thing to rot.
//
// The listing follows each page's Link header rather than counting pages.
// Once the repository passed 1,000 issues and pull requests, GitHub refused
// `page=11` to an unauthenticated request and served the cursor URL its Link
// header names for the same page (#1029). A short page still ends the listing,
// as it did before.
func fetchStates(ctx context.Context, api, repo string, want []int) (func(int) (bool, bool), error) {
	states := map[int]bool{}
	next, err := url.Parse(fmt.Sprintf("%s/repos/%s/issues?state=all&per_page=100", api, repo))
	if err != nil {
		return nil, err
	}
	for pages := 0; next != nil; pages++ {
		if pages == 50 {
			return nil, fmt.Errorf("issue listing did not terminate after 50 pages")
		}
		var batch []issueState
		header, err := getJSON(ctx, next.String(), &batch)
		if err != nil {
			return nil, err
		}
		for _, is := range batch {
			states[is.Number] = is.State == "open"
		}
		if len(batch) < 100 {
			break
		}
		if next, err = nextLink(header.Values("Link"), next); err != nil {
			return nil, err
		}
	}
	for _, n := range want {
		if _, ok := states[n]; ok {
			continue
		}
		var one issueState
		_, err := getJSON(ctx, fmt.Sprintf("%s/repos/%s/issues/%d", api, repo, n), &one)
		if err != nil {
			// A number the registry cites that GitHub will not serve is a
			// finding, not a crash: Check reports it as unknown-issue with the
			// entry that cites it, which is the useful place to read it.
			//
			// The status is read from the response, never from the error text.
			// Every error here names the URL it failed on, so a substring test
			// for "404" reads a transport failure on issue #404 as an absent
			// issue — the one number where the difference matters most.
			var he *statusError
			if errors.As(err, &he) && he.status == http.StatusNotFound {
				continue
			}
			return nil, err
		}
		states[one.Number] = one.State == "open"
	}
	return func(n int) (bool, bool) {
		open, known := states[n]
		return open, known
	}, nil
}

// nextLink is the rel="next" target of a response's Link fields, resolved
// against the page that sent them, or nil when they name none. A target must
// stay on that page's scheme and host, because every request carries the token
// when the environment offers one. The first page is on the API's own, so every
// page is.
func nextLink(fields []string, page *url.URL) (*url.URL, error) {
	for rest := strings.Join(fields, ","); ; {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			return nil, nil
		}
		target, after, ok := strings.Cut(rest[open+1:], ">")
		if !ok {
			return nil, fmt.Errorf("issue listing: a Link header with no closing \">\": %q", strings.Join(fields, ","))
		}
		rest = after
		params, _, _ := strings.Cut(rest, "<")
		isNext := false
		for _, p := range strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ',' }) {
			key, val, _ := strings.Cut(p, "=")
			if strings.EqualFold(strings.TrimSpace(key), "rel") &&
				slices.Contains(strings.Fields(strings.ToLower(strings.Trim(strings.TrimSpace(val), `"`))), "next") {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		ref, err := url.Parse(target)
		if err != nil {
			return nil, fmt.Errorf("issue listing: the next link %q: %w", target, err)
		}
		next := page.ResolveReference(ref)
		if next.Scheme != page.Scheme || next.Host != page.Host {
			return nil, fmt.Errorf("issue listing: the next link %q leaves %s://%s", target, page.Scheme, page.Host)
		}
		return next, nil
	}
}

// getJSON decodes a 200 answer into `into` and returns the answer's header.
func getJSON(ctx context.Context, addr string, into any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok := token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// Name the rate limit when that is what happened: it is the one
		// failure a reader of a red scheduled run would otherwise misread as
		// the registry having rotted.
		if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining == "0" {
			return nil, &statusError{resp.StatusCode, fmt.Sprintf("GET %s: %s — the GitHub rate limit is exhausted (reset at %s); set GITHUB_TOKEN to raise it",
				addr, resp.Status, resp.Header.Get("X-RateLimit-Reset"))}
		}
		return nil, &statusError{resp.StatusCode, fmt.Sprintf("GET %s: %s: %s", addr, resp.Status, strings.TrimSpace(string(body)))}
	}
	return resp.Header, json.Unmarshal(body, into)
}

// statusError carries the HTTP status alongside the message, so a caller can
// tell a 404 from a transport failure without reading prose.
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

func token() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
