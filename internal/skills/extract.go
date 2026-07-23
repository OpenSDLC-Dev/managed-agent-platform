package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// MaxArchiveBytes caps how many *compressed* bytes a materializer reads from an
// object-store stream before handing the archive to Extract. It is set well
// above the platform's upload limit (MaxTotalBytes, 30 MB) but far below
// ExtractMaxBytes: a canonical zip is built from at most MaxTotalBytes of
// validated content, so its compressed form fits comfortably here, while a
// stored object larger than this is malformed or hostile and is refused before
// it can consume memory — Extract's own decompressed cap then guards what a
// valid archive expands to. Capping the *read* at a realistic archive size (not
// the gigabyte decompressed ceiling) is what keeps refusing a hostile object
// from ever needing a gigabyte-scale allocation.
const MaxArchiveBytes = 64 << 20

// ErrDigestMismatch reports an archive whose bytes do not hash to the digest
// recorded for that skill version at upload — storage bit-rot, truncation, or a
// substituted object between upload and materialization.
var ErrDigestMismatch = errors.New("skill archive does not match its recorded sha256")

// Digest is the archive digest this platform records and verifies: the
// lowercase-hex sha256 of the stored bytes.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReadArchive reads a skill archive from an object-store stream, refusing more
// than MaxArchiveBytes so a hostile or corrupt object cannot exhaust memory,
// then verifies it against the digest recorded for that version.
// The store's reported length is deliberately NOT used to pre-size the buffer:
// it is untrusted, and a large hint would let a tiny hostile stream provoke a
// huge eager allocation. The cap is enforced on bytes actually read.
//
// Verification lives here, in the one function both materialization halves call
// between fetching an archive and extracting it, so a caller cannot read an
// archive and forget to check it. wantSHA256 empty means no digest was recorded
// for this version — a row predating the sha256 column, or (on the wire half) a
// control plane that sends no digest header — and the archive is read
// unverified; callers log that. Anything else must match, case-insensitively:
// our own digests are lowercase by construction, but rejecting another
// implementation's uppercase hex would be a gratuitous failure. A malformed
// expectation needs no format check of its own — it can never equal a real
// digest, so it fails closed.
func ReadArchive(r io.Reader, wantSHA256 string) ([]byte, error) {
	data, err := readArchiveLimited(r, MaxArchiveBytes)
	if err != nil {
		return nil, err
	}
	if wantSHA256 != "" {
		if got := Digest(data); !strings.EqualFold(wantSHA256, got) {
			return nil, fmt.Errorf("%w: read %d bytes hashing to %s, expected %s",
				ErrDigestMismatch, len(data), got, wantSHA256)
		}
	}
	return data, nil
}

// readArchiveLimited reads the stream into one contiguous buffer whose capacity
// is grown by doubling but hard-clamped at maxBytes, then probes one more byte
// to detect an oversized object. Growing the buffer ourselves — rather than via
// io.ReadAll or bytes.Buffer.ReadFrom — is deliberate: both of those overshoot
// to ~2x the cap at the boundary (bytes.Buffer.ReadFrom grows *before* the read
// that returns EOF; io.ReadAll retains growing chunks and reallocates), which
// is a needless multi-hundred-MB spike here and would be a gigabyte spike or a
// 32-bit panic under a larger cap. The clamp means the buffer never allocates
// past maxBytes, and a hostile object is refused via the probe without reading
// the rest of the stream. The probe reads from the raw reader so the cap on the
// buffered bytes and the "is there more?" test stay independent.
func readArchiveLimited(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes < 1 {
		maxBytes = 1
	}
	const initialCap = 64 << 10
	c := int64(initialCap)
	if c > maxBytes {
		c = maxBytes
	}
	buf := make([]byte, 0, c)
	empty := 0
	for {
		if len(buf) == cap(buf) {
			if int64(cap(buf)) >= maxBytes {
				return probeOverCap(r, buf, maxBytes)
			}
			next := int64(cap(buf)) * 2
			if next > maxBytes {
				next = maxBytes
			}
			grown := make([]byte, len(buf), next)
			copy(grown, buf)
			buf = grown
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		switch {
		case err == io.EOF:
			return buf, nil
		case err != nil:
			return nil, fmt.Errorf("read skill archive: %v", err)
		case n > 0:
			empty = 0
		default:
			// A reader that returns (0, nil) is discouraged but permitted; a
			// broken one that does so forever must not pin the goroutine.
			empty++
			if empty >= maxEmptyReads {
				return nil, fmt.Errorf("read skill archive: %w", io.ErrNoProgress)
			}
		}
	}
}

// maxEmptyReads bounds consecutive no-progress (0-byte, nil-error) reads before
// a Reader is declared broken — the same guard bufio applies.
const maxEmptyReads = 100

// probeOverCap reports whether the reader still holds a byte past a buffer
// already filled to the cap; any byte means the object exceeds maxBytes. Like
// the main loop it bounds no-progress reads so a broken reader cannot hang it,
// and it reads only one byte at a time so the buffer never grows past the cap.
func probeOverCap(r io.Reader, buf []byte, maxBytes int64) ([]byte, error) {
	var b [1]byte
	for empty := 0; ; {
		n, err := r.Read(b[:])
		if n > 0 {
			return nil, fmt.Errorf("skill archive exceeds %d bytes", maxBytes)
		}
		switch {
		case err == io.EOF:
			return buf, nil
		case err != nil:
			return nil, fmt.Errorf("read skill archive: %v", err)
		default:
			empty++
			if empty >= maxEmptyReads {
				return nil, fmt.Errorf("read skill archive: %w", io.ErrNoProgress)
			}
		}
	}
}

// BlobKey is the object-store key for a skill version's archive — the one
// layout the API's upload/download, the importer, and the executor's
// materialization all share.
func BlobKey(skillID, version string) string {
	return "skills/" + skillID + "/" + version + ".zip"
}

// ArchiveDigestHeader carries Digest(archive) on the /content download response
// so the BYOC worker — which never reads the database — can verify what it
// downloaded. It lives beside BlobKey for the same reason: one definition
// shared by the API that sends it and the worker that reads it. Additive and
// ignored by reference clients (the SDK treats the body as opaque bytes).
const ArchiveDigestHeader = "x-skill-archive-sha256"

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
	// SHA256 is the archive digest recorded for this version, from the same
	// trusted metadata that supplies Dir — empty when the source records none.
	// The executor fills it from the version row it already reads; the BYOC
	// worker leaves it empty because the SDK's version object carries no
	// checksum field, and learns the digest from the download response instead.
	SHA256 string
}

// SentinelVersion is the marker's integrity generation — what a successful
// materialization the marker records was actually guaranteed to have done.
// Version 2 means "every recorded archive was verified against the digest the
// registry holds for it, where one was recorded" (#155); version 1 was the
// bare, unversioned JSON array written before any digest existed, and is no
// longer accepted. Bumping this is how a marker written under a weaker
// guarantee is stopped from satisfying a stronger one: it costs exactly one
// re-materialization pass per live sandbox at upgrade — which is where the
// stronger guarantee gets applied — and nothing at steady state. Recording the
// digests themselves would be the alternative, and is not viable: the BYOC
// worker learns a digest only from the download response, i.e. after the skip
// decision, so it would have to spend a wire round trip per skill per pass to
// answer a question this constant answers for free.
const SentinelVersion = 2

// markerFile is the marker's on-disk shape: the integrity generation plus the
// materialized set.
type markerFile struct {
	Version int           `json:"v"`
	Skills  []markerEntry `json:"skills"`
}

// markerEntry is one skill as the marker file records it: {skill_id, version}
// only. The directory is deliberately NOT recorded — it is recomputed from
// trusted metadata on the next pass, so a marker an agent tool rewrote cannot
// point the presence probe at a decoy directory.
type markerEntry struct {
	ID      string `json:"skill_id"`
	Version string `json:"version"`
}

// Sentinel canonically encodes the materialized set for the marker file: the
// integrity generation plus sorted {skill_id, version} entries, so equal sets
// always produce equal bytes.
func Sentinel(rs []Resolved) []byte {
	entries := make([]markerEntry, len(rs))
	for i, r := range rs {
		entries[i] = markerEntry{ID: r.ID, Version: r.Version}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	b, _ := json.Marshal(markerFile{Version: SentinelVersion, Skills: entries})
	return b
}

// ParseSentinel decodes a marker file; ok is false for bytes that are not a
// marker of the current integrity generation — unreadable, or written under an
// older one (including the unversioned array form) — which a caller treats as
// "materialize", so the current generation's guarantees are applied on that
// pass. A marker from a *newer* generation is likewise not accepted: a
// downgraded binary must re-materialize rather than trust a claim it cannot
// evaluate.
func ParseSentinel(data []byte) ([]markerEntry, bool) {
	var f markerFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != SentinelVersion {
		return nil, false
	}
	return f.Skills, true
}

// SentinelMatches reports whether the marker proves the resolved set rs is
// already fully materialized. Against an agent-writable marker it holds these:
//   - the probe directory is rs[i].Dir (trusted metadata), never the marker,
//     so a rewritten marker cannot redirect the probe at a decoy directory;
//   - the marker's {id, version} set must be an EXACT bijection with rs (same
//     length, each rs id present exactly once with its version), so a forged,
//     duplicated, or zero-value entry cannot mask a skill that is absent from
//     its directory;
//   - every resolved directory must still hold its SKILL.md (canonical
//     archives place one at the root), so a deleted tree self-heals next pass.
//
// It is NOT a soundness proof against a fully hostile agent: the SKILL.md probe
// tests presence, not content, so an agent that both forges the marker's
// version and leaves an older version's files in place (the landing directory
// is the skill name, shared across a skill's versions) can suppress an upgrade
// — the same tampering class as an in-place content edit, and equally beyond a
// presence probe. Both residuals are recorded in docs/DIVERGENCES.md; closing
// them would mean abandoning the skip and re-extracting every pass, as the
// reference does. read is the sandbox's ReadFile, passed as a function so this
// package needs no sandbox dependency. rs must be deduplicated by id.
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
