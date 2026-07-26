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
// write's. Both backends land a batch the same way — an archive carrying a
// manifest and one temporary file per member, then the exec that renames every
// member into place — so the archive, the manifest format and the shells live
// here once.
//
// What differs is who extracts the archive, and that decides who owns what it
// creates. The k8s backend pipes it through `tar` inside the pod, so the members
// and their parents are the sandbox user's, and one exec does the whole batch.
// The docker daemon extracts on the host as root, so parents it created would
// belong to root and a non-root sandbox could rename nothing into them: that
// backend delivers the bookkeeping, runs BulkPrepareShell to make the
// directories inside the sandbox, then delivers the members — two execs,
// still a fixed cost rather than one per member.

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
	// stamp is taken once so the archive is byte-identical every time it is
	// built: a delivery that failed is retried from this same value, and a tar
	// header carrying a fresh clock would make the retry a different archive.
	stamp time.Time
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
	b.stamp = time.Now()
	return b, nil
}

// Archive streams the whole batch as one tar: the bookkeeping, then one entry per
// member under its temporary name. It is what a backend whose sandbox extracts for
// itself delivers, in one stream.
//
// Entry names are relative, so both untars extract it at `/`. It carries no
// directory entries, and that is deliberate: an explicit directory entry chmods a
// directory that already exists (measured, 0700 → 0755 under both untars), and a
// write must not change the mode of a directory it merely passes through.
//
// The parents a member needs are made by Bookkeeping + the prepare pass rather
// than left to the untar, because *who* makes them decides whether the write can
// finish. An untar running on the host makes them root's, and a sandbox whose
// image runs as anyone else then cannot rename anything into them — measured: on
// a non-root image the whole batch fails where a file-at-a-time loop succeeds.
// Made inside the sandbox they belong to the sandbox user, exactly as the single
// write's own `mkdir -p` makes them.
func (b *BulkWrite) Archive(w io.Writer) error {
	tw := tar.NewWriter(w)
	if err := b.bookkeeping(tw); err != nil {
		return err
	}
	if err := b.members(tw); err != nil {
		return err
	}
	return tw.Close()
}

// Bookkeeping streams a tar carrying only the manifest and the directory list —
// the first of the two deliveries a backend makes when the sandbox cannot extract
// for itself. Both land in the workdir, which exists, so this delivery needs no
// directory made for it; what it carries is the list of the ones that must be.
func (b *BulkWrite) Bookkeeping(w io.Writer) error {
	tw := tar.NewWriter(w)
	if err := b.bookkeeping(tw); err != nil {
		return err
	}
	return tw.Close()
}

// Members streams a tar carrying only the members, under their temporary names.
// It is delivered after the prepare pass has made the directories they land in.
func (b *BulkWrite) Members(w io.Writer) error {
	tw := tar.NewWriter(w)
	if err := b.members(tw); err != nil {
		return err
	}
	return tw.Close()
}

// The bookkeeping is world-readable, unlike a temporary file whose bytes are the
// caller's: a host-side untar lands it owned by root, and the sandbox user has to
// be able to read it to act on it. It names paths the sandbox can already list.
func (b *BulkWrite) bookkeeping(tw *tar.Writer) error {
	if err := b.entry(tw, b.Manifest, b.manifest, 0o644); err != nil {
		return err
	}
	return b.entry(tw, b.DirList, b.dirs, 0o644)
}

func (b *BulkWrite) members(tw *tar.Writer) error {
	for i, f := range b.files {
		if err := b.entry(tw, b.tmps[i], f.Data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (b *BulkWrite) entry(tw *tar.Writer, path string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(path, "/"),
		Mode:     mode,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		ModTime:  b.stamp,
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
// Each member is the single write's sequence exactly — set a created file's mode
// to 0644, refuse a target that is a directory, carry an existing target's mode
// onto the temporary file, move, then ask again in case something made the target
// a directory in between — so a batch and a loop of writes land a file
// identically. Only the batch's *shape* is new: the first failure stops the run,
// and the members that already landed stay landed, because that is what the loop
// this replaces did.
//
// The `chmod 0644` is the batch's half of #213, and it is the delivery it answers
// for rather than the rename: where the target's parent directory carries a
// **default POSIX ACL**, the kernel takes a created file's bits from the ACL and
// ignores the umask the delivery set, and a **non-root** `tar` does not chmod over
// it — measured, GNU tar 1.35 and BusyBox 1.36 alike, a member extracted under
// `setfacl -d -m u::rwx,g::---,o::---` landing 0600 where the docker daemon's
// host-side untar, which chmods, lands 0644 for the same batch. (A root `tar`
// restores the header's mode by itself, which is why the divergence is a non-root
// image's; `-p` does not close it, because tar then assumes the `open` it asked
// for got the mode and skips the chmod.) An existing target's own bits still win:
// __map_preserve_mode chmods over this, per member, just as it does in the single
// write.
//
// It runs before the loop rather than inside it, so the pass costs a process per
// hundred members instead of one per member — 100 for the 10,000 a skill upload
// may carry, against a batch that already takes seconds. A hundred rather than the
// whole list because this one failure would be a **silent** one: `chmod` reports
// E2BIG like anything else, and its status is deliberately ignored here, so a
// chunk that overflowed the command line would leave its members on the ACL's bits
// with the batch still reporting success. A hundred paths cannot overflow it —
// 100 × PATH_MAX is 400 KB against the 2 MB a Linux command line holds, so the
// bound holds for pathological paths and not only for the ones a skill really
// carries. (The prepare pass hands its whole directory list to one `mkdir` on a
// looser argument, that a real batch's directories run to a few hundred kilobytes;
// it can afford to, because an over-long `mkdir` fails the batch out loud.)
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
  __i=0
  while [ "$__i" -lt "${#__tmps[@]}" ]; do
    chmod 0644 "${__tmps[@]:$__i:100}" 2>/dev/null
    __i=$((__i+100))
  done
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

// BulkPrepareShell defines __map_bulk_prepare, which both backends embed: given
// the directory list ($1) an already-delivered bookkeeping pass landed, it makes
// every directory the batch's members need, and says whether a non-directory is
// what blocked the path (ExitPathNotDirectory) rather than letting that read as
// the sandbox breaking.
//
// It runs INSIDE the sandbox, and that is the whole point of it rather than an
// implementation detail. A host-side untar makes a missing parent root's, and a
// sandbox whose image runs as anyone else cannot then rename a member into it —
// measured on a non-root image, the batch fails where a file-at-a-time loop
// succeeds, because the loop's own `mkdir -p` ran in here too. Made here, the
// directories belong to whoever the sandbox runs as, exactly as they did before.
//
// `umask 022` so the directories a batch creates land 0755 whatever the image's
// umask is, matching what a host-side untar gives on the other backend; the
// argument for fixing that answer rather than following the image is on Archive.
//
// The whole list is handed to one `mkdir` rather than one per directory, so the
// pass costs a process however large the batch is — bounded by ARG_MAX, and
// comfortably: a batch is capped at 10,000 members by what may be uploaded, and
// their distinct directories run to a few hundred kilobytes against the couple of
// megabytes a command line holds. The walk that classifies a blocked path is all
// shell builtins, so it costs nothing at all.
const BulkPrepareShell = PathFaultShell + `
__map_bulk_prepare() {
  [ -f "$1" ] || return 0
  __dirs=()
  while IFS= read -r -d '' __d; do __dirs+=("$__d"); done < "$1"
  [ "${#__dirs[@]}" -eq 0 ] && return 0
  umask 022
  mkdir -p "${__dirs[@]}" && return 0
  for __d in "${__dirs[@]}"; do __map_path_fault "$__d"; done
  return 1
}
`

// BulkDiscardShell defines __map_bulk_discard, which both backends embed: given
// the manifest ($1) and the directory list ($2), it sheds every member the batch
// delivered and then the two files themselves, so a batch that ends badly leaves
// the sandbox holding nothing of itself. A manifest that is not there took the
// list of what to shed with it; the rest still goes.
const BulkDiscardShell = `
__map_bulk_discard() {
  if [ -f "$1" ]; then
    __tmps=()
    while IFS= read -r -d '' __t && IFS= read -r -d '' __d; do __tmps+=("$__t"); done < "$1"
    [ "${#__tmps[@]}" -eq 0 ] || rm -f "${__tmps[@]}"
  fi
  rm -f "$1" "$2"
}
`
