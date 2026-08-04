package executor

// The outputs_harvest consumer (docs/plan/21_outcomes.md, Decision 8). When a
// cloud session's turn settles into an outcome-grading cycle, the brain
// enqueues one outputs_harvest item instead of the grading turn directly; this
// file walks /mnt/session/outputs/ in the session's sandbox, publishes the
// tree into the files registry as a per-path snapshot, and chains the grading
// model_turn — so the grader (and, through GET /v1/files, the caller) sees the
// deliverables as they stood when the cycle began. The kind is internal: Poll
// never serves it, so nothing of this appears on the worker wire.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"go.opentelemetry.io/otel/codes"
)

// outputsDir is where a session's deliverables live inside the sandbox: files
// the agent leaves under it are harvested into the files registry when a
// grading cycle begins. The path matches the reference's documented outputs
// directory.
const outputsDir = "/mnt/session/outputs"

// harvestListScript enumerates the regular files under outputsDir as
// NUL-separated relative paths (NUL because a filename can carry anything else,
// newlines included). dotglob includes dotfiles, nullglob makes an empty tree
// an empty listing, and a missing directory — the agent never created it — is
// an empty snapshot, not an error. Symlinks are skipped: eligible means a
// regular file, and following links could pull in bytes from outside the tree.
// Bash at /bin/bash is the sandbox contract's guarantee (Exec runs under it),
// and globstar needs nothing newer than the contract's bash. LC_ALL=C pins the
// glob's emission to byte order whatever locale the image defaults to — the
// truncation degradation keeps the listing's complete-entry prefix, and that
// prefix is only the lexicographic one (Go's sort.Strings order) under C
// collation.
const harvestListScript = `export LC_ALL=C
cd ` + outputsDir + ` 2>/dev/null || exit 0
shopt -s globstar dotglob nullglob
for f in **/*; do
  if [ -f "$f" ] && [ ! -L "$f" ]; then printf '%s\0' "$f"; fi
done`

// The harvest caps (Decision 8): applied greedily in lexicographic path order —
// an over-cap file is excluded and the walk continues, so a given tree always
// yields the same snapshot. Ineligible or excluded means absent from the
// registry, never an error. Vars rather than consts so the caps test can
// shrink them.
var (
	harvestFileCapBytes    int64 = 50 << 20
	harvestSessionCapBytes int64 = 500 << 20
	harvestSessionCapFiles       = 200
)

// harvestFile is one staged deliverable: its bytes already sit in the blob
// store at blob.FilesKey(id) and settleHarvest decides whether the registry
// row appears (publish) or the object is deleted (discard).
type harvestFile struct {
	path string
	id   domain.ID
	size int64
	mime string
}

// processHarvest runs one outputs_harvest item: stage every eligible file's
// bytes first, then publish the snapshot, chain the grading turn, and complete
// the item in one transaction. Any fault leaves the item for reclaim with the
// previous snapshot intact — the registry moves only when a whole new snapshot
// commits.
func (e *Executor) processHarvest(ctx context.Context, item *queue.Item) (err error) {
	ctx, span := consumerSpan(ctx, item, "outputs_harvest")
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			e.report(ctx, item, err)
		}
		span.End()
	}()

	sess, live, err := e.sessionForRun(ctx, item)
	if err != nil || !live {
		return err
	}

	if e.blobs == nil {
		// A storage-less deploy cannot publish deliverables (the same posture
		// as materializeFiles): skip the snapshot — leaving any previous one
		// untouched, since there is nothing to replace it with — but still
		// chain the grading turn, so the outcome settles transcript-only
		// instead of stalling the session.
		slog.WarnContext(ctx, "executor: no blob store configured; outputs harvest skipped, grading proceeds transcript-only",
			"session", item.SessionID)
		return e.settleHarvest(ctx, item, nil, false)
	}

	// Keep the lease from provisioning through the reads: a large tree can
	// outlast a fixed TTL, and losing the lease mid-stage cancels the work.
	kctx, keeper := e.queue.KeepLease(ctx, item, e.cfg.LeaseTTL)
	files, runErr := e.collectOutputs(kctx, item, sess)
	if kerr := keeper.Close(); kerr != nil {
		e.discardStaged(ctx, files)
		return fmt.Errorf("lease keeper: %w", kerr)
	}
	if runErr != nil {
		return runErr
	}
	return e.settleHarvest(ctx, item, files, true)
}

// collectOutputs lists the outputs tree and stages every admitted file's bytes
// into the blob store at its final key. On error the staged objects are
// deleted — a snapshot either stages whole or leaves no residue.
func (e *Executor) collectOutputs(ctx context.Context, item *queue.Item, sess sessionRun) (_ []harvestFile, err error) {
	var staged []harvestFile
	defer func() {
		if err != nil {
			// Not ctx itself: when the fault is the lease keeper cancelling
			// this context, the discard must still reach the blob store.
			e.discardStaged(context.WithoutCancel(ctx), staged)
		}
	}()

	sb, err := e.provisionSandbox(ctx, item.SessionID, sess)
	if err != nil {
		return nil, err
	}
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: harvestListScript})
	if err != nil {
		return nil, fmt.Errorf("list outputs: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list outputs: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	listing := res.Stdout
	if res.Truncated {
		// The exec cap cut the listing. The glob emits paths sorted, so its
		// complete entries are the tree's lexicographic prefix — what greedy
		// admission takes first anyway. Keep them and drop the trailing
		// mid-path fragment: the tree is static during grading, so a fault
		// here would repeat identically on every reclaim and wedge the
		// session, where a partial snapshot (logged) still grades.
		listing = listing[:strings.LastIndexByte(listing, 0)+1]
		slog.WarnContext(ctx, "executor: outputs listing exceeded the exec output cap; snapshot degrades to its sorted prefix",
			"session", item.SessionID)
	}
	paths, rejected := parseListing(listing)
	for _, p := range rejected {
		slog.WarnContext(ctx, "executor: outputs listing produced an invalid path; excluded",
			"session", item.SessionID, "path", p)
	}

	var total int64
	for _, p := range paths {
		if len(staged) >= harvestSessionCapFiles {
			// No later path can be admitted once the count cap is hit — one
			// warning for the rest, not one per path.
			slog.WarnContext(ctx, "executor: remaining outputs excluded, session file-count cap reached",
				"session", item.SessionID, "first_excluded", p, "cap", harvestSessionCapFiles)
			break
		}
		rc, size, rerr := sb.ReadFileStream(ctx, outputsDir+"/"+p, harvestFileCapBytes)
		if rerr != nil {
			// The path sentinels are the "ineligible = absent" arm: too large,
			// vanished since the listing, or not a regular file after all.
			// Anything else is a backend fault that aborts the harvest.
			if errors.Is(rerr, sandbox.ErrFileTooLarge) || errors.Is(rerr, sandbox.ErrFileNotExist) ||
				errors.Is(rerr, sandbox.ErrIsDirectory) || errors.Is(rerr, sandbox.ErrNotRegularFile) {
				slog.WarnContext(ctx, "executor: output ineligible; excluded from the snapshot",
					"session", item.SessionID, "path", p, "reason", rerr)
				continue
			}
			return nil, fmt.Errorf("read output %s: %w", p, rerr)
		}
		if total+size > harvestSessionCapBytes {
			_ = rc.Close()
			slog.WarnContext(ctx, "executor: output excluded, session byte cap would be exceeded",
				"session", item.SessionID, "path", p, "size", size, "cap", harvestSessionCapBytes)
			continue
		}
		id := domain.NewID(domain.PrefixFile)
		m := harvestMime(p)
		perr := e.blobs.Put(ctx, blob.FilesKey(id.String()), rc, size, m)
		_ = rc.Close()
		if perr != nil {
			return nil, fmt.Errorf("stage output %s: %w", p, perr)
		}
		staged = append(staged, harvestFile{path: p, id: id, size: size, mime: m})
		total += size
	}
	return staged, nil
}

// settleHarvest is the harvest's single registry transaction: under the
// session row lock, replace the session's snapshot rows, enqueue the grading
// model_turn, and complete the item — all or nothing. The fence is the outcome
// entry still being `evaluating`: a cycle that settled while the harvest ran
// (a user.interrupt flips the entry and cancels the queue in its own
// transaction) wins outright, mirroring the brain's settleVerdict rule —
// nothing of this harvest commits and the staged bytes are discarded.
func (e *Executor) settleHarvest(ctx context.Context, item *queue.Item, files []harvestFile, replace bool) (err error) {
	published := false
	defer func() {
		if !published {
			e.discardStaged(ctx, files)
		}
	}()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var evalsJSON []byte
	if err := tx.QueryRow(ctx,
		`SELECT outcome_evaluations FROM sessions WHERE id = $1 FOR UPDATE`,
		item.SessionID.String()).Scan(&evalsJSON); err != nil {
		return err
	}
	var evals []domain.OutcomeEvaluation
	if len(evalsJSON) > 0 {
		if err := json.Unmarshal(evalsJSON, &evals); err != nil {
			return fmt.Errorf("decode stored outcome_evaluations: %w", err)
		}
	}
	if active, ok := events.ActiveOutcome(evals); !ok || active.Result != domain.OutcomeResultEvaluating {
		if err := e.queue.Complete(ctx, tx, item); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	// The snapshot is keyed per path (the files_scope_filename_idx unique
	// index): delete-all + insert-all under the lock replaces changed paths,
	// drops vanished ones, and keeps the whole move atomic.
	var oldIDs []string
	if replace {
		rows, qerr := tx.Query(ctx,
			`DELETE FROM files WHERE scope_type = 'session' AND scope_id = $1 RETURNING id`,
			item.SessionID.String())
		if qerr != nil {
			return qerr
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			oldIDs = append(oldIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, f := range files {
			if _, err := tx.Exec(ctx,
				`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
				 VALUES ($1, $2, $3, $4, true, 'session', $5)`,
				f.id.String(), f.path, f.mime, f.size, item.SessionID.String()); err != nil {
				return err
			}
		}
	}
	if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, queue.ModelTurn); err != nil {
		return err
	}
	if err := e.queue.Complete(ctx, tx, item); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	published = true

	// The replaced snapshot's bytes went unreferenced the moment the deletes
	// committed; removing the objects is best-effort residue cleanup, exactly
	// like a discard.
	for _, id := range oldIDs {
		if derr := e.blobs.Delete(ctx, blob.FilesKey(id)); derr != nil {
			slog.WarnContext(ctx, "executor: could not delete a replaced deliverable blob",
				"session", item.SessionID, "file", id, "error", derr)
		}
	}
	return nil
}

// discardStaged deletes staged blobs for a snapshot that will never publish.
// Best-effort: a failure leaves an orphaned object — storage residue nothing
// references again — which is logged, not escalated.
func (e *Executor) discardStaged(ctx context.Context, files []harvestFile) {
	for _, f := range files {
		if err := e.blobs.Delete(ctx, blob.FilesKey(f.id.String())); err != nil {
			slog.WarnContext(ctx, "executor: could not delete a staged deliverable blob",
				"file", f.id, "error", err)
		}
	}
}

// parseListing splits the NUL-separated listing into validated, de-duplicated,
// lexicographically sorted relative paths, returning the rejects separately so
// the caller can log them — an exclusion is never silent.
func parseListing(out string) (paths, rejected []string) {
	seen := make(map[string]bool)
	for _, p := range strings.Split(out, "\x00") {
		if p == "" {
			continue
		}
		if !validHarvestPath(p) {
			rejected = append(rejected, p)
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, rejected
}

// harvestForbiddenChars is the Files filename rule's forbidden set
// (internal/api's validateFilename) minus the path separators: "/" is
// structural here — the recorded divergence (docs/DIVERGENCES.md) — and a
// backslash stays rejected.
const harvestForbiddenChars = `<>:"|?*\`

// validHarvestPath accepts only a clean relative path that stays inside the
// outputs tree. The listing script emits exactly that shape, but the sandbox
// is agent-writable and the listing runs in it — a forged path must not become
// a registry filename or a read outside the tree. Invalid UTF-8 is rejected
// too: a POSIX filename is arbitrary bytes, and one stray byte at the
// filename's text-column bind would fault the settlement and loop the reclaim
// (the #135 class).
func validHarvestPath(p string) bool {
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "/") || !utf8.ValidString(p) {
		return false
	}
	if clean := path.Clean(p); clean != p || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if !validHarvestSegment(seg) {
			return false
		}
	}
	return true
}

// validHarvestSegment holds each path segment to the platform's own Files
// filename rule (validateFilename: 1–255 runes, none of the Windows-reserved
// set, no U+0000–U+001F) — a harvested filename the upload endpoint would
// reject must not appear on the Files wire. An ineligible name excludes the
// file (logged by the caller), never faults the harvest.
func validHarvestSegment(seg string) bool {
	if utf8.RuneCountInString(seg) > 255 || strings.ContainsAny(seg, harvestForbiddenChars) {
		return false
	}
	for _, r := range seg {
		if r < 0x20 {
			return false
		}
	}
	return true
}

// harvestMimeByExt is the fixed extension → mime table harvestMime consults —
// and the only thing it consults: mime.TypeByExtension merges the host's
// /etc/mime.types, so the same tree would yield different registry rows on
// different executor hosts (#264). The table is a pinned snapshot of Go 1.26's
// builtin table (mime.builtinTypesLower — copied, not referenced, so a
// toolchain upgrade cannot change registry rows either) plus a bounded set of
// textual deliverable types that table lacks. The grader's inline rule
// (text/*, application/json — inlineableMime, internal/brain/grader.go) reads
// these values, so the textual additions carry text/* deliberately (.tar, the
// one binary addition, keeps application/x-tar): an entry's value decides
// whether that deliverable's content reaches the grader. Unlisted
// extensions fall back to application/octet-stream on every host; extend the
// table when a real deliverable class needs it.
var harvestMimeByExt = map[string]string{
	// Go 1.26 mime.builtinTypesLower, verbatim:
	".ai":    "application/postscript",
	".apk":   "application/vnd.android.package-archive",
	".apng":  "image/apng",
	".avif":  "image/avif",
	".bin":   "application/octet-stream",
	".bmp":   "image/bmp",
	".com":   "application/octet-stream",
	".css":   "text/css; charset=utf-8",
	".csv":   "text/csv; charset=utf-8",
	".doc":   "application/msword",
	".docx":  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".ehtml": "text/html; charset=utf-8",
	".eml":   "message/rfc822",
	".eps":   "application/postscript",
	".exe":   "application/octet-stream",
	".flac":  "audio/flac",
	".gif":   "image/gif",
	".gz":    "application/gzip",
	".htm":   "text/html; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".ico":   "image/vnd.microsoft.icon",
	".ics":   "text/calendar; charset=utf-8",
	".jfif":  "image/jpeg",
	".jpeg":  "image/jpeg",
	".jpg":   "image/jpeg",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json",
	".m4a":   "audio/mp4",
	".mjs":   "text/javascript; charset=utf-8",
	".mp3":   "audio/mpeg",
	".mp4":   "video/mp4",
	".oga":   "audio/ogg",
	".ogg":   "audio/ogg",
	".ogv":   "video/ogg",
	".opus":  "audio/ogg",
	".pdf":   "application/pdf",
	".pjp":   "image/jpeg",
	".pjpeg": "image/jpeg",
	".png":   "image/png",
	".ppt":   "application/vnd.ms-powerpoint",
	".pptx":  "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".ps":    "application/postscript",
	".rdf":   "application/rdf+xml",
	".rtf":   "application/rtf",
	".shtml": "text/html; charset=utf-8",
	".svg":   "image/svg+xml",
	".text":  "text/plain; charset=utf-8",
	".tif":   "image/tiff",
	".tiff":  "image/tiff",
	".txt":   "text/plain; charset=utf-8",
	".vtt":   "text/vtt; charset=utf-8",
	".wasm":  "application/wasm",
	".wav":   "audio/wav",
	".webm":  "audio/webm",
	".webp":  "image/webp",
	".xbl":   "text/xml; charset=utf-8",
	".xbm":   "image/x-xbitmap",
	".xht":   "application/xhtml+xml",
	".xhtml": "application/xhtml+xml",
	".xls":   "application/vnd.ms-excel",
	".xlsx":  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".xml":   "text/xml; charset=utf-8",
	".xsl":   "text/xml; charset=utf-8",
	".zip":   "application/zip",
	// Textual deliverable types the builtin table lacks — text/* so the
	// grader inlines them (see the note above), plus .tar for symmetry with
	// the builtin .gz/.zip:
	".ini":   "text/plain; charset=utf-8",
	".jsonl": "text/plain; charset=utf-8",
	".log":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".py":    "text/x-python; charset=utf-8",
	".rst":   "text/x-rst; charset=utf-8",
	".sql":   "text/x-sql; charset=utf-8",
	".tar":   "application/x-tar",
	".tex":   "text/x-tex; charset=utf-8",
	".toml":  "text/x-toml; charset=utf-8",
	".tsv":   "text/tab-separated-values; charset=utf-8",
	".yaml":  "text/yaml; charset=utf-8",
	".yml":   "text/yaml; charset=utf-8",
}

// harvestMime derives the registry mime type from the path's extension alone —
// the sandbox offers no content type. Unknown extensions fall back to
// application/octet-stream, the upload endpoint's own fallback.
func harvestMime(p string) string {
	if m := harvestMimeByExt[strings.ToLower(path.Ext(p))]; m != "" {
		return m
	}
	return "application/octet-stream"
}
