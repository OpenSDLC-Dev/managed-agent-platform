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

func (h *harness) enqueueHarvest(t *testing.T) {
	t.Helper()
	if _, err := h.queue.Enqueue(context.Background(), h.pool, h.envID, h.sid, queue.OutputsHarvest); err != nil {
		t.Fatal(err)
	}
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
		outputsDir + "/data/model":       "\x00\x01binary",
		"/mnt/session/workspace/scratch": "not an output",
	}}
	h := newHarness(t, sb)
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)

	h.stepOnce(t)

	rows := h.fileRows(t)
	if len(rows) != 2 {
		t.Fatalf("files rows = %d, want 2: %+v", len(rows), rows)
	}
	// Lexicographic by path: data/model before report.json.
	if rows[0].filename != "data/model" || rows[1].filename != "report.json" {
		t.Fatalf("filenames = %q, %q; want data/model, report.json", rows[0].filename, rows[1].filename)
	}
	if rows[0].mime != "application/octet-stream" {
		t.Errorf("extensionless mime = %q, want application/octet-stream", rows[0].mime)
	}
	if !strings.HasPrefix(rows[1].mime, "application/json") {
		t.Errorf("report.json mime = %q, want application/json", rows[1].mime)
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
	h.exec = New(h.pool, h.log, h.queue, h.prov, nil, Config{})
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
