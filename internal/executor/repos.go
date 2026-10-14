package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// repoRef is the minimal shape of one session resources[] entry of the
// github_repository variant — the token-free wire object slice 1 stores
// (sessionresources.go's repoResourceJSON). The secret itself never appears
// here: it is sealed beside the row in session_resource_credentials and
// decrypted per clone (plan 25 decision 2).
type repoRef struct {
	ID        string        `json:"id"`
	MountPath string        `json:"mount_path"`
	Type      string        `json:"type"`
	URL       string        `json:"url"`
	Checkout  *repoCheckout `json:"checkout"`
}

// repoCheckout is the stored checkout union: {type:"branch",name} |
// {type:"commit",sha}, or absent for the repository's default branch.
type repoCheckout struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	Sha  string `json:"sha,omitempty"`
}

// repoCloneErrorType is the session.error variant a failed clone surfaces,
// the credential_host_unreachable_error twin (plan 25 decision 4).
const repoCloneErrorType = "github_repository_clone_error"

// The messages a github_repository_clone_error carries, a small fixed set:
// emitRepoCloneError dedupes on them.
const (
	repoCloneFailedMessage   = "the repository could not be cloned into the sandbox"
	repoNoCipherMessage      = "a secrets cipher is not configured on this executor"
	repoMountOccupiedMessage = "the repository's mount path lies outside the platform's directories, where a clone takes only an absent path or an empty directory it can remove"
)

// errRepoNoCredential classifies a repo resource whose sealed token row is
// gone. It cannot happen through the API (the row is written in the create
// transaction and dies only with the session), so it is an internal fault
// rather than a user-visible reason of its own.
var errRepoNoCredential = errors.New("repository credential row missing")

// materializeRepos clones each mounted repository into the sandbox before the
// tools run, first of the passes (provisionAndRun says why). Idempotence is
// the probe alone — a repo
// materializes iff the probe answers that `<mount>/.git` is absent, and a pass
// whose probe does not answer clones nothing (#860, repoPresence); outside the
// platform's directories the mount must also be vacant (replacesWhole) — deliberately without a sentinel:
// the mounted set is fixed at create, so a marker would have no drift to
// detect, and plan 24 strips agent-writable markers from checkpoints at
// capture, which would make a restored workspace re-clone over the agent's own
// work. The tree is the only authority there is.
//
// Per-repository failure is tolerated exactly like a file's: the run continues
// and the agent sees an absent path. Unlike a file's, it also surfaces as a
// session.error the client can see (decision 4). refs come from the same
// locked session read that gated the run (sessionForRun). It returns the
// repositories whose checkout this pass found or landed, in resource order.
func (e *Executor) materializeRepos(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []repoRef, skilled bool, progress func()) (present []repoRef) {
	mounts := make([]repoRef, 0, len(refs))
	for _, r := range refs {
		if r.Type == "github_repository" && r.ID != "" && r.URL != "" && r.MountPath != "" {
			mounts = append(mounts, r)
		}
	}
	if len(mounts) == 0 {
		return nil
	}

	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "repos_materialize")
	defer span.End()
	start := time.Now()
	defer func() { recordReposMaterializeDuration(ctx, time.Since(start)) }()
	span.SetAttributes(attribute.Int("repos.referenced", len(mounts)))

	if e.cipher == nil {
		// The control plane refuses a repo-bearing create without a cipher, so
		// this is a deployment whose executor and control plane disagree. Said
		// once for the session rather than once per repository below.
		slog.WarnContext(ctx, "session references github_repository resources but no secrets cipher is configured",
			"session_id", sid, "repos", len(mounts))
	}

	var landed, unchanged, unprobed, failed int
	for _, m := range mounts {
		// Per repository, at the top of the iteration (the files.go rule, and
		// for a sharper reason): a session may mount eight, each allowed
		// RepoCloneTimeout, so a per-pass report would let a wholly healthy pass
		// sit silent for far longer than any stall budget (#383).
		progress()
		switch repoPresence(ctx, sb, m) {
		case sandbox.Present:
			unchanged++
			present = append(present, m)
			recordRepoMaterialized(ctx, repoOutcomeUnchanged)
			slog.InfoContext(ctx, "repository already present, skipping clone",
				"session_id", sid, "resource_id", m.ID, "url", m.URL, "mount_path", m.MountPath)
			continue
		case sandbox.PresenceUnknown:
			// The sandbox answered neither probe, twice (repoPresence). That
			// is no sign the checkout is gone, and a clone in the platform's
			// directories replaces the mount whole (extractRepo's rm -rf), the
			// agent's work in it with it (#860); nor that it is there, so no
			// clone error either — the next pass asks again.
			unprobed++
			recordRepoMaterialized(ctx, repoOutcomeUnprobed)
			slog.WarnContext(ctx, "repository presence not answered; clone skipped this pass",
				"session_id", sid, "resource_id", m.ID, "url", m.URL, "mount_path", m.MountPath)
			continue
		}
		// Judged again here, having already been judged at create. The
		// duplication is deliberate and narrow: what follows can build an
		// `rm -rf` on this path inside the session's sandbox, and a row that reached the
		// database by any route other than the create endpoint would otherwise
		// have that command built for it. `/workspace/repo/..` cleans to
		// `/workspace`; `/tmp` is the platform's own staging root.
		if err := e.safeRepoMount(m.MountPath); err != nil {
			failed++
			recordRepoMaterialized(ctx, repoOutcomeInternal)
			slog.WarnContext(ctx, "repository mount path refused by the executor",
				"session_id", sid, "resource_id", m.ID, "mount_path", m.MountPath, "err", err)
			e.emitRepoCloneError(ctx, sid, m, repoOutcomeInternal, repoCloneFailedMessage)
			continue
		}
		// Asked after the probe, not before it: a mount that already carries a
		// checkout — restored from a checkpoint, or landed by a correctly
		// configured executor before the drift — needs no token, and reporting
		// it as failed would tell the client a checkout is missing while the
		// agent is reading it.
		if e.cipher == nil {
			failed++
			recordRepoMaterialized(ctx, repoOutcomeInternal)
			e.emitRepoCloneError(ctx, sid, m, repoOutcomeInternal, repoNoCipherMessage)
			continue
		}
		// The probe and the two guards above are behind us; what follows is the
		// clone, whose own deadline is the floor's anchor. Reported here so that
		// deadline is the whole of this interval rather than its tail — a slow
		// presence exec ahead of a full-length clone would otherwise overrun a
		// budget floored on the clone alone (#383).
		progress()
		// A run with no skills has no skills sentinel to drop.
		var sentinel string
		if skilled {
			sentinel = e.skillsSentinel(m.MountPath)
		}
		bytes, err := e.materializeRepo(ctx, sb, sid, m, sentinel, progress)
		if err != nil {
			failed++
			reason := cloneReason(err)
			recordRepoMaterialized(ctx, reason)
			slog.WarnContext(ctx, "repository not materialized",
				"session_id", sid, "resource_id", m.ID, "url", m.URL, "mount_path", m.MountPath,
				"reason", reason, "err", err)
			message := repoCloneFailedMessage
			if errors.Is(err, errMountOccupied) {
				message = repoMountOccupiedMessage
			}
			e.emitRepoCloneError(ctx, sid, m, reason, message)
			continue
		}
		landed++
		present = append(present, m)
		recordRepoMaterialized(ctx, repoOutcomeOK)
		recordRepoMaterializeBytes(ctx, bytes)
		slog.InfoContext(ctx, "repository materialized",
			"session_id", sid, "resource_id", m.ID, "url", m.URL, "mount_path", m.MountPath, "bytes", bytes)
	}
	span.SetAttributes(
		attribute.Int("repos.materialized", landed),
		attribute.Int("repos.unchanged", unchanged),
		attribute.Int("repos.unprobed", unprobed),
		attribute.Int("repos.failed", failed),
	)
	return present
}

// materializeRepo clones one repository and lands it at its mount path,
// returning the shipped tar's size. The token is decrypted here, held only for
// the clone's own HTTP requests, and never written anywhere: the executor
// sends it as an Authorization header, so the tar that reaches the sandbox
// carries a clean .git/config and the sandbox never sees the secret in any
// form (plan 25 decision 1). sentinel, when set, is the skills sentinel the
// extraction drops (extractRepo).
func (e *Executor) materializeRepo(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, m repoRef, sentinel string, progress func()) (int64, error) {
	// A mount the clone may not replace whole is probed before the fetch, so
	// a path that visibly holds something costs one exec a pass rather than a
	// clone. One the probe gives no answer for, or cannot list, is fetched,
	// and the extraction judges it.
	whole := e.replacesWhole(m.MountPath)
	if !whole {
		if mountOccupied(ctx, sb, m.MountPath) {
			return 0, errMountOccupied
		}
		progress()
	}
	token, err := e.repoToken(ctx, sid, m.ID)
	if err != nil {
		return 0, err
	}
	// The row read and the cipher's decrypt are their own round trips — a KMS
	// among them — and the clone below is what the stall floor is measured
	// against, so the two do not share an interval (#383).
	progress()
	ctx, cancel := context.WithTimeout(ctx, e.cfg.RepoCloneTimeout)
	defer cancel()

	tarPath, size, cleanup, err := cloneToTar(ctx, m, token, e.cfg.RepoCloneMaxBytes)
	defer cleanup()
	if err != nil {
		return 0, err
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// The tar lands under the platform's staging path, which no repository is
	// mounted over: create roots every mount under /workspace (#922), and
	// safeRepoMount refuses it for a row from any other route.
	sandboxTar := path.Join(repoStagingRoot, "repo-"+m.ID+".tar")
	// From the upload on, the sandbox can hold up to the whole byte budget in
	// /tmp, and the only thing that removes it is the extraction command's own
	// cleanup tail, which runs only if the extraction script does. An upload
	// that breaks off, an extraction whose exec never starts, or a context
	// cancelled between the two — the clone deadline, or a lost lease — would
	// strand a file the size of the repository inside the session's sandbox
	// for its whole life. So every failure from here is swept, on a context the
	// cancellation cannot reach. A script that ran and failed has already
	// cleaned up, and sweeping again costs one exec on a pass that failed.
	landed := false
	defer func() {
		if landed {
			return
		}
		sweep, stop := context.WithTimeout(context.WithoutCancel(ctx), repoSweepTimeout)
		defer stop()
		if _, err := sandbox.ExecScript(sweep, sb, sandbox.ExecRequest{
			Command: "rm -rf " + shellQuote(sandboxTar) + " " + shellQuote(stagingPath(m.MountPath)),
		}); err != nil {
			slog.WarnContext(ctx, "the failed clone's sandbox staging was not swept",
				"session_id", sid, "resource_id", m.ID, "path", sandboxTar, "err", err)
		}
	}()
	if err := sb.WriteFileStream(ctx, sandboxTar, f, size); err != nil {
		return 0, fmt.Errorf("%w: %w", errExtraction, err)
	}
	if err := extractRepo(ctx, sb, m.MountPath, sandboxTar, sentinel, whole); err != nil {
		return 0, err
	}
	landed = true
	return size, nil
}

// repoSweepTimeout bounds the post-cancellation sweep. Short on purpose: the
// work item is already over, and a sandbox that will not answer a `rm` will not
// answer a longer one either.
const repoSweepTimeout = 15 * time.Second

// repoStagingRoot is where a clone's tar lands inside the sandbox on its way
// to the mount. A repository never mounts over it: create roots every mount
// under /workspace (#922), and safeRepoMount refuses it for a row from any
// other route.
const repoStagingRoot = "/tmp"

// safeRepoMount is the executor's own judgment of a mount path, independent of
// the API's. It admits only an absolute path that is already in clean form and
// names something strictly inside a parent — which is what makes `rm -rf` on it
// bounded — and refuses what this executor knows is already spoken for.
//
// It is deliberately not a second copy of the API's full grammar (nesting,
// per-session caps, the URL-derived default). Those decide what a caller may
// ask for; this decides only where a clone may go, and replacesWhole what it
// may destroy there. Three of the refusals are
// things the control plane cannot know, which is why they live here:
//
//   - the workdir or a directory holding it. Create never yields one under
//     the default workdir (#922), but the workdir is configurable.
//   - a materialization sentinel, `<workdir>/skills/.materialized` or
//     `<workdir>/.files_materialized`, or a path below one: the skills and
//     files steps write them as files after the repositories land.
//   - another repository's staging path, or a path below one.
//     `/workspace/.x.map-repo-staging` is a mount path the API accepts, and
//     cloning `/workspace/x` removes it, and all below it, twice.
//
// The workdir is executor configuration, and the staging name is an executor
// implementation detail; neither is visible where the resource is validated.
//
// It also refuses a directory the platform keeps beside the workdir, or one
// holding it: the repository root, the uploads and outputs directories,
// the memory stores' root and the shell state root. Create never yields
// one since #922, but a repository stored before keeps its literal path,
// and `/mnt` was one create accepted. Below one, a re-clone removes only
// the checkout's own tree. What the image ships at a path is replacesWhole's
// to keep (#930).
func (e *Executor) safeRepoMount(mount string) error {
	if mount == "" || !path.IsAbs(mount) {
		return fmt.Errorf("mount path %q is not absolute", mount)
	}
	if path.Clean(mount) != mount {
		return fmt.Errorf("mount path %q is not in clean form", mount)
	}
	if mount == "/" || mount == repoStagingRoot {
		return fmt.Errorf("mount path %q is reserved", mount)
	}
	if path.Dir(mount) == mount {
		return fmt.Errorf("mount path %q has no parent to stage beside", mount)
	}
	workdir := e.cfg.Workdir
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	if wd := path.Clean(workdir); mount == wd || strings.HasPrefix(wd, mount+"/") {
		return fmt.Errorf("mount path %q is the sandbox workdir or holds it", mount)
	}
	for _, s := range []string{e.skillsSentinelPath(), path.Join(workdir, filesSentinelName)} {
		if mount == s || strings.HasPrefix(mount, s+"/") {
			return fmt.Errorf("mount path %q is a materialization sentinel or below one", mount)
		}
	}
	for _, c := range strings.Split(mount, "/") {
		if strings.HasPrefix(c, ".") && strings.HasSuffix(c, stagingSuffix) {
			return fmt.Errorf("mount path %q is a repository staging path or below one", mount)
		}
	}
	for _, d := range []string{sandbox.RepositoryRoot, "/mnt/session/uploads", outputsDir, toolset.MemoryMountRoot, sandbox.ShellStateRoot} {
		if mount == d || strings.HasPrefix(d, mount+"/") {
			return fmt.Errorf("mount path %q is a platform directory or holds one", mount)
		}
	}
	return nil
}

// skillsSentinel is the skills sentinel a clone at mount must drop, or "" for
// a mount outside the skills directory. A repository may share that directory
// with the run's skills (#922), and a clone at or below it replaces skill
// files the sentinel still vouches for: its probe reads only each skill's
// SKILL.md, which the checkout may bring itself. Without the drop, the skills
// step after the repositories would trust them.
func (e *Executor) skillsSentinel(mount string) string {
	sentinel := e.skillsSentinelPath()
	if root := path.Dir(sentinel); mount != root && !strings.HasPrefix(mount, root+"/") {
		return ""
	}
	return sentinel
}

// stagingPath is where a mount's tree is unpacked before being renamed into
// place — a sibling of the mount, so the rename stays on one filesystem. Named
// once because the cancellation sweep has to remove the same directory the
// extraction would have.
func stagingPath(mount string) string {
	clean := path.Clean(mount)
	return path.Join(path.Dir(clean), "."+path.Base(clean)+stagingSuffix)
}

// stagingSuffix marks a staging directory. It is also what safeRepoMount
// refuses as a mount path, so no repository can be mounted where another one's
// extraction will remove it.
const stagingSuffix = ".map-repo-staging"

// replacesWhole reports whether a clone may replace what is at mount whole.
// It may below a directory the platform keeps writable (sandbox.WritablePaths:
// the repository root, the workdir, /tmp, the shell state root and /mnt),
// the directories the platform writes and the agent works in. Anywhere else is
// the image's own filesystem, which only a repository stored before #922 can
// name (#930). There a clone takes only an absent path or an empty directory,
// so a row at /usr cannot remove what the image ships. A row at a path the
// image leaves absent, such as /opt/repo, clones as before; one whose
// checkout lost its .git there now fails instead of being replaced.
func (e *Executor) replacesWhole(mount string) bool {
	for _, root := range sandbox.WritablePaths(e.cfg.Workdir) {
		if strings.HasPrefix(mount, root+"/") {
			return true
		}
	}
	return false
}

// vacantScript asks, and changes nothing, whether a clone may take mount:
// whether it is absent, or an empty directory that is not a symlink. It
// prints vacant or occupied, and mountOccupied reads the answer framed, so a
// shell that an image's startup ends first gives no answer, whatever its exit
// status. A listing that fails, as on a directory the sandbox user may not
// read, prints nothing and so reads as empty: the probe answers vacant, and
// the rmdir in vacateScript decides. It names no variable, because an
// image's startup can leave attributes on any name it might take.
func vacantScript(mount string) string {
	q := shellQuote(mount)
	return "if { [ ! -e " + q + " ] && [ ! -L " + q + " ]; } || { [ -d " + q + " ] && [ ! -L " + q +
		" ] && [ -z \"$(ls -A " + q + " 2>/dev/null)\" ]; }; then echo vacant; else echo occupied; fi"
}

// vacateScript clears a mount that replacesWhole refuses, in place of the
// `rm -rf` right before the rename. It leaves an absent path alone, removes
// an empty directory, and refuses whatever rmdir refuses: a directory with
// content, a file, a symlink, dangling or not, and an empty directory the
// sandbox user cannot remove. A refusal ends the extraction with
// occupiedStatus. It can refuse a path vacantScript let through, such as one
// the probe could not list; the fetch is then spent, and nothing is lost.
func vacateScript(mount string) string {
	q := shellQuote(mount)
	return "{ { [ ! -e " + q + " ] && [ ! -L " + q + " ]; } || rmdir " + q + " || (exit " +
		strconv.Itoa(occupiedStatus) + "); }"
}

// occupiedStatus is the extraction's exit status when vacateScript refuses
// the mount, so the refusal is reported as errMountOccupied and not as a
// failed extraction. No platform command in the script exits with it; an
// image's startup that ends the shell first, or a command the image wraps,
// could, and would read as an occupied path.
const occupiedStatus = 75

// errMountOccupied is a clone refused because its mount path is outside the
// platform's directories and is neither absent nor an empty directory the
// clone can remove: by mountOccupied before the fetch, or by vacateScript at
// the extraction.
var errMountOccupied = errors.New("the mount path is neither absent nor an empty directory the clone can remove")

// errExtraction is a failure after the fetch, in the sandbox: shipping the
// tar there, or the extraction script. It is the sandbox's, never the git
// host's, though its text can read like the host's: a dropped exec stream says
// EOF, and a script's stderr quotes the mount path, such as /opt/EOF. It never
// carries errFetch, so cloneReason does not read that text.
var errExtraction = errors.New("landing the repository in the sandbox failed")

// repoVacancyTimeout bounds mountOccupied's probe, which runs before the
// clone's own deadline is set.
const repoVacancyTimeout = 30 * time.Second

// mountOccupied asks vacantScript before the fetch, framed
// (sandbox.ExecFramed), and only the answer occupied refuses. Anything else
// reads as vacant: an exec that failed or timed out, or an answer that never
// reached the output because an image's startup ended the shell first or
// printed past the cap (#860). vacateScript judges the path again in the
// extraction, and that judgment is the one that holds.
func mountOccupied(ctx context.Context, sb sandbox.Sandbox, mount string) bool {
	res, framed, err := sandbox.ExecFramed(ctx, sb, "repovacancy",
		sandbox.ExecRequest{Command: vacantScript(mount), Timeout: repoVacancyTimeout})
	return err == nil && framed && !res.TimedOut && strings.TrimSpace(res.Stdout) == "occupied"
}

// extractRepo unpacks the shipped tar into a staging directory beside the
// mount and renames it into place, so `<mount>` — and therefore the
// `<mount>/.git` the idempotence probe reads — exists only once the tree is
// complete: a mid-extraction failure (disk full, unwritable member, a kill)
// can never leave a partial tree for a later pass to trust. Staging is a
// sibling of the mount so the rename stays within one filesystem.
//
// sentinel, when set, is the skills sentinel this mount shares a directory
// with (skillsSentinel). It goes in the same script, once the new tree is
// staged and before the old one is removed: a clone that failed before the
// script leaves it, and the skills, alone; an executor that dies after it
// leaves the skills to be written again; and a removal that fails stops the
// script with nothing lost. A checkout at the skills directory itself has
// its own copy of the name removed from the staged tree, so the skills step
// never reads the checkout's file as its marker.
//
// whole is replacesWhole's verdict on the mount. Without it, vacateScript
// takes the place of the `rm -rf`. It runs here, right before the rename,
// rather than only in mountOccupied's look before the fetch, because each
// script can run an image's startup hook first (#860). A hook that makes the
// directory again would otherwise have `mv` put the checkout inside it. A
// refusal there ends the script with occupiedStatus.
//
// Every path is shell-quoted: mount_path is request-controlled text, and
// unquoted interpolation would be command injection inside the sandbox.
// Cleanup runs unconditionally — after a `;`, never sequenced behind an `&&` —
// so a failed extraction still removes the tar and any partial staging.
func extractRepo(ctx context.Context, sb sandbox.Sandbox, mount, tarPath, sentinel string, whole bool) error {
	clean := path.Clean(mount)
	staging := stagingPath(mount)
	q := shellQuote
	var drop string
	if sentinel != "" {
		drop = " && rm -rf " + q(sentinel)
		if path.Dir(sentinel) == clean {
			drop += " " + q(path.Join(staging, path.Base(sentinel)))
		}
	}
	remove := " && rm -rf " + q(clean)
	if !whole {
		remove = " && " + vacateScript(clean)
	}
	cmd := strings.Join([]string{
		"rm -rf " + q(staging),
		" && mkdir -p " + q(staging),
		" && tar -xf " + q(tarPath) + " -C " + q(staging),
		drop,
		remove,
		" && mkdir -p " + q(path.Dir(clean)),
		" && mv " + q(staging) + " " + q(clean),
		"; rc=$?; rm -rf " + q(staging) + " " + q(tarPath) + "; exit $rc",
	}, "")
	res, err := sandbox.ExecScript(ctx, sb, sandbox.ExecRequest{Command: cmd})
	var startup *sandbox.StartupOutputError
	if errors.As(err, &startup) {
		// The extraction ran, and its exit status was pushed out of the
		// output by the image's startup (#860). What it did is what the probe
		// reads: a .git at the mount names a tree the rename put there whole,
		// which a stat answers through the read exec's own status.
		if sandbox.StatPresence(ctx, sb, path.Join(clean, ".git")) == sandbox.Present {
			return nil
		}
		return fmt.Errorf("%w: %w", errExtraction, err)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errExtraction, err)
	}
	if !whole && res.ExitCode == occupiedStatus {
		return errMountOccupied
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w with exit %d: %s", errExtraction,
			res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// repoPresence is the idempotence probe: a repository is materialized iff its
// mount carries a .git (stage-and-rename means a present .git always names a
// complete tree). It is asked as a probe exec (sandbox.ProbePaths) and, where
// that does not answer — an image's startup pushing its exit record out of
// the output, a lost exit stream (#105) — as a stat through the file API
// (sandbox.StatPresence), whose Kubernetes answer is the read exec's own exit
// status, which no output flood displaces; and the pair is asked once more
// before it is left unanswered (PresenceUnknown), which is neither: read as
// absent, it re-cloned over the agent's checkout.
func repoPresence(ctx context.Context, sb sandbox.Sandbox, m repoRef) sandbox.Presence {
	git := path.Join(path.Clean(m.MountPath), ".git")
	for range 2 {
		if p := sandbox.ProbePaths(ctx, sb, git); p != sandbox.PresenceUnknown {
			return p
		}
		if p := sandbox.StatPresence(ctx, sb, git); p != sandbox.PresenceUnknown {
			return p
		}
	}
	return sandbox.PresenceUnknown
}

// repoToken decrypts the resource's sealed authorization token. The row is
// scoped by session as well as resource so a stray id cannot reach another
// session's credential, and token_key_id — stored beside the ciphertext, the
// vaults precedent — is what keeps the decrypt honest across a backend key
// rotation.
func (e *Executor) repoToken(ctx context.Context, sid domain.ID, resourceID string) (string, error) {
	var ciphertext []byte
	var keyID string
	err := e.pool.QueryRow(ctx,
		`SELECT token_ciphertext, token_key_id FROM session_resource_credentials
		  WHERE resource_id = $1 AND session_id = $2`,
		resourceID, sid.String()).Scan(&ciphertext, &keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", errRepoNoCredential, resourceID)
	}
	if err != nil {
		return "", err
	}
	plain, err := e.cipher.Decrypt(ctx, ciphertext, keyID)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// emitRepoCloneError appends the session.error variant for a failed clone,
// deduped on (resource_id, reason, message) over the session's history. Two
// failing repositories are two events. A failure with a (reason, message)
// pair the resource has not had yet is emitted; one it has had is not,
// however long ago. The message counts because one reason carries several:
// an occupied mount path's refusal must reach the client after a generic
// `internal` failure. Spanning the history bounds the log, so a session
// polling a failing clone emits once per pair at most. The cost is that a
// return to an earlier failure is not emitted again, so the latest event can
// name a failure that is no longer current.
// The work item's lease already makes this executor the session's single
// writer, so the check-then-append needs no further guarding. Emission is
// best effort: a failure to record the error must not turn a tolerated clone
// failure into a failed run.
func (e *Executor) emitRepoCloneError(ctx context.Context, sid domain.ID, m repoRef, reason, message string) {
	var already bool
	err := e.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM events
		 WHERE session_id = $1 AND type = 'session.error'
		   AND payload->'error'->>'type' = $2
		   AND payload->'error'->>'resource_id' = $3
		   AND payload->'error'->>'reason' = $4
		   AND payload->'error'->>'message' = $5)`,
		sid.String(), repoCloneErrorType, m.ID, reason, message).Scan(&already)
	if err != nil {
		slog.WarnContext(ctx, "checking for an existing repository clone error failed",
			"session_id", sid, "resource_id", m.ID, "err", err)
		return
	}
	if already {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":        repoCloneErrorType,
			"resource_id": m.ID,
			"url":         m.URL,
			"mount_path":  m.MountPath,
			"reason":      reason,
			"message":     message,
			// Required on every variant of the reference's error union, and
			// carried by every other session.error this platform writes.
			// `retrying` for all reasons, including auth: the next work item
			// probes the mount, finds no .git, and clones again — so no clone
			// failure is ever the last attempt, and `exhausted` would tell a
			// client this repository is finished when it is not. An occupied
			// mount path (errMountOccupied) is probed again on every pass too,
			// and clones if the path is ever emptied; one the image ships, such
			// as /usr, never is.
			"retry_status": map[string]any{"type": "retrying"},
		},
	})
	if err != nil {
		return
	}
	if _, err := e.log.Append(ctx, sid, []events.NewEvent{{
		Type: domain.EventSessionError, Payload: payload,
	}}); err != nil {
		slog.WarnContext(ctx, "recording a repository clone error failed",
			"session_id", sid, "resource_id", m.ID, "err", err)
	}
}
