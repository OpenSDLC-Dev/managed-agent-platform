package worker

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

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// This file is the BYOC half of memory stores (plan 36 slice 6, decision 16):
// the executor's materialization and three-phase sync (internal/executor/
// memory.go) done over the wire, since the worker holds no database. The
// rules both halves share — the marker, the baseline, the tree hash and the
// per-path decision — are internal/memsync's; what differs is the settle
// phase, which here is the five memory calls the item's sessions token
// admits (list, get, create, update, delete), each answered by the API's own
// status codes rather than a row lock: a 409 is a lost race the store wins,
// a 404 on an update is a memory to re-create, a 400 is a refusal to
// remember — the reference worker's reading of the same routes.

// ErrSessionMemoryNoToken fails a work item whose session attaches a memory
// store when the item carried no sessions token: the memory routes admit
// nothing else, so the stores cannot be mounted, and running the tools
// without them would hand the agent the amnesia the reference worker refuses
// (anthropic-sdk-go v1.66.0 lib/environments/worker.go:516-528). Its text is
// the reference's own, so the two workers fail the same item the same way.
// The lease loop drains such an item — a re-hand-out would carry no token
// either, and reclaiming it would loop.
var ErrSessionMemoryNoToken = errors.New("the work item carried no sessions token, so the session's memory stores cannot be mounted")

// memoryRef is the memory_store arm of a session's resources[] as the worker
// reads it — the executor's shape: the store, the access the attachment
// asked for, and the mount the API derived from the store's name. The other
// element types decode to a Type mismatch and are filtered out.
type memoryRef struct {
	Type          string `json:"type"`
	MemoryStoreID string `json:"memory_store_id"`
	Access        string `json:"access"`
	MountPath     string `json:"mount_path"`
}

const (
	// memorySyncDir holds one baseline file per store, beside the mounts
	// rather than inside them so it is never a memory and never hashed —
	// the executor's path, byte for byte; the two never share a sandbox.
	memorySyncDir = "/mnt/memory/.sync"
	// memoryFileMode is what a memory file lands with (decision 10): a
	// batch's members land root-owned, and a root-owned 0644 file refuses a
	// non-root agent's in-place `>>`.
	memoryFileMode = 0o666
	// The reference worker's page sizes (lib/environments/memories.go
	// listPageSize, fullListPageSize): the largest the wire allows for each
	// view — the full view is capped at 20.
	memoryListPageSize     = 100
	memoryFullListPageSize = 20
)

func baselinePath(storeID string) string { return path.Join(memorySyncDir, storeID) }

// memoryRefs reads the session's memory_store elements over the wire — the
// one read the run's memory needs, made before the sandbox is provisioned so
// an item that cannot mount its stores fails before it costs a container.
func memoryRefs(ctx context.Context, client sdk.Client, sessionID string) ([]memoryRef, error) {
	sess, err := client.Beta.Sessions.Get(ctx, sessionID, sdk.BetaSessionGetParams{})
	if err != nil {
		return nil, fmt.Errorf("read session for memory: %w", err)
	}
	var snapshot struct {
		Resources []memoryRef `json:"resources"`
	}
	if err := json.Unmarshal([]byte(sess.RawJSON()), &snapshot); err != nil {
		return nil, fmt.Errorf("parse session for memory: %w", err)
	}
	return memoryMounts(snapshot.Resources), nil
}

// memoryMounts is the attached stores a run acts on, in store-id order — the
// executor's order, so the passes and their telemetry read the same on both
// deployment points.
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

// memoryStores is one run's view of its session's stores: the mounts, the
// sandbox they live in, and the credential the memory routes admit — the
// item's sessions token, which every call here rides in place of the client's
// environment key (refused on those routes, decision 15). Nil-safe: a session
// without stores has nothing to land or sync.
type memoryStores struct {
	client    sdk.Client
	opts      []option.RequestOption
	sessionID string
	sb        sandbox.Sandbox
	mounts    []memoryRef
}

func newMemoryStores(client sdk.Client, token, sessionID string, sb sandbox.Sandbox, mounts []memoryRef) *memoryStores {
	if len(mounts) == 0 {
		return nil
	}
	return &memoryStores{client: client, opts: sessionsTokenOptions(token), sessionID: sessionID, sb: sb, mounts: mounts}
}

// roots hands the tool runner what the executor hands its own: every mount,
// and the read_only ones as read-only roots. An archived store is not among
// the latter here — the token reaches no store read, so the worker learns of
// an archive only when the store refuses a write, and withholds the sync
// then (settleStore); the reference worker holds the same position.
func (m *memoryStores) roots() (all, readOnly []string) {
	if m == nil {
		return nil, nil
	}
	for _, r := range m.mounts {
		all = append(all, r.MountPath)
		if r.Access == "read_only" {
			readOnly = append(readOnly, r.MountPath)
		}
	}
	return all, readOnly
}

// listMemories pages a store's memories at the largest page the view allows,
// prefix rollups skipped (none come without depth; the type is checked
// rather than assumed), handing each to f.
func (m *memoryStores) listMemories(ctx context.Context, storeID string, view sdk.BetaManagedAgentsMemoryView, f func(sdk.BetaManagedAgentsMemory)) error {
	limit := int64(memoryListPageSize)
	if view == sdk.BetaManagedAgentsMemoryViewFull {
		limit = memoryFullListPageSize
	}
	pager := m.client.Beta.MemoryStores.Memories.ListAutoPaging(ctx, storeID,
		sdk.BetaMemoryStoreMemoryListParams{View: view, Limit: param.NewOpt(limit)}, m.opts...)
	for pager.Next() {
		item := pager.Current()
		if item.Type != "memory" {
			continue
		}
		f(item.AsMemory())
	}
	return pager.Err()
}

// materialize writes each attached store into the sandbox at its mount before
// the tools run — the executor's materializeMemory over the wire. A store
// whose marker holds the expected bytes is left alone (the sync reconciles
// it); one whose directory holds files but no trusted marker is left alone
// too and synced pull-only (decision 12); a fresh directory gets the store's
// memories, the marker, and a baseline that says they agree. A store the
// routes no longer know is a logged, counted miss; a failed write is logged
// and tolerated, never fatal to the run. It answers how many stores the
// sandbox already held, for the caller to reconcile before the tools read
// them.
func (m *memoryStores) materialize(ctx context.Context, progress func()) (existing int) {
	if m == nil {
		return 0
	}
	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "memory_materialize")
	defer span.End()
	start := time.Now()
	defer func() { recordMemoryMaterializeDuration(ctx, time.Since(start)) }()
	span.SetAttributes(attribute.Int("memory.referenced", len(m.mounts)))

	landed := 0
	for _, ref := range m.mounts {
		// Reported per store, the files materializer's reason: a store can be
		// 2,000 files, and the pass has moved whether it landed or was skipped.
		progress()
		outcome, err := m.materializeStore(ctx, ref, progress)
		recordMemoryMaterialized(ctx, outcome)
		switch {
		case err != nil:
			slog.WarnContext(ctx, "memory store not materialized",
				"session_id", m.sessionID, "memory_store_id", ref.MemoryStoreID, "mount_path", ref.MountPath,
				"outcome", outcome, "err", err)
		case outcome == memoryOutcomeOK:
			landed++
			slog.InfoContext(ctx, "memory store materialized",
				"session_id", m.sessionID, "memory_store_id", ref.MemoryStoreID, "mount_path", ref.MountPath)
		case outcome == memoryOutcomeUnchanged:
			existing++
		case outcome == memoryOutcomeUntrusted:
			// Held all the same: the sync before the tools pulls into it
			// (readStore judges the marker and makes it pull-only), so the
			// view the tools get is current.
			existing++
			slog.WarnContext(ctx, "memory store directory holds files but no trusted marker; not re-materialized, and pull-only until the sandbox is replaced",
				"session_id", m.sessionID, "memory_store_id", ref.MemoryStoreID, "mount_path", ref.MountPath)
		}
	}
	progress()
	span.SetAttributes(attribute.Int("memory.materialized", landed))
	return existing
}

// materializeStore lands one store, answering with its outcome.
func (m *memoryStores) materializeStore(ctx context.Context, ref memoryRef, progress func()) (string, error) {
	marker := path.Join(ref.MountPath, memsync.MarkerName)
	want := memsync.MarkerBytes(ref.MemoryStoreID)
	// The marker's bytes, not its presence: the directory is agent-writable,
	// and a marker rewritten to name another store is the altered marker
	// decision 12 stops trusting.
	if prev, err := m.sb.ReadFile(ctx, marker); err == nil && bytes.Equal(prev, want) {
		return memoryOutcomeUnchanged, nil
	}
	res, err := m.sb.Exec(ctx, sandbox.ExecRequest{Command: memsync.HashTreeCommand(ref.MountPath)})
	if err != nil {
		return memoryOutcomeFailed, err
	}
	// A listing with anything in it — or one too long to capture, or one
	// that failed — is a directory nothing vouches for. An absent directory
	// lists nothing and exits 0, which is the fresh case.
	if len(res.Stdout) > 0 || res.Truncated || res.ExitCode != 0 {
		return memoryOutcomeUntrusted, nil
	}
	// One batch for the whole store, bounded by the store's own caps (2,000
	// memories of 100 kB): the members, the marker, and the baseline saying
	// the directory and the store agree on every one of them.
	writes := []sandbox.FileWrite{{Path: marker, Data: want}}
	baseline := memsync.Baseline{Synced: map[string]string{}}
	err = m.listMemories(ctx, ref.MemoryStoreID, sdk.BetaManagedAgentsMemoryViewFull, func(mem sdk.BetaManagedAgentsMemory) {
		writes = append(writes, sandbox.FileWrite{Path: ref.MountPath + mem.Path, Data: []byte(mem.Content), Mode: memoryFileMode})
		baseline.Synced[mem.Path] = mem.ContentSha256
		progress()
	})
	if isStatus(err, 404) {
		// The store is gone, or this session no longer attaches it: the
		// miss the brain's block hedges.
		return memoryOutcomeNotFound, err
	}
	if err != nil {
		return memoryOutcomeFailed, err
	}
	writes = append(writes, sandbox.FileWrite{Path: baselinePath(ref.MemoryStoreID), Data: baseline.Encode()})
	if err := m.sb.WriteFiles(ctx, writes); err != nil {
		return memoryOutcomeFailed, err
	}
	return memoryOutcomeOK, nil
}

// storeSync is one store's sync: what the read phase saw, and what the
// settlement decided the sandbox should get — the executor's storeSync.
type storeSync struct {
	ref      memoryRef
	markerOK bool
	local    map[string]string
	baseline memsync.Baseline
	// raw is the baseline file as read, so a sync that changed nothing can
	// tell it has nothing to write back.
	raw []byte
	// contents holds the bytes of every locally changed file, read before
	// the settlement so a push carries exactly what was hashed.
	contents map[string][]byte
	// archived is set once the store refused a write as archived: the rest
	// of this sync is pull-only, the way the executor's is from the start.
	archived bool
	// restamp marks a rebuilt directory: the marker goes back with the files.
	restamp bool

	writes   []sandbox.FileWrite
	removals []string
	next     memsync.Baseline
	counts   memorySyncCounts
}

// memorySyncCounts is a store's sync in numbers; all but withheld — the
// pushes a pull-only sync dropped, logged rather than measured — are the
// memory.sync.actions instrument's.
type memorySyncCounts struct{ pulled, pushed, deleted, conflict, refused, withheld int }

func (c *memorySyncCounts) add(o memorySyncCounts) {
	c.pulled += o.pulled
	c.pushed += o.pushed
	c.deleted += o.deleted
	c.conflict += o.conflict
	c.refused += o.refused
	c.withheld += o.withheld
}

// sync reconciles every store with its directory: the three phases of
// decision 11, per store — read (the tree hash, the baseline, the bytes of
// what changed), settle (the store's heads listed, the plan made, each write
// sent with the compare-and-set it carries), apply (the settlement written
// back). Unlike the executor's, the settle phase commits nothing as a unit:
// each call lands on its own, so a store whose settlement fails midway is
// skipped this run with its baseline unwritten, and the next run re-derives
// what this one could not finish — a push already landed then reads as
// agreement, a pull as still pending. Nothing here fails the run.
func (m *memoryStores) sync(ctx context.Context, progress func()) {
	if m == nil {
		return
	}
	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "memory_sync")
	defer span.End()
	start := time.Now()
	defer func() { recordMemorySyncDuration(ctx, time.Since(start)) }()
	span.SetAttributes(attribute.Int("memory.stores", len(m.mounts)))

	var total memorySyncCounts
	for _, ref := range m.mounts {
		progress()
		st := &storeSync{ref: ref}
		if err := m.readStore(ctx, st, progress); err != nil {
			slog.WarnContext(ctx, "memory store not synced: its directory could not be read",
				"session_id", m.sessionID, "memory_store_id", ref.MemoryStoreID, "mount_path", ref.MountPath, "err", err)
			continue
		}
		settled, err := m.settleStore(ctx, st, progress)
		if err != nil {
			slog.WarnContext(ctx, "memory store not synced: its settlement failed; retried at the next run",
				"session_id", m.sessionID, "memory_store_id", ref.MemoryStoreID, "err", err)
			continue
		}
		if !settled {
			continue
		}
		recordMemorySyncActions(ctx, st.counts)
		total.add(st.counts)
		if c := st.counts; c.pulled+c.pushed+c.deleted+c.conflict+c.refused+c.withheld > 0 {
			slog.InfoContext(ctx, "memory store synced",
				"session_id", m.sessionID, "memory_store_id", ref.MemoryStoreID, "pulled", c.pulled, "pushed", c.pushed,
				"deleted", c.deleted, "conflict", c.conflict, "refused", c.refused, "withheld", c.withheld)
		}
		m.applyStore(ctx, st)
	}
	progress()
	span.SetAttributes(
		attribute.Int("memory.pulled", total.pulled), attribute.Int("memory.pushed", total.pushed),
		attribute.Int("memory.deleted", total.deleted), attribute.Int("memory.conflict", total.conflict),
		attribute.Int("memory.refused", total.refused))
}

// memoryFlushTimeout bounds the shutdown flush's own context — the reference
// worker's MemoryFlushTimeout (anthropic-sdk-go v1.66.0 lib/environments/
// memories.go): a slow store cannot stall teardown past it, and it stays
// inside the window the sessions token is still valid after a stop.
const memoryFlushTimeout = 30 * time.Second

// flush is the push-only shutdown pass — the reference worker's FlushWrites,
// run once the tools are closed on every exit, clean or not. A run that ended
// cleanly has already synced (its baseline names what the store holds, so the
// read below finds nothing to push); a run the control plane stopped, or one
// that faulted, never reached that sync and its item is force-stopped with no
// later run to reconcile — so without this pass the memory the agent wrote is
// lost (the worker has no reaper to catch it, unlike the cloud half). It
// uploads new and changed files and nothing else: no pulls, no remote
// deletes, no local removals. Its context is detached from the caller's —
// which a stop has already cancelled — keeping the trace but bounded fresh by
// memoryFlushTimeout; it is best-effort and never reported as a failure.
func (m *memoryStores) flush(ctx context.Context, progress func()) {
	if m == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memoryFlushTimeout)
	defer cancel()
	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "memory_flush")
	defer span.End()
	for _, ref := range m.mounts {
		progress()
		m.flushStore(ctx, &storeSync{ref: ref}, progress)
	}
	progress()
}

// flushStore uploads one store's new and changed files — the reference
// worker's flushStore. It reads the directory the way the sync's read phase
// does, lists the store's heads, and for each changed file creates it, or
// updates it against the head the baseline recorded, skipping a file the
// store already holds at those bytes and one that changed on both sides (the
// store's copy wins there, as it does in a full sync). A read-only store, and
// a directory whose marker does not vouch for it, upload nothing. It writes no
// baseline back: a push the next run repeats is a no-op the store answers as
// agreement, where a baseline written on a torn-down sandbox would not help.
func (m *memoryStores) flushStore(ctx context.Context, st *storeSync, progress func()) {
	id := st.ref.MemoryStoreID
	if st.ref.Access == "read_only" {
		return
	}
	if err := m.readStore(ctx, st, progress); err != nil {
		slog.WarnContext(ctx, "memory not flushed: its directory could not be read",
			"session_id", m.sessionID, "memory_store_id", id, "err", err)
		return
	}
	// An untrusted marker uploads nothing (the sync leaves such a directory
	// as found); nothing changed against the baseline is nothing to push.
	if !st.markerOK || len(st.contents) == 0 {
		return
	}
	remote := map[string]memsync.Head{}
	if err := m.listMemories(ctx, id, sdk.BetaManagedAgentsMemoryViewBasic, func(mem sdk.BetaManagedAgentsMemory) {
		remote[mem.Path] = memsync.Head{ID: mem.ID, SHA: mem.ContentSha256}
		progress()
	}); err != nil {
		slog.WarnContext(ctx, "memory not flushed: the store's heads could not be listed",
			"session_id", m.sessionID, "memory_store_id", id, "err", err)
		return
	}
	paths := make([]string, 0, len(st.contents))
	for p := range st.contents {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// The uploads are sequential, not the reference's concurrent uploadAll: a
	// best-effort bounded flush that saves fewer files under a very large
	// change set is the accepted cost of the simpler pass.
	pushed := 0
	for _, p := range paths {
		progress()
		localSHA := st.local[p]
		head, exists := remote[p]
		baseSHA, hasBase := st.baseline.Synced[p]
		if exists && head.SHA == localSHA {
			continue
		}
		if exists && (!hasBase || head.SHA != baseSHA) {
			slog.WarnContext(ctx, "memory changed both locally and remotely; the flush leaves the remote version",
				"session_id", m.sessionID, "memory_store_id", id, "path", p)
			continue
		}
		act := memsync.Action{Kind: memsync.Push, Path: p, LocalSHA: localSHA, BaselineSHA: baseSHA}
		if exists {
			act.ID = head.ID
		}
		outcome, err := m.push(ctx, id, act, st.contents[p])
		if err != nil {
			// A per-file transport error does not forfeit the files behind it —
			// the reference's uploadAll logs and moves on; only the flush's own
			// bound stops the pass.
			slog.WarnContext(ctx, "memory flush: a file did not upload; continuing with the rest",
				"session_id", m.sessionID, "memory_store_id", id, "path", p, "err", err)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		switch outcome {
		case pushOK:
			pushed++
		case pushArchived:
			// The store is archived: no write of this run's will land.
			return
		}
	}
	if pushed > 0 {
		slog.InfoContext(ctx, "memory flushed on shutdown",
			"session_id", m.sessionID, "memory_store_id", id, "pushed", pushed)
	}
}

// readStore is the read phase, the executor's readStore: the marker judged,
// the tree listed and parsed, the baseline decoded, and the bytes of every
// changed file read up to the content cap.
func (m *memoryStores) readStore(ctx context.Context, st *storeSync, progress func()) error {
	mount, id := st.ref.MountPath, st.ref.MemoryStoreID
	marker, err := m.sb.ReadFile(ctx, path.Join(mount, memsync.MarkerName))
	st.markerOK = err == nil && bytes.Equal(marker, memsync.MarkerBytes(id))

	res, err := m.sb.Exec(ctx, sandbox.ExecRequest{Command: memsync.HashTreeCommand(mount)})
	if err != nil {
		return err
	}
	if res.Truncated {
		return errors.New("the listing overflows the exec output cap")
	}
	// A non-zero exit is a listing not to act on — under the command's
	// pipefail, a stage that failed, whose files would otherwise read as
	// deleted; or a mount that is no longer the directory the store was
	// landed in. An absent directory is an empty listing that exits 0.
	if res.ExitCode != 0 {
		return fmt.Errorf("the listing exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	st.local, err = memsync.ParseHashTree([]byte(res.Stdout))
	if err != nil {
		return err
	}
	raw, err := m.sb.ReadFile(ctx, baselinePath(id))
	if err != nil && !errors.Is(err, sandbox.ErrFileNotExist) {
		return fmt.Errorf("read baseline: %w", err)
	}
	st.raw = raw
	if st.baseline, err = memsync.DecodeBaseline(raw); err != nil {
		// The reference worker's answer: an unreadable baseline is an empty
		// one. Every file then looks new, and the store wins where the two
		// differ — a bounded loss, where refusing to sync would be unbounded.
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
	// Bounded by the store's own shape, not by what bash could plant under
	// the mount: a listing with more changed files than a store holds is not
	// a store's directory, and is skipped this run like a listing that failed.
	if changed > memsync.MaxMemoriesPerStore {
		return fmt.Errorf("%d changed files, more than a store holds", changed)
	}
	for p, sha := range st.local {
		if st.baseline.Synced[p] == sha || st.baseline.Refused[p] == sha {
			continue
		}
		progress()
		data, err := readCapped(ctx, m.sb, mount+p, memsync.MaxContentBytes)
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

// settleStore is the settle phase over the wire: the store's heads listed
// (`view=basic`, the digests without the bodies), the plan made, and each
// action sent as the call it is, with the answer the routes give judged the
// way the reference worker judges it. It reports false, with no error, for a
// store the routes no longer know — deleted since the attachment (decision 7
// keeps the element): the directory stays as the agent left it, and nothing
// is written. An error is a settlement to retry next run.
func (m *memoryStores) settleStore(ctx context.Context, st *storeSync, progress func()) (bool, error) {
	id := st.ref.MemoryStoreID
	remote := map[string]memsync.Head{}
	err := m.listMemories(ctx, id, sdk.BetaManagedAgentsMemoryViewBasic, func(mem sdk.BetaManagedAgentsMemory) {
		remote[mem.Path] = memsync.Head{ID: mem.ID, SHA: mem.ContentSha256}
		progress()
	})
	if isStatus(err, 404) {
		slog.InfoContext(ctx, "memory store not synced: the store no longer exists",
			"session_id", m.sessionID, "memory_store_id", id)
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// read_only is the attachment's; an untrusted marker is the directory's.
	// Either makes this sync pull-only; an archive, which the executor reads
	// from the store's row, is learned here from the store's first refusal.
	plan := memsync.Plan(memsync.Input{
		Local: st.local, Baseline: st.baseline, Remote: remote,
		PullOnly: st.ref.Access == "read_only" || !st.markerOK,
	})
	st.next = plan.Next
	st.counts.refused += len(plan.Skipped)
	st.counts.withheld = plan.Withheld
	st.restamp = plan.Rebuild
	if plan.Rebuild {
		slog.WarnContext(ctx, "memory store directory is empty against a baseline of several files; rebuilt, nothing deleted",
			"session_id", m.sessionID, "memory_store_id", id, "baseline_files", len(st.baseline.Synced))
	}
	// Deletions settle first, then the rest in path order — the executor's
	// order: a create needs the room a deletion makes under the cap, and a
	// file replacing a directory needs the descendant's deletion settled
	// before its occupancy check.
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
		progress()
		target := st.ref.MountPath + act.Path
		switch act.Kind {
		case memsync.Pull:
			mem, err := m.client.Beta.MemoryStores.Memories.Get(ctx, act.ID,
				sdk.BetaMemoryStoreMemoryGetParams{MemoryStoreID: id, View: sdk.BetaManagedAgentsMemoryViewFull}, m.opts...)
			if isStatus(err, 404) {
				// Deleted since the listing. The baseline entry, if any, is
				// kept, so the next sync reads the deletion as the store's
				// rather than the file as new.
				if base, ok := st.baseline.Synced[act.Path]; ok {
					st.next.Synced[act.Path] = base
				}
				continue
			}
			if err != nil {
				return false, fmt.Errorf("pull %s: %w", act.Path, err)
			}
			// The digest recorded is the body fetched, which may be newer
			// than the head listed; the next sync then judges from it.
			st.writes = append(st.writes, sandbox.FileWrite{Path: target, Data: []byte(mem.Content), Mode: memoryFileMode})
			st.next.Synced[act.Path] = mem.ContentSha256
			st.counts.pulled++
			if act.Conflict {
				st.counts.conflict++
			}
		case memsync.RemoveLocal:
			st.removals = append(st.removals, act.Path)
			st.counts.deleted++
		case memsync.DeleteRemote:
			if st.archived {
				st.next.Synced[act.Path] = act.BaselineSHA
				st.counts.withheld++
				continue
			}
			_, err := m.client.Beta.MemoryStores.Memories.Delete(ctx, act.ID,
				sdk.BetaMemoryStoreMemoryDeleteParams{MemoryStoreID: id, ExpectedContentSha256: param.NewOpt(act.BaselineSHA)}, m.opts...)
			switch {
			case err == nil:
				st.counts.deleted++
			case isStatus(err, 404):
				// Already gone remotely too: nothing to do, and the
				// baseline entry drops.
			case isStatus(err, 409):
				// The store moved on: the baseline is kept, so the next
				// sync sees the change and pulls — the reference's answer.
				st.next.Synced[act.Path] = act.BaselineSHA
				st.counts.conflict++
			case isStatus(err, 400) && refusalKind(err) == refusalArchived:
				st.archived = true
				st.next.Synced[act.Path] = act.BaselineSHA
				st.counts.withheld++
			default:
				return false, fmt.Errorf("delete %s: %w", act.Path, err)
			}
		case memsync.Push:
			data, ok := st.contents[act.Path]
			if !ok {
				return false, fmt.Errorf("push %s: no bytes were read for it", act.Path)
			}
			if st.archived {
				// What memsync.Plan does for a pull-only sync: the edit
				// stays local, an update's baseline kept for the next sync
				// to judge against.
				if act.ID != "" {
					st.next.Synced[act.Path] = act.BaselineSHA
				}
				st.counts.withheld++
				continue
			}
			outcome, err := m.push(ctx, id, act, data)
			if err != nil {
				return false, err
			}
			switch outcome {
			case pushOK:
				st.next.Synced[act.Path] = act.LocalSHA
				st.counts.pushed++
			case pushConflict:
				if act.ID == "" {
					// A create the store's occupancy refused: it holds a
					// memory the file is in the way of — at this exact path
					// (another session created it since the listing, a race
					// the executor's row lock forecloses but the wire does
					// not), or at an ancestor or descendant, where a file and
					// a directory cannot share a name. Either way the store
					// wins, as it wins every both-sides change: the file goes,
					// and the memory's own pull lands where it was.
					slog.WarnContext(ctx, "memory file removed: the store holds a memory the file is in the way of",
						"session_id", m.sessionID, "memory_store_id", id, "path", act.Path)
					st.removals = append(st.removals, act.Path)
				} else {
					// An update whose precondition lost: the file stays as
					// written, unrecorded, so the next sync judges it
					// against the store's answer — the local edit loses.
					st.next.Synced[act.Path] = act.BaselineSHA
				}
				st.counts.conflict++
			case pushOverCap:
				// The store's state, not the file's: not remembered, so the
				// next run retries it against whatever room deletions made.
				slog.WarnContext(ctx, "memory not created: the store holds its maximum",
					"session_id", m.sessionID, "memory_store_id", id, "path", act.Path, "cap", memsync.MaxMemoriesPerStore)
				st.counts.refused++
			case pushArchived:
				slog.WarnContext(ctx, "memory store is archived; its directory's changes stay local",
					"session_id", m.sessionID, "memory_store_id", id)
				st.archived = true
				if act.ID != "" {
					st.next.Synced[act.Path] = act.BaselineSHA
				}
				st.counts.withheld++
			case pushRefused:
				// The same bytes would be refused again: remembered, and
				// retried only once the file changes — the reference's rule.
				slog.WarnContext(ctx, "memory file refused by the store; not retried until its content changes",
					"session_id", m.sessionID, "memory_store_id", id, "path", act.Path)
				st.next.Refused[act.Path] = act.LocalSHA
				if act.ID != "" {
					st.next.Synced[act.Path] = act.BaselineSHA
				}
				st.counts.refused++
			}
		}
	}
	return true, nil
}

const (
	pushOK       = "ok"
	pushConflict = "conflict"
	pushOverCap  = "over the cap"
	pushArchived = "archived"
	pushRefused  = "refused"
)

// push sends one file to the store: a create (act.ID empty), or an update
// conditioned on the head the baseline recorded — and, when that update
// finds the memory gone, a create, since the file is now the only copy (the
// reference's answer to a 404). The routes' statuses are its outcomes: a
// 409 is the occupancy rule or a lost precondition, a 400 a refusal whose
// kind refusalKind reads.
func (m *memoryStores) push(ctx context.Context, storeID string, act memsync.Action, data []byte) (string, error) {
	content := param.NewOpt(string(data))
	create := func() error {
		_, err := m.client.Beta.MemoryStores.Memories.New(ctx, storeID,
			sdk.BetaMemoryStoreMemoryNewParams{Path: act.Path, Content: content}, m.opts...)
		return err
	}
	var err error
	if act.ID == "" {
		err = create()
	} else {
		_, err = m.client.Beta.MemoryStores.Memories.Update(ctx, act.ID,
			sdk.BetaMemoryStoreMemoryUpdateParams{
				MemoryStoreID: storeID,
				Content:       content,
				Precondition: sdk.BetaManagedAgentsPreconditionParam{
					Type:          sdk.BetaManagedAgentsPreconditionTypeContentSha256,
					ContentSha256: param.NewOpt(act.BaselineSHA),
				},
			}, m.opts...)
		if isStatus(err, 404) {
			err = create()
		}
	}
	switch {
	case err == nil:
		return pushOK, nil
	case isStatus(err, 409):
		return pushConflict, nil
	case isStatus(err, 400), isStatus(err, 413):
		return refusalKind(err), nil
	default:
		return "", err
	}
}

const refusalArchived = pushArchived

// refusalKind reads a 400's reason. The reference worker remembers every
// 400 as the file's own refusal, retried once the bytes change — right for
// a path or body the store's rules refuse, wrong for the two refusals that
// are the store's state: the 2,000 cap, which the executor does not remember
// either, and an archive, which makes the whole store pull-only whatever the
// attachment asked (decision 3). The token reaches no store read, so an
// archive is learned here, from the wording internal/api's own routes use
// (memories.go: "is archived", "holds N memories"); a server that words a
// refusal otherwise gets the reference's own rule, and the file is retried
// once it changes.
func refusalKind(err error) string {
	var apierr *sdk.Error
	if !errors.As(err, &apierr) {
		return pushRefused
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(apierr.RawJSON()), &body)
	switch {
	case strings.Contains(body.Error.Message, "is archived"):
		return pushArchived
	case strings.Contains(body.Error.Message, fmt.Sprintf("holds %d memories", memsync.MaxMemoriesPerStore)):
		return pushOverCap
	}
	return pushRefused
}

// applyStore is the apply phase, the executor's applyMemory for one store:
// the store's deletions removed from the directory (and their emptied
// directories with them), then its changes, the marker if the directory was
// rebuilt, and the new baseline written in one batch — in that order,
// because a baseline that no longer names a file still present would push it
// back as new on the next sync. A write that fails leaves the old baseline,
// and the next sync re-derives what this one could not land.
func (m *memoryStores) applyStore(ctx context.Context, st *storeSync) {
	id := st.ref.MemoryStoreID
	for _, cmd := range memsync.RemoveCommands(st.ref.MountPath, st.removals) {
		res, err := m.sb.Exec(ctx, sandbox.ExecRequest{Command: cmd})
		if err != nil || res.ExitCode != 0 {
			slog.WarnContext(ctx, "memory deletions not applied; the baseline is kept for the next sync",
				"session_id", m.sessionID, "memory_store_id", id, "err", err, "exit", res.ExitCode, "stderr", strings.TrimSpace(res.Stderr))
			return
		}
	}
	// A sync that moved nothing and agrees with the baseline it read has
	// nothing to write: the common run costs the directory one listing.
	next := st.next.Encode()
	if len(st.writes) == 0 && !st.restamp && bytes.Equal(next, st.raw) {
		return
	}
	writes := st.writes
	if st.restamp {
		// A rebuilt directory gets its marker back with its files — the
		// reference worker's stampAndPull — or it would stay pull-only for
		// the sandbox's life, the agent's every later write withheld.
		writes = append(writes, sandbox.FileWrite{Path: path.Join(st.ref.MountPath, memsync.MarkerName), Data: memsync.MarkerBytes(id)})
	}
	writes = append(writes, sandbox.FileWrite{Path: baselinePath(id), Data: next})
	if err := m.sb.WriteFiles(ctx, writes); err != nil {
		slog.WarnContext(ctx, "memory changes not applied; the baseline is kept for the next sync",
			"session_id", m.sessionID, "memory_store_id", id, "err", err)
	}
}
