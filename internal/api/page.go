package api

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// The managed-agents lists paginate with an opaque cursor: the response
// carries {"data":[…],"next_page":<cursor|null>} (sessions additionally carry
// "prev_page"), and clients pass the cursor back as ?page=…. Cursors are
// keyset positions — (created_at, id) for resource lists, the version number
// for agent-version lists — so concurrent inserts and deletes never duplicate
// or skip rows the way row offsets would.

const (
	defaultLimit = 20
	maxLimit     = 100
	// The session-events list accepts limit up to 1000, not maxLimit: the
	// reference worker's SessionToolRunner reconciles a session by listing its
	// events with limit=1000 (anthropic-sdk-go betasessiontoolrunner.go), which a
	// 100 cap 400s before it can run a tool. 1000 is the value the worker requests
	// and the reference's general list convention (documented "1 to 1000" on most
	// SDK list params); the event-list param itself documents no explicit maximum,
	// so this is our compatible upper bound (some cap is needed — an unbounded
	// limit is a query-cost risk), not a proven reference cap.
	maxEventLimit = 1000
)

// cursor directions: fetch rows after the position (next) or before it (prev).
const (
	dirNext = "n"
	dirPrev = "p"
)

type cursor struct {
	dir string
	// time-keyed position (agents, environments, sessions)
	t  time.Time
	id string
	// version-keyed position (agent versions); used when versioned is true
	versioned bool
	version   int64
	// seq-keyed position (session events); binds the sort direction so a
	// follow-up request that omits ?order= cannot flip the walk around.
	seqKeyed bool
	seqDesc  bool
	seq      int64
}

type pageParams struct {
	limit int
	cur   *cursor
}

func parsePage(q url.Values) (pageParams, error) {
	return parsePageMax(q, maxLimit)
}

// parsePageMax is parsePage with an explicit maximum limit, for the one list
// (session events) whose reference cap differs from maxLimit.
func parsePageMax(q url.Values, max int) (pageParams, error) {
	p := pageParams{limit: defaultLimit}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > max {
			return p, errInvalid("limit must be an integer between 1 and %d", max)
		}
		p.limit = n
	}
	if s := q.Get("page"); s != "" {
		c, err := decodeCursor(s)
		if err != nil {
			return p, err
		}
		// A time cursor embeds a resource id; a crafted one carrying an unstorable
		// byte would otherwise bind into the keyset comparison as a 500. A
		// server-issued cursor's id is always valid, and the seq/version cursors
		// carry no id (c.id == ""). See #135. The storableText fallback exists for
		// the skills list: the imported anthropic catalog's ids are short names
		// ("xlsx"), not prefixed ids, and #135's actual invariant is that no
		// unstorable byte reaches a bind parameter.
		if c.id != "" && !domain.ID(c.id).Valid() && !storableText(c.id) {
			return p, errInvalid("invalid page cursor")
		}
		p.cur = c
	}
	return p, nil
}

func encodeTimeCursor(dir string, t time.Time, id string) string {
	raw := fmt.Sprintf("k1|%s|t|%d|%s", dir, t.UnixNano(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func encodeVersionCursor(version int64) string {
	raw := fmt.Sprintf("k1|%s|v|%d", dirNext, version)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func encodeSeqCursor(desc bool, seq int64) string {
	order := "a"
	if desc {
		order = "d"
	}
	raw := fmt.Sprintf("k1|%s|s|%s|%d", dirNext, order, seq)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (*cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errInvalid("invalid page cursor")
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) < 4 || parts[0] != "k1" || (parts[1] != dirNext && parts[1] != dirPrev) {
		return nil, errInvalid("invalid page cursor")
	}
	switch parts[2] {
	case "t":
		if len(parts) != 5 {
			return nil, errInvalid("invalid page cursor")
		}
		nanos, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || parts[4] == "" {
			return nil, errInvalid("invalid page cursor")
		}
		return &cursor{dir: parts[1], t: time.Unix(0, nanos).UTC(), id: parts[4]}, nil
	case "v":
		if len(parts) != 4 {
			return nil, errInvalid("invalid page cursor")
		}
		version, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || version < 1 {
			return nil, errInvalid("invalid page cursor")
		}
		return &cursor{dir: parts[1], versioned: true, version: version}, nil
	case "s":
		if len(parts) != 5 || (parts[3] != "a" && parts[3] != "d") {
			return nil, errInvalid("invalid page cursor")
		}
		seq, err := strconv.ParseInt(parts[4], 10, 64)
		if err != nil || seq < 1 {
			return nil, errInvalid("invalid page cursor")
		}
		return &cursor{dir: parts[1], seqKeyed: true, seqDesc: parts[3] == "d", seq: seq}, nil
	default:
		return nil, errInvalid("invalid page cursor")
	}
}

// keysetClause returns the SQL comparison and ORDER BY direction for a
// time-keyed page fetch, given the list's sort direction ("ASC"/"DESC") and
// the cursor. fetchReversed reports that rows come back opposite to the sort
// order and must be reversed before rendering (prev-page fetches).
//
// The returned clause references two placeholders for (created_at, id) that
// the caller appends to its args as argOffset+1 and argOffset+2.
func keysetClause(sortDir string, cur *cursor, argOffset int) (clause, orderDir string, fetchReversed bool) {
	if cur == nil {
		return "", sortDir, false
	}
	forward := cur.dir == dirNext
	// Moving forward through a DESC list means strictly-smaller keys;
	// backward means strictly-greater. ASC flips both.
	cmp := "<"
	if (sortDir == "ASC") == forward {
		cmp = ">"
	}
	orderDir = sortDir
	if !forward {
		if sortDir == "ASC" {
			orderDir = "DESC"
		} else {
			orderDir = "ASC"
		}
	}
	clause = fmt.Sprintf(" AND (created_at, id) %s ($%d, $%d)", cmp, argOffset+1, argOffset+2)
	return clause, orderDir, !forward
}

// pageEdges computes next_page/prev_page for a time-keyed page.
//
//	rows       — the page rows in render order (already un-reversed)
//	more       — whether the fetch found a row beyond the page in fetch direction
//	hadCursor  — whether the request carried a cursor
//	reversed   — whether this was a prev-page fetch
//
// key returns the keyset position of a rendered row.
func pageEdges(n int, more, hadCursor, reversed bool, key func(i int) (time.Time, string)) (next, prev *string) {
	if n == 0 {
		return nil, nil
	}
	lastT, lastID := key(n - 1)
	firstT, firstID := key(0)
	if reversed {
		// Walking backwards: something newer (the page we came from) always
		// exists after this page; more-before is what the probe row answered.
		c := encodeTimeCursor(dirNext, lastT, lastID)
		next = &c
		if more {
			p := encodeTimeCursor(dirPrev, firstT, firstID)
			prev = &p
		}
		return next, prev
	}
	if more {
		c := encodeTimeCursor(dirNext, lastT, lastID)
		next = &c
	}
	if hadCursor {
		p := encodeTimeCursor(dirPrev, firstT, firstID)
		prev = &p
	}
	return next, prev
}

// parseBoolParam parses an optional boolean query parameter.
func parseBoolParam(q url.Values, key string) (bool, error) {
	s := q.Get(key)
	if s == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, errInvalid("%s must be true or false", key)
	}
	return v, nil
}

// parseTimeParam parses an optional RFC3339 query parameter such as
// created_at[gte].
func parseTimeParam(q url.Values, key string) (*time.Time, error) {
	s := q.Get(key)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, errInvalid("%s must be an RFC 3339 timestamp", key)
	}
	return &t, nil
}

// pageJSON is the unidirectional list envelope (agents, environments,
// agent versions).
type pageJSON struct {
	Data     []any   `json:"data"`
	NextPage *string `json:"next_page"`
}

// biPageJSON is the bidirectional list envelope (sessions).
type biPageJSON struct {
	Data     []any   `json:"data"`
	NextPage *string `json:"next_page"`
	PrevPage *string `json:"prev_page"`
}

// filePageJSON is the classic Files API list envelope — {data, has_more,
// first_id, last_id} (anthropic-sdk-go packages/pagination Page[T]). The Files
// API predates the managed-agents next_page cursor convention and paginates by
// bare object id (after_id / before_id) instead of an opaque keyset cursor, so
// it gets its own envelope alongside pageJSON. first_id/last_id are the first
// and last ids of the returned page (nullable — null on an empty page).
type filePageJSON struct {
	Data    []any   `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
}
