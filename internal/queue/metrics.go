package queue

import (
	"context"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The work-queue depth/pending/workers_polling gauges — the OTLP metric mirror of
// the /work/stats endpoint's BetaSelfHostedWorkQueueStats shape, reported per
// self_hosted environment. They are asynchronous: an operator reads a queue's
// backlog at collection time, not a running total, so the SDK samples them
// through the callback RegisterMetrics installs. The names are exported so the
// telemetry contract test can assert they reach an OTLP collector.
const (
	MetricQueueDepth          = "queue.depth"
	MetricQueuePending        = "queue.pending"
	MetricQueueWorkersPolling = "queue.workers_polling"
)

// meterName is this package's OTel instrumentation scope.
const meterName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"

// RegisterMetrics installs the observable queue gauges on the global meter
// provider. It is called once per process (the control plane, which already owns
// the /work/stats view), after telemetry.Init has installed that provider, and
// the returned error is only a registration failure. The callback enumerates the
// self_hosted environments and reports each one's Stats, so depth/pending/
// workers_polling carry an environment.id and mean exactly what /work/stats does.
// A Stats failure aborts that one collection rather than a turn — an observable
// callback has nothing to fail but the sample.
//
// With more than one control-plane replica each would report the same per-
// environment values (the stats are global to the database), so a dashboard must
// read the gauge as last/max across instances, not a sum — the standard caveat
// for a gauge sampled from shared state. v1 is single-replica.
//
// The returned Registration must be Unregister-ed before the pool it reads is
// closed: the meter provider's shutdown does a final collection, and a callback
// that outlived the pool would query a closed pool (a benign but noisy error and
// a dropped final sample). The control plane unregisters ahead of pool.Close.
func (q *Queue) RegisterMetrics() (metric.Registration, error) {
	meter := otel.GetMeterProvider().Meter(meterName)
	depth, err := meter.Int64ObservableGauge(MetricQueueDepth,
		metric.WithUnit("{item}"),
		metric.WithDescription("Queued work items with no live poll reservation, per self_hosted environment."))
	if err != nil {
		return nil, err
	}
	pending, err := meter.Int64ObservableGauge(MetricQueuePending,
		metric.WithUnit("{item}"),
		metric.WithDescription("Queued work items with a live poll reservation, per self_hosted environment."))
	if err != nil {
		return nil, err
	}
	workers, err := meter.Int64ObservableGauge(MetricQueueWorkersPolling,
		metric.WithUnit("{worker}"),
		metric.WithDescription("Distinct workers polling within the window, per self_hosted environment."))
	if err != nil {
		return nil, err
	}

	return meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		envIDs, err := q.selfHostedEnvIDs(ctx)
		if err != nil {
			return err
		}
		for _, envID := range envIDs {
			s, err := q.Stats(ctx, envID)
			if err != nil {
				return err
			}
			attrs := metric.WithAttributes(attribute.String("environment.id", envID.String()))
			o.ObserveInt64(depth, s.Depth, attrs)
			o.ObserveInt64(pending, s.Pending, attrs)
			o.ObserveInt64(workers, s.WorkersPolling, attrs)
		}
		return nil
	}, depth, pending, workers)
}

// selfHostedEnvIDs lists the environments whose queue the gauges report on: the
// self_hosted work queues the workers poll. Cloud environments run the built-in
// executor, which claims rather than polls, so workers_polling is meaningless
// there and they are left out — the same scoping /work/stats applies.
func (q *Queue) selfHostedEnvIDs(ctx context.Context) ([]domain.ID, error) {
	rows, err := q.pool.Query(ctx,
		`SELECT id FROM environments WHERE kind = 'self_hosted'`)
	if err != nil {
		return nil, fmt.Errorf("queue: list self_hosted environments: %w", err)
	}
	defer rows.Close()
	var ids []domain.ID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, domain.ID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list self_hosted environments: %w", err)
	}
	return ids, nil
}
