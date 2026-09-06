package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
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

// packagesCredsRemoveTimeout bounds the removal of a manager's credential
// directory. It is not an install: `rm -rf` on one directory either answers at
// once or the sandbox is not answering at all, and giving it the install's
// budget would put two full install timeouts inside one silent interval.
const packagesCredsRemoveTimeout = 30 * time.Second

// packageInstallAttempts is how many times one unchanged list may fail in a
// sandbox before it is left alone until it changes. A typo'd entry, or a
// registry the gate refuses, must stop costing a full install budget before
// every tool call; a transient failure still gets two more chances
// (decision 2).
const packageInstallAttempts = 3

// maxInstallCommandBytes bounds the assembled command handed to one Exec. It
// is one execve argument, which Linux caps near 128 KiB (MAX_ARG_STRLEN); a
// command past that faults at exec startup rather than running, and a fault
// reclaim-loops the item. The API's per-manager byte cap does not bound this:
// it counts entry bytes, while `go` emits one `go install` per entry (far more
// than the entry's own bytes), and a row stored before that cap existed never
// passed it. This is the backstop, set below the ceiling with room for the
// pipefail/tail wrapper.
const maxInstallCommandBytes = 120 << 10

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
	// netrc says this manager's fetch reads $HOME/.netrc — pip's own fetcher
	// does, and git does, which is what a `git+https` entry becomes for pip,
	// npm and go alike. apt, cargo and gem read their own credential stores, so
	// moving a credential into a netrc for them would break an install that
	// works rather than protect one.
	netrc bool
	// npmrc says this manager's own fetcher reads no netrc, so a credential
	// lifted out of one of its entries needs npm's per-host pair as well.
	npmrc bool
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
		netrc:     true,
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
		// A `git+https` npm entry is git's fetch, which reads the netrc; npm's
		// own fetcher reads none, so a credentialed tarball URL needs the
		// per-host pair its config carries as well (plan 46, measured).
		netrc: true,
		npmrc: true,
		install: func(entries []string) string {
			return "npm install -g " + quoteEntries(entries)
		},
	},
	{
		name: "pip",
		// Not `command -v pip`: a slim image ships python3 without pip, and
		// that failure exits 1 rather than 127.
		preflight: "python3 -m pip --version >/dev/null 2>&1 || exit 127",
		netrc:     true,
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

// A config.packages entry may be a URL, and a URL may carry its own
// credential — `pip: ["git+https://user:token@host/repo"]`. Left in the entry
// it rides in the install command, which is one execve argument on the docker
// backend and the exec subresource's `command` parameters on Kubernetes, where
// the apiserver's audit log records it for anyone who reads that log (#599, the
// one half of this that leaves the session's trust domain). So it is lifted out
// and written into the file the fetcher reads instead. Which file, per
// transport, was measured rather than recalled — plan 46 carries the table.

// packageCredential is one entry's userinfo, decoded.
type packageCredential struct {
	// authority is the URL's host with its port, which is what an npmrc key
	// carries; machine is the hostname alone, which is what a netrc line
	// matches on. They differ exactly when a URL names a port.
	authority string
	machine   string
	user      string
	secret    string
}

// strippedPackages is one manager's list with its credentials lifted out.
type strippedPackages struct {
	// entries is what the manager is handed: the original list, with the
	// userinfo cut from every entry a credential was taken from.
	entries []string
	creds   []packageCredential
	// inlined names the hosts whose credential stayed in its entry — its
	// transport reads nothing this pass writes, no netrc can carry its value, or
	// another entry claims the same host with a different one — so the caller
	// can say so rather than leaving a silent exception behind.
	inlined []string
}

// packageURLRe finds a URL's scheme and authority wherever they sit in an
// entry, because the entry is not always the URL: pip's PEP 508 direct
// reference (`private-lib @ git+https://user:token@host/repo`) and npm's alias
// (`private-lib@https://user:token@host/pkg.tgz`) each nest one, and both are
// ordinary syntax their managers accept.
//
// It matches every URL, not only the ones carrying a credential, because both
// halves of the one-credential-per-host rule need them: a netrc line matches on
// the hostname alone and is therefore sent to every URL in the list naming that
// host, so an uncredentialed one is a host that would start receiving a secret
// it never had. The authority class excludes every character that ends an
// authority, so the match stops where a URL parser stops.
var packageURLRe = regexp.MustCompile(
	`([a-zA-Z][a-zA-Z0-9+.\-]*)://([^\s/?#]*)`)

// credentialSchemes are the transports whose fetcher reads a file this pass
// writes — measured, not assumed. Everything else keeps its credential where it
// is: `git+ssh` never used the URL's password in the first place (ssh takes
// none from a URL), and `hg+https`, `svn+https` and `bzr+http` authenticate
// from their own stores, so lifting the credential out would break an install
// that works today rather than protect one.
var credentialSchemes = map[string]bool{
	"http": true, "https": true, "git+http": true, "git+https": true,
}

// candidate is one credential the scan found, and where in which entry it sits.
type candidate struct {
	entry    int
	from, to int // the userinfo span, `@` excluded
	cred     packageCredential
}

// stripPackageCredentials lifts every URL credential out of a manager's list.
// The test is the URL's shape, not the manager's name: pip and npm are where a
// credentialed entry is common, not where it is possible.
//
// It scans before it cuts, because whether a credential can be moved at all
// depends on the others: a netrc line matches on the hostname alone, so it
// serves every URL in the list naming that host. Two entries naming one host
// with different credentials have no representation that keeps them apart, and
// one entry naming it with *no* credential would start receiving the other's —
// preemptively, since pip sends Basic from a netrc on the first request rather
// than on a 401. Either way the host keeps every credential it has inline.
func stripPackageCredentials(entries []string, manager packageManager) strippedPackages {
	out := strippedPackages{entries: make([]string, len(entries))}
	copy(out.entries, entries)

	// bare names a host some URL in this list reaches with no credential of its
	// own. It is a disagreement exactly as two different credentials are: one
	// netrc line serves every URL naming the host, so writing one would widen
	// the secret to an origin that never received it.
	bare := map[string]bool{}
	var found []candidate
	for i, e := range entries {
		for _, m := range packageURLRe.FindAllStringSubmatchIndex(e, -1) {
			scheme := strings.ToLower(e[m[2]:m[3]])
			authority := e[m[4]:m[5]]
			// A URL parser splits the userinfo on the authority's LAST `@`,
			// which is what makes a password containing one come out whole; the
			// same split here is what the cut below removes.
			at := strings.LastIndex(authority, "@")
			host := authority
			if at >= 0 {
				host = authority[at+1:]
			}
			// The span the regexp found is re-read by the URL parser, which is
			// what decodes the percent-encoding and splits an IPv6 literal from
			// its port. The scan locates; the parser interprets.
			u, err := url.Parse(scheme + "://" + authority + "/")
			if err != nil {
				if at >= 0 {
					// Go's userinfo grammar refuses characters a manager would
					// have accepted (`^`, a malformed `%` escape). The
					// credential stays where it is, and is named rather than
					// dropped in silence.
					out.inlined = append(out.inlined, host)
				}
				continue
			}
			machine := netrcMachine(u.Hostname())
			// Whether this URL's own fetch would read what the pass writes. It
			// decides both halves below: what may be lifted, and what may be
			// widened by something else being lifted.
			reads := credentialSchemes[scheme] && manager.netrc
			var secret string
			if u.User != nil {
				secret, _ = u.User.Password()
			}
			if secret == "" {
				// A bare `user@host`, a `user:@host`, or no userinfo at all:
				// nothing to move. But a netrc matches on the hostname alone,
				// so a credential written for another URL naming this host
				// would start being sent *here* — to a service that never had
				// it, over whatever scheme and port this URL names. A URL
				// carrying its own credential is unaffected, because its own
				// wins; one carrying none is not, so it counts as a
				// disagreement about the host.
				if reads && machine != "" {
					bare[machine] = true
				}
				continue
			}
			if machine == "" || !netrcSafe(u.User.Username()) || !netrcSafe(secret) || !reads {
				// An empty machine name would make the whole file unparseable
				// for pip and drop every other host's credential with it.
				out.inlined = append(out.inlined, host)
				continue
			}
			found = append(found, candidate{
				entry: i, from: m[4], to: m[4] + at,
				cred: packageCredential{
					authority: u.Host, machine: machine,
					user: u.User.Username(), secret: secret,
				},
			})
		}
	}

	// A hostname the list disagrees about keeps every credential on it inline.
	// What counts as disagreement is the credential, not the port it was named
	// on: one netrc line serves every port of a host, so the same user and
	// secret on two of them is one line, not a collision — while a *different*
	// credential, or no credential at all, is a URL the line would reach with a
	// secret meant for another.
	type login struct{ user, secret string }
	conflicted := map[string]bool{}
	for m := range bare {
		conflicted[m] = true
	}
	seen := map[string]login{}
	for _, c := range found {
		l := login{c.cred.user, c.cred.secret}
		if prev, ok := seen[c.cred.machine]; ok && prev != l {
			conflicted[c.cred.machine] = true
			continue
		}
		seen[c.cred.machine] = l
	}

	// Cut back to front, so an earlier span's index still names the same byte.
	credOf := map[packageCredential]bool{}
	for i := len(found) - 1; i >= 0; i-- {
		c := found[i]
		if conflicted[c.cred.machine] {
			out.inlined = append(out.inlined, c.cred.machine)
			continue
		}
		e := out.entries[c.entry]
		out.entries[c.entry] = e[:c.from] + e[c.to+1:] // the span and its `@`
		if !credOf[c.cred] {
			credOf[c.cred] = true
			out.creds = append(out.creds, c.cred)
		}
	}
	// Found back to front; written in the order the entries name them, which is
	// the order netrc resolves ties in.
	slices.Reverse(out.creds)
	return out
}

// netrcSafe reports whether a value can be written to a netrc as it is, which
// is the only way it may be written. A netrc value *can* be double-quoted —
// curl reads one, escapes included — but the writer must not, because pip's own
// fetcher reads the file through Python's `netrc` module and that module did not
// strip quotes before 3.11: measured, python 3.10 returns `"bot"` and
// `"s3cr3t"` where 3.12 returns `bot` and `s3cr3t`, so a quoted file makes pip
// send the quotes and the origin refuse. Ubuntu 22.04 and Debian bullseye ship
// that Python.
//
// So a value carrying anything a bare token cannot — whitespace, a quote, a
// backslash, a `#`, a control character — has no representation here and its
// entry keeps its credential, which is an exposure unchanged rather than an
// install broken.
func netrcSafe(v string) bool {
	return v != "" && !strings.ContainsFunc(v, func(r rune) bool {
		return r < 0x20 || r == 0x7f || r == ' ' || r == '\t' || r == '"' || r == '\\' || r == '#'
	})
}

// netrcMachine folds a hostname to the form a netrc consumer matches on: case
// is ignored there, and a trailing dot names the same host as no trailing dot,
// so `Registry.Example` and `registry.example.` must not read as two hosts with
// two credentials — curl takes the first case-insensitive match and would send
// one service the other's secret.
func netrcMachine(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// netrcFile is what git and pip's own fetcher read. A `machine` line matches on
// the hostname alone, port excluded, and case-insensitively — which is why the
// machine name is folded before it is compared or written. Values are bare;
// netrcSafe is what guarantees they can be.
func netrcFile(creds []packageCredential) []byte {
	var b strings.Builder
	written := map[string]bool{}
	for _, c := range creds {
		// One line per host. Two entries reaching one host on two ports are two
		// npmrc keys — that file is keyed by authority — and one netrc line,
		// because a machine line has no port to differ on. Writing the second
		// would be a line no consumer ever reaches.
		if written[c.machine] {
			continue
		}
		written[c.machine] = true
		fmt.Fprintf(&b, "machine %s\nlogin %s\npassword %s\n", c.machine, c.user, c.secret)
	}
	return []byte(b.String())
}

// npmrcFile is npm's fetcher's half. The secret rides as base64, so it carries
// what a netrc could not have; the username is written as it is, and a `#` in
// one would truncate the value — an install that fails to authenticate, which
// is loud, rather than a credential that leaks, which is not.
//
// No `always-auth`, which npm's older documentation asked for beside these
// keys: npm 11 authenticates from the pair alone and warns that the key is
// unknown, and npm 6 sends no credential for a non-registry fetch with or
// without it (both measured). So it buys a warning in one and nothing in the
// other — and an npm 6 image is the one shape this pass makes worse, which
// docs/self-hosted-security.md says rather than leaves to be discovered.
func npmrcFile(creds []packageCredential) []byte {
	var b strings.Builder
	for _, c := range creds {
		fmt.Fprintf(&b, "//%s/:username=%s\n//%s/:_password=%s\n",
			c.authority, c.user,
			c.authority, base64.StdEncoding.EncodeToString([]byte(c.secret)))
	}
	return []byte(b.String())
}

// packagesCredsDir is where one manager's credential files live for the length
// of its install. /tmp because it is writable in every hardening shape, and
// under a random name because the sandbox is agent-writable: a fixed path could
// be pre-created as a regular file to make the write fail. The install's own
// trap removes it.
func packagesCredsDir() string {
	var b [8]byte
	// crypto/rand.Read does not fail on a running system, and sandbox.TempName
	// reads it the same way: a collision would cost one install.
	_, _ = rand.Read(b[:])
	return "/tmp/.map-pkgcreds-" + hex.EncodeToString(b[:])
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
//
// credsDir, when a list carried a credential, is the scratch HOME the
// materialized files live in: the install reads them from there, and the trap
// removes them when the group ends of its own accord — including the
// preflight's 127, which is why the trap is set before it. It carries a path
// rather than a secret, so it is as argv-safe as the rest of the command.
//
// It is not the only removal, because it cannot be: a timed-out install is
// SIGKILLed by process group on both backends and no EXIT trap runs then, so
// installPackages asks for the directory again after the install returns. What
// neither survives is this executor dying in between; the directory then lives
// as long as the sandbox, and the next pass writes to a fresh random name.
func (m packageManager) command(entries []string, credsDir string) string {
	install := m.install(entries)
	body := m.preflight + "; " + install
	if credsDir != "" {
		q := shellQuote(credsDir)
		// `exit "$?"` ends the group on a builtin, carrying the status the
		// install left. Without it the group's last command is the install
		// itself, and a shell that replaces the subshell with it — bash 3.2
		// does, bash 5 does not, both measured — takes the EXIT trap with it and
		// the credentials outlive the install.
		body = "trap \"rm -rf " + q + "\" EXIT; " + m.preflight +
			"; export HOME=" + q + "; " + install + "; exit \"$?\""
	}
	return "set -o pipefail; { " + body + "; } 2>&1 | tail -c " +
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
		// The credential comes out before anything else looks at the list: the
		// digest is taken over the stripped form, which is what stops it being
		// an offline oracle for a weak credential (#599's second surface —
		// the event subtree it rides on is readable with an environment key,
		// while the config it digests needs a management key).
		stripped := stripPackageCredentials(entries, m)
		// Two digests, because they answer different questions. The sentinel
		// asks "is this the list I last tried", and a rotated credential is a
		// changed list — comparing the stripped form would let a corrected
		// credential inherit the exhausted attempt count of the broken one, and
		// never install. It stays inside the sandbox, where plan 40 already put
		// it. The published one rides on an event an environment key can read
		// while the config it digests needs a management key, so it is taken
		// over the stripped form: that is what stops it being an offline oracle
		// for a weak credential (#599's second surface).
		digest := packagesDigest(entries)
		published := packagesDigest(stripped.entries)
		rec, seen := recs[m.name]
		// A list this sandbox tried before and has since been given in another
		// form, credential included. Two lists differing only in a credential
		// publish the same digest, so the emission's own dedupe would suppress
		// the second — this is what tells it not to.
		//
		// A *missing* record is deliberately not "changed": the refusal
		// branches below emit and `continue` without writing a sentinel, so a
		// stored invalid list has no record on any pass. Reading that as a
		// changed list would skip the dedupe every time and append the same
		// exhausted error on every tool call, forever. With no record there is
		// nothing to have changed from, and the query is the right answer.
		changed := seen && rec.Digest != digest
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
				"an entry is empty or begins with '-', which a package manager reads as an option rather than a package",
				published, true, changed)
			continue
		}
		// The assembled command is one execve argument, which Linux caps near
		// 128 KiB. `go`'s per-entry amplification or a row stored before the
		// API's byte cap existed can exceed it, faulting the install at exec
		// startup and reclaim-looping the item. Refused terminally here, before
		// the probe, exactly like an invalid entry.
		credsDir := ""
		if len(stripped.creds) > 0 {
			credsDir = packagesCredsDir()
		}
		cmd := m.command(stripped.entries, credsDir)
		if len(cmd) > maxInstallCommandBytes {
			failed++
			recordPackageInstalled(ctx, m.name, packageOutcomeInvalid)
			e.emitPackageInstallError(ctx, sid, m.name, packageReasonInvalid,
				fmt.Sprintf("the assembled install command is %d bytes, over the %d-byte exec-argument limit", len(cmd), maxInstallCommandBytes),
				published, true, changed)
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
				e.emitPackageInstallError(ctx, sid, "", reason, packageProbeMessages[reason], "", true, false)
				return nil
			}
		}
		if len(stripped.inlined) > 0 {
			// Said rather than silently excepted: these entries keep the
			// exposure they have today, and nothing else about the pass tells
			// anyone which ones.
			slog.WarnContext(ctx, "a package credential this platform cannot move out of band stayed in the install command",
				"session_id", sid, "manager", m.name, "hosts", stripped.inlined)
		}
		// The install's own trap removes the scratch directory when the group
		// ends of its own accord, and this removes it when nothing ran the trap:
		// a timed-out install is SIGKILLed by process group on both backends,
		// which no EXIT trap survives, and a write or an exec can fail before
		// the shell is ever reached. Best effort — a sandbox that cannot answer
		// this can no longer be cleaned by anything, and the directory dies with
		// it.
		removeCreds := func() {
			if credsDir == "" {
				return
			}
			// Its own budget, not the install's: two calls each carrying
			// PackageInstallTimeout would put twice the stall floor's longest
			// single step inside one silent interval, which is the reclaim loop
			// #383 is about. progress() first, for the same reason.
			progress()
			_, _ = sb.Exec(ctx, sandbox.ExecRequest{
				Command: "rm -rf " + shellQuote(credsDir),
				Timeout: packagesCredsRemoveTimeout,
			})
		}
		if credsDir != "" {
			// 0600, because a zero Mode lands 0644 and these two files are the
			// credential. It does not close the same-user read the design
			// concedes — the install and the agent share a root — but an image
			// with any other user in it no longer has these readable by
			// default, and the install owns them either way.
			files := []sandbox.FileWrite{
				{Path: credsDir + "/.netrc", Data: netrcFile(stripped.creds), Mode: 0o600},
			}
			if m.npmrc {
				files = append(files, sandbox.FileWrite{
					Path: credsDir + "/.npmrc", Data: npmrcFile(stripped.creds), Mode: 0o600,
				})
			}
			// A write that fails faults the item exactly as a failed Exec does.
			// Falling back to the credential in argv instead would answer a
			// sandbox-side failure by widening the exposure this pass exists to
			// close.
			if err := sb.WriteFiles(ctx, files); err != nil {
				removeCreds()
				return err
			}
		}
		progress()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Command: cmd,
			Timeout: e.cfg.PackageInstallTimeout,
		})
		removeCreds()
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
				published, rec.Attempts >= packageInstallAttempts, changed)
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
// entry), which redactURL — anchored on `http(s)://` — does not reach. Three
// residuals remain: a query credential in a non-http URL; one whose `http(s)://` scheme
// the in-shell `tail -c` cut off; and a run of trailing punctuation
// (`.,:;!?)]}"'`) redactURL reattaches to keep the sentence readable, which
// leaks a credential's trailing-punctuation suffix (its whole value only if the
// credential is nothing but those characters).
//
// #599 narrowed all three rather than closing them: a credential lifted out
// before the command is assembled is one the manager cannot echo, but every
// entry shape stripPackageCredentials leaves alone still reaches it, and for
// those this redaction is what stands between the credential and the log.
func packageMessage(out string) string {
	msg := urlInText.ReplaceAllStringFunc(out, redactURL)
	msg = anySchemeUserinfoRe.ReplaceAllString(msg, "$1***@")
	msg = toolset.SanitizeText(msg)
	return toolset.TruncateRunes(msg, maxPackageMessage)
}

// anySchemeUserinfoRe matches the userinfo of a URL of any scheme, for the
// non-http schemes redactURL leaves alone. The class excludes every character
// that ends an authority — `/`, `?` and `#` — so the greedy `+` runs to the
// LAST `@` before the first of them: a password containing `@`
// (`user:p@ss@host`) is dropped whole, while an `@` in a pathless query or
// fragment (`scheme://host?owner=a@b`) ends the authority before the `@` and is
// left alone, mirroring how a URL parser splits userinfo from host.
var anySchemeUserinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s/?#]+@`)

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
// rather than one the first list's history suppresses.
//
// `changed` is what keeps that last property true now that the published digest
// is stripped of credentials (#599): two lists differing only in a credential
// publish the same digest, so a rotated credential's failures would be
// suppressed by the broken credential's history and a client watching would see
// silence where it should see the new attempt. The caller knows the list
// changed — the sentinel compares the list as written — and says so, and a
// changed list is emitted without consulting the history at all.
//
// Otherwise the list identity is the digest, mirroring the clone error's
// (resource_id, reason) key. The work item's lease already makes this executor
// the session's single writer, so the check-then-append needs no further
// guarding, and emission is best effort:
// failing to record the error must not turn a tolerated install failure into a
// failed run (the emitRepoCloneError precedent).
//
// The entry list itself is deliberately not carried — only its digest: the
// environment's config already holds the list for a management key, while the
// events subtree is also readable with an environment key, and the manager plus
// the output tail name what failed (decision 4). digest is empty for the
// sandbox-level reasons, which are the sandbox's rather than a list's.
func (e *Executor) emitPackageInstallError(ctx context.Context, sid domain.ID, manager, reason, message, digest string, exhausted, changed bool) {
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
	if !changed {
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
