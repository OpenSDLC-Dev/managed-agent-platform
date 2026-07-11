package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	data, err := r.Sandbox.ReadFile(ctx, r.resolve(in.FilePath))
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
		return failf("read: view_range end line %d is before start line %d", in.ViewRange[1], in.ViewRange[0])
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
	if err := r.Sandbox.WriteFile(ctx, r.resolve(in.FilePath), []byte(in.Content)); err != nil {
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

// fileFault classifies a sandbox file error. The sentinels describe the file the
// model asked for — it can read a different one, or make the one it wanted — so
// they are tool results. Anything else is the sandbox itself failing, and that
// is the executor's to handle. The path in the message is the one the model
// used, not the resolved one: it is the name the model can act on.
func fileFault(verb, display string, err error) (Result, error) {
	switch {
	case errors.Is(err, sandbox.ErrFileNotExist):
		return failf("%s %s: no such file or directory", verb, display)
	case errors.Is(err, sandbox.ErrIsDirectory), errors.Is(err, sandbox.ErrNotRegularFile):
		return failf("%s: %s is not a regular file", verb, display)
	case errors.Is(err, sandbox.ErrFileTooLarge):
		return failf("%s: %s exceeds the %d-byte limit. Use bash (head/tail/sed) to work on a slice.",
			verb, display, sandbox.MaxFileBytes)
	default:
		return Result{}, err
	}
}
