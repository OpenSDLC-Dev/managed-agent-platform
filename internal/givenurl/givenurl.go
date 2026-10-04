// Package givenurl is web_fetch's provenance rule (#823): which URLs a session
// was given, the form two URLs compare in, and the per-session index that
// answers which given URL a request names (#836).
//
// The tool's description, the reference's word for word, tells the model it
// "can only fetch EXACT URLs that have been provided directly by the user or
// have been returned in results from the web_search and web_fetch tools". The
// executor holds that through Source: a model a prompt injection has turned
// cannot build a URL of its own — one carrying a secret in its query or
// fragment, say — and have it fetched. The model's URL only chooses: what is
// fetched is the given URL itself, as it was written in the session, so
// nothing the model adds — a fragment, a spelling Go would re-encode into the
// same form — reaches the reader.
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
// Those readings are found once, when their event is appended, in the
// append's transaction (IndexAppended), and stored under the normalized form a
// request must have to name them (readings), so a lookup is one indexed read
// whatever the session's length. What people wrote is met first, then the
// newest results, and within a payload the order the matcher reads it in; a
// spelling exactly the request's wins over all of them.
//
// Two things are read at lookup instead. A payload too costly to index — a page
// of URLs run together, which reading every way is quadratic in — is listed on
// the session's index row and read by the matcher, which reads only the window
// a match for this request fits in and stops at its budget (readingBudget),
// refusing the fetch rather than stalling the executor. And a session the index
// lags — one older than it, or one a build without it appended to — is caught
// up first (catchUp).
package givenurl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lookupAttempts bounds how often a lookup catches the index up and finds
// it behind again: only appends from a build without the index, during a
// rolling upgrade, can do that, and each round indexes what they wrote.
const lookupAttempts = 8

// Source returns the given URL that raw, the URL web_fetch was asked for,
// names in the session sid, exactly as it was written there, or "" when none
// does or the session is gone. It sees every event committed before it began.
func Source(ctx context.Context, pool *pgxpool.Pool, sid domain.ID, raw string) (string, error) {
	m, ok := newURLMatcher(raw)
	if !ok {
		return "", nil
	}
	for range lookupAttempts {
		found, behind, err := lookup(ctx, pool, sid, m)
		if errors.Is(err, errSessionGone) {
			return "", nil
		}
		if err != nil || !behind {
			return found, err
		}
		if ok, err := catchUp(ctx, pool, sid); err != nil || !ok {
			return "", err
		}
	}
	return "", errors.New("the session's index stayed behind its events")
}

// lookup answers m from sid's index in one snapshot, or reports the index
// behind the session's events.
func lookup(ctx context.Context, pool *pgxpool.Pool, sid domain.ID, m *urlMatcher) (found string, behind bool, err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ix, err := readIndex(ctx, tx, sid)
	if err != nil || ix.through < ix.last {
		return "", err == nil, err
	}

	// The key is short, so a row is taken only when the form its spelling
	// gives is the request's.
	rs, err := tx.Query(ctx, `SELECT spelling, plain, rank, seq, ord FROM session_given_urls
		WHERE index_id = $1 AND url_key = $2`, ix.id, urlKey(m.want))
	if err != nil {
		return "", false, fmt.Errorf("read the given URLs: %w", err)
	}
	var best row
	for rs.Next() {
		var (
			r        row
			spelling []byte
		)
		if err := rs.Scan(&spelling, &r.plain, &r.rank, &r.seq, &r.ord); err != nil {
			rs.Close()
			return "", false, fmt.Errorf("read the given URLs: %w", err)
		}
		r.spelling = string(spelling)
		if want, ok := wantOf(r.spelling, r.plain); !ok || want != m.want {
			continue
		}
		if r.spelling == m.raw {
			rs.Close()
			return m.raw, false, nil
		}
		if best.spelling == "" || r.before(best) {
			best = r
		}
	}
	if err := rs.Err(); err != nil {
		return "", false, fmt.Errorf("read the given URLs: %w", err)
	}

	unread, err := unindexedPayloads(ctx, tx, sid, ix.unindexed)
	if err != nil {
		return "", false, err
	}
	// The matcher reads them in the order a lookup meets them, so the first
	// it finds is the one an index would hold first among them.
	for _, p := range unread {
		before := m.found
		var v any
		if err := json.Unmarshal(p.payload, &v); err != nil {
			return "", false, fmt.Errorf("decode a given payload: %w", err)
		}
		over := m.walk(v)
		if m.exact {
			return m.raw, false, nil
		}
		if before == "" && m.found != "" && (best.spelling == "" || p.row.before(best)) {
			best = row{spelling: m.found, rank: p.rank, seq: p.seq}
		}
		if over {
			break
		}
	}
	if best.spelling == "" && m.budget <= 0 {
		return "", false, ErrReadingBudget
	}
	return best.spelling, false, nil
}

type unindexed struct {
	row
	payload []byte
}

// unindexedPayloads reads the payloads at seqs in sid, in the order a lookup
// meets them.
func unindexedPayloads(ctx context.Context, tx pgx.Tx, sid domain.ID, seqs []int64) ([]unindexed, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	rs, err := tx.Query(ctx, `
		SELECT CASE WHEN type = $3 THEN 1 ELSE 0 END::smallint, seq,
		       CASE WHEN type = $4 THEN payload->'input' ELSE payload END
		  FROM events WHERE session_id = $1 AND seq = ANY($2)`, sid.String(), seqs,
		string(domain.EventAgentToolResult), string(domain.EventAgentToolUse))
	if err != nil {
		return nil, fmt.Errorf("read the unindexed payloads: %w", err)
	}
	out, err := pgx.CollectRows(rs, func(r pgx.CollectableRow) (unindexed, error) {
		var u unindexed
		err := r.Scan(&u.rank, &u.seq, &u.payload)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the unindexed payloads: %w", err)
	}
	slices.SortFunc(out, func(a, b unindexed) int { return a.row.compare(b.row) })
	return out, nil
}
