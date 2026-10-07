package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Tokens are the counts an upstream reported for one request, in
// Anthropic's terms: Input excludes the cached tokens, which CacheWrite and
// CacheRead count.
type Tokens struct {
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
}

// limited is what a key's TPM counts: every token but a cache read, as
// Anthropic's own limits count them (platform.claude.com/docs/en/api/rate-limits,
// "Cache-aware ITPM", read 2026-10-07).
func (t Tokens) limited() int64 { return t.Input + t.CacheWrite + t.Output }

// Usage is one request in the ledger.
type Usage struct {
	ID           int64
	RequestID    string
	CreatedAt    time.Time // when the request ended
	APIKeyID     string
	Model        string // the name the caller sent
	Alias        string // the configured alias it matched, "*" for the wildcard
	DeploymentID string // the attempt that answered, or the last one made
	CredentialID string
	SessionID    string // "" when the caller sent none
	Protocol     string // the inbound protocol
	Endpoint     string // "messages", "count_tokens"
	Status       int    // the HTTP status the caller was given
	ErrorType    string // the error the caller was given, "" when none
	Tokens       *Tokens
	Cost         *float64 // at the deployment's prices when written; nil with Tokens
	Latency      time.Duration
	TTFT         time.Duration // zero when the answer was not streamed
}

// readCommitted is the isolation the limits and the ledger name rather than
// inherit: each upserts a row other requests update too, which a stricter
// default an operator may set refuses to update once another has committed
// since the statement began, where READ COMMITTED waits and goes on
// (internal/store's BeginObjectDelete makes the same choice).
var readCommitted = pgx.TxOptions{IsoLevel: pgx.ReadCommitted}

// exec runs one statement in a transaction of its own at readCommitted.
func (s *Store) exec(ctx context.Context, sql string, args ...any) error {
	return pgx.BeginTxFunc(ctx, s.pool, readCommitted, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// RecordUsage writes u to the ledger and adds it to its day's rollup, in one
// statement, and, when the key's TPM is limited, adds its tokens to the
// current minute's window — the minute the request ended, by the database's
// clock — in another, so a row the ledger refuses still counts against the
// limit. Cost is computed here, at the prices the deployment has now, a
// missing price costing nothing and missing tokens making no cost; it is
// returned, nil when the row has none, was not written, or holds one no
// float64 can.
func (s *Store) RecordUsage(ctx context.Context, u Usage, tpmLimited bool) (*float64, error) {
	var in, out, cw, cr *int64
	var windowErr error
	if t := u.Tokens; t != nil {
		in, out, cw, cr = &t.Input, &t.Output, &t.CacheWrite, &t.CacheRead
		if tpmLimited {
			windowErr = s.exec(ctx, `
				INSERT INTO modelgateway.rate_windows AS w (api_key_id, minute, tokens)
				VALUES ($1, date_trunc('minute', now()), $2)
				ON CONFLICT (api_key_id, minute) DO UPDATE SET tokens = w.tokens + EXCLUDED.tokens`,
				u.APIKeyID, t.limited())
		}
	}
	var ttft *int64
	if u.TTFT > 0 {
		us := u.TTFT.Microseconds()
		ttft = &us
	}
	var cost *string
	ledgerErr := pgx.BeginTxFunc(ctx, s.pool, readCommitted, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
		WITH u AS (
		  INSERT INTO modelgateway.usage (request_id, api_key_id, model, alias, deployment_id, credential_id,
		      session_id, protocol, endpoint, status, error_type, input_tokens, output_tokens,
		      cache_write_tokens, cache_read_tokens, cost, latency_us, ttft_us)
		  SELECT $1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10, NULLIF($11, ''),
		         $12::bigint, $13::bigint, $14::bigint, $15::bigint,
		         (coalesce(d.price_input, 0) * $12 + coalesce(d.price_output, 0) * $13
		          + coalesce(d.price_cache_write, 0) * $14 + coalesce(d.price_cache_read, 0) * $15) / 1000000,
		         $16, $17
		  FROM (VALUES (1)) one LEFT JOIN modelgateway.deployments d ON d.id = $5
		  RETURNING created_at, api_key_id, alias, deployment_id, error_type,
		            input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost
		)
		INSERT INTO modelgateway.usage_daily AS t (day, api_key_id, alias, deployment_id, requests, errors,
		    input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost)
		SELECT (created_at AT TIME ZONE 'UTC')::date, api_key_id, alias, deployment_id, 1,
		       (error_type IS NOT NULL)::integer, coalesce(input_tokens, 0), coalesce(output_tokens, 0),
		       coalesce(cache_write_tokens, 0), coalesce(cache_read_tokens, 0), coalesce(cost, 0)
		FROM u
		ON CONFLICT (day, api_key_id, alias, deployment_id) DO UPDATE SET
		  requests = t.requests + 1, errors = t.errors + EXCLUDED.errors,
		  input_tokens = t.input_tokens + EXCLUDED.input_tokens,
		  output_tokens = t.output_tokens + EXCLUDED.output_tokens,
		  cache_write_tokens = t.cache_write_tokens + EXCLUDED.cache_write_tokens,
		  cache_read_tokens = t.cache_read_tokens + EXCLUDED.cache_read_tokens,
		  cost = t.cost + EXCLUDED.cost
		RETURNING (SELECT cost::text FROM u)`,
			u.RequestID, u.APIKeyID, u.Model, u.Alias, u.DeploymentID, u.CredentialID, u.SessionID, u.Protocol,
			u.Endpoint, u.Status, u.ErrorType, in, out, cw, cr, u.Latency.Microseconds(), ttft).Scan(&cost)
	})
	if ledgerErr != nil || cost == nil {
		return nil, errors.Join(windowErr, ledgerErr)
	}
	// The cost is read as text and parsed once the row is committed: one no
	// float64 holds loses the caller its reading, never the row.
	f, err := strconv.ParseFloat(*cost, 64)
	if err != nil {
		return nil, windowErr
	}
	return &f, windowErr
}

// UsageFilter narrows a usage read; an empty field matches every value.
type UsageFilter struct {
	APIKeyID     string
	Alias        string
	DeploymentID string
	SessionID    string // ListUsage only: the daily rollup has no sessions
}

// ListUsage reads the ledger newest first: up to limit rows older than the
// row with id before (zero: from the newest), and whether more follow. The
// query names only the filters given, so each can use its index: a plan
// written for every filter at once would serve none of them well.
func (s *Store) ListUsage(ctx context.Context, f UsageFilter, before int64, limit int) ([]Usage, bool, error) {
	where, args := []string{"true"}, []any{}
	for _, c := range []struct {
		cond string
		arg  any
		set  bool
	}{
		{"id < $%d", before, before > 0},
		{"api_key_id = $%d", f.APIKeyID, f.APIKeyID != ""},
		{"alias = $%d", f.Alias, f.Alias != ""},
		{"deployment_id = $%d", f.DeploymentID, f.DeploymentID != ""},
		{"session_id = $%d", f.SessionID, f.SessionID != ""},
	} {
		if c.set {
			args = append(args, c.arg)
			where = append(where, fmt.Sprintf(c.cond, len(args)))
		}
	}
	args = append(args, limit+1)
	rows, err := s.pool.Query(ctx, `
		SELECT id, request_id, created_at, api_key_id, model, alias, deployment_id, credential_id,
		       coalesce(session_id, ''), protocol, endpoint, status, coalesce(error_type, ''),
		       input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost::float8,
		       latency_us, coalesce(ttft_us, 0)
		FROM modelgateway.usage
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Usage
	for rows.Next() {
		var u Usage
		var in, o, cw, cr *int64
		var latency, ttft int64
		if err := rows.Scan(&u.ID, &u.RequestID, &u.CreatedAt, &u.APIKeyID, &u.Model, &u.Alias, &u.DeploymentID,
			&u.CredentialID, &u.SessionID, &u.Protocol, &u.Endpoint, &u.Status, &u.ErrorType,
			&in, &o, &cw, &cr, &u.Cost, &latency, &ttft); err != nil {
			return nil, false, err
		}
		if in != nil {
			u.Tokens = &Tokens{Input: *in, Output: deref(o), CacheWrite: deref(cw), CacheRead: deref(cr)}
		}
		u.Latency, u.TTFT = time.Duration(latency)*time.Microsecond, time.Duration(ttft)*time.Microsecond
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// DailyUsage is one day's rollup for a key, alias and deployment.
type DailyUsage struct {
	Day          time.Time // midnight UTC
	APIKeyID     string
	Alias        string
	DeploymentID string
	Requests     int64
	Errors       int64 // requests the caller was given an error for
	Tokens       Tokens
	Cost         float64
}

// ListDailyUsage reads the rollups of the UTC days from through to, both
// included, in order of day, key, alias and deployment: up to limit rows,
// and whether more match.
func (s *Store) ListDailyUsage(ctx context.Context, f UsageFilter, from, to time.Time, limit int) ([]DailyUsage, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT day, api_key_id, alias, deployment_id, requests, errors, input_tokens, output_tokens,
		       cache_write_tokens, cache_read_tokens, cost::float8
		FROM modelgateway.usage_daily
		WHERE day BETWEEN $1::date AND $2::date AND ($3 = '' OR api_key_id = $3) AND ($4 = '' OR alias = $4)
		  AND ($5 = '' OR deployment_id = $5)
		ORDER BY day, api_key_id, alias, deployment_id LIMIT $6`,
		from.UTC().Format(time.DateOnly), to.UTC().Format(time.DateOnly), f.APIKeyID, f.Alias, f.DeploymentID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []DailyUsage
	for rows.Next() {
		var d DailyUsage
		if err := rows.Scan(&d.Day, &d.APIKeyID, &d.Alias, &d.DeploymentID, &d.Requests, &d.Errors,
			&d.Tokens.Input, &d.Tokens.Output, &d.Tokens.CacheWrite, &d.Tokens.CacheRead, &d.Cost); err != nil {
			return nil, false, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// sweepLock is the advisory lock a sweep holds, so replicas take turns
// rather than racing to delete the same rows.
var sweepLock = func() int64 {
	h := fnv.New64a()
	h.Write([]byte("modelgateway.usage_sweep"))
	return int64(h.Sum64())
}()

// sweepBatch bounds the rows one transaction deletes, so a first sweep over
// a long backlog holds no lock for long.
var sweepBatch int64 = 10000

// SweepUsage deletes the ledger rows older than keep, a batch per
// transaction, and the rate windows no admission reads any more. A replica
// that finds another sweeping leaves the work to it. The daily rollups stay.
func (s *Store) SweepUsage(ctx context.Context, keep time.Duration) (int64, error) {
	var total int64
	for {
		var locked bool
		var n int64
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, sweepLock).Scan(&locked); err != nil || !locked {
				return err
			}
			tag, err := tx.Exec(ctx, `
				DELETE FROM modelgateway.usage WHERE id IN (
				  SELECT id FROM modelgateway.usage
				  WHERE created_at < now() - $1::bigint * interval '1 microsecond' LIMIT $2)`,
				keep.Microseconds(), sweepBatch)
			if err != nil {
				return err
			}
			n = tag.RowsAffected()
			if n < sweepBatch {
				_, err = tx.Exec(ctx, `DELETE FROM modelgateway.rate_windows WHERE minute < now() - interval '1 hour'`)
			}
			return err
		})
		total += n
		if err != nil || !locked || n < sweepBatch {
			return total, err
		}
	}
}

// RunRetention sweeps the ledger now and every interval after until ctx
// ends; a failed sweep is logged, and the next one picks up its work.
func (s *Store) RunRetention(ctx context.Context, every, keep time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n, err := s.SweepUsage(ctx, keep)
		switch {
		case err != nil && !errors.Is(err, context.Canceled):
			slog.ErrorContext(ctx, "modelgateway: usage sweep failed", "error", err)
		case n > 0:
			slog.InfoContext(ctx, "modelgateway: usage swept", "rows", n, "older_than", keep.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
