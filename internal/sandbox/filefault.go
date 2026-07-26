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
// not a wire behavior of the managed-agents reference. (The SDK's own host-side
// agenttoolset writes a fixed 0644 instead; that divergence is in the registry.)
//
// Only an existing regular file has a mode worth carrying over. The symlink is the
// case worth spelling out, and `-h` is tested first because `-f` follows a link
// while `stat` does not: `stat -c %a` is an lstat, so a link reports its own 0777
// (measured, GNU coreutils and BusyBox alike) and an unguarded preservation would
// land the replacing file world-writable. What the rename replaces is the link, and
// a link has no mode worth having. A FIFO, socket or device node is skipped the
// same way. Where there is nothing to carry over, the temporary file keeps its own
// mode — the 0644 a fresh write lands from the docker backend's tar header, and
// from the k8s backend's `tee` under an image umask of 022.
//
// Every failure here is silent, and deliberately so: the bytes are landed and the
// write is one `mv` from succeeding, so a step that cannot run costs the mode
// rather than the write. Two paths reach it. An image whose `stat` cannot do `-c`
// keeps the mode behavior it had before this existed. And on the docker backend the
// temporary file is extracted by the daemon rather than created by the sandbox
// user, so an image whose default user is not root cannot chmod it and the write
// still lands 0644 — where the k8s backend, whose `tee` creates the file as the
// sandbox user, preserves the mode. That residual divergence is measured and
// tracked (#209); the contract suite runs root-default images, so it does not see
// it.
//
// `stat` and `chmod` come off the agent's own PATH, as `mkdir`, `tee`, `mv` and
// `rm` in both write paths already do, so a planted `stat` chooses what mode this
// applies. Two things bound that. The value is checked to be octal digits and
// passed as one quoted argument, so it can be neither an option (`--reference=` on
// some setuid binary) nor a second command. And the chmod runs with the agent's own
// credentials on a file in a directory the agent already writes — so even a planted
// `stat` that steers it, or a symlink swapped in for the temporary file (chmod
// follows one), reaches nothing the agent could not reach with its own `chmod`.
const PreserveModeShell = `
__map_preserve_mode() {
  if [ -h "$1" ] || [ ! -f "$1" ]; then return 0; fi
  __m=$(stat -c %a "$1" 2>/dev/null) || return 0
  case "$__m" in *[!0-7]*|'') return 0 ;; esac
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
