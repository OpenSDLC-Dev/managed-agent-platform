package sandbox

import (
	"context"
	"errors"
	"strings"
)

// Presence is what a probe of paths in a sandbox found (ProbePaths): that
// they are all there, that one is not, or nothing — the probe did not answer.
type Presence int

const (
	// PresenceUnknown is a probe that did not answer. A caller must not take
	// it for either answer: read as absent, it would redo — over the agent's
	// own edits — work the sandbox already has (#860).
	PresenceUnknown Presence = iota
	// Present is every path there.
	Present
	// Absent is at least one path not there.
	Absent
)

// ProbePaths asks whether every path exists in sb, `test -e` on each in
// scripted execs (ExecScript), as few as the bound on one command allows
// (MaxCommandBytes) — so a set too long for one exec is still asked, batch by
// batch, rather than refused unasked. Absent when any batch exits 1; Present
// when every batch exits 0; and PresenceUnknown when no batch said Absent and
// one that ran did not answer — an exec error, the answer an image's startup
// pushed out of the output (*StartupOutputError) among them, a timeout, any
// other exit — which is no answer about the paths. A batch the platform
// refused as too long before it ran (one path past the bound alone) asked the
// sandbox nothing either way, and reads as Absent, as before #860: the caller
// then redoes the work, as it would for a path not there.
func ProbePaths(ctx context.Context, sb Sandbox, paths ...string) Presence {
	answer := Present
	for _, batch := range probeBatches(paths) {
		switch probeBatch(ctx, sb, batch) {
		case Absent:
			return Absent
		case PresenceUnknown:
			answer = PresenceUnknown
		}
	}
	return answer
}

// probeBatches splits paths into `test -e` commands each within the bound one
// exec carries once scripted (CheckScript): a path past it alone is a batch of
// its own, which ExecScript refuses.
func probeBatches(paths []string) []string {
	const tail = "true"
	var batches []string
	var cur strings.Builder
	for _, p := range paths {
		test := "test -e '" + strings.ReplaceAll(p, "'", `'\''`) + "' && "
		if cur.Len() > 0 && len(ScriptPreamble)+cur.Len()+len(test)+len(tail) > MaxCommandBytes {
			batches = append(batches, cur.String()+tail)
			cur.Reset()
		}
		cur.WriteString(test)
	}
	return append(batches, cur.String()+tail)
}

func probeBatch(ctx context.Context, sb Sandbox, cmd string) Presence {
	res, err := ExecScript(ctx, sb, ExecRequest{Command: cmd})
	var tooLong *CommandTooLongError
	switch {
	case errors.As(err, &tooLong):
		return Absent
	case err != nil, res.TimedOut:
		return PresenceUnknown
	case res.ExitCode == 0:
		return Present
	case res.ExitCode == 1:
		return Absent
	default:
		return PresenceUnknown
	}
}

// ReadAnswered reports whether err, from reading a path in a sandbox
// (ReadFile, ReadFileStream), answers anything about that path: nil, the
// bytes, or one of the path sentinels — not there, a directory or no regular
// file, a parent no directory, too large. Anything else — the answer an
// image's startup pushed out (*StartupOutputError), a failed exec, a sandbox
// gone — says nothing of it, which a caller deciding whether to redo work the
// path records must not take for "not there" (#860).
func ReadAnswered(err error) bool {
	return err == nil || errors.Is(err, ErrFileNotExist) || errors.Is(err, ErrIsDirectory) ||
		errors.Is(err, ErrNotRegularFile) || errors.Is(err, ErrNotDirectory) || errors.Is(err, ErrFileTooLarge)
}
