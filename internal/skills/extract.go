package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

// ExtractMaxBytes caps a skill archive's total decompressed content at
// extraction time, matching the reference worker's guard (anthropic-sdk-go
// tools/agenttoolset/skillarchive.go: 1 GiB). Far above the 30 MB upload cap
// on purpose — extraction guards protect the sandbox even if a stored object
// did not come through this platform's upload validation.
const ExtractMaxBytes = 1 << 30

// SentinelName is the marker file written under {workdir}/skills/ after a
// successful materialization pass, recording the resolved {skill_id: version}
// set so re-entrant sandbox provisioning skips rewriting unchanged skills.
const SentinelName = ".materialized"

// BlobKey is the object-store key for a skill version's archive — the one
// layout the API's upload/download, the importer, and the executor's
// materialization all share.
func BlobKey(skillID, version string) string {
	return "skills/" + skillID + "/" + version + ".zip"
}

// Extract opens a stored skill archive (the canonical zip the registry
// serves) and returns its files with slash-relative paths, the single
// top-level wrapper directory stripped — the reference worker's extraction
// semantics with its guards: escape ("slip") refusal and member/byte caps.
// Only zip is accepted: this platform stores and serves canonical zips, so
// the reference's tar fallback would be dead code here.
func Extract(data []byte) ([]File, error) {
	return extractWithLimits(data, MaxMembers, ExtractMaxBytes)
}

func extractWithLimits(data []byte, maxMembers int, maxBytes int64) ([]File, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("skill archive is not a readable zip")
	}
	if len(zr.File) == 0 {
		return nil, fmt.Errorf("skill archive is empty")
	}
	if len(zr.File) > maxMembers {
		return nil, fmt.Errorf("skill archive has %d members; the maximum is %d", len(zr.File), maxMembers)
	}

	// Every member name must be a safe relative path before any strip/read
	// decision — one hostile member rejects the whole archive.
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		p := strings.TrimSuffix(f.Name, "/")
		if p == "" || !utf8.ValidString(p) || strings.ContainsRune(p, 0) ||
			strings.ContainsRune(p, '\\') || strings.HasPrefix(f.Name, "/") {
			return nil, fmt.Errorf("refusing archive member %q", f.Name)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." {
				return nil, fmt.Errorf("refusing archive member %q", f.Name)
			}
		}
		names = append(names, p)
	}

	// Strip the single shared top-level directory when every member sits
	// under one root and at least one is nested (the reference's
	// archiveTopDir rule); flat or multi-root archives extract unchanged.
	strip := topDir(names)

	remaining := maxBytes
	var files []File
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		p := f.Name
		if strip != "" {
			p = strings.TrimPrefix(p, strip+"/")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("read archive member %q: %v", f.Name, err)
		}
		// The budget counts actual decompressed bytes — declared sizes are
		// not trusted.
		data, err := io.ReadAll(io.LimitReader(rc, remaining+1))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read archive member %q: %v", f.Name, err)
		}
		if int64(len(data)) > remaining {
			return nil, fmt.Errorf("skill archive exceeds %d bytes decompressed", maxBytes)
		}
		remaining -= int64(len(data))
		files = append(files, File{Path: p, Data: data})
	}
	return files, nil
}

// topDir returns the single top-level directory shared by every member path,
// or "" when the members are flat or span multiple roots.
func topDir(names []string) string {
	top, nested := "", false
	for _, p := range names {
		root, rest, hasRest := strings.Cut(p, "/")
		if top == "" {
			top = root
		} else if root != top {
			return ""
		}
		if hasRest && rest != "" {
			nested = true
		}
	}
	if !nested {
		return ""
	}
	return top
}

// TargetDir mirrors the reference worker's materialization-directory choice:
// the version object's name, falling back to the skill id when the name is
// empty or unusable as a single path segment.
func TargetDir(name, skillID string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) {
		return skillID
	}
	return name
}

// SentinelEntry is one materialized skill as the marker file records it: the
// resolved reference plus the directory it landed in, so a later skip
// decision can re-probe the tree without resolving names again.
type SentinelEntry struct {
	ID      string `json:"skill_id"`
	Version string `json:"version"`
	Dir     string `json:"directory"`
}

// Sentinel canonically encodes the materialized set for the marker file:
// sorted JSON, so equal sets always produce equal bytes regardless of order.
func Sentinel(entries []SentinelEntry) []byte {
	sorted := make([]SentinelEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	b, _ := json.Marshal(sorted)
	return b
}

// ParseSentinel decodes a marker file; ok is false only for bytes that are
// not a JSON array of entries, which a caller treats as "materialize". A
// pre-upgrade marker (entries without a "directory" field) parses cleanly
// with ok=true and an empty Dir — upgrade safety comes not from rejecting it
// here but from SentinelMatches' presence probe, whose path collapses to a
// nonexistent {workdir}/skills/SKILL.md for an empty Dir and so forces
// re-materialization. Do not special-case an empty Dir on the strength of
// this ok value.
func ParseSentinel(data []byte) ([]SentinelEntry, bool) {
	var entries []SentinelEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, false
	}
	return entries, true
}

// SentinelMatches reports whether a stored marker proves the resolved
// {skill_id: version} set is already fully materialized. The sandbox workdir
// is agent-writable, so the marker bytes alone cannot be trusted — a tool
// call may have deleted a skill tree while leaving the marker intact — and
// every recorded directory must still carry its SKILL.md (canonical archives
// always place one at the root). In-place content edits are deliberately not
// re-checked: presence is the tractable probe, and the residual is recorded
// in docs/DIVERGENCES.md. read is the sandbox's ReadFile, passed as a
// function so this package needs no sandbox dependency.
func SentinelMatches(ctx context.Context, read func(context.Context, string) ([]byte, error),
	workdir string, data []byte, resolved map[string]string) bool {
	entries, ok := ParseSentinel(data)
	if !ok || len(entries) != len(resolved) {
		return false
	}
	for _, e := range entries {
		if resolved[e.ID] != e.Version {
			return false
		}
		if _, err := read(ctx, path.Join(workdir, "skills", e.Dir, skillMDName)); err != nil {
			return false
		}
	}
	return true
}
