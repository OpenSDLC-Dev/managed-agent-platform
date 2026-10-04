package givenurl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScanSource is the lookup as it was before the index (#836), the executor's
// webFetchSource at #823, kept word for word so the index can be held to it:
// it reads every payload that gives URLs in the session, narrowed in SQL to
// the ones that name the request's host, and walks each with the matcher,
// what people wrote first and then the newest results. withFilter false drops
// the narrowing, which TestTheScanFilterMissedAnEscapedHost shows dropped a
// payload it should have kept.
func ScanSource(ctx context.Context, pool *pgxpool.Pool, sid domain.ID, raw string, withFilter bool) (string, error) {
	m, ok := newURLMatcher(raw)
	if !ok {
		return "", nil
	}
	key := ""
	if u, err := url.Parse(m.raw); err == nil && withFilter {
		if h := u.Hostname(); isPlainASCIIHost(h) && !strings.Contains(strings.ToLower(h), "xn--") {
			key = h
		}
	}
	const narrow = `($2 = '' OR strpos(lower(%[1]s::text), lower($2)) > 0
		       OR octet_length(%[1]s::text) <> char_length(%[1]s::text))`
	rows, err := pool.Query(ctx, `
		SELECT payload FROM (
		SELECT m.payload, 0 AS rank, m.seq FROM events m
		 WHERE m.session_id = $1 AND m.type = ANY($3)
		   AND `+fmt.Sprintf(narrow, "m.payload")+`
		UNION ALL
		SELECT u.payload->'input', 0, u.seq FROM events u
		 WHERE u.session_id = $1 AND u.type = $5 AND u.payload->>'name' = 'web_fetch'
		   AND EXISTS (SELECT 1 FROM events c
		                WHERE c.session_id = u.session_id AND c.type = $6
		                  AND c.payload->>'tool_use_id' = u.id AND c.payload->>'result' = 'allow')
		UNION ALL
		SELECT r.payload, 1, r.seq FROM events r
		  JOIN events u ON u.session_id = r.session_id AND u.id = r.payload->>'tool_use_id'
		 WHERE r.session_id = $1 AND r.type = $4
		   AND u.type = $5 AND u.payload->>'name' IN ('web_search', 'web_fetch')
		   AND NOT COALESCE((r.payload->>'is_error')::boolean, false)
		   AND `+fmt.Sprintf(narrow, "r.payload")+`
		) given ORDER BY rank, seq DESC`,
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
		return "", ErrReadingBudget
	}
	return m.found, nil
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

// Readings exposes readings to the external tests: every reading the decoded
// JSON value v gives, and whether it fits the index's budget.
func Readings(v any) (spellings, wants []string, ok bool) {
	rs, ok := readings(v)
	for _, r := range rs {
		spellings, wants = append(spellings, r.spelling), append(wants, r.want)
	}
	return spellings, wants, ok
}

// Normalize exposes normalizeFetchURL to the external tests.
var Normalize = normalizeFetchURL

// URLKey exposes the index's key for a normalized URL.
var URLKey = urlKey

// CatchUpChunk is how many seqs a catch-up transaction indexes.
const CatchUpChunk = catchUpChunk
