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
// an interval when none is open, so a reclaim's running inside one does not
// restart it; idle, rescheduled and terminated close it. An interval that
// reads negative — two hosts' clocks — adds nothing.
func (c *sessionClock) step(typ domain.EventType, at time.Time) {
	switch typ {
	case domain.EventSessionStatusRunning:
		if c.since == nil {
			c.since = &at
			c.changed = true
		}
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

// readSessionClock reads what a batch carrying the session's status events
// needs, under the row lock the append holds: the running time, and the token
// totals an idle's snapshot reports, with the append's own turn folded in,
// since AppendInTx folds AddUsage only after its insert.
func readSessionClock(ctx context.Context, tx pgx.Tx, sessionID domain.ID, addUsage *domain.ModelUsage) (*sessionClock, domain.Usage, error) {
	var (
		clock sessionClock
		raw   []byte
		usage domain.Usage
	)
	if err := tx.QueryRow(ctx,
		`SELECT usage, active_seconds, running_since FROM sessions WHERE id = $1`,
		sessionID.String()).Scan(&raw, &clock.active, &clock.since); err != nil {
		return nil, usage, err
	}
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil, usage, fmt.Errorf("decode stored session usage: %w", err)
	}
	if addUsage != nil {
		usage.Add(*addUsage)
	}
	return &clock, usage, nil
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
