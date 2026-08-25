package memsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MarkerName is the file every mounted store directory carries at its root
// (decision 10). Its bytes name the store the directory is a copy of; a
// directory whose marker is missing or altered is no longer trusted with
// pushes, because nothing says its files are that store's any more.
const MarkerName = ".anthropic-memory-store"

// MarkerBytes is the marker's content: a version line and the store id, which
// is what the reference worker writes and later re-hashes to decide whether a
// directory it finds is one it stamped (anthropic-cli v1.26.1
// lib/environments/memories.go, scanMarker/markerSHA).
func MarkerBytes(storeID string) []byte {
	return []byte("version 1\n" + storeID)
}

// Baseline is what the directory and the store last agreed on, kept beside the
// mounts (decision 11): Synced maps a memory path to the digest both sides
// held after the last sync, which is how the next one tells an edit from a
// remote change; Refused maps a path to the digest the store refused (a path
// or body the routes would reject, a full store), which is skipped rather
// than retried until the bytes change.
type Baseline struct {
	Synced  map[string]string `json:"synced,omitempty"`
	Refused map[string]string `json:"refused,omitempty"`
}

// Encode is the baseline file's bytes: JSON with sorted keys, so an unchanged
// baseline rewrites to identical bytes.
func (b Baseline) Encode() []byte {
	data, _ := json.Marshal(b) // maps of strings cannot fail to marshal
	return data
}

// DecodeBaseline reads Encode's bytes back. Nothing — an absent file, an empty
// one — is an empty baseline, and either way both maps are usable.
func DecodeBaseline(data []byte) (Baseline, error) {
	var b Baseline
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &b); err != nil {
			return Baseline{}, fmt.Errorf("memsync: baseline: %w", err)
		}
	}
	if b.Synced == nil {
		b.Synced = map[string]string{}
	}
	if b.Refused == nil {
		b.Refused = map[string]string{}
	}
	return b, nil
}

// HashTreeCommand is the read phase's one exec: every regular file under the
// mount but the marker, with its SHA-256, NUL-terminated so any file name
// survives, and sorted under LC_ALL=C so the listing is in byte order like
// the store's own. It wants GNU coreutils (`sha256sum -z`, `sort -z`), which
// the sandbox images this platform ships and tests have; a shell that lacks
// them fails the exec, and the caller skips that store's sync and says so.
func HashTreeCommand(mount string) string {
	return "cd " + shellQuote(mount) + " && find . -type f ! -path " + shellQuote("./"+MarkerName) +
		" -print0 | LC_ALL=C sort -z | xargs -0r sha256sum -z"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ParseHashTree reads HashTreeCommand's stdout into memory paths and digests.
// A record is `<64 hex>  ./<name>` (or ` *` before the name, sha256sum's
// binary-mode spelling) ending in NUL; a record missing its NUL is refused
// rather than trusted, since a cut-off capture ends in exactly that.
func ParseHashTree(stdout []byte) (map[string]string, error) {
	files := map[string]string{}
	for len(stdout) > 0 {
		end := bytes.IndexByte(stdout, 0)
		if end < 0 {
			return nil, errors.New("memsync: hash listing ends mid-record")
		}
		rec := stdout[:end]
		stdout = stdout[end+1:]
		if len(rec) < 64+2+2 || !isHex(rec[:64]) || rec[64] != ' ' || (rec[65] != ' ' && rec[65] != '*') ||
			rec[66] != '.' || rec[67] != '/' || len(rec) == 68 {
			return nil, fmt.Errorf("memsync: malformed hash record %q", rec)
		}
		path := string(rec[67:])
		if _, dup := files[path]; dup {
			return nil, fmt.Errorf("memsync: path %q listed twice", path)
		}
		files[path] = string(rec[:64])
	}
	return files, nil
}

func isHex(b []byte) bool {
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
