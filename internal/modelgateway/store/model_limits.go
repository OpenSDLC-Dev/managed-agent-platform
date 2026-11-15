package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// ModelLimit is the organization's budget for a configured model alias.
// A nil value leaves that dimension unlimited. '*' covers all wildcard matches.
type ModelLimit struct {
	Model           string
	RPM, ITPM, OTPM *int64
}

// Admission is the shared model and legacy-key limit decision for one request.
type Admission struct {
	Admitted   bool
	Tokens     bool   // a retained key policy refused for tokens rather than requests
	RetryAfter int    // seconds until the window the refusal counted in ends
	Limit      string // organization or legacy-key dimension that refused admission
}

// lockModelLimit orders configuration changes and admission, including when no
// configuration row exists yet. Read committed reads the limit after the wait.
func lockModelLimit(ctx context.Context, tx pgx.Tx, model string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 69))`, "modelgateway.model_limit/"+model)
	return err
}

func (s *Store) PutModelLimit(ctx context.Context, limit ModelLimit, authorize func(pgx.Tx) error) (ModelLimit, error) {
	if limit.Model == "" || utf8.RuneCountInString(limit.Model) > 256 || !utf8.ValidString(limit.Model) || strings.ContainsRune(limit.Model, 0) {
		return ModelLimit{}, fail(ErrInvalid, "model must be 1 to 256 UTF-8 characters without NUL")
	}
	for _, n := range []*int64{limit.RPM, limit.ITPM, limit.OTPM} {
		if n != nil && *n < 1 {
			return ModelLimit{}, fail(ErrInvalid, "rate limits must be positive, or null for no limit")
		}
	}
	err := pgx.BeginTxFunc(ctx, s.pool, readCommitted, func(tx pgx.Tx) error {
		if authorize != nil {
			if err := authorize(tx); err != nil {
				return err
			}
		}
		if err := lockModelLimit(ctx, tx, limit.Model); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO modelgateway.model_limits (model,rpm,itpm,otpm) VALUES ($1,$2,$3,$4)
			ON CONFLICT (model) DO UPDATE SET rpm=EXCLUDED.rpm,itpm=EXCLUDED.itpm,otpm=EXCLUDED.otpm`,
			limit.Model, limit.RPM, limit.ITPM, limit.OTPM)
		if err != nil {
			return err
		}
		// Principal and session locks fence withdrawals, but time still passes
		// during either the model advisory wait or the row write. Recheck expiry
		// after both; a refusal rolls back the tentative limit change.
		if authorize != nil {
			return authorize(tx)
		}
		return nil
	})
	return limit, err
}

func (s *Store) ListModelLimits(ctx context.Context) ([]ModelLimit, error) {
	rows, err := s.pool.Query(ctx, `SELECT model,rpm,itpm,otpm FROM modelgateway.model_limits ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelLimit{}
	for rows.Next() {
		var limit ModelLimit
		if err := rows.Scan(&limit.Model, &limit.RPM, &limit.ITPM, &limit.OTPM); err != nil {
			return nil, err
		}
		out = append(out, limit)
	}
	return out, rows.Err()
}

var errRateRefused = errors.New("rate limit reached")

// AdmitModel counts one external request against the organization's model
// budget and, for an unowned key, its existing policy. Both counts commit or
// neither does. Token limits use completed usage, so in-flight calls can
// overshoot them; RPM bounds admissions. Cache reads do not count as input.
func (s *Store) AdmitModel(ctx context.Context, model, key string, rpm *int32, tpm *int64) (Admission, error) {
	var a Admission
	err := pgx.BeginTxFunc(ctx, s.pool, readCommitted, func(tx pgx.Tx) error {
		if err := lockModelLimit(ctx, tx, model); err != nil {
			return err
		}
		var limit ModelLimit
		err := tx.QueryRow(ctx, `SELECT rpm,itpm,otpm FROM modelgateway.model_limits WHERE model=$1`, model).
			Scan(&limit.RPM, &limit.ITPM, &limit.OTPM)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Stamp the minute after a possible lock wait, not at BEGIN.
		var minute time.Time
		if err := tx.QueryRow(ctx, `SELECT date_trunc('minute',clock_timestamp())`).Scan(&minute); err != nil {
			return err
		}
		finish := func(refused bool) error {
			if err := tx.QueryRow(ctx, `SELECT greatest(1,ceil(extract(epoch FROM $1::timestamptz+interval '1 minute'-clock_timestamp())))::integer`, minute).Scan(&a.RetryAfter); err != nil {
				return err
			}
			if refused {
				return errRateRefused
			}
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO modelgateway.model_rate_windows (model,minute) VALUES ($1,$2::timestamptz)
			ON CONFLICT DO NOTHING`, model, minute); err != nil {
			return err
		}
		var requests, input, output int64
		if err := tx.QueryRow(ctx, `SELECT requests,input_tokens,output_tokens FROM modelgateway.model_rate_windows
			WHERE model=$1 AND minute=$2::timestamptz FOR UPDATE`, model, minute).Scan(&requests, &input, &output); err != nil {
			return err
		}
		switch {
		case limit.RPM != nil && requests >= *limit.RPM:
			a.Limit = "requests_per_minute"
		case limit.ITPM != nil && input >= *limit.ITPM:
			a.Limit = "input_tokens_per_minute"
		case limit.OTPM != nil && output >= *limit.OTPM:
			a.Limit = "output_tokens_per_minute"
		}
		if a.Limit != "" {
			return finish(true)
		}
		if rpm != nil || tpm != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO modelgateway.rate_windows (api_key_id,minute) VALUES ($1,$2::timestamptz)
				ON CONFLICT DO NOTHING`, key, minute); err != nil {
				return err
			}
			var kr, kt int64
			if err := tx.QueryRow(ctx, `SELECT requests,tokens FROM modelgateway.rate_windows WHERE api_key_id=$1 AND minute=$2::timestamptz FOR UPDATE`, key, minute).
				Scan(&kr, &kt); err != nil {
				return err
			}
			switch {
			case tpm != nil && kt >= *tpm:
				a.Limit, a.Tokens = "key_tokens_per_minute", true
			case rpm != nil && kr >= int64(*rpm):
				a.Limit = "key_requests_per_minute"
			}
			if a.Limit != "" {
				return finish(true)
			}
			if _, err := tx.Exec(ctx, `UPDATE modelgateway.rate_windows SET requests=requests+1 WHERE api_key_id=$1 AND minute=$2::timestamptz`, key, minute); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE modelgateway.model_rate_windows SET requests=requests+1 WHERE model=$1 AND minute=$2::timestamptz`, model, minute)
		if err != nil {
			return err
		}
		return finish(false)
	})
	if errors.Is(err, errRateRefused) {
		return a, nil
	}
	a.Admitted = err == nil
	return a, err
}
