package givenurl

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is what indexing needs of a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// personTypes are the events a person writes into a session; every string in
// one is given.
var personTypes = []string{string(domain.EventUserMessage), string(domain.EventUserDefineOutcome),
	string(domain.EventUserToolConfirm), string(domain.EventSystemMessage)}

// mayGive reports whether an event of type t can give a URL: a person's (a
// confirmation among them, which can allow a web_fetch call), or a web result.
func mayGive(t domain.EventType) bool {
	return t == domain.EventAgentToolResult || slices.Contains(personTypes, string(t))
}

// givenPayloads reads the payloads the events of sid at seq in (from, to] give
// URLs in, each with the place a lookup meets it in: rank 0 for what a person
// wrote, 1 for a web result, and the seq of the event it is. A confirmation
// allowing a web_fetch call gives that call's input, at the call's seq. An
// answer names a call made before it — the brain appends a call before any
// executor can answer it, and ValidateToolConfirmations refuses a confirmation
// naming no call — so the call is found wherever it lies.
const givenPayloads = `
	SELECT 0::smallint, m.seq, m.payload FROM events m
	 WHERE m.session_id = $1 AND m.seq > $2 AND m.seq <= $3 AND m.type = ANY($4)
	UNION ALL
	SELECT 0, u.seq, u.payload->'input' FROM events c
	  JOIN events u ON u.session_id = c.session_id AND u.id = c.payload->>'tool_use_id'
	 WHERE c.session_id = $1 AND c.seq > $2 AND c.seq <= $3 AND c.type = $5
	   AND c.payload->>'result' = 'allow'
	   AND u.type = $6 AND u.payload->>'name' = 'web_fetch'
	UNION ALL
	SELECT 1, r.seq, r.payload FROM events r
	  JOIN events u ON u.session_id = r.session_id AND u.id = r.payload->>'tool_use_id'
	 WHERE r.session_id = $1 AND r.seq > $2 AND r.seq <= $3 AND r.type = $7
	   AND u.type = $6 AND u.payload->>'name' IN ('web_search', 'web_fetch')
	   AND NOT COALESCE((r.payload->>'is_error')::boolean, false)`

// row is one reading as the index stores it.
type row struct {
	urlKey   int64
	spellKey [16]byte
	plain    bool
	spelling string
	rank     int16
	seq      int64
	ord      int32
}

// compare orders rows as a lookup meets them: what a person wrote first,
// then the newest, then the payload's own order.
func (r row) compare(s row) int {
	return cmp.Or(cmp.Compare(r.rank, s.rank), cmp.Compare(s.seq, r.seq), cmp.Compare(r.ord, s.ord))
}

func (r row) before(s row) bool { return r.compare(s) < 0 }

func urlKey(want string) int64 {
	k := sha256.Sum256([]byte(want))
	return int64(binary.BigEndian.Uint64(k[:8]))
}

func spellKey(spelling string) (k [16]byte) {
	h := sha256.Sum256([]byte(spelling))
	copy(k[:], h[:])
	return k
}

// indexRange reads the readings the events of sid at seq in (from, to] give,
// and the seqs of the payloads too costly to index.
func indexRange(ctx context.Context, q Querier, sid domain.ID, from, to int64) ([]row, []int64, error) {
	rs, err := q.Query(ctx, givenPayloads, sid.String(), from, to, personTypes,
		string(domain.EventUserToolConfirm), string(domain.EventAgentToolUse), string(domain.EventAgentToolResult))
	if err != nil {
		return nil, nil, fmt.Errorf("read the given payloads: %w", err)
	}
	defer rs.Close()
	var (
		rows      []row
		unindexed []int64
	)
	for rs.Next() {
		var (
			rank    int16
			seq     int64
			payload []byte
		)
		if err := rs.Scan(&rank, &seq, &payload); err != nil {
			return nil, nil, fmt.Errorf("read the given payloads: %w", err)
		}
		if payload == nil {
			continue // a call with no input gives nothing
		}
		var v any
		if err := json.Unmarshal(payload, &v); err != nil {
			return nil, nil, fmt.Errorf("decode a given payload: %w", err)
		}
		found, ok := readings(v)
		if !ok {
			unindexed = append(unindexed, seq)
			continue
		}
		for i, r := range found {
			rows = append(rows, row{urlKey(r.want), spellKey(r.spelling), r.plain, r.spelling, rank, seq, int32(i)})
		}
	}
	return rows, unindexed, rs.Err()
}

// store writes rows into the index id and lists unindexed on it. A spelling
// given again keeps the place a lookup meets it first.
func store(ctx context.Context, q Querier, id int64, rows []row, unindexed []int64) error {
	type key struct {
		url   int64
		spell [16]byte
	}
	best := map[key]row{}
	for _, r := range rows {
		k := key{r.urlKey, r.spellKey}
		if b, ok := best[k]; !ok || r.before(b) {
			best[k] = r
		}
	}
	keys := slices.SortedFunc(maps.Keys(best), func(a, b key) int {
		return cmp.Or(cmp.Compare(a.url, b.url), slices.Compare(a.spell[:], b.spell[:]))
	})
	const batch = 4096
	for len(keys) > 0 {
		n := min(batch, len(keys))
		var (
			urlKeys, seqs     []int64
			ords              []int32
			ranks             []int16
			plains            []bool
			spellKeys, spells [][]byte
		)
		for _, k := range keys[:n] {
			r := best[k]
			urlKeys, seqs, ords, ranks, plains = append(urlKeys, r.urlKey), append(seqs, r.seq), append(ords, r.ord), append(ranks, r.rank), append(plains, r.plain)
			spellKeys, spells = append(spellKeys, r.spellKey[:]), append(spells, []byte(r.spelling))
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO session_given_urls AS g (index_id, url_key, seq, ord, rank, plain, spell_key, spelling)
			SELECT $1, u.*
			  FROM unnest($2::bigint[], $3::bigint[], $4::int[], $5::smallint[], $6::boolean[], $7::bytea[], $8::bytea[]) AS u
			ON CONFLICT (index_id, url_key, spell_key) DO UPDATE
			   SET rank = excluded.rank, seq = excluded.seq, ord = excluded.ord, plain = excluded.plain
			 WHERE (excluded.rank, -excluded.seq, excluded.ord) < (g.rank, -g.seq, g.ord)`,
			id, urlKeys, seqs, ords, ranks, plains, spellKeys, spells); err != nil {
			return fmt.Errorf("index the given URLs: %w", err)
		}
		keys = keys[n:]
	}
	if len(unindexed) > 0 {
		if _, err := q.Exec(ctx, `UPDATE session_given_url_indexes SET unindexed = unindexed || $2::bigint[]
			WHERE id = $1`, id, unindexed); err != nil {
			return fmt.Errorf("list the unindexed payloads: %w", err)
		}
	}
	return nil
}

// IndexAppended indexes the events an append just wrote to sid at seq in
// (prev, last], in the append's transaction, which holds the session row
// lock: a lookup sees their URLs exactly when it sees them. types are their
// types. It indexes only an index complete through prev, and moves it to
// last; an index that lags — a session older than it, or one a build without
// it appended to — is left to the next lookup, which catches it up (catchUp).
func IndexAppended(ctx context.Context, tx Querier, sid domain.ID, prev, last int64, types []domain.EventType) error {
	var id int64
	err := tx.QueryRow(ctx, `UPDATE session_given_url_indexes SET indexed_through = $3
		WHERE session_id = $1 AND indexed_through = $2 RETURNING id`, sid.String(), prev, last).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if prev != 0 {
			return nil
		}
		// The session's first events: its index starts with them.
		err = tx.QueryRow(ctx, `INSERT INTO session_given_url_indexes (session_id, indexed_through)
			VALUES ($1, $2) RETURNING id`, sid.String(), last).Scan(&id)
	}
	if err != nil {
		return fmt.Errorf("move the given-URL index: %w", err)
	}
	if !slices.ContainsFunc(types, mayGive) {
		return nil
	}
	rows, unindexed, err := indexRange(ctx, tx, sid, prev, last)
	if err != nil {
		return err
	}
	return store(ctx, tx, id, rows, unindexed)
}

// catchUpChunk is how many seqs one catch-up transaction indexes.
const catchUpChunk = 64

// errSessionGone is a session deleted while a lookup read it.
var errSessionGone = errors.New("session deleted")

// catchUp indexes the events of sid its index does not yet cover, a chunk per
// transaction, until it covers every event. A session older than the index
// has no index row and is read from its first event. Each chunk is read and
// indexed before the session row is locked, and written under the lock, as an
// append writes, once the index is seen not to have moved. It reports false
// when sid does not exist.
func catchUp(ctx context.Context, pool *pgxpool.Pool, sid domain.ID) (bool, error) {
	for {
		done, err := catchUpOnce(ctx, pool, sid)
		if errors.Is(err, errSessionGone) {
			return false, nil
		}
		if err != nil || done {
			return true, err
		}
	}
}

// catchUpOnce indexes one chunk. Its transaction is READ COMMITTED whatever
// the database's default, so the index row read again under the lock is the
// latest one.
func catchUpOnce(ctx context.Context, pool *pgxpool.Pool, sid domain.ID) (bool, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ix, err := readIndex(ctx, tx, sid)
	if err != nil || ix.through >= ix.last {
		return true, err
	}
	to := min(ix.last, ix.through+catchUpChunk)
	rows, unindexed, err := indexRange(ctx, tx, sid, ix.through, to)
	if err != nil {
		return false, err
	}
	if err := tx.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()).Scan(new(int)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, errSessionGone
		}
		return false, err
	}
	if now, err := readIndex(ctx, tx, sid); err != nil || now.through != ix.through {
		return false, err // another lookup moved it first: read again
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO session_given_url_indexes (session_id, indexed_through) VALUES ($1, $2)
		ON CONFLICT (session_id) DO UPDATE SET indexed_through = excluded.indexed_through
		RETURNING id`, sid.String(), to).Scan(&id); err != nil {
		return false, fmt.Errorf("move the given-URL index: %w", err)
	}
	if err := store(ctx, tx, id, rows, unindexed); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

// index is a session's index row as a transaction reads it, with the seq of
// the session's last event.
type index struct {
	id, through, last int64
	unindexed         []int64
}

// readIndex reads sid's index row. A session without one reads as id 0,
// indexed through 0. It returns errSessionGone when sid does not exist.
func readIndex(ctx context.Context, tx pgx.Tx, sid domain.ID) (index, error) {
	var (
		ix     index
		exists bool
	)
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1),
		COALESCE(i.id, 0), COALESCE(i.indexed_through, 0), COALESCE(i.unindexed, '{}'),
		COALESCE((SELECT max(seq) FROM events WHERE session_id = $1), 0)
		FROM (SELECT 1) one LEFT JOIN session_given_url_indexes i ON i.session_id = $1`,
		sid.String()).Scan(&exists, &ix.id, &ix.through, &ix.unindexed, &ix.last)
	if err == nil && !exists {
		err = errSessionGone
	}
	return ix, err
}
