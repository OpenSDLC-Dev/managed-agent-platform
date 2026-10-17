package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// The session.usage event and the running time behind its active_seconds
// (plan 68). The reference writes a session.usage immediately before every
// session.status_idle, whatever the stop reason, so AppendInTx — which every
// event goes through, under the session row lock — places one before each
// idle a batch carries, and keeps the session's running time from the
// session's own status events as it stamps them.

// sessionStatusTypes are the session's own status events, the ones that move
// its running time. The thread events beside them do not: the session's
// status is the fold over its threads, running while any thread runs, so its
// own events already count concurrent threads once.
var sessionStatusTypes = map[domain.EventType]bool{
	domain.EventSessionStatusRunning:     true,
	domain.EventSessionStatusIdle:        true,
	domain.EventSessionStatusRescheduled: true,
	domain.EventSessionStatusTerminated:  true,
}

// withUsageSnapshots returns evs with a session.usage placed immediately
// before every session.status_idle, or evs itself when it has none. The
// usage takes the idle's stamp, minted here when the caller left it to the
// append, so the time it reports is the time the idle closes. Its body is
// written by the append, which knows the stamps only once the batch's order
// has settled them.
func withUsageSnapshots(evs []NewEvent) []NewEvent {
	n := 0
	for _, ev := range evs {
		if ev.Type == domain.EventSessionStatusIdle {
			n++
		}
	}
	if n == 0 {
		return evs
	}
	out := make([]NewEvent, 0, len(evs)+n)
	for _, ev := range evs {
		if ev.Type == domain.EventSessionStatusIdle {
			if ev.ProcessedAt == nil {
				now := time.Now().UTC()
				ev.ProcessedAt = &now
			}
			out = append(out, NewEvent{Type: domain.EventSessionUsage, ProcessedAt: ev.ProcessedAt})
		}
		out = append(out, ev)
	}
	return out
}

// sessionClock is the session's running time through one append: the sum of
// its closed running intervals (sessions.active_seconds) and the open one's
// start (sessions.running_since, nil while the session is not running).
type sessionClock struct {
	active  float64
	since   *time.Time
	changed bool
}

// step folds one of the session's status events, at its stamp. Running opens
// an interval; idle, rescheduled and terminated close it. An interval that
// reads negative — two hosts' clocks — adds nothing.
//
// No batch this build writes runs a running session again without closing
// first: the reclaim's forced pair closes with its rescheduled. So an interval
// already open at a running means a close went unseen, written by a replica
// on the earlier build, and the running restarts it: the count drops that
// turn's time rather than adding the idle that followed it.
func (c *sessionClock) step(typ domain.EventType, at time.Time) {
	switch typ {
	case domain.EventSessionStatusRunning:
		c.since = &at
		c.changed = true
	case domain.EventSessionStatusIdle, domain.EventSessionStatusRescheduled, domain.EventSessionStatusTerminated:
		if c.since != nil {
			c.active += max(0, at.Sub(*c.since).Seconds())
			c.since = nil
			c.changed = true
		}
	}
}

// through is the running time with the open interval counted up to at.
func (c *sessionClock) through(at time.Time) float64 {
	if c.since == nil {
		return c.active
	}
	return c.active + max(0, at.Sub(*c.since).Seconds())
}

// snapshotUsage is the token totals an idle's snapshot reports: the stored
// usage, read under the row lock, with the append's own turn folded in, since
// AppendInTx folds AddUsage only after its insert.
func snapshotUsage(stored []byte, addUsage *domain.ModelUsage) (domain.Usage, error) {
	var usage domain.Usage
	if err := json.Unmarshal(stored, &usage); err != nil {
		return usage, fmt.Errorf("decode stored session usage: %w", err)
	}
	if addUsage != nil {
		usage.Add(*addUsage)
	}
	return usage, nil
}

// writeSessionClock stores the running time a batch moved.
func writeSessionClock(ctx context.Context, tx pgx.Tx, sessionID domain.ID, clock *sessionClock) error {
	if clock == nil || !clock.changed {
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET active_seconds = $2, running_since = $3 WHERE id = $1`,
		sessionID.String(), clock.active, utcOrNil(clock.since))
	return err
}

// usageSnapshot is a session.usage body: the session's token totals and its
// running time, in the reference's shape for a session without a budget —
// which omits the budget key rather than writing a null. It carries no
// list_cost and no server_tool_use: the platform has no price table and
// counts no server tool requests (plan 68 decision 3).
func usageSnapshot(usage domain.Usage, active float64) (json.RawMessage, error) {
	return json.Marshal(struct {
		Usage domain.SessionUsage `json:"usage"`
	}{domain.NewSessionUsage(usage, active)})
}
