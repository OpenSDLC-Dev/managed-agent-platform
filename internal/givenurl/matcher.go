package givenurl

import (
	"errors"
	"maps"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
)

// maxFetchURL is the longest URL web_fetch accepts. It bounds the window every
// reading is read in, and no browser or reader takes a longer one.
const maxFetchURL = 8 << 10

// readingBudget is the most one lookup spends reading the payloads the index
// left unindexed (indexBudget), in bytes parsed: a parse costs its length, a
// scan an eighth of what it scans, and a plain tail's string comparison a
// sixteenth of its length. A lookup past it — in practice one that meets pages
// built to exhaust it — refuses the fetch. Counted in bytes, not parses,
// because a parse costs its length and a request may be long.
const readingBudget = 64 << 20

// ErrReadingBudget is a lookup that spent readingBudget before deciding.
var ErrReadingBudget = errors.New("the session holds too many URLs naming this host to check this one")

// urlMatcher looks through text for a reading of a URL that normalizes to
// want: the given URL a request names. found is the first such reading, or one
// spelled exactly as the request when there is one.
type urlMatcher struct {
	raw, want, host  string // the request, its normalized form, and its canonical host[:port]
	head, tail       string // want split after its authority: "scheme://authority" and the rest
	authMax          int    // the longest authority (userinfo, host, port) a match can be spelled with
	tailMin, tailMax int    // the byte lengths of what follows that authority in a match
	budget           int    // bytes of URL left to parse
	found            string
	exact            bool
}

// newURLMatcher returns the matcher for the request raw, or false when raw is
// not an absolute http(s) URL with a host, or is longer than maxFetchURL. A
// spelling normalizes to at most three times its length (a path's "\" or "é"
// becomes a three-byte escape, a Unicode label its longer A-label) and at least
// a third of it (a full-width letter is three bytes and folds to one), so each
// part of a match — the authority, and what follows it — is read in a window
// three times that part of the request's normalized form, with room for the "/"
// an empty path gains. A host spelled with characters IDNA deletes (soft
// hyphens) past that window is not found: a refusal, never a wrong fetch.
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
	w := windowsOf(want)
	return &urlMatcher{raw: raw, want: want, host: host, head: want[:w.head], tail: want[w.head:],
		authMax: w.authMax, tailMin: w.tailMin, tailMax: w.tailMax,
		budget: readingBudget}, true
}

// windows are what a request's normalized form want decides of how a match
// is read: where want's authority ends (head), the longest authority a match
// can be spelled with, and the fewest and most bytes of what follows it.
// newURLMatcher and the index (readings) both take them from here, so the two
// read every occurrence alike.
type windows struct{ head, authMax, tailMin, tailMax int }

func windowsOf(want string) windows {
	rest := want[strings.Index(want, "://")+len("://"):]
	auth := len(rest)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		auth = i
	}
	head := len(want) - len(rest) + auth
	tail := len(want) - head
	return windows{head: head, authMax: 3*auth + 64, tailMin: max(0, (tail-16)/3), tailMax: 3*tail + 64}
}

// walk reads every string in the decoded JSON value v, an object's members in
// key order, the order the index records them in (readings). It reports true
// when the search is over: an exact match found, or the budget spent.
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
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if m.walk(v[k]) {
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
// title's parentheses are. Every reading is a substring of the text as written.
//
// The authority is read a fixed number of ways, each one parse: to the first
// "/", "?" or "#"; to the first non-ASCII character; to the first ASCII
// character no host carries; and, for a reading that ends in its authority,
// with its trailing dots and colons trimmed first. Only an occurrence whose
// authority, read to its "/", "?" or "#", is the request's goes on: the run cut
// just before each character that can end a URL in text (endsURLInText), and
// the whole run, within the tail's window. The scan itself is charged too.
func (m *urlMatcher) occurrence(s string) bool {
	start := strings.Index(s, "://") + len("://")
	run, whole := s, true
	if limit := start + m.authMax + m.tailMax; len(run) > limit {
		run, whole = run[:limit], false
	}
	if end := strings.IndexFunc(run, endsRun); end >= 0 {
		run, whole = run[:end], true
	}
	m.budget -= len(run)/8 + 1
	area := run[start:min(len(run), start+m.authMax)]
	authEnd := -1
	if i := strings.IndexAny(area, "/?#"); i >= 0 {
		authEnd = start + i
	} else if len(run) <= start+m.authMax && whole {
		authEnd = len(run)
	}
	var ends []int
	if authEnd >= 0 {
		ends = append(ends, authEnd)
	}
	if i := strings.IndexFunc(area, func(r rune) bool { return r > unicode.MaxASCII }); i >= 0 {
		ends = append(ends, start+i)
	}
	if i := strings.IndexFunc(area, func(r rune) bool {
		return r <= unicode.MaxASCII && !isHostByte(byte(r))
	}); i >= 0 {
		ends = append(ends, start+i)
	}
	headOK := false
	for _, e := range ends {
		if e == authEnd {
			headOK = m.sameHead(run[:e])
		}
		// A reading that ends in its authority: trailing dots and colons
		// trimmed first (a sentence's "example.com." or "example.com:"),
		// then as it stands.
		k := e
		for n := 0; k > start && n < 16 && (run[k-1] == '.' || run[k-1] == ':'); n++ {
			k--
		}
		for _, c := range []int{k, e} {
			if c != authEnd && c > start && m.sameHost(run[:c]) {
				if m.try(run[:c]) {
					return true
				}
				break
			}
			if k == e {
				break
			}
		}
		if m.budget <= 0 {
			return true
		}
	}
	if !headOK {
		return false
	}
	for i, r := range run[authEnd:] {
		if k := authEnd + i; endsURLInText(r) && i >= m.tailMin && m.tryTail(run, authEnd, k) {
			return true
		}
	}
	return whole && len(run)-authEnd >= m.tailMin && m.tryTail(run, authEnd, len(run))
}

// endsRun reports whether r ends a URL's run: whitespace, a control character,
// or a character no URL carries unescaped.
func endsRun(r rune) bool {
	return unicode.IsSpace(r) || r < ' ' || strings.ContainsRune("<>\"`", r)
}

// isHostByte reports whether c can appear in an authority: userinfo, a host
// name, an IPv6 literal, a port. Text punctuation that ends a bare host in a
// sentence — a comma, an apostrophe, a parenthesis — cannot.
func isHostByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		strings.IndexByte("-._~%:@[]", c) >= 0
}

// sameHost reports whether the authority of the reading r names the request's
// host, spending r's length.
func (m *urlMatcher) sameHost(r string) bool {
	m.budget -= len(r)
	u, err := url.Parse(r)
	if err != nil {
		return false
	}
	h, ok := canonicalHostPort(u)
	return ok && h == m.host
}

// sameHead reports whether r, a scheme and an authority, normalizes to the
// request's: the same scheme, userinfo, host and port.
func (m *urlMatcher) sameHead(r string) bool {
	m.budget -= len(r)
	got, ok := normalizeFetchURL(r)
	return ok && strings.TrimSuffix(got, "/") == m.head
}

// tryTail compares the reading run[:k], whose head is the request's, with the
// request. A tail of plain characters — ones Go's URL type spells back as
// written — is compared as a string, for a sixteenth of the charge; any other
// is parsed.
func (m *urlMatcher) tryTail(run string, authEnd, k int) bool {
	tail := run[authEnd:k]
	if !isPlainTail(tail) {
		return m.try(run[:k])
	}
	m.budget -= len(tail)/16 + 1
	if tail == "" || tail[0] == '?' {
		tail = "/" + tail
	}
	if tail == m.tail && m.record(run[:k]) {
		return true
	}
	return m.budget <= 0
}

// isPlainTail reports whether t, the path and query after an authority,
// normalizes to itself: a path of unreserved characters, sub-delimiters, ":",
// "@" and "/", and a query of printable ASCII, with no escape and no fragment.
func isPlainTail(t string) bool {
	q := strings.IndexByte(t, '?')
	for i := range len(t) {
		c := t[i]
		switch {
		case c <= ' ' || c >= unicode.MaxASCII || c == '%' || c == '#':
			return false
		case q >= 0 && i > q:
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9':
		case strings.IndexByte("-._~!$&'()*+,;=:@/?", c) < 0:
			return false
		}
	}
	return true
}

// try compares one reading with the request, parsing it and spending its
// length. It reports true when the search is over: this reading is spelled
// exactly as the request, or the budget is spent.
func (m *urlMatcher) try(r string) bool {
	m.budget -= len(r)
	if got, ok := normalizeFetchURL(r); ok && got == m.want && m.record(r) {
		return true
	}
	return m.budget <= 0
}

// record notes a matching reading, reporting true when it is spelled exactly as
// the request, which ends the search.
func (m *urlMatcher) record(r string) bool {
	if r == m.raw {
		m.found, m.exact = r, true
		return true
	}
	if m.found == "" {
		m.found = r
	}
	return false
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
