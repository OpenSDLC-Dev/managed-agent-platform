package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// The two holds the dream runner keeps on the public surface (plan 41 §4.4):
// the rows it owns are unreachable, and the session it drives is read-only
// while it drives it.

// notInternal hides the runner's own agent and environment rows from every
// public path — one WHERE fragment rather than a copy per route, so the
// ErrNoRows it produces is the 404 an unknown id already answers and no route
// invents a second refusal. The runner creates one agent (an always_allow
// toolset, bash included) and one environment, both with internal = true, and
// their ids are not secret: a dream's pipeline session renders agent.id and
// environment_id to any viewer. So the resolvers carry the fragment too, not
// just the id-addressed routes — otherwise any developer key could open a
// session, or point a deployment, at an agent no operator created or can see.
// The runner's own reads are the only ones without it: the insert helpers and
// createSessionIn.internal, which no request can set.
//
// It is unqualified on purpose: `agent_versions` has no such column, so it
// stays unambiguous inside the joins that read a pinned version.
//
// resolveRoster is the one resolver that cannot use it — it looks its members
// up in a batch, where an excluded row and an unknown id are indistinguishable
// — so it reads the column and refuses with the same 404 (roster.go).
const notInternal = ` AND NOT internal`

// requireNotDreamOwned refuses a public mutation of the session a live dream
// owns (§4.4). The pipeline session is listed, readable and streamable like any
// other — the reference exposes it — but the hidden agent behind it runs an
// always_allow toolset the dream steers, so a send, an update, an archive, a
// delete or a resource change from a developer key would run whatever the
// caller liked under the platform's own agent, and under update_existing write
// it into the store the hold exists to protect.
//
// The hold lasts while a dream names the session and has not closed (found
// through dreams_session_idx). Once the closing arm stamps closed_at the gate
// lifts and the routes answer for the session as they do for any archived one.
// The runner writes through none of these handlers, so it needs no bypass.
func requireNotDreamOwned(ctx context.Context, db querier, sessionID string) error {
	var dreamID string
	err := db.QueryRow(ctx,
		`SELECT id FROM dreams WHERE session_id = $1 AND closed_at IS NULL`, sessionID).Scan(&dreamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return errInvalid("session is owned by dream %s", dreamID)
}
