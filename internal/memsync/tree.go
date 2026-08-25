package memsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

// MarkerName is the file every mounted store directory carries at its root
// (decision 10). Its bytes name the store the directory is a copy of; a
// directory whose marker is missing or altered is no longer trusted with
// pushes, because nothing says its files are that store's any more.
const MarkerName = ".anthropic-memory-store"

// MarkerBytes is the marker's content: a version line and the store id, which
// is what the reference worker writes and later re-hashes to decide whether a
// directory it finds is one it stamped (anthropic-sdk-go v1.66.0
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
// the store's own. An absent mount lists nothing and exits 0 — the empty tree
// the wipe guard then judges. Anything else that goes wrong reaches the
// caller as a non-zero exit or as something on stderr, and either is a
// listing not to act on: the pipeline's own status is xargs's, so a `find`
// that could not enter a directory would otherwise pass as a smaller tree
// than there is, and a `sha256sum` without `-z` (BusyBox's) as an empty one.
// It wants GNU coreutils (`sha256sum -z`, `sort -z`), which the sandbox
// images this platform ships and tests have.
func HashTreeCommand(mount string) string {
	m := shellQuote(mount)
	return "[ -d " + m + " ] || exit 0; cd " + m + " && find . -type f ! -path " + shellQuote("./"+MarkerName) +
		" -print0 | LC_ALL=C sort -z | xargs -0r sha256sum -z"
}

// removeCommandBytes bounds one removal command: the sandbox hands a command
// to its shell as a single argument, which Linux caps at 128 KiB, and 2,000
// memory paths of up to 1,024 bytes are well past that.
const removeCommandBytes = 32 << 10

// RemoveCommands is the apply phase's deletions, as one exec or several: the
// store's removed memories unlinked from the mount, then each one's directory
// removed as far up as it is empty — never the mount itself, which the
// relative `rmdir -p` cannot reach — so a memory the store later creates at a
// removed directory's path (`/a/b` gone, `/a` created, a transition its
// occupancy rule allows) finds no directory in its way. Paths are the
// memories' own, `/`-rooted at the mount. An unlink that fails fails its
// command; the directory pass is best-effort.
func RemoveCommands(mount string, paths []string) []string {
	var cmds, files, dirs []string
	size := 0
	flush := func() {
		if len(files) == 0 {
			return
		}
		cmd := "cd " + shellQuote(mount) + " && rm -f -- " + strings.Join(files, " ") + " || exit 1"
		if len(dirs) > 0 {
			cmd += "; rmdir -p --ignore-fail-on-non-empty -- " + strings.Join(dirs, " ") + " 2>/dev/null; exit 0"
		}
		cmds = append(cmds, cmd)
		files, dirs, size = nil, nil, 0
	}
	for _, p := range paths {
		rel := strings.TrimPrefix(p, "/")
		if size+2*len(rel) > removeCommandBytes {
			flush()
		}
		files = append(files, shellQuote(rel))
		size += len(rel) + 3
		if dir := path.Dir(rel); dir != "." && (len(dirs) == 0 || dirs[len(dirs)-1] != shellQuote(dir)) {
			dirs = append(dirs, shellQuote(dir))
			size += len(dir) + 3
		}
	}
	flush()
	return cmds
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
