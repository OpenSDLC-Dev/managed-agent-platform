package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

type readInput struct {
	FilePath  string  `json:"file_path"`
	ViewRange []int64 `json:"view_range"`
}

type writeInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

type editInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (r Runner) read(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in readInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return failf("invalid read input: %v", err)
	}
	if in.FilePath == "" {
		return failf("read: file_path is required")
	}
	if res, bad := badField("read", "file_path", in.FilePath); bad {
		return res, nil
	}
	p := r.resolve(in.FilePath)
	if res, bad := pathTooLong("read", p); bad {
		return res, nil
	}
	data, err := r.Sandbox.ReadFile(ctx, p)
	if err != nil {
		return fileFault("read", in.FilePath, err)
	}
	if len(in.ViewRange) == 0 {
		return succeed(string(data))
	}
	if len(in.ViewRange) != 2 {
		return failf("read: view_range must be [start_line, end_line]")
	}

	// 1-indexed inclusive, and everything stays int64: a view_range the model
	// picked out of thin air must not overflow an index on a 32-bit build.
	lines := strings.Split(string(data), "\n")
	start := max(in.ViewRange[0]-1, 0)
	if start >= int64(len(lines)) {
		return succeed("")
	}
	end := int64(len(lines))
	if e := in.ViewRange[1]; e > 0 && e < end {
		end = e
	}
	if end < start {
		// An inverted range selects nothing — empty content, not an error, the
		// reference toolset's answer since anthropic-sdk-go v1.63.0 — fs.go
		// execRead.
		return succeed("")
	}
	return succeed(strings.Join(lines[start:end], "\n"))
}

func (r Runner) write(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in writeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return failf("invalid write input: %v", err)
	}
	if in.FilePath == "" {
		return failf("write: file_path is required")
	}
	if res, bad := badField("write", "file_path", in.FilePath); bad {
		return res, nil
	}
	p := r.resolve(in.FilePath)
	if res, bad := pathTooLong("write", p); bad {
		return res, nil
	}
	if why := r.unwritable(in.FilePath, p); why != "" {
		return failf("write: %s", why)
	}
	if err := r.Sandbox.WriteFile(ctx, p, []byte(in.Content)); err != nil {
		return fileFault("write", in.FilePath, err)
	}
	return succeed(fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.FilePath))
}

func (r Runner) edit(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in editInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return failf("invalid edit input: %v", err)
	}
	if in.FilePath == "" {
		return failf("edit: file_path is required")
	}
	if in.OldString == "" {
		return failf("edit: old_string is required")
	}
	if res, bad := badField("edit", "file_path", in.FilePath); bad {
		return res, nil
	}
	p := r.resolve(in.FilePath)
	if res, bad := pathTooLong("edit", p); bad {
		return res, nil
	}
	if why := r.unwritable(in.FilePath, p); why != "" {
		return failf("edit: %s", why)
	}
	data, err := r.Sandbox.ReadFile(ctx, p)
	if err != nil {
		return fileFault("edit", in.FilePath, err)
	}

	content := string(data)
	count := strings.Count(content, in.OldString)
	switch {
	case count == 0:
		return failf("edit: old_string not found in %s", in.FilePath)
	case count > 1 && !in.ReplaceAll:
		return failf("edit: old_string appears %d times in %s (must be unique)", count, in.FilePath)
	}
	updated := strings.Replace(content, in.OldString, in.NewString, count)
	if err := r.Sandbox.WriteFile(ctx, p, []byte(updated)); err != nil {
		return fileFault("edit", in.FilePath, err)
	}
	return succeed(fmt.Sprintf("edited %s (%d replacement(s))", in.FilePath, count))
}

// Linux's bounds on a path: PATH_MAX, 4096 bytes counting the NUL that ends
// it, and NAME_MAX, the longest name a directory entry can hold.
const (
	maxPathBytes = 4096 - 1
	maxNameBytes = 255
)

// pathTooLong refuses, as a tool error, a file_path the kernel would refuse
// with ENAMETOOLONG ("file name too long"): resolved — the path the sandbox
// would be handed — past maxPathBytes, or a name in it past maxNameBytes. It
// is asked before the sandbox is, because no backend answers that refusal as
// the path's: the k8s backend hands the path to its exec as an argument, which
// one long enough overflows (E2BIG) before anything runs, and Docker's archive
// endpoint answers it with a 500 naming no cause — each a fault the executor
// would leave to a reclaim, which would make the same call again. Within the
// bounds, every command a file primitive builds around the path stays far
// below sandbox.MaxCommandBytes.
func pathTooLong(verb, resolved string) (Result, bool) {
	if n := len(resolved); n > maxPathBytes {
		res, _ := failf("%s: file name too long: the file_path resolves to a %d-byte path, over the %d bytes a Linux path can hold; shorten it",
			verb, n, maxPathBytes)
		return res, true
	}
	for name := range strings.SplitSeq(resolved, "/") {
		if n := len(name); n > maxNameBytes {
			res, _ := failf("%s: file name too long: the file_path holds a %d-byte name, over the %d bytes a Linux file name can hold; shorten it",
				verb, n, maxNameBytes)
			return res, true
		}
	}
	return Result{}, false
}

// fileFault classifies a sandbox file error. The sentinels describe the file the
// model asked for — it can read a different one, or make the one it wanted — so
// they are tool results. Anything else is the sandbox itself failing, and that
// is the executor's to handle — or, for a command the backend refused as too
// long to run, the platform's own (Runner.dispatch): a path within Linux's
// bounds (pathTooLong) cannot make one. The path in the message is the one the
// model used, not the resolved one: it is the name the model can act on.
//
// The distinction is not cosmetic: a fault left unclassified reaches the executor,
// which stops the tool set and abandons the work item to lease reclaim — so the
// same doomed call is retried until the lease runs out (#71).
func fileFault(verb, display string, err error) (Result, error) {
	switch {
	case errors.Is(err, sandbox.ErrFileNotExist):
		return failf("%s %s: no such file or directory", verb, display)
	case errors.Is(err, sandbox.ErrIsDirectory), errors.Is(err, sandbox.ErrNotRegularFile):
		return failf("%s: %s is not a regular file", verb, display)
	case errors.Is(err, sandbox.ErrNotDirectory):
		return failf("%s %s: not a directory", verb, display)
	case errors.Is(err, sandbox.ErrNotReplaceable):
		return failf("%s: %s cannot be replaced by an atomic write (a device node, or a file bind-mounted into the sandbox). Use bash redirection to write through it.",
			verb, display)
	case errors.Is(err, sandbox.ErrNotWritable):
		return failf("%s %s: %s", verb, display, notWritableReason(err))
	case errors.Is(err, sandbox.ErrFileTooLarge):
		return failf("%s: %s exceeds the %d-byte limit. Use bash (head/tail/sed) to work on a slice.",
			verb, display, sandbox.MaxFileBytes)
	default:
		return Result{}, err
	}
}

// notWritableReason is the wording of an ErrNotWritable result: the sandbox's
// own strerror text, normalized the way the reference toolset's fsErrorMessage
// table is — its one mapped case matches fs.ErrPermission, which Go answers
// for EACCES and EPERM alike, so both strerror spellings take the reference's
// wording; anything else it answers with Go's own errno text (since
// anthropic-sdk-go v1.63.0 — agenttoolset.go fsErrorMessage), which is glibc's
// strerror in lowercase — "read-only file system", "no space left on device" —
// so the passthrough lowercases its first rune (parity for a glibc image; a
// musl image's strerror can differ in wording — "filename too long" — and
// passes through as it is); and a refusal that carried no reason falls back to
// the sentinel's own words (plan 23, #306).
func notWritableReason(err error) string {
	var pnw *sandbox.PathNotWritableError
	if !errors.As(err, &pnw) || pnw.Reason == "" {
		return "cannot be written"
	}
	if strings.EqualFold(pnw.Reason, "permission denied") ||
		strings.EqualFold(pnw.Reason, "operation not permitted") {
		return "permission denied"
	}
	r := []rune(pnw.Reason)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
