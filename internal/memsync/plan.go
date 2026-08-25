package memsync

import "sort"

// Head is a store's current memory at a path: its id, and the digest the
// store holds for it.
type Head struct {
	ID  string
	SHA string
}

// Kind is what an Action asks its applier to do.
type Kind uint8

const (
	// Pull writes the store's content at Path into the directory (ID names
	// the memory, RemoteSHA is the entry the baseline takes once written).
	Pull Kind = iota + 1
	// Push writes the directory's bytes at Path to the store: a create when
	// ID is empty, else an update of ID conditioned on the store still
	// holding BaselineSHA. LocalSHA is the digest of the bytes being pushed.
	Push
	// DeleteRemote deletes memory ID, conditioned on the store still holding
	// BaselineSHA.
	DeleteRemote
	// RemoveLocal removes the directory's file at Path: the store deleted the
	// memory and the directory never changed it.
	RemoveLocal
)

// Action is one step of a sync. Conflict marks a Pull that overwrites a local
// edit or restores a local deletion — the store won.
type Action struct {
	Kind        Kind
	Path        string
	ID          string
	BaselineSHA string
	LocalSHA    string
	RemoteSHA   string
	Conflict    bool
}

// Input is what Plan decides from: the directory's files by digest (the
// marker excluded), the baseline the last sync left, the store's heads, and
// whether this sync may write to the store at all. PullOnly is a read_only
// attachment, an archived store, or a directory whose marker is missing or
// altered (decision 12).
type Input struct {
	Local    map[string]string
	Baseline Baseline
	Remote   map[string]Head
	PullOnly bool
}

// Result is the plan. Actions are in path order. Next is the baseline the
// sync will write, holding only the entries Plan settled without an action —
// the applier adds what each action lands (a pull's RemoteSHA, a push's
// LocalSHA), keeps a DeleteRemote's BaselineSHA when the store refused it,
// and drops the rest. Skipped lists pushes withheld by a standing refusal;
// their refusals are already carried in Next. Rebuild is the wipe guard.
type Result struct {
	Rebuild bool
	Actions []Action
	Next    Baseline
	Skipped []string
}

// Plan is decision 11's per-path table, the reference worker's syncPath
// decisions with our two omissions (no corroboration window and no deletion
// cap, both registered): for each path in the union of the three maps, the
// baseline tells whether the store changed (its head is not the baseline)
// and whether the directory changed (its file is not the baseline), and the
// answer is the one the reference gives — the store wins a conflict, a
// deletion on one side over an unchanged other side propagates, an edit over
// a remote deletion re-creates. Pull-only mode drops every write to the store
// and otherwise decides identically, so a read-only directory still follows
// the store's changes and deletions. A push whose bytes the store refused
// before, or whose path the store would refuse, is skipped and stays refused
// until the bytes change.
//
// The wipe guard is the reference's own: an empty directory against a
// baseline that remembered more than one file is a wiped mount, so everything
// is pulled and nothing deleted.
func Plan(in Input) Result {
	res := Result{Next: Baseline{Synced: map[string]string{}, Refused: map[string]string{}}}
	if len(in.Local) == 0 && len(in.Baseline.Synced) > 1 {
		res.Rebuild = true
		for _, path := range sortedKeys(nil, nil, in.Remote) {
			head := in.Remote[path]
			res.Actions = append(res.Actions, Action{Kind: Pull, Path: path, ID: head.ID, RemoteSHA: head.SHA})
		}
		return res
	}
	for _, path := range sortedKeys(in.Local, in.Baseline.Synced, in.Remote) {
		local, localPresent := in.Local[path]
		base, hasBase := in.Baseline.Synced[path]
		head, remotePresent := in.Remote[path]
		if refused, ok := in.Baseline.Refused[path]; ok && localPresent && refused == local {
			res.Next.Refused[path] = refused
		}
		push := func(act Action) {
			switch {
			case in.PullOnly:
			case res.Next.Refused[path] == local:
				res.Skipped = append(res.Skipped, path)
			case ValidatePath(path) != nil:
				res.Next.Refused[path] = local
				res.Skipped = append(res.Skipped, path)
			default:
				res.Actions = append(res.Actions, act)
			}
		}

		switch {
		case !remotePresent && !localPresent:
			// Gone on both sides: the baseline entry, if any, just drops.
		case !remotePresent && !hasBase:
			push(Action{Kind: Push, Path: path, LocalSHA: local})
		case !remotePresent && local == base:
			res.Actions = append(res.Actions, Action{Kind: RemoveLocal, Path: path})
		case !remotePresent:
			// Deleted in the store, edited here: the edit is worth keeping.
			push(Action{Kind: Push, Path: path, LocalSHA: local})
		case !localPresent && !hasBase:
			res.Actions = append(res.Actions, Action{Kind: Pull, Path: path, ID: head.ID, RemoteSHA: head.SHA})
		case !localPresent && head.SHA != base:
			res.Actions = append(res.Actions, Action{Kind: Pull, Path: path, ID: head.ID, RemoteSHA: head.SHA, Conflict: true})
		case !localPresent && in.PullOnly:
			res.Next.Synced[path] = base
		case !localPresent:
			res.Actions = append(res.Actions, Action{Kind: DeleteRemote, Path: path, ID: head.ID, BaselineSHA: base})
		case local == head.SHA:
			res.Next.Synced[path] = head.SHA
		case !hasBase || head.SHA != base:
			// The store changed; a local file that differs from it loses.
			res.Actions = append(res.Actions, Action{Kind: Pull, Path: path, ID: head.ID, RemoteSHA: head.SHA,
				Conflict: !hasBase || local != base})
		case local == base:
			res.Next.Synced[path] = base
		default:
			if in.PullOnly || res.Next.Refused[path] == local || ValidatePath(path) != nil {
				res.Next.Synced[path] = base
			}
			push(Action{Kind: Push, Path: path, ID: head.ID, BaselineSHA: base, LocalSHA: local})
		}
	}
	return res
}

func sortedKeys(a, b map[string]string, c map[string]Head) []string {
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range a {
		add(k)
	}
	for k := range b {
		add(k)
	}
	for k := range c {
		add(k)
	}
	sort.Strings(keys)
	return keys
}
