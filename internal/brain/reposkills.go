package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

// repoSkillsFor returns the repository skills this turn's agent is told about
// (plan 65 decisions 2 and 4), and whether the turn must first wait for the
// executor to scan for them. None are told on a self-hosted session, which
// mounts no repository, on a session that mounts none, or to an agent whose
// read tool is off, as the reference announces none then (toolset.ReadUsable,
// the test the API applies to a file added to a session). The record is read
// from Postgres on every turn, so a replayed brain renders the same block;
// its entries come in the session's resource order.
//
// A session is held once. A turn woken with nothing recorded goes ahead
// without the list, which joins the prompt at the first request after a run
// records it: a run that stalled wakes the turn without a record, and so do
// an interrupt that stopped the held run and an executor that predates the
// scan, and holding again would cycle the turn through the executor for as
// long as any of them lasts.
func (b *Brain) repoSkillsFor(ctx context.Context, sid domain.ID, resourcesJSON []byte, envKind string, agent domain.ResolvedAgent) (found []skills.RepoSkill, wait bool, err error) {
	if envKind != string(domain.EnvCloud) {
		return nil, false, nil
	}
	ids := mountedRepositories(resourcesJSON)
	if len(ids) == 0 {
		return nil, false, nil
	}
	if usable, _ := toolset.ReadUsable(agent.Tools); !usable {
		return nil, false, nil
	}
	var raw []byte
	var held bool
	if err := b.pool.QueryRow(ctx,
		`SELECT repo_skills, repo_skills_held_at IS NOT NULL FROM sessions WHERE id = $1`,
		sid.String()).Scan(&raw, &held); err != nil {
		return nil, false, err
	}
	if raw == nil {
		return nil, !held, nil
	}
	var recorded map[string][]skills.RepoSkill
	if err := json.Unmarshal(raw, &recorded); err != nil {
		// The executor wrote this, so a decode failure is corruption, and
		// waiting would not cure it: the turn runs without the block.
		slog.WarnContext(ctx, "repository skills not injected", "session_id", sid.String(), "err", err)
		return nil, false, nil
	}
	for _, id := range ids {
		found = append(found, recorded[id]...)
	}
	return found, false, nil
}

// mountedRepositories is the ids of the github_repository resources the
// session mounts, in resource order, by the test the repositories block
// applies (resolveReposBlock) and the one the executor clones by.
func mountedRepositories(resourcesJSON []byte) []string {
	var mounts []repoMount
	if len(resourcesJSON) == 0 || json.Unmarshal(resourcesJSON, &mounts) != nil {
		return nil
	}
	var ids []string
	for _, m := range mounts {
		if m.Type == "github_repository" && m.ID != "" && m.URL != "" && m.MountPath != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// suspendForRepoSkills holds a turn until the session's repositories are
// scanned for skills, as suspendForDiscovery holds one for an MCP listing: in
// one commit it marks the session held, completes the model turn and enqueues
// the session's tool_exec, whose run provisions the sandbox, clones, records
// the scan, finds no tool to run and wakes the thread (internal/executor's
// settleDrain). It appends no event: the reference leaves no trace of the
// scan, and waiting for it is not part of the conversation.
func (b *Brain) suspendForRepoSkills(ctx context.Context, sid domain.ID, item *queue.Item) error {
	slog.InfoContext(ctx, "brain: turn suspended for repository skill discovery", "session_id", sid.String())
	_, err := b.log.AppendWith(ctx, sid, nil, events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			if err := b.queue.Complete(ctx, tx, item); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE sessions SET repo_skills_held_at = now() WHERE id = $1 AND repo_skills_held_at IS NULL`,
				sid.String()); err != nil {
				return err
			}
			_, err := b.queue.Enqueue(ctx, tx, item.EnvironmentID, sid, queue.ToolExec)
			return err
		},
	})
	if err != nil {
		return fmt.Errorf("suspend for repository skills: %w", err)
	}
	return nil
}
