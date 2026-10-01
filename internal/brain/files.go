package brain

import (
	"context"
	"encoding/json"
	"log/slog"
	"path"
	"strconv"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
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
// Other resource types are skipped by Type.
type fileMount struct {
	FileID    string `json:"file_id"`
	MountPath string `json:"mount_path"`
	Type      string `json:"type"`
}

// uploadsPointer is the reference's own sentence about file mounts, as the
// model quoted it in the 2026-09-02 recording (probe
// sessF.events.after-ls-after-delete): it lists no file and sends the agent to
// ls (#681).
const uploadsPointer = "User uploads (files uploaded to the session by the user) are available at " +
	"`" + uploadsRoot + "`. Use `ls` on that directory to see available files."

// uploadsRoot is the directory api.resolveMountPath has rooted every file mount
// in since #323. A mount stored before that, by an unreleased build, can lie
// elsewhere and still materializes there, so its path alone is named beside the
// pointer — never a filename, MIME type or size.
const uploadsRoot = "/mnt/session/uploads"

// resolveFilesBlock returns the uploads pointer when the session's resources[]
// hold a live file mount under uploadsRoot, followed by the quoted paths of any
// live mount outside it, or those paths alone when none lies under it, and ""
// when no mount is live — the gating is ours, the recording having shown the
// sentence only in a session with uploads (docs/DIVERGENCES.md). It also
// returns the number of live mounts and the number of misses. Best-effort,
// mirroring resolveSkillsBlock: a dangling mount (its file row gone — the
// delete raced the reference, plan decision 2) or a store error is a logged,
// counted miss, never a failed turn. The executor is what actually writes the
// bytes into the sandbox.
func (b *Brain) resolveFilesBlock(ctx context.Context, resourcesJSON []byte) (string, int, int) {
	if len(resourcesJSON) == 0 {
		return "", 0, 0
	}
	var resources []fileMount
	if err := json.Unmarshal(resourcesJSON, &resources); err != nil {
		slog.WarnContext(ctx, "session resources not injected", "err", err)
		return "", 0, 0
	}
	var mounts []fileMount
	var ids []string
	for _, m := range resources {
		if m.Type == "file" && m.FileID != "" && m.MountPath != "" {
			mounts = append(mounts, m)
			ids = append(ids, m.FileID)
		}
	}
	if len(mounts) == 0 {
		return "", 0, 0
	}
	alive, err := b.liveFiles(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "mounted files not injected", "err", err)
		return "", 0, len(mounts)
	}
	live, misses, rooted := 0, 0, false
	var outside []string
	for _, m := range mounts {
		if !alive[m.FileID] {
			slog.WarnContext(ctx, "mounted file not injected (file gone)",
				"file_id", m.FileID, "mount_path", m.MountPath)
			misses++
			continue
		}
		live++
		if p := path.Clean(m.MountPath); p == uploadsRoot || strings.HasPrefix(p, uploadsRoot+"/") {
			rooted = true
		} else {
			// Quoted, so a newline cannot start a line of its own in the
			// prompt and a comma cannot split one path into two.
			outside = append(outside, strconv.Quote(p))
		}
	}
	switch {
	case live == 0:
		return "", 0, misses
	case len(outside) == 0:
		return uploadsPointer, live, misses
	case !rooted:
		// Nothing is under the uploads directory, so the pointer would send
		// the agent to ls a directory with none of its files in it.
		return "Files are mounted at: " + strings.Join(outside, ", "), live, misses
	}
	return uploadsPointer + "\nFiles are also mounted at: " + strings.Join(outside, ", "), live, misses
}

// liveFiles reports which of ids name a files row that still has content.
// Expired counts as gone here too: the executor no longer materializes such a
// mount, so counting it would point the model at a file that is not there
// (#655, plan 49).
func (b *Brain) liveFiles(ctx context.Context, ids []string) (map[string]bool, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT id FROM files WHERE id = ANY($1) AND `+store.FileLiveSQL, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	alive := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		alive[id] = true
	}
	return alive, rows.Err()
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
