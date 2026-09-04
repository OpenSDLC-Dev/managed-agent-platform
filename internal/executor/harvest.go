package executor

// The outputs_harvest consumer (docs/plan/21_outcomes.md Decision 8, extended
// by docs/plan/38). A cloud session's brain enqueues one outputs_harvest item
// for either of two reasons: a turn settling into an outcome-grading cycle
// (in place of the grading turn, which this file chains once the snapshot is
// in, so the grader never reads a stale one), or the session simply folding
// idle with no outcome at all (nothing to chain — the snapshot is the point).
// Either way this file walks /mnt/session/outputs/ in the session's sandbox and
// publishes the tree into the files registry as a per-path snapshot, so the
// caller sees the deliverables through GET /v1/files. The work is identical
// whichever trigger scheduled it; only settleHarvest branches. The kind is
// internal: Poll never serves it, so nothing of this appears on the worker
// wire.

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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mimetab"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"go.opentelemetry.io/otel/codes"
)

// outputsDir is where a session's deliverables live inside the sandbox: files
// the agent leaves under it are harvested into the files registry when a
// grading cycle begins, and when the session goes idle. The path matches the
// reference's documented outputs directory.
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
//
// How it reaches the sandbox depends on the trigger (docs/plan/38 decision 8).
// A grading harvest (ChainGrading) provisions one — the grader needs a current
// snapshot, so a session whose sandbox was reaped gets a fresh one. An
// idle-triggered harvest never provisions: it fires on every cloud idle, and
// spinning up a container to walk an empty /mnt/session/outputs on a plain
// text-only session would make ordinary sessions pay for a feature they never
// use. It reuses a sandbox only if one is already live (the session just ran
// tools and wrote deliverables); with none it settles as a no-op, publishing
// nothing and replacing nothing.
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
		// settle, so a grading cycle chains its turn and runs transcript-only
		// instead of stalling the session, and an idle-triggered pass simply
		// completes.
		//
		// A grading harvest expected storage to be there, so its skip is a
		// warning an operator should see; an idle harvest fires on every cloud
		// idle, so on a blob-less deploy it is the ordinary case, not a
		// misconfiguration — log it at debug (item 6 of the #263 review).
		if item.ChainGrading {
			slog.WarnContext(ctx, "executor: no blob store configured; grading outputs harvest skipped",
				"session", item.SessionID)
		} else {
			slog.DebugContext(ctx, "executor: no blob store configured; idle outputs harvest skipped",
				"session", item.SessionID)
		}
		return e.settleHarvest(ctx, item, nil, false)
	}

	// Keep the lease from provisioning through the reads: a large tree can
	// outlast a fixed TTL, and losing the lease mid-stage cancels the work.
	kctx, keeper := e.queue.KeepLease(ctx, item, e.cfg.LeaseTTL, e.cfg.StallTimeout)
	sb, walk, runErr := e.harvestSandbox(kctx, item, sess, keeper.Progress)
	var files []harvestFile
	if runErr == nil && walk {
		files, runErr = e.collectOutputs(kctx, item, sb, keeper.Progress)
	}
	if kerr := keeper.Close(); kerr != nil {
		e.discardStaged(ctx, files)
		return fmt.Errorf("lease keeper: %w", kerr)
	}
	if runErr != nil {
		return runErr
	}
	// walk is also the replace flag: a pass that read no sandbox replaces
	// nothing, so an idle harvest that found no live sandbox leaves the
	// previous snapshot in place rather than deleting it (docs/plan/38
	// decision 6).
	return e.settleHarvest(ctx, item, files, walk)
}

// harvestSandbox obtains the sandbox this harvest reads, and reports whether
// there is one to walk (docs/plan/38 decision 8). A grading harvest provisions;
// an idle one only attaches to a live sandbox and, finding none (ErrNotFound),
// returns (nil, false, nil) so the caller settles a no-op. Any OTHER attach
// error is propagated, never swallowed: it is a transient fault (a Docker
// inspect hiccup, a K8s API timeout), not "no sandbox", so the lease-recovery
// reclaim must retry it — exactly as a grading harvest's provision error is
// retried. Taking it for no-sandbox would silently drop the session's only
// harvest on a one-off fault.
func (e *Executor) harvestSandbox(ctx context.Context, item *queue.Item, sess sessionRun, progress func()) (sandbox.Sandbox, bool, error) {
	if item.ChainGrading {
		sb, err := e.provisionSandbox(ctx, item.SessionID, sess, progress)
		if err != nil {
			return nil, false, err
		}
		progress()
		return sb, true, nil
	}
	sb, err := e.provider.Attach(ctx, item.SessionID)
	switch {
	case errors.Is(err, sandbox.ErrNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("attach session sandbox: %w", err)
	}
	progress()
	return sb, true, nil
}

// collectOutputs lists the outputs tree in the given sandbox and stages every
// admitted file's bytes into the blob store at its final key. On error the
// staged objects are deleted — a snapshot either stages whole or leaves no
// residue.
//
// progress is called as each step lands, because this is the other lane that
// reads a sandbox and so the other lane a wedged call can park (#383): a large
// tree legitimately takes a while, and only a per-file report tells that apart
// from a read that will never return.
func (e *Executor) collectOutputs(ctx context.Context, item *queue.Item, sb sandbox.Sandbox, progress func()) (_ []harvestFile, err error) {
	var staged []harvestFile
	defer func() {
		if err != nil {
			// Not ctx itself: when the fault is the lease keeper cancelling
			// this context, the discard must still reach the blob store.
			e.discardStaged(context.WithoutCancel(ctx), staged)
		}
	}()

	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: harvestListScript})
	if err != nil {
		return nil, fmt.Errorf("list outputs: %w", err)
	}
	progress()
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
		// At the top of the iteration, so the exclusions below count too: a file
		// that vanished, overran a cap, or turned out not to be regular still
		// took a sandbox round trip, and a tree of thousands of excluded paths
		// is a run that is moving (#383).
		progress()
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
		m := mimetab.ByPath(p)
		perr := e.blobs.Put(ctx, blob.FilesKey(id.String()), rc, size, m)
		_ = rc.Close()
		if perr != nil {
			return nil, fmt.Errorf("stage output %s: %w", p, perr)
		}
		staged = append(staged, harvestFile{path: p, id: id, size: size, mime: m})
		total += size
	}
	// The loop reports as each path is taken up, so the last file's staging is
	// reported here — otherwise it and the settlement behind it would share one
	// silent interval.
	progress()
	return staged, nil
}

// settleHarvest is the harvest's single registry transaction: under the
// session row lock, replace the session's snapshot rows, chain the grading
// model_turn where one is waiting, and complete the item — all or nothing.
// The fence has three arms (docs/plan/38 decision 2), described where it is
// applied below; the one it has always had is the outcome entry still being
// `evaluating`, so that a grading cycle which settled while the harvest ran (a
// user.interrupt flips the entry and cancels the queue in its own transaction)
// wins outright, mirroring the brain's settleVerdict rule — nothing of that
// harvest commits and the staged bytes are discarded.
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
			// A grading harvest cannot settle without the outcome it grades, so
			// a decode failure there is a genuine fault. An idle harvest never
			// reads the outcome — its publish-or-no-op arm ignores it — and must
			// not fault on one: a session whose outcome state is corrupt is one
			// runTurn already idled for that same corruption ("session outcome
			// state is corrupt"), which is what scheduled this idle harvest, so
			// faulting here would reclaim-loop it forever (#263 review). Treat
			// the undecodable state as no active outcome and settle.
			if item.ChainGrading {
				return fmt.Errorf("decode stored outcome_evaluations: %w", err)
			}
			evals = nil
		}
	}
	// Live outcome state first, the item's own tag second (docs/plan/38
	// decision 2). Reading the tag first would be wrong twice over. An outcome
	// that is evaluating right now owns this pass whichever trigger enqueued
	// it: the two flavors share one live slot per session, so a grading cycle
	// starting while an idle-tagged item is still queued has its own enqueue
	// silently swallowed, and refusing to serve it would strand that outcome in
	// evaluating forever. And once nothing is evaluating, the outcome column
	// can no longer tell a cycle a user.interrupt settled mid-harvest (discard,
	// the fence this has always been) from a session that never had an outcome
	// at all (publish) — only the tag can, which is why it exists.
	active, ok := events.ActiveOutcome(evals)
	grading := ok && active.Result == domain.OutcomeResultEvaluating
	switch {
	case grading && !replace && !item.ChainGrading:
		// Grading takeover on an idle-tagged item that walked no sandbox
		// (docs/plan/38 decision 2): the idle item held the one live harvest slot
		// when a fresh outcome turned evaluating, so the cycle's own enqueue was
		// swallowed and this stale item is all it has — but an idle attach found
		// no live sandbox, so replace is false and nothing was freshly collected.
		// Chaining the grader here would grade a stale or empty snapshot (before
		// the idle trigger existed a grading harvest always provisioned, so this
		// never arose). Hand the item back as a grading harvest instead: its next
		// claim provisions a sandbox, walks a current tree, publishes it, and
		// chains the grader then. The !ChainGrading guard is load-bearing: a
		// grading-tagged item reaching here with replace=false is the blob-less
		// deploy, which must still chain the grader transcript-only (below), not
		// requeue.
		if err := e.queue.RequeueAsGrading(ctx, tx, item); err != nil {
			return err
		}
		return tx.Commit(ctx)
	case !grading && item.ChainGrading:
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
	// Only a live grading cycle is waiting on this snapshot. A harvest a
	// session's idle asked for publishes and stops: chaining a turn there would
	// wake a session nobody asked to resume.
	//
	// The chaining branch is the one settlement that wakes the brain without
	// first asking whether an MCP call is outstanding, and deliberately: a
	// grading harvest runs inside the outcome cycle, after the turn has already
	// ended, and the only producer of an agent.mcp_tool_use is the brain inside
	// a turn — so there is no call here to schedule ahead of this. Said rather
	// than left implicit, because the six sites that *do* ask are a set someone
	// will count again.
	if grading {
		if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, queue.ModelTurn); err != nil {
			return err
		}
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
