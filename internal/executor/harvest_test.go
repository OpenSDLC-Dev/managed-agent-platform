package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// seedOutcome puts one outcome entry with the given result onto the session
// row, the state settleEndTurn leaves behind when it schedules a harvest.
func (h *harness) seedOutcome(t *testing.T, result string) {
	t.Helper()
	evals := []domain.OutcomeEvaluation{{
		Type:        "outcome_evaluation",
		OutcomeID:   domain.NewID(domain.PrefixOutcome),
		Description: "ship the report",
		Iteration:   1,
		Result:      result,
	}}
	b, err := json.Marshal(evals)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET outcome_evaluations = $1 WHERE id = $2`, b, h.sid.String()); err != nil {
		t.Fatal(err)
	}
}

// enqueueHarvest schedules the grading cycle's own harvest, as
// settleEndTurn's grading branch does.
func (h *harness) enqueueHarvest(t *testing.T) {
	t.Helper()
	if _, err := h.queue.EnqueueOutputsHarvest(context.Background(), h.pool, h.envID, h.sid, true); err != nil {
		t.Fatal(err)
	}
}

// enqueueIdleHarvest schedules the harvest a session's idle fold asks for, as
// the brain's enqueueIdleHarvest does — no outcome need exist, and the session
// is idle rather than running by the time the item is claimed.
func (h *harness) enqueueIdleHarvest(t *testing.T) {
	t.Helper()
	pgtest.SetSessionStatus(t, h.pool, h.sid, "idle")
	if _, err := h.queue.EnqueueOutputsHarvest(context.Background(), h.pool, h.envID, h.sid, false); err != nil {
		t.Fatal(err)
	}
}

// chainGradingOf reads the live outputs_harvest item's chain_grading flag — how
// a test tells a requeued grading harvest from the idle-tagged item it began as.
func (h *harness) chainGradingOf(t *testing.T) bool {
	t.Helper()
	var cg bool
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COALESCE((metadata->>'chain_grading')::bool, false)
		   FROM work_items WHERE session_id=$1 AND kind='outputs_harvest' AND state != 'stopped'`,
		h.sid.String()).Scan(&cg); err != nil {
		t.Fatal(err)
	}
	return cg
}

type fileRow struct {
	id, filename, mime string
	size               int64
	downloadable       bool
	scopeType, scopeID string
}

// fileRows returns every files-registry row, ordered by filename.
func (h *harness) fileRows(t *testing.T) []fileRow {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT id, filename, mime_type, size_bytes, downloadable,
		        coalesce(scope_type, ''), coalesce(scope_id, '')
		   FROM files ORDER BY filename`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []fileRow
	for rows.Next() {
		var r fileRow
		if err := rows.Scan(&r.id, &r.filename, &r.mime, &r.size, &r.downloadable, &r.scopeType, &r.scopeID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// blobBytes reads a staged deliverable's bytes back from the store.
func (h *harness) blobBytes(t *testing.T, fileID string) string {
	t.Helper()
	rc, _, err := h.blobs.Get(context.Background(), blob.FilesKey(fileID))
	if err != nil {
		t.Fatalf("blob for %s: %v", fileID, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHarvestPublishesSnapshotAndChainsGrading(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{
		outputsDir + "/report.json":      `{"npv": 42}`,
		outputsDir + "/build_dcf.py":     "print()",
		outputsDir + "/data/model":       "\x00\x01binary",
		"/mnt/session/workspace/scratch": "not an output",
	}}
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)

	h.stepOnce(t)

	rows := h.fileRows(t)
	if len(rows) != 3 {
		t.Fatalf("files rows = %d, want 3: %+v", len(rows), rows)
	}
	// Lexicographic by path: build_dcf.py, data/model, report.json.
	if rows[0].filename != "build_dcf.py" || rows[1].filename != "data/model" || rows[2].filename != "report.json" {
		t.Fatalf("filenames = %q, %q, %q; want build_dcf.py, data/model, report.json",
			rows[0].filename, rows[1].filename, rows[2].filename)
	}
	// The exact pinned-table value (the #264 symptom file): .py is absent from
	// Go's builtin table and hosts that do know it say text/x-python without
	// the charset, so this row proves the harvest wiring still consults
	// mimetab, not the process mime registry.
	if rows[0].mime != "text/x-python; charset=utf-8" {
		t.Errorf("build_dcf.py mime = %q, want text/x-python; charset=utf-8", rows[0].mime)
	}
	if rows[1].mime != "application/octet-stream" {
		t.Errorf("extensionless mime = %q, want application/octet-stream", rows[1].mime)
	}
	if !strings.HasPrefix(rows[2].mime, "application/json") {
		t.Errorf("report.json mime = %q, want application/json", rows[2].mime)
	}
	for _, r := range rows {
		if !r.downloadable {
			t.Errorf("%s: downloadable = false, want true", r.filename)
		}
		if r.scopeType != "session" || r.scopeID != h.sid.String() {
			t.Errorf("%s: scope = %q/%q, want session/%s", r.filename, r.scopeType, r.scopeID, h.sid)
		}
		want := sb.files[outputsDir+"/"+r.filename]
		if r.size != int64(len(want)) {
			t.Errorf("%s: size = %d, want %d", r.filename, r.size, len(want))
		}
		if got := h.blobBytes(t, r.id); got != want {
			t.Errorf("%s: blob bytes = %q, want %q", r.filename, got, want)
		}
	}

	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1 (grading turn chained)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
	}
}

func TestHarvestReportsProgressAsItStages(t *testing.T) {
	// The harvest is the second lane that reads a sandbox, so its item carries
	// the same stall bound (#383) — and that bound is only safe if a harvest
	// that is merely large keeps reporting. Counted rather than timed: the guard
	// itself is tested in executor_test.go, and what can silently break here is
	// the reporting, which a count pins deterministically.
	sb := &fakeSandbox{files: map[string]string{
		outputsDir + "/a.txt": "1",
		outputsDir + "/b.txt": "2",
		outputsDir + "/c.txt": "3",
	}}
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)

	ctx := context.Background()
	item, err := h.queue.Claim(ctx, queue.OutputsHarvest, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("Claim: item=%v err=%v", item, err)
	}
	sess, live, err := h.exec.sessionForRun(ctx, item)
	if err != nil || !live {
		t.Fatalf("sessionForRun: live=%v err=%v", live, err)
	}

	var reports int
	provisioned, err := h.exec.provisionSandbox(ctx, item.SessionID, sess, func() {})
	if err != nil {
		t.Fatalf("provisionSandbox: %v", err)
	}
	files, err := h.exec.collectOutputs(ctx, item, provisioned, func() { reports++ })
	if err != nil {
		t.Fatalf("collectOutputs: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("staged = %d, want 3", len(files))
	}
	// One per staged file, the listing before them, and the boundary that
	// reports the last file staging — an exact count over collectOutputs' own
	// steps (the sandbox arrives already provisioned), because a report that
	// quietly stops being made is the failure this test exists to catch.
	if reports != 5 {
		t.Errorf("progress reports = %d, want 5 (listing, %d files, pass boundary)",
			reports, len(files))
	}
}

func TestReHarvestReplacesSnapshotPerPath(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{
		outputsDir + "/report.json": "v1",
		outputsDir + "/stale.log":   "gone next cycle",
	}}
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	h.stepOnce(t)

	first := h.fileRows(t)
	if len(first) != 2 {
		t.Fatalf("first harvest rows = %d, want 2", len(first))
	}

	// Next cycle: the report changed, the log vanished, a new file appeared.
	delete(sb.files, outputsDir+"/stale.log")
	sb.files[outputsDir+"/report.json"] = "v2"
	sb.files[outputsDir+"/summary.txt"] = "fresh"
	h.enqueueHarvest(t)
	h.stepOnce(t)

	rows := h.fileRows(t)
	if len(rows) != 2 {
		t.Fatalf("re-harvest rows = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].filename != "report.json" || rows[1].filename != "summary.txt" {
		t.Fatalf("filenames = %q, %q; want report.json, summary.txt", rows[0].filename, rows[1].filename)
	}
	if got := h.blobBytes(t, rows[0].id); got != "v2" {
		t.Errorf("report.json blob = %q, want v2", got)
	}
	// The snapshot replaces per path: fresh ids, and the replaced snapshot's
	// blobs are gone from storage.
	for _, old := range first {
		if _, _, err := h.blobs.Get(context.Background(), blob.FilesKey(old.id)); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("old blob %s (%s) still stored, err = %v; want ErrNotFound", old.id, old.filename, err)
		}
	}
	if h.blobs.Len() != 2 {
		t.Errorf("stored blobs = %d, want 2", h.blobs.Len())
	}
}

func TestHarvestCapsAreGreedyAndDeterministic(t *testing.T) {
	oldFile, oldBytes, oldFiles := harvestFileCapBytes, harvestSessionCapBytes, harvestSessionCapFiles
	harvestFileCapBytes, harvestSessionCapBytes, harvestSessionCapFiles = 10, 25, 3
	t.Cleanup(func() {
		harvestFileCapBytes, harvestSessionCapBytes, harvestSessionCapFiles = oldFile, oldBytes, oldFiles
	})

	sb := &fakeSandbox{files: map[string]string{
		outputsDir + "/a": strings.Repeat("x", 8),  // admitted (total 8)
		outputsDir + "/b": strings.Repeat("x", 11), // over the per-file cap
		outputsDir + "/c": strings.Repeat("x", 9),  // admitted (total 17)
		outputsDir + "/d": strings.Repeat("x", 9),  // 17+9 > 25: excluded, walk continues
		outputsDir + "/e": strings.Repeat("x", 8),  // admitted (total 25, exactly the cap)
		outputsDir + "/f": "x",                     // file-count cap reached
	}}
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	h.stepOnce(t)

	rows := h.fileRows(t)
	var got []string
	for _, r := range rows {
		got = append(got, r.filename)
	}
	if len(rows) != 3 || rows[0].filename != "a" || rows[1].filename != "c" || rows[2].filename != "e" {
		t.Fatalf("admitted = %v, want [a c e]", got)
	}
	if h.blobs.Len() != 3 {
		t.Errorf("stored blobs = %d, want 3 (excluded files never staged)", h.blobs.Len())
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1 (caps never block grading)", got)
	}
}

func TestHarvestDiscardsWhenCycleSettled(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	// The outcome settled (a user.interrupt) while the harvest item waited:
	// the settlement wins — nothing of the harvest commits.
	h.seedOutcome(t, domain.OutcomeResultInterrupted)
	h.enqueueHarvest(t)
	h.stepOnce(t)

	if rows := h.fileRows(t); len(rows) != 0 {
		t.Errorf("files rows = %d, want 0 (discarded)", len(rows))
	}
	if h.blobs.Len() != 0 {
		t.Errorf("stored blobs = %d, want 0 (staged bytes discarded)", h.blobs.Len())
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0 (no grading turn for a settled cycle)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
	}
}

func TestHarvestReadFaultLeavesPreviousSnapshotAndItemForReclaim(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)

	// A previous cycle's published snapshot, which must survive the fault.
	prev := domain.NewID(domain.PrefixFile)
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1, 'report.json', 'application/json', 2, true, 'session', $2)`,
		prev.String(), h.sid.String()); err != nil {
		t.Fatal(err)
	}
	if err := h.blobs.Put(context.Background(), blob.FilesKey(prev.String()),
		strings.NewReader("v0"), 2, "application/json"); err != nil {
		t.Fatal(err)
	}

	sb.readErr = errors.New("daemon gone")
	var faulted error
	h.exec.onFault = func(_ *queue.Item, err error) { faulted = err }
	h.enqueueHarvest(t)
	h.stepOnce(t)

	if faulted == nil {
		t.Fatal("no fault reported for a backend read failure")
	}
	rows := h.fileRows(t)
	if len(rows) != 1 || rows[0].id != prev.String() {
		t.Errorf("rows after fault = %+v, want only the previous snapshot", rows)
	}
	if h.blobs.Len() != 1 {
		t.Errorf("stored blobs = %d, want 1 (previous snapshot only, staged bytes discarded)", h.blobs.Len())
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 1 {
		t.Errorf("outputs_harvest live = %d, want 1 (left for reclaim)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0 (no grading before a complete snapshot)", got)
	}
}

// TestHarvestLostLeaseAbortsPublish exercises the registry-commit boundary
// directly: a claimant whose lease was taken (reclaim, or a cancel) must not
// publish — the transaction rolls back whole and the staged bytes are
// discarded.
func TestHarvestLostLeaseAbortsPublish(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	item, err := h.queue.Claim(context.Background(), queue.OutputsHarvest, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %v, %v", item, err)
	}

	staged := harvestFile{path: "report.json", id: domain.NewID(domain.PrefixFile), size: 2, mime: "application/json"}
	if err := h.blobs.Put(context.Background(), blob.FilesKey(staged.id.String()),
		strings.NewReader("v1"), 2, "application/json"); err != nil {
		t.Fatal(err)
	}

	item.Lease = item.Lease.Add(-time.Hour) // someone else owns the item now
	if err := h.exec.settleHarvest(context.Background(), item, []harvestFile{staged}, true); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("settleHarvest with a lost lease = %v, want ErrLeaseLost", err)
	}

	if rows := h.fileRows(t); len(rows) != 0 {
		t.Errorf("files rows = %d, want 0 (publish rolled back)", len(rows))
	}
	if h.blobs.Len() != 0 {
		t.Errorf("stored blobs = %d, want 0 (staged bytes discarded)", h.blobs.Len())
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0", got)
	}
}

func TestHarvestWithoutBlobStoreStillChainsGrading(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	// A storage-less deploy: no blob store, so no snapshot can publish — but
	// grading must still run, transcript-only, and the previous snapshot (if
	// any) must not be wiped by a harvest that cannot replace it.
	h.exec = New(h.pool, h.log, h.queue, h.prov, nil, h.cipher, Config{})
	prev := domain.NewID(domain.PrefixFile)
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1, 'earlier.txt', 'text/plain', 2, true, 'session', $2)`,
		prev.String(), h.sid.String()); err != nil {
		t.Fatal(err)
	}
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	h.stepOnce(t)

	rows := h.fileRows(t)
	if len(rows) != 1 || rows[0].id != prev.String() {
		t.Errorf("rows = %+v, want only the pre-existing snapshot", rows)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1 (grading proceeds transcript-only)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
	}
}

func TestHarvestStaleSessionDrains(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'terminated' WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}

	h.stepOnce(t)

	if rows := h.fileRows(t); len(rows) != 0 {
		t.Errorf("files rows = %d, want 0 (dead session never harvests)", len(rows))
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (drained)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0", got)
	}
}

func TestHarvestTruncatedListingDegradesToSortedPrefix(t *testing.T) {
	// A tree big enough to overflow the exec output cap would fault every
	// reclaim identically — the tree is static during grading — wedging the
	// session. A truncated listing instead publishes its complete entries (the
	// glob's sorted prefix) and drops the trailing mid-path fragment, even
	// when that fragment happens to name a file that exists ("repo" here is a
	// real file but arrived as a cut of "report.json").
	sb := &fakeSandbox{files: map[string]string{
		outputsDir + "/a.txt":       "alpha",
		outputsDir + "/b.txt":       "beta",
		outputsDir + "/repo":        "a real file the fragment must not publish",
		outputsDir + "/report.json": "cut off by the cap",
	}}
	sb.execStdout = "a.txt\x00b.txt\x00repo"
	sb.execTruncated = true
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	h.stepOnce(t)

	rows := h.fileRows(t)
	if len(rows) != 2 || rows[0].filename != "a.txt" || rows[1].filename != "b.txt" {
		t.Fatalf("rows = %+v, want exactly a.txt and b.txt (fragment dropped)", rows)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1 (grading chained)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
	}
}

func TestHarvestHostileListingPathsAreExcluded(t *testing.T) {
	// The sandbox is agent-writable, so the listing is untrusted output: a
	// path that escapes the outputs tree must never become a registry row or
	// a read outside it.
	sb := &fakeSandbox{files: map[string]string{
		outputsDir + "/ok.json": "fine",
	}}
	sb.execStdout = "ok.json\x00../../../etc/passwd\x00/etc/shadow\x00a/../b\x00"
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	h.stepOnce(t)

	rows := h.fileRows(t)
	if len(rows) != 1 || rows[0].filename != "ok.json" {
		t.Fatalf("rows = %+v, want only ok.json", rows)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1", got)
	}
}

func TestValidHarvestPathRejectsEscapes(t *testing.T) {
	// Direct guard coverage: the harness tests above cannot distinguish a
	// validator rejection from the fake sandbox's file-not-found skip, so a
	// lost validator would pass them (proven by mutation during slice-4
	// verification). This test fails the moment validHarvestPath stops
	// rejecting an escape shape.
	accept := []string{
		"ok.json", "sub/model.bin", "a b/c.txt", ".hidden", "..data",
		"报告.txt",                        // multibyte: 6 runes, 10 bytes
		strings.Repeat("中", 255),        // exactly the per-segment rune cap, 765 bytes
		"d/" + strings.Repeat("y", 255), // the cap is per segment, not per path
	}
	for _, p := range accept {
		if !validHarvestPath(p) {
			t.Errorf("validHarvestPath(%q) = false, want true", p)
		}
	}
	reject := []string{
		"",
		"/etc/shadow",
		"..",
		"../peer",
		"../../../etc/passwd",
		"a/../b",
		"a//b",
		"./a",
		"a/",
		"a/./b",
		strings.Repeat("x", 1025),
		"\xff\xfe.bin",  // invalid UTF-8: would fault the filename bind (#135 class)
		"bad\nname.txt", // control character, the upload rule's U+0000–U+001F
		"a\x01b",
		"win|pipe.txt", // the upload rule's forbidden set
		"colon:name.txt",
		`back\slash.txt`,
		"a<b.txt",
		"a>b.txt",
		"q?.txt",
		"star*.txt",
		`quote".txt`,
		"d/" + strings.Repeat("y", 256), // a segment beyond the 255-rune filename cap
	}
	for _, p := range reject {
		if validHarvestPath(p) {
			t.Errorf("validHarvestPath(%q) = true, want false", p)
		}
	}
}

func TestParseListingSortsDedupesAndReportsRejects(t *testing.T) {
	out := "b.txt\x00a.txt\x00b.txt\x00/abs\x00../up\x00\x00\xffraw.bin\x00c/d.bin\x00"
	paths, rejected := parseListing(out)
	if want := []string{"a.txt", "b.txt", "c/d.bin"}; !slices.Equal(paths, want) {
		t.Errorf("paths = %q, want %q", paths, want)
	}
	if want := []string{"/abs", "../up", "\xffraw.bin"}; !slices.Equal(rejected, want) {
		t.Errorf("rejected = %q, want %q", rejected, want)
	}
}

// A session that idles with no outcome ever defined harvests all the same
// (docs/plan/38, #263): the deliverables reach the files registry, and nothing
// chains a turn — there is no grading cycle waiting on this snapshot.
func TestIdleHarvestPublishesSnapshotWithoutChainingATurn(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{
		outputsDir + "/build_deck.py": "print()",
		outputsDir + "/output.pptx":   "deck bytes",
	}}
	h := newHarness(t, sb)
	// The session ran tools and still holds its sandbox — what an idle harvest
	// reuses; it provisions nothing of its own (see TestToollessIdleHarvestNeverProvisions).
	h.prov.markRunning(h.sid)
	h.enqueueIdleHarvest(t)

	h.stepOnce(t)

	rows := h.fileRows(t)
	if len(rows) != 2 {
		t.Fatalf("files rows = %d, want 2: %+v", len(rows), rows)
	}
	if h.prov.provisions != 0 {
		t.Errorf("Provision called %d times, want 0 (an idle harvest reuses a live sandbox)", h.prov.provisions)
	}
	for _, r := range rows {
		if !r.downloadable || r.scopeType != "session" || r.scopeID != h.sid.String() {
			t.Errorf("%s: downloadable=%v scope=%q/%q", r.filename, r.downloadable, r.scopeType, r.scopeID)
		}
		if got := h.blobBytes(t, r.id); got != sb.files[outputsDir+"/"+r.filename] {
			t.Errorf("%s: blob bytes = %q", r.filename, got)
		}
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0 (nothing is grading this snapshot)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
	}
}

// The fence is not "any non-evaluating state discards": an idle harvest on a
// session whose earlier outcome already settled still publishes, because what
// decides the discard is the item's own trigger, not the absence of a live
// cycle.
func TestIdleHarvestIgnoresUnrelatedTerminalOutcome(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	h.prov.markRunning(h.sid)
	h.seedOutcome(t, domain.OutcomeResultSatisfied)
	h.enqueueIdleHarvest(t)

	h.stepOnce(t)

	if rows := h.fileRows(t); len(rows) != 1 {
		t.Errorf("files rows = %+v, want the one deliverable", rows)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0 (the outcome settled long ago)", got)
	}
}

// The decision-2 race, end to end. An idle-tagged item is still queued when a
// fresh outcome reaches its own grading enqueue: the two flavors share one live
// slot, so that enqueue is swallowed and the stale item is all the cycle has.
// Reading the session's live outcome state before the item's tag is what makes
// it serve — it publishes and chains the grading turn, instead of completing
// silently and stranding the outcome in evaluating forever.
func TestFreshOutcomeGradingReclaimsAStaleIdleHarvestSlot(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	// The fresh outcome's own agent turn ran and left a live sandbox, which the
	// reclaimed idle item reuses to publish the grader's snapshot.
	h.prov.markRunning(h.sid)
	h.enqueueIdleHarvest(t)

	// What settleEndTurn's grading branch commits on the next turn: the entry
	// flips to evaluating and its own harvest enqueue runs — and finds the
	// idle-tagged item still live.
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	pgtest.SetSessionStatus(t, h.pool, h.sid, "running")
	created, err := h.queue.EnqueueOutputsHarvest(context.Background(), h.pool, h.envID, h.sid, true)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("the grading enqueue created a second item; the two flavors must share one live slot")
	}

	h.stepOnce(t)

	if rows := h.fileRows(t); len(rows) != 1 {
		t.Errorf("files rows = %+v, want the deliverable the grader will read", rows)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1 (the grading turn the fresh cycle waits on)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
	}
}

// The idle carve-out in sessionForRun admits an idle session and nothing more:
// a terminated one still drains its item without reading a sandbox.
func TestIdleHarvestDeadSessionDrains(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	h.enqueueIdleHarvest(t)
	pgtest.SetSessionStatus(t, h.pool, h.sid, "terminated")

	h.stepOnce(t)

	if rows := h.fileRows(t); len(rows) != 0 {
		t.Errorf("files rows = %d, want 0 (a dead session never harvests)", len(rows))
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (drained)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0", got)
	}
}

// The cost guard (docs/plan/38 decision 8, #263 review): a tool-less cloud
// session that idles has no live sandbox, and an idle harvest must not spin one
// up just to walk an empty outputs tree. Provision is wired to fail, so a
// stray provisioning attempt is a loud test failure rather than a silent cost;
// the item still completes, and a previous snapshot is left untouched (no walk,
// no replace).
func TestToollessIdleHarvestNeverProvisions(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	h.prov.provisionErr = errors.New("Provision must not be called for a tool-less idle harvest")
	// No markRunning: the session never ran a tool, so Attach finds nothing.
	prev := domain.NewID(domain.PrefixFile)
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1, 'earlier.txt', 'text/plain', 2, true, 'session', $2)`,
		prev.String(), h.sid.String()); err != nil {
		t.Fatal(err)
	}
	h.enqueueIdleHarvest(t)

	h.stepOnce(t)

	if h.prov.provisions != 0 {
		t.Errorf("Provision called %d times, want 0 — a tool-less idle harvest must reach no sandbox", h.prov.provisions)
	}
	rows := h.fileRows(t)
	if len(rows) != 1 || rows[0].id != prev.String() {
		t.Errorf("rows = %+v, want only the pre-existing snapshot (no walk, no delete-all)", rows)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed as a no-op, not reclaim-looping)", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0", got)
	}
}

// Corrupt outcome state must not wedge an idle harvest (#263 review). runTurn
// idles a session whose outcome_evaluations will not decode ("session outcome
// state is corrupt"), which now schedules an idle harvest; that harvest reads
// the same undecodable column, and treating the decode failure as a fault would
// reclaim-loop it forever. It settles as a no-op instead.
func TestIdleHarvestOnCorruptOutcomeStateDrains(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET outcome_evaluations = '{}'::jsonb WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}
	h.enqueueIdleHarvest(t)

	h.stepOnce(t)

	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed, not reclaim-looping on corrupt state)", got)
	}
	if rows := h.fileRows(t); len(rows) != 0 {
		t.Errorf("files rows = %d, want 0 (nothing published)", len(rows))
	}
}

// A storage-less deploy has nothing to publish and, on this trigger, nothing to
// chain either — so the item completes and the session stays idle, where the
// grading flavor would still have chained its turn.
func TestIdleHarvestWithoutBlobStoreCompletesSilently(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	h.exec = New(h.pool, h.log, h.queue, h.prov, nil, h.cipher, Config{})
	prev := domain.NewID(domain.PrefixFile)
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1, 'earlier.txt', 'text/plain', 2, true, 'session', $2)`,
		prev.String(), h.sid.String()); err != nil {
		t.Fatal(err)
	}
	h.enqueueIdleHarvest(t)

	h.stepOnce(t)

	rows := h.fileRows(t)
	if len(rows) != 1 || rows[0].id != prev.String() {
		t.Errorf("rows = %+v, want only the pre-existing snapshot", rows)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0 (no cycle to chain)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
	}
}

// A transient Attach fault is NOT "no sandbox" (docs/plan/38 decision 8, #263
// review): a one-off Docker inspect hiccup or a K8s API timeout must be
// propagated so the lease-recovery reclaim retries the harvest, not silently
// taken for an empty session that drops its only harvest. ErrNotFound alone
// no-ops; anything else faults and leaves the item live.
func TestIdleHarvestTransientAttachErrorReclaims(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	// The session did write deliverables and still holds its sandbox, but Attach
	// faults transiently rather than answering ErrNotFound.
	h.prov.markRunning(h.sid)
	h.prov.attachErr = errors.New("docker inspect: connection reset")
	var faulted error
	h.exec.onFault = func(_ *queue.Item, err error) { faulted = err }
	h.enqueueIdleHarvest(t)

	h.stepOnce(t)

	if faulted == nil {
		t.Fatal("no fault reported for a transient Attach error (it was taken for no-sandbox and dropped)")
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 1 {
		t.Errorf("outputs_harvest live = %d, want 1 (a transient reclaims, not silently drops)", got)
	}
	if rows := h.fileRows(t); len(rows) != 0 {
		t.Errorf("files rows = %d, want 0 (nothing published on a faulted attach)", len(rows))
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0", got)
	}
}

// Corrupt resolved_agent must not wedge an idle harvest (#263 review). An idle
// attach-only pass reads none of the environment config, resolved agent or
// resources — it attaches by session id alone — so sessionForRun skips those
// decodes for it. runTurn already idles a session whose resolved_agent will not
// decode, which is what scheduled this harvest; re-decoding it here and faulting
// would reclaim-loop it forever, exactly as the outcome-decode fault would.
func TestIdleHarvestOnCorruptAgentStateDrains(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	// A bare JSON string where sessionForRun's agent struct is expected: the
	// decode that faults is resolved_agent, before any sandbox is touched.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = '"bad"'::jsonb WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}
	var faulted error
	h.exec.onFault = func(_ *queue.Item, err error) { faulted = err }
	h.enqueueIdleHarvest(t)

	h.stepOnce(t)

	if faulted != nil {
		t.Errorf("idle harvest faulted on corrupt resolved_agent: %v (it must skip the decode it does not need)", faulted)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (drained, not reclaim-looping on corrupt agent state)", got)
	}
	if rows := h.fileRows(t); len(rows) != 0 {
		t.Errorf("files rows = %d, want 0 (nothing published)", len(rows))
	}
}

// The decode-skip is gated to idle harvests alone: a grading harvest provisions
// a fresh sandbox and needs the full run state, so it must still decode it. Pin
// that its provision carries the environment's networking, which only a decode
// surfaces — an over-broad skip would provision with an empty spec. Guards the
// `!item.ChainGrading` half of the sessionForRun carve-out (removing it fails
// this test).
func TestGradingHarvestStillDecodesTheRunState(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "v1"}}
	h := newHarness(t, sb)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE environments SET config = '{"type":"cloud","networking":{"type":"limited","allowed_hosts":["example.com"]}}'
		   WHERE id = $1`, h.envID.String()); err != nil {
		t.Fatal(err)
	}
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)

	h.stepOnce(t)

	if got := h.prov.lastSpec.Networking.Type; got != domain.NetLimited {
		t.Errorf("grading harvest provision networking = %q, want %q (the run state must be decoded, not skipped)",
			got, domain.NetLimited)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1 (grading still chains its turn)", got)
	}
}

// The decision-2 reclaim race when the sandbox is GONE (#263 review). An
// idle-tagged item holds the one live harvest slot; a fresh outcome turns
// evaluating and its own grading enqueue is swallowed by that slot. But this
// pass attaches to no live sandbox (none running), so it walked nothing — and
// chaining the grader against a stale or empty snapshot is exactly what the
// pre-idle design (which always provisioned) never did. The settlement requeues
// the item as a grading harvest instead; once it can provision, it publishes a
// fresh snapshot and chains the grader then. (The live-sandbox variant,
// TestFreshOutcomeGradingReclaimsAStaleIdleHarvestSlot, chains immediately.)
func TestNoLiveSandboxGradingCollisionRequeuesInsteadOfGradingStale(t *testing.T) {
	sb := &fakeSandbox{files: map[string]string{outputsDir + "/report.json": "fresh"}}
	h := newHarness(t, sb)
	// No markRunning: an idle attach finds no live sandbox to walk.
	h.enqueueIdleHarvest(t)

	// The fresh outcome reaches evaluating; its grading enqueue is swallowed by
	// the idle item's live slot (the two flavors share one).
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	pgtest.SetSessionStatus(t, h.pool, h.sid, "running")
	created, err := h.queue.EnqueueOutputsHarvest(context.Background(), h.pool, h.envID, h.sid, true)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("the grading enqueue created a second item; the flavors must share one live slot")
	}

	// Settle the stale, no-sandbox idle item: it must NOT chain the grader.
	h.stepOnce(t)

	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0 (no grader chained against a snapshot never walked)", got)
	}
	if rows := h.fileRows(t); len(rows) != 0 {
		t.Errorf("files rows = %d, want 0 (nothing walked, nothing published)", len(rows))
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 1 {
		t.Fatalf("outputs_harvest live = %d, want 1 (requeued as grading, not completed)", got)
	}
	if !h.chainGradingOf(t) {
		t.Error("requeued harvest chain_grading = false, want true (it must provision and walk on its next claim)")
	}

	// The requeued grading harvest provisions a sandbox, walks the current tree,
	// publishes it, and chains the grader.
	h.stepOnce(t)

	if rows := h.fileRows(t); len(rows) != 1 || rows[0].filename != "report.json" {
		t.Errorf("files rows = %+v, want the freshly-walked snapshot", rows)
	}
	if h.prov.provisions != 1 {
		t.Errorf("Provision called %d times, want 1 (the requeued grading harvest provisions)", h.prov.provisions)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn live = %d, want 1 (grader chained after a fresh walk)", got)
	}
	if got := h.liveOf(t, queue.OutputsHarvest); got != 0 {
		t.Errorf("outputs_harvest live = %d, want 0 (completed)", got)
	}
}
