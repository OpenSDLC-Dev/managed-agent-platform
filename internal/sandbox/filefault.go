package sandbox

import (
	"crypto/rand"
	"encoding/hex"
)

// This file is the write path's shared shell. Both backends land a file's bytes
// under a temporary name in the target's own directory and rename it into place,
// and both name a blocked path the same way — so the piece of that reasoning a
// shell has to do lives here, embedded by each backend, rather than twice where
// the copies could drift. The contract suite (internal/sandbox/sandboxtest) pins
// the behavior from the outside; this keeps one implementation of it.

// Exit codes the shell below and the backends' write scripts use to name a path
// fault. They are a namespace shared by both backends, so a backend's private
// codes must not collide with them. The scripts spell them as literals, and each
// backend's script test compares what a real shell exited with against these
// constants — so a number that drifts fails a test rather than a session.
const (
	// ExitPathNotDirectory: something that is not a directory blocks the path.
	ExitPathNotDirectory = 15
	// ExitPathIsDirectory: the target of a write is itself a directory.
	ExitPathIsDirectory = 16
)

// TempPrefix names the file a write lands under before it is renamed into place.
// It is exported so the contract suite can assert that a failed write leaves none
// behind: a mount that dies part way through 500 MB must not leave 400 of them in
// the sandbox.
const TempPrefix = ".map-write-"

// PathFaultShell defines __map_path_fault, which both backends embed. Given a
// path, it exits ExitPathNotDirectory when the nearest existing component of that
// path — the path itself, if that exists — is not a directory, which is the
// ENOTDIR the kernel refused with; otherwise it returns 0 and the caller's own
// failure stands. Callers pass the path whose directory chain they needed: the
// directory a write must land in, or the parent of a file that could not be read.
//
// It walks up rather than testing one level because the block can be any distance
// above the leaf: `/tmp/afile/x/y` is refused by `/tmp/afile`, two levels up, and
// a backend that only looked at `/tmp/afile/x` would find nothing there and report
// the sandbox as broken. `-h` is tested alongside `-e` so a dangling symlink
// counts as a block: it exists as a name, `mkdir -p` will not build through it,
// and falling through to the directory above it would call the path fine.
//
// Every operation here is a bash builtin — `[`, `case`, and the `%` parameter
// expansion — and that is deliberate. The sandbox filesystem is the agent's, so a
// `dirname` on its PATH is the agent's too: one that echoed its argument back
// would spin this loop forever, and one that lied would answer for it. A builtin
// cannot be shadowed by a file on PATH, and the expansion is byte-exact where a
// `$(dirname …)` substitution would have eaten a component's trailing newlines.
// The loop terminates because each turn either returns or drops a component, and
// `/` and `.` both exist.
const PathFaultShell = `
__map_path_fault() {
  __p="$1"
  while :; do
    if [ -e "$__p" ] || [ -h "$__p" ]; then
      [ -d "$__p" ] || exit 15
      return 0
    fi
    case "$__p" in
      */?*) __p="${__p%/*}"; [ -n "$__p" ] || __p="/" ;;
      *)    return 0 ;;
    esac
  done
}
`

// TempName is the name a write lands under before being renamed into place. It
// goes in the target's own directory so the rename stays inside one filesystem
// and is therefore atomic — the whole reason writes are done this way — and it is
// random per call so two writes into one directory cannot collide. The leading dot
// keeps it out of a plain `ls` for the moment it exists.
func TempName() string {
	var b [8]byte
	// crypto/rand.Read does not fail on a running system, and the k8s backend's
	// exec nonce reads it the same way: a collision would cost one write.
	_, _ = rand.Read(b[:])
	return TempPrefix + hex.EncodeToString(b[:])
}
