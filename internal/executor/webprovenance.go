package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// The web_fetch provenance rule (#823). The tool's description, the reference's
// word for word, tells the model it "can only fetch EXACT URLs that have been
// provided directly by the user or have been returned in results from the
// web_search and web_fetch tools". This is where that holds: a model a prompt
// injection has turned cannot build a URL of its own — one carrying a secret in
// its query or fragment, say — and have the executor fetch it.
//
// The model's URL only chooses: what is fetched is the given URL itself, as it
// was written in the session, so nothing the model adds — a fragment, a
// spelling Go would re-encode into the same form — reaches the reader.
//
// A URL is given when it appears in what a person wrote into the session — a
// user.message, a user.define_outcome, a user.tool_confirmation (its
// deny_message) or an operator's system.message, on any thread — in a
// web_search or web_fetch result that is not an error, or as the input of a
// web_fetch call a person allowed by confirmation. An agent's message to
// another thread is an agent.thread_message_received, so a coordinator cannot
// launder a URL through a child; other tools' results, client-side tool results
// included, are not counted. Every string in those payloads is read (a text
// block, a document's URL source, a search hit's source, title and snippet, a
// fetched page's text), and a URL in one is read every way running text allows
// (urlReadings). Two URLs are the same when normalizeFetchURL says so.

// webFetchSource returns the given URL that raw, the URL web_fetch was asked
// for, names in the session sid, exactly as it was written there, or "" when
// none does. It reads the committed log: only the payloads that mention the
// host, when the host is plain ASCII, so a long session is not decoded whole
// for one fetch.
func (e *Executor) webFetchSource(ctx context.Context, sid domain.ID, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	want, ok := normalizeFetchURL(raw)
	if !ok {
		return "", nil
	}
	// The prefilter only narrows, so it is skipped wherever a payload could
	// spell the host differently from the request: a Unicode name or its
	// A-label, a percent-escape. strpos with "" matches every row.
	key := ""
	if u, err := url.Parse(raw); err == nil {
		if h := u.Hostname(); isPlainASCIIHost(h) && !strings.Contains(strings.ToLower(h), "xn--") {
			key = h
		}
	}
	rows, err := e.pool.Query(ctx, `
		SELECT m.payload FROM events m
		 WHERE m.session_id = $1 AND m.type = ANY($3)
		   AND strpos(lower(m.payload::text), lower($2)) > 0
		UNION ALL
		SELECT r.payload FROM events r
		  JOIN events u ON u.session_id = r.session_id AND u.id = r.payload->>'tool_use_id'
		 WHERE r.session_id = $1 AND r.type = $4
		   AND u.type = $5 AND u.payload->>'name' IN ('web_search', 'web_fetch')
		   AND NOT COALESCE((r.payload->>'is_error')::boolean, false)
		   AND strpos(lower(r.payload::text), lower($2)) > 0
		UNION ALL
		SELECT u.payload->'input' FROM events u
		 WHERE u.session_id = $1 AND u.type = $5 AND u.payload->>'name' = 'web_fetch'
		   AND EXISTS (SELECT 1 FROM events c
		                WHERE c.session_id = u.session_id AND c.type = $6
		                  AND c.payload->>'tool_use_id' = u.id AND c.payload->>'result' = 'allow')`,
		sid.String(), key,
		[]string{string(domain.EventUserMessage), string(domain.EventUserDefineOutcome),
			string(domain.EventUserToolConfirm), string(domain.EventSystemMessage)},
		string(domain.EventAgentToolResult), string(domain.EventAgentToolUse), string(domain.EventUserToolConfirm))
	if err != nil {
		return "", fmt.Errorf("read the session's given URLs: %w", err)
	}
	defer rows.Close()
	found := ""
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return "", fmt.Errorf("read the session's given URLs: %w", err)
		}
		var v any
		if err := json.Unmarshal(payload, &v); err != nil {
			return "", fmt.Errorf("decode a payload naming the host: %w", err)
		}
		if given := givenIn(v, want, raw); given == raw {
			return given, rows.Err()
		} else if given != "" && found == "" {
			found = given
		}
	}
	return found, rows.Err()
}

// givenIn returns a URL reading in any string of the decoded JSON value v that
// normalizes to want, preferring one spelled exactly as raw, or "".
func givenIn(v any, want, raw string) string {
	found := ""
	var walk func(any) bool
	walk = func(v any) bool {
		switch v := v.(type) {
		case string:
			for _, r := range urlsIn(v) {
				if got, ok := normalizeFetchURL(r); ok && got == want {
					if r == raw {
						found = r
						return true
					}
					if found == "" {
						found = r
					}
				}
			}
		case []any:
			for _, x := range v {
				if walk(x) {
					return true
				}
			}
		case map[string]any:
			for _, x := range v {
				if walk(x) {
					return true
				}
			}
		}
		return false
	}
	walk(v)
	return found
}

// normalizeFetchURL is the form two URLs are compared in: the scheme lowercased,
// the host in the form the egress allowlist compares (egress.CanonicalHost:
// ASCII case folded, a Unicode name as its A-label, so a look-alike such as
// "wİkİpedİa" stays a different host), the fragment dropped, and an empty path
// read as "/". The rest is compared as Go's URL type spells it. It reports
// false for anything but an absolute http(s) URL with a host.
func normalizeFetchURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" || u.Host == "" {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	host := egress.CanonicalHost(u.Hostname())
	if host == "" {
		return "", false
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	u.Host = host
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" && u.RawPath == "" {
		u.Path = "/"
	}
	return u.String(), true
}

// isPlainASCIIHost reports whether h is spelled in ASCII with no escape, so its
// lowercase form is how any payload naming it spells it.
func isPlainASCIIHost(h string) bool {
	for i := range len(h) {
		if h[i] > unicode.MaxASCII || h[i] == '%' {
			return false
		}
	}
	return h != ""
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

// urlReadings returns the ways the URL at the start of s can be read. Running
// text does not say where a URL ends. It runs to whitespace or a character no
// URL carries unescaped (<, >, a double quote, a backtick), but a sentence's
// full stop, a markdown link's closing parenthesis, a CJK comma with more text
// after it, or a possessive's apostrophe can follow it inside that run — and
// each of those can also be part of a URL, as a search hit's source ending in
// "?" or a Wikipedia title's parentheses are. So every reading is kept: the
// whole run, and the run cut just before each character that can end a URL in
// text. Each is a prefix of the text as written, so none carries anything the
// text does not.
func urlReadings(s string) []string {
	end := strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r < ' ' || strings.ContainsRune("<>\"`", r)
	})
	if end < 0 {
		end = len(s)
	}
	run := s[:end]
	out := []string{run}
	for i, r := range run {
		if i > 0 && endsURLInText(r) && out[len(out)-1] != run[:i] {
			out = append(out, run[:i])
		}
	}
	return out
}

// endsURLInText reports whether r is a character running text can put right
// after a URL: ASCII sentence punctuation, a quote or bracket, or any non-ASCII
// punctuation or symbol (a CJK comma or full stop, an em dash).
func endsURLInText(r rune) bool {
	if r > unicode.MaxASCII {
		return unicode.IsPunct(r) || unicode.IsSymbol(r)
	}
	return strings.ContainsRune(".,;:!?*'()[]{}|\\^", r)
}
