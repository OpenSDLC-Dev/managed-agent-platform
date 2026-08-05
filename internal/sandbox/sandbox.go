// Package sandbox is the "hands" boundary: a disposable per-session container
// where the built-in toolset executes. Cattle, not pets — a sandbox dying is
// one tool-call error, never a lost session, because all durable state lives
// in the event log.
//
// The surface is deliberately small. Higher-level tools (glob, grep, edit) are
// pure functions of Exec and the file primitives below, so they live once in
// the toolset layer instead of being re-implemented by every backend.
//
// Divergence from the plan: there is no Attach. Provision is idempotent per
// session — it returns the session's existing sandbox when one is running —
// which is the only thing an executor ever needed Attach for, and it saves
// persisting a sandbox id nothing else would read.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// DefaultWorkdir is where a sandbox runs commands and where the toolset's
// relative paths resolve when Spec.Workdir is empty. It is one constant so
// those two can never disagree.
const DefaultWorkdir = "/workspace"

// MaxOutputBytes caps what Exec keeps from each of stdout and stderr. It is a
// memory guard on the executor, not the tool-result limit: a command that
// writes a gigabyte must not be able to kill the process that ran it. The
// command still runs to completion — the excess is drained and discarded.
const MaxOutputBytes = 1 << 20

// MaxFileBytes caps ReadFile. The sandbox's filesystem is agent-controlled, so
// a read is an untrusted-length allocation: refuse rather than truncate, since
// a silently half-read file is worse than a failed tool call.
const MaxFileBytes = 4 << 20

var (
	// ErrNotFound reports that the sandbox is gone (destroyed, or reaped by
	// the host). The caller's tool call fails; the session does not.
	ErrNotFound = errors.New("sandbox: no such sandbox")
	// ErrSpecMismatch reports a session's existing sandbox that was created from
	// a different spec than the one this provision asks for. The mismatched
	// settings are fixed at create (networking, image, workdir), so the sandbox
	// cannot be adopted; the provision fails closed rather than silently serving
	// the wrong containment, and removes nothing — replacement is an explicit
	// lifecycle the platform does not have. Both backends produce it, from
	// their twin `adoptable` checks (#29 docker, #296 k8s).
	ErrSpecMismatch = errors.New("sandbox: existing sandbox does not match the requested spec")
	// ErrFileNotExist reports a read of a path that does not exist.
	ErrFileNotExist = errors.New("sandbox: no such file")
	// ErrIsDirectory reports a file read of a directory, or a write onto one.
	ErrIsDirectory = errors.New("sandbox: path is a directory")
	// ErrNotDirectory reports a path blocked by something that is not a directory
	// — a write to `/a/file/child`, or a read of it — the kernel's ENOTDIR. Like
	// ErrIsDirectory it describes the path the caller asked for, so a tool hands
	// it to the model, which can pick another path; left unclassified it would
	// reach the executor as a sandbox fault and the same doomed call would be
	// retried until the lease ran out (#71).
	ErrNotDirectory = errors.New("sandbox: path is not a directory")
	// ErrNotRegularFile reports a file read of a device, FIFO, socket, or other
	// non-regular file. Like ErrIsDirectory it describes the path the caller
	// asked for, not the sandbox failing, so a tool surfaces it to the model
	// rather than to the executor.
	ErrNotRegularFile = errors.New("sandbox: not a regular file")
	// ErrNotReplaceable reports a write onto a target that cannot be renamed
	// onto: a device node, or a file bind-mounted into the sandbox (/etc/hosts).
	// The write path is atomic by rename, and there the rename either fails —
	// rename(2) refuses a mount point with EBUSY — or, worse, succeeds and
	// *supplants* the node with a regular file instead of writing through it.
	// Both backends refuse instead and say why. Like ErrIsDirectory it
	// describes the target the caller asked for — the model can write through
	// such a target with bash redirection — so a tool hands it over as a tool
	// result rather than letting it reach the executor as a sandbox fault and
	// be retried until the lease runs out (#205).
	ErrNotReplaceable = errors.New("sandbox: target cannot be replaced")
	// ErrFileTooLarge reports a read of a file above MaxFileBytes.
	ErrFileTooLarge = errors.New("sandbox: file too large")
)

// Spec is what a session's sandbox is made of. Image is a platform deployment
// choice (the wire's environment config has no image field); Networking comes
// from the environment.
type Spec struct {
	SessionID  domain.ID
	Image      string
	Workdir    string
	Networking domain.Networking
	// Env is injected at provision time and visible to every tool exec (nil =
	// none). Slice 4 populates it with the per-session egress-proxy address and
	// the vault env-var placeholders; both backends thread it in the same way,
	// so the behavior is identical across Docker and Kubernetes.
	//
	// Keys must be valid environment-variable names (ValidateEnv); an invalid
	// key fails provisioning on both backends rather than diverging (Docker
	// would fold a '=' into the value, Kubernetes would reject the pod). Values
	// are unconstrained opaque strings.
	//
	// Env is bound when the sandbox is first created. Provision is idempotent
	// and adopts a session's existing sandbox without re-applying a changed Env
	// — fixed at create, as Networking is, though the two part ways on a
	// mismatch: a changed Networking is refused at adoption (ErrSpecMismatch,
	// #29/#296) where a changed Env is silently kept. A caller
	// that re-provisions a session must therefore keep its Env stable; the
	// egress gate relies on this by minting stable per-session placeholders and
	// resolving their live values at egress rather than re-injecting them.
	Env map[string]string
	// Hardening is the containment the provider applies when it creates the
	// sandbox — cgroup limits, capability drops, a non-root uid, a read-only
	// root filesystem. The zero value applies none of it, which is what a
	// provider did before the field existed; the platform's own defaults are
	// resolved by HardeningFromEnv in the binaries that provision sandboxes.
	// Like Env it is bound at create and not re-applied to an adopted sandbox.
	// See Hardening for what each backend can express.
	Hardening Hardening
	// Gate, when non-nil, tells the provider to run a per-session egress gate
	// sidecar: a gate container (Gate.Image, on the deploy network, holding
	// CAP_NET_ADMIN) that owns the network namespace and installs owner-match
	// iptables, with the sandbox joining that namespace and reaching the network
	// only through the gate's loopback proxy (the provider points the sandbox's
	// HTTP_PROXY there — a deployment detail it owns, not the caller's env). nil =
	// the sandbox networks directly (unrestricted, no vault
	// credentials). The executor sets it for sessions that are `limited` or
	// vault-attached; both backends consume it — Docker as a gate-pair the
	// sandbox joins, K8s as a native sidecar in the sandbox's pod.
	Gate *GateSpec
	// GateTokenRevoker revokes the session's persisted gate token when a
	// provision dismantles its gate without replacing it — the gated→ungated
	// reshape, the one transition where no re-mint (whose revoke-on-re-mint
	// covers every other path) will ever run, so the token would otherwise
	// stay live until the session archives. It cannot live on GateSpec the way
	// TokenMinter does: the provision that needs it is precisely the one with
	// Gate == nil. The executor sets it on every spec (the transition is only
	// discoverable inside the provider, which inspects the previous shape);
	// nil skips revocation.
	GateTokenRevoker GateTokenRevoker
}

// GateSpec configures a session's egress-gate sidecar. The provider mints the
// gate's per-session token via TokenMinter, and only when it creates the pair
// (never when it adopts an existing one) — so a re-provision, which is the normal
// path for every tool call after the first, does not revoke the token a
// still-running gate is using. The token is therefore not carried here.
type GateSpec struct {
	Image           string          // the gate container image (built with `docker build --target gate`)
	ControlplaneURL string          // the gate fetches its per-session config from here
	TokenMinter     GateTokenMinter // mints the gate token on create; see GateTokenMinter
	// OTelEndpoint and OTelInsecure carry the deployment's OTLP collector config
	// into the gate container so its egress_request spans export to the same
	// collector as the rest of the platform (observability is built in, not bolted
	// on). The gate is a separate process that does not inherit the executor's
	// environment, so its telemetry endpoint must be handed to it explicitly. Empty
	// OTelEndpoint means no collector configured — the gate runs without an
	// exporter, exactly as the executor does with an empty endpoint.
	OTelEndpoint string
	OTelInsecure bool
}

// GateTokenMinter mints a per-session gate token in two steps so the provider can
// persist it only after it wins the create race for the gate container. Generate
// returns a fresh plaintext token in memory without any durable effect; the
// provider puts it in the new container's GATE_TOKEN and, only once the create
// succeeds, calls Persist to record its hash as the session's live token. A
// provider that instead adopts an existing gate (the normal path for every tool
// call after the first) never calls either method, so it never revokes the token
// the running gate is already authenticating with — and a create that loses the
// race (409) discards its generated token unpersisted, leaving the winner's
// intact. It lives on GateSpec rather than on the provider because Persist needs
// the executor's DB pool (constructed after the provider) and both backends mint
// identically; the executor supplies an implementation backed by
// internal/gatetoken (a random token from Generate, its hash written by Ensure in
// Persist, over its pool).
type GateTokenMinter interface {
	// Generate returns a fresh gate token in memory, with no durable effect.
	Generate() string
	// Persist records token as sessionID's live gate token (by hash). The provider
	// calls it only after creating the gate container with that token.
	Persist(ctx context.Context, sessionID domain.ID, token string) error
}

// GateTokenRevoker revokes a session's live gate token — the teardown half of
// GateTokenMinter, split into its own interface because it must be reachable
// from an ungated Spec (Gate == nil, so there is no GateSpec to carry it).
// Revoke is idempotent: a session with no live token is a no-op, which lets a
// provider revoke before tearing a stale gate pair down and safely retry both
// if the teardown fails partway. The executor's implementation backs both
// interfaces with internal/gatetoken over the same pool.
type GateTokenRevoker interface {
	Revoke(ctx context.Context, sessionID domain.ID) error
}

// ValidateEnv reports whether every key in a Spec.Env map is a valid
// environment-variable name — [A-Za-z_][A-Za-z0-9_]* — the portable grammar
// both backends inject unchanged. Both call it before rendering so a bad key is
// one clear error, not a silent Docker mis-parse or an opaque Kubernetes pod
// rejection. Values are never constrained.
func ValidateEnv(env map[string]string) error {
	for k := range env {
		if !ValidEnvName(k) {
			return fmt.Errorf("sandbox: invalid environment variable name %q", k)
		}
	}
	return nil
}

// reservedEnvNames are variables an untrusted source must not be allowed to set
// in a sandbox: injecting an opaque value over one of these breaks the sandbox
// or subverts how it launches processes. PATH governs binary resolution for the
// bootstrap (`/bin/bash -c "mkdir … && sleep …"`, unqualified) and every tool
// exec, so overriding it with a placeholder makes the container exit before it
// serves work — a reclaim-loop. LD_PRELOAD/LD_LIBRARY_PATH/LD_AUDIT are dynamic
// loader hooks and BASH_ENV/ENV are shell-startup hooks — process-injection
// surfaces. The egress-proxy variables the gate injects (HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY, upper- and lower-case) are the platform's own, set after this filter;
// they are reserved here so a vault credential's secret_name cannot shadow them
// and divert or cut the sandbox's route to its gate.
var reservedEnvNames = map[string]struct{}{
	"PATH": {}, "LD_PRELOAD": {}, "LD_LIBRARY_PATH": {}, "LD_AUDIT": {},
	"BASH_ENV": {}, "ENV": {}, "IFS": {},
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "no_proxy": {},
}

// ReservedEnvName reports whether k is an environment variable the platform
// reserves — one a caller must not let an untrusted source (a vault credential's
// secret_name) set, because injecting over it would break or subvert the
// sandbox. Such a name is skipped like a name that fails ValidEnvName; it is not
// a grammar rule, so ValidateEnv (which the platform's own trusted injections
// also pass through) does not enforce it.
func ReservedEnvName(k string) bool {
	_, ok := reservedEnvNames[k]
	return ok
}

// ValidEnvName reports whether k is a valid environment-variable name —
// [A-Za-z_][A-Za-z0-9_]*. A caller assembling Spec.Env from an external source
// whose keys are not guaranteed valid (vault credential secret_names) uses it to
// drop the keys ValidateEnv would reject, rather than fault the whole provision
// on one bad name.
func ValidEnvName(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// ExecRequest runs Command through /bin/bash -c inside the sandbox's workdir.
// A zero Timeout means "no limit", and then only the context bounds the call.
type ExecRequest struct {
	Command string
	Timeout time.Duration
}

// ExecResult is a finished command. TimedOut means the command itself outlived
// its deadline: the sandbox stopped it, or stopped waiting for it, or caught it
// still running past the deadline and exiting later on its own terms. TimedOut
// is the authoritative field — ExitCode may be the kill's code, or the code a
// command that dodged the kill chose for itself — and the output is whatever
// arrived. Truncated means output exceeded MaxOutputBytes and the tail was
// discarded.
//
// A backend must decide TimedOut where the sandboxed command cannot reach the
// decision. Anything inside the sandbox is the agent's to tamper with, so a
// deadline enforced only in there is a deadline the command can lift.
//
// The command's own life is what a deadline is measured against, not the life
// of what it leaves behind: a process the command backgrounds inherits its
// output stream and can hold it open long after the command has exited.
type ExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
}

// Sandbox is one session's execution environment.
type Sandbox interface {
	// ID identifies the sandbox to the backend (a container id, a pod name).
	ID() string
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
	// ReadFile returns a file's bytes verbatim, binary included.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// ReadFileStream returns a regular file's bytes as a stream together with
	// their exact count, refusing a file larger than maxBytes with
	// ErrFileTooLarge and answering the same path sentinels as ReadFile. The
	// caller closes the reader.
	//
	// It exists for reads above the fixed ReadFile cap — the deliverables
	// harvest moves files up to its own per-file cap out of the sandbox — so
	// the ceiling is the caller's to name per read. The docker backend
	// streams the bytes through; the k8s backend buffers up to maxBytes
	// internally, because its exec transport frames stdout with a trailing
	// marker that can only be verified once the stream has ended.
	ReadFileStream(ctx context.Context, path string, maxBytes int64) (io.ReadCloser, int64, error)
	// WriteFile writes data, creating parent directories and overwriting any
	// existing file.
	//
	// The write is atomic: the bytes land under a temporary name in the target's
	// own directory (TempName) and are renamed into place, so a transfer that
	// fails part way through leaves the target holding what it held before — or
	// nothing, where there was nothing — never a truncated file. The path itself
	// is answered rather than the sandbox blamed: a path blocked by a
	// non-directory is ErrNotDirectory, a target that is a directory is
	// ErrIsDirectory (the directory is left intact, never replaced), and a
	// target a rename cannot replace — a device node, or a file bind-mounted
	// into the sandbox — is ErrNotReplaceable (the node or mount is left what
	// it was, neither supplanted nor written through; #205).
	//
	// Being a rename, it replaces the *name*, and four consequences follow that a
	// write-through would not have had:
	//   - A symlink at the target is supplanted by a regular file; what it pointed
	//     at is untouched. A symlink to a *directory* is a directory here, as it is
	//     to every other question asked of a path, and is refused as one.
	//   - The parent directory must be writable, even where the target itself
	//     already is.
	//   - The target's permission bits would go with the name, so they are put
	//     back first: the temporary file is chmod'd to the target's mode before the
	//     move, and a script made executable in bash survives being rewritten
	//     (#204). Only an existing *regular* target has bits worth carrying: one
	//     that does not exist lands 0644 on either backend, fixed by the platform
	//     rather than by the image (a tar header on docker; on k8s a `umask 022`
	//     the write script sets — #212 — and a `chmod 0644` beside it, because a
	//     parent directory's default POSIX ACL decides those bits over the umask,
	//     #213), and a symlink or FIFO lands 0644 too — what the rename
	//     replaces is the name, and a link's own mode is 0777.
	//     One case still differs between the backends: docker's temporary
	//     file is extracted by the daemon, so an image whose default user is not
	//     root cannot chmod it and the write lands 0644 where k8s preserves the
	//     mode (#209). (The Claude Code harness's atomic write does the same three
	//     steps — a harness-design observation, not a wire behavior of the
	//     managed-agents reference; the SDK's host-side agenttoolset writes a fixed
	//     0644 instead.)
	//   - A file bind-mounted into the sandbox cannot be renamed onto at all, and
	//     a device node could only be *supplanted*, never written through — so
	//     both are refused with ErrNotReplaceable before the move, identically on
	//     both backends, by the shared __map_unreplaceable probe (#205). A write
	//     to a bind-mounted file fails where the pre-#71 k8s backend used to
	//     write through it; the model is told why, and bash redirection still
	//     reaches it.
	WriteFile(ctx context.Context, path string, data []byte) error
	// WriteFileStream writes exactly size bytes read from src to path, creating
	// parent directories and overwriting any existing file, atomically and with
	// the same path sentinels as WriteFile. Unlike WriteFile it never buffers the
	// whole payload in the caller, so a large mounted file (up to the Files API's
	// 500 MB cap) streams straight through from object storage. size must equal
	// the number of bytes src yields: a short or long stream is an error, not a
	// silently truncated file.
	WriteFileStream(ctx context.Context, path string, src io.Reader, size int64) error
	// WriteFiles writes a whole set of files, each exactly as WriteFile writes
	// one — creating parent directories, overwriting, landing under a temporary
	// name in the target's own directory and renaming into place, carrying an
	// existing target's mode over, and answering the same path sentinels — save
	// ErrNotReplaceable, which only the single writes answer: the bulk path
	// carries no unreplaceable probe, deliberately, because its one caller
	// (skill materialization) writes under the workdir, where a bind-mounted or
	// device target does not arise (#205). An empty batch writes nothing. A member's Path must be absolute and clean; a
	// batch naming the same target twice lands them in order, so the last wins.
	//
	// What it buys is round trips: the whole batch travels as an archive and costs
	// a fixed couple of execs — one on the k8s backend, two on docker — where the
	// same files written one at a time cost one exec each, about 14ms apiece
	// against a local daemon, which is most of what a small write costs (#206). A
	// skill of ten thousand files is the case that made it worth a method.
	//
	// The batch is NOT a transaction, and deliberately not: the first failure
	// stops the run, the members that already landed stay landed, and the rest
	// are never written. That is what a loop of WriteFile calls did, and the one
	// caller of either — materializing a skill — re-runs the whole skill next
	// time rather than reasoning about what got through. Every member is still
	// atomic on its own, so a failure leaves each target holding what it held,
	// never a truncated file. The single exception is a delivery that arrived
	// short, which lands nothing at all: every member is checked to be present
	// before any of them is moved.
	//
	// A batch that reached the sandbox and failed there sheds its temporary
	// files. One whose exec could not be run, or whose shell was killed before it
	// could clean up, leaves what it had delivered — as a single write in the
	// same position already does, at one file rather than N.
	//
	// The error names the member it stopped on, with two honest limits. Where
	// more than one member would have failed it names one of them, and a path
	// blocked by a non-directory is preferred over the delivery's own failure
	// because it is the one a caller can act on. And the naming rides a marker on
	// the sandbox's stderr, which the sandbox can flood or forge: it is a
	// diagnostic, not a guarantee, and it degrades to naming no member rather
	// than the wrong one where the marker is unusable. The error's *class* — the
	// sentinels below — does not depend on it.
	WriteFiles(ctx context.Context, files []FileWrite) error
	// Destroy removes the sandbox. It is idempotent: destroying an already
	// destroyed sandbox is not an error.
	Destroy(ctx context.Context) error
}

// Provider makes sandboxes. Every backend passes the same contract suite
// (internal/sandbox/sandboxtest).
type Provider interface {
	// Provision returns the session's sandbox, creating it only if none is
	// running. Concurrent executors provisioning the same session converge on
	// one sandbox rather than racing to create two.
	Provision(ctx context.Context, spec Spec) (Sandbox, error)
}
