package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// setPackages rewrites the session's environment config packages block, the
// way a client's POST /v1/environments leaves it.
func (h *harness) setPackages(t *testing.T, pkgs map[string][]string) {
	t.Helper()
	raw, err := json.Marshal(pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE environments SET config = jsonb_set(config, '{packages}', $2::jsonb) WHERE id = $1`,
		h.envID.String(), raw); err != nil {
		t.Fatalf("set environment packages: %v", err)
	}
}

// installCmds are the package-install commands the sandbox was asked to run,
// in order — every other pass's exec, and the probe, filtered out by the one
// prefix only these commands carry.
func installCmds(sb *fakeSandbox) []string {
	var out []string
	for _, c := range sb.cmds {
		if strings.HasPrefix(c, "set -o pipefail; { ") {
			out = append(out, c)
		}
	}
	return out
}

// probes counts the sandbox probes the install pass ran.
func probes(sb *fakeSandbox) int {
	n := 0
	for _, c := range sb.cmds {
		if c == packagesProbeCommand {
			n++
		}
	}
	return n
}

// sentinel decodes the sandbox's /tmp/.map-packages, or nil when none was
// written.
func sentinel(t *testing.T, sb *fakeSandbox) map[string]packageRecord {
	t.Helper()
	raw, ok := sb.files[packagesSentinelPath]
	if !ok {
		return nil
	}
	var recs map[string]packageRecord
	if err := json.Unmarshal([]byte(raw), &recs); err != nil {
		t.Fatalf("decode the sentinel %q: %v", raw, err)
	}
	return recs
}

// packageErrors are the session.error events of the install variant, in order.
func (h *harness) packageErrors(t *testing.T) []map[string]any {
	t.Helper()
	return h.errorsOfType(t, packageInstallErrorType)
}

// wrap is the shape every manager's command has: the preflight and the install
// in one group whose combined output is tailed, under pipefail so the group's
// status survives the pipe.
func wrap(inner string) string {
	return "set -o pipefail; { " + inner + "; } 2>&1 | tail -c 8192"
}

// TestInstallsEveryManagerInTheReferencesOrder pins both halves of decision 3
// at once: the alphabetical order the docs promise a client whose apt list must
// land before its pip list, and the exact command each manager is handed —
// including the choices that are ours (the /usr/local roots, the pip
// environment overrides, apt's dpkg repair, and `go install`'s per-entry
// invocation with its @latest suffix).
func TestInstallsEveryManagerInTheReferencesOrder(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{
		"apt":   {"jq"},
		"cargo": {"hyperfine@1.18.0"},
		"gem":   {"rails:7.1.0"},
		// One pinned entry and one unpinned: only the second gets @latest.
		"go":  {"golang.org/x/tools/cmd/goimports@latest", "github.com/foo/bar"},
		"npm": {"express@4.18.0"},
		"pip": {"sqlalchemy==2.0.30"},
	})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	want := []string{
		wrap("command -v apt-get >/dev/null 2>&1 || exit 127; export DEBIAN_FRONTEND=noninteractive; " +
			"dpkg --configure -a; apt-get -o APT::Sandbox::User=root update -q && " +
			"apt-get -o APT::Sandbox::User=root install -y -q 'jq'"),
		wrap("command -v cargo >/dev/null 2>&1 || exit 127; cargo install --root /usr/local 'hyperfine@1.18.0'"),
		wrap("command -v gem >/dev/null 2>&1 || exit 127; gem install --no-document 'rails:7.1.0'"),
		wrap("command -v go >/dev/null 2>&1 || exit 127; " +
			"GOBIN=/usr/local/bin go install 'golang.org/x/tools/cmd/goimports@latest' && " +
			"GOBIN=/usr/local/bin go install 'github.com/foo/bar@latest'"),
		wrap("command -v npm >/dev/null 2>&1 || exit 127; npm install -g 'express@4.18.0'"),
		wrap("python3 -m pip --version >/dev/null 2>&1 || exit 127; " +
			"PIP_BREAK_SYSTEM_PACKAGES=1 PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_INPUT=1 " +
			"python3 -m pip install 'sqlalchemy==2.0.30'"),
	}
	got := installCmds(sb)
	if len(got) != len(want) {
		t.Fatalf("install commands = %d, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
	// One probe for the whole pass, not one per manager.
	if n := probes(sb); n != 1 {
		t.Errorf("probes = %d, want 1 (once, before the first manager that has work)", n)
	}
	recs := sentinel(t, sb)
	if len(recs) != 6 {
		t.Fatalf("sentinel records = %d, want one per manager: %+v", len(recs), recs)
	}
	for name, rec := range recs {
		if !rec.Installed || rec.Attempts != 1 {
			t.Errorf("sentinel[%s] = %+v, want installed after one attempt", name, rec)
		}
	}
}

// TestPackageEntriesAreOneArgvMemberWhateverTheyContain: an entry is passed to
// its manager verbatim, which is only true if quoting makes it one argument —
// a name carrying a quote and spaces must not become three arguments, or an
// injection.
func TestPackageEntriesAreOneArgvMemberWhateverTheyContain(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"apt": {"it's a package", "; rm -rf /"}})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	got := installCmds(sb)
	if len(got) != 1 {
		t.Fatalf("install commands = %d, want 1", len(got))
	}
	// Anchored at `install`, not `apt-get`, so the assertion is about the entries
	// being one argv member each and survives options between the two (the
	// APT::Sandbox::User=root the capability drop needs).
	if want := `install -y -q 'it'\''s a package' '; rm -rf /'`; !strings.Contains(got[0], want) {
		t.Errorf("command = %s, want it to contain %s", got[0], want)
	}
}

// TestASettledManagerIsNotReinstalled is decision 2's sentinel: a sandbox's
// later provisions — every tool_exec after the first — must not repeat what is
// already installed, and a changed list must start over.
func TestASettledManagerIsNotReinstalled(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"apt": {"jq"}})
	h.suspend(t, writeUse("a.txt", "1"))
	h.stepOnce(t)
	if n := len(installCmds(sb)); n != 1 {
		t.Fatalf("after the first item, install commands = %d, want 1", n)
	}

	h.suspend(t, writeUse("b.txt", "2"))
	h.stepOnce(t)
	if n := len(installCmds(sb)); n != 1 {
		t.Errorf("after a second item on the same list, install commands = %d, want still 1", n)
	}
	// The probe is skipped too: nothing is left for it to gate.
	if n := probes(sb); n != 1 {
		t.Errorf("probes = %d, want 1 (a settled pass runs no exec at all)", n)
	}

	h.setPackages(t, map[string][]string{"apt": {"jq", "curl"}})
	h.suspend(t, writeUse("c.txt", "3"))
	h.stepOnce(t)
	got := installCmds(sb)
	if len(got) != 2 {
		t.Fatalf("after the list changed, install commands = %d, want 2", len(got))
	}
	if !strings.Contains(got[1], "'jq' 'curl'") {
		t.Errorf("the reinstall = %s, want the changed list", got[1])
	}
	if rec := sentinel(t, sb)["apt"]; rec.Attempts != 1 {
		t.Errorf("attempts after a changed list = %d, want a fresh count", rec.Attempts)
	}
}

// failInstall makes every install command fail with the given result, leaving
// the probe and every other exec alone.
func failInstall(res sandbox.ExecResult) func(sandbox.ExecRequest) *sandbox.ExecResult {
	return func(req sandbox.ExecRequest) *sandbox.ExecResult {
		if strings.HasPrefix(req.Command, "set -o pipefail; { ") {
			return &res
		}
		return nil
	}
}

// TestAFailedInstallSurfacesAndTheSessionRunsOn is decision 4's core: the
// failure is a session.error the client can read, the run continues, and the
// sentinel records the attempt without claiming an install.
func TestAFailedInstallSurfacesAndTheSessionRunsOn(t *testing.T) {
	sb := &fakeSandbox{execHook: failInstall(sandbox.ExecResult{
		ExitCode: 100,
		Stdout:   "E: Unable to locate package nosuchpkg\n",
	})}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"apt": {"nosuchpkg"}})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	errs := h.packageErrors(t)
	if len(errs) != 1 {
		t.Fatalf("package errors = %d, want 1: %+v", len(errs), errs)
	}
	e := errs[0]
	if e["manager"] != "apt" || e["reason"] != packageReasonFailed {
		t.Errorf("error = %+v, want manager apt and reason failed", e)
	}
	if msg, _ := e["message"].(string); !strings.Contains(msg, "Unable to locate package nosuchpkg") {
		t.Errorf("message = %q, want the manager's own output tail", msg)
	}
	if rs, _ := e["retry_status"].(map[string]any); rs["type"] != "retrying" {
		t.Errorf("retry_status = %+v, want retrying on the first of three attempts", rs)
	}
	if rec := sentinel(t, sb)["apt"]; rec.Installed || rec.Attempts != 1 {
		t.Errorf("sentinel = %+v, want one attempt and no install", rec)
	}
	// The session ran on: the tool answered and the brain was woken.
	if n := len(h.types(t, "agent.tool_result")); n != 1 {
		t.Errorf("tool results = %d, want the run to have continued past the failed install", n)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 1 {
		t.Errorf("model_turn = %d, want 1 (resume)", got)
	}
}

// TestARepeatedFailureIsOneEventUntilItIsExhausted pins the dedupe and the
// three-attempt cap together, because they are one behavior: a client watching
// a session must see one event per distinct (manager, reason, retry_status),
// and an install that will never work must stop costing a budget before every
// tool call.
func TestARepeatedFailureIsOneEventUntilItIsExhausted(t *testing.T) {
	sb := &fakeSandbox{execHook: failInstall(sandbox.ExecResult{ExitCode: 100, Stdout: "boom\n"})}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"apt": {"nosuchpkg"}})

	for i := 1; i <= 4; i++ {
		h.suspend(t, writeUse(fmt.Sprintf("f%d.txt", i), "x"))
		h.stepOnce(t)
	}

	if n := len(installCmds(sb)); n != 3 {
		t.Errorf("install attempts = %d, want 3 — the fourth item must run none", n)
	}
	errs := h.packageErrors(t)
	if len(errs) != 2 {
		t.Fatalf("package errors = %d, want 2 (the retrying one, then the exhausted one): %+v", len(errs), errs)
	}
	for i, want := range []string{"retrying", "exhausted"} {
		if rs, _ := errs[i]["retry_status"].(map[string]any); rs["type"] != want {
			t.Errorf("error %d retry_status = %+v, want %s", i, rs, want)
		}
	}
	if rec := sentinel(t, sb)["apt"]; rec.Attempts != 3 || rec.Installed {
		t.Errorf("sentinel = %+v, want three spent attempts and no install", rec)
	}
}

// TestADifferentFailingListIsNotSuppressedByTheFirst is the dedupe's list
// identity: a client that changes a manager's list after the first list
// exhausted must see the new list fail, not silence. The dedupe keys on the
// list digest as well as (manager, reason, retry_status), the way the clone
// error keys on its resource id — without that, the first list's history would
// swallow every later failure of the same manager and reason.
func TestADifferentFailingListIsNotSuppressedByTheFirst(t *testing.T) {
	sb := &fakeSandbox{execHook: failInstall(sandbox.ExecResult{ExitCode: 100, Stdout: "boom\n"})}
	h := newHarness(t, sb)

	// First list: fail it to exhaustion (retrying, then exhausted).
	h.setPackages(t, map[string][]string{"apt": {"nosuchpkg"}})
	for i := 1; i <= 3; i++ {
		h.suspend(t, writeUse(fmt.Sprintf("a%d.txt", i), "x"))
		h.stepOnce(t)
	}
	if n := len(h.packageErrors(t)); n != 2 {
		t.Fatalf("after the first list: package errors = %d, want 2 (retrying, exhausted)", n)
	}

	// A different list, same manager and same failure reason: a fresh attempt
	// with its own count, and a fresh event rather than one the first suppresses.
	h.setPackages(t, map[string][]string{"apt": {"othertypo"}})
	h.suspend(t, writeUse("b.txt", "y"))
	h.stepOnce(t)

	errs := h.packageErrors(t)
	if len(errs) != 3 {
		t.Fatalf("after the second list: package errors = %d, want 3 — the new list's failure must not be deduped away: %+v", len(errs), errs)
	}
	if rs, _ := errs[2]["retry_status"].(map[string]any); rs["type"] != "retrying" {
		t.Errorf("third error retry_status = %+v, want retrying (the second list's first attempt)", rs)
	}
	if d, _ := errs[2]["packages_digest"].(string); d != packagesDigest([]string{"othertypo"}) {
		t.Errorf("third error packages_digest = %q, want the second list's digest", d)
	}
}

// TestInstallFailureReasons: the classification a client reads. TimedOut is
// asked before the exit code, because a killed command's code may be the
// kill's or one it chose for itself.
func TestInstallFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  sandbox.ExecResult
		want string
	}{
		{"timeout", sandbox.ExecResult{TimedOut: true, ExitCode: 137, Stdout: "Get:1 …\n"}, packageReasonTimeout},
		{"manager missing", sandbox.ExecResult{ExitCode: 127}, packageReasonManagerMissing},
		{"failed", sandbox.ExecResult{ExitCode: 1, Stdout: "no\n"}, packageReasonFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &fakeSandbox{execHook: failInstall(tc.res)}
			h := newHarness(t, sb)
			h.setPackages(t, map[string][]string{"pip": {"cowsay==6.1"}})
			h.suspend(t, writeUse("out.txt", "hello"))
			h.stepOnce(t)

			errs := h.packageErrors(t)
			if len(errs) != 1 {
				t.Fatalf("package errors = %d, want 1: %+v", len(errs), errs)
			}
			if errs[0]["reason"] != tc.want {
				t.Errorf("reason = %v, want %s", errs[0]["reason"], tc.want)
			}
		})
	}
}

// TestAnInvalidEntryIsRefusedWithoutRunningAnything is decision 6's executor
// half: the API refuses these at create, but a row stored before that rule must
// be refused here rather than handed to a manager that would read it as an
// option. Exhausted from the first, because nothing in the session's life makes
// the entry acceptable.
func TestAnInvalidEntryIsRefusedWithoutRunningAnything(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"pip": {"--index-url=http://evil.example/simple"}})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	if got := installCmds(sb); len(got) != 0 {
		t.Errorf("install commands = %v, want none — a refused entry never reaches a manager", got)
	}
	if n := probes(sb); n != 0 {
		t.Errorf("probes = %d, want 0: a pass with nothing to run must run nothing", n)
	}
	errs := h.packageErrors(t)
	if len(errs) != 1 {
		t.Fatalf("package errors = %d, want 1: %+v", len(errs), errs)
	}
	if errs[0]["reason"] != packageReasonInvalid || errs[0]["manager"] != "pip" {
		t.Errorf("error = %+v, want reason invalid on pip", errs[0])
	}
	if rs, _ := errs[0]["retry_status"].(map[string]any); rs["type"] != "exhausted" {
		t.Errorf("retry_status = %+v, want exhausted from the first", rs)
	}
}

// TestTheProbeRefusesASandboxThatCannotInstall is decision 7: every manager
// writes under /usr or /var, so a non-root or read-only sandbox is refused once
// — with no manager on the event, because the fault is the sandbox's — rather
// than six budgets' worth of "Permission denied".
func TestTheProbeRefusesASandboxThatCannotInstall(t *testing.T) {
	for _, reason := range []string{packageReasonNotRoot, packageReasonReadOnly} {
		t.Run(reason, func(t *testing.T) {
			sb := &fakeSandbox{probeOut: reason}
			h := newHarness(t, sb)
			h.setPackages(t, map[string][]string{"apt": {"jq"}, "pip": {"cowsay==6.1"}})
			h.suspend(t, writeUse("out.txt", "hello"))
			h.stepOnce(t)

			if got := installCmds(sb); len(got) != 0 {
				t.Errorf("install commands = %v, want none", got)
			}
			errs := h.packageErrors(t)
			if len(errs) != 1 {
				t.Fatalf("package errors = %d, want exactly 1 for the sandbox itself: %+v", len(errs), errs)
			}
			if errs[0]["reason"] != reason {
				t.Errorf("reason = %v, want %s", errs[0]["reason"], reason)
			}
			if _, ok := errs[0]["manager"]; ok {
				t.Errorf("error = %+v, want no manager key: the refusal is the sandbox's, not a manager's", errs[0])
			}
			if rs, _ := errs[0]["retry_status"].(map[string]any); rs["type"] != "exhausted" {
				t.Errorf("retry_status = %+v, want exhausted", rs)
			}
			if len(sentinel(t, sb)) != 0 {
				t.Errorf("sentinel = %+v, want none written: nothing settled", sentinel(t, sb))
			}
		})
	}
}

// TestAnUnrecognizedProbeAnswerInstallsAnyway: the probe exists to save six
// timeouts, not to invent a reason for the wire. A shell so broken it printed
// something else proceeds, and the install's own failure is then the honest
// diagnosis.
func TestAnUnrecognizedProbeAnswerInstallsAnyway(t *testing.T) {
	sb := &fakeSandbox{probeOut: "bash: id: command not found"}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"apt": {"jq"}})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	if n := len(installCmds(sb)); n != 1 {
		t.Errorf("install commands = %d, want 1", n)
	}
	if errs := h.packageErrors(t); len(errs) != 0 {
		t.Errorf("package errors = %+v, want none: an unreadable probe is not a reason", errs)
	}
}

// TestTheFailureMessageIsSanitizedAndRedacted: this is the one session.error
// this platform writes whose text is sandbox-controlled, so both of its guards
// are pinned here. A NUL would fault the jsonb append and reclaim-loop the item;
// a pip or npm entry may legitimately be a URL carrying a credential, and a
// manager echoes its arguments.
func TestTheFailureMessageIsSanitizedAndRedacted(t *testing.T) {
	sb := &fakeSandbox{execHook: failInstall(sandbox.ExecResult{
		ExitCode: 1,
		Stdout:   "ERROR\x00 could not install https://deploy:s3cr3t@pkgs.example.com/simple/x\n",
	})}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"pip": {"x"}})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	errs := h.packageErrors(t)
	if len(errs) != 1 {
		t.Fatalf("package errors = %d, want 1: %+v", len(errs), errs)
	}
	msg, _ := errs[0]["message"].(string)
	if strings.ContainsRune(msg, 0) {
		t.Errorf("message %q still carries a NUL", msg)
	}
	if strings.Contains(msg, "s3cr3t") {
		t.Errorf("message %q carries the credential the manager echoed", msg)
	}
	if want := "https://pkgs.example.com"; !strings.Contains(msg, want) {
		t.Errorf("message = %q, want it to keep the URL reduced to %q", msg, want)
	}
}

// TestPackageMessageRedactsEveryCredentialForm pins packageMessage directly,
// because the userinfo-only redactor it replaced leaked two shapes a manager
// really echoes: a password containing '@' (redacted only up to the first '@')
// and a credential in the query rather than the userinfo (`?token=`, a pip/npm
// convention). Reducing every URL to scheme://host closes both.
func TestPackageMessageRedactsEveryCredentialForm(t *testing.T) {
	for _, tc := range []struct {
		name, in, leak, keep string
	}{
		{"userinfo", "fetching https://deploy:s3cr3t@pkgs.example.com/x failed", "s3cr3t", "https://pkgs.example.com"},
		{"password with @", "auth https://user:p@sSWORD@host.example/x 401", "sSWORD", "https://host.example"},
		{"query token", "GET https://host.example/simple?token=SECRETTOK 403", "SECRETTOK", "https://host.example"},
		{"non-http scheme userinfo", "cloning git+ssh://deploy:KEY123@repo.example/x.git failed", "KEY123", "git+ssh://***@repo.example/x.git"},
		{"nul", "boom\x00tail", "\x00", "boomtail"},
		{"benign url kept as host", "could not reach https://pypi.org/simple/", "", "https://pypi.org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := packageMessage(tc.in)
			if tc.leak != "" && strings.Contains(got, tc.leak) {
				t.Errorf("packageMessage(%q) = %q, still leaks %q", tc.in, got, tc.leak)
			}
			if !strings.Contains(got, tc.keep) {
				t.Errorf("packageMessage(%q) = %q, want it to keep %q", tc.in, got, tc.keep)
			}
		})
	}
}

// TestABackendFaultDuringInstallFaultsTheItem: an Exec error is the sandbox
// failing the caller rather than a command failing — the sandbox gone, the
// context cancelled — so it faults the item exactly as a provision failure
// does, leaving the item reclaimable and nothing committed.
func TestABackendFaultDuringInstallFaultsTheItem(t *testing.T) {
	boom := errors.New("daemon unreachable")
	sb := &fakeSandbox{execErr: boom, execErrOn: "apt-get"}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"apt": {"jq"}})
	h.suspend(t, writeUse("out.txt", "hello"))

	faulted := h.stepExpectingFault(t)
	if !errors.Is(faulted, boom) {
		t.Errorf("fault = %v, want the backend error", faulted)
	}
	if n := len(h.types(t, "agent.tool_result")); n != 0 {
		t.Errorf("tool results = %d, want none: a faulted item commits nothing", n)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 1 {
		t.Errorf("tool_exec live = %d, want 1 (still claimable after the lease lapses)", got)
	}
}

// TestAnEmptyPackagesConfigRunsNothing: every stored cloud config carries all
// six lists, empty or not, so the map being present says nothing — what decides
// is whether any list has entries.
func TestAnEmptyPackagesConfigRunsNothing(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"apt": {}, "cargo": {}, "gem": {}, "go": {}, "npm": {}, "pip": {}})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	if got := installCmds(sb); len(got) != 0 {
		t.Errorf("install commands = %v, want none", got)
	}
	if n := probes(sb); n != 0 {
		t.Errorf("probes = %d, want 0", n)
	}
	if len(sentinel(t, sb)) != 0 {
		t.Errorf("sentinel written for a package-less session: %+v", sentinel(t, sb))
	}
}

// TestTheGradingHarvestInstallsNothing: the harvest reads the sandbox and runs
// no tools, so repeating the install would spend up to six budgets on a pass
// that only lists and reads files (decision 2).
func TestTheGradingHarvestInstallsNothing(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"apt": {"jq"}})
	h.seedOutcome(t, domain.OutcomeResultEvaluating)
	h.enqueueHarvest(t)
	h.stepOnce(t)

	if got := installCmds(sb); len(got) != 0 {
		t.Errorf("install commands = %v, want none on the harvest's own provision", got)
	}
	if n := probes(sb); n != 0 {
		t.Errorf("probes = %d, want 0", n)
	}
}

// TestPackagesInstallBeforeSkillsMaterialize: the pass belongs to
// provisionSandbox, which runs before every materializer — so a skill whose
// SKILL.md calls a listed package finds it. The sandbox's write count at
// install time is the proof: nothing writes into the sandbox before the
// materializers do.
func TestPackagesInstallBeforeSkillsMaterialize(t *testing.T) {
	var writesAtInstall = -1
	sb := &fakeSandbox{}
	sb.execHook = func(req sandbox.ExecRequest) *sandbox.ExecResult {
		if writesAtInstall < 0 && strings.HasPrefix(req.Command, "set -o pipefail; { ") {
			writesAtInstall = sb.writes
		}
		return nil
	}
	h := newHarness(t, sb)
	h.seedSkill(t, "skill_pkg", "1", "helper", map[string]string{"SKILL.md": "# helper"})
	h.refSkills(t, [2]string{"skill_pkg", "1"})
	h.setPackages(t, map[string][]string{"apt": {"jq"}})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	if writesAtInstall != 0 {
		t.Errorf("the sandbox had %d writes when the install ran, want 0 — the install must precede every materializer", writesAtInstall)
	}
	// And the skills pass still landed, so the ordering claim is about a real
	// materialization rather than one that never happened.
	if _, ok := sb.files["/workspace/skills/helper/SKILL.md"]; !ok {
		t.Error("the skill did not materialize; the ordering assertion above proves nothing")
	}
}

// TestTheSentinelIsNotPreservedAcrossACheckpoint is the other half of decision
// 2's "a restored sandbox installs again". A restore builds a FRESH container,
// so the reinstall rests entirely on the sentinel not travelling with the
// checkpoint: /tmp is deliberately outside the preserved roots (plan 24 D5),
// and a sentinel moved under one of them would silently make a restored
// session skip an install it no longer has.
func TestTheSentinelIsNotPreservedAcrossACheckpoint(t *testing.T) {
	e := &Executor{cfg: Config{}.withDefaults()}
	for _, root := range e.checkpointRoots(domain.NewID(domain.PrefixSession)) {
		if packagesSentinelPath == root || strings.HasPrefix(packagesSentinelPath, root+"/") {
			t.Errorf("the sentinel %s lives under the preserved root %s; a restored sandbox would skip its install",
				packagesSentinelPath, root)
		}
	}
}

// TestTheInstallRunsInsideProvisionSandbox pins the call site decision 2 rests
// on: the pass is inside provisionSandbox, which holds the session advisory
// lock, so a reclaiming executor waits on the lapsed holder's install rather
// than racing its apt-get for the dpkg lock.
func TestTheInstallRunsInsideProvisionSandbox(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)

	got, err := h.exec.provisionSandbox(context.Background(), h.sid,
		sessionRun{packages: map[string][]string{"apt": {"jq"}}}, func() {})
	if err != nil {
		t.Fatalf("provisionSandbox: %v", err)
	}
	if got != sb {
		t.Fatalf("provisionSandbox returned %v, want the fake sandbox", got)
	}
	if n := len(installCmds(sb)); n != 1 {
		t.Errorf("install commands = %d, want provisionSandbox itself to have run the install", n)
	}
}
