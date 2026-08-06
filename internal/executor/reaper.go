package executor

// The sandbox reaper (plan 24): the one owner of sandbox destruction. It lives
// in the executor because this is the only process holding both the sandbox
// provider and the pool, and it needs no coordination across replicas — Owned
// is endpoint-local (each executor sees only its own daemon or namespace, a
// natural shard) and Reap is idempotent, so N executors reap concurrently.
// Teardown is eventual, one reap interval behind the trigger, which the wire
// cannot observe: no API surface exposes a sandbox's existence.

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// MetricSessionsReaped counts reaped sessions by tier.
const MetricSessionsReaped = "sandbox.sessions.reaped"

// reapTier classifies why a session's sandbox may be destroyed. The zero value
// means it may not.
type reapTier string

const (
	tierNone reapTier = ""
	// tierDeleted: the session row is gone. The checkpoint blob goes too —
	// nothing will ever resume this session.
	tierDeleted reapTier = "deleted"
	// tierArchived / tierTerminated: the session record persists, so its
	// checkpoint blob stays until the row itself is deleted.
	tierArchived   reapTier = "archived"
	tierTerminated reapTier = "terminated"
)

// sessionLockKey maps a session id onto the Postgres advisory-lock keyspace.
// One derivation shared by the reaper and provisionSandbox — the whole point
// of the lock is that both sides compute the same key. FNV-1a over the id: the
// keyspace is 64-bit and ids are random, so a collision merely serializes two
// unrelated sessions' provisions, costing latency, never correctness.
func sessionLockKey(id domain.ID) int64 {
	h := fnv.New64a()
	h.Write([]byte(id))
	return int64(h.Sum64())
}

// reapHookAfterClassify, when non-nil, runs between the pre-lock candidate
// classification and the lock acquisition. Test-only: it is the seam that
// proves the criteria are re-read under the lock (a session revived in that
// window must not be reaped on the stale answer). Always nil in production.
var reapHookAfterClassify func(domain.ID)

// reapLoop drives one reap pass per interval until the context ends.
func (e *Executor) reapLoop(ctx context.Context) {
	t := time.NewTicker(e.cfg.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := e.reapPass(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "reap pass incomplete; the next interval retries", "error", err)
		}
	}
}

// reapPass sweeps this endpoint's owned sessions once. Per-session failures
// are joined, not fatal to the pass — one wedged session must not shield the
// rest of the endpoint from teardown.
func (e *Executor) reapPass(ctx context.Context) error {
	owned, err := e.provider.Owned(ctx)
	if err != nil {
		return fmt.Errorf("list owned sessions: %w", err)
	}
	var errs error
	for _, sid := range owned {
		if err := e.reapSession(ctx, sid); err != nil {
			errs = errors.Join(errs, fmt.Errorf("session %s: %w", sid, err))
		}
	}
	return errs
}

// reapSession destroys one session's holding when its database lifecycle says
// so: candidate classification → per-session advisory try-lock → the
// authoritative re-classification under the lock (plan 24 D4 — the pre-lock
// answer is stale the moment it returns) → for the deleted tier, the
// checkpoint blob first (a failed blob delete aborts while the sandbox is
// still owned, keeping the retry trigger) → Reap.
func (e *Executor) reapSession(ctx context.Context, sid domain.ID) error {
	tier, err := e.classifyForReap(ctx, sid)
	if err != nil || tier == tierNone {
		return err
	}
	if reapHookAfterClassify != nil {
		reapHookAfterClassify(sid)
	}

	// Try-lock, deliberately: a held lock is a provision in flight — the
	// session is in use, this pass skips it, the next one re-asks. Blocking
	// here would pin the reaper behind an image pull.
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, sessionLockKey(sid)).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer unlockSession(conn, sid)

	tier, err = e.classifyForReap(ctx, sid)
	if err != nil || tier == tierNone {
		return err
	}
	if tier == tierDeleted && e.blobs != nil {
		if err := e.blobs.Delete(ctx, blob.SessionCheckpointKey(sid.String())); err != nil {
			return fmt.Errorf("delete checkpoint blob: %w", err)
		}
	}
	if err := e.provider.Reap(ctx, sid); err != nil {
		return err
	}
	recordSessionReaped(ctx, tier)
	return nil
}

// classifyForReap reads the session's terminal tier from the database — never
// from a caller's claim. A missing row is the deleted tier; archived beats
// terminated (the stricter lifecycle answer); everything else — idle, running,
// rescheduling — is not this slice's to touch (the idle-TTL tier is plan 24
// slice 5).
func (e *Executor) classifyForReap(ctx context.Context, sid domain.ID) (reapTier, error) {
	var status string
	var archived bool
	err := e.pool.QueryRow(ctx,
		`SELECT status, archived_at IS NOT NULL FROM sessions WHERE id = $1`, sid.String()).
		Scan(&status, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return tierDeleted, nil
	}
	if err != nil {
		return tierNone, err
	}
	if archived {
		return tierArchived, nil
	}
	if status == string(domain.SessionTerminated) {
		return tierTerminated, nil
	}
	return tierNone, nil
}

// unlockSession releases a session's advisory lock on its own connection,
// detached from the caller's context — a cancelled reap must still unlock
// (the connection going back to the pool healthy keeps the lock from leaking
// with it).
func unlockSession(conn *pgxpool.Conn, sid domain.ID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, sessionLockKey(sid)); err != nil {
		slog.Warn("advisory unlock failed; the connection's release frees the lock", "session", sid, "error", err)
	}
}

// recordSessionReaped counts one reaped session by tier (bounded values).
func recordSessionReaped(ctx context.Context, tier reapTier) {
	counter, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricSessionsReaped,
		metric.WithDescription("Sandboxes destroyed by the reaper, by lifecycle tier."))
	if err != nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("tier", string(tier))))
}
