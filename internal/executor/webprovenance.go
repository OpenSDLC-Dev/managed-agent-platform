package executor

import (
	"context"
	"encoding/json"
	"errors"
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
// (urlMatcher). Two URLs are the same when normalizeFetchURL says so.
//
// The reading is bounded, because the text is not ours: a fetched page can hold
// 100 KiB of URLs run together, and reading each occurrence to the end of such
// a run, cut at every punctuation mark, is quadratic. A reading that can match
// is no longer than urlMatcher's window, an occurrence naming another host is
// skipped after one parse of its authority, and a lookup that would parse more
// than readingBudget readings refuses the fetch rather than stall the executor.

// maxFetchURL is the longest URL web_fetch accepts. It bounds the window every
// reading is read in, and no browser or reader takes a longer one.
const maxFetchURL = 8 << 10

// readingBudget is the most URL parses one lookup spends. A session past it —
// in practice a page built to exhaust it — refuses the fetch.
const readingBudget = 500_000

// errReadingBudget is a lookup that spent readingBudget before deciding.
var errReadingBudget = errors.New("the session holds too many URLs naming this host to check this one")

// webFetchSource returns the given URL that raw, the URL web_fetch was asked
// for, names in the session sid, exactly as it was written there, or "" when
// none does. It reads the committed log: only the payloads that mention the
// host or hold non-ASCII text (a host can be spelled there in a Unicode form
// that folds to the request's), when the request's host is plain ASCII.
func (e *Executor) webFetchSource(ctx context.Context, sid domain.ID, raw string) (string, error) {
	m, ok := newURLMatcher(raw)
	if !ok {
		return "", nil
	}
	// The prefilter only narrows, so it is skipped wherever a payload could
	// spell the host differently from the request: a Unicode name or its
	// A-label, a percent-escape. An empty key matches every row.
	key := ""
	if u, err := url.Parse(m.raw); err == nil {
		if h := u.Hostname(); isPlainASCIIHost(h) && !strings.Contains(strings.ToLower(h), "xn--") {
			key = h
		}
	}
	const narrow = `($2 = '' OR strpos(lower(%[1]s::text), lower($2)) > 0
		       OR octet_length(%[1]s::text) <> char_length(%[1]s::text))`
	rows, err := e.pool.Query(ctx, `
		SELECT m.payload FROM events m
		 WHERE m.session_id = $1 AND m.type = ANY($3)
		   AND `+fmt.Sprintf(narrow, "m.payload")+`
		UNION ALL
		SELECT r.payload FROM events r
		  JOIN events u ON u.session_id = r.session_id AND u.id = r.payload->>'tool_use_id'
		 WHERE r.session_id = $1 AND r.type = $4
		   AND u.type = $5 AND u.payload->>'name' IN ('web_search', 'web_fetch')
		   AND NOT COALESCE((r.payload->>'is_error')::boolean, false)
		   AND `+fmt.Sprintf(narrow, "r.payload")+`
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
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return "", fmt.Errorf("read the session's given URLs: %w", err)
		}
		var v any
		if err := json.Unmarshal(payload, &v); err != nil {
			return "", fmt.Errorf("decode a payload naming the host: %w", err)
		}
		if m.walk(v) {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if m.found == "" && m.budget <= 0 {
		return "", errReadingBudget
	}
	return m.found, nil
}

// urlMatcher looks through text for a reading of a URL that normalizes to
// want: the given URL a request names. found is the first such reading, or one
// spelled exactly as the request when there is one.
type urlMatcher struct {
	raw, want, host string // the request, its normalized form, and its canonical host[:port]
	min, max        int    // the byte lengths a matching reading can have
	budget          int    // URL parses left
	found           string
	exact           bool
}

// newURLMatcher returns the matcher for the request raw, or false when raw is
// not an absolute http(s) URL with a host, or is longer than maxFetchURL. A
// reading normalizes to at most three times its length (a path's "\" or "é"
// becomes a three-byte escape, a Unicode label its longer A-label) and at least
// a third of it (a full-width host letter is three bytes and folds to one), so
// the window is three times the request's normalized length either way, with
// room for the "/" an empty path gains.
func newURLMatcher(raw string) (*urlMatcher, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) > maxFetchURL {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	want, ok := normalizeURL(u)
	if !ok {
		return nil, false
	}
	host, _ := canonicalHostPort(u)
	return &urlMatcher{raw: raw, want: want, host: host,
		min: max(0, (len(want)-16)/3), max: 3*len(want) + 64, budget: readingBudget}, true
}

// walk reads every string in the decoded JSON value v. It reports true when the
// search is over: an exact match found, or the budget spent.
func (m *urlMatcher) walk(v any) bool {
	switch v := v.(type) {
	case string:
		return m.scan(v)
	case []any:
		for _, x := range v {
			if m.walk(x) {
				return true
			}
		}
	case map[string]any:
		for _, x := range v {
			if m.walk(x) {
				return true
			}
		}
	}
	return false
}

// scan reads every URL that starts at an "http://" or "https://" in s, matched
// without regard to case. One nested in another (a reader's URL wrapping its
// target) is read as well, since it appears in the text as written.
func (m *urlMatcher) scan(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] == 'h' || s[i] == 'H') && (hasPrefixFold(s[i:], "http://") || hasPrefixFold(s[i:], "https://")) {
			if m.occurrence(s[i:]) {
				return true
			}
		}
	}
	return false
}

// occurrence tries the readings of the URL at the start of s. Running text does
// not say where a URL ends. It runs to whitespace or a character no URL carries
// unescaped (<, >, a double quote, a backtick), but a sentence's full stop, a
// markdown link's closing parenthesis, CJK text with no space before it, or a
// possessive's apostrophe can follow it inside that run — and each of those can
// also be part of a URL, as a search hit's source ending in "?" or a Wikipedia
// title's parentheses are. So the readings are the whole run, and the run cut
// just before each character that can end a URL in text (endsURLInText), each
// a substring of the text as written. Only the window can match, and only an
// occurrence whose authority names the request's host has any reading worth a
// parse beyond the authority's own.
func (m *urlMatcher) occurrence(s string) bool {
	run, whole := s, true
	if len(run) > m.max {
		run, whole = run[:m.max], false
	}
	if end := strings.IndexFunc(run, func(r rune) bool {
		return unicode.IsSpace(r) || r < ' ' || strings.ContainsRune("<>\"`", r)
	}); end >= 0 {
		run, whole = run[:end], true
	}
	start := strings.Index(run, "://") + len("://")
	authEnd := len(run)
	if i := strings.IndexAny(run[start:], "/?#"); i >= 0 {
		authEnd = start + i
	}
	// The authority, and each cut inside it ("example.com." in a sentence),
	// is parsed once; only a host matching the request's goes further.
	hostOK := false
	for i, r := range run[start:authEnd] {
		if k := start + i; k > start && endsURLInText(r) && m.sameHost(run[:k]) {
			if m.try(run[:k]) {
				return true
			}
		}
	}
	if m.sameHost(run[:authEnd]) {
		hostOK = true
	}
	if m.budget <= 0 {
		return true
	}
	if !hostOK {
		return false
	}
	for i, r := range run[authEnd:] {
		if k := authEnd + i; endsURLInText(r) && k >= m.min && m.try(run[:k]) {
			return true
		}
	}
	return whole && len(run) >= m.min && m.try(run)
}

// sameHost reports whether the authority of the reading r names the request's
// host, spending one parse.
func (m *urlMatcher) sameHost(r string) bool {
	m.budget--
	u, err := url.Parse(r)
	if err != nil {
		return false
	}
	h, ok := canonicalHostPort(u)
	return ok && h == m.host
}

// try compares one reading with the request. It reports true when the search
// is over: this reading is spelled exactly as the request, or the budget is
// spent.
func (m *urlMatcher) try(r string) bool {
	m.budget--
	if got, ok := normalizeFetchURL(r); ok && got == m.want {
		if r == m.raw {
			m.found, m.exact = r, true
			return true
		}
		if m.found == "" {
			m.found = r
		}
	}
	return m.budget <= 0
}

// normalizeFetchURL is the form two URLs are compared in: the scheme lowercased,
// the host in the form the egress allowlist compares (canonicalHostPort), the
// fragment dropped, and an empty path read as "/". The rest is compared as Go's
// URL type spells it. It reports false for anything but an absolute http(s) URL
// with a host.
func normalizeFetchURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	return normalizeURL(u)
}

// normalizeURL is normalizeFetchURL on a parsed URL, which it changes.
func normalizeURL(u *url.URL) (string, bool) {
	if u.Opaque != "" {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	host, ok := canonicalHostPort(u)
	if !ok {
		return "", false
	}
	u.Host = host
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" && u.RawPath == "" {
		u.Path = "/"
	}
	return u.String(), true
}

// canonicalHostPort is u's host in the form the egress allowlist compares
// (egress.CanonicalHost: ASCII case folded, a Unicode name as its A-label, so a
// look-alike such as "wİkİpedİa" stays a different host), with its port.
func canonicalHostPort(u *url.URL) (string, bool) {
	if u.Host == "" {
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
	return host, true
}

// isPlainASCIIHost reports whether h is spelled in ASCII with no escape, so its
// lowercase form is how any ASCII payload naming it spells it.
func isPlainASCIIHost(h string) bool {
	for i := range len(h) {
		if h[i] > unicode.MaxASCII || h[i] == '%' {
			return false
		}
	}
	return h != ""
}

// hasPrefixFold is strings.HasPrefix ignoring ASCII case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// endsURLInText reports whether r is a character running text can put right
// after a URL: ASCII sentence punctuation, a quote, a bracket or a fragment's
// "#", or any non-ASCII character (a CJK comma, an em dash, CJK text written
// with no space after the URL).
func endsURLInText(r rune) bool {
	return r > unicode.MaxASCII || strings.ContainsRune(".,;:!?*'()[]{}|\\^#", r)
}
