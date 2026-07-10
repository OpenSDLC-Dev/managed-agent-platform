package api

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The managed-agents lists paginate with an opaque cursor: the response
// carries {"data":[…],"next_page":<cursor|null>} (sessions additionally carry
// "prev_page"), and clients pass the cursor back as ?page=…. Our cursor
// encodes a row offset; clients must treat it as opaque.

const (
	defaultLimit  = 20
	maxLimit      = 100
	cursorVersion = "o1."
)

type pageParams struct {
	limit  int
	offset int
}

func parsePage(q url.Values) (pageParams, error) {
	p := pageParams{limit: defaultLimit}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxLimit {
			return p, errInvalid("limit must be an integer between 1 and %d", maxLimit)
		}
		p.limit = n
	}
	if s := q.Get("page"); s != "" {
		off, err := decodeCursor(s)
		if err != nil {
			return p, err
		}
		p.offset = off
	}
	return p, nil
}

func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorVersion + strconv.Itoa(offset)))
}

func decodeCursor(s string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), cursorVersion) {
		return 0, errInvalid("invalid page cursor")
	}
	off, err := strconv.Atoi(strings.TrimPrefix(string(raw), cursorVersion))
	if err != nil || off < 0 {
		return 0, errInvalid("invalid page cursor")
	}
	return off, nil
}

// nextCursor returns the next_page value given that the query fetched
// limit+1 rows to probe for more.
func (p pageParams) nextCursor(fetched int) *string {
	if fetched <= p.limit {
		return nil
	}
	c := encodeCursor(p.offset + p.limit)
	return &c
}

// prevCursor returns the prev_page value (bidirectional lists only).
func (p pageParams) prevCursor() *string {
	if p.offset == 0 {
		return nil
	}
	off := p.offset - p.limit
	if off < 0 {
		off = 0
	}
	c := encodeCursor(off)
	return &c
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

func newPage(p pageParams, fetched int, data []any) pageJSON {
	if data == nil {
		data = []any{}
	}
	return pageJSON{Data: data, NextPage: p.nextCursor(fetched)}
}

func newBiPage(p pageParams, fetched int, data []any) biPageJSON {
	if data == nil {
		data = []any{}
	}
	return biPageJSON{Data: data, NextPage: p.nextCursor(fetched), PrevPage: p.prevCursor()}
}
