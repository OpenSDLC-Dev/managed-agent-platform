package sandbox

import (
	"crypto/rand"
	"encoding/hex"
)

// This file is the write path's shared shell. Both backends land a file's bytes
// under a temporary name in the target's own directory and rename it into place,
// both carry the target's mode over to that temporary file first, and both name a
// blocked path the same way — so the piece of that reasoning a shell has to do
// lives here, embedded by each backend, rather than twice where the copies could
// drift. The contract suite (internal/sandbox/sandboxtest) pins the behavior from
// the outside; this keeps one implementation of it.

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

// PreserveModeShell defines __map_preserve_mode, which both backends embed. Given
// a write's target and the temporary file about to be renamed onto it, it puts the
// target's permission bits on the temporary file, so the rename replaces what a
// file holds without also replacing what it is allowed to do. Without it the
// workflow that breaks is an ordinary one: `write` a script, `chmod +x` it in
// bash, `edit` it, and it no longer runs (#204). The Claude Code harness's own
// atomic write does the same three steps — stat the target, chmod the temporary
// file, rename — which is a harness-design observation from the local snapshot,
// not a wire behavior of the managed-agents reference.
//
// Only an existing regular file has a mode worth carrying over, and the symlink is
// the case worth spelling out: `-h` is tested first because `-f` follows the link,
// and what the rename replaces is the *link*, not what it points at — taking the
// pointee's mode would dress the new file in the bits of a file this write never
// touched. Where there is nothing to carry over the temporary file keeps its own
// mode, which is the 0644 a fresh write has always landed (the docker backend's
// tar header, the k8s backend's `tee` under the image's 022 umask).
//
// Every failure here is silent, and deliberately so: the bytes are landed and the
// write is one `mv` from succeeding, so an image whose `stat` cannot do `-c` keeps
// the mode behavior it had before this existed rather than losing the write. The
// contract suite is what says the images we do support preserve the mode.
//
// `stat` and `chmod` come off the agent's own PATH, as `mkdir`, `tee`, `mv` and
// `rm` in both write paths already do, and a planted `stat` can therefore choose
// the mode this puts on the file. That is not a privilege it gains: the mode lands
// on a file the sandbox user owns, which the same agent could `chmod` directly. The
// value is one quoted argument to `chmod`, never a word to the shell, so a planted
// `stat` cannot get a second command out of it either.
const PreserveModeShell = `
__map_preserve_mode() {
  if [ -h "$1" ] || [ ! -f "$1" ]; then return 0; fi
  __m=$(stat -c %a "$1" 2>/dev/null) || return 0
  [ -n "$__m" ] || return 0
  chmod "$__m" "$2" 2>/dev/null || return 0
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
