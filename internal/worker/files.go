package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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
func SetupFiles(ctx context.Context, client sdk.Client, sessionID string, sb sandbox.Sandbox, workdir string, progress func()) error {
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
		if prev, err := sb.ReadFile(ctx, sentinelPath); err == nil && bytes.Equal(prev, marker) {
			// The marker read and the presence exec are two round trips, and the
			// skip returns without entering the write loop — the executor's
			// rule, so the pair is not one silent step (#383).
			progress()
			if mountsPresent(ctx, sb, mounts) {
				span.SetAttributes(attribute.Bool("files.unchanged", true))
				return nil
			}
		}
	}

	landed := make([]fileRef, 0, len(mounts))
	for _, m := range mounts {
		// Per mount, at the top of the iteration — the executor's rule
		// (internal/executor/files.go): a tolerated miss continues, and one
		// mount can be 500 MB, so a per-pass report would make a legitimately
		// large set look silent to the lease loop's stall guard (#383).
		progress()
		if err := materializeFile(ctx, client, sb, m); err != nil {
			skipFile(ctx, sessionID, m, err)
			continue
		}
		landed = append(landed, m)
		recordFileMaterialized(ctx, fileOutcomeOK)
		slog.InfoContext(ctx, "file materialized",
			"session_id", sessionID, "file_id", m.FileID, "mount_path", m.MountPath)
	}
	// The pass boundary: the report at the top of the loop covers every item but
	// the last one, whose landing would otherwise share a silent interval with
	// the sentinel write behind it — a 500 MB mount and a slow sandbox write are
	// each well inside the budget and together need not be (#383).
	progress()
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

// maxSpoolBytes bounds the temp file a length-less download is spooled to. It
// is not a policy — the upload cap is the authority on how large a mounted file
// may be — only the guard that keeps a body which never declares its end from
// filling the worker's disk, so it is sized to the Files API's own per-file cap
// and refuses nothing a legitimate mount can be. A package var, not a const, so
// a test can lower it and exercise the refusal without half a gigabyte of body.
var maxSpoolBytes int64 = 500 << 20

// materializeFile streams one mount's bytes from the file-content lane to its
// mount_path. The response's Content-Length drives the streaming write, so a
// large mount never fully buffers in the worker — but a length is exactly what
// an intermediary can take away: Go reports -1 for a chunked body and for one it
// decompressed on the way, and -1 is not a byte count any stream can match, so
// passing it through refused every mount (#386). The worker cannot ask anyone
// for the true count — the session resource carries no size (it mirrors the
// reference's file resource, which has none) and the environment key's file lane
// admits only /v1/files/{id}/content, never the metadata read (the API's
// isFileReadPath) — so it measures the bytes itself, spooling them to disk
// rather than to memory because a mount can be 500 MB.
//
// What that measurement cannot do is check the transfer against anything: the
// count comes from the bytes that arrived, so the seam's short-stream guard has
// nothing independent left to compare. Two of the three ways a length can go
// missing carry their own end-of-message — chunked framing errors on a missing
// terminator, and a decompressed body fails its checksum — and both surface as
// a copy error here. The third, a body delimited only by the connection
// closing, cannot be told from a complete one by any client, so a mount behind
// such a hop can land short. That is still strictly better than the refusal it
// replaces, and it is the reason this platform's own control plane always
// declares a length.
func materializeFile(ctx context.Context, client sdk.Client, sb sandbox.Sandbox, m fileRef) error {
	resp, err := client.Beta.Files.Download(ctx, m.FileID, sdk.BetaFileDownloadParams{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 {
		return sb.WriteFileStream(ctx, m.MountPath, resp.Body, resp.ContentLength)
	}
	spool, size, err := spoolBody(resp.Body)
	if err != nil {
		return err
	}
	defer spool.Close()
	return sb.WriteFileStream(ctx, m.MountPath, spool, size)
}

// spoolBody copies a body of undeclared length to a temp file and returns it
// rewound, with the count the write seam requires. The +1 on the budget lets an
// over-budget body be detected rather than truncated into a silently short
// mount — the executor's checkpoint spool measures the same way.
//
// The file is unlinked the moment it exists, so the only thing that holds it is
// the descriptor and closing it is all any path has to do. Unlike the executor's
// spools, which run on machines this platform operates, this one holds a
// customer's file bytes on a customer's own worker: a kill between create and
// cleanup — an eviction, an OOM, a rolling redeploy — would otherwise leave up
// to 500 MB of them in /tmp with nothing that ever sweeps it. Where the spool
// lands is $TMPDIR, the ordinary lever for an operator whose /tmp is a small
// tmpfs; no knob of our own, because Go already reads that one.
func spoolBody(body io.Reader) (*os.File, int64, error) {
	spool, err := os.CreateTemp("", "map-mount-*")
	if err != nil {
		return nil, 0, err
	}
	if err := os.Remove(spool.Name()); err != nil {
		spool.Close()
		return nil, 0, fmt.Errorf("unlink file spool: %w", err)
	}
	n, err := io.Copy(spool, io.LimitReader(body, maxSpoolBytes+1))
	if err != nil {
		spool.Close()
		return nil, 0, fmt.Errorf("spool file content: %w", err)
	}
	if n > maxSpoolBytes {
		spool.Close()
		return nil, 0, fmt.Errorf("file content exceeds the worker's %d-byte spool budget", maxSpoolBytes)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		spool.Close()
		return nil, 0, fmt.Errorf("rewind file spool: %w", err)
	}
	return spool, n, nil
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
