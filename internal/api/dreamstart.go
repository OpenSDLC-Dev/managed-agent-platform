package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/transcript"
)

// The start arm (plan 41 §4.2), in three phases because its network I/O may
// run under no row lock at all: the claim is the tick's own transaction
// (dreamClaim), the rendering below runs unlocked on the arm's budget slot,
// and the write transaction re-locks the row and commits `running` only if it
// is still `pending`. A cancel issued during the render therefore finds the
// row free and lands at once — which is what §2.6 promises for a pending
// dream.

// dreamAgentID and dreamEnvID are the platform-owned rows every dream's
// pipeline session runs on (§4.3): created lazily by the first dream on a
// fresh platform and found by every later one, hidden from every list and
// every id-addressed route, and resolvable by the runner alone. Fixed rather
// than minted, so the pair is one row each however many replicas race to
// create it — ON CONFLICT DO NOTHING then makes the second writer a no-op.
const (
	dreamAgentID = "agent_dreamrunner"
	dreamEnvID   = "env_dreamrunner"
)

// dreamAgentBody and dreamEnvBody are what POST /v1/agents and
// POST /v1/environments accept, handed to the handlers' own insert bodies
// (§4.2 step 3) so the stored rows carry the handlers' normalization —
// resolveRoster rewriting {"type":"self"} to this agent's own id and version
// among it — and no code here spells a stored shape.
//
// The model and system are placeholders: every session overrides both, which
// is how one row serves whatever model a dream names. The toolset is
// always_allow with the two web tools off (no egress is the environment's job
// too), and bash stays on because the merge stage removes and moves memory
// files and no file tool can.
const dreamAgentBody = `{
	"name": "dream",
	"model": {"id": "dream-placeholder"},
	"system": "",
	"tools": [{"type": "agent_toolset_20260401",
	           "default_config": {"enabled": true, "permission_policy": {"type": "always_allow"}},
	           "configs": [{"type": "web_fetch", "name": "web_fetch", "enabled": false},
	                       {"type": "web_search", "name": "web_search", "enabled": false}]}],
	"mcp_servers": [],
	"skills": [],
	"multiagent": {"type": "coordinator", "agents": [{"type": "self"}]}
}`

const dreamEnvBody = `{
	"name": "dream",
	"config": {"type": "cloud", "networking": {"type": "limited", "allowed_hosts": []}, "packages": {}}
}`

// dreamCloneBatch is the multi-row insert width the clone writes: a
// 2,000-memory store is four statements for the memories and four for their
// versions, not four thousand. A var for the test setter
// (dreamrunner.go:55-61's idiom), because no test store is large enough to
// make the loop run more than once at 500.
var dreamCloneBatch = 500

// dreamFileMIME is what a rendered transcript is: markdown, the form §3.2
// renders and the model reads.
const dreamFileMIME = "text/markdown"

// errDreamInternalRowArchived is the one start failure that is neither
// classified nor a retry worth burning: a database operator archived the
// hidden agent or environment, ON CONFLICT DO NOTHING cannot replace it, and
// nothing in the platform can un-archive it (§9). The dream stays `pending`
// and the runner logs the id, until a human intervenes.
var errDreamInternalRowArchived = errors.New("the dream runner's internal agent or environment is archived")

// dreamStartHookAfterRender, when non-nil, runs in the window §4.2 leaves
// unlocked — after the rendered objects are stored and before the write
// transaction re-locks the row — so a test can land a cancel there, or return
// an error and drive the unclassified rollback the claim survives. Always nil
// in production.
var dreamStartHookAfterRender func() error

// dreamStartHookInWrite, when non-nil, runs inside the write transaction with
// the dream row locked FOR UPDATE and nothing written yet, so a test can hold
// the row there and drive the contention §4.1 bounds with lock_timeout — the
// cancel that waits at the row and fails rather than hangs. Always nil in
// production.
var dreamStartHookInWrite func()

// dreamFile is one rendered file on its way into the sandbox: the row's
// filename keeps GET /v1/files legible while the dream runs, the mount is the
// short path the agent reads (§4.2 step 5 — two fields, deliberately).
type dreamFile struct {
	id, filename, mount string
	data                []byte
}

func (f dreamFile) key() string { return blob.FilesKey(f.id) }

// startDream is the claim's continuation, run after the claim committed and
// with no lock held. It never returns an error: an unclassified failure is
// this replica's to log and the next tick's to retry, and the attempt that
// exhausts dreamStartAttempts settles the dream itself.
func (s *server) startDream(ctx context.Context, d dreamRow, cfg DreamRunnerConfig) {
	// Minted here because step 4's cloned versions are attributed to a session
	// that does not exist yet. A collision fails the session INSERT like any
	// other unique violation and the next attempt mints a fresh one.
	sessionID := domain.NewID(domain.PrefixSession).String()

	files, err := s.renderDreamInputs(ctx, d)
	if err == nil {
		if h := dreamStartHookAfterRender; h != nil {
			err = h()
		}
	}
	started := false
	var created createdSession
	if err == nil {
		created, started, err = s.writeDreamStart(ctx, d, sessionID, files, cfg)
	}
	if started {
		// The write transaction has committed, so the pipeline session's own
		// post-commit half runs here — the same recordCreated a wire create,
		// a deployment fire and a scheduled fire each call after their commit.
		created.recordCreated(ctx)
		return
	}
	// Everything short of the committed start leaves the objects unreferenced:
	// a cancel that landed during the render, a classified failure settled in
	// the write transaction, or a rollback. They go best-effort, and the next
	// attempt mints fresh ids rather than reuse anything.
	s.deleteDreamBlobs(ctx, dreamFileKeys(files))
	switch {
	case err == nil:
		return
	case errors.Is(err, errDreamInternalRowArchived):
		slog.ErrorContext(ctx, "dream start blocked: un-archive the runner's internal rows",
			"dream_id", d.id, "agent_id", dreamAgentID, "environment_id", dreamEnvID)
		return
	}
	slog.ErrorContext(ctx, "dream start failed; a later tick retries",
		"dream_id", d.id, "attempts", d.attempts, "error", err)
	if d.attempts >= dreamStartAttempts {
		s.settleExhaustedDream(ctx, d, err)
	}
}

// settleExhaustedDream ends a dream whose claims are spent, with the last
// error's text (§4.2). Guarded on `pending`, so a cancel that landed in the
// meantime keeps its own terminal state.
func (s *server) settleExhaustedDream(ctx context.Context, d dreamRow, cause error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE dreams
		   SET status = 'failed', ended_at = now(), closed_at = now(), error = $2, updated_at = now()
		 WHERE id = $1 AND status = 'pending'`,
		d.id, mustJSON(dreamErrorJSON{Type: "internal_error",
			Message: fmt.Sprintf("the dream could not be started in %d attempts: %s", dreamStartAttempts, cause)}))
	if err != nil {
		slog.ErrorContext(ctx, "dream could not be settled after its last attempt", "dream_id", d.id, "error", err)
		return
	}
	if tag.RowsAffected() == 1 {
		recordDreamTransition(ctx, "failed")
	}
}

// renderDreamInputs is the unlocked phase: render each input session's
// transcript (streamed and redacted by internal/transcript), assemble
// INDEX.md, and put every object. It returns whatever it put even on failure,
// so the caller can delete it.
func (s *server) renderDreamInputs(ctx context.Context, d dreamRow) ([]dreamFile, error) {
	if s.blobs == nil {
		return nil, errors.New("object storage is not configured; a dream cannot mount its transcripts")
	}
	created, err := sessionCreationTimes(ctx, s.pool, d.inputSessionIDs)
	if err != nil {
		return nil, err
	}

	files := make([]dreamFile, 0, len(d.inputSessionIDs)+1)
	var index strings.Builder
	fmt.Fprintf(&index, "# %d transcripts — seq · session_id · created_at · turns · rendered bytes · first user message\n\n",
		len(d.inputSessionIDs))
	for i, sid := range d.inputSessionIDs {
		rendered, err := transcript.RenderDream(ctx, s.log, sid)
		if err != nil {
			return files, err
		}
		seq := i + 1
		name := strconv.Itoa(seq) + "-" + sid + ".md"
		f := dreamFile{
			id:       domain.NewID(domain.PrefixFile).String(),
			filename: "dream/" + d.id + "/" + name,
			mount:    dreamTranscriptDir + name,
			data:     rendered.Text,
		}
		files = append(files, f)
		if err := s.putDreamFile(ctx, f); err != nil {
			return files, err
		}
		index.WriteString(transcript.IndexLine(seq, rendered, created[sid]) + "\n")
	}
	idx := dreamFile{
		id:       domain.NewID(domain.PrefixFile).String(),
		filename: "dream/" + d.id + "/INDEX.md",
		mount:    dreamIndexPath,
		data:     []byte(index.String()),
	}
	files = append(files, idx)
	if err := s.putDreamFile(ctx, idx); err != nil {
		return files, err
	}
	return files, nil
}

func (s *server) putDreamFile(ctx context.Context, f dreamFile) error {
	if err := s.blobs.Put(ctx, f.key(), bytes.NewReader(f.data), int64(len(f.data)), dreamFileMIME); err != nil {
		return fmt.Errorf("store transcript %s: %w", f.id, err)
	}
	return nil
}

func dreamFileKeys(files []dreamFile) []string {
	keys := make([]string, 0, len(files))
	for _, f := range files {
		keys = append(keys, f.key())
	}
	return keys
}

// sessionCreationTimes reads the INDEX.md timestamp column. A session missing
// here renders a zero time and is settled by the write transaction's own
// existence check (step 2), which is the one place the absence is classified.
func sessionCreationTimes(ctx context.Context, db querier, ids []string) (map[string]time.Time, error) {
	rows, err := db.Query(ctx, `SELECT id, created_at FROM sessions WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]time.Time, len(ids))
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

// writeDreamStart is §4.2's write transaction, its steps in the plan's order
// so a dream is `running` only with everything it needs. It reports whether
// the start committed: everything else — a cancel that landed, a classified
// failure settled here, a rollback — leaves the rendered objects unreferenced
// for the caller to delete. On a committed start it also hands back the
// pipeline session's post-commit half, for the caller to record.
func (s *server) writeDreamStart(ctx context.Context, d dreamRow, sessionID string, files []dreamFile, cfg DreamRunnerConfig) (createdSession, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return createdSession{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setDreamLockWait(ctx, tx); err != nil {
		return createdSession{}, false, err
	}

	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM dreams WHERE id = $1 FOR UPDATE`, d.id).Scan(&status); err != nil {
		return createdSession{}, false, err
	}
	if h := dreamStartHookInWrite; h != nil {
		h()
	}
	if status != "pending" {
		// A cancel landed while the render ran, and closed the dream. Nothing
		// to write; the objects go.
		return createdSession{}, false, nil
	}

	// Steps 1 and 2, the two classified failures, before anything is written:
	// a settle here is one commit with no partial work behind it.
	store, errType, msg, err := dreamStartChecks(ctx, tx, d, cfg)
	if err != nil {
		return createdSession{}, false, err
	}
	if errType != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE dreams SET status = 'failed', ended_at = now(), closed_at = now(),
			       error = $2, updated_at = now()
			 WHERE id = $1`, d.id, mustJSON(dreamErrorJSON{Type: errType, Message: msg})); err != nil {
			return createdSession{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return createdSession{}, false, err
		}
		recordDreamTransition(ctx, "failed")
		return createdSession{}, false, nil
	}

	if err := s.ensureDreamInternalRows(ctx, tx); err != nil { // step 3
		return createdSession{}, false, err
	}
	cloneID, err := s.cloneDreamStore(ctx, tx, d, store, sessionID) // step 4
	if err != nil {
		return createdSession{}, false, err
	}
	for _, f := range files { // step 5
		if _, err := tx.Exec(ctx, `
			INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, dream_id)
			VALUES ($1, $2, $3, $4, false, $5)`,
			f.id, f.filename, dreamFileMIME, int64(len(f.data)), d.id); err != nil {
			return createdSession{}, false, err
		}
	}
	created, err := s.createDreamSession(ctx, tx, d, cloneID, sessionID, files) // step 6
	if err != nil {
		return createdSession{}, false, err
	}
	// Step 7: outputs[] and session_id land in the same commit as `running`,
	// so no client can observe a running dream with an empty outputs list.
	if _, err := tx.Exec(ctx, `
		UPDATE dreams SET status = 'running', stage = 1, session_id = $2, outputs = $3, updated_at = now()
		 WHERE id = $1`, d.id, sessionID,
		mustJSON([]any{map[string]string{"type": "memory_store", "memory_store_id": cloneID}})); err != nil {
		return createdSession{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return createdSession{}, false, err
	}
	recordDreamTransition(ctx, "running")
	return created, true, nil
}

// dreamInputStore is what step 1 read and step 4 clones from.
type dreamInputStore struct{ name, description string }

// dreamStartChecks is steps 1 and 2: the input store live and inside the byte
// cap, and every input session still present. Archived input sessions are
// accepted — their logs are intact.
func dreamStartChecks(ctx context.Context, tx pgx.Tx, d dreamRow, cfg DreamRunnerConfig) (dreamInputStore, string, string, error) {
	var in dreamInputStore
	var archivedAt *time.Time
	err := tx.QueryRow(ctx,
		`SELECT name, description, archived_at FROM memory_stores WHERE id = $1 FOR SHARE`,
		d.inputStoreID).Scan(&in.name, &in.description, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && archivedAt != nil) {
		return in, "input_memory_store_unavailable",
			"input memory store " + d.inputStoreID + " is missing or archived", nil
	}
	if err != nil {
		return in, "", "", err
	}
	var size int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(content_size_bytes), 0) FROM memories WHERE memory_store_id = $1`,
		d.inputStoreID).Scan(&size); err != nil {
		return in, "", "", err
	}
	if size > cfg.MaxInputBytes {
		return in, "input_memory_store_too_large",
			fmt.Sprintf("input memory store %s holds %d bytes, over the %d-byte limit",
				d.inputStoreID, size, cfg.MaxInputBytes), nil
	}
	missing, err := missingSessions(ctx, tx, d.inputSessionIDs)
	if err != nil {
		return in, "", "", err
	}
	if missing != "" {
		return in, "input_session_unavailable", "input session " + missing + " is missing", nil
	}
	return in, "", "", nil
}

// ensureDreamInternalRows is step 3: the hidden pair, through the create
// handlers' own insert bodies. Both inserts are ON CONFLICT DO NOTHING, so the
// first dream on a fresh platform writes them and every later one finds them —
// and an archived row is this caller's to notice, because DO NOTHING cannot
// replace one.
func (s *server) ensureDreamInternalRows(ctx context.Context, tx pgx.Tx) error {
	if _, err := s.insertEnvironmentInTx(ctx, tx, json.RawMessage(dreamEnvBody), dreamEnvID, true); err != nil {
		return err
	}
	if _, err := s.insertAgentInTx(ctx, tx, json.RawMessage(dreamAgentBody), dreamAgentID, true); err != nil {
		return err
	}
	for _, q := range []struct{ table, id string }{
		{"environments", dreamEnvID}, {"agents", dreamAgentID},
	} {
		var archivedAt *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT archived_at FROM `+q.table+` WHERE id = $1`, q.id).Scan(&archivedAt); err != nil {
			return err
		}
		if archivedAt != nil {
			return fmt.Errorf("%s %s: %w", q.table, q.id, errDreamInternalRowArchived)
		}
	}
	return nil
}

// cloneDreamStore is step 4: a new store carrying the input's content, so the
// input is never touched and the caller can discard the result. The name is
// distinct so the two stores are told apart in a list and can be mounted side
// by side; every cloned version is `created`, attributed to the pipeline
// session that does not exist yet.
func (s *server) cloneDreamStore(ctx context.Context, tx pgx.Tx, d dreamRow, in dreamInputStore, sessionID string) (string, error) {
	cloneID := domain.NewID(domain.PrefixMemoryStore).String()
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_stores (id, name, description, metadata) VALUES ($1, $2, $3, '{}'::jsonb)`,
		cloneID, dreamCloneName(in.name, d.id), in.description); err != nil {
		return "", err
	}

	type memoryCopy struct {
		path, content, sha string
		size               int64
	}
	rows, err := tx.Query(ctx, `
		SELECT path, content, content_sha256, content_size_bytes
		  FROM memories WHERE memory_store_id = $1 ORDER BY path`, d.inputStoreID)
	if err != nil {
		return "", err
	}
	var copies []memoryCopy
	for rows.Next() {
		var m memoryCopy
		if err := rows.Scan(&m.path, &m.content, &m.sha, &m.size); err != nil {
			rows.Close()
			return "", err
		}
		copies = append(copies, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	// No occupancy or count check: a valid store's paths are already mutually
	// non-occupying and within the cap, so the clone inherits both.
	actor := mustJSON(map[string]string{"type": "session_actor", "session_id": sessionID})
	// Two statements per batch, each with its own parameters: a parameter a
	// statement does not mention has no inferable type, so the two cannot
	// share one argument list however alike their rows look.
	for start := 0; start < len(copies); start += dreamCloneBatch {
		end := min(start+dreamCloneBatch, len(copies))
		memArgs, verArgs := []any{cloneID}, []any{cloneID, actor}
		var mem, ver strings.Builder
		for i, m := range copies[start:end] {
			memoryID := domain.NewID(domain.PrefixMemory).String()
			versionID := domain.NewID(domain.PrefixMemoryVersion).String()
			if i > 0 {
				mem.WriteString(", ")
				ver.WriteString(", ")
			}
			n := len(memArgs)
			memArgs = append(memArgs, memoryID, m.path, m.content, m.sha, m.size, versionID)
			fmt.Fprintf(&mem, "($%d, $1, $%d, $%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4, n+5, n+6)
			n = len(verArgs)
			verArgs = append(verArgs, versionID, memoryID, m.path, m.content, m.sha, m.size)
			fmt.Fprintf(&ver, "($%d, $1, $%d, 'created', $%d, $%d, $%d, $%d, $2)",
				n+1, n+2, n+3, n+4, n+5, n+6)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_versions (id, memory_store_id, memory_id, operation, path,
				content, content_sha256, content_size_bytes, created_by) VALUES `+ver.String(), verArgs...); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memories (id, memory_store_id, path, content, content_sha256,
				content_size_bytes, memory_version_id) VALUES `+mem.String(), memArgs...); err != nil {
			return "", err
		}
	}
	return cloneID, nil
}

// dreamCloneName is the output store's name: the input's, followed by
// " (dream <token>)". The input's portion is truncated so the whole stays
// inside memoryStoreNameMax and the suffix always survives — an input already
// at the bound would otherwise clone under its own name, and so under its own
// slug and mount.
func dreamCloneName(name, dreamID string) string {
	_, token, _ := strings.Cut(dreamID, "_")
	suffix := " (dream " + token + ")"
	if room := memoryStoreNameMax - utf8.RuneCountInString(suffix); utf8.RuneCountInString(name) > room {
		name = string([]rune(name)[:max(room, 0)])
	}
	return name + suffix
}

// createDreamSession is step 6: the pipeline session, through the same
// createSessionInTx a wire create and a scheduled fire run. Two fields of
// createSessionIn are the runner's alone and unreachable from the wire — the
// pre-minted id step 4's actor already named, and the internal bypass that
// admits the hidden pair. created_by is whatever the runner's context carries,
// which is nothing, so the session lands unattributed exactly as a scheduled
// fire's does; the dream row carries its own created_by for audit.
func (s *server) createDreamSession(ctx context.Context, tx pgx.Tx, d dreamRow, cloneID, sessionID string, files []dreamFile) (createdSession, error) {
	store := resourceInput{kind: resourceKindMemory, memoryStoreID: cloneID, access: "read_write"}
	// The mount path the prompt spells is the resource's own: the snapshot
	// createSessionInTx takes a moment below, taken here first, so no slug is
	// computed twice or by hand.
	mounted, err := snapshotMemoryStore(ctx, tx, store)
	if err != nil {
		return createdSession{}, err
	}
	inputs := []resourceInput{store}
	for _, f := range files {
		mount, err := resolveMountPath(f.mount)
		if err != nil {
			return createdSession{}, err
		}
		inputs = append(inputs, resourceInput{kind: resourceKindFile, fileID: f.id, mountPath: mount})
	}

	var instructions string
	if d.instructions != nil {
		instructions = *d.instructions
	}
	agentRaw := mustJSON(map[string]any{
		"type":   "agent_with_overrides",
		"id":     dreamAgentID,
		"model":  json.RawMessage(d.model),
		"system": dreamSystemPrompt(mounted.MountPath),
	})
	initial := mustJSON(map[string]any{
		"type": "user.message",
		"content": []any{map[string]any{
			"type": "text",
			"text": dreamStageMessage(mounted.MountPath, len(d.inputSessionIDs), instructions),
		}},
	})
	// The post-commit half goes back to startDream, which commits: a pipeline
	// session is born running with its transcripts and clone attached, and
	// those counts are the shared committer's, not the runner's own.
	return s.createSessionInTx(ctx, tx, createSessionIn{
		id: sessionID, internal: true, envID: dreamEnvID, agentRaw: agentRaw,
		resourceInputs: inputs, rawInitial: []json.RawMessage{initial},
	})
}
