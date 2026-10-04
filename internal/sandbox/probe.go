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

// ProbePaths asks whether every path exists in sb, in one exec of `test -e`
// on each (ExecScript): Present when the exec exits 0, Absent when it exits 1,
// and PresenceUnknown for anything else — an exec error, the answer an
// image's startup pushed out of the output (*StartupOutputError) among them,
// a timeout, any other exit — which is no answer about the paths.
func ProbePaths(ctx context.Context, sb Sandbox, paths ...string) Presence {
	var cmd strings.Builder
	for _, p := range paths {
		cmd.WriteString("test -e '")
		cmd.WriteString(strings.ReplaceAll(p, "'", `'\''`))
		cmd.WriteString("' && ")
	}
	cmd.WriteString("true")
	res, err := ExecScript(ctx, sb, ExecRequest{Command: cmd.String()})
	switch {
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
