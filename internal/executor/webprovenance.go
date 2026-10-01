package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// The web_fetch provenance rule (#823). The tool's description, the reference's
// word for word, tells the model it "can only fetch EXACT URLs that have been
// provided directly by the user or have been returned in results from the
// web_search and web_fetch tools". This is where that holds: a model a prompt
// injection has turned cannot build a URL of its own — one carrying a secret in
// its query, say — and have the executor fetch it.
//
// A URL is provided when it appears in a user.message the session holds, on
// any thread, or in a web_search or web_fetch result that is not an error. An
// agent's message to another thread is an agent.thread_message_received, never
// a user.message, so a coordinator cannot launder a URL through a child. Every
// string in those payloads counts — a text block, a document's URL source, a
// search hit's source, title and snippet, a fetched page's text — and a URL in
// one is found at each "http://" or "https://" and read every way running text
// allows (urlReadings). Matching is exact after normalizeFetchURL on both
// sides.

// fetchProvenanced reports whether target, a URL web_fetch was asked for, was
// provided in the session sid. It reads the committed log: only the payloads
// that mention the target's host, so a long session is not decoded whole for
// one fetch.
func (e *Executor) fetchProvenanced(ctx context.Context, sid domain.ID, target *url.URL) (bool, error) {
	want, ok := normalizeFetchURL(target.String())
	if !ok {
		return false, nil
	}
	rows, err := e.pool.Query(ctx, `
		SELECT m.payload FROM events m
		 WHERE m.session_id = $1 AND m.type = $3
		   AND strpos(lower(m.payload::text), $2) > 0
		UNION ALL
		SELECT r.payload FROM events r
		  JOIN events u ON u.session_id = r.session_id AND u.id = r.payload->>'tool_use_id'
		 WHERE r.session_id = $1 AND r.type = $4
		   AND u.type = $5 AND u.payload->>'name' IN ('web_search', 'web_fetch')
		   AND NOT COALESCE((r.payload->>'is_error')::boolean, false)
		   AND strpos(lower(r.payload::text), $2) > 0`,
		sid.String(), strings.ToLower(target.Hostname()),
		string(domain.EventUserMessage), string(domain.EventAgentToolResult), string(domain.EventAgentToolUse))
	if err != nil {
		return false, fmt.Errorf("read the session's provided URLs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return false, fmt.Errorf("read the session's provided URLs: %w", err)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return false, fmt.Errorf("decode a payload naming the host: %w", err)
		}
		if mentionsURL(v, want) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// mentionsURL reports whether any string in the decoded JSON value v holds a
// URL that normalizes to want.
func mentionsURL(v any, want string) bool {
	switch v := v.(type) {
	case string:
		for _, found := range urlsIn(v) {
			if got, ok := normalizeFetchURL(found); ok && got == want {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if mentionsURL(x, want) {
				return true
			}
		}
	case map[string]any:
		for _, x := range v {
			if mentionsURL(x, want) {
				return true
			}
		}
	}
	return false
}

// normalizeFetchURL is the form two URLs are compared in: the scheme and host
// lowercased, as RFC 3986 makes them case-insensitive, the fragment dropped,
// since it never reaches the server, and an empty path read as "/". The path
// and query stay exactly as given. It reports false for anything but an
// absolute http(s) URL.
func normalizeFetchURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" && u.RawPath == "" {
		u.Path = "/"
	}
	return u.String(), true
}

// urlsIn returns every reading of every URL that starts at an "http://" or
// "https://" in s, matched without regard to case (urlReadings). One nested in
// another (a reader's URL wrapping its target) is read as well, since it
// appears in the text as written.
func urlsIn(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if (s[i] == 'h' || s[i] == 'H') && (hasPrefixFold(s[i:], "http://") || hasPrefixFold(s[i:], "https://")) {
			out = append(out, urlReadings(s[i:])...)
		}
	}
	return out
}

// hasPrefixFold is strings.HasPrefix ignoring ASCII case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// trailingPunct is what a sentence or markup can put after a URL; each is
// also a character a URL may end with.
const trailingPunct = ".,;:!?*'"

// urlReadings returns the ways the URL at the start of s can be read. Running
// text does not say where a URL ends: a full stop or a markdown link's closing
// parenthesis follows one, yet either can also be its last character, as in a
// search hit's source ending in "?". So every reading is kept — the run to
// whitespace or a character no URL carries, the run to a closing parenthesis
// or bracket it did not open (so a balanced pair, as in a Wikipedia title,
// stays), and each without trailing punctuation — and a URL matching any of
// them is one the text holds. Each is a prefix of the text as written.
func urlReadings(s string) []string {
	run := s[:strings.IndexFunc(s+" ", func(r rune) bool {
		return r <= ' ' || strings.ContainsRune("<>\"`{}|\\^", r)
	})]
	parens, brackets, end := 0, 0, 0
scan:
	for ; end < len(run); end++ {
		switch run[end] {
		case '(':
			parens++
		case '[':
			brackets++
		case ')':
			if parens == 0 {
				break scan
			}
			parens--
		case ']':
			if brackets == 0 {
				break scan
			}
			brackets--
		}
	}
	balanced := run[:end]
	var out []string
	for _, r := range []string{run, strings.TrimRight(run, trailingPunct), balanced, strings.TrimRight(balanced, trailingPunct)} {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}
