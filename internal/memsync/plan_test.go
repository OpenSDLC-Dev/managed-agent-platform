package memsync_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
)

// The nine-case matrix of decision 11, one path per case, each pinned in both
// the read-write and the pull-only mode. The digests are the reference
// worker's own vocabulary: local is what the tree hash saw, baseline what the
// last sync agreed on, remote the store's head.
func TestPlanMatrix(t *testing.T) {
	const a, b, c = "aaaa", "bbbb", "cccc"
	head := func(sha string) memsync.Head { return memsync.Head{ID: "mem_" + sha, SHA: sha} }

	type tc struct {
		name         string
		local, base  string // "" for absent
		remote       string // "" for absent
		refused      string
		want         *memsync.Action // nil for no action
		wantPullOnly *memsync.Action
		next         string // the baseline entry Plan settles itself; "" for none
		nextPullOnly string
		skipped      bool // the push was skipped for a standing refusal
		keepRefusal  bool
	}
	cases := []tc{
		{name: "1 gone on both sides drops the baseline", base: a},
		{name: "2 a new local file is created",
			local: a,
			want:  &memsync.Action{Kind: memsync.Push, Path: "/p", LocalSHA: a}},
		{name: "3 deleted remotely, untouched locally, is removed",
			local: a, base: a,
			want: &memsync.Action{Kind: memsync.RemoveLocal, Path: "/p"}, wantPullOnly: &memsync.Action{Kind: memsync.RemoveLocal, Path: "/p"}},
		{name: "4 deleted remotely but edited locally is re-created",
			local: b, base: a,
			want: &memsync.Action{Kind: memsync.Push, Path: "/p", LocalSHA: b}},
		{name: "5 a new remote memory is pulled",
			remote:       a,
			want:         &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + a, RemoteSHA: a},
			wantPullOnly: &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + a, RemoteSHA: a}},
		{name: "6a deleted locally while the store changed is restored, and that is a conflict",
			base: a, remote: b,
			want:         &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + b, RemoteSHA: b, Conflict: true},
			wantPullOnly: &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + b, RemoteSHA: b, Conflict: true}},
		{name: "6b deleted locally with the store unchanged deletes remotely; pull-only keeps the baseline",
			base: a, remote: a,
			want:         &memsync.Action{Kind: memsync.DeleteRemote, Path: "/p", ID: "mem_" + a, BaselineSHA: a},
			nextPullOnly: a},
		{name: "7a remote changed, local untouched, is pulled without conflict",
			local: a, base: a, remote: b,
			want:         &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + b, RemoteSHA: b},
			wantPullOnly: &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + b, RemoteSHA: b}},
		{name: "7b both changed: the store wins and the local edit is a conflict",
			local: c, base: a, remote: b,
			want:         &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + b, RemoteSHA: b, Conflict: true},
			wantPullOnly: &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + b, RemoteSHA: b, Conflict: true}},
		{name: "7c both sides already hold the same new bytes: adopted, nothing moves",
			local: b, base: a, remote: b,
			next: b, nextPullOnly: b},
		{name: "7d never synced but identical is adopted too",
			local: a, remote: a,
			next: a, nextPullOnly: a},
		{name: "7e never synced and different: the store wins, the local file was an edit",
			local: b, remote: a,
			want:         &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + a, RemoteSHA: a, Conflict: true},
			wantPullOnly: &memsync.Action{Kind: memsync.Pull, Path: "/p", ID: "mem_" + a, RemoteSHA: a, Conflict: true}},
		{name: "8 a local edit over an unchanged store is pushed with the baseline as precondition; pull-only keeps the baseline",
			local: b, base: a, remote: a,
			want:         &memsync.Action{Kind: memsync.Push, Path: "/p", ID: "mem_" + a, BaselineSHA: a, LocalSHA: b},
			nextPullOnly: a},
		{name: "9 all three agree: nothing",
			local: a, base: a, remote: a,
			next: a, nextPullOnly: a},
		{name: "a refused edit is skipped until its bytes change",
			local: b, base: a, remote: a, refused: b,
			next: a, nextPullOnly: a, skipped: true, keepRefusal: true},
		{name: "a refused new file is skipped too",
			local: a, refused: a,
			skipped: true, keepRefusal: true},
		{name: "a refusal for other bytes is forgotten",
			local: c, base: a, remote: a, refused: b,
			want:         &memsync.Action{Kind: memsync.Push, Path: "/p", ID: "mem_" + a, BaselineSHA: a, LocalSHA: c},
			nextPullOnly: a},
	}
	for _, c := range cases {
		for _, pullOnly := range []bool{false, true} {
			name := c.name
			if pullOnly {
				name += " (pull-only)"
			}
			t.Run(name, func(t *testing.T) {
				in := memsync.Input{
					Local: map[string]string{}, Remote: map[string]memsync.Head{},
					Baseline: memsync.Baseline{Synced: map[string]string{}, Refused: map[string]string{}},
					PullOnly: pullOnly,
				}
				if c.local != "" {
					in.Local["/p"] = c.local
				}
				if c.base != "" {
					in.Baseline.Synced["/p"] = c.base
				}
				if c.refused != "" {
					in.Baseline.Refused["/p"] = c.refused
				}
				if c.remote != "" {
					in.Remote["/p"] = head(c.remote)
				}
				res := memsync.Plan(in)
				if res.Rebuild {
					t.Fatal("rebuild")
				}
				want, next, skipped := c.want, c.next, c.skipped
				if pullOnly {
					want, next = c.wantPullOnly, c.nextPullOnly
					if want == nil && c.want != nil && c.want.Kind == memsync.Pull {
						t.Fatal("test bug: a pull applies in both modes")
					}
					skipped = false // pull-only is a mode, not a refusal: nothing is withheld
				}
				var wantActions []memsync.Action
				if want != nil {
					wantActions = []memsync.Action{*want}
				}
				if !reflect.DeepEqual(res.Actions, wantActions) {
					t.Errorf("actions = %+v, want %+v", res.Actions, wantActions)
				}
				if got := res.Next.Synced["/p"]; got != next {
					t.Errorf("next baseline = %q, want %q", got, next)
				}
				if got := len(res.Skipped) == 1; got != skipped {
					t.Errorf("skipped = %v, want %v", res.Skipped, skipped)
				}
				if got := res.Next.Refused["/p"] != ""; got != c.keepRefusal {
					t.Errorf("refusal kept = %v, want %v", got, c.keepRefusal)
				}
				// Pull-only mode counts every push it drops, refused bytes
				// included, so a run that pushed nothing can say why.
				wantWithheld := 0
				if pullOnly && ((c.want != nil && c.want.Kind == memsync.Push) || c.skipped) {
					wantWithheld = 1
				}
				if res.Withheld != wantWithheld {
					t.Errorf("withheld = %d, want %d", res.Withheld, wantWithheld)
				}
			})
		}
	}
}

// The wipe guard (decision 11, the reference worker's own): an empty directory
// against a baseline that knew more than one file is a wiped mount, not a mass
// deletion — everything is pulled back and nothing is deleted. One remembered
// file is below the guard: deleting the only file is a thing an agent does.
func TestPlanWipeGuard(t *testing.T) {
	remote := map[string]memsync.Head{"/a": {ID: "mem_a", SHA: "a"}, "/b": {ID: "mem_b", SHA: "b"}, "/c": {ID: "mem_c", SHA: "c"}}
	res := memsync.Plan(memsync.Input{
		Local:    map[string]string{},
		Baseline: memsync.Baseline{Synced: map[string]string{"/a": "a", "/b": "b"}, Refused: map[string]string{"/z": "z"}},
		Remote:   remote,
	})
	if !res.Rebuild {
		t.Fatal("not a rebuild")
	}
	want := []memsync.Action{
		{Kind: memsync.Pull, Path: "/a", ID: "mem_a", RemoteSHA: "a"},
		{Kind: memsync.Pull, Path: "/b", ID: "mem_b", RemoteSHA: "b"},
		{Kind: memsync.Pull, Path: "/c", ID: "mem_c", RemoteSHA: "c"},
	}
	if !reflect.DeepEqual(res.Actions, want) {
		t.Errorf("actions = %+v, want %+v", res.Actions, want)
	}
	if len(res.Next.Synced) != 0 || len(res.Next.Refused) != 0 {
		t.Errorf("next = %+v, want empty", res.Next)
	}

	// Below the guard: the one remembered file was deleted on purpose.
	res = memsync.Plan(memsync.Input{
		Local:    map[string]string{},
		Baseline: memsync.Baseline{Synced: map[string]string{"/a": "a"}},
		Remote:   map[string]memsync.Head{"/a": {ID: "mem_a", SHA: "a"}},
	})
	if res.Rebuild || len(res.Actions) != 1 || res.Actions[0].Kind != memsync.DeleteRemote {
		t.Errorf("one-file deletion planned %+v, rebuild %v", res.Actions, res.Rebuild)
	}
}

// Actions come out in path order whatever order the maps iterate, so a run's
// settle and its telemetry are reproducible; a path the store would refuse is
// never pushed and is remembered as refused; and nil maps are inputs too.
func TestPlanOrderAndRefusedPaths(t *testing.T) {
	res := memsync.Plan(memsync.Input{
		Local: map[string]string{"/z": "z", "/a": "a", "/m/n": "n", "/bad\x01": "x", "/x/../y": "y"},
	})
	var paths []string
	for _, act := range res.Actions {
		if act.Kind != memsync.Push {
			t.Errorf("unexpected %+v", act)
		}
		paths = append(paths, act.Path)
	}
	if got := strings.Join(paths, " "); got != "/a /m/n /z" {
		t.Errorf("order = %q", got)
	}
	if got := len(res.Skipped); got != 2 {
		t.Errorf("skipped = %v", res.Skipped)
	}
	if res.Next.Refused["/bad\x01"] != "x" || res.Next.Refused["/x/../y"] != "y" {
		t.Errorf("refused = %v", res.Next.Refused)
	}
	if res.Next.Synced == nil {
		t.Error("next synced map is nil")
	}
	// The zero input plans nothing and hands back usable maps.
	if res := memsync.Plan(memsync.Input{}); len(res.Actions) != 0 || res.Rebuild || res.Next.Synced == nil || res.Next.Refused == nil {
		t.Errorf("zero input = %+v", res)
	}
}
