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

// MaxArchiveBytes caps how many bytes a materializer reads from an object-store
// stream before handing the archive to Extract. It matches the decompressed
// cap: a canonical zip never exceeds its own decompressed size, so a stored
// object larger than this is malformed or hostile and Extract would reject it
// anyway. Bounding the read keeps a corrupt or oversized object from OOMing the
// executor/worker via an unbounded io.ReadAll.
const MaxArchiveBytes = ExtractMaxBytes

// ReadArchive reads a skill archive from an object-store stream, refusing more
// than MaxArchiveBytes so a hostile or corrupt object cannot exhaust memory.
// sizeHint is the store's reported length (Content-Length / object size, 0 or
// negative when unknown): it only sizes the initial buffer, never relaxes the
// cap. The cap is enforced on bytes actually read, so a lying hint cannot beat
// it.
func ReadArchive(r io.Reader, sizeHint int64) ([]byte, error) {
	return readArchiveLimited(r, sizeHint, MaxArchiveBytes)
}

func readArchiveLimited(r io.Reader, sizeHint, maxBytes int64) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	if sizeHint > 0 && sizeHint <= maxBytes {
		buf.Grow(int(sizeHint))
	}
	n, err := buf.ReadFrom(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read skill archive: %v", err)
	}
	if n > maxBytes {
		return nil, fmt.Errorf("skill archive exceeds %d bytes", maxBytes)
	}
	return buf.Bytes(), nil
}

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

// Resolved is a skill the caller intends to materialize: its id, the concrete
// version resolved at use time, and the directory it lands in. Dir is
// TargetDir of the version's name derived from TRUSTED metadata (the DB row
// or the version object), never from the marker — so the skip's presence
// probe cannot be redirected by an agent that rewrites the marker.
type Resolved struct {
	ID      string
	Version string
	Dir     string
}

// markerEntry is one skill as the marker file records it: {skill_id, version}
// only. The directory is deliberately NOT recorded — it is recomputed from
// trusted metadata on the next pass, so a marker an agent tool rewrote cannot
// point the presence probe at a decoy directory.
type markerEntry struct {
	ID      string `json:"skill_id"`
	Version string `json:"version"`
}

// Sentinel canonically encodes the materialized set for the marker file:
// sorted {skill_id, version} JSON, so equal sets always produce equal bytes.
func Sentinel(rs []Resolved) []byte {
	entries := make([]markerEntry, len(rs))
	for i, r := range rs {
		entries[i] = markerEntry{ID: r.ID, Version: r.Version}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	b, _ := json.Marshal(entries)
	return b
}

// ParseSentinel decodes a marker file; ok is false only for bytes that are
// not a JSON array of {skill_id, version} entries (an unreadable or older
// marker), which a caller treats as "materialize".
func ParseSentinel(data []byte) ([]markerEntry, bool) {
	var entries []markerEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, false
	}
	return entries, true
}

// SentinelMatches reports whether the marker proves the resolved set rs is
// already fully materialized. It is sound against an agent-writable marker:
//   - the probe directory is rs[i].Dir (trusted metadata), never the marker;
//   - the marker's {id, version} set must be an EXACT bijection with rs (same
//     length, each rs id present exactly once with its version), so a forged,
//     duplicated, or zero-value entry cannot mask a missing skill;
//   - every resolved directory must still hold its SKILL.md (canonical
//     archives place one at the root).
//
// An in-place content edit of a materialized file is deliberately not detected
// — presence is the tractable proxy, the residual is recorded in
// docs/DIVERGENCES.md. read is the sandbox's ReadFile, passed as a function so
// this package needs no sandbox dependency. rs must be deduplicated by id.
func SentinelMatches(ctx context.Context, read func(context.Context, string) ([]byte, error),
	workdir string, data []byte, rs []Resolved) bool {
	entries, ok := ParseSentinel(data)
	if !ok || len(entries) != len(rs) {
		return false
	}
	want := make(map[string]string, len(rs))
	for _, r := range rs {
		if _, dup := want[r.ID]; dup {
			return false // a deduped set is the caller's contract; be safe
		}
		want[r.ID] = r.Version
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		v, known := want[e.ID]
		if !known || seen[e.ID] || v != e.Version {
			return false
		}
		seen[e.ID] = true
	}
	for _, r := range rs {
		if _, err := read(ctx, path.Join(workdir, "skills", r.Dir, skillMDName)); err != nil {
			return false
		}
	}
	return true
}
