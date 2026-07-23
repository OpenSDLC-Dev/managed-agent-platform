package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// fileRef is the minimal shape of one session resources[] entry the executor
// materializes — the file variant stored by the API (sessionresources.go's
// fileResourceJSON). Non-file resource types (none exist in v1; the git half of
// #55 will add github_repository) are filtered out by Type, never mounted.
type fileRef struct {
	FileID    string `json:"file_id"`
	MountPath string `json:"mount_path"`
	Type      string `json:"type"`
}

// filesSentinelName marks a sandbox whose file mounts are already materialized,
// so re-provisioning a live session's sandbox skips restreaming unchanged mounts.
const filesSentinelName = ".files_materialized"

// errFileMissing classifies a dangling mount — the file's row is gone, or (rarer)
// its object, because a delete raced the reference. Tolerated by design (plan
// decision 2): the mount is skipped and the agent sees an absent path, never a
// failed run.
var errFileMissing = errors.New("file missing")

// materializeFiles streams each mounted file's bytes into the sandbox at its
// mount_path before the tools run — the platform-managed half of file
// materialization, the twin of materializeSkills. Bytes stream straight from
// object storage into the sandbox (WriteFileStream), so a 500 MB mount never
// fully buffers in the executor. A sentinel records the mounted set so
// re-provisioning a live sandbox skips an unchanged, still-present set;
// per-file failure is logged and tolerated, never fatal to the run. refs come
// from the same locked session read that gated the run (sessionForRun).
func (e *Executor) materializeFiles(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []fileRef) {
	mounts := make([]fileRef, 0, len(refs))
	for _, r := range refs {
		if r.Type == "file" && r.FileID != "" && r.MountPath != "" {
			mounts = append(mounts, r)
		}
	}
	if len(mounts) == 0 {
		return
	}
	if e.blobs == nil {
		slog.WarnContext(ctx, "session references file resources but object storage is not configured",
			"session_id", sid, "files", len(mounts))
		return
	}

	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "files_materialize")
	defer span.End()
	start := time.Now()
	defer func() { recordFilesMaterializeDuration(ctx, time.Since(start)) }()
	span.SetAttributes(attribute.Int("files.referenced", len(mounts)))

	workdir := e.cfg.Workdir
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	sentinelPath := path.Join(workdir, filesSentinelName)
	// A mount at the sentinel's own path disables the sentinel for this session: the
	// file owns that path, so the marker is neither trusted for the skip (else
	// marker-equal file bytes — a pre-guard clobber healed on upgrade, or bytes the
	// agent wrote — would wedge the mount) nor written (which would clobber the
	// file). Such a session re-materializes every pass — correct, just unoptimized.
	sentinelUsable := !mountAtPath(mounts, sentinelPath)
	// Skip only when the marker names exactly this mounted set AND every mount
	// still exists — the sandbox filesystem is agent-writable, so a tool call
	// may have deleted a mount the marker still claims. Presence is probed with
	// a shell test, never ReadFile: a 500 MB mount cannot be read back to check
	// it is there.
	marker := filesSentinel(mounts)
	if sentinelUsable {
		if prev, err := sb.ReadFile(ctx, sentinelPath); err == nil &&
			bytes.Equal(prev, marker) && e.mountsPresent(ctx, sb, mounts) {
			span.SetAttributes(attribute.Bool("files.unchanged", true))
			return
		}
	}

	landed := make([]fileRef, 0, len(mounts))
	for _, m := range mounts {
		if err := e.materializeFile(ctx, sb, m); err != nil {
			outcome := fileOutcomeFailed
			if errors.Is(err, errFileMissing) {
				outcome = fileOutcomeNotFound
			}
			recordFileMaterialized(ctx, outcome)
			slog.WarnContext(ctx, "file not materialized",
				"session_id", sid, "file_id", m.FileID, "mount_path", m.MountPath, "err", err)
			continue
		}
		landed = append(landed, m)
		recordFileMaterialized(ctx, fileOutcomeOK)
		slog.InfoContext(ctx, "file materialized",
			"session_id", sid, "file_id", m.FileID, "mount_path", m.MountPath)
	}
	span.SetAttributes(attribute.Int("files.materialized", len(landed)))
	// The sentinel records only what landed: a partial pass (a dangling mount)
	// leaves a marker that never equals the full set, so the next pass re-runs —
	// the skills-registry behavior, carried over.
	if !sentinelUsable {
		slog.WarnContext(ctx, "files sentinel skipped: a mount occupies the sentinel path",
			"session_id", sid, "sentinel_path", sentinelPath)
	} else if err := sb.WriteFile(ctx, sentinelPath, filesSentinel(landed)); err != nil {
		slog.WarnContext(ctx, "files sentinel not written", "session_id", sid, "err", err)
	}
}

// materializeFile streams one mount's bytes from object storage to its
// mount_path. The object store's authoritative size drives the streaming write,
// whose own byte accounting rejects a truncated transfer.
func (e *Executor) materializeFile(ctx context.Context, sb sandbox.Sandbox, m fileRef) error {
	// The files row is authoritative for existence, so check it before streaming.
	// A deleted file leaves its object best-effort (api deleteFile: row gone, blob
	// orphan accepted), so a still-present blob is not proof the file exists — and
	// the brain's resolveFilesBlock already treats a row-less mount as dangling.
	// Mounting the orphan would make the two halves disagree and contradict the
	// documented absent-mount behavior (plan decision 2); check the row so a
	// deleted file is the same dangling miss on both halves.
	var exists bool
	err := e.pool.QueryRow(ctx, `SELECT true FROM files WHERE id = $1`, m.FileID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", errFileMissing, m.FileID)
	}
	if err != nil {
		return err
	}
	rc, size, err := e.blobs.Get(ctx, blob.FilesKey(m.FileID))
	if errors.Is(err, blob.ErrNotFound) {
		return fmt.Errorf("%w: %s", errFileMissing, m.FileID)
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	return sb.WriteFileStream(ctx, m.MountPath, rc, size)
}

// mountsPresent reports whether every mount path still exists in the sandbox,
// in one exec (test -e chained with &&). A missing sandbox or a failed exec
// reads as "not present", so the caller re-materializes rather than skipping.
func (e *Executor) mountsPresent(ctx context.Context, sb sandbox.Sandbox, mounts []fileRef) bool {
	var cmd strings.Builder
	for _, m := range mounts {
		cmd.WriteString("test -e ")
		cmd.WriteString(shellQuote(m.MountPath))
		cmd.WriteString(" && ")
	}
	cmd.WriteString("true")
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: cmd.String()})
	return err == nil && res.ExitCode == 0
}

// filesSentinel is the marker's content: the mounted set as sorted
// {file_id, mount_path} pairs, so two provisions of the same set produce
// byte-identical markers regardless of resources[] order.
func filesSentinel(mounts []fileRef) []byte {
	type pair struct {
		FileID    string `json:"file_id"`
		MountPath string `json:"mount_path"`
	}
	pairs := make([]pair, 0, len(mounts))
	for _, m := range mounts {
		pairs = append(pairs, pair{FileID: m.FileID, MountPath: m.MountPath})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].FileID != pairs[j].FileID {
			return pairs[i].FileID < pairs[j].FileID
		}
		return pairs[i].MountPath < pairs[j].MountPath
	})
	b, _ := json.Marshal(pairs) // a slice of two-string structs cannot fail to marshal
	return b
}

// shellQuote makes a path a single, literal shell word for the presence probe.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// mountAtPath reports whether any mount targets p (cleaned, so /workspace/./x
// matches /workspace/x) — the guard that keeps the sentinel from overwriting a
// file mounted at the sentinel's own path and then skipping it forever.
func mountAtPath(mounts []fileRef, p string) bool {
	for _, m := range mounts {
		if path.Clean(m.MountPath) == p {
			return true
		}
	}
	return false
}
