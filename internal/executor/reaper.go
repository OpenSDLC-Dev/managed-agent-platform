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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
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
	// tierIdle: the session is idle past the configured TTL with no work owed
	// and no unanswered ask — its workspace is checkpointed, then the sandbox
	// goes (plan 24 slice 5). A later user.message provisions fresh and
	// restores.
	tierIdle reapTier = "idle"
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
	tier, err := e.classifyForReap(ctx, e.pool, sid)
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

	// Re-classify on the connection already holding the lock — a second pool
	// acquisition here would deadlock a deliberately tiny pool.
	tier, err = e.classifyForReap(ctx, conn, sid)
	if err != nil || tier == tierNone {
		return err
	}
	if tier == tierDeleted {
		if e.blobs != nil {
			if err := e.blobs.Delete(ctx, blob.SessionCheckpointKey(sid.String())); err != nil {
				return fmt.Errorf("delete checkpoint blob: %w", err)
			}
		}
		// The marker row goes with its blob (blob first, so a failed blob
		// delete aborts while the retry trigger — the owned sandbox — stands).
		// A deleted session is never provisioned again, so the row until here
		// was inert; this is its one cleanup owner.
		if _, err := conn.Exec(ctx,
			`DELETE FROM session_checkpoints WHERE session_id = $1`, sid.String()); err != nil {
			return fmt.Errorf("delete checkpoint marker: %w", err)
		}
	}
	if tier == tierIdle {
		// D8: every capture failure degrades to reap-without-checkpoint, loudly
		// — an agent must not pin its sandbox immortal by filling the disk
		// (too_large), and a sandbox that cannot be read cannot be saved. The
		// capture metric records the outcome; a failed capture writes no
		// marker, so the next provision starts fresh.
		if err := e.captureCheckpoint(ctx, sid); err != nil {
			slog.WarnContext(ctx, "idle reap proceeds without a checkpoint",
				"session", sid, "error", err)
		}
	}
	if err := e.provider.Reap(ctx, sid); err != nil {
		return err
	}
	recordSessionReaped(ctx, tier)
	return nil
}

// idleTTL is the idle tier's effective TTL: the configured value, or zero —
// tier off — when no object store exists to hold the checkpoints reaping
// would take (Run logs that disablement once at startup).
func (e *Executor) idleTTL() time.Duration {
	if e.blobs == nil {
		return 0
	}
	return e.cfg.SandboxIdleTTL
}

// classifyForReap reads the session's terminal tier from the database — never
// from a caller's claim — and answers tierNone for anything that is not a
// **cloud** session: a self_hosted session's sandbox carries the same
// ownership label but belongs to the customer's BYOC worker, which never
// takes the advisory lock, so on a shared daemon it is not the platform's to
// destroy in any tier. The deleted tier requires the tombstone deleteSession
// writes (which records the kind, the row being gone), not merely a missing
// row: a missing row also describes a holding that was never this
// deployment's (a second deployment sharing the Docker daemon or K8s
// namespace labels sandboxes with ids this database never saw), and those are
// skipped. Archived beats terminated (the stricter lifecycle answer). The
// idle tier (plan 24 slice 5) takes what remains only when armed
// (idleTTL() > 0) and every predicate holds in the same snapshot: status
// idle, last activity older than the TTL, no queued/starting/active work
// item (a pending harvest or tool_exec must find the tree it was enqueued
// against), and no unanswered confirmation ask (HITL-idle is still mid-turn
// — the approved command must run in the context the human saw; the ask read
// is ordered before the main query, see below). `user.interrupt` does not
// reap — an interrupted-then-abandoned session falls to the TTL like any
// other. Running and rescheduling stay untouchable.
func (e *Executor) classifyForReap(ctx context.Context, q events.Querier, sid domain.ID) (reapTier, error) {
	// The idle tier's ask exclusion is read BEFORE the main criteria query,
	// deliberately: the two reads are separate snapshots, and a confirmation
	// batch answers the ask, enqueues the tool's work item, and flips the
	// session running in ONE transaction. Asks-first means that transaction
	// either lands before both reads (the main query then sees running or the
	// live work item — ineligible) or after the ask read (the ask read still
	// saw the unanswered ask — ineligible). Main-first would let the
	// transaction land between the reads and both come back permissive.
	askBlocked := false
	if e.idleTTL() > 0 {
		asks, err := events.UnconfirmedAskEvents(ctx, q, sid, nil)
		if err != nil {
			return tierNone, err
		}
		askBlocked = len(asks) > 0
	}

	var status, kind string
	var archived, idlePastTTL, liveWork bool
	err := q.QueryRow(ctx,
		`SELECT s.status, s.archived_at IS NOT NULL, e.kind,
		        s.updated_at < now() - make_interval(secs => $2),
		        EXISTS (SELECT 1 FROM work_items w
		                WHERE w.session_id = s.id
		                  AND w.state IN ('queued', 'starting', 'active'))
		 FROM sessions s JOIN environments e ON e.id = s.environment_id
		 WHERE s.id = $1`, sid.String(), e.idleTTL().Seconds()).
		Scan(&status, &archived, &kind, &idlePastTTL, &liveWork)
	if errors.Is(err, pgx.ErrNoRows) {
		var deadKind string
		err := q.QueryRow(ctx,
			`SELECT environment_kind FROM deleted_sessions WHERE id = $1`, sid.String()).Scan(&deadKind)
		if errors.Is(err, pgx.ErrNoRows) {
			return tierNone, nil
		}
		if err != nil {
			return tierNone, err
		}
		if deadKind == string(domain.EnvCloud) {
			return tierDeleted, nil
		}
		return tierNone, nil
	}
	if err != nil {
		return tierNone, err
	}
	if kind != string(domain.EnvCloud) {
		return tierNone, nil
	}
	if archived {
		return tierArchived, nil
	}
	if status == string(domain.SessionTerminated) {
		return tierTerminated, nil
	}
	if e.idleTTL() > 0 && status == string(domain.SessionIdle) &&
		idlePastTTL && !liveWork && !askBlocked {
		return tierIdle, nil
	}
	return tierNone, nil
}

// unlockSession releases a session's advisory lock on its own detached
// context — a cancelled reap must still unlock. A failed unlock must not
// return a healthy connection to the pool still holding the lock (session
// advisory locks survive a pool release), so the connection is closed
// instead: the server frees every lock a closing connection holds.
func unlockSession(conn *pgxpool.Conn, sid domain.ID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, sessionLockKey(sid)); err != nil {
		_ = conn.Conn().Close(ctx)
		slog.Warn("advisory unlock failed; closing the connection so the server frees the lock",
			"session", sid, "error", err)
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
