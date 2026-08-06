package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// fakeDaemonWithRevoker is fakeDaemon plus a provider-level gate-token revoker —
// the seam Reap revokes through (a reap has no Spec to carry one).
func fakeDaemonWithRevoker(t *testing.T, revoker sandbox.GateTokenRevoker, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, err := New(Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://"), GateTokenRevoker: revoker})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return p
}

// summaryJSON renders a /containers/json row for an owned container.
func summaryJSON(id, name string, sid domain.ID) string {
	return fmt.Sprintf(`{"Id":%q,"Names":[%q],"Labels":{%q:%q}}`, id, "/"+name, sessionLabel, string(sid))
}

// TestReapRemovesGatePairAndRevokesTokenFirst: a gated session's reap revokes
// the persisted token before any removal (revoke-before-teardown keeps a
// partial failure retryable, #197), then removes the sandbox before the gate —
// the sandbox lives in the gate's netns — even though the daemon listed the
// gate first.
func TestReapRemovesGatePairAndRevokesTokenFirst(t *testing.T) {
	sid := domain.NewID("sesn")
	var removed []string
	var removedAtRevoke []int
	revoker := revokerFunc(func(_ context.Context, got domain.ID) error {
		if got != sid {
			t.Errorf("revoked %s, want %s", got, sid)
		}
		removedAtRevoke = append(removedAtRevoke, len(removed))
		return nil
	})
	p := fakeDaemonWithRevoker(t, revoker, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			if r.URL.Query().Get("all") != "1" {
				t.Errorf("list query all = %q, want 1 (a stopped container is still owned)", r.URL.Query().Get("all"))
			}
			var filters map[string][]string
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
				t.Errorf("list filters do not parse: %v", err)
			}
			if want := []string{sessionLabel + "=" + string(sid)}; !slices.Equal(filters["label"], want) {
				t.Errorf("list label filters = %v, want %v", filters["label"], want)
			}
			// The gate deliberately listed first: Reap must reorder.
			io := "[" + summaryJSON("gate1", gateName(sid), sid) + "," + summaryJSON("sb1", containerName(sid), sid) + "]"
			w.Write([]byte(io))
		case r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/containers/")
			if r.URL.Query().Get("v") != "1" {
				t.Errorf("remove %s without v=1; anonymous volumes would leak", id)
			}
			removed = append(removed, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if err := p.Reap(context.Background(), sid); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !slices.Equal(removedAtRevoke, []int{0}) {
		t.Errorf("revokes landed after %v removals, want exactly one revoke before any", removedAtRevoke)
	}
	if !slices.Equal(removed, []string{"sb1", "gate1"}) {
		t.Errorf("removed = %v, want [sb1 gate1] (sandbox before its netns owner)", removed)
	}
}

// TestReapRevokeFailureStopsBeforeRemoval: a failed revoke aborts the reap with
// nothing removed — the containers stay owned, so the next pass retries both
// halves instead of losing the trigger.
func TestReapRevokeFailureStopsBeforeRemoval(t *testing.T) {
	revoker := revokerFunc(func(context.Context, domain.ID) error { return errors.New("db down") })
	p := fakeDaemonWithRevoker(t, revoker, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("daemon reached (%s %s) after a failed revoke", r.Method, r.URL.Path)
	})
	if err := p.Reap(context.Background(), domain.NewID("sesn")); err == nil {
		t.Fatal("reap succeeded despite a failed revoke")
	}
}

// TestReapNothingOwnedStillRevokes: a session owning no containers is a
// removal no-op, but the revoke still lands — the token is platform state, not
// daemon state, and may outlive the containers (an operator's manual docker rm).
func TestReapNothingOwnedStillRevokes(t *testing.T) {
	revoked := 0
	revoker := revokerFunc(func(context.Context, domain.ID) error { revoked++; return nil })
	p := fakeDaemonWithRevoker(t, revoker, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/json" {
			w.Write([]byte(`[]`))
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	})
	if err := p.Reap(context.Background(), domain.NewID("sesn")); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if revoked != 1 {
		t.Errorf("revoked %d times, want 1", revoked)
	}
}

// TestReapTreats404AsRemoved: a container that vanished between the list and
// the remove (a racing reaper on another executor) is the outcome asked for.
func TestReapTreats404AsRemoved(t *testing.T) {
	sid := domain.NewID("sesn")
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			w.Write([]byte("[" + summaryJSON("sb1", containerName(sid), sid) + "]"))
		case r.Method == http.MethodDelete:
			http.Error(w, `{"message":"No such container: sb1"}`, http.StatusNotFound)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if err := p.Reap(context.Background(), sid); err != nil {
		t.Fatalf("reap: %v", err)
	}
}

// TestReapRemovalFailuresSurfaceAndContinue: one stuck container does not
// strand the rest — every removal is attempted and every failure surfaces.
func TestReapRemovalFailuresSurfaceAndContinue(t *testing.T) {
	sid := domain.NewID("sesn")
	var attempted []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			w.Write([]byte("[" + summaryJSON("sb1", containerName(sid), sid) + "," +
				summaryJSON("gate1", gateName(sid), sid) + "]"))
		case r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/containers/")
			attempted = append(attempted, id)
			if id == "sb1" {
				http.Error(w, `{"message":"sandbox removal stuck"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	err := p.Reap(context.Background(), sid)
	if err == nil || !strings.Contains(err.Error(), "sandbox removal stuck") {
		t.Fatalf("reap error = %v, want the stuck sandbox surfaced", err)
	}
	if !slices.Equal(attempted, []string{"sb1", "gate1"}) {
		t.Errorf("attempted = %v, want both halves tried", attempted)
	}
}

// TestOwnedListsDistinctSessions: Owned collapses a session's pair to one id,
// keeps sessions apart, and asks the daemon for stopped containers too.
func TestOwnedListsDistinctSessions(t *testing.T) {
	sidA, sidB := domain.NewID("sesn"), domain.NewID("sesn")
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		if r.URL.Query().Get("all") != "1" {
			t.Errorf("list query all = %q, want 1", r.URL.Query().Get("all"))
		}
		var filters map[string][]string
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
			t.Errorf("list filters do not parse: %v", err)
		}
		if want := []string{sessionLabel}; !slices.Equal(filters["label"], want) {
			t.Errorf("list label filters = %v, want presence filter %v", filters["label"], want)
		}
		w.Write([]byte("[" + summaryJSON("sb1", containerName(sidA), sidA) + "," +
			summaryJSON("gate1", gateName(sidA), sidA) + "," +
			summaryJSON("sb2", containerName(sidB), sidB) + "]"))
	})
	owned, err := p.Owned(context.Background())
	if err != nil {
		t.Fatalf("owned: %v", err)
	}
	slices.Sort(owned)
	want := []domain.ID{sidA, sidB}
	slices.Sort(want)
	if !slices.Equal(owned, want) {
		t.Errorf("owned = %v, want %v", owned, want)
	}
}
