// Package memsync holds the memory-store rules that both halves of the sync
// share (docs/plan/36_memory-stores.md decision 17). Two callers, one copy:
// internal/api validates what a client writes, and the executor (which writes
// rows, from slice 4) and internal/worker (which goes over the wire, from
// slice 6) reconcile a sandbox directory against a store — a path or a body the routes
// would refuse has to be refused locally too, or a run spends a round trip
// learning it.
//
// Slice 2 put the path and content rules and the mount slug here; slice 4 the
// rest of the shared half: the marker file's bytes, the baseline file's
// encoding, the tree-hash command and its parser (tree.go), and the pure
// decision table Plan(local, baseline, remote) → actions that both engines
// apply (plan.go).
//
// Nothing here touches a database, a sandbox or the network — it is text rules
// over strings, so either caller can use it without carrying the other's
// dependencies.
package memsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	// MaxPathBytes and MaxContentBytes are the documented memory bounds — a
	// path of at most 1,024 bytes and content of at most 100 kB — counted in
	// BYTES, not runes, which is what the reference states for both
	// (checked against anthropic-sdk-go v1.66.0 — betamemorystorememory.go
	// BetaManagedAgentsMemory.Path and BetaManagedAgentsMemory.Content and
	// BetaMemoryStoreMemoryNewParams.Content and
	// BetaMemoryStoreMemoryNewParams.Path). They differ from the store
	// surface's rune-counted "characters" for that reason and no other.
	MaxPathBytes    = 1024
	MaxContentBytes = 102400

	// MaxMemoriesPerStore is the documented cap: 2,000 memories per store,
	// past which "writes to new memories fail … Existing memories remain
	// readable and editable" (the memory guide). The API's create and the
	// sync's create push both hold it.
	MaxMemoriesPerStore = 2000
)

// IsMarkerPath reports whether a memory at path would land on the marker file
// every store directory carries at its root (decision 10) — the reference
// client's own test, exactly (checked against anthropic-sdk-go v1.70.1 —
// memories.go SessionMemoryStores.listMemories): the path with its leading
// slashes trimmed is the marker's name. The reference accepts a memory there
// (#669); the update route refuses a rename onto it, and the consumers skip
// it with ShadowsMarker.
func IsMarkerPath(path string) bool {
	return strings.TrimLeft(path, "/") == MarkerName
}

// ShadowsMarker reports whether a memory at path can never land in a mounted
// store directory: one at the marker's path would be written over the marker,
// and one under it needs a directory where the marker file is, which fails
// every write batch it rides in. It is what every consumer that lands or
// syncs a store skips. The reference client skips the marker's path alone
// (IsMarkerPath); the descendants are ours, registered in docs/DIVERGENCES.md.
// Nothing else collides — a memory at /x/.anthropic-memory-store lands in a
// subdirectory, and the tree hash excludes the marker by path rather than by
// basename.
func ShadowsMarker(path string) bool {
	return IsMarkerPath(path) || strings.HasPrefix(strings.TrimLeft(path, "/"), MarkerName+"/")
}

// WarnShadowedMemory logs a memory ShadowsMarker skipped, from whichever
// consumer skipped it — the reference worker's own warning when its listing
// carries the marker's path (checked against anthropic-sdk-go v1.70.1 —
// memories.go SessionMemoryStores.listMemories).
func WarnShadowedMemory(ctx context.Context, sessionID, storeID, path string) {
	slog.WarnContext(ctx, "the store holds a memory at or under the reserved marker path; skipping",
		"session_id", sessionID, "memory_store_id", storeID, "path", path)
}

// ValidatePath holds the reference's own documented path rule verbatim
// (checked against anthropic-sdk-go v1.70.1 — betamemorystorememory.go
// BetaMemoryStoreMemoryNewParams.Path): "Must start with `/`, contain at least
// one non-empty segment, and be at most 1,024 bytes. Must not contain empty
// segments, `.` or `..` segments, control or format characters, or the Unicode
// line and paragraph separators (U+2028, U+2029), and must be NFC-normalized.
// Paths are case-sensitive."
//
// docs/plan/36_memory-stores.md decision 4 is the same table as it read at the
// SDK version pinned then, before the separators clause: an archived plan is
// the record of a decision, so it keeps the rule it was decided against.
//
// NFC is a rejection, not a normalization: the rule reads as a constraint on
// what a client may send, and normalizing silently would hand back a path the
// caller did not write while its own SHA-based preconditions still spoke of
// the original bytes.
//
// The marker's path is not refused here: the reference accepts a create there
// (#669), and ShadowsMarker is how the consumers keep such a memory, or one
// under it, off the marker file. A rename onto the marker's path is refused by
// the update route alone.
func ValidatePath(path string) error {
	if path == "" {
		return errors.New("path is required")
	}
	if !strings.HasPrefix(path, "/") {
		return errors.New(`path must start with "/"`)
	}
	if len(path) > MaxPathBytes {
		return fmt.Errorf("path cannot exceed %d bytes", MaxPathBytes)
	}
	if path == "/" {
		return errors.New("path must contain at least one non-empty segment")
	}
	for _, segment := range strings.Split(path[1:], "/") {
		switch segment {
		case "":
			return errors.New("path cannot contain empty segments")
		case ".", "..":
			return errors.New(`path cannot contain "." or ".." segments`)
		}
	}
	// Load-bearing on the sync lane only, like ValidateContent's: an invalid
	// byte ranges as U+FFFD, which no rule below would catch.
	if !utf8.ValidString(path) {
		return errors.New("path must be valid UTF-8")
	}
	for _, r := range path {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return errors.New("path cannot contain control or format characters")
		}
		// The separators are their own categories — Zl and Zp — so the Cc/Cf
		// test above does not reach them, and the rule names them separately
		// for that reason. Matched as the two code points the rule gives
		// rather than as their categories, which are singletons only today.
		if r == '\u2028' || r == '\u2029' {
			return errors.New("path cannot contain line or paragraph separators")
		}
	}
	if !norm.NFC.IsNormalString(path) {
		return errors.New("path must be NFC-normalized")
	}
	return nil
}

// ValidateContent holds the other half of decision 4: at most 100 kB of valid
// UTF-8 text — and text Postgres can hold, which valid UTF-8 does not settle:
// U+0000 is a legal code point that a text column refuses (SQLSTATE 22021).
// Both of those halves are inert on the API lane — a JSON string decodes to
// valid UTF-8 by construction, and the API refuses a NUL anywhere in a body
// before any field binds — and load-bearing on the sync lane, where the
// bytes come from a file in a sandbox: unrefused, a NUL in one file would
// fail its store's whole settlement on every run until the sandbox died.
func ValidateContent(content string) error {
	if len(content) > MaxContentBytes {
		return fmt.Errorf("content cannot exceed %d bytes", MaxContentBytes)
	}
	if !utf8.ValidString(content) {
		return errors.New("content must be valid UTF-8")
	}
	if strings.ContainsRune(content, 0) {
		return errors.New("content must not contain U+0000")
	}
	return nil
}

// Slug renders a store's display name as the directory name it mounts under
// (decision 8): "The directory name is the store's display name sanitized to a
// filesystem-safe slug (lowercased; non-alphanumeric runs become a single
// hyphen)". Two readings the reference leaves open are settled here and
// registered: leading and trailing hyphens are trimmed — the documented rule
// alone would mount "(Notes)" at /mnt/memory/-notes- — and "alphanumeric"
// means ASCII, so a name written in another script slugs to its ASCII
// remainder. A name with nothing left is the caller's fallback.
func Slug(name, fallback string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	if slug := strings.TrimSuffix(b.String(), "-"); slug != "" {
		return slug
	}
	return fallback
}
