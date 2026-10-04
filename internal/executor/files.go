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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
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
// fully buffers in the executor. A sentinel records each mount that landed,
// so re-provisioning a live sandbox lands only the mounts it does not still
// hold; per-file failure is logged and tolerated, never fatal to the run. refs come
// from the same locked session read that gated the run (sessionForRun).
func (e *Executor) materializeFiles(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []fileRef, progress func()) {
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
	// The marker records each mount a pass landed, under the file it landed,
	// and a pass lands only the mounts it cannot keep: any the marker does not
	// record under the file the set names there now — new, reassigned, or one
	// an earlier pass failed to land — and any it does that are gone. The
	// rest are the agent's to have edited, and stay. (A pass used to land the
	// whole set whenever one mount was gone or had failed to land, over the
	// agent's edits to every other one.)
	land, kept := mounts, []fileRef(nil)
	if sentinelUsable {
		// A marker that could not be read — the agent made it unreadable, an
		// image's startup pushed it out — or that does not parse records no
		// mount: the whole current set lands and the marker is rewritten
		// from what did. Probing instead and keeping what is there would keep
		// a path's bytes under whatever file the set now names there, with
		// nothing to say which.
		if prev, err := sb.ReadFile(ctx, sentinelPath); err == nil {
			var unknown int
			land, kept, unknown = keptMounts(ctx, sb, prev, mounts, progress)
			if unknown > 0 {
				// The marker says these landed, and a probe that did not
				// answer — an image's startup pushing it out of the output,
				// a failed exec — cannot say one has gone since. Re-streaming
				// them on that would overwrite the agent's edits on every pass
				// (#860), so what is there stays.
				slog.WarnContext(ctx, "file mounts not probed; keeping what the sandbox holds",
					"session_id", sid, "files", unknown)
			}
			// Nothing to land, and nothing recorded that the set no longer
			// names: a mount removed from it is forgotten here, so one added
			// back at its path lands again rather than keep what the agent
			// wrote there in between.
			if len(land) == 0 && bytes.Equal(prev, filesSentinel(kept)) {
				span.SetAttributes(attribute.Bool("files.unchanged", true))
				return
			}
		}
	}

	landed := make([]fileRef, 0, len(land))
	for _, m := range land {
		// Reported at the top of the iteration, not the bottom: every tolerated
		// miss below continues, and the run has moved either way — a mount that
		// took its time and one that was skipped both leave the pass advancing
		// (#383). One mount can be 500 MB, so a per-pass report would make a
		// legitimately large set look silent.
		progress()
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
	// The pass boundary: the report at the top of the loop covers every item but
	// the last one, whose landing would otherwise share a silent interval with
	// the sentinel write behind it — a 500 MB mount and a slow sandbox write are
	// each well inside the budget and together need not be (#383).
	progress()
	span.SetAttributes(attribute.Int("files.materialized", len(landed)))
	// The sentinel records what the sandbox holds: what was kept and what
	// landed. A mount that did not land is not in it, so the next pass tries
	// that one again, and that one alone.
	if !sentinelUsable {
		slog.WarnContext(ctx, "files sentinel skipped: a mount occupies the sentinel path",
			"session_id", sid, "sentinel_path", sentinelPath)
	} else if err := sb.WriteFile(ctx, sentinelPath, filesSentinel(append(kept, landed...))); err != nil {
		slog.WarnContext(ctx, "files sentinel not written", "session_id", sid, "err", err)
	}
}

// keptMounts splits mounts by marker — the sentinel as a pass read it — into
// those this pass lands and those it keeps, with how many of the kept a probe
// could not answer for. A mount is kept where the marker records it under the
// file the set names at its path now, and one probe of every such mount
// (sandbox.ProbeEach, as few execs as their paths fill) does not find it
// gone; one the probe cannot answer for stays as well. A marker that does not
// parse records nothing, and every mount lands.
func keptMounts(ctx context.Context, sb sandbox.Sandbox, marker []byte, mounts []fileRef, progress func()) (land, kept []fileRef, unknown int) {
	var recorded []fileRef
	if json.Unmarshal(marker, &recorded) != nil {
		return mounts, nil, 0
	}
	landedAs := make(map[string]string, len(recorded))
	for _, r := range recorded {
		landedAs[r.MountPath] = r.FileID
	}
	// held is the mounts the marker records under their file — never one at
	// a path it does not record, since every mount names a file.
	var held []fileRef
	for _, m := range mounts {
		if landedAs[m.MountPath] == m.FileID {
			held = append(held, m)
		} else {
			land = append(land, m)
		}
	}
	if len(held) == 0 {
		return land, nil, 0
	}
	// The marker read and the probe are separate round trips, and the
	// unchanged pass returns without entering the write loop, so the read
	// reports before the probe rather than the pair counting as one silent
	// step (#383).
	progress()
	for i, p := range sandbox.ProbeEach(ctx, sb, mountPaths(held)...) {
		if p == sandbox.Absent {
			land = append(land, held[i])
			continue
		}
		if p == sandbox.PresenceUnknown {
			unknown++
		}
		kept = append(kept, held[i])
	}
	return land, kept, unknown
}

// materializeFile streams one mount's bytes from object storage to its
// mount_path. The object store's authoritative size drives the streaming write,
// whose own byte accounting rejects a truncated transfer.
func (e *Executor) materializeFile(ctx context.Context, sb sandbox.Sandbox, m fileRef) error {
	// The files row is authoritative for existence, so check it before streaming.
	// A deleted file's object outlives its row until the control plane's drain
	// removes it (api deleteFile enqueues it, #703), so a still-present blob is
	// not proof the file exists — and the brain's resolveFilesBlock already
	// treats a row-less mount as dangling.
	// Mounting the orphan would make the two halves disagree and contradict the
	// documented absent-mount behavior (plan decision 2); check the row so a
	// deleted file is the same dangling miss on both halves.
	// An expired file is the same dangling miss as a deleted one, for the reason
	// above: past expires_at the content route answers 404, so mounting the bytes
	// here would make the platform-managed half serve what the BYOC half refuses
	// (#655, plan 49).
	//
	// The bytes are at the row's key, not at one derived from the id: a
	// session's copy of an upload shares the upload's object (#578).
	var key string
	err := e.pool.QueryRow(ctx,
		`SELECT `+store.FileObjectKeySQL+` FROM files WHERE id = $1 AND `+store.FileLiveSQL, m.FileID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", errFileMissing, m.FileID)
	}
	if err != nil {
		return err
	}
	rc, size, err := e.blobs.Get(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return fmt.Errorf("%w: %s", errFileMissing, m.FileID)
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	return sb.WriteFileStream(ctx, m.MountPath, rc, size)
}

// mountPaths is the mounts' paths, for the presence probe (sandbox.ProbeEach).
func mountPaths(mounts []fileRef) []string {
	paths := make([]string, len(mounts))
	for i, m := range mounts {
		paths[i] = m.MountPath
	}
	return paths
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
