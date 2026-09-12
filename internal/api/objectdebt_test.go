package api_test

import (
	"net/http"
	"slices"
	"testing"

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
