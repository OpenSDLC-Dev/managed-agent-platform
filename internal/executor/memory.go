package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// This file is the platform-managed half of memory stores (plan 36 slice 4,
// decisions 9-12): a cloud session's attached stores land in its sandbox
// before the tools run, and what the agent does to those directories is
// reconciled with the stores when the run ends. The BYOC worker will do the
// same over the wire (slice 6); the rules both halves share — the marker, the
// baseline, the tree hash and the per-path decision — are internal/memsync's.

// memoryRef is the memory_store arm of a session's resources[] as the executor
// reads it: the store, the access the attachment asked for, and the mount the
// API derived from the store's name. The other element types decode to an
// empty Type-mismatch and are filtered out, as fileRef and repoRef do.
type memoryRef struct {
	Type          string `json:"type"`
	MemoryStoreID string `json:"memory_store_id"`
	Access        string `json:"access"`
	MountPath     string `json:"mount_path"`
}

const (
	// memorySyncDir holds one baseline file per store, beside the mounts
	// rather than inside them so it is never a memory and never hashed.
	memorySyncDir = "/mnt/memory/.sync"
	// memoryFileMode is what a memory file lands with (decision 10): the docker
	// daemon lands a batch's members root-owned, and a root-owned 0644 file
	// refuses a non-root agent's in-place `>>` even though the file tools'
	// rename-over succeeds.
	memoryFileMode = 0o666
)

func baselinePath(storeID string) string { return path.Join(memorySyncDir, storeID) }

// memoryMounts is the attached stores a run acts on, in store-id order — the
// one order every session shares, since a mount path is a slug of the name
// the store had when each session attached it — so two sessions settling the
// same stores lock their rows the same way round and cannot deadlock, and the
// passes and their telemetry are reproducible.
func memoryMounts(refs []memoryRef) []memoryRef {
	mounts := make([]memoryRef, 0, len(refs))
	for _, r := range refs {
		if r.Type == "memory_store" && r.MemoryStoreID != "" && strings.HasPrefix(r.MountPath, toolset.MemoryMountRoot+"/") {
			mounts = append(mounts, r)
		}
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].MemoryStoreID < mounts[j].MemoryStoreID })
	return mounts
}

// archivedStores answers which of the mounted stores are archived — read-only
// whatever their attachment asked (decision 3). Nothing for no mounts.
func (e *Executor) archivedStores(ctx context.Context, mounts []memoryRef) (map[string]bool, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	ids := make([]string, len(mounts))
	for i, m := range mounts {
		ids[i] = m.MemoryStoreID
	}
	rows, err := e.pool.Query(ctx,
		`SELECT id FROM memory_stores WHERE id = ANY($1) AND archived_at IS NOT NULL`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	archived := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		archived[id] = true
	}
	return archived, rows.Err()
}

// materializeMemory writes each attached store into the sandbox at its mount
// before the tools run — the memory twin of materializeFiles, after it in
// provisionAndRun. A store whose marker is present and holds the expected
// bytes is left alone (the sync reconciles it); one whose directory holds
// files but no trusted marker is left alone too, and the sync treats it as
// pull-only (decision 12); a fresh directory gets the store's memories, the
// marker, and a baseline that says they agree. A missing store row is a
// logged, counted miss the brain's block hedges; a failed write is logged and
// tolerated, never fatal to the run. It answers how many stores the sandbox
// already held, for the caller to reconcile before the tools read them.
func (e *Executor) materializeMemory(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []memoryRef, progress func()) (existing int) {
	mounts := memoryMounts(refs)
	if len(mounts) == 0 {
		return 0
	}
	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "memory_materialize")
	defer span.End()
	start := time.Now()
	defer func() { recordMemoryMaterializeDuration(ctx, time.Since(start)) }()
	span.SetAttributes(attribute.Int("memory.referenced", len(mounts)))

	landed := 0
	for _, m := range mounts {
		// Reported per store for materializeFiles's reason: a store can be
		// 2,000 files, and the pass has moved whether it landed or was skipped.
		progress()
		outcome, err := e.materializeStore(ctx, sb, m)
		recordMemoryMaterialized(ctx, outcome)
		switch {
		case err != nil:
			slog.WarnContext(ctx, "memory store not materialized",
				"session_id", sid, "memory_store_id", m.MemoryStoreID, "mount_path", m.MountPath,
				"outcome", outcome, "err", err)
		case outcome == memoryOutcomeOK:
			landed++
			slog.InfoContext(ctx, "memory store materialized",
				"session_id", sid, "memory_store_id", m.MemoryStoreID, "mount_path", m.MountPath)
		case outcome == memoryOutcomeUnchanged:
			existing++
		case outcome == memoryOutcomeUntrusted:
			// Held all the same: the sync before the tools pulls into it
			// (readMemory judges the marker and makes it pull-only), so
			// the view the tools get is current, not a run stale.
			existing++
			slog.WarnContext(ctx, "memory store directory holds files but no trusted marker; not re-materialized, and pull-only until the sandbox is replaced",
				"session_id", sid, "memory_store_id", m.MemoryStoreID, "mount_path", m.MountPath)
		}
	}
	progress()
	span.SetAttributes(attribute.Int("memory.materialized", landed))
	return existing
}

// materializeStore lands one store, answering with its outcome.
func (e *Executor) materializeStore(ctx context.Context, sb sandbox.Sandbox, m memoryRef) (string, error) {
	marker := path.Join(m.MountPath, memsync.MarkerName)
	want := memsync.MarkerBytes(m.MemoryStoreID)
	// The marker's bytes, not its presence: the directory is agent-writable,
	// and a marker rewritten to name another store is the altered marker
	// decision 12 stops trusting.
	if prev, err := sb.ReadFile(ctx, marker); err == nil && bytes.Equal(prev, want) {
		return memoryOutcomeUnchanged, nil
	}
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: memsync.HashTreeCommand(m.MountPath)})
	if err != nil {
		return memoryOutcomeFailed, err
	}
	// A listing with anything in it — or one too long to capture, or one
	// that failed — is a directory nothing vouches for: files with no marker,
	// files the listing could not read, or a path that is no longer the
	// directory. An absent directory lists nothing and exits 0, which is the
	// fresh case.
	if len(res.Stdout) > 0 || res.Truncated || res.ExitCode != 0 {
		return memoryOutcomeUntrusted, nil
	}

	var exists bool
	err = e.pool.QueryRow(ctx, `SELECT true FROM memory_stores WHERE id = $1`, m.MemoryStoreID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return memoryOutcomeNotFound, errors.New("memory store row missing")
	}
	if err != nil {
		return memoryOutcomeFailed, err
	}
	rows, err := e.pool.Query(ctx,
		`SELECT path, content, content_sha256 FROM memories WHERE memory_store_id = $1 ORDER BY path`,
		m.MemoryStoreID)
	if err != nil {
		return memoryOutcomeFailed, err
	}
	defer rows.Close()
	// One batch for the whole store, bounded by the store's own caps (2,000
	// memories of 100 kB): the members, the marker, and the baseline saying
	// the directory and the store agree on every one of them.
	writes := []sandbox.FileWrite{{Path: marker, Data: want}}
	baseline := memsync.Baseline{Synced: map[string]string{}}
	for rows.Next() {
		var p, content, sha string
		if err := rows.Scan(&p, &content, &sha); err != nil {
			return memoryOutcomeFailed, err
		}
		writes = append(writes, sandbox.FileWrite{Path: m.MountPath + p, Data: []byte(content), Mode: memoryFileMode})
		baseline.Synced[p] = sha
	}
	if err := rows.Err(); err != nil {
		return memoryOutcomeFailed, err
	}
	writes = append(writes, sandbox.FileWrite{Path: baselinePath(m.MemoryStoreID), Data: baseline.Encode()})
	if err := sb.WriteFiles(ctx, writes); err != nil {
		return memoryOutcomeFailed, err
	}
	return memoryOutcomeOK, nil
}

// memorySync is one sync of a session's stores, carried across its three
// phases (decision 11): read, which happens in the sandbox once the tools
// have run; settle, which happens inside the transaction that commits the
// run; and apply, which writes the settlement back into the sandbox after the
// commit. The phases are split that way so the store's rows and the run's
// results commit together, and the sandbox is written only from a state the
// database holds.
type memorySync struct {
	sb     sandbox.Sandbox
	sid    domain.ID
	stores []*storeSync
	span   trace.Span
	start  time.Time
	ended  bool
}

// storeSync is one store's sync: what the read phase saw, and what the
// settlement decided the sandbox should get.
type storeSync struct {
	ref      memoryRef
	markerOK bool
	local    map[string]string
	baseline memsync.Baseline
	// raw is the baseline file as read, so a sync that changed nothing can
	// tell it has nothing to write back.
	raw []byte
	// contents holds the bytes of every locally changed file, read before
	// the settlement so a push carries exactly what was hashed. Its bound is
	// the store's own (2,000 memories of 100 kB) and only for files that
	// changed since the baseline — the common run reads none.
	contents map[string][]byte
	// skipped marks a store this sync leaves alone: a listing that could not
	// be read or trusted, a settlement that failed, or a store whose row is
	// gone.
	skipped bool
	// restamp marks a rebuilt directory: the marker goes back with the files.
	restamp bool

	writes []sandbox.FileWrite
	// removals are memory paths the store deleted, for the apply phase to
	// unlink.
	removals []string
	next     memsync.Baseline
	counts   memorySyncCounts
}

// memorySyncCounts is a store's sync in numbers; all but withheld — the
// pushes a pull-only sync dropped, logged rather than measured — are the
// memory.sync.actions instrument's.
type memorySyncCounts struct{ pulled, pushed, deleted, conflict, refused, withheld int }

// readMemory is the read phase: each store's tree hash, its baseline and the
// bytes of what changed. Nil when the session has no stores. The span and
// the duration it opens end in apply — or in end, whichever the caller
// reaches — so they cover the settlement too.
func (e *Executor) readMemory(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []memoryRef, progress func()) *memorySync {
	mounts := memoryMounts(refs)
	if len(mounts) == 0 {
		return nil
	}
	_, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "memory_sync")
	ms := &memorySync{sb: sb, sid: sid, span: span, start: time.Now()}
	span.SetAttributes(attribute.Int("memory.stores", len(mounts)))
	for _, m := range mounts {
		progress()
		st := &storeSync{ref: m}
		ms.stores = append(ms.stores, st)
		if err := e.readStore(ctx, sb, st); err != nil {
			st.skipped = true
			slog.WarnContext(ctx, "memory store not synced: its directory could not be read",
				"session_id", sid, "memory_store_id", m.MemoryStoreID, "mount_path", m.MountPath, "err", err)
		}
	}
	progress()
	return ms
}

func (e *Executor) readStore(ctx context.Context, sb sandbox.Sandbox, st *storeSync) error {
	mount, id := st.ref.MountPath, st.ref.MemoryStoreID
	marker, err := sb.ReadFile(ctx, path.Join(mount, memsync.MarkerName))
	st.markerOK = err == nil && bytes.Equal(marker, memsync.MarkerBytes(id))

	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: memsync.HashTreeCommand(mount)})
	if err != nil {
		return err
	}
	if res.Truncated {
		return errors.New("the listing overflows the exec output cap")
	}
	// A non-zero exit is a listing not to act on — under the command's
	// pipefail, a stage that failed: a directory find could not enter, whose
	// files would otherwise read as deleted; or a mount that is no longer the
	// directory the store was landed in. An absent directory is an empty
	// listing that exits 0 — the empty tree the wipe guard then judges.
	if res.ExitCode != 0 {
		return fmt.Errorf("the listing exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	st.local, err = memsync.ParseHashTree([]byte(res.Stdout))
	if err != nil {
		return err
	}
	raw, err := sb.ReadFile(ctx, baselinePath(id))
	if err != nil && !errors.Is(err, sandbox.ErrFileNotExist) {
		return fmt.Errorf("read baseline: %w", err)
	}
	st.raw = raw
	if st.baseline, err = memsync.DecodeBaseline(raw); err != nil {
		// The reference worker's answer: an unreadable baseline is an empty
		// one. Every file then looks new, and the store wins where the two
		// differ — a bounded loss, where refusing to sync at all would be an
		// unbounded one.
		slog.WarnContext(ctx, "memory baseline unreadable; treated as empty",
			"memory_store_id", id, "err", err)
		st.baseline, _ = memsync.DecodeBaseline(nil)
	}
	st.contents = map[string][]byte{}
	changed := 0
	for p, sha := range st.local {
		if st.baseline.Synced[p] != sha && st.baseline.Refused[p] != sha {
			changed++
		}
	}
	// What this reads is bounded by the store's own shape — at most a
	// store's worth of changed files, each read up to the content cap and
	// no further — not by what bash could plant under the mount: a listing
	// with more changed files than a store holds is not a store's directory,
	// and is skipped this run like a listing that failed.
	if changed > memsync.MaxMemoriesPerStore {
		return fmt.Errorf("%d changed files, more than a store holds", changed)
	}
	for p, sha := range st.local {
		if st.baseline.Synced[p] == sha || st.baseline.Refused[p] == sha {
			continue
		}
		data, err := readCapped(ctx, sb, mount+p, memsync.MaxContentBytes)
		if errors.Is(err, sandbox.ErrFileTooLarge) {
			st.baseline.Refused[p] = sha
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		// The bytes are what a push carries, so their digest is the one the
		// plan decides from — a file rewritten between the listing and this
		// read is pushed as read, not as listed.
		if sum := sha256hex(data); sum != sha {
			st.local[p], sha = sum, sum
		}
		if err := memsync.ValidateContent(string(data)); err != nil {
			st.baseline.Refused[p] = sha
			slog.WarnContext(ctx, "memory file refused", "memory_store_id", id, "path", p, "err", err)
			continue
		}
		st.contents[p] = data
	}
	return nil
}

// readCapped reads a file of at most maxBytes, ErrFileTooLarge past that —
// so a file the content cap will refuse is never read in full.
func readCapped(ctx context.Context, sb sandbox.Sandbox, p string, maxBytes int64) ([]byte, error) {
	rc, _, err := sb.ReadFileStream(ctx, p, maxBytes)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// settleMemory is the settle phase, inside the transaction that commits the
// run (the session row is already locked by it): each store's row is locked
// like the API's own writers lock it, its heads listed, the plan made, and
// every write to the store applied with the compare-and-set the plan carries.
// A conflict, a refusal and a vanished store are outcomes, counted and
// applied. Each store settles inside a savepoint of the run's transaction,
// so a store whose settlement fails is skipped — its baseline kept, its sync
// retried next run — rather than failing the commit, which would re-run
// every tool for a memory the run's results never depended on. The sandbox
// is not touched here.
func (e *Executor) settleMemory(ctx context.Context, tx pgx.Tx, ms *memorySync) error {
	if ms == nil {
		return nil
	}
	for _, st := range ms.stores {
		if st.skipped {
			continue
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if err := e.settleStore(ctx, sp, ms.sid, st); err != nil {
			_ = sp.Rollback(ctx)
			st.skipped = true
			slog.WarnContext(ctx, "memory store not synced: its settlement failed; retried at the next run",
				"session_id", ms.sid, "memory_store_id", st.ref.MemoryStoreID, "err", err)
			continue
		}
		if err := sp.Commit(ctx); err != nil {
			return fmt.Errorf("memory store %s: %w", st.ref.MemoryStoreID, err)
		}
	}
	return nil
}

func (e *Executor) settleStore(ctx context.Context, tx pgx.Tx, sid domain.ID, st *storeSync) error {
	id := st.ref.MemoryStoreID
	var archivedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT archived_at FROM memory_stores WHERE id = $1 FOR UPDATE`, id).Scan(&archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleted since the attachment (decision 7 keeps the element): the
		// directory stays as the agent left it, and nothing is written.
		st.skipped = true
		slog.InfoContext(ctx, "memory store not synced: the store no longer exists",
			"session_id", sid, "memory_store_id", id)
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, path, content_sha256 FROM memories WHERE memory_store_id = $1`, id)
	if err != nil {
		return err
	}
	remote := map[string]memsync.Head{}
	for rows.Next() {
		var head memsync.Head
		var p string
		if err := rows.Scan(&head.ID, &p, &head.SHA); err != nil {
			rows.Close()
			return err
		}
		remote[p] = head
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// read_only is the attachment's; archived is the store's; an untrusted
	// marker is the directory's. Any of the three makes this sync pull-only.
	plan := memsync.Plan(memsync.Input{
		Local: st.local, Baseline: st.baseline, Remote: remote,
		PullOnly: st.ref.Access == "read_only" || archivedAt != nil || !st.markerOK,
	})
	st.next = plan.Next
	st.counts.refused += len(plan.Skipped)
	st.counts.withheld = plan.Withheld
	st.restamp = plan.Rebuild
	if plan.Rebuild {
		slog.WarnContext(ctx, "memory store directory is empty against a baseline of several files; rebuilt, nothing deleted",
			"session_id", sid, "memory_store_id", id, "baseline_files", len(st.baseline.Synced))
	}
	// The wire's session_actor (decision 6), the attribution every version a
	// session writes carries.
	actorJSON, _ := json.Marshal(map[string]string{"type": "session_actor", "session_id": sid.String()})
	actor := json.RawMessage(actorJSON)
	held := len(remote)
	// Deletions settle first, then the rest in path order: a create needs
	// the room a deletion makes under the cap, and a file replacing a
	// directory (`/a/b` gone, `/a` written) needs the descendant's deletion
	// settled before its occupancy check, or the store would refuse it.
	ordered := make([]memsync.Action, 0, len(plan.Actions))
	for _, act := range plan.Actions {
		if act.Kind == memsync.DeleteRemote {
			ordered = append(ordered, act)
		}
	}
	for _, act := range plan.Actions {
		if act.Kind != memsync.DeleteRemote {
			ordered = append(ordered, act)
		}
	}
	for _, act := range ordered {
		target := st.ref.MountPath + act.Path
		switch act.Kind {
		case memsync.Pull:
			var content string
			if err := tx.QueryRow(ctx, `SELECT content FROM memories WHERE id = $1`, act.ID).Scan(&content); err != nil {
				return fmt.Errorf("pull %s: %w", act.Path, err)
			}
			st.writes = append(st.writes, sandbox.FileWrite{Path: target, Data: []byte(content), Mode: memoryFileMode})
			st.next.Synced[act.Path] = act.RemoteSHA
			st.counts.pulled++
			if act.Conflict {
				st.counts.conflict++
			}
		case memsync.RemoveLocal:
			st.removals = append(st.removals, act.Path)
			st.counts.deleted++
		case memsync.DeleteRemote:
			tag, err := tx.Exec(ctx, `DELETE FROM memories WHERE id = $1 AND content_sha256 = $2`, act.ID, act.BaselineSHA)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				// The store moved on (or the memory is already gone): the
				// baseline is kept, so the next sync sees the change and pulls.
				st.next.Synced[act.Path] = act.BaselineSHA
				st.counts.conflict++
				continue
			}
			versionID := domain.NewID(domain.PrefixMemoryVersion).String()
			if err := insertSessionVersion(ctx, tx, versionID, id, act.ID, "deleted", act.Path, nil, actor); err != nil {
				return err
			}
			held-- // room for a create later in this same settlement
			st.counts.deleted++
		case memsync.Push:
			data, ok := st.contents[act.Path]
			if !ok {
				return fmt.Errorf("push %s: no bytes were read for it", act.Path)
			}
			outcome, err := e.pushMemory(ctx, tx, id, act, data, actor, &held)
			if err != nil {
				return err
			}
			switch outcome {
			case memoryPushOK:
				st.next.Synced[act.Path] = act.LocalSHA
				st.counts.pushed++
			case memoryPushConflict:
				if act.ID == "" {
					// A create the store's occupancy refused: the store holds
					// a memory at an ancestor or descendant of this path (a
					// head at the path itself would have been a pull), and a
					// file and a directory cannot share a name. The store wins,
					// as it wins every both-sides change: the file goes, and
					// the memory's own pull lands where the file was in its way.
					slog.WarnContext(ctx, "memory file removed: the store holds a memory at an ancestor or descendant path",
						"session_id", sid, "memory_store_id", id, "path", act.Path)
					st.removals = append(st.removals, act.Path)
				} else {
					// An update the store's head no longer matches: the file
					// stays as written, unrecorded, so the next sync judges
					// it against the store's answer.
					st.next.Synced[act.Path] = act.BaselineSHA
				}
				st.counts.conflict++
			case memoryPushOverCap:
				// The store's state, not the file's: not remembered, so the
				// next run retries it against whatever room deletions made.
				slog.WarnContext(ctx, "memory not created: the store holds its maximum",
					"session_id", sid, "memory_store_id", id, "path", act.Path, "cap", memsync.MaxMemoriesPerStore)
				st.counts.refused++
			}
		}
	}
	return nil
}

const (
	memoryPushOK       = "ok"
	memoryPushConflict = "conflict"
	memoryPushOverCap  = "over the cap"
)

// pushMemory writes one file to the store: a create (act.ID empty) refused by
// occupancy or the 2,000 cap, or an update conditioned on the head the
// baseline recorded — the API's own rules and the reference worker's own
// answers to them (a 409 loses, a 400 or 413 is remembered as refused). The
// head row is written with the version's id before the version row, which is
// what a compare-and-set that lands nothing must leave nothing behind; the
// version row then carries that same id, so the head points at a version
// that exists (a redact's head check and the wire's pointer both read it).
func (e *Executor) pushMemory(ctx context.Context, tx pgx.Tx, storeID string, act memsync.Action,
	data []byte, actor json.RawMessage, held *int) (string, error) {
	content := string(data)
	versionID := domain.NewID(domain.PrefixMemoryVersion).String()
	if act.ID == "" {
		// Occupancy is the API's predicate (internal/api/memories.go
		// occupiedBy): a path, its ancestors and its descendants. The unique
		// index would catch only the first, and here nothing else can — the
		// store row is locked, so no other writer is between the listing and
		// this insert.
		var occupied bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM memories
			   WHERE memory_store_id = $1
			     AND (path = $2
			       OR left(path, length($2) + 1) = $2 || '/'
			       OR left($2, length(path) + 1) = path || '/'))`, storeID, act.Path).Scan(&occupied); err != nil {
			return "", err
		}
		if occupied {
			return memoryPushConflict, nil
		}
		if *held >= memsync.MaxMemoriesPerStore {
			return memoryPushOverCap, nil
		}
		memoryID := domain.NewID(domain.PrefixMemory).String()
		tag, err := tx.Exec(ctx,
			`INSERT INTO memories
			   (id, memory_store_id, path, content, content_sha256, content_size_bytes, memory_version_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
			memoryID, storeID, act.Path, content, act.LocalSHA, len(content), versionID)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			return memoryPushConflict, nil
		}
		*held++
		return memoryPushOK, insertSessionVersion(ctx, tx, versionID, storeID, memoryID, "created", act.Path, &content, actor)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE memories
		    SET content = $3, content_sha256 = $4, content_size_bytes = $5,
		        memory_version_id = $6, updated_at = now()
		  WHERE id = $1 AND content_sha256 = $2`,
		act.ID, act.BaselineSHA, content, act.LocalSHA, len(content), versionID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return memoryPushConflict, nil
	}
	return memoryPushOK, insertSessionVersion(ctx, tx, versionID, storeID, act.ID, "modified", act.Path, &content, actor)
}

// insertSessionVersion is the version row every session write appends, the
// shape internal/api's insertMemoryVersion writes with a session_actor for
// its created_by (decision 6; migration 0029 names the columns), under the
// id the head row was given: a `deleted` version carries its path and
// nothing else.
func insertSessionVersion(ctx context.Context, tx pgx.Tx, versionID, storeID, memoryID, operation, p string,
	content *string, actor json.RawMessage) error {
	var body, sha, size any
	if content != nil {
		body, sha, size = *content, sha256hex([]byte(*content)), len(*content)
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO memory_versions
		   (id, memory_store_id, memory_id, operation, path, content, content_sha256,
		    content_size_bytes, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		versionID, storeID, memoryID, operation, p, body, sha, size, actor)
	return err
}

// applyMemory is the apply phase, after the commit: the store's deletions are
// removed from the directory (and their emptied directories with them), then
// its changes, the marker if the directory was rebuilt, and the new baseline
// are written in one batch — in that order, because a baseline that no
// longer names a file still present would push it back as new on the next
// sync. A write that fails leaves the old baseline, and the next sync
// re-derives what this one could not land: a push already committed then
// reads as agreement, a pull as still pending. What the settlement committed
// is counted before the sandbox is touched, so a failed apply never hides it.
func (e *Executor) applyMemory(ctx context.Context, ms *memorySync) {
	if ms == nil {
		return
	}
	defer ms.end(ctx)
	for _, st := range ms.stores {
		if st.skipped {
			continue
		}
		id := st.ref.MemoryStoreID
		recordMemorySyncActions(ctx, st.counts)
		if c := st.counts; c.pulled+c.pushed+c.deleted+c.conflict+c.refused+c.withheld > 0 {
			slog.InfoContext(ctx, "memory store synced",
				"session_id", ms.sid, "memory_store_id", id, "pulled", c.pulled, "pushed", c.pushed,
				"deleted", c.deleted, "conflict", c.conflict, "refused", c.refused, "withheld", c.withheld)
		}
		removed := true
		for _, cmd := range memsync.RemoveCommands(st.ref.MountPath, st.removals) {
			res, err := ms.sb.Exec(ctx, sandbox.ExecRequest{Command: cmd})
			if err != nil || res.ExitCode != 0 {
				slog.WarnContext(ctx, "memory deletions not applied; the baseline is kept for the next sync",
					"session_id", ms.sid, "memory_store_id", id, "err", err, "exit", res.ExitCode, "stderr", strings.TrimSpace(res.Stderr))
				removed = false
				break
			}
		}
		if !removed {
			continue
		}
		// A sync that moved nothing and agrees with the baseline it read has
		// nothing to write: the common run costs the directory one listing.
		next := st.next.Encode()
		if len(st.writes) == 0 && !st.restamp && bytes.Equal(next, st.raw) {
			continue
		}
		writes := st.writes
		if st.restamp {
			// A rebuilt directory gets its marker back with its files — the
			// reference worker's stampAndPull — or it would stay pull-only for
			// the sandbox's life, the agent's every later write withheld.
			writes = append(writes, sandbox.FileWrite{Path: path.Join(st.ref.MountPath, memsync.MarkerName), Data: memsync.MarkerBytes(id)})
		}
		writes = append(writes, sandbox.FileWrite{Path: baselinePath(id), Data: next})
		if err := ms.sb.WriteFiles(ctx, writes); err != nil {
			slog.WarnContext(ctx, "memory changes not applied; the baseline is kept for the next sync",
				"session_id", ms.sid, "memory_store_id", id, "err", err)
		}
	}
}

// end closes the sync's span and records its duration, once, whichever phase
// the sync got to.
func (ms *memorySync) end(ctx context.Context) {
	if ms == nil || ms.ended {
		return
	}
	ms.ended = true
	var c memorySyncCounts
	for _, st := range ms.stores {
		if st.skipped {
			continue // a settlement rolled back counted nothing that landed
		}
		c.pulled += st.counts.pulled
		c.pushed += st.counts.pushed
		c.deleted += st.counts.deleted
		c.conflict += st.counts.conflict
		c.refused += st.counts.refused
	}
	ms.span.SetAttributes(
		attribute.Int("memory.pulled", c.pulled), attribute.Int("memory.pushed", c.pushed),
		attribute.Int("memory.deleted", c.deleted), attribute.Int("memory.conflict", c.conflict),
		attribute.Int("memory.refused", c.refused))
	recordMemorySyncDuration(ctx, time.Since(ms.start))
	ms.span.End()
}

// syncMemoryStandalone is the three phases in one call, for a sandbox no run
// is committing for: the reaper's, before it checkpoints and destroys an idle
// session's sandbox (decision 11), so what the agent wrote after its last run
// — nothing, unless that run faulted — reaches the store before the sandbox
// goes. The resources are read here, since the reaper holds no session.
func (e *Executor) syncMemoryStandalone(ctx context.Context, sb sandbox.Sandbox, sid domain.ID) error {
	var resourcesJSON []byte
	err := e.pool.QueryRow(ctx, `SELECT resources FROM sessions WHERE id = $1`, sid.String()).Scan(&resourcesJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var refs []memoryRef
	if err := json.Unmarshal(resourcesJSON, &refs); err != nil {
		return err
	}
	return e.syncMemoryNow(ctx, sb, sid, refs, func() {})
}

// syncMemoryNow runs the three phases in their own transaction, which takes
// the session row lock the run's commit would: the reaper's sync, and the
// run's own before its tools read a mount the sandbox already held — so a
// store's change reaches a session at its next run rather than the one
// after (scope decision 7), and a run that faulted before its sync pushes
// what it wrote at the next.
func (e *Executor) syncMemoryNow(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []memoryRef, progress func()) error {
	ms := e.readMemory(ctx, sb, sid, refs, progress)
	if ms == nil {
		return nil
	}
	defer ms.end(ctx)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
	}
	if err := e.settleMemory(ctx, tx, ms); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	e.applyMemory(ctx, ms)
	return nil
}
