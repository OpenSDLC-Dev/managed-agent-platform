package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// The environment's config.packages installed into the session's sandbox
// (docs/plan/40_environment-packages.md). One Exec per non-empty manager
// through the seam every tool already uses, so both backends behave
// identically and sandbox.Spec is untouched (decision 1).

// packageInstallErrorType is the session.error variant a failed install
// surfaces — this platform's own, like the clone error's: the reference's
// session-error union has no packages variant, and what one looks like on its
// wire is unrecorded (plan 40 ground truth).
const packageInstallErrorType = "environment_package_install_error"

// The reasons the variant carries. The first four name a manager; the last two
// are the sandbox's own (decision 7) and carry none.
const (
	packageReasonFailed         = "failed"
	packageReasonManagerMissing = "manager_missing"
	packageReasonTimeout        = "timeout"
	packageReasonInvalid        = "invalid"
	packageReasonNotRoot        = "sandbox_not_root"
	packageReasonReadOnly       = "rootfs_read_only"
)

// packagesSentinelPath records what each manager last attempted in THIS
// sandbox. /tmp because it is writable in every hardening shape and, unlike
// the checkpoint roots (checkpoint.go), is deliberately not preserved — so a
// restored sandbox, which is a fresh container, installs again (decision 2).
//
// It is agent-writable and trusted for nothing load-bearing: a forged one
// skips an install the agent then lacks, a deleted or unparsable one costs a
// repeated install.
const packagesSentinelPath = "/tmp/.map-packages"

// packageInstallAttempts is how many times one unchanged list may fail in a
// sandbox before it is left alone until it changes. A typo'd entry, or a
// registry the gate refuses, must stop costing a full install budget before
// every tool call; a transient failure still gets two more chances
// (decision 2).
const packageInstallAttempts = 3

// packageOutputTailBytes is what the install command's own pipeline keeps —
// the LAST bytes of the combined output, which is the failure, where Exec's
// own cap would keep the head. It matches maxPackageMessage on purpose: the
// tail is already the message.
const packageOutputTailBytes = 8 << 10

// maxPackageMessage bounds the text the session.error carries — the same bound
// the brain's maxFailureMessage holds for a session.error. The tail above is
// already this size, so the cut only ever bites on output a forged sandbox
// command produced.
const maxPackageMessage = 8 << 10

// packageRecord is one manager's line in the sentinel: a digest of the list it
// last attempted, whether that attempt installed, and how many attempts that
// list has had in this sandbox.
//
// The digest, not the list itself: the sentinel is agent-readable (it lives in
// /tmp of the sandbox the agent's tools run in), and a pip or npm entry may
// legitimately be a URL carrying a management-key credential, which must not
// become catable from inside the sandbox. The skip check and the error dedupe
// need only to tell one list from another, which a digest does (decision 2/4).
type packageRecord struct {
	Digest    string `json:"digest"`
	Installed bool   `json:"installed"`
	Attempts  int    `json:"attempts"`
}

// packageManager is one of the six the reference names. install builds the
// manager's own command from the entries verbatim — quoted, never
// interpolated — as the reference's table shows each entry passed to its
// manager in that manager's native syntax.
type packageManager struct {
	name string
	// preflight makes a missing manager exit 127 whatever the manager, so the
	// classification below does not have to tell one manager's "not found"
	// exit from another's ordinary failure.
	preflight string
	install   func(entries []string) string
}

// packageManagers is the reference's order — alphabetical, which is what the
// docs promise a client whose apt list must land before its pip list.
//
// The choices that are ours rather than the reference's are argued in
// decision 3: cargo and go binaries land in /usr/local/bin rather than a home
// directory no PATH in an arbitrary image includes; npm installs globally, so
// a package's binaries are on PATH; pip overrides PEP 668's
// externally-managed refusal through the environment variable rather than the
// flag, because an older pip rejects an unknown flag and ignores an unknown
// variable; apt-get update precedes the install because a slim image ships no
// package lists; and `go install` gets an @latest suffix and one invocation
// per entry.
var packageManagers = []packageManager{
	{
		name:      "apt",
		preflight: packagePreflight("apt-get"),
		install: func(entries []string) string {
			// `dpkg --configure -a` repairs a transaction an earlier deadline
			// killed, which otherwise wedges every later apt-get — the agent's
			// own included. Unconditional, and before the update: an install's
			// idempotence is a property of a command that ran to completion,
			// not of one the deadline killed (decision 2).
			//
			// APT::Sandbox::User=root keeps apt's acquire methods as root. apt
			// otherwise drops them to `_apt`, which takes CAP_SETUID and
			// CAP_SETGID — the two the platform's own default hardening drops
			// (sandbox.DefaultCapDrop) and a gated sandbox always drops — so
			// without it every fetch dies with `setgroups 65534 failed` and
			// `Method http has died unexpectedly`, on the default deployment
			// and not merely a hardened one. apt's sandbox guards a host from
			// its fetchers; inside a container that already is the platform's
			// sandbox it guards nothing this platform relies on.
			const noSandbox = "-o APT::Sandbox::User=root"
			return "export DEBIAN_FRONTEND=noninteractive; dpkg --configure -a; " +
				"apt-get " + noSandbox + " update -q && apt-get " + noSandbox + " install -y -q " + quoteEntries(entries)
		},
	},
	{
		name:      "cargo",
		preflight: packagePreflight("cargo"),
		install: func(entries []string) string {
			return "cargo install --root /usr/local " + quoteEntries(entries)
		},
	},
	{
		name:      "gem",
		preflight: packagePreflight("gem"),
		install: func(entries []string) string {
			return "gem install --no-document " + quoteEntries(entries)
		},
	},
	{
		name:      "go",
		preflight: packagePreflight("go"),
		install: func(entries []string) string {
			// One invocation per entry, because `go install` refuses @version
			// arguments from different modules in one call; an entry carrying
			// no '@' gets @latest, because outside a module `go install`
			// requires a version and the docs promise an unpinned entry
			// installs the latest.
			cmds := make([]string, len(entries))
			for i, e := range entries {
				if !strings.Contains(e, "@") {
					e += "@latest"
				}
				cmds[i] = "GOBIN=/usr/local/bin go install " + shellQuote(e)
			}
			return strings.Join(cmds, " && ")
		},
	},
	{
		name:      "npm",
		preflight: packagePreflight("npm"),
		install: func(entries []string) string {
			return "npm install -g " + quoteEntries(entries)
		},
	},
	{
		name: "pip",
		// Not `command -v pip`: a slim image ships python3 without pip, and
		// that failure exits 1 rather than 127.
		preflight: "python3 -m pip --version >/dev/null 2>&1 || exit 127",
		install: func(entries []string) string {
			return "PIP_BREAK_SYSTEM_PACKAGES=1 PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_INPUT=1 " +
				"python3 -m pip install " + quoteEntries(entries)
		},
	},
}

// packagePreflight is the missing-manager probe: `|| exit 127` inside the
// pipeline's left-hand subshell, which pipefail then carries out as the whole
// command's status.
func packagePreflight(bin string) string {
	return "command -v " + bin + " >/dev/null 2>&1 || exit 127"
}

// packagesDigest reduces a manager's list to the value the agent-readable
// sentinel and the error dedupe compare, so neither has to carry the entries
// themselves. NUL-joined so that no concatenation of entries collides with a
// different list (["a","bc"] and ["ab","c"] differ).
func packagesDigest(entries []string) string {
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// quoteEntries single-quotes every entry, so a list element is one argv member
// whatever it contains.
func quoteEntries(entries []string) string {
	quoted := make([]string, len(entries))
	for i, e := range entries {
		quoted[i] = shellQuote(e)
	}
	return strings.Join(quoted, " ")
}

// command is the whole `bash -c` string for one manager. `set -o pipefail` is
// what makes the group's status — the preflight's 127, or the install's own —
// survive the tail that keeps the last bytes of the combined output.
func (m packageManager) command(entries []string) string {
	return "set -o pipefail; { " + m.preflight + "; " + m.install(entries) + "; } 2>&1 | tail -c " +
		strconv.Itoa(packageOutputTailBytes)
}

// validPackageList reports whether every entry may be handed to its manager.
// One refused entry refuses the whole list: the entries of one manager are one
// command, so there is no way to install the rest and skip that one.
func validPackageList(entries []string) bool {
	for _, e := range entries {
		if !domain.ValidPackageEntry(e) {
			return false
		}
	}
	return true
}

// packagesProbeCommand is decision 7's cheap refusal: every manager writes
// under /usr or /var, so a sandbox that is not root, or whose root filesystem
// is read-only, cannot install anything. Probing the sandbox rather than
// reading cfg.Hardening is deliberate — RunAsUser and ReadOnlyRootfs are bound
// at create and an adopted sandbox may disagree with this executor's current
// config, and an image whose own default user is non-root is invisible to the
// config entirely.
const packagesProbeCommand = `if [ "$(id -u)" != 0 ]; then echo sandbox_not_root; ` +
	`elif ! [ -w /usr ] || ! [ -w /var ]; then echo rootfs_read_only; else echo ok; fi`

// packageProbeMessages are the sandbox-level reasons' fixed messages. Unlike a
// manager's, these are the platform's own text: no command ran, so there is no
// output to carry.
var packageProbeMessages = map[string]string{
	packageReasonNotRoot:  "the sandbox does not run as root, so no package manager can install into it",
	packageReasonReadOnly: "the sandbox's root filesystem is read-only, so no package manager can install into it",
}

// installPackages installs the environment's config.packages into the freshly
// provisioned sandbox, in the reference's alphabetical order, one Exec per
// manager that has work. It runs inside provisionSandbox's advisory-lock hold
// (decision 2), so a reclaiming executor waits on the lapsed holder's pass
// instead of racing its apt-get for the dpkg lock.
//
// Per-manager failure is surfaced, never fatal (decision 4): the session's
// other managers still install, its tools still run, and the agent meets a
// missing package the way it would on a host — an import error it can read.
// The only error returned is a backend fault from Exec (the sandbox gone, the
// context cancelled), which faults the item exactly as a provision failure
// does.
func (e *Executor) installPackages(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, pkgs map[string][]string, progress func()) error {
	// Every stored cloud config carries all six lists, empty or not, so the
	// map being non-nil says nothing: what decides is whether any list has
	// entries. Answered before the span opens, so a package-less session — the
	// overwhelming majority — leaves no series behind.
	work := false
	for _, m := range packageManagers {
		if len(pkgs[m.name]) > 0 {
			work = true
			break
		}
	}
	if !work {
		return nil
	}

	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "packages_install")
	defer span.End()

	recs := readPackageSentinel(ctx, sb)
	var ran, skipped, failed int
	start := time.Now()
	defer func() {
		// Only a pass that actually installed belongs in the duration histogram;
		// a settled session's per-turn skip pass would otherwise dominate it with
		// near-zero samples and hide the real install times (review).
		if ran > 0 {
			recordPackagesInstallDuration(ctx, time.Since(start))
		}
	}()
	defer func() {
		span.SetAttributes(
			attribute.Int("packages.managers", ran),
			attribute.Int("packages.skipped", skipped),
			attribute.Int("packages.failed", failed),
		)
	}()
	probed := false
	for _, m := range packageManagers {
		entries := pkgs[m.name]
		if len(entries) == 0 {
			continue
		}
		digest := packagesDigest(entries)
		rec, seen := recs[m.name]
		if seen && rec.Digest == digest {
			// Settled, or out of attempts: either way this sandbox is done
			// with this list until it changes.
			if rec.Installed || rec.Attempts >= packageInstallAttempts {
				skipped++
				recordPackageInstalled(ctx, m.name, packageOutcomeSkipped)
				continue
			}
		} else {
			// A changed list is a new attempt with a fresh count.
			rec = packageRecord{}
		}
		// Judged again here, having already been judged at create (decision 6):
		// a row stored before that rule is refused at install rather than
		// passed to a manager that would read it as an option.
		if !validPackageList(entries) {
			failed++
			recordPackageInstalled(ctx, m.name, packageOutcomeInvalid)
			// Exhausted from the first attempt: nothing in the session's life
			// makes a refused entry acceptable, so telling a client to wait
			// for a retry would be a lie.
			e.emitPackageInstallError(ctx, sid, m.name, packageReasonInvalid,
				"an entry is empty or begins with '-', which a package manager reads as an option rather than a package", digest, true)
			continue
		}
		// The probe is lazy: it costs an Exec, and a pass whose every manager
		// is settled or refused must run none at all.
		if !probed {
			probed = true
			progress()
			reason, err := probeSandboxForPackages(ctx, sb, e.cfg.PackageInstallTimeout)
			if err != nil {
				return err
			}
			if reason != "" {
				slog.WarnContext(ctx, "the sandbox cannot install packages",
					"session_id", sid, "reason", reason)
				// Recorded once, with no manager, and running none: six
				// timeouts' worth of "Permission denied" tell a client
				// nothing the probe has not already said.
				e.emitPackageInstallError(ctx, sid, "", reason, packageProbeMessages[reason], "", true)
				return nil
			}
		}
		progress()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: m.command(entries),
			Timeout: e.cfg.PackageInstallTimeout,
		})
		if err != nil {
			return err
		}
		ran++
		rec.Digest = digest
		rec.Attempts++
		reason := packageFailureReason(res)
		rec.Installed = reason == ""
		if reason == "" {
			recordPackageInstalled(ctx, m.name, packageOutcomeOK)
			slog.InfoContext(ctx, "packages installed",
				"session_id", sid, "manager", m.name, "packages", len(entries))
		} else {
			failed++
			// The reason IS the metric's outcome value for a failed install:
			// one vocabulary, so a dashboard and an event agree.
			recordPackageInstalled(ctx, m.name, reason)
			slog.WarnContext(ctx, "packages not installed",
				"session_id", sid, "manager", m.name, "reason", reason, "attempts", rec.Attempts)
			e.emitPackageInstallError(ctx, sid, m.name, reason, packageMessage(res.Stdout),
				digest, rec.Attempts >= packageInstallAttempts)
		}
		recs[m.name] = rec
		writePackageSentinel(ctx, sb, sid, recs)
	}
	return nil
}

// packageFailureReason classifies one settled install. TimedOut is the
// authoritative field (sandbox.ExecResult), so it is asked first: a killed
// command's exit code may be the kill's or one it chose for itself.
func packageFailureReason(res sandbox.ExecResult) string {
	switch {
	case res.TimedOut:
		return packageReasonTimeout
	case res.ExitCode == 127:
		return packageReasonManagerMissing
	case res.ExitCode != 0:
		return packageReasonFailed
	default:
		return ""
	}
}

// packageMessage is the kept tail of a manager's own output on its way onto the
// event log: URL credentials redacted, NUL-stripped (a NUL would fault the
// jsonb append and reclaim-loop the item), and bounded. This is the one
// session.error whose text a sandbox controls (decision 4), so redaction is not
// optional.
//
// redactURL is the MCP path's own redactor (mcpwork.go): it reduces every
// http(s) URL to scheme://host, so a credential in the userinfo OR the query
// (`?token=…`, a pip/npm convention) is dropped — not merely the userinfo up to
// the first '@', which a password containing '@' would defeat and a query
// credential would slip past entirely. anySchemeUserinfoRe then drops the
// userinfo of a non-http URL too (`git+ssh://token@…`, a legitimate pip/npm VCS
// entry), which redactURL — anchored on `http(s)://` — does not reach. A query
// credential in a non-http URL, and one whose `http(s)://` scheme the in-shell
// `tail -c` cut off, are the residuals #599's out-of-band credential injection
// closes at the source.
func packageMessage(out string) string {
	msg := urlInText.ReplaceAllStringFunc(out, redactURL)
	msg = anySchemeUserinfoRe.ReplaceAllString(msg, "$1***@")
	msg = toolset.SanitizeText(msg)
	return toolset.TruncateRunes(msg, maxPackageMessage)
}

// anySchemeUserinfoRe matches the userinfo of a URL of any scheme, for the
// non-http schemes redactURL leaves alone.
var anySchemeUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s/@]+@`)

// probeSandboxForPackages answers decision 7's question, returning the reason the pass
// must be refused or "" to proceed. An answer the probe cannot have produced —
// a shell so broken it printed something else — proceeds with a log line
// rather than inventing a reason for the wire: the install's own failure is
// then the honest diagnosis.
func probeSandboxForPackages(ctx context.Context, sb sandbox.Sandbox, timeout time.Duration) (string, error) {
	// Bounded by the same budget as an install: the probe is trivial, but a
	// container whose shell wedges answering it must not hang provisioning on the
	// outer lease alone (review).
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: packagesProbeCommand, Timeout: timeout})
	if err != nil {
		return "", err
	}
	switch answer := strings.TrimSpace(res.Stdout); answer {
	case packageReasonNotRoot, packageReasonReadOnly:
		return answer, nil
	case "ok":
		return "", nil
	default:
		slog.WarnContext(ctx, "the package-install probe answered unrecognizably; installing anyway",
			"answer", answer, "exit_code", res.ExitCode)
		return "", nil
	}
}

// readPackageSentinel reads what this sandbox has already settled. Absent,
// unreadable or unparsable all read as "nothing settled": the sentinel is
// agent-writable, so the safe direction is a repeated install rather than a
// skipped one.
func readPackageSentinel(ctx context.Context, sb sandbox.Sandbox) map[string]packageRecord {
	data, err := sb.ReadFile(ctx, packagesSentinelPath)
	if err != nil {
		return map[string]packageRecord{}
	}
	var recs map[string]packageRecord
	if err := json.Unmarshal(data, &recs); err != nil || recs == nil {
		return map[string]packageRecord{}
	}
	return recs
}

// writePackageSentinel records the settled set after each manager, through
// WriteFile, which is atomic — so a pass cut short mid-install leaves either
// the previous record or a complete new one, never half of one. A failed write
// costs a repeated install and is never fatal.
func writePackageSentinel(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, recs map[string]packageRecord) {
	data, err := json.Marshal(recs)
	if err != nil {
		return
	}
	if err := sb.WriteFile(ctx, packagesSentinelPath, data); err != nil {
		slog.WarnContext(ctx, "the package-install sentinel was not written",
			"session_id", sid, "err", err)
	}
}

// emitPackageInstallError appends the session.error variant for a refused or
// failed install, deduped on (manager, reason, retry_status.type, digest) — a
// repeated identical failure of the same list is one event, a reason flip is a
// new one, the attempt that exhausts the cap re-emits under the flipped
// retry_status, and a *different* list that fails the same way is a new event
// rather than one the first list's history suppresses. The list identity is the
// digest, mirroring the clone error's (resource_id, reason) key. The work
// item's lease already makes this executor the session's single writer, so the
// check-then-append needs no further guarding, and emission is best effort:
// failing to record the error must not turn a tolerated install failure into a
// failed run (the emitRepoCloneError precedent).
//
// The entry list itself is deliberately not carried — only its digest: the
// environment's config already holds the list for a management key, while the
// events subtree is also readable with an environment key, and the manager plus
// the output tail name what failed (decision 4). digest is empty for the
// sandbox-level reasons, which are the sandbox's rather than a list's.
func (e *Executor) emitPackageInstallError(ctx context.Context, sid domain.ID, manager, reason, message, digest string, exhausted bool) {
	// Required on every variant of the reference's error union. `exhausted`
	// where nothing the session can do will change the answer: the attempt cap
	// is spent, the entry is refused, or the sandbox itself cannot install.
	retryStatus := "retrying"
	if exhausted {
		retryStatus = "exhausted"
	}
	var already bool
	// COALESCE, because the sandbox-level reasons carry neither a manager nor a
	// digest and a NULL would never compare equal to the '' this passes for them.
	err := e.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM events
		 WHERE session_id = $1 AND type = 'session.error'
		   AND payload->'error'->>'type' = $2
		   AND COALESCE(payload->'error'->>'manager', '') = $3
		   AND payload->'error'->>'reason' = $4
		   AND payload->'error'->'retry_status'->>'type' = $5
		   AND COALESCE(payload->'error'->>'packages_digest', '') = $6)`,
		sid.String(), packageInstallErrorType, manager, reason, retryStatus, digest).Scan(&already)
	if err != nil {
		slog.WarnContext(ctx, "checking for an existing package install error failed",
			"session_id", sid, "manager", manager, "err", err)
		return
	}
	if already {
		return
	}
	errObj := map[string]any{
		"type":         packageInstallErrorType,
		"reason":       reason,
		"message":      message,
		"retry_status": map[string]any{"type": retryStatus},
	}
	if manager != "" {
		errObj["manager"] = manager
	}
	// A non-secret list fingerprint, so a client (and this dedupe) can tell one
	// failing list from another. Absent for the sandbox-level reasons.
	if digest != "" {
		errObj["packages_digest"] = digest
	}
	payload, err := json.Marshal(map[string]any{"error": errObj})
	if err != nil {
		return
	}
	if _, err := e.log.Append(ctx, sid, []events.NewEvent{{
		Type: domain.EventSessionError, Payload: payload,
	}}); err != nil {
		slog.WarnContext(ctx, "recording a package install error failed",
			"session_id", sid, "manager", manager, "err", err)
	}
}
