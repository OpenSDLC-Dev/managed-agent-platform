package store

import "context"

// Admission is a limited key's answer for one request.
type Admission struct {
	Admitted   bool
	Tokens     bool // refused for its tokens rather than its requests
	RetryAfter int  // seconds until the window the refusal counted in ends
}

// Admit counts a request against its key's limits for the current minute
// of the database's clock (docs/plan/59_model-gateway.md, "Routing,
// retries, limits"), in one upsert: admitted while the minute's admitted
// requests are under rpm and its completed tokens under tpm, a nil limit
// being none. A refused request is not counted, so a caller retrying in a
// loop does not keep itself out of the next window. TPM is soft: a
// response's tokens count in the minute it ends (RecordUsage), so requests
// already in flight may overshoot it; RPM is what bounds those.
func (s *Store) Admit(ctx context.Context, apiKeyID string, rpm *int32, tpm *int64) (Admission, error) {
	var a Admission
	var tokens int64
	err := s.pool.QueryRow(ctx, `
		WITH up AS (
		  INSERT INTO modelgateway.rate_windows AS w (api_key_id, minute, requests)
		  VALUES ($1, date_trunc('minute', now()), 1)
		  ON CONFLICT (api_key_id, minute) DO UPDATE SET requests = w.requests + 1
		    WHERE ($2::integer IS NULL OR w.requests < $2) AND ($3::bigint IS NULL OR w.tokens < $3)
		  RETURNING 1
		)
		SELECT EXISTS (SELECT 1 FROM up),
		       coalesce((SELECT tokens FROM modelgateway.rate_windows
		                 WHERE api_key_id = $1 AND minute = date_trunc('minute', now())), 0),
		       greatest(1, ceil(extract(epoch FROM date_trunc('minute', now()) + interval '1 minute' - now())))::integer`,
		apiKeyID, rpm, tpm).Scan(&a.Admitted, &tokens, &a.RetryAfter)
	if err != nil {
		return Admission{}, err
	}
	a.Tokens = !a.Admitted && tpm != nil && tokens >= *tpm
	return a, nil
}
