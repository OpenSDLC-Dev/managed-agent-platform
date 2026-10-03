package api_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The request-path removers #703 records. Each deleted its rows and then
// deleted its objects best-effort, so a store that refused left bytes no tier
// could enumerate: the ids were the objects' only names, and the rows that held
// them were already gone. They now write the debt down on the deleting
// transaction, which deleteSession and the expired-file sweep already did
// (plan 50 decision 2).
//
// Every rung below runs against a store that refuses every delete, because a
// store that works cannot tell the two designs apart — both end with the object
// gone. The refusal is the whole difference: the old shape logged a warning and
// lost the key, the new one owes it.

// TestDeletingAFileOwesItsObject is the one site that had no transaction of its
// own: deleteFile removed its row with a bare pool Exec, so decision 2's "on
// the transaction" had nothing to ride and one had to be opened for it.
func TestDeletingAFileOwesItsObject(t *testing.T) {
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	id := s.uploadFile(t, "one.md", nil, "bytes")["id"].(string)
	store.setRefusing(true)

	if status, res := s.do(http.MethodDelete, "/v1/files/"+id, nil); status != http.StatusOK {
		t.Fatalf("delete the file: %d %v", status, res)
	}

	want := []string{blob.FilesKey(id)}
	if got := pendingKeys(t, s.pool); !slices.Equal(got, want) {
		t.Fatalf("the delete owes %v, want the file's object %v: a refused delete left the key nowhere, which is the orphan #703 is about", got, want)
	}
	// And the request path did not touch the store at all, which is the other
	// half of plan 50's shape: the object is the sweeper's to remove, so a
	// store having a bad day can no longer make a delete slow as well as lossy.
	if got := store.attempts(); len(got) != 0 {
		t.Fatalf("the request path deleted %v; plan 50 leaves every object to the sweeper", got)
	}
}

// TestDeletingASkillVersionOwesItsArchive and its cascade sibling below are the
// pair a reviewer on PR #704 found after this issue was filed. Both already ran
// their object delete on context.WithoutCancel, arguing that a client which
// gave up after the commit must not decide whether the object goes. Enqueueing
// subsumes that argument rather than contradicting it: a queued key needs no
// live context at all.
func TestDeletingASkillVersionOwesItsArchive(t *testing.T) {
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	id := s.createSkill(t)["id"].(string)
	first := s.latestVersion(t, id)
	s.addSkillVersion(t, id)
	store.setRefusing(true)

	if status, res := s.do(http.MethodDelete, "/v1/skills/"+id+"/versions/"+first, nil); status != http.StatusOK {
		t.Fatalf("delete the version: %d %v", status, res)
	}

	want := []string{skills.BlobKey(id, first)}
	if got := pendingKeys(t, s.pool); !slices.Equal(got, want) {
		t.Fatalf("the version delete owes %v, want that version's archive %v", got, want)
	}
	// The retired disconnect rung was table-driven over the cascade and this
	// single-version delete alike, so the structural replacement has to cover
	// both: a regression that put the context.WithoutCancel discard back after
	// this commit would still enqueue, and only this assertion would see it.
	if got := store.attempts(); len(got) != 0 {
		t.Fatalf("the request path deleted %v; plan 50 leaves every object to the sweeper", got)
	}
}

// TestDeletingASkillOwesEveryVersionsArchive is the cascade, and the site whose
// old shape could lose the most at once: N sequential deletes with nothing left
// to answer the client with, so a store having a bad day orphaned not the odd
// archive the old note accepted but every one the skill still had.
func TestDeletingASkillOwesEveryVersionsArchive(t *testing.T) {
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	id := s.createSkill(t)["id"].(string)
	first := s.latestVersion(t, id)
	s.addSkillVersion(t, id)
	second := s.latestVersion(t, id)
	if first == second {
		t.Fatalf("the second upload did not become a new version: both are %q", first)
	}
	store.setRefusing(true)

	if status, res := s.do(http.MethodDelete, "/v1/skills/"+id, nil); status != http.StatusOK {
		t.Fatalf("delete the skill: %d %v", status, res)
	}

	want := []string{skills.BlobKey(id, first), skills.BlobKey(id, second)}
	slices.Sort(want)
	if got := pendingKeys(t, s.pool); !slices.Equal(got, want) {
		t.Fatalf("the cascade owes %v, want both versions' archives %v", got, want)
	}
	// And it attempted no delete of its own, which is what retires the rung
	// that used to disconnect a client mid-sweep: there is no sweep on the
	// request path to disconnect from, so the half-state it guarded against —
	// rows gone, archives orphaned — has no window left to happen in.
	if got := store.attempts(); len(got) != 0 {
		t.Fatalf("the request path deleted %v; plan 50 leaves every object to the sweeper", got)
	}
}

// TestTheDreamCloseWakesTheDrainThroughItsRunner is the one rung here that
// composes both halves — the close writing the debt down and the drain paying
// it — and the only one that has to, because the seam it covers is the wiring
// rather than the arm. The wake this change added to the closing arm was dead
// on arrival: StartDreamRunner built its server with no queue in it, so Wake()
// returned on its nil guard in the only process that runs the arm. Every other
// dream rung reaches that arm through DreamTickForTest, which builds its own
// server and would pass either way; a rung that picks its own seam agrees with
// the code instead of checking it. So this one starts the real loop.
//
// The sweep's own cadence is pushed out of reach for the same reason. Left at a
// minute it would eventually remove the bytes whether or not anything woke it,
// and the rung would pass against the defect it exists for.
func TestTheDreamCloseWakesTheDrainThroughItsRunner(t *testing.T) {
	t.Cleanup(api.SetObjectDeleteIntervalForTest(10 * time.Minute))
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, _ := startedDream(t, s, body)
	fileIDs := dreamFileIDs(t, s, dreamID)
	if len(fileIDs) == 0 {
		t.Fatal("the dream owns no files, so the close would owe nothing and the drain would have nothing to pay")
	}
	atLastStage(t, s, dreamID)

	// The sweeper first, so it is already parked on the queue when the close
	// wakes it; then the runner, handed that same queue the way main.go hands
	// it one.
	q := startSweeper(t, s, s.blobs)
	cfg := dreamCfg()
	cfg.TickInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); api.StartDreamRunner(ctx, s.pool, s.blobs, nil, q, cfg) }()
	defer func() { cancel(); waitForStop(t, done) }()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner never closed the dream, so nothing here can be said about its wake")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// From here the interval is ten minutes away, so bytes that go within this
	// window went because the close woke the queue the runner was given.
	awaitDrained(t, s.pool, "the closing arm's wake")
	for _, id := range fileIDs {
		if _, _, err := s.blobs.Get(context.Background(), blob.FilesKey(id)); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("the transcript object for %s outlived the drain the close woke: %v", id, err)
		}
	}
}

// TestEveryObjectDeleteBeginsReadCommitted: migration 0046's reference count
// refuses a files/ key under REPEATABLE READ and SERIALIZABLE (#578), and a
// database may default to one of them. Every transaction that owes an
// object names READ COMMITTED when it begins (store.BeginObjectDelete), so each
// remover here still deletes on such a database: a file, a session, a skill
// version and a skill, the expiry sweep, and a dream's close. The two skill
// deletes owe no files/ key and would pass at any level. internal/executor's
// TestAHarvestBeginsReadCommitted holds the harvest.
func TestEveryObjectDeleteBeginsReadCommitted(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	// The fixtures go in at the default; the deletes are what is under test.
	agentID, envID := readableFixture(t, s)
	uploadID := uploadOneFile(t, s, "in.txt")
	expiringID := uploadOneFile(t, s, "old.txt")
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": uploadID}},
	})["id"].(string)
	skillID := s.createSkill(t)["id"].(string)
	first := s.latestVersion(t, skillID)
	s.addSkillVersion(t, skillID)
	second := s.latestVersion(t, skillID)
	_, body := seededDreamBody(t, s)
	dreamID, _ := startedDream(t, s, body)
	transcripts := dreamFileIDs(t, s, dreamID)
	atLastStage(t, s, dreamID)
	if _, err := s.pool.Exec(ctx,
		`UPDATE files SET expires_at = now() - interval '31 days' WHERE id = $1`, expiringID); err != nil {
		t.Fatal(err)
	}

	pgtest.DefaultRepeatableRead(t, s.pool)
	plain, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var iso string
	err = plain.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&iso)
	_ = plain.Rollback(ctx)
	if err != nil || iso != "repeatable read" {
		t.Fatalf("a plain transaction is %q (err %v), want repeatable read", iso, err)
	}

	for _, d := range []string{
		"/v1/files/" + uploadID,
		"/v1/sessions/" + sid,
		"/v1/skills/" + skillID + "/versions/" + second,
		"/v1/skills/" + skillID,
	} {
		if status, res := s.do(http.MethodDelete, d, nil); status != http.StatusOK {
			t.Errorf("DELETE %s = %d %v", d, status, res)
		}
	}
	if n, err := api.PurgeExpiredFilesForTest(ctx, s.pool, 30*24*time.Hour); err != nil || n != 1 {
		t.Errorf("the expiry sweep took %d rows (err %v), want the expired upload", n, err)
	}
	tick(t, s) // arm 10 completes the dream
	tick(t, s) // arm 1 closes it, deleting its transcripts
	if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt == nil {
		t.Error("the dream's close did not commit")
	}

	owed := pendingKeys(t, s.pool)
	want := []string{
		blob.FilesKey(uploadID), // its session's copy, the last row naming it, went with the session
		blob.FilesKey(expiringID),
		blob.SessionCheckpointKey(sid),
		skills.BlobKey(skillID, first),
		skills.BlobKey(skillID, second),
	}
	for _, id := range transcripts {
		want = append(want, blob.FilesKey(id))
	}
	for _, k := range want {
		if !slices.Contains(owed, k) {
			t.Errorf("the queue %v does not owe %s", owed, k)
		}
	}
}

// The previous build begins every transaction at the database's default, and
// migration 0046's reference count refuses a files/ key under REPEATABLE READ
// and SERIALIZABLE (#578), before it counts. So on a database defaulting to
// REPEATABLE READ, each of that build's transactions that enqueues a files/
// key fails, even one whose key the count would drop. Here that is all five
// the migration lists: the delete of an upload a session's copy still names,
// and of that session, whose tombstone trigger enqueues the copy's key, which
// the upload still names, so neither would owe anything; an expiry sweep that
// takes a row; a harvest replacing an output; and a dream's close deleting its
// transcripts. Its skill delete and the delete of a session holding no file
// enqueue only a skill archive and a checkpoint, which no files row can name,
// and succeed as before 0046.
func TestAPreviousBuildOnAStricterDefaultFailsEveryTransactionEnqueuingAFilesKey(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	agentID, envID := readableFixture(t, s)
	uploadID := uploadOneFile(t, s, "in.txt")
	expiringID := uploadOneFile(t, s, "old.txt")
	fileless := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	holding := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": uploadID}},
	})["id"].(string)
	output := domain.NewID("file").String()
	if _, err := s.pool.Exec(ctx, pgtest.PrevHarvestInsertSQL, output, "out.txt", "text/plain", 1, holding); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE files SET expires_at = now() - interval '31 days' WHERE id = $1`, expiringID); err != nil {
		t.Fatal(err)
	}
	skillID := s.createSkill(t)["id"].(string)
	version := s.latestVersion(t, skillID)
	_, body := seededDreamBody(t, s)
	dreamID, _ := startedDream(t, s, body)
	transcripts := dreamFileIDs(t, s, dreamID)
	if len(transcripts) == 0 {
		t.Fatal("the dream wrote no transcripts, so its close would enqueue nothing")
	}
	pgtest.DefaultRepeatableRead(t, s.pool)

	if err := prevDeleteSkill(ctx, s.pool, skillID); err != nil {
		t.Errorf("the previous build's skill delete: %v", err)
	}
	if err := prevDeleteSession(ctx, s.pool, fileless); err != nil {
		t.Errorf("the previous build's delete of a session holding no file: %v", err)
	}
	reharvest := func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, pgtest.PrevHarvestInsertSQL,
			domain.NewID("file").String(), "out.txt", "text/plain", 2, holding)
		return err
	}
	for what, err := range map[string]error{
		"the previous build's delete of a session holding a copy": prevDeleteSession(ctx, s.pool, holding),
		"the previous build's delete of an upload":                prevDeleteFile(ctx, s.pool, uploadID),
		"the previous build's expiry sweep": prevRemoveFiles(ctx, s.pool, nil,
			pgtest.PrevPurgeSQL, (30 * 24 * time.Hour).Seconds(), 1000),
		"the previous build's harvest replacing an output": prevRemoveFiles(ctx, s.pool, reharvest,
			pgtest.PrevHarvestDeleteSQL, holding),
		"the previous build's dream close": prevRemoveFiles(ctx, s.pool, nil,
			pgtest.PrevDreamFilesDeleteSQL, dreamID),
	} {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "25000" {
			t.Errorf("%s => %v, want invalid_transaction_state (25000)", what, err)
		}
	}

	owed := pendingKeys(t, s.pool)
	want := []string{skills.BlobKey(skillID, version), blob.SessionCheckpointKey(fileless)}
	slices.Sort(want)
	if !slices.Equal(owed, want) {
		t.Errorf("queue = %v, want %v: the refused transactions owe nothing", owed, want)
	}
	var left int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE id = ANY($1) OR scope_id = $2`,
		append([]string{uploadID, expiringID}, transcripts...), holding).Scan(&left); err != nil {
		t.Fatal(err)
	}
	// The upload, the expired upload, the transcripts, and the session's copy
	// and output.
	if want := 2 + len(transcripts) + 2; left != want {
		t.Errorf("%d of the %d rows the refused transactions deleted are left, want all: they rolled back", left, want)
	}
}

// prevRemoveFiles runs one of the previous build's removers that deletes files
// rows by a statement returning their ids and then enqueues their keys, in a
// transaction begun as that build begins it: its expiry sweep, its harvest
// replacing a snapshot, which writes the new one (then) in between, and its
// dream close, past the transcripts it deletes.
func prevRemoveFiles(ctx context.Context, pool *pgxpool.Pool, then func(pgx.Tx) error, del string, args ...any) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, del, args...)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("deleted no row, so it enqueues nothing")
	}
	if then != nil {
		if err := then(tx); err != nil {
			return err
		}
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = blob.FilesKey(id)
	}
	if _, err := tx.Exec(ctx, pgtest.PrevObjectEnqueueSQL, keys); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// prevDeleteFile runs the previous build's DELETE /v1/files/{id} past its
// reads, in a transaction begun as that build begins it.
func prevDeleteFile(ctx context.Context, pool *pgxpool.Pool, id string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, pgtest.PrevFileDeleteSQL, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, pgtest.PrevObjectEnqueueSQL, []string{blob.FilesKey(id)}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// prevDeleteSkill runs the previous build's DELETE /v1/skills/{id} past its
// refusals, in a transaction begun as that build begins it.
func prevDeleteSkill(ctx context.Context, pool *pgxpool.Pool, id string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var source string
	if err := tx.QueryRow(ctx, pgtest.PrevSkillLockSQL, id).Scan(&source); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, pgtest.PrevSkillVersionsDeleteSQL, id)
	if err != nil {
		return err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, pgtest.PrevSkillDeleteSQL, id); err != nil {
		return err
	}
	keys := make([]string, 0, len(versions))
	for _, v := range versions {
		keys = append(keys, skills.BlobKey(id, v))
	}
	if _, err := tx.Exec(ctx, pgtest.PrevObjectEnqueueSQL, keys); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// addSkillVersion uploads a second bundle to an existing skill, so a delete has
// more than one archive to be on the hook for.
func (s *tserver) addSkillVersion(t *testing.T, id string) {
	t.Helper()
	ct, body := skillForm(t, nil, []upFile{
		{name: "financial-skill/SKILL.md", content: testSkillMD},
		{name: "financial-skill/reference.md", content: "revised notes"},
	})
	if status, obj := s.doForm("POST", "/v1/skills/"+id+"/versions", ct, body); status != http.StatusOK {
		t.Fatalf("add a skill version: %d %v", status, obj)
	}
}
