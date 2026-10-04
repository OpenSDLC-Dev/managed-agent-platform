package sandbox

import (
	"context"
	"errors"
	"strconv"
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

// probeEachLabel names ProbeEach's frame in what the sandbox printed.
const probeEachLabel = "presence"

// ProbeEach asks which of paths exist in sb, answering each path's Presence in
// paths' order. One framed exec (ExecFramed) tests as many of them as the
// bound on one command allows (MaxCommandBytes) and lists, by index, the ones
// not there — so a set is asked in as many execs as its tests' bytes fill
// commands, as ProbePaths's are, never one per path (5,000 paths of 1 KB take
// 44). A path is Absent where its batch listed it, Present where its batch
// answered without it, and PresenceUnknown where its batch's answer could not
// be read: an exec error (the answer an image's startup pushed out,
// *StartupOutputError, among them), output the frame did not carry whole, a
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
	if err != nil || !framed || res.TimedOut || res.ExitCode != 0 {
		return
	}
	// echo ends every line it prints, so output that does not end in a
	// newline is no whole answer — a stdout the output cap cut short among
	// it, whose last newline Cut takes for the start of the end line.
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
// parent no directory, Absent — and PresenceUnknown for any other failure. On
// Kubernetes that answer is the read exec's own exit status, which no output
// an image's startup prints displaces; on Docker it is the daemon's archive
// endpoint, which starts tarring a directory it is asked for (measured: about
// 1.3 s for a 383 MB .git before the stream closes), so a caller asks
// ProbePaths first and this only where that did not answer.
func StatPresence(ctx context.Context, sb Sandbox, path string) Presence {
	rc, _, err := sb.ReadFileStream(ctx, path, 0)
	switch {
	case err == nil:
		rc.Close()
		return Present
	case errors.Is(err, ErrFileNotExist), errors.Is(err, ErrNotDirectory):
		return Absent
	case errors.Is(err, ErrIsDirectory), errors.Is(err, ErrNotRegularFile), errors.Is(err, ErrFileTooLarge):
		return Present
	default:
		return PresenceUnknown
	}
}
