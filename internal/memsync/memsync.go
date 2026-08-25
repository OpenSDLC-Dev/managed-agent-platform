// Package memsync holds the memory-store rules that both halves of the sync
// share (docs/plan/36_memory-stores.md decision 17). Two callers, one copy:
// internal/api validates what a client writes, and from slice 4 on the
// executor (which writes rows) and internal/worker (which goes over the wire)
// reconcile a sandbox directory against a store — a path or a body the routes
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
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	// MaxPathBytes and MaxContentBytes are the documented memory bounds — a
	// path of at most 1,024 bytes and content of at most 100 kB — counted in
	// BYTES, not runes, which is what the reference states for both
	// (anthropic-sdk-go v1.66.0 betamemorystorememory.go:207-218, 414-422).
	// They differ from the store surface's rune-counted "characters" for that
	// reason and no other.
	MaxPathBytes    = 1024
	MaxContentBytes = 102400

	// MaxMemoriesPerStore is the documented cap: 2,000 memories per store,
	// past which "writes to new memories fail … Existing memories remain
	// readable and editable" (the memory guide). The API's create and the
	// sync's create push both hold it.
	MaxMemoriesPerStore = 2000

	// markerPath is the path a memory would have to occupy to collide with the
	// per-mount marker file `.anthropic-memory-store` (decision 10), which
	// every store directory carries at its root. Only the root one collides: a
	// memory at /x/.anthropic-memory-store lands in a subdirectory, where the
	// worker's own exclusion is by path rather than by basename.
	markerPath = "/.anthropic-memory-store"
)

// ValidatePath holds decision 4's path table, which is the reference's own
// documented rule verbatim: "Must start with `/`, contain at least one
// non-empty segment, and be at most 1,024 bytes. Must not contain empty
// segments, `.` or `..` segments, control or format characters, and must be
// NFC-normalized."
//
// NFC is a rejection, not a normalization: the rule reads as a constraint on
// what a client may send, and normalizing silently would hand back a path the
// caller did not write while its own SHA-based preconditions still spoke of
// the original bytes.
//
// The marker path is this platform's own addition, registered as an inference:
// a memory there would overwrite the mount's marker file or be overwritten by
// it, and the marker is what tells a worker the directory is a real store.
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
	}
	if !norm.NFC.IsNormalString(path) {
		return errors.New("path must be NFC-normalized")
	}
	if path == markerPath {
		return fmt.Errorf("path %s is reserved for the memory store's marker file", markerPath)
	}
	return nil
}

// ValidateContent holds the other half of decision 4: at most 100 kB of valid
// UTF-8 text. The UTF-8 half is inert on the API lane — a JSON string decodes
// to valid UTF-8 by construction — and load-bearing on the sync lane, where
// the bytes come from a file in a sandbox.
func ValidateContent(content string) error {
	if len(content) > MaxContentBytes {
		return fmt.Errorf("content cannot exceed %d bytes", MaxContentBytes)
	}
	if !utf8.ValidString(content) {
		return errors.New("content must be valid UTF-8")
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
