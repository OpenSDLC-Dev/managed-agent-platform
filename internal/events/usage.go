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
// usage takes the idle's stamp when the caller set one; otherwise the append
// stamps the usage in its place and gives the idle the same. It takes the
// idle's Restated too: the snapshot of a restated idle reports the closed sum
// alone, the value that idle leaves stored. Its body is written by the
// append, which knows the stamps only once the batch's order has settled
// them.
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
			out = append(out, NewEvent{Type: domain.EventSessionUsage, ProcessedAt: ev.ProcessedAt, Restated: ev.Restated})
		}
		out = append(out, ev)
	}
	return out
}

// ActiveSecondsAt is a session's running time read at at: the sum of its
// closed intervals plus the open one up to at, where an open interval two
// hosts' clocks make negative counts zero. The session.usage snapshot and the
// session resource both read the running time this way.
func ActiveSecondsAt(active float64, since *time.Time, at time.Time) float64 {
	if since == nil {
		return active
	}
	return active + max(0, at.Sub(*since).Seconds())
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
// No batch AppendInTx writes runs a running session again without closing
// first, and none closes one that was not running except as a restated
// close (NewEvent.Restated). So an interval open at either was left by a
// close the columns never saw — one a binary from before migration 0055
// wrote, or one committed during its scan. A running restarts it, and a
// restated close drops it, so neither counts the gap after that close.
func (c *sessionClock) step(typ domain.EventType, at time.Time, restated bool) {
	switch typ {
	case domain.EventSessionStatusRunning:
		c.since = &at
		c.changed = true
	case domain.EventSessionStatusIdle, domain.EventSessionStatusRescheduled, domain.EventSessionStatusTerminated:
		if c.since != nil {
			if !restated {
				c.active = ActiveSecondsAt(c.active, c.since, at)
			}
			c.since = nil
			c.changed = true
		}
	}
}

// usageAfter is the session's token totals once the append commits: the
// stored usage, read under the row lock, with the append's own turn folded
// in. A snapshot reports it before the append writes it.
func usageAfter(stored []byte, addUsage *domain.ModelUsage) (domain.Usage, error) {
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
