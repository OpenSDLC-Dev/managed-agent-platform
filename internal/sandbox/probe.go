package sandbox

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// Presence is what a probe of a path in a sandbox found (ProbeEach,
// StatPresence): that it is there, that it is not, or nothing — the probe did
// not answer.
type Presence int

const (
	// PresenceUnknown is a probe that did not answer. A caller must not take
	// it for either answer: read as absent, it would redo — over the agent's
	// own edits — work the sandbox already has (#860).
	PresenceUnknown Presence = iota
	// Present is the path there.
	Present
	// Absent is the path not there.
	Absent
)

// probeEachLabel names ProbeEach's frame in what the sandbox printed.
const probeEachLabel = "presence"

// ProbeEach asks which of paths exist in sb, answering each path's Presence in
// paths' order. One framed exec (ExecFramed) tests as many of them as the
// bound on one command allows (MaxCommandBytes) and lists, by index, the ones
// not there — so a set is asked in as many execs as its tests' bytes fill
// commands, never one per path (5,000 paths of 1 KB take 44). A path is
// Absent where its batch listed it, Present where its batch answered without
// it, and PresenceUnknown where its batch's answer could not be read: an exec
// error (the answer an image's startup pushed out, *StartupOutputError, among
// them), output the frame did not carry whole or the cap cut short, a
// timeout, an exit other than 0, a line that is no index of the batch's. That
// is no answer about the batch's paths, and a caller must not take it for
// Absent (#860).
//
// batched, when not nil, is called after each batch, answered or not: a set
// that takes many execs on a slow sandbox reports progress between them, so a
// caller's stall guard (the executor's lease keeper) does not take a probe
// still working for a stalled one.
func ProbeEach(ctx context.Context, sb Sandbox, batched func(), paths ...string) []Presence {
	answer := make([]Presence, len(paths))
	room := MaxCommandBytes - len(NewFrame(probeEachLabel).Wrap(""))
	var script strings.Builder
	from := 0
	for i, p := range paths {
		test := "test -e '" + strings.ReplaceAll(p, "'", `'\''`) + "' || echo " + strconv.Itoa(i) + "\n"
		if script.Len() > 0 && script.Len()+len(test) > room {
			probeEachBatch(ctx, sb, script.String(), from, answer[from:i])
			if batched != nil {
				batched()
			}
			script.Reset()
			from = i
		}
		script.WriteString(test)
	}
	if script.Len() > 0 {
		probeEachBatch(ctx, sb, script.String(), from, answer[from:])
		if batched != nil {
			batched()
		}
	}
	return answer
}

// probeEachBatch runs one batch of ProbeEach's tests, those of the paths from
// index from on, and sets answer — theirs — from the indices it listed, a
// line each; it leaves answer PresenceUnknown where it cannot read them.
func probeEachBatch(ctx context.Context, sb Sandbox, script string, from int, answer []Presence) {
	res, framed, err := ExecFramed(ctx, sb, probeEachLabel, ExecRequest{Command: script})
	// A stdout the output cap cut short lost the lines past the cut, and
	// lost them all where the cut met the begin line: what is left would
	// read their paths as there.
	if err != nil || !framed || res.TimedOut || res.ExitCode != 0 || res.StdoutTruncated {
		return
	}
	// echo ends every line it prints, so an answer that does not end in a
	// newline is not one the script printed whole.
	lines := strings.Split(res.Stdout, "\n")
	if lines[len(lines)-1] != "" {
		return
	}
	absent := make([]bool, len(answer))
	for _, line := range lines[:len(lines)-1] {
		i, err := strconv.Atoi(line)
		if err != nil || i < from || i >= from+len(answer) {
			return
		}
		absent[i-from] = true
	}
	for i := range answer {
		answer[i] = Present
		if absent[i] {
			answer[i] = Absent
		}
	}
}

// StatPresence asks whether path exists in sb through the file API rather
// than a probe exec: a read of it capped at no bytes at all
// (ReadFileStream), whose refusal says what is there — a directory, no
// regular file, or one too large for that cap is Present; nothing there, or a
// parent no directory, Absent — and PresenceUnknown for any other failure, or
// for a refusal the read's exit status alone carried (ErrUnframedStatus). On
// Kubernetes that answer is the read exec's own exit status, which no output
// an image's startup prints displaces; on Docker it is the daemon's archive
// endpoint, which starts tarring a directory it is asked for (measured: about
// 1.3 s for a 383 MB .git before the stream closes), so a caller asks
// ProbeEach first and this only where that did not answer.
func StatPresence(ctx context.Context, sb Sandbox, path string) Presence {
	rc, _, err := sb.ReadFileStream(ctx, path, 0)
	switch {
	case err == nil:
		rc.Close()
		return Present
	case errors.Is(err, ErrUnframedStatus):
		return PresenceUnknown
	case errors.Is(err, ErrFileNotExist), errors.Is(err, ErrNotDirectory):
		return Absent
	case errors.Is(err, ErrIsDirectory), errors.Is(err, ErrNotRegularFile), errors.Is(err, ErrFileTooLarge):
		return Present
	default:
		return PresenceUnknown
	}
}
