package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
func fetchStates(ctx context.Context, api, repo string, want []int) (func(int) (bool, bool), error) {
	states := map[int]bool{}
	for page := 1; ; page++ {
		url := fmt.Sprintf("%s/repos/%s/issues?state=all&per_page=100&page=%d", api, repo, page)
		var batch []issueState
		if err := getJSON(ctx, url, &batch); err != nil {
			return nil, err
		}
		for _, is := range batch {
			states[is.Number] = is.State == "open"
		}
		if len(batch) < 100 {
			break
		}
		if page > 50 {
			return nil, fmt.Errorf("issue listing did not terminate after 50 pages")
		}
	}
	for _, n := range want {
		if _, ok := states[n]; ok {
			continue
		}
		var one issueState
		err := getJSON(ctx, fmt.Sprintf("%s/repos/%s/issues/%d", api, repo, n), &one)
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

func getJSON(ctx context.Context, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok := token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		// Name the rate limit when that is what happened: it is the one
		// failure a reader of a red scheduled run would otherwise misread as
		// the registry having rotted.
		if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining == "0" {
			return &statusError{resp.StatusCode, fmt.Sprintf("GET %s: %s — the GitHub rate limit is exhausted (reset at %s); set GITHUB_TOKEN to raise it",
				url, resp.Status, resp.Header.Get("X-RateLimit-Reset"))}
		}
		return &statusError{resp.StatusCode, fmt.Sprintf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))}
	}
	return json.Unmarshal(body, into)
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
