package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
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

// errRepoNoCredential classifies a repo resource whose sealed token row is
// gone. It cannot happen through the API (the row is written in the create
// transaction and dies only with the session), so it is an internal fault
// rather than a user-visible reason of its own.
var errRepoNoCredential = errors.New("repository credential row missing")

// materializeRepos clones each mounted repository into the sandbox before the
// tools run, between skills and files so a file mount may deliberately overlay
// into a checkout (plan 25 decision 5). Idempotence is the probe alone — a repo
// materializes iff `<mount>/.git` is absent — deliberately without a sentinel:
// the mounted set is fixed at create, so a marker would have no drift to
// detect, and plan 24 strips agent-writable markers from checkpoints at
// capture, which would make a restored workspace re-clone over the agent's own
// work. The tree is the only authority there is.
//
// Per-repository failure is tolerated exactly like a file's: the run continues
// and the agent sees an absent path. Unlike a file's, it also surfaces as a
// session.error the client can see (decision 4). refs come from the same
// locked session read that gated the run (sessionForRun).
func (e *Executor) materializeRepos(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, refs []repoRef) {
	mounts := make([]repoRef, 0, len(refs))
	for _, r := range refs {
		if r.Type == "github_repository" && r.ID != "" && r.URL != "" && r.MountPath != "" {
			mounts = append(mounts, r)
		}
	}
	if len(mounts) == 0 {
		return
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

	var landed, unchanged, failed int
	for _, m := range mounts {
		if e.repoPresent(ctx, sb, m) {
			unchanged++
			recordRepoMaterialized(ctx, repoOutcomeUnchanged)
			slog.InfoContext(ctx, "repository already present, skipping clone",
				"session_id", sid, "resource_id", m.ID, "url", m.URL, "mount_path", m.MountPath)
			continue
		}
		// Judged again here, having already been judged at create. The
		// duplication is deliberate and narrow: what follows builds an `rm -rf`
		// on this path inside the session's sandbox, and a row that reached the
		// database by any route other than the create endpoint would otherwise
		// have that command built for it. `/workspace/repo/..` cleans to
		// `/workspace`; `/tmp` is the platform's own staging root.
		if err := e.safeRepoMount(m.MountPath); err != nil {
			failed++
			recordRepoMaterialized(ctx, repoOutcomeInternal)
			slog.WarnContext(ctx, "repository mount path refused by the executor",
				"session_id", sid, "resource_id", m.ID, "mount_path", m.MountPath, "err", err)
			e.emitRepoCloneError(ctx, sid, m, repoOutcomeInternal,
				"the repository could not be cloned into the sandbox")
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
			e.emitRepoCloneError(ctx, sid, m, repoOutcomeInternal,
				"a secrets cipher is not configured on this executor")
			continue
		}
		bytes, err := e.materializeRepo(ctx, sb, sid, m)
		if err != nil {
			failed++
			reason := cloneReason(err)
			if errors.Is(err, errRepoNoCredential) {
				reason = repoOutcomeInternal
			}
			recordRepoMaterialized(ctx, reason)
			slog.WarnContext(ctx, "repository not materialized",
				"session_id", sid, "resource_id", m.ID, "url", m.URL, "mount_path", m.MountPath,
				"reason", reason, "err", err)
			e.emitRepoCloneError(ctx, sid, m, reason, "the repository could not be cloned into the sandbox")
			continue
		}
		landed++
		recordRepoMaterialized(ctx, repoOutcomeOK)
		recordRepoMaterializeBytes(ctx, bytes)
		slog.InfoContext(ctx, "repository materialized",
			"session_id", sid, "resource_id", m.ID, "url", m.URL, "mount_path", m.MountPath, "bytes", bytes)
	}
	span.SetAttributes(
		attribute.Int("repos.materialized", landed),
		attribute.Int("repos.unchanged", unchanged),
		attribute.Int("repos.failed", failed),
	)
}

// materializeRepo clones one repository and lands it at its mount path,
// returning the shipped tar's size. The token is decrypted here, held only for
// the clone's own HTTP requests, and never written anywhere: the executor
// sends it as an Authorization header, so the tar that reaches the sandbox
// carries a clean .git/config and the sandbox never sees the secret in any
// form (plan 25 decision 1).
func (e *Executor) materializeRepo(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, m repoRef) (int64, error) {
	token, err := e.repoToken(ctx, sid, m.ID)
	if err != nil {
		return 0, err
	}
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

	// The tar lands under the platform's staging path, which create reserves
	// from mount_path (decision 3) so a repository can never be mounted over it.
	sandboxTar := path.Join(repoStagingRoot, "repo-"+m.ID+".tar")
	if err := sb.WriteFileStream(ctx, sandboxTar, f, size); err != nil {
		return 0, err
	}
	// From here the sandbox holds up to the whole byte budget in /tmp, and the
	// only thing that removes it is the extraction command's own cleanup tail.
	// A context cancelled between the two — the clone deadline, or a lost lease
	// — makes Exec return without ever running that tail, stranding a file the
	// size of the repository inside the session's sandbox for its whole life.
	// Swept here instead, on a context the cancellation cannot reach.
	defer func() {
		if ctx.Err() == nil {
			return
		}
		sweep, stop := context.WithTimeout(context.WithoutCancel(ctx), repoSweepTimeout)
		defer stop()
		if _, err := sb.Exec(sweep, sandbox.ExecRequest{
			Command: "rm -rf " + shellQuote(sandboxTar) + " " + shellQuote(stagingPath(m.MountPath)),
		}); err != nil {
			slog.WarnContext(ctx, "the cancelled clone's sandbox staging was not swept",
				"session_id", sid, "resource_id", m.ID, "path", sandboxTar, "err", err)
		}
	}()
	if err := extractRepo(ctx, sb, m.MountPath, sandboxTar); err != nil {
		return 0, err
	}
	return size, nil
}

// repoSweepTimeout bounds the post-cancellation sweep. Short on purpose: the
// work item is already over, and a sandbox that will not answer a `rm` will not
// answer a longer one either.
const repoSweepTimeout = 15 * time.Second

// repoStagingRoot is where a clone's tar lands inside the sandbox on its way
// to the mount. /tmp is one of the two reserved mount targets (plan 25
// decision 3), which is what keeps a repository from being mounted over the
// staging area.
const repoStagingRoot = "/tmp"

// safeRepoMount is the executor's own judgment of a mount path, independent of
// the API's. It admits only an absolute path that is already in clean form and
// names something strictly inside a parent — which is what makes `rm -rf` on it
// bounded — and refuses what this executor knows is already spoken for.
//
// It is deliberately not a second copy of the API's full grammar (nesting,
// per-session caps, the URL-derived default). Those decide what a caller may
// ask for; this decides only what may be destroyed. Three of the refusals are
// things the control plane cannot know, which is why they live here:
//
//   - the workdir, and the skills directory inside it. Skills are materialized
//     into <workdir>/skills a few lines before repositories are, so a mount of
//     either would `rm -rf` a tree written in the same pass — while the system
//     prompt goes on naming skills/<dir>/SKILL.md. This is reachable without
//     anyone trying: a repository named `skills` derives exactly
//     <workdir>/skills as its default mount.
//   - another repository's staging path. `/workspace/.x.map-repo-staging` is a
//     mount path the API accepts, and cloning `/workspace/x` removes it twice.
//
// The workdir is executor configuration, and the staging name is an executor
// implementation detail; neither is visible where the resource is validated.
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
	if mount == path.Clean(workdir) || mount == path.Join(workdir, "skills") {
		return fmt.Errorf("mount path %q is the sandbox workdir or its skills directory", mount)
	}
	if strings.HasPrefix(path.Base(mount), ".") && strings.HasSuffix(mount, stagingSuffix) {
		return fmt.Errorf("mount path %q is a repository staging path", mount)
	}
	return nil
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

// extractRepo unpacks the shipped tar into a staging directory beside the
// mount and renames it into place, so `<mount>` — and therefore the
// `<mount>/.git` the idempotence probe reads — exists only once the tree is
// complete: a mid-extraction failure (disk full, unwritable member, a kill)
// can never leave a partial tree for a later pass to trust. Staging is a
// sibling of the mount so the rename stays within one filesystem.
//
// Every path is shell-quoted: mount_path is request-controlled text, and
// unquoted interpolation would be command injection inside the sandbox.
// Cleanup runs unconditionally — after a `;`, never sequenced behind an `&&` —
// so a failed extraction still removes the tar and any partial staging.
func extractRepo(ctx context.Context, sb sandbox.Sandbox, mount, tarPath string) error {
	clean := path.Clean(mount)
	staging := stagingPath(mount)
	q := shellQuote
	cmd := strings.Join([]string{
		"rm -rf " + q(staging),
		" && mkdir -p " + q(staging),
		" && tar -xf " + q(tarPath) + " -C " + q(staging),
		" && rm -rf " + q(clean),
		" && mkdir -p " + q(path.Dir(clean)),
		" && mv " + q(staging) + " " + q(clean),
		"; rc=$?; rm -rf " + q(staging) + " " + q(tarPath) + "; exit $rc",
	}, "")
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: cmd})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("extracting the repository into the sandbox failed with exit %d: %s",
			res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// repoPresent is the idempotence probe: a repository is materialized iff its
// mount carries a .git. A missing sandbox or a failed exec reads as "absent",
// so the caller re-clones rather than skipping — the files precedent, and the
// safe direction (stage-and-rename means a present .git always names a
// complete tree).
func (e *Executor) repoPresent(ctx context.Context, sb sandbox.Sandbox, m repoRef) bool {
	cmd := "test -e " + shellQuote(path.Join(path.Clean(m.MountPath), ".git")) + " && true"
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: cmd})
	return err == nil && res.ExitCode == 0
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
// deduped on (resource_id, reason) — two failing repositories are two events,
// and a reason flip re-emits, while a repeated identical failure does not.
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
		   AND payload->'error'->>'reason' = $4)`,
		sid.String(), repoCloneErrorType, m.ID, reason).Scan(&already)
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
			// client this repository is finished when it is not.
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
