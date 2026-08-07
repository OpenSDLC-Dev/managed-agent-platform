package brain

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// repoMount is the minimal shape of one session resources[] github_repository
// entry the brain injects — the token-free wire object the API stores
// (sessionresources.go's repoResourceJSON). The brain never reads the sealed
// credential row, so the block cannot leak what it never sees.
type repoMount struct {
	ID        string `json:"id"`
	MountPath string `json:"mount_path"`
	Type      string `json:"type"`
	URL       string `json:"url"`
	Checkout  *struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
		Sha  string `json:"sha,omitempty"`
	} `json:"checkout"`

	// failedReason is the reason of the most recent clone failure recorded for
	// this resource, or empty if none was. It is not part of the stored
	// resource — see cloneFailures.
	failedReason string
}

// resolveReposBlock builds the "Mounted repositories" system-prompt block from
// the session's resources[], so the agent can find checkouts that live outside
// its workdir. It returns the block and the number of repositories injected.
//
// Every rendered fact (path, url, checkout) already lives in the stored
// resource, so the only lookup is for what the resource cannot say: whether the
// clone actually landed.
//
// The block is emitted only for cloud environments (plan 25 decision 8):
// nothing materializes repositories on self_hosted — neither this platform nor
// the reference — and telling the model a checkout is present when none is
// would be a false statement, not a harmless omission. The same rule applies
// one repository at a time: a clone the executor could not make is tolerated
// deliberately, leaving the session running with an absent mount, and the
// session.error that records it never reaches the model in replay. Rendered
// unqualified, this block would then assert that checkout for the rest of the
// session and send the model looking down a path that is not there.
func (b *Brain) resolveReposBlock(ctx context.Context, sid domain.ID, resourcesJSON []byte, envKind string) (string, int) {
	if len(resourcesJSON) == 0 || envKind != "cloud" {
		return "", 0
	}
	var mounts []repoMount
	if err := json.Unmarshal(resourcesJSON, &mounts); err != nil {
		slog.WarnContext(ctx, "session repositories not injected", "err", err)
		return "", 0
	}
	kept := make([]repoMount, 0, len(mounts))
	for _, m := range mounts {
		if m.Type != "github_repository" || m.URL == "" || m.MountPath == "" {
			continue
		}
		kept = append(kept, m)
	}
	if len(kept) == 0 {
		return "", 0
	}
	failed := b.cloneFailures(ctx, sid)
	for i := range kept {
		kept[i].failedReason = failed[kept[i].ID]
	}
	return renderReposBlock(kept), len(kept)
}

// cloneFailures returns the most recent clone-failure reason per resource id.
//
// The log is the only record there is: a failure is a session.error, a success
// is not an event at all, and the executor's dedupe means a repetition of the
// same failure is not re-appended. So this answers "the last thing we said out
// loud about this repository", which is why the block hedges rather than
// declares — the executor re-probes and re-clones on every work item, so a
// repository named here may well have landed since.
//
// Best effort by design: a failed lookup logs and leaves the block unqualified,
// which is the shape the block had before this existed and is strictly better
// than failing the turn over prompt metadata.
func (b *Brain) cloneFailures(ctx context.Context, sid domain.ID) map[string]string {
	rows, err := b.pool.Query(ctx,
		`SELECT DISTINCT ON (payload->'error'->>'resource_id')
		        payload->'error'->>'resource_id', payload->'error'->>'reason'
		   FROM events
		  WHERE session_id = $1 AND type = 'session.error'
		    AND payload->'error'->>'type' = 'github_repository_clone_error'
		  ORDER BY payload->'error'->>'resource_id', seq DESC`, sid.String())
	if err != nil {
		slog.WarnContext(ctx, "repository clone failures not read; the block is unqualified",
			"session_id", sid, "err", err)
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			slog.WarnContext(ctx, "a repository clone failure row did not scan", "session_id", sid, "err", err)
			return out
		}
		out[id] = reason
	}
	return out
}

// renderReposBlock formats the repository mounts as a system-prompt block. The
// wording and placement are inferences (docs/DIVERGENCES.md), mirroring the
// files block; the checkout descriptor names the branch, the commit, or the
// repository's default branch, which is what the clone resolved.
func renderReposBlock(mounts []repoMount) string {
	if len(mounts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Mounted repositories. Each repository below is checked out at the given path in your sandbox; work in it with your file and bash tools.\n")
	for _, m := range mounts {
		b.WriteString("\n- ")
		b.WriteString(m.MountPath)
		b.WriteString(" (")
		b.WriteString(m.URL)
		b.WriteString(", ")
		switch {
		case m.Checkout == nil:
			b.WriteString("default branch")
		case m.Checkout.Type == "branch":
			b.WriteString("branch ")
			b.WriteString(m.Checkout.Name)
		case m.Checkout.Type == "commit":
			b.WriteString("commit ")
			b.WriteString(m.Checkout.Sha)
		default:
			b.WriteString("default branch")
		}
		b.WriteString(")")
		if m.failedReason != "" {
			// Hedged rather than flat, and deliberately: the executor retries
			// on the next work item, so this may have landed since the failure
			// was recorded. Telling the model to look is the honest instruction
			// either way.
			b.WriteString(" — NOT AVAILABLE: the last clone attempt failed (")
			b.WriteString(m.failedReason)
			b.WriteString("). The platform retries it on later turns, so check the path before relying on it.")
		}
	}
	return b.String()
}
