package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
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

// Usage is one request in the ledger. A deployment test's attempt (the admin
// API's test route) is one too, under the authenticated human principal
// and endpoint "deployment_test",
// with the deployment's upstream id for its model and no alias: no inference
// API key made it, and its tokens cost what a call's do.
type Usage struct {
	ID           int64
	RequestID    string
	CreatedAt    time.Time // when the request ended
	APIKeyID     string
	PrincipalID  string // authenticated owner; empty for a legacy key or old ledger row
	Model        string // the name the caller sent
	Alias        string // the configured alias it matched, "*" for the wildcard
	DeploymentID string // the attempt that answered, or the last one made
	CredentialID string
	SessionID    string // "" when the caller sent none
	Protocol     string // the inbound protocol
	Endpoint     string // the route's: "messages", "count_tokens", "chat_completions", "responses", "embeddings", "rerank", "deployment_test"
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
// statement. The ledger and each token-window transaction start independently
// under the caller's deadline. They share the connection pool. All writes reuse
// one database completion timestamp, including across minute/day boundaries.
// Tokens feed the model window even if the ledger rejects the row.
// Cost is computed here, at the prices the deployment has now, a
// missing price costing nothing and missing tokens making no cost; it is
// returned, nil when the row has none, was not written, or holds one no
// float64 can.
func (s *Store) RecordUsage(ctx context.Context, u Usage) (*float64, error) {
	var completed time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&completed); err != nil {
		return nil, err
	}
	var in, out, cw, cr *int64
	if t := u.Tokens; t != nil {
		in, out, cw, cr = &t.Input, &t.Output, &t.CacheWrite, &t.CacheRead
	}
	var windows sync.WaitGroup
	var modelErr error
	if t := u.Tokens; t != nil {
		if u.Alias != "" {
			windows.Go(func() {
				modelErr = s.exec(ctx, `INSERT INTO modelgateway.model_rate_windows AS w (model,minute,input_tokens,output_tokens)
					VALUES ($1,date_trunc('minute',$4::timestamptz),$2,$3)
					ON CONFLICT (model,minute) DO UPDATE SET
					input_tokens=w.input_tokens+EXCLUDED.input_tokens,output_tokens=w.output_tokens+EXCLUDED.output_tokens`,
					u.Alias, t.Input+t.CacheWrite, t.Output, completed)
			})
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
		      cache_write_tokens, cache_read_tokens, cost, latency_us, ttft_us, principal_id, created_at, workspace_id)
		  SELECT $1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10, NULLIF($11, ''),
		         $12::bigint, $13::bigint, $14::bigint, $15::bigint,
		         (coalesce(d.price_input, 0) * $12 + coalesce(d.price_output, 0) * $13
		          + coalesce(d.price_cache_write, 0) * $14 + coalesce(d.price_cache_read, 0) * $15) / 1000000,
		         $16, $17, NULLIF($18, ''), $19, 'default'
		  FROM (VALUES (1)) one LEFT JOIN modelgateway.deployments d ON d.id = $5
		  RETURNING created_at, api_key_id, principal_id, alias, deployment_id, error_type, workspace_id,
		            input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost
		)
		INSERT INTO modelgateway.usage_daily AS t (day, api_key_id, alias, deployment_id, requests, errors,
		    input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost, principal_id, workspace_id)
		SELECT (created_at AT TIME ZONE 'UTC')::date, api_key_id, alias, deployment_id, 1,
		       (error_type IS NOT NULL)::integer, coalesce(input_tokens, 0), coalesce(output_tokens, 0),
		       coalesce(cache_write_tokens, 0), coalesce(cache_read_tokens, 0), coalesce(cost, 0), principal_id, workspace_id
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
			u.Endpoint, u.Status, u.ErrorType, in, out, cw, cr, u.Latency.Microseconds(), ttft, u.PrincipalID, completed).Scan(&cost)
	})
	windows.Wait()
	windowErr := modelErr
	if ledgerErr != nil || cost == nil {
		return nil, errors.Join(windowErr, ledgerErr)
	}
	// The cost is read as text and parsed once the row is committed: one no
	// float64 holds, or that is no number (a price written to the database
	// directly can be NaN or infinite), loses the caller its reading, never
	// the row.
	f, err := strconv.ParseFloat(*cost, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, windowErr
	}
	return &f, windowErr
}

// UsageFilter narrows a usage read; an empty field matches every value.
type UsageFilter struct {
	APIKeyID     string
	PrincipalID  string
	Alias        string
	DeploymentID string
	SessionID    string // ListUsage only: the daily rollup has no sessions
}

// ListUsage reads the ledger newest first: up to limit rows older than the
// row with id before (zero: from the newest), and whether more follow. The
// query names only the filters given, so each can use its index: a plan
// written for every filter at once would serve none of them well.
func (s *Store) ListUsage(ctx context.Context, f UsageFilter, before int64, limit int) ([]Usage, bool, error) {
	where, args := []string{"workspace_id='default'"}, []any{}
	for _, c := range []struct {
		cond string
		arg  any
		set  bool
	}{
		{"id < $%d", before, before > 0},
		{"api_key_id = $%d", f.APIKeyID, f.APIKeyID != ""},
		{"principal_id = $%d", f.PrincipalID, f.PrincipalID != ""},
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
		       latency_us, coalesce(ttft_us, 0), coalesce(principal_id, '')
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
			&in, &o, &cw, &cr, &u.Cost, &latency, &ttft, &u.PrincipalID); err != nil {
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

// DailyUsage is one day's rollup for a principal (or legacy key), alias and
// deployment. A credential-filtered read retains the per-key breakdown.
type DailyUsage struct {
	Day          time.Time // midnight UTC
	APIKeyID     string
	PrincipalID  string
	Alias        string
	DeploymentID string
	Requests     int64
	Errors       int64 // requests the caller was given an error for
	Tokens       Tokens
	Cost         float64
}

// ListDailyUsage reads the rollups of the UTC days from through to, both
// included, in order of day, principal (or legacy key), alias and deployment:
// up to limit rows, and whether more match.
func (s *Store) ListDailyUsage(ctx context.Context, f UsageFilter, from, to time.Time, limit int) ([]DailyUsage, bool, error) {
	// A credential filter retains the old per-key view for existing Console
	// clients. Without it, owned keys are combined in the canonical actor rollup.
	source := `SELECT day, api_key_id, principal_id, alias, deployment_id, requests, errors,
		input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost FROM modelgateway.usage_actor_daily WHERE workspace_id='default'`
	if f.APIKeyID != "" {
		source = `SELECT d.day, d.api_key_id, d.principal_id, d.alias, d.deployment_id,
			d.requests, d.errors, d.input_tokens, d.output_tokens, d.cache_write_tokens, d.cache_read_tokens, d.cost
			FROM modelgateway.usage_daily d WHERE d.workspace_id='default'`
	}
	rows, err := s.pool.Query(ctx, `
		SELECT day, api_key_id, alias, deployment_id, requests, errors, input_tokens, output_tokens,
		       cache_write_tokens, cache_read_tokens, cost::float8, coalesce(principal_id, '')
		FROM (`+source+`) d
		WHERE day BETWEEN $1::date AND $2::date AND ($3 = '' OR api_key_id = $3) AND ($4 = '' OR alias = $4)
		  AND ($5 = '' OR deployment_id = $5) AND ($6 = '' OR principal_id = $6)
		ORDER BY day, coalesce(principal_id, api_key_id), alias, deployment_id LIMIT $7`,
		from.UTC().Format(time.DateOnly), to.UTC().Format(time.DateOnly), f.APIKeyID, f.Alias, f.DeploymentID, f.PrincipalID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []DailyUsage
	for rows.Next() {
		var d DailyUsage
		if err := rows.Scan(&d.Day, &d.APIKeyID, &d.Alias, &d.DeploymentID, &d.Requests, &d.Errors,
			&d.Tokens.Input, &d.Tokens.Output, &d.Tokens.CacheWrite, &d.Tokens.CacheRead, &d.Cost, &d.PrincipalID); err != nil {
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
				_, err = tx.Exec(ctx, `DELETE FROM modelgateway.model_rate_windows WHERE minute < now() - interval '1 hour'`)
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
