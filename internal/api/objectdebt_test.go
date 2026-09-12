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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
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
