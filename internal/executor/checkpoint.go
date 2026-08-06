package executor

// The workspace checkpoint/restore engine (plan 24 slice 4). Capture reads
// the agent's durable state out of a sandbox the reaper is about to destroy —
// the workdir, the persistent shell's cwd/env state, and the published
// deliverables — into one gzipped tar in object storage; restore replays it
// into a fresh sandbox so an idle-reaped session resumes where it left off
// (#28). Both halves run under the session advisory lock: capture inside the
// reaper's hold (slice 5's TTL tier), restore inside provisionSandbox's. In
// this slice the engine's only trigger is its test suite.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	gopath "path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
)

const (
	// MetricCheckpoints counts capture attempts by outcome; the duration
	// histogram shares its name with a ".duration" suffix.
	MetricCheckpoints = "sandbox.checkpoint"
	// MetricRestores counts restore attempts by outcome.
	MetricRestores = "sandbox.restore"
)

// ErrCheckpointTooLarge reports a capture whose members exceed the configured
// budget. The TTL tier treats it as "reap without checkpoint" — an agent must
// not pin its sandbox immortal by filling the disk (plan 24 D8).
var ErrCheckpointTooLarge = errors.New("executor: checkpoint exceeds the configured size budget")

// restoreTarPath is where the checkpoint tar lands inside the sandbox before
// the in-sandbox extraction; /tmp is writable in every hardening shape and is
// deliberately not part of the checkpoint itself.
const restoreTarPath = "/tmp/.map-restore.tar"

// checkpointRoots are the three roots plan 24 D5 preserves: the workdir, the
// persistent shell's per-session state (cwd/env survive a resume), and the
// published deliverables (a resumed session's next harvest must not erase
// them). /tmp is explicitly not preserved; file-resource mounts re-materialize
// from their own store.
func (e *Executor) checkpointRoots(sid domain.ID) []string {
	return []string{
		e.checkpointWorkdir(),
		sandbox.ShellStateRoot + "/" + sid.String(),
		outputsDir,
	}
}

// checkpointWorkdir resolves the workdir the way every other consumer does:
// empty means sandbox.DefaultWorkdir on both sides of the provision.
func (e *Executor) checkpointWorkdir() string {
	if e.cfg.Workdir == "" {
		return sandbox.DefaultWorkdir
	}
	return gopath.Clean(e.cfg.Workdir)
}

// ValidateWorkdir refuses a configured workdir that would alias the
// checkpoint's other roots or its own machinery: a workdir under (or over)
// the shell-state root or /mnt/session double-captures and double-charges the
// shared subtree, and one under /tmp would put the restore staging file — and
// state the checkpoint deliberately drops — inside the archive. cmd/executor
// calls this at startup; a violation is configuration, not data.
func ValidateWorkdir(workdir string) error {
	if workdir == "" {
		return nil
	}
	w := gopath.Clean(workdir)
	if !gopath.IsAbs(w) {
		return fmt.Errorf("workdir %q must be an absolute path", workdir)
	}
	if w == "/" {
		return errors.New(`workdir "/" would checkpoint the whole filesystem`)
	}
	for _, reserved := range []string{"/tmp", sandbox.ShellStateRoot, "/mnt/session"} {
		if w == reserved || strings.HasPrefix(w, reserved+"/") || strings.HasPrefix(reserved, w+"/") {
			return fmt.Errorf("workdir %q overlaps the reserved path %s", workdir, reserved)
		}
	}
	return nil
}

// captureCheckpoint reads the session's durable roots out of its sandbox into
// one gzipped tar in object storage and marks it `ready`. Roots the session
// never used are skipped; any other read failure aborts — a partial
// checkpoint restored later would silently lose state. The caller holds the
// session's advisory lock.
func (e *Executor) captureCheckpoint(ctx context.Context, sid domain.ID) (err error) {
	start := time.Now()
	defer func() { recordCheckpoint(ctx, err, time.Since(start)) }()
	if e.blobs == nil {
		return errors.New("executor: checkpoint capture needs an object store")
	}

	spool, err := os.CreateTemp("", "map-checkpoint-*.tar.gz")
	if err != nil {
		return err
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()
	gz := gzip.NewWriter(spool)
	// The budget meters the framed tar stream itself — every header, padding
	// and trailer byte — because that is precisely what restore's decompressed
	// bound measures. One measure on both sides makes "captured cleanly but
	// arithmetically unrestorable" impossible; charging member content alone
	// would let framing overhead (unbounded across many small members,
	// directories and symlinks, which carry no content at all) push a passing
	// capture over restore's identical cap.
	lw := &limitedWriter{w: gz, remaining: e.cfg.CheckpointMaxBytes}
	tw := tar.NewWriter(lw)

	workdir := e.checkpointWorkdir()
	for _, root := range e.checkpointRoots(sid) {
		rc, err := e.provider.Export(ctx, sid, root)
		if errors.Is(err, sandbox.ErrFileNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("export %s: %w", root, err)
		}
		err = rerootInto(tw, rc, root, root == workdir)
		if err == nil {
			// The export stream's own failure can arrive only AFTER the
			// archive's end-of-archive blocks: tar.Reader stops reading at
			// them, so a K8s tar that exited non-zero behind a
			// syntactically complete archive is visible only by draining
			// the stream to its close error. Without this, a partial
			// export is captured and marked ready as if complete.
			_, err = io.Copy(io.Discard, rc)
		}
		rc.Close()
		if err != nil {
			return fmt.Errorf("capture %s: %w", root, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	size, err := spool.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	key := blob.SessionCheckpointKey(sid.String())
	if err := e.blobs.Put(ctx, key, spool, size, "application/gzip"); err != nil {
		return fmt.Errorf("upload checkpoint: %w", err)
	}
	_, err = e.pool.Exec(ctx,
		`INSERT INTO session_checkpoints (session_id, blob_key, state, taken_at)
		 VALUES ($1, $2, 'ready', now())
		 ON CONFLICT (session_id) DO UPDATE
		 SET blob_key = EXCLUDED.blob_key, state = 'ready', taken_at = now()`,
		sid.String(), key)
	return err
}

// limitedWriter meters the capture's framed tar stream and refuses the byte
// that would exceed the budget — the write path's half of the one-measure
// rule captureCheckpoint documents.
type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.remaining {
		return 0, ErrCheckpointTooLarge
	}
	n, err := l.w.Write(p)
	l.remaining -= int64(n)
	return n, err
}

// rerootInto copies one exported root into the combined checkpoint tar,
// enforcing the walk contract as it goes: member paths must be clean and
// relative with no dot-dot, member types are regular files, directories, and
// symlinks only (anything else — devices, fifos, and hardlinks too — is
// dropped: an agent can mkfifo in its workspace, and a checkpoint that
// refuses to exist over it would let that pin the sandbox; a hardlinked
// name's content survives under the member the archiver stored the bytes on,
// the extra name alone is lost — a conscious trade recorded before the TTL
// tier makes capture routine), and the materialization sentinels are stripped
// so a restored workspace re-materializes skills and files instead of
// trusting a restored, agent-writable marker. Export's contract puts every
// member under one top-level directory named after the root's base name; this
// strips that prefix and re-roots the member at the root's real path.
func rerootInto(tw *tar.Writer, rc io.Reader, root string, stripSentinels bool) error {
	prefix := strings.TrimPrefix(gopath.Clean(root), "/")
	base := gopath.Base(prefix)
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := gopath.Clean(hdr.Name)
		if gopath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("member %q escapes the archive", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		default:
			continue
		}
		var rest string
		switch {
		case name == base:
			rest = ""
		case strings.HasPrefix(name, base+"/"):
			rest = name[len(base)+1:]
		default:
			return fmt.Errorf("member %q outside the export's top-level directory", hdr.Name)
		}
		if stripSentinels && isSentinel(rest) {
			continue
		}
		out := *hdr // shallow copy; Name is rewritten, everything else carried
		// Except the detected wire format: re-rooting lengthens the name, and
		// a member the export encoded as plain ustar would pin the writer to
		// a format whose 100-byte name field the longer path can overflow.
		// Unknown lets the writer pick (PAX when needed).
		out.Format = tar.FormatUnknown
		switch {
		case rest == "" && hdr.Typeflag == tar.TypeDir:
			out.Name = prefix + "/"
		case rest == "":
			// The root itself, but not a directory — an agent can replace a
			// root with a plain file, and a trailing slash on a non-dir
			// member is a malformed archive, not this member's name.
			out.Name = prefix
		default:
			out.Name = prefix + "/" + rest
		}
		if err := tw.WriteHeader(&out); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

// isSentinel reports whether a workdir-relative member is one of the two
// materialization markers: skills/.materialized and .files_materialized.
// Matched by exact relative path, not by base name — an agent's own file that
// happens to share the name elsewhere is workspace state and is preserved.
func isSentinel(rest string) bool {
	return rest == "skills/"+skills.SentinelName || rest == filesSentinelName
}

// checkpointMarker reads the session's restore marker on q (the provision
// lock's own connection — see classifyForReap on why). No row means no
// checkpoint was ever taken.
func checkpointMarker(ctx context.Context, q rowQueryer, sid domain.ID) (state, key string, err error) {
	err = q.QueryRow(ctx,
		`SELECT state, blob_key FROM session_checkpoints WHERE session_id = $1`, sid.String()).
		Scan(&state, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return state, key, err
}

// execer is the one statement shape the marker flip needs, satisfied by both
// the pool and the provision lock's pinned connection.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// restoreCheckpoint replays a ready checkpoint into a fresh sandbox: download,
// decompress under the same byte budget capture enforced, re-validate the
// walk (a blob is storage, not memory — defense against anything that touched
// it), stream the tar in, extract it in-sandbox as the sandbox user (the one
// transfer path that preserves modes and symlinks), and only then flip the
// marker consumed — a crash mid-restore leaves `ready` standing and the next
// provision replaces the half-restored tree (plan 24 D6). The caller holds
// the session's advisory lock and hands in the lock's connection.
func (e *Executor) restoreCheckpoint(ctx context.Context, q execer, sid domain.ID, key string, sb sandbox.Sandbox) (err error) {
	start := time.Now()
	defer func() { recordRestore(ctx, err, time.Since(start)) }()

	rc, _, err := e.blobs.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("fetch checkpoint: %w", err)
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("open checkpoint: %w", err)
	}

	spool, err := os.CreateTemp("", "map-restore-*.tar")
	if err != nil {
		return err
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()
	// Decompressed budget from actual bytes, not the blob's claimed size — the
	// +1 lets an over-budget stream be detected rather than truncated silently.
	n, err := io.Copy(spool, io.LimitReader(gz, e.cfg.CheckpointMaxBytes+1))
	if err != nil {
		return fmt.Errorf("spool checkpoint: %w", err)
	}
	if n > e.cfg.CheckpointMaxBytes {
		return ErrCheckpointTooLarge
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := validateCheckpointTar(spool, e.checkpointRoots(sid)); err != nil {
		return fmt.Errorf("invalid checkpoint: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}

	if err := sb.WriteFileStream(ctx, restoreTarPath, spool, n); err != nil {
		return fmt.Errorf("ship checkpoint: %w", err)
	}
	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Command: "tar -xf " + restoreTarPath + " -C / && rm -f " + restoreTarPath,
	})
	if err != nil {
		return fmt.Errorf("extract checkpoint: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("extract checkpoint: tar exited %d: %s", res.ExitCode, res.Stderr)
	}

	if _, err := q.Exec(ctx,
		`UPDATE session_checkpoints SET state = 'consumed' WHERE session_id = $1`, sid.String()); err != nil {
		return fmt.Errorf("consume checkpoint marker: %w", err)
	}
	return nil
}

// validateCheckpointTar re-walks a spooled checkpoint before anything ships
// into the sandbox: clean relative dot-dot-free paths, only regular files,
// directories, and symlinks, and every member under one of the durable roots
// the capture walk writes (capture never writes anything else, so anything
// else here is tampering, not data — the extraction runs `tar -C /`, and
// without the root restriction a corrupted blob's relative `etc/profile`
// would land on the sandbox's real /etc).
func validateCheckpointTar(r io.Reader, roots []string) error {
	prefixes := make([]string, 0, len(roots))
	for _, root := range roots {
		prefixes = append(prefixes, strings.TrimPrefix(gopath.Clean(root), "/"))
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := gopath.Clean(hdr.Name)
		if gopath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("member %q escapes the extraction root", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		default:
			return fmt.Errorf("member %q has forbidden type %d", hdr.Name, hdr.Typeflag)
		}
		under := false
		for _, p := range prefixes {
			if name == p || strings.HasPrefix(name, p+"/") {
				under = true
				break
			}
		}
		if !under {
			return fmt.Errorf("member %q is outside every checkpoint root", hdr.Name)
		}
	}
}

// recordCheckpoint counts one capture by outcome with its duration.
func recordCheckpoint(ctx context.Context, err error, d time.Duration) {
	recordTransfer(ctx, MetricCheckpoints, "Workspace checkpoints captured by the reaper, by outcome.", err, d)
}

// recordRestore counts one restore by outcome with its duration.
func recordRestore(ctx context.Context, err error, d time.Duration) {
	recordTransfer(ctx, MetricRestores, "Workspace checkpoints restored at provision, by outcome.", err, d)
}

func recordTransfer(ctx context.Context, name, desc string, err error, d time.Duration) {
	outcome := "ok"
	switch {
	case errors.Is(err, ErrCheckpointTooLarge):
		outcome = "too_large"
	case err != nil:
		outcome = "error"
	}
	meter := otel.GetMeterProvider().Meter(meterName)
	counter, cerr := meter.Int64Counter(name, metric.WithDescription(desc))
	if cerr == nil {
		counter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
	hist, herr := meter.Float64Histogram(name+".duration", metric.WithUnit("s"))
	if herr == nil {
		hist.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}
