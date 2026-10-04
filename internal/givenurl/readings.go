package givenurl

import (
	"maps"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// indexBudget is the most indexing one payload spends, counted as the matcher
// counts (readingBudget): a parse, or a key built and hashed, costs its
// length, and a scan an eighth of what it scans. maxPayloadReadings is the most
// distinct readings one payload keeps, which bounds the rows its append writes
// under the session lock. A payload past either is left unindexed and read by
// the matcher at lookup instead (Source). Text that gets there is a page of
// URLs run together, a URL with a long paragraph after it and no space
// between, as CJK text writes one — every character after it is a reading —
// or a page of several thousand links.
const (
	indexBudget        = 4 << 20
	maxPayloadReadings = 8192
)

// reading is one URL a payload gives: the spelling, as written, and the form
// a request must normalize to for the matcher to accept that spelling there.
// plain says which way the matcher compares it: as tryTail compares a plain
// tail, or normalized (wantOf).
type reading struct {
	want, spelling string
	plain          bool
}

// wantOf recomputes, from a reading's spelling alone, the form it is indexed
// under. A plain reading's head is its spelling up to its first "/", "?" or
// "#", which is the authority the tail loop read it from.
func wantOf(spelling string, plain bool) (string, bool) {
	if !plain {
		return normalizeFetchURL(spelling)
	}
	start := strings.Index(spelling, "://") + len("://")
	a := len(spelling)
	if i := strings.IndexAny(spelling[start:], "/?#"); i >= 0 {
		a = start + i
	}
	head, ok := normalizeFetchURL(spelling[:a])
	if !ok {
		return "", false
	}
	tail := spelling[a:]
	if tail == "" || tail[0] == '?' {
		tail = "/" + tail
	}
	return strings.TrimSuffix(head, "/") + tail, true
}

// readings returns every reading a urlMatcher would accept in the decoded JSON
// value v, for every request, in the order the matcher meets them, or false
// when v is past indexBudget or maxPayloadReadings.
//
// Which readings a matcher accepts depends on the request: its windows
// (windowsOf) bound how far an occurrence is read, its host which authority
// readings are tried. But a matcher accepts a reading only for the request
// whose normalized form is the reading's own, so each candidate reading is
// judged against the windows and host that form gives — the request it could
// match — and kept if that matcher would accept it. The candidates are every
// reading any window could reach: an occurrence's authority read to each of
// its ends (urlMatcher.occurrence), and its run cut before each character
// that can end a URL in text, inside a character where a window would cut the
// run there, and whole. The judgment mirrors urlMatcher.occurrence step by
// step, and TestReadingsAreWhatTheMatcherAccepts holds the two together.
func readings(v any) ([]reading, bool) {
	x := indexer{budget: indexBudget, seen: map[[2]string]bool{}}
	x.walk(v)
	return x.out, !x.spent()
}

type indexer struct {
	budget int
	out    []reading
	seen   map[[2]string]bool
}

// add keeps r unless the payload gave it already, where it was met first.
func (x *indexer) add(r reading) {
	if k := [2]string{r.want, r.spelling}; !x.seen[k] {
		x.seen[k] = true
		x.out = append(x.out, r)
	}
}

func (x *indexer) spent() bool { return x.budget <= 0 || len(x.out) > maxPayloadReadings }

// walk reads every string in v, an object's members in key order, as
// urlMatcher.walk does.
func (x *indexer) walk(v any) {
	switch v := v.(type) {
	case string:
		x.text(v)
	case []any:
		for _, e := range v {
			x.walk(e)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			x.walk(v[k])
		}
	}
}

// text reads every occurrence in s, as urlMatcher.scan does. The positions an
// occurrence needs — where its run ends, its first "/", "?" or "#", its first
// non-ASCII byte, its first ASCII byte no host carries — are searched for
// from where the last occurrence's search stopped, so a run of URLs costs one
// pass, not one per occurrence.
func (x *indexer) text(s string) {
	x.budget -= len(s)/8 + 1
	var c cursors
	for i := 0; i < len(s) && !x.spent(); i++ {
		if (s[i] == 'h' || s[i] == 'H') && (hasPrefixFold(s[i:], "http://") || hasPrefixFold(s[i:], "https://")) {
			x.occurrence(s, i, &c)
		}
	}
}

// cursors hold, for each search an occurrence needs, the first match at or
// after the position it last started from. Occurrences start at increasing
// positions, and so does each search, so a cursor that has not been passed
// still answers: nothing between its start and its match matched.
type cursors struct {
	run, slash, wide, punct cursor
}

type cursor struct{ from, at, size int }

func (c *cursor) find(s string, from int, match func(s string, i int) int) (int, int) {
	if c.size == 0 || from < c.from || from > c.at {
		c.from, c.at, c.size = from, len(s), -1
		for i := from; i < len(s); {
			if n := match(s, i); n > 0 {
				c.at, c.size = i, n
				break
			}
			_, n := utf8.DecodeRuneInString(s[i:])
			i += n
		}
	}
	return c.at, c.size
}

// byteMatch is a cursor search for a byte: the matched byte's length, or 0.
func byteMatch(f func(byte) bool) func(string, int) int {
	return func(s string, i int) int {
		if f(s[i]) {
			return 1
		}
		return 0
	}
}

var (
	endsRunAt = func(s string, i int) int {
		if r, n := utf8.DecodeRuneInString(s[i:]); endsRun(r) {
			return n
		}
		return 0
	}
	slashAt = byteMatch(func(b byte) bool { return b == '/' || b == '?' || b == '#' })
	wideAt  = byteMatch(func(b byte) bool { return b >= utf8.RuneSelf })
	punctAt = byteMatch(func(b byte) bool { return b < utf8.RuneSelf && !isHostByte(b) })
)

// occ is one occurrence: t is the text from its "http", and every position is
// an offset into t.
type occ struct {
	t     string
	start int // just past "://"
	rn    int // where the run ends unwindowed: its first character endsRun says so, or the text's end
	rsz   int // rn plus that character's length: a window cutting inside it does not see it
	a     int // the first "/", "?" or "#" after start; len(t)+1 when none
	ae    int // the authority's end for every window that reads one (a, or the run's end)
	hash  int // the first "#" from ae, where a fragment starts; len(t)+1 when none
}

// window is urlMatcher.occurrence's reading of the occurrence for the request
// whose normalized form is want: the run (its end r, and whether it ended
// unwindowed), the authority's area, the authority's end, and the tail's
// least length.
type window struct {
	r, areaEnd, authEnd, tailMin int
	whole                        bool
}

func (o *occ) window(want string) window {
	ws := windowsOf(want)
	limit := o.start + ws.authMax + ws.tailMax
	w := window{tailMin: ws.tailMin, whole: o.rsz <= limit, r: o.rn}
	if !w.whole {
		w.r = limit
	}
	w.areaEnd = min(w.r, o.start+ws.authMax)
	w.authEnd = -1
	if o.a < w.areaEnd {
		w.authEnd = o.a
	} else if w.r <= o.start+ws.authMax && w.whole {
		w.authEnd = w.r
	}
	return w
}

func (x *indexer) occurrence(s string, p int, c *cursors) {
	t := s[p:]
	o := occ{t: t, start: strings.Index(t, "://") + len("://"), rn: len(t), rsz: len(t), a: len(t) + 1}
	if i, n := c.run.find(s, p, endsRunAt); i < len(s) {
		o.rn, o.rsz = i-p, i-p+n
	}
	if i, _ := c.slash.find(s, p+o.start, slashAt); i < len(s) {
		o.a = i - p
	}
	o.ae = min(o.a, o.rn)
	o.hash = len(t) + 1
	if i := strings.IndexByte(t[o.ae:o.rsz], '#'); i >= 0 {
		o.hash = o.ae + i
	}
	x.budget -= o.rn/8 + 1

	// The ends loop: the authority's end, its first non-ASCII byte and its
	// first ASCII byte no host carries, each with trailing dots and colons
	// trimmed first. An end a window could not reach is skipped before
	// anything is parsed: every area ends before rsz.
	ends := []int{o.ae}
	for _, f := range []struct {
		c     *cursor
		match func(string, int) int
	}{{&c.wide, wideAt}, {&c.punct, punctAt}} {
		if i, _ := f.c.find(s, p+o.start, f.match); i-p < o.rsz {
			ends = append(ends, i-p)
		} else {
			ends = append(ends, -1)
		}
	}
	for slot, e := range ends {
		if e < 0 {
			continue
		}
		k := e
		for n := 0; k > o.start && n < 16 && (t[k-1] == '.' || t[k-1] == ':'); n++ {
			k--
		}
		// The authority's end is the tail's to read, so in its own slot only
		// a trimmed reading can be the matcher's.
		if slot > 0 || k != e {
			x.authority(&o, slot, e, k, k)
		}
		if slot > 0 && k != e {
			x.authority(&o, slot, e, k, e)
		}
	}

	// The tail: only for an authority that reads to its own end, which every
	// request whose head is this occurrence's has.
	x.budget -= o.ae
	head, ok := normalizeFetchURL(t[:o.ae])
	if !ok || x.spent() {
		return
	}
	head = strings.TrimSuffix(head, "/")
	for q := o.ae; q < o.rsz && !x.spent(); {
		r, n := utf8.DecodeRuneInString(t[q:])
		// A cut before the character at q: one the run holds, or the run's
		// last character itself when a window cuts the run inside it.
		if q < o.rn && endsURLInText(r) || q == o.rn && n > 1 {
			x.tail(&o, head, q, q, n)
		}
		// A cut inside it, where a window ends the run there.
		for j := q + 1; j < q+n && x.mayCutInside(&o, j); j++ {
			x.tail(&o, head, j, q, n)
		}
		q += n
	}
	if !x.spent() {
		x.tail(&o, head, o.rn, -1, 0)
	}
}

// authority judges the reading t[:c] for the ends loop's slot holding e,
// whose trimmed form is k: the matcher tries k first and e only when k does
// not name its host, and neither when it is the authority's end, which the
// tail reads.
func (x *indexer) authority(o *occ, slot, e, k, c int) {
	if c <= o.start || x.spent() {
		return
	}
	r := o.t[:c]
	x.budget -= len(r)
	want, ok := normalizeFetchURL(r)
	if !ok {
		return
	}
	w := o.window(want)
	if slot == 0 && w.authEnd != e || slot > 0 && e >= w.areaEnd {
		return // not one of this window's ends
	}
	host := matcherHost(r)
	sameHost := func(c int) bool {
		x.budget -= c
		u, err := url.Parse(o.t[:c])
		if err != nil {
			return false
		}
		h, ok := canonicalHostPort(u)
		return ok && h == host
	}
	triesK := k != w.authEnd && k > o.start && sameHost(k)
	if c == k && triesK || c != k && !triesK && e != w.authEnd && sameHost(e) {
		x.add(reading{want, r, false})
	}
}

// matcherHost is the host a matcher compares an authority with, for a request
// spelled r: newURLMatcher's, the canonical host of r's normalized URL. Any
// request that normalizes to r's normalized form names that host, since the
// form spells it.
func matcherHost(r string) string {
	u, err := url.Parse(r)
	if err != nil {
		return ""
	}
	h, ok := canonicalHostPort(u)
	if !ok {
		return ""
	}
	h, _ = canonicalHostPort(&url.URL{Host: h})
	return h
}

// mayCutInside reports whether some window could end the run at j, inside a
// character: one whose limit lands within three bytes past j. A limit is three
// times the want's length past its scheme, plus 128, and a want is at least a
// one-byte host, a third of the path and the whole query (a path's byte
// escapes to at most three, an escape unescapes to one, and a query is kept as
// written), so the limit outruns j unless the authority and the fragment,
// which the want drops, are long. A loose bound: it only spares the parse.
func (x *indexer) mayCutInside(o *occ, j int) bool {
	return (o.ae-o.start)+max(0, j-o.hash) >= 120
}

// tail judges the reading t[:j] the tail loop reads (q is the start of the
// character j cuts before or inside, n its length; q < 0 for the whole run).
// A plain tail is compared as tryTail compares it, with the head the
// occurrence's authority normalizes to; any other is parsed.
func (x *indexer) tail(o *occ, head string, j, q, n int) {
	r, tail := o.t[:j], o.t[o.ae:j]
	x.budget -= len(r)
	var want string
	plain := isPlainTail(tail)
	if plain {
		if tail == "" || tail[0] == '?' {
			tail = "/" + tail
		}
		want = head + tail
	} else {
		var ok bool
		if want, ok = normalizeFetchURL(r); !ok {
			return
		}
	}
	w := o.window(want)
	if w.authEnd != o.ae || want[:windowsOf(want).head] != head || j-o.ae < w.tailMin {
		return
	}
	switch {
	case q < 0: // the whole run
		if !w.whole {
			return
		}
	case j == q: // before a character the run holds whole, or cut inside
		if j >= w.r {
			return
		}
	default: // inside a character, where the window's limit cuts it
		if w.whole || j >= w.r || w.r >= q+n {
			return
		}
	}
	x.add(reading{want, r, plain})
}
