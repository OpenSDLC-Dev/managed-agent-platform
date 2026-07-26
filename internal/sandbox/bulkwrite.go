package sandbox

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	gopath "path"
	"strconv"
	"strings"
	"time"
)

// This file is the bulk write path's shared half, as filefault.go is the single
// write's. Both backends land a batch the same way — one archive carrying a
// manifest and one temporary file per member, then one exec that renames every
// member into place — so the archive, the manifest format and the shell live
// here once. What differs is only how the archive travels: the docker daemon
// extracts it, the k8s backend pipes it through `tar` inside the pod.

// FileWrite is one member of a bulk write. Path must be absolute and clean
// (`/a/b`, never `/a/../b` or `/a/b/`), because it also names an entry in the
// archive that carries it; Data is the file's bytes.
type FileWrite struct {
	Path string
	Data []byte
}

// BulkWrite is one prepared batch: the archive a backend delivers, the two
// bookkeeping files the shared scripts read, and the classification of what the
// scripts report back. A backend builds one, delivers Archive, and runs the
// scripts; the batch is replayable, so a delivery that failed can be retried
// from the same value.
type BulkWrite struct {
	// Manifest names the file listing `tmp\0target\0` for every member, and
	// DirList the deduplicated parent directories. Both land in the workdir,
	// which exists, and both are the archive's first entries — so a delivery that
	// fails on a later one has still landed what the recovery pass needs. The
	// rename script removes them.
	Manifest string
	DirList  string

	files    []FileWrite
	tmps     []string
	manifest []byte
	dirs     []byte
}

// NewBulkWrite prepares files for delivery into a sandbox whose workdir is
// workdir (empty means DefaultWorkdir). Every member's temporary file goes in
// its own target's directory — that is what keeps the rename inside one
// filesystem, and therefore atomic — and every member of one batch shares a
// nonce, so two concurrent batches into one directory cannot collide.
func NewBulkWrite(workdir string, files []FileWrite) (*BulkWrite, error) {
	if workdir == "" {
		workdir = DefaultWorkdir
	}
	nonce := TempName()
	b := &BulkWrite{
		Manifest: gopath.Join(workdir, nonce),
		DirList:  gopath.Join(workdir, nonce+".dirs"),
		files:    files,
		tmps:     make([]string, 0, len(files)),
	}
	var manifest, dirs bytes.Buffer
	seen := make(map[string]bool, len(files))
	for i, f := range files {
		if f.Path == "" || f.Path[0] != '/' || f.Path == "/" || gopath.Clean(f.Path) != f.Path {
			return nil, fmt.Errorf("sandbox: bulk write path %q is not an absolute, clean path", f.Path)
		}
		// A NUL is what separates the manifest's fields, so a path carrying one
		// would split its own record: the shell would read the bytes before it as
		// the whole target, rename the member onto THAT path, and take the
		// remainder for the next record — a write landing somewhere the caller
		// never named, reported as a success. No such path can exist on a POSIX
		// filesystem anyway, so refusing costs nothing and closes the misroute.
		if strings.ContainsRune(f.Path, 0) {
			return nil, fmt.Errorf("sandbox: bulk write path %q contains a NUL byte", f.Path)
		}
		dir := gopath.Dir(f.Path)
		tmp := gopath.Join(dir, nonce+"-"+strconv.Itoa(i))
		b.tmps = append(b.tmps, tmp)
		manifest.WriteString(tmp)
		manifest.WriteByte(0)
		manifest.WriteString(f.Path)
		manifest.WriteByte(0)
		if !seen[dir] {
			seen[dir] = true
			dirs.WriteString(dir)
			dirs.WriteByte(0)
		}
	}
	b.manifest, b.dirs = manifest.Bytes(), dirs.Bytes()
	return b, nil
}

// Archive streams the batch's tar to w: the manifest, the directory list, then
// one entry per member under its temporary name. Entry names are relative, so
// both untars extract it at `/`.
//
// It carries no directory entries, and that is deliberate: an explicit directory
// entry chmods a directory that already exists (measured, 0700 → 0755 under both
// untars), and a write must not change the mode of a directory it merely passes
// through. The parents an entry needs are left to the untar, which creates them —
// 0755 on both backends, with the file at 0644.
//
// Those 0755 directories are the platform's answer rather than the image's, and
// they are NOT what the single write gives: its `mkdir -p` runs inside the sandbox
// and takes the image's umask, so a hardened image gets 0700 there and 0755 here.
// The docker daemon extracts on the host and creates a missing parent 0755
// whatever the image's umask says, so the two backends can agree only by fixing
// the answer, and cross-backend agreement is the property the contract suite
// exists to hold. The k8s script's `umask 022` matches it — and is load-bearing
// for the file mode besides: measured, a non-root sandbox user extracting under a
// 077 umask lands the file 0600 without it, where #212 requires 0644. Following
// the image's umask on both instead would take a round trip of its own to
// pre-create the directories, to buy a difference only a second UID inside the
// same sandbox can see.
func (b *BulkWrite) Archive(w io.Writer) error {
	tw := tar.NewWriter(w)
	if err := tarEntry(tw, b.Manifest, b.manifest, 0o600); err != nil {
		return err
	}
	if err := tarEntry(tw, b.DirList, b.dirs, 0o600); err != nil {
		return err
	}
	for i, f := range b.files {
		if err := tarEntry(tw, b.tmps[i], f.Data, 0o644); err != nil {
			return err
		}
	}
	return tw.Close()
}

func tarEntry(tw *tar.Writer, path string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(path, "/"),
		Mode:     mode,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
	}); err != nil {
		return fmt.Errorf("sandbox: build bulk archive: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("sandbox: build bulk archive: %w", err)
	}
	return nil
}

// Fault turns what a shared bulk script exited with into the error the caller
// sees. backend names the backend for the messages that are its own failure
// rather than the path's, exactly as the single-file writes name it. stderr is
// the exec's: it carries the marker naming which member failed, and — for a
// blocked path — `mkdir`'s own message naming the directory, which is better
// than anything invented here.
func (b *BulkWrite) Fault(backend string, code int, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	switch code {
	case ExitPathIsDirectory:
		return fmt.Errorf("%s: %w", b.blamed(stderr), ErrIsDirectory)
	case ExitPathNotDirectory:
		return fmt.Errorf("%s: bulk write: %w (%s)", backend, ErrNotDirectory, stderr)
	case ExitBulkIncomplete:
		return fmt.Errorf("%s: bulk write %s: the archive did not deliver every member",
			backend, b.blamed(stderr))
	default:
		return fmt.Errorf("%s: bulk write: exit %d: %s", backend, code, stderr)
	}
}

// bulkFailMarker prefixes the line a script prints to name the member it failed
// on, by index. A marker and an index rather than the path itself because an
// image's own noise shares that stream: a line nothing else writes, holding a
// number that indexes what this side already has, cannot be confused with it.
const bulkFailMarker = "map-bulk-fail "

// blamed resolves the marker in stderr back to the member's path, falling back
// to naming the batch when there is no usable marker — a script that died before
// printing one, or an image that wrote over it. The search runs backwards
// because the script prints its marker last, immediately before it exits:
// anything wearing the marker ahead of that came from the image.
func (b *BulkWrite) blamed(stderr string) string {
	lines := strings.Split(stderr, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, bulkFailMarker) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(line, bulkFailMarker))
		if err != nil || n < 0 || n >= len(b.files) {
			continue
		}
		return b.files[n].Path
	}
	return fmt.Sprintf("(one of %d files)", len(b.files))
}

// BulkRenameShell defines __map_bulk_rename, which both backends embed: given
// the manifest ($1) and the directory list ($2) an already-delivered archive
// landed, it renames every member into place and takes both files away with it.
// It is the bulk half of what the single write's own rename does, member by
// member and for one exec instead of N.
//
// Each member is the single write's sequence exactly — refuse a target that is a
// directory, carry the target's mode onto the temporary file, move, then ask
// again in case something made the target a directory in between — so a batch
// and a loop of writes land a file identically. Only the batch's *shape* is new:
// the first failure stops the run, and the members that already landed stay
// landed, because that is what the loop this replaces did.
//
// A temporary file the manifest lists but that is not there is the delivery having
// lost it, and stops the run at that member (ExitBulkIncomplete). That is the k8s
// backend's short-stream guard, restated for a batch: nothing downstream of
// client-go can see a stdin stream that ended early, so a batch that arrived short
// has to be noticed here or not at all. What it costs is what any other failure
// mid-batch costs — the members ahead of the gap are already renamed and stay,
// exactly as D2 says — and what it buys is that the call fails instead of
// reporting success over a tree with holes in it.
//
// Everything the loop drops on the way out is a temporary file nobody will ever
// claim: the member that failed, and every member after it, are removed rather
// than left in the sandbox.
//
// The manifest is a file on the agent's own filesystem, so a command running in
// the sandbox can rewrite it between the delivery and this pass and steer where
// a member lands — as it can already swap the temporary file a single write is
// about to rename. It is the same bound in both cases and it is not a new one:
// the move runs with the agent's own credentials, so a redirected member reaches
// nothing the agent could not have written with its own `mv`.
const BulkRenameShell = PreserveModeShell + `
__map_bulk_rename() {
  __tmps=(); __dsts=(); __bad=-1
  while IFS= read -r -d '' __t && IFS= read -r -d '' __d; do
    if [ ! -f "$__t" ] && [ "$__bad" -lt 0 ]; then __bad=${#__tmps[@]}; fi
    __tmps+=("$__t"); __dsts+=("$__d")
  done < "$1"
  [ "${#__tmps[@]}" -eq 0 ] && __bad=0
  if [ "$__bad" -ge 0 ]; then
    [ "${#__tmps[@]}" -eq 0 ] || rm -f "${__tmps[@]}"
    rm -f "$1" "$2"
    printf 'map-bulk-fail %d\n' "$__bad" >&2
    return 17
  fi
  __code=0; __bad=0; __i=0
  while [ "$__i" -lt "${#__tmps[@]}" ]; do
    __t=${__tmps[$__i]}; __d=${__dsts[$__i]}; __at=$__i; __i=$((__i+1))
    if [ "$__code" -ne 0 ]; then rm -f "$__t"; continue; fi
    if [ -d "$__d" ]; then rm -f "$__t"; __code=16; __bad=$__at; continue; fi
    __map_preserve_mode "$__d" "$__t"
    mv -f "$__t" "$__d" || { rm -f "$__t"; __code=1; __bad=$__at; continue; }
    if [ -d "$__d" ]; then rm -f "$__d/${__t##*/}"; __code=16; __bad=$__at; continue; fi
  done
  rm -f "$1" "$2"
  [ "$__code" -eq 0 ] || printf 'map-bulk-fail %d\n' "$__bad" >&2
  return "$__code"
}
`

// BulkRecoverShell defines __map_bulk_recover, the bulk path's answer to what
// the single write does when its own delivery is refused: make the directories,
// which is also what says whether a non-directory is what blocked the path
// (ExitPathNotDirectory), and shed what the failed attempt left behind. The
// caller then delivers once more. It is the same dance `WriteFile` already does
// with `mkdirAll`, over a whole batch and still for one exec.
//
// A manifest that is not there is a delivery that failed before landing
// anything, and there is nothing to recover: the caller's own error stands.
//
// It takes the manifest and the directory list with it, having read them, so a
// batch that ends here leaves the sandbox holding nothing of itself whichever way
// it ends. A retry can afford that: they are the archive's first two entries, so
// the next delivery brings them back.
//
// Both lists are handed to one `rm` and one `mkdir` rather than one per member,
// so the pass costs a process each however large the batch is. That is bounded
// by ARG_MAX rather than unbounded, and comfortably: the whole batch is capped
// at 10,000 members by what may be uploaded, whose paths run to a few hundred
// kilobytes against the couple of megabytes a command line holds. The walk that
// classifies a blocked path is all shell builtins, so it costs nothing at all.
const BulkRecoverShell = PathFaultShell + `
__map_bulk_recover() {
  [ -f "$1" ] || return 0
  __tmps=(); __dirs=()
  while IFS= read -r -d '' __t && IFS= read -r -d '' __d; do __tmps+=("$__t"); done < "$1"
  if [ -f "$2" ]; then
    while IFS= read -r -d '' __d; do __dirs+=("$__d"); done < "$2"
  fi
  [ "${#__tmps[@]}" -eq 0 ] || rm -f "${__tmps[@]}"
  rm -f "$1" "$2"
  [ "${#__dirs[@]}" -eq 0 ] && return 0
  umask 022
  mkdir -p "${__dirs[@]}" && return 0
  for __d in "${__dirs[@]}"; do __map_path_fault "$__d"; done
  return 1
}
`
