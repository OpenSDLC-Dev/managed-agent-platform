package worker

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

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// fileRef is one session resources[] file mount — the file variant the API
// stores (sessionresources.go's fileResourceJSON), of which materialization
// needs only the id and mount path. Unlike skills (nested under agent), file
// resources are top-level in the session render. Non-file resource types are
// filtered out by Type.
type fileRef struct {
	FileID    string `json:"file_id"`
	MountPath string `json:"mount_path"`
	Type      string `json:"type"`
}

// filesSentinelName marks a sandbox whose file mounts are already materialized —
// the executor's twin, so re-provisioning a live BYOC session skips restreaming
// an unchanged, still-present set. The format matches the executor's by
// construction; the two never share a sandbox (a session runs on cloud OR
// self_hosted), so the sentinels never meet.
const filesSentinelName = ".files_materialized"

// SetupFiles is the BYOC-worker twin of the executor's materializeFiles: it reads
// the session's file mounts over the wire and streams each file's bytes from GET
// /v1/files/{id}/content (the environment-key content lane) into the sandbox at
// its mount_path, recording the same sentinel and metrics. Wire-only — no
// database, no object store: the control plane's environment-scoped lane is the
// authority on which files this environment may read, so a file no session in the
// environment mounts answers 404 and is tolerated as a not_found miss. Only the
// session read is fatal; a per-file failure is logged, counted, and skipped,
// never failing the run.
func SetupFiles(ctx context.Context, client sdk.Client, sessionID string, sb sandbox.Sandbox, workdir string) error {
	sess, err := client.Beta.Sessions.Get(ctx, sessionID, sdk.BetaSessionGetParams{})
	if err != nil {
		return fmt.Errorf("read session for files: %w", err)
	}
	var snapshot struct {
		Resources []fileRef `json:"resources"`
	}
	if err := json.Unmarshal([]byte(sess.RawJSON()), &snapshot); err != nil {
		return fmt.Errorf("parse session for files: %w", err)
	}
	mounts := make([]fileRef, 0, len(snapshot.Resources))
	for _, r := range snapshot.Resources {
		if r.Type == "file" && r.FileID != "" && r.MountPath != "" {
			mounts = append(mounts, r)
		}
	}
	if len(mounts) == 0 {
		return nil
	}

	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "files_materialize")
	defer span.End()
	start := time.Now()
	defer func() { recordFilesMaterializeDuration(ctx, time.Since(start)) }()
	span.SetAttributes(attribute.Int("files.referenced", len(mounts)))

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
	// Skip only when the marker names exactly this mounted set AND every mount is
	// still present (a shell test, never a read-back — a mount can be 500 MB), the
	// executor's rule.
	marker := filesSentinel(mounts)
	if sentinelUsable {
		if prev, err := sb.ReadFile(ctx, sentinelPath); err == nil &&
			bytes.Equal(prev, marker) && mountsPresent(ctx, sb, mounts) {
			span.SetAttributes(attribute.Bool("files.unchanged", true))
			return nil
		}
	}

	landed := make([]fileRef, 0, len(mounts))
	for _, m := range mounts {
		if err := materializeFile(ctx, client, sb, m); err != nil {
			skipFile(ctx, sessionID, m, err)
			continue
		}
		landed = append(landed, m)
		recordFileMaterialized(ctx, fileOutcomeOK)
		slog.InfoContext(ctx, "file materialized",
			"session_id", sessionID, "file_id", m.FileID, "mount_path", m.MountPath)
	}
	span.SetAttributes(attribute.Int("files.materialized", len(landed)))
	// The sentinel records only what landed, so a partial pass (a dangling mount)
	// leaves a marker that never equals the full set and the next pass re-runs.
	if !sentinelUsable {
		slog.WarnContext(ctx, "files sentinel skipped: a mount occupies the sentinel path",
			"session_id", sessionID, "sentinel_path", sentinelPath)
	} else if err := sb.WriteFile(ctx, sentinelPath, filesSentinel(landed)); err != nil {
		slog.WarnContext(ctx, "files sentinel not written", "session_id", sessionID, "err", err)
	}
	return nil
}

// materializeFile streams one mount's bytes from the file-content lane to its
// mount_path. The response's Content-Length drives the streaming write, so a
// large mount never fully buffers in the worker.
func materializeFile(ctx context.Context, client sdk.Client, sb sandbox.Sandbox, m fileRef) error {
	resp, err := client.Beta.Files.Download(ctx, m.FileID, sdk.BetaFileDownloadParams{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return sb.WriteFileStream(ctx, m.MountPath, resp.Body, resp.ContentLength)
}

// skipFile classifies and logs a tolerated per-file failure — the skipSkill twin:
// a 404 (the file is gone, or this environment does not mount it) is not_found,
// anything else failed.
func skipFile(ctx context.Context, sessionID string, m fileRef, err error) {
	outcome := fileOutcomeFailed
	var apierr *sdk.Error
	if errors.As(err, &apierr) && apierr.StatusCode == 404 {
		outcome = fileOutcomeNotFound
	}
	recordFileMaterialized(ctx, outcome)
	slog.WarnContext(ctx, "file not materialized",
		"session_id", sessionID, "file_id", m.FileID, "mount_path", m.MountPath, "err", err)
}

// mountsPresent reports whether every mount path still exists in the sandbox, in
// one exec (test -e chained with &&) — the executor's presence probe. A missing
// sandbox or a failed exec reads as "not present", so the caller re-materializes.
func mountsPresent(ctx context.Context, sb sandbox.Sandbox, mounts []fileRef) bool {
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
// {file_id, mount_path} pairs, byte-identical to the executor's filesSentinel, so
// two provisions of the same set produce the same marker regardless of order.
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

// shellQuote makes a path a single literal shell word for the presence probe.
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
