package brain

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// MetricFileResolveMisses counts mounted-file references the brain could not
// resolve to a live file row at injection time — a dangling mount (its file row
// gone, plan decision 2) or a transient store error. The mounted-files twin of
// [MetricSkillResolveMisses]; exported so the telemetry test can assert the name.
const MetricFileResolveMisses = "files.resolve.misses"

// fileMount is the minimal shape of one session resources[] file entry the
// brain injects — file_id + mount_path from the stored fileResourceJSON.
// Non-file types (none in v1) are skipped by Type.
type fileMount struct {
	FileID    string `json:"file_id"`
	MountPath string `json:"mount_path"`
	Type      string `json:"type"`
}

// uploadsPointer is the whole of what the system prompt says about file mounts:
// the reference's own sentence, as the model quoted it in the 2026-09-02
// recording (probe sessF.events.after-ls-after-delete), which lists no file and
// sends the agent to ls (#681). Every file mount created since #323 lands under
// this directory, which api.resolveMountPath roots it in; one stored elsewhere
// before that is materialized but no longer named.
const uploadsPointer = "User uploads (files uploaded to the session by the user) are available at " +
	"`/mnt/session/uploads`. Use `ls` on that directory to see available files."

// resolveFilesBlock returns the uploads pointer when the session's resources[]
// hold at least one live file mount, and "" otherwise — the gating is ours, the
// recording having shown the sentence only in a session with uploads
// (docs/DIVERGENCES.md). It also returns the number of live mounts and the
// number of misses. Best-effort, mirroring resolveSkillsBlock: a dangling mount
// (its file row gone — the delete raced the reference, plan decision 2) or a
// store error is a logged, counted miss, never a failed turn. The executor is
// what actually writes the bytes into the sandbox.
func (b *Brain) resolveFilesBlock(ctx context.Context, resourcesJSON []byte) (string, int, int) {
	if len(resourcesJSON) == 0 {
		return "", 0, 0
	}
	var mounts []fileMount
	if err := json.Unmarshal(resourcesJSON, &mounts); err != nil {
		slog.WarnContext(ctx, "session resources not injected", "err", err)
		return "", 0, 0
	}
	live, misses := 0, 0
	for _, m := range mounts {
		if m.Type != "file" || m.FileID == "" || m.MountPath == "" {
			continue
		}
		// Expired counts as gone here too: the executor no longer materializes
		// such a mount, so counting it would point the model at a file that is
		// not there (#655, plan 49).
		var one int
		err := b.pool.QueryRow(ctx,
			`SELECT 1 FROM files WHERE id = $1 AND `+store.FileLiveSQL, m.FileID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "mounted file not injected (file gone)",
				"file_id", m.FileID, "mount_path", m.MountPath)
			misses++
			continue
		}
		if err != nil {
			slog.WarnContext(ctx, "mounted file not injected", "file_id", m.FileID, "err", err)
			misses++
			continue
		}
		live++
	}
	if live == 0 {
		return "", 0, misses
	}
	return uploadsPointer, live, misses
}

// recordFileResolveMisses adds to the mounted-file resolve-miss counter, the twin
// of recordResolveMisses: the meter is resolved per call and a telemetry error
// just drops the reading, never failing the turn.
func recordFileResolveMisses(ctx context.Context, n int) {
	if n <= 0 {
		return
	}
	c, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricFileResolveMisses,
		metric.WithDescription("Mounted-file references the brain could not resolve for injection."))
	if err != nil {
		return
	}
	c.Add(ctx, int64(n))
}
