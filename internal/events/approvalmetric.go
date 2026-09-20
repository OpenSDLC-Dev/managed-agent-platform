package events

import (
	"context"
	"errors"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// MetricApprovalWait is the human-in-the-loop approval latency histogram: how
// long a session sat suspended on requires_action before a confirmation cleared
// the gate. Exported so the telemetry contract test can assert this exact name
// reaches an OTLP collector.
const MetricApprovalWait = "approval.wait.duration"

// PendingApprovalThreads snapshots gates before an executor settlement, which
// can process a confirmation received earlier behind another tool's result.
func PendingApprovalThreads(ctx context.Context, q Querier, sid domain.ID) ([]domain.ID, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT COALESCE(tu.thread_id,'') FROM events tu
 WHERE tu.session_id=$1 AND tu.type=ANY($2) AND tu.payload->>'evaluated_permission'='ask'
 AND NOT `+processedAnswerBy(3)+`
 AND NOT EXISTS (SELECT 1 FROM events c WHERE c.session_id=tu.session_id
 AND c.type='user.tool_confirmation' AND c.payload->>'tool_use_id'=tu.id AND c.processed_at IS NOT NULL)`,
		sid.String(), confirmableToolUseTypes, toolResultTypes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClearedApprovalWait measures a gate that was pending before the transaction's
// processing step. The caller records the returned reading only after commit.
func ClearedApprovalWait(ctx context.Context, q Querier, sid, tid domain.ID) (*float64, error) {
	pending, err := PendingThreadApprovals(ctx, q, sid, tid)
	if err != nil || pending {
		return nil, err
	}
	var seconds float64
	err = q.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM (clock_timestamp()-created_at)) FROM events
 WHERE session_id=$1 AND ((type=$2 AND thread_id IS NOT DISTINCT FROM $3::text)
 OR(type=$4 AND $3::text IS NULL)) AND payload->'stop_reason'->>'type'='requires_action'
 ORDER BY(type=$2) DESC,seq DESC LIMIT 1`, sid.String(), string(domain.EventSessionThreadStatusIdle),
		nullableID(tid), string(domain.EventSessionStatusIdle)).Scan(&seconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &seconds, err
}

// RecordApprovalWait records one approval wait, in seconds. The interval is
// measured in the database (clock_timestamp() minus the requires_action idle
// event's created_at) so both ends read the same clock, and recorded here after
// the resuming transaction commits — a confirmation whose commit rolled back did
// not resume anything.
//
// It resolves the meter per call rather than caching an instrument that would pin
// whichever MeterProvider was installed first, and never fails the caller: a
// telemetry error just drops the reading.
func RecordApprovalWait(ctx context.Context, seconds float64) {
	hist, err := otel.GetMeterProvider().Meter(meterName).Float64Histogram(
		MetricApprovalWait,
		metric.WithUnit("s"),
		metric.WithDescription("Time a session waited on a requires_action approval gate before it resumed."))
	if err != nil {
		return
	}
	hist.Record(ctx, seconds)
}
