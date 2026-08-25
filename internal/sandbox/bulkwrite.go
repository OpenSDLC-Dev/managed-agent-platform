package sandbox

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
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
//
// Mode is the permission bits a file the batch creates lands with — zero means
// 0644, what every write landed before the field existed, and only permission
// bits are accepted: a batch carries no setuid, setgid or sticky bit. It rides
// both the tar header (what the docker daemon's host-side untar restores, the
// member being root-owned there) and the manifest (what the rename pass chmods
// on a backend whose untar ran as the sandbox user under its umask). An
// existing target's own mode still wins over it where the sandbox user may
// chmod, as it does for a single write (PreserveModeShell). Memory-store files
// are the one caller so far: 0666, so a non-root agent can append in place to
// a root-owned member (docs/plan/36_memory-stores.md decision 10).
type FileWrite struct {
	Path string
	Data []byte
	Mode fs.FileMode
}

// BulkWrite is one prepared batch: the archive a backend delivers, the two
// bookkeeping files the shared scripts read, and the classification of what the
// scripts report back. A backend builds one, delivers Archive, and runs the
// scripts; the batch is replayable, so a delivery that failed can be retried
// from the same value.
type BulkWrite struct {
	// Manifest names the file listing `tmp\0target\0mode\0` for every member,
	// and DirList the deduplicated parent directories. Both land in the workdir,
	// which exists, and both are the archive's first entries — so a delivery that
	// fails on a later one has still landed what the recovery pass needs. The
	// rename script removes them.
	Manifest string
	DirList  string

	files    []FileWrite
	tmps     []string
	modes    []fs.FileMode
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
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		if mode&^fs.ModePerm != 0 {
			return nil, fmt.Errorf("sandbox: bulk write mode %v for %q is not permission bits", f.Mode, f.Path)
		}
		dir := gopath.Dir(f.Path)
		tmp := gopath.Join(dir, nonce+"-"+strconv.Itoa(i))
		b.tmps = append(b.tmps, tmp)
		b.modes = append(b.modes, mode)
		manifest.WriteString(tmp)
		manifest.WriteByte(0)
		manifest.WriteString(f.Path)
		manifest.WriteByte(0)
		manifest.WriteString(fmt.Sprintf("%04o", mode))
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
		if err := b.entry(tw, b.tmps[i], f.Data, int64(b.modes[i])); err != nil {
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

// bulkLeftMarker prefixes the line __map_bulk_left prints for each of a batch's
// own files still on the filesystem after a shed pass ran. Its argument is a
// member's index in the manifest, or `m` / `d` for the two bookkeeping files —
// never a path, for the reason bulkFailMarker carries an index rather than one:
// the marker shares a stream with whatever the image writes to it, and a number
// that indexes what this side already has cannot be confused with an image's own
// line. It is also what keeps the report from steering anything: the shell reads
// its indices out of a manifest the sandbox can rewrite, but LeftBehind resolves
// them against the platform's own list, so a rewritten manifest can name nothing
// but the paths this batch chose.
const bulkLeftMarker = "map-bulk-left "

// bulkLeftBeginMarker opens the report, and is what makes the markers after it
// the *shed's* rather than the image's. An image controls this stream before the
// script ever runs — `bash -c` sources an `ENV BASH_ENV` file first, the channel
// #310 measured — so a forged `map-bulk-left 0` printed there would name a file
// the shed had just removed, and emptying it would put back, as a zero-byte
// file, exactly the litter this cleanup exists to remove. Only the lines after
// the LAST of these count, which is the bound blamed already takes for the fail
// marker and for the same reason: the image's turn at the stream comes first,
// and the shed prints this immediately before its own answer. A report with no
// opening line at all is not one — nothing is emptied, and what the sandbox user
// could not shed stays, this being best effort throughout.
//
// That last case is reachable, and by the image again rather than by the batch:
// a stream is capped and it is the *head* that is kept, so an image writing a
// megabyte to stdout during the pass loses the opening line and the markers with
// it. The batch's own report is ~190 KB at ten thousand members, nowhere near
// it. Losing the whole report is the right way to lose it — a truncated one
// would be markers with nothing to vouch for them.
//
// It is a distinct line rather than a prefix of bulkLeftMarker so neither can be
// read as the other: `map-bulk-left ` ends in a space and this does not.
//
// It is printed with a newline **in front of it**, and that is load-bearing
// rather than cosmetic. The stream arrives as frames concatenated with no
// separator, so an image whose output does not end in a newline would otherwise
// absorb this line into its own last partial one — measured: the marker stops
// being a line, the image's forged copy becomes the last valid one, and the
// framing hands the attacker exactly what it was written to take away. Leading
// with the newline terminates whatever came before, so this always lands as a
// line of its own however the image left the stream.
const bulkLeftBeginMarker = "map-bulk-left-begin"

// bulkLeftNoListMarker is the shed saying it had no list to walk, because the
// manifest it reads was not there — which the sandbox can arrange, the manifest
// being a file in its own workdir and the docker backend delivering it one round
// trip before the exec that reads it (#206 names that window). Without it the
// shed removes nothing and names nothing, and a report of "nothing is left" is
// exactly what a report of "I could not look" would otherwise be: every member's
// root-owned payload would stay, and deleting one file would be all it took to
// defeat the emptying this whole pass exists to do.
//
// It is not a list, so it cannot be acted on alone; LostItsList is how a backend
// asks, and only a backend that knows on its own that the members were delivered
// may answer it by emptying all of them — see docker's rename-fault branch, the
// one site where that is known.
const bulkLeftNoListMarker = "map-bulk-left-nolist"

// bulkLeftShell defines __map_bulk_left, which the rename and discard shells
// both embed and call after their own `rm -f`: it names, on stdout, every file
// of the batch's that is still there. Both sheds run as the sandbox user, and on
// a backend whose delivery did not — the docker daemon extracts on the host, as
// root — that user cannot unlink from a directory it cannot write, so its `rm`
// silently leaves the whole payload. What it could not take, the backend that
// landed it can (#316, the batch's half of #310); a backend whose delivery ran
// as the sandbox user has nothing to read here and does not look.
//
// The walk is two builtin tests per member and nothing else, so it costs no
// process even for the ten thousand members a skill may carry. `-f` alone would
// not say what it looks like it says — it stats *through* a link, so a temporary
// the sandbox swapped for a symlink to a regular file answers yes — so `! -h`
// goes with it and the pass reports nothing but the regular files the delivery
// itself landed. A directory or a link the sandbox put at one of these names is
// that sandbox's own object, in its own reach, and not this pass's to take away.
//
// What it names is a member's *temporary* and the two bookkeeping files, which
// is what the delivery landed and therefore all a backend can empty by index.
// One residue is outside that set: where a target turned into a directory under
// the `mv`, the member ends up at `$__d/${__t##*/}` and the run removes it there
// by path. If that `rm` is refused the file stays, unnamed — the sandbox user
// must have been able to `mv` into the directory but not unlink from it, which a
// sticky directory holding a root-owned entry can arrange.
//
// __tmps is the array the caller already built from the manifest; $1 and $2 are
// the manifest and directory list themselves.
const bulkLeftShell = `
__map_bulk_left() {
  printf '\n` + bulkLeftBeginMarker + `\n'
  [ "${#__tmps[@]}" -eq 0 ] && printf '` + bulkLeftNoListMarker + `\n'
  __i=0
  while [ "$__i" -lt "${#__tmps[@]}" ]; do
    __l=${__tmps[$__i]}
    if [ -f "$__l" ] && [ ! -h "$__l" ]; then printf '` + bulkLeftMarker + `%d\n' "$__i"; fi
    __i=$((__i+1))
  done
  if [ -f "$1" ] && [ ! -h "$1" ]; then printf '` + bulkLeftMarker + `m\n'; fi
  if [ -f "$2" ] && [ ! -h "$2" ]; then printf '` + bulkLeftMarker + `d\n'; fi
}
`

// LeftBehind resolves the markers a shed pass printed back to the paths of this
// batch's files that are still in the sandbox, so a backend whose delivery ran
// with credentials the shed did not have can take them back itself. An index
// that is not one of this batch's is dropped, as blamed drops one: the stream it
// came off is shared with the image.
//
// Only what follows the last opening line is read, and nothing at all when there
// is none — bulkLeftBeginMarker carries why. What survives that framing is still
// only *this batch's* own paths — a member's temporary, or one of the two
// bookkeeping files — never a target, so acting on the report can neither
// destroy what a failed write promised to leave alone nor reach outside what the
// batch itself put in the sandbox.
func (b *BulkWrite) LeftBehind(stdout string) []string {
	var left []string
	for _, line := range report(stdout) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, bulkLeftMarker) {
			continue
		}
		switch token := strings.TrimPrefix(line, bulkLeftMarker); token {
		case "m":
			left = append(left, b.Manifest)
		case "d":
			left = append(left, b.DirList)
		default:
			n, err := strconv.Atoi(token)
			if err != nil || n < 0 || n >= len(b.tmps) {
				continue
			}
			left = append(left, b.tmps[n])
		}
	}
	return left
}

// report is the shed's own lines: everything after the LAST opening marker, and
// nothing at all when there is none. bulkLeftBeginMarker carries why the framing
// is where the trust lives.
func report(stdout string) []string {
	lines := strings.Split(stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == bulkLeftBeginMarker {
			return lines[i+1:]
		}
	}
	return nil
}

// LostItsList answers whether the shed could not read the manifest, and so
// removed nothing and can name nothing — the sandbox having deleted it in the
// window between the delivery and the exec that reads it. A caller that knows
// the members were delivered answers this by emptying Delivered() instead, since
// nothing was removed there is nothing to recreate. A caller that does not know
// must not: it would put zero-byte files at the names of members that never
// arrived.
func (b *BulkWrite) LostItsList(stdout string) bool {
	for _, line := range report(stdout) {
		if strings.TrimSpace(line) == bulkLeftNoListMarker {
			return true
		}
	}
	return false
}

// Delivered names every member's temporary — the platform's own list, held here
// all along, and the answer to a manifest the sandbox deleted.
//
// The two bookkeeping files are deliberately NOT in it. A pass that lost its
// list still removes them itself, and still looks at them afterwards, so it
// speaks accurately about those two whatever became of the manifest: LeftBehind's
// own answer is the one to trust there. Putting them in this list instead would
// empty them on a branch that had *just removed them*, recreating as zero-byte
// files exactly what the shed had taken away — the harm the naming exists to
// avoid, reintroduced by the fallback meant to protect it.
func (b *BulkWrite) Delivered() []string {
	return append([]string(nil), b.tmps...)
}

// EmptyArchive streams a tar carrying a zero-byte entry for each of paths — the
// batch's form of the single write's own emptying (docker's `reclaim`), and one
// archive for the whole batch rather than one per member, because a batch that
// failed under a root-owned parent left one file per member and ten thousand
// round trips is not a cleanup. Extracting it puts the name back without the
// payload.
//
// What it must not do is put a name back that the shed had just taken away, and
// nothing here can check: it writes what the caller passes. A caller passing
// LeftBehind's answer is asking about files a `[ -f ]` found after the `rm`, so
// an honest report recreates nothing — and a forged one, framed out by
// bulkLeftBeginMarker, would at worst leave a zero-byte file at one of this
// batch's own temporary names, never at a target and never outside the batch.
func (b *BulkWrite) EmptyArchive(paths []string, w io.Writer) error {
	tw := tar.NewWriter(w)
	for _, path := range paths {
		if err := b.entry(tw, path, nil, 0o644); err != nil {
			return err
		}
	}
	return tw.Close()
}

// BulkRenameShell defines __map_bulk_rename, which both backends embed: given
// the manifest ($1) and the directory list ($2) an already-delivered archive
// landed, it renames every member into place and takes both files away with it.
// It is the bulk half of what the single write's own rename does, member by
// member and for one exec instead of N.
//
// Each member is the single write's sequence exactly — set a created file's mode
// to 0644 (or the mode its manifest record asks for, one chmod per member that
// does), refuse a target that is a directory, carry an existing target's mode
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
// than left in the sandbox. The `rm` runs as the sandbox user, which on the
// docker backend is not who landed them, so the pass says what is still there
// (__map_bulk_left) for the backend to take back itself (#316) — on the way out
// of a *successful* run too, because the two bookkeeping files are removed by
// that same `rm` and a workdir the sandbox user cannot write keeps them however
// well the members landed.
//
// The manifest is a file on the agent's own filesystem, so a command running in
// the sandbox can rewrite it between the delivery and this pass and steer where
// a member lands — as it can already swap the temporary file a single write is
// about to rename. It is the same bound in both cases and it is not a new one:
// the move runs with the agent's own credentials, so a redirected member reaches
// nothing the agent could not have written with its own `mv`. The mode field is
// read the same way — four octal digits or it is 0644 — and a rewritten one
// chmods a file the agent could chmod itself.
const BulkRenameShell = PreserveModeShell + bulkLeftShell + `
__map_bulk_rename() {
  __tmps=(); __dsts=(); __modes=(); __bad=-1
  while IFS= read -r -d '' __t && IFS= read -r -d '' __d && IFS= read -r -d '' __m; do
    if [ ! -f "$__t" ] && [ "$__bad" -lt 0 ]; then __bad=${#__tmps[@]}; fi
    case "$__m" in [0-7][0-7][0-7][0-7]) ;; *) __m=0644 ;; esac
    __tmps+=("$__t"); __dsts+=("$__d"); __modes+=("$__m")
  done < "$1"
  [ "${#__tmps[@]}" -eq 0 ] && __bad=0
  if [ "$__bad" -ge 0 ]; then
    [ "${#__tmps[@]}" -eq 0 ] || rm -f "${__tmps[@]}"
    rm -f "$1" "$2"
    __map_bulk_left "$1" "$2"
    printf 'map-bulk-fail %d\n' "$__bad" >&2
    return 17
  fi
  __i=0
  while [ "$__i" -lt "${#__tmps[@]}" ]; do
    chmod 0644 "${__tmps[@]:$__i:100}" 2>/dev/null
    __i=$((__i+100))
  done
  __i=0
  while [ "$__i" -lt "${#__tmps[@]}" ]; do
    [ "${__modes[$__i]}" = 0644 ] || chmod "${__modes[$__i]}" "${__tmps[$__i]}" 2>/dev/null
    __i=$((__i+1))
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
  __map_bulk_left "$1" "$2"
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
// the sandbox holding nothing **it can shed**. That is the whole claim on the
// backend whose delivery ran as the sandbox user; where it ran as root — the
// docker daemon extracts on the host — the `rm` here reaches nothing under a
// parent that user cannot write, and what it could not take is named on stdout
// for the backend to empty instead (__map_bulk_left, #316). A manifest that is
// not there took the list of what to shed with it; the rest still goes, and the
// naming pass says so rather than reporting an empty sandbox.
//
// The exit status is the bookkeeping `rm`'s, kept across the naming pass so it
// still says whether the shed could do its job. Neither backend reads it today;
// both would be reading a constant if the report were allowed to overwrite it.
const BulkDiscardShell = bulkLeftShell + `
__map_bulk_discard() {
  __tmps=()
  if [ -f "$1" ]; then
    while IFS= read -r -d '' __t && IFS= read -r -d '' __d && IFS= read -r -d '' __m; do __tmps+=("$__t"); done < "$1"
    [ "${#__tmps[@]}" -eq 0 ] || rm -f "${__tmps[@]}"
  fi
  rm -f "$1" "$2"
  __rc=$?
  __map_bulk_left "$1" "$2"
  return "$__rc"
}
`
