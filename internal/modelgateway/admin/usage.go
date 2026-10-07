package admin

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// dailyUsageView is one UTC day's rollup for a key, alias and deployment.
type dailyUsageView struct {
	Day              string  `json:"day"`
	APIKeyID         string  `json:"api_key_id"`
	Alias            string  `json:"alias"`
	DeploymentID     string  `json:"deployment_id"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	Cost             float64 `json:"cost"`
}

// usageView is one request in the ledger. The counts are null when the
// upstream reported none — once it reports one, a count it leaves out is 0 —
// and a session, error or time to first token the request did not have is
// null.
type usageView struct {
	ID               int64     `json:"id"`
	RequestID        string    `json:"request_id"`
	CreatedAt        time.Time `json:"created_at"`
	APIKeyID         string    `json:"api_key_id"`
	Model            string    `json:"model"`
	Alias            string    `json:"alias"`
	DeploymentID     string    `json:"deployment_id"`
	CredentialID     string    `json:"credential_id"`
	SessionID        *string   `json:"session_id"`
	Protocol         string    `json:"protocol"`
	Endpoint         string    `json:"endpoint"`
	Status           int       `json:"status"`
	ErrorType        *string   `json:"error_type"`
	InputTokens      *int64    `json:"input_tokens"`
	OutputTokens     *int64    `json:"output_tokens"`
	CacheWriteTokens *int64    `json:"cache_write_tokens"`
	CacheReadTokens  *int64    `json:"cache_read_tokens"`
	Cost             *float64  `json:"cost"`
	LatencyMS        float64   `json:"latency_ms"`
	TTFTMS           *float64  `json:"ttft_ms"`
}

// usagePage is a page of the ledger, newest first; has_more says another
// follows, read by passing the last row's id as before.
type usagePage struct {
	Data    []usageView `json:"data"`
	HasMore bool        `json:"has_more"`
}

// The daily read's default span and longest one, and the ledger page's
// default and largest size.
const (
	defaultDays  = 30
	maxDays      = 366
	defaultLimit = 100
	maxLimit     = 1000
)

// maxDailyRows bounds the rollups one daily read answers, some 10 MB of JSON:
// a read that matches more is refused, to be narrowed, rather than cut short
// or built whole in memory.
var maxDailyRows = 50000

// query returns the request's query, refusing a malformed one, a parameter
// outside allowed, given twice or given empty, as a body's unknown field is
// refused: a pair that does not parse or names no value is not dropped,
// which would read as a filter the caller did not ask to lift.
func query(r *http.Request, allowed ...string) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, invalid("the query does not parse: %s", err)
	}
	for k, vs := range q {
		if !slices.Contains(allowed, k) {
			return nil, invalid("unknown query parameter %q", k)
		}
		if len(vs) > 1 {
			return nil, invalid("query parameter %q is given more than once", k)
		}
		if vs[0] == "" {
			return nil, invalid("query parameter %q is empty", k)
		}
	}
	return q, nil
}

func usageFilter(q url.Values) store.UsageFilter {
	return store.UsageFilter{APIKeyID: q.Get("api_key_id"), Alias: q.Get("alias"),
		DeploymentID: q.Get("deployment_id"), SessionID: q.Get("session_id")}
}

// dailyUsage answers the rollups of the UTC days from through to, both
// included and given as YYYY-MM-DD: to defaults to today and from to the 30
// days ending at to, and the span is at most 366 days.
func (h *handler) dailyUsage(r *http.Request) (any, error) {
	q, err := query(r, "from", "to", "api_key_id", "alias", "deployment_id")
	if err != nil {
		return nil, err
	}
	day := func(name string, def time.Time) (time.Time, error) {
		v := q.Get(name)
		if v == "" {
			return def, nil
		}
		t, err := time.Parse(time.DateOnly, v)
		if err != nil {
			return time.Time{}, invalid("%s: %q is not a date as YYYY-MM-DD", name, v)
		}
		return t, nil
	}
	to, err := day("to", time.Now().UTC().Truncate(24*time.Hour))
	if err != nil {
		return nil, err
	}
	from, err := day("from", to.AddDate(0, 0, 1-defaultDays))
	if err != nil {
		return nil, err
	}
	switch {
	case from.After(to):
		return nil, invalid("from (%s) is after to (%s)", from.Format(time.DateOnly), to.Format(time.DateOnly))
	case to.Sub(from) >= maxDays*24*time.Hour:
		return nil, invalid("from and to span more than %d days", maxDays)
	}
	days, more, err := h.cfg.Store.ListDailyUsage(r.Context(), usageFilter(q), from, to, maxDailyRows)
	if err != nil {
		return nil, err
	}
	if more {
		return nil, invalid("more than %d rollups match; narrow the span, or filter by key, alias or deployment", maxDailyRows)
	}
	out := make([]dailyUsageView, 0, len(days))
	for _, d := range days {
		out = append(out, dailyUsageView{Day: d.Day.Format(time.DateOnly), APIKeyID: d.APIKeyID, Alias: d.Alias,
			DeploymentID: d.DeploymentID, Requests: d.Requests, Errors: d.Errors, InputTokens: d.Tokens.Input,
			OutputTokens: d.Tokens.Output, CacheWriteTokens: d.Tokens.CacheWrite, CacheReadTokens: d.Tokens.CacheRead,
			Cost: d.Cost})
	}
	return listView{Data: out}, nil
}

// listUsage answers a page of the ledger, newest first: up to limit rows
// (default 100, at most 1000) older than the row whose id is before.
func (h *handler) listUsage(r *http.Request) (any, error) {
	q, err := query(r, "before", "limit", "api_key_id", "alias", "deployment_id", "session_id")
	if err != nil {
		return nil, err
	}
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > maxLimit {
			return nil, invalid("limit: %q is not a whole number from 1 to %d", v, maxLimit)
		}
	}
	var before int64
	if v := q.Get("before"); v != "" {
		if before, err = strconv.ParseInt(v, 10, 64); err != nil || before < 1 {
			return nil, invalid("before: %q is not a row id", v)
		}
	}
	rows, more, err := h.cfg.Store.ListUsage(r.Context(), usageFilter(q), before, limit)
	if err != nil {
		return nil, err
	}
	out := make([]usageView, 0, len(rows))
	for _, u := range rows {
		v := usageView{ID: u.ID, RequestID: u.RequestID, CreatedAt: u.CreatedAt, APIKeyID: u.APIKeyID, Model: u.Model,
			Alias: u.Alias, DeploymentID: u.DeploymentID, CredentialID: u.CredentialID, SessionID: nonEmpty(u.SessionID),
			Protocol: u.Protocol, Endpoint: u.Endpoint, Status: u.Status, ErrorType: nonEmpty(u.ErrorType), Cost: u.Cost,
			LatencyMS: millis(u.Latency)}
		if t := u.Tokens; t != nil {
			v.InputTokens, v.OutputTokens, v.CacheWriteTokens, v.CacheReadTokens = &t.Input, &t.Output, &t.CacheWrite, &t.CacheRead
		}
		if u.TTFT > 0 {
			ms := millis(u.TTFT)
			v.TTFTMS = &ms
		}
		out = append(out, v)
	}
	return usagePage{Data: out, HasMore: more}, nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
