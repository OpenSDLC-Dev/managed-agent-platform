package shell_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/shell"
)

const testImage = "debian:stable-slim"

// provision gives the whole test one real container; each subtest scopes its own
// shell with a fresh session id, so they share the container but not its state.
// A missing daemon is a hard failure, as with the other suites.
func provision(t *testing.T) sandbox.Sandbox {
	t.Helper()
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("shell tests require Docker: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Workdir:    "/workspace",
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})
	return sb
}

// newShell is a subtest's shell, scoped to a fresh session; each call gets a
// fresh tool id. It also returns the session, so a test can issue a restart
// against the same shell.
func newShell(t *testing.T, sb sandbox.Sandbox) (func(cmd string, timeout time.Duration) shell.Result, domain.ID) {
	t.Helper()
	session := domain.NewID("sesn")
	run := func(cmd string, timeout time.Duration) shell.Result {
		t.Helper()
		res, err := shell.Run(context.Background(), sb, session, domain.NewID("sevt"),
			shell.Request{Command: cmd, Timeout: timeout})
		if err != nil {
			t.Fatalf("shell.Run(%q): %v", cmd, err)
		}
		return res
	}
	return run, session
}

func TestShell(t *testing.T) {
	sb := provision(t)

	t.Run("StatePersistsAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("export X=1; cd /tmp; f() { echo hi; }", 0)
		got := sh(`echo "$X:$(pwd):$(f)"`, 0)
		if strings.TrimSpace(got.Stdout) != "1:/tmp:hi" {
			t.Errorf("stdout = %q, want 1:/tmp:hi (env/cwd/function did not persist)", got.Stdout)
		}
		if got.ExitCode != 0 {
			t.Errorf("exit = %d, want 0", got.ExitCode)
		}
	})

	t.Run("OptionsPersistAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("set -o pipefail; shopt -s nullglob", 0)
		got := sh(`[[ -o pipefail ]] && echo PIPEFAIL_ON; shopt -q nullglob && echo NULLGLOB_ON`, 0)
		if !strings.Contains(got.Stdout, "PIPEFAIL_ON") {
			t.Errorf("set -o option did not persist; stdout=%q", got.Stdout)
		}
		if !strings.Contains(got.Stdout, "NULLGLOB_ON") {
			t.Errorf("shopt option did not persist; stdout=%q", got.Stdout)
		}
	})

	// errexit is the option the snapshot is most likely to lose, because the save
	// has to turn it off before it can safely write: the option state must be
	// captured before that happens, or `set -e` can never carry.
	t.Run("ErrexitPersistsAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("set -o errexit", 0)
		got := sh(`[[ -o errexit ]] && echo ERREXIT_ON || echo ERREXIT_OFF`, 0)
		if !strings.Contains(got.Stdout, "ERREXIT_ON") {
			t.Errorf("set -e did not persist; stdout=%q", got.Stdout)
		}
	})

	// Only exported variables carry. Nothing in `declare` separates a user's plain
	// variables from bash's own internals, so the snapshot draws the line at
	// `export` — and the line has to hold in both directions.
	t.Run("PlainVariablesDoNotCarryButExportedOnesDo", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("PLAIN=here; export EXPORTED=here", 0)
		got := sh(`echo "[${PLAIN:-gone}][${EXPORTED:-gone}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[gone][here]" {
			t.Errorf("stdout = %q, want [gone][here] — the snapshot draws the line at export", got.Stdout)
		}
	})

	// Traps do not carry. The next call is a fresh bash whose only EXIT trap is
	// the template's own save; a trap the command installed is not in it.
	t.Run("TrapsDoNotCarryAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh(`trap 'echo BYE' EXIT; trap 'echo HUP' HUP`, 0)
		got := sh("trap -p", 0)
		if strings.Contains(got.Stdout, "BYE") || strings.Contains(got.Stdout, "HUP") {
			t.Errorf("trap -p = %q — a command's traps carried into the next call", got.Stdout)
		}
	})

	t.Run("AliasesPersistAcrossCalls", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("shopt -s expand_aliases; alias greet='echo aliased'", 0)
		got := sh("greet", 0)
		if strings.TrimSpace(got.Stdout) != "aliased" {
			t.Errorf("stdout = %q, want aliased (alias did not persist)", got.Stdout)
		}
	})

	// A readonly export must be carried as a readonly export, not dropped. The
	// snapshot may only skip the names a fresh bash makes readonly for itself.
	t.Run("ReadonlyExportPersists", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("declare -rx TOKEN=secret", 0)
		got := sh(`echo "[${TOKEN:-unset}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[secret]" {
			t.Errorf("stdout = %q, want [secret] — a readonly export was dropped", got.Stdout)
		}
	})

	// A line-oriented filter over `declare -px` would cut a multi-line value in
	// half and leave the env snapshot with an unterminated quote, taking every
	// variable declared after it down with it.
	t.Run("MultilineExportSurvivesAndDoesNotCorruptTheRest", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh(`export V=$'x\ndeclare -ar Y'; export AFTER=ok`, 0)
		got := sh(`[ "$V" = $'x\ndeclare -ar Y' ] && echo V_OK; echo "[${AFTER:-lost}]"`, 0)
		if !strings.Contains(got.Stdout, "V_OK") {
			t.Errorf("multi-line exported value did not survive; stdout=%q", got.Stdout)
		}
		if !strings.Contains(got.Stdout, "[ok]") {
			t.Errorf("a variable exported after the multi-line one was lost; stdout=%q", got.Stdout)
		}
	})

	// Each call is a fresh bash that re-inherits the container's environment, so a
	// variable the shell unset has to be unset again on restore or it silently
	// reappears — which is exactly what an agent scrubbing a secret must not see.
	t.Run("UnsetOfAnInheritedVariablePersists", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		if probe := sh(`echo "[${HOME:-unset}]"`, 0); strings.TrimSpace(probe.Stdout) == "[unset]" {
			t.Fatalf("test needs HOME inherited from the container, got %q", probe.Stdout)
		}
		sh("unset HOME", 0)
		got := sh(`echo "[${HOME:-UNSET}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[UNSET]" {
			t.Errorf("stdout = %q, want [UNSET] — an unset variable came back from the container env", got.Stdout)
		}
	})

	// A command that installs its own EXIT trap takes the only trap slot there is.
	// The snapshot has to survive that, or one `trap ... EXIT` silently discards
	// the whole call's state. The trap itself is discarded UNFIRED when the command
	// returns normally — the template clears it to run its own save — so an EXIT
	// trap only ever fires for a command that exits through it.
	t.Run("CommandsOwnExitTrapDoesNotLoseTheSnapshot", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		installed := sh(`cd /tmp; export T=1; trap 'echo BYE' EXIT`, 0)
		if strings.Contains(installed.Stdout, "BYE") {
			t.Errorf("stdout = %q — a normally-returning command's EXIT trap fired; the template clears it", installed.Stdout)
		}
		got := sh(`echo "[$(pwd)][${T:-unset}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[/tmp][1]" {
			t.Errorf("stdout = %q, want [/tmp][1] — a command's EXIT trap ate the snapshot", got.Stdout)
		}
	})

	// The save runs in the SAME shell as the command, and a bash function overrides
	// a builtin of the same name. A command that wraps `printf` — a wrapper, not an
	// attack — would otherwise have the save write an empty `names` file and still
	// earn its marker; the next call would restore `env` and then unset every name
	// `names` does not list, i.e. all of them, leaving the shell without a PATH.
	t.Run("AFunctionShadowingABuiltinCannotCorruptTheSnapshot", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh(`export KEEP=yes; cd /var; printf() { return 0; }`, 0)
		got := sh(`echo "[${KEEP:-LOST}][$(pwd)][${PATH:+PATH_OK}]"; declare -F printf >/dev/null && echo FN_KEPT`, 0)
		if !strings.Contains(got.Stdout, "[yes][/var][PATH_OK]") {
			t.Errorf("stdout = %q, want [yes][/var][PATH_OK] — a shadowed builtin corrupted the snapshot", got.Stdout)
		}
		if !strings.Contains(got.Stdout, "FN_KEPT") {
			t.Errorf("stdout = %q — the command's own function did not carry", got.Stdout)
		}
	})

	// The restore's unset-diff reads names a line at a time under `IFS=`. Splitting
	// `$(compgen -e)` instead would collapse to one nonsense word under an exported
	// `IFS=`, unset nothing, and let a scrubbed secret come back from the container
	// environment — the exact guarantee UnsetOfAnInheritedVariablePersists pins.
	t.Run("AnExportedIFSDoesNotBreakTheUnsetDiff", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("export IFS=; unset HOME", 0)
		got := sh(`echo "[${HOME:-UNSET}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[UNSET]" {
			t.Errorf("stdout = %q, want [UNSET] — an exported IFS disabled the unset-diff and HOME came back", got.Stdout)
		}
	})

	// The marker is a completeness check, not a trust boundary — it is on a
	// filesystem the command owns. Forging it and then skipping the save commits an
	// empty snapshot, which resets THIS session's shell. That is the documented
	// self-sabotage boundary, and this pins its blast radius: the command can lose
	// its own shell state, which it could already do by deleting the state
	// directory, and it reaches nothing else. The sandbox is per-session, so there
	// is no other session's state in this container to reach.
	t.Run("ForgingTheMarkerOnlyResetsTheCommandsOwnSession", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		other, _ := newShell(t, sb)
		other("export OTHER=intact; cd /var", 0)
		sh("export KEEP=yes; cd /var", 0)
		sh(`: >"$__map_snap/done"; exec echo forged`, 0)
		if got := sh(`echo "[${KEEP:-LOST}][$(pwd)]"`, 0); strings.TrimSpace(got.Stdout) != "[LOST][/workspace]" {
			t.Errorf("stdout = %q, want [LOST][/workspace] — forging the marker is documented to reset the session", got.Stdout)
		}
		if got := other(`echo "[${OTHER:-LOST}][$(pwd)]"`, 0); strings.TrimSpace(got.Stdout) != "[intact][/var]" {
			t.Errorf("stdout = %q, want [intact][/var] — self-sabotage reached beyond its own session", got.Stdout)
		}
	})

	// The template's trailing lines are parsed AFTER the restore has sourced the
	// snapshot's aliases, so a carried alias is expanded over them. `alias
	// trap=true` alone turned `trap '__map_save' EXIT` into a no-op, and every
	// later call that ended by calling `exit` lost its state, silently, for the
	// rest of the session. Aliases on `exit` and `.` reach the same lines.
	t.Run("AnAliasCannotHijackTheTemplatesOwnLines", func(t *testing.T) {
		for _, tc := range []struct{ name, alias string }{
			{"OnTrap", "trap=true"},
			{"OnExit", "exit=true"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sh, _ := newShell(t, sb)
				sh("shopt -s expand_aliases; alias "+tc.alias, 0)
				// A command that ends by calling exit: only the EXIT trap can save it.
				sh("export MUST_PERSIST=1; cd /tmp; exit 0", 0)
				got := sh(`echo "[${MUST_PERSIST:-LOST}][$(pwd)]"`, 0)
				if strings.TrimSpace(got.Stdout) != "[1][/tmp]" {
					t.Errorf("stdout = %q, want [1][/tmp] — an alias on %q hijacked the template's own lines",
						got.Stdout, tc.alias)
				}
			})
		}
	})

	// Every word `__map_main` runs is a builtin a command can shadow with a
	// function of the same name — `.` above all, which is a legal function name,
	// is snapshotted like any other, and once restored makes the template source
	// nothing: every later call returns exit 0 with no output, forever. `trap` is
	// the same shape and costs the next call its snapshot.
	t.Run("AFunctionCannotHijackTheTemplatesOwnWords", func(t *testing.T) {
		for _, tc := range []struct{ name, fn string }{
			{"Dot", `.() { echo HIJACKED_DOT; }`},
			{"Trap", `trap() { echo HIJACKED_TRAP; }`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sh, _ := newShell(t, sb)
				sh("export KEEP=yes; cd /var; "+tc.fn, 0)
				got := sh(`export SECOND=2; echo "[${KEEP:-LOST}][$(pwd)]"`, 0)
				if strings.TrimSpace(got.Stdout) != "[yes][/var]" {
					t.Fatalf("stdout = %q, want [yes][/var] — %s hijacked the template", got.Stdout, tc.fn)
				}
				after := sh(`echo "[${SECOND:-LOST}]"`, 0)
				if strings.TrimSpace(after.Stdout) != "[2]" {
					t.Errorf("stdout = %q, want [2] — %s cost the next call its snapshot", after.Stdout, tc.fn)
				}
			})
		}
	})

	// The restore is the other half of the same problem, and the harder half: it
	// SOURCES the snapshot's functions and aliases, so from that line on the
	// command's own definitions are live — over the restore's remaining words, and
	// over the words the `aliases` and `opts` files themselves run. A command that
	// defines `set() { … }` or `.() { … }` would otherwise silently and PERMANENTLY
	// lose the session its options and aliases: the restore drops them, the save
	// then snapshots a shell that no longer has them, and every later call carries
	// the loss forward. Unlike every other shadowing path here, that one fails
	// unsafe — it still earns its marker and still commits.
	t.Run("AFunctionOrAliasCannotHijackTheRestore", func(t *testing.T) {
		// Everything a snapshot can carry, so a hijack anywhere in the restore shows
		// up: two option families, an alias (which needs expand_aliases, itself an
		// option), an exported variable and a cwd.
		const setup = `shopt -s expand_aliases; shopt -s nullglob; set -o pipefail
alias greet='echo aliased'; export KEEP=yes; cd /var`
		// The probe has to survive the poison too — it runs in a call where the
		// poison is restored and live — so it reaches its own words as `\builtin`:
		// routed past a shadowing function, and quoted, because a quoted word is
		// never alias-expanded. A probe the poison can silence proves nothing, and
		// an unquoted `builtin echo` under `alias builtin=true` prints nothing at
		// all — which is correct bash, and the command's own doing, not the
		// template's. `[[` and `case` need no guard: they are keywords.
		const probe = `[[ -o pipefail ]] && \builtin echo P_ON || \builtin echo P_OFF
\builtin shopt -q nullglob && \builtin echo NG_ON || \builtin echo NG_OFF
\builtin shopt -q expand_aliases && \builtin echo XA_ON || \builtin echo XA_OFF
greet
\builtin echo "[${KEEP:-LOST}][$(pwd)]"`

		for _, tc := range []struct{ name, poison string }{
			// Functions, live from the moment the restore sources `funcs`.
			{"FuncOnDot", `.() { builtin return 0; }`},       // the restore's own `.`
			{"FuncOnBracket", `[() { builtin return 1; }`},   // the restore's own tests
			{"FuncOnEval", `eval() { builtin return 0; }`},   // the restore's own `eval`
			{"FuncOnSet", `set() { builtin return 0; }`},     // a word the `opts` file runs
			{"FuncOnShopt", `shopt() { builtin return 0; }`}, // ditto
			{"FuncOnAlias", `alias() { builtin return 0; }`}, // a word the `aliases` file runs
			// Aliases, live from the moment the restore sources `aliases` — and
			// expanded over whatever is parsed after that, `opts` included.
			{"AliasOnSet", `builtin alias set=true`},
			{"AliasOnShopt", `builtin alias shopt=true`},
			{"AliasOnBuiltin", `builtin alias builtin=true`}, // the guard's own word
		} {
			t.Run(tc.name, func(t *testing.T) {
				sh, _ := newShell(t, sb)
				sh(setup, 0)
				sh(tc.poison, 0)
				got := sh(probe, 0)
				for _, want := range []string{"P_ON", "NG_ON", "XA_ON", "aliased", "[yes][/var]"} {
					if !strings.Contains(got.Stdout, want) {
						t.Errorf("stdout = %q, want it to contain %q — %s hijacked the restore",
							got.Stdout, want, tc.poison)
					}
				}
			})
		}
	})

	// The namespace filter is `case $name in __map_*) continue`, and it is only as
	// good as the tool that reads the name back. A function or alias CAN be named
	// like an option — `-p`, `--` — and `declare -f "-p"` / `alias "-p"` then read
	// the name as the print-everything flag and dump the WHOLE table, the template's
	// own `__map_main` among it, straight past the filter; the next call restores
	// that `__map_main` over the template's and runs it, stripping the session. The
	// save passes every snapshotted name after `--` so a name is only ever a name.
	t.Run("AnOptionLikeNameCannotDumpTheWholeTablePastTheFilter", func(t *testing.T) {
		t.Run("Function", func(t *testing.T) {
			sh, _ := newShell(t, sb)
			sh(`export KEEP=yes; f(){ echo fn; }; cd /var`, 0)
			// `-p` as a function name; a user __map_main that would strip the session
			// if `declare -f -p` dumped it into the snapshot.
			sh("function -p { :; }\n__map_main() { set +e; unset KEEP; cd /workspace; __map_save 0; builtin exit 0; }", 0)
			got := sh(`\builtin echo "[${KEEP:-LOST}][$(pwd)]"`, 0)
			if strings.TrimSpace(got.Stdout) != "[yes][/var]" {
				t.Errorf("stdout = %q, want [yes][/var] — a `-p` function dumped the table past the filter", got.Stdout)
			}
		})
		t.Run("Alias", func(t *testing.T) {
			sh, _ := newShell(t, sb)
			sh(`export KEEP=yes; shopt -s expand_aliases; alias saved='echo a'; cd /var`, 0)
			sh("alias -- -p=':'\nalias __map_main='unset KEEP; __map_save 0; exit 0'", 0)
			got := sh(`\builtin echo "[${KEEP:-LOST}][$(pwd)]"`, 0)
			if strings.TrimSpace(got.Stdout) != "[yes][/var]" {
				t.Errorf("stdout = %q, want [yes][/var] — a `-p` alias dumped the table past the filter", got.Stdout)
			}
		})
	})

	// A carried alias on the template's own names must not survive either: the
	// alias table is namespace-filtered exactly as the exports and functions are.
	// Without that, `alias __map_main=...` replaces the whole call.
	t.Run("AnAliasOnTheTemplatesOwnNamesIsNotSnapshotted", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh(`shopt -s expand_aliases; alias __map_main='echo HIJACKED_MAIN'; export KEEP=yes`, 0)
		got := sh(`echo "[${KEEP:-LOST}]"`, 0)
		if strings.Contains(got.Stdout, "HIJACKED_MAIN") {
			t.Errorf("stdout = %q — an alias on __map_main was carried and ran instead of the command", got.Stdout)
		}
		if strings.TrimSpace(got.Stdout) != "[yes]" {
			t.Errorf("stdout = %q, want [yes]", got.Stdout)
		}
	})

	// An ERR or DEBUG trap under errtrace/functrace is inherited into the save's
	// process substitutions, and a trap that PRINTS writes into the very pipe the
	// name-reading loop consumes — its text is read as a variable name, the save
	// aborts, and the call silently loses everything it did. `set -E; trap … ERR`
	// is an ordinary script idiom.
	// The `exit` variants matter on their own: the command exits THROUGH the EXIT
	// trap, so __map_main's post-command trap clearing never runs and __map_save
	// has to drop the traps itself.
	t.Run("AnErrOrDebugTrapThatPrintsDoesNotCostTheCallItsSnapshot", func(t *testing.T) {
		for _, tc := range []struct{ name, setup, ending string }{
			{"ErrTrap", `set -E; trap 'echo boom' ERR`, ""},
			{"DebugTrap", `set -T; trap 'echo boom' DEBUG`, ""},
			{"ErrTrapAndExits", `set -E; trap 'echo boom' ERR`, "; exit 0"},
			{"DebugTrapAndExits", `set -T; trap 'echo boom' DEBUG`, "; exit 0"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sh, _ := newShell(t, sb)
				sh(tc.setup+`; export MARK=1; cd /tmp`+tc.ending, 0)
				got := sh(`set +ET; trap - DEBUG ERR; echo "[${MARK:-LOST}][$(pwd)]"`, 0)
				if !strings.Contains(got.Stdout, "[1][/tmp]") {
					t.Errorf("stdout = %q, want [1][/tmp] — a printing %s trap ate the call's snapshot",
						got.Stdout, tc.name)
				}
			})
		}
	})

	// The save runs in the command's shell, so every name it declares can collide
	// with one the command exported. They are all __map_*; `code` was not, and an
	// exported `code` came back as the previous call's exit status.
	// __map_main drops the command's DEBUG/ERR/RETURN traps as soon as the command
	// is done. __map_save clears them again on entry, and THAT is what protects the
	// snapshot (a command exiting through the EXIT trap never reaches __map_main's
	// line) — this earlier clear protects the tool RESULT: without it the template's
	// own remaining commands each fire the command's trap, and the model reads six
	// lines of `TICK` it did not print.
	t.Run("TheTemplatesOwnLinesDoNotFireTheCommandsDebugTrap", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		got := sh(`set -T; trap 'echo TICK' DEBUG; echo HELLO`, 0)
		if !strings.Contains(got.Stdout, "HELLO") {
			t.Fatalf("stdout = %q, want the command's own output in it", got.Stdout)
		}
		// The trap unavoidably fires on the very lines that clear it; what must not
		// happen is the whole save running under it.
		if n := strings.Count(got.Stdout, "TICK"); n > 4 {
			t.Errorf("stdout = %q — %d DEBUG-trap ticks: the template ran its save under the command's trap", got.Stdout, n)
		}
	})

	t.Run("ATemplateLocalDoesNotClobberACommandsVariable", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("export code=mysecret", 0)
		got := sh(`echo "code=[${code:-LOST}]"`, 0)
		if strings.TrimSpace(got.Stdout) != "code=[mysecret]" {
			t.Errorf("stdout = %q, want code=[mysecret] — a template local clobbered the command's variable", got.Stdout)
		}
	})

	// The executor may retry a tool call under the id it already used. The
	// snapshot must not be that id's directory, or the retry inherits the previous
	// attempt's files — above all its `done` marker, which would let a re-save that
	// failed part-way be committed on top of them.
	t.Run("ARetryUnderTheSameToolIDGetsAFreshSnapshot", func(t *testing.T) {
		session := domain.NewID("sesn")
		id := domain.NewID("sevt")
		run := func(cmd string) shell.Result {
			t.Helper()
			res, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: cmd})
			if err != nil {
				t.Fatalf("Run(%q): %v", cmd, err)
			}
			return res
		}
		run("export KEEP=yes; cd /var") // first attempt: commits under this id
		// The retry reuses the id and its save fails part-way. With a per-id
		// snapshot it would find the first attempt's marker and commit a torn mix.
		run(`cd /tmp; export EVIL=1; mkdir "$__map_snap/env"`)
		after := run(`echo "[${KEEP:-LOST}][${EVIL:-unset}][$(pwd)]"`)
		if strings.TrimSpace(after.Stdout) != "[yes][unset][/var]" {
			t.Errorf("stdout = %q, want [yes][unset][/var] — a retry reused the first attempt's snapshot", after.Stdout)
		}
	})

	// The template's own names must never be snapshotted, or a command that
	// defines one reaches across into the next call's machinery. (__map_save
	// itself is a poor probe: the template defines it on every call. A command
	// that redefines __map_save sabotages only its own call's snapshot, which is
	// the documented self-inflicted case.)
	t.Run("TemplateMachineryIsNotSnapshotted", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh(`__map_helper() { echo bad; }; export __map_state=hijacked; helper() { echo kept; }; cd /var`, 0)
		got := sh(`declare -F __map_helper >/dev/null && echo FN_LEAKED || echo FN_CLEAN
			[ "${__map_state:-}" = hijacked ] && echo VAR_LEAKED || echo VAR_CLEAN
			helper; pwd`, 0)
		if strings.Contains(got.Stdout, "FN_LEAKED") {
			t.Error("a command's __map_* function was carried into the next call")
		}
		if strings.Contains(got.Stdout, "VAR_LEAKED") {
			t.Error("a command's exported __map_* variable was carried into the next call")
		}
		if !strings.Contains(got.Stdout, "kept") || !strings.Contains(got.Stdout, "/var") {
			t.Errorf("ordinary state was lost alongside it; stdout=%q", got.Stdout)
		}
	})

	t.Run("ExitCodeFidelityThroughTheTrap", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		for _, tc := range []struct {
			cmd  string
			want int
		}{
			{"true", 0},
			{"false", 1},
			{"(exit 7)", 7},
			{"exit 3", 3},
			{"bash -c 'exit 42'", 42},
		} {
			if got := sh(tc.cmd, 0); got.ExitCode != tc.want {
				t.Errorf("%q: exit = %d, want %d", tc.cmd, got.ExitCode, tc.want)
			}
		}
	})

	t.Run("TimeoutDoesNotKillTheSession", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("export KEEP=yes; cd /var", 0)
		to := sh("sleep 300", time.Second)
		if !to.TimedOut {
			t.Fatalf("sleep 300 under a 1s timeout: TimedOut=false (%+v)", to)
		}
		after := sh(`echo "$KEEP:$(pwd)"`, 0)
		if strings.TrimSpace(after.Stdout) != "yes:/var" {
			t.Errorf("session state lost after a timeout: stdout=%q, want yes:/var", after.Stdout)
		}
	})

	// The killed path: the SIGKILL skips the save outright.
	t.Run("TimeoutDropsTheTimedOutCallsMutations", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("cd /workspace", 0)
		to := sh("cd /tmp; export EVIL=1; sleep 300", time.Second)
		if !to.TimedOut {
			t.Fatalf("expected TimedOut, got %+v", to)
		}
		after := sh(`echo "[$(pwd)][${EVIL:-unset}]"`, 0)
		if strings.TrimSpace(after.Stdout) != "[/workspace][unset]" {
			t.Errorf("stdout = %q, want [/workspace][unset] — a timed-out call's mutations persisted", after.Stdout)
		}
	})

	// The path a SIGKILL never reaches: the command kills the in-container
	// watchdog, overruns its deadline, and then exits on its own terms — so its
	// EXIT trap DOES run and its snapshot IS written. The call still reports a
	// timeout, so that snapshot is never committed, and the mutations still drop.
	t.Run("OverrunThatDodgedTheKillAlsoDropsItsMutations", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("cd /workspace", 0)
		to := sh(killWatchdog+"cd /tmp; export EVIL=1; sleep 2", time.Second)
		if !to.TimedOut {
			t.Fatalf("a command that killed its watchdog and overran was not a timeout: %+v", to)
		}
		after := sh(`echo "[$(pwd)][${EVIL:-unset}]"`, 0)
		if strings.TrimSpace(after.Stdout) != "[/workspace][unset]" {
			t.Errorf("stdout = %q, want [/workspace][unset] — an overrun that ran its EXIT trap committed its state", after.Stdout)
		}
	})

	// A call can finish well inside its deadline and still never reach its save:
	// it replaced the shell with `exec`, the shell was killed outright, it exited
	// through an EXIT trap of its own, or it sent the save somewhere it could not
	// write. None of those is a timeout, so the deadline does not gate them — the
	// snapshot's own `done` marker does. Without that gate every one of them
	// points `head` at the empty directory the call created on its way in, which
	// loses not just that call's mutations but every earlier call's with them.
	t.Run("CallThatNeverFinishesItsSnapshotKeepsThePreviousState", func(t *testing.T) {
		for _, tc := range []struct{ name, ending string }{
			{"ExecReplacesTheShell", `exec echo replaced`},
			{"ShellKilledOutright", `kill -9 $$`},
			{"CommandExitsThroughItsOwnExitTrap", `trap 'echo bye' EXIT; exit 0`},
			{"CommandSendsTheSaveNowhere", `export __map_snap=/nonexistent/pwned`},
			// The save fails on ONE file and can still write the rest, including the
			// marker — which is what a mid-save ENOSPC or EIO looks like. The marker
			// must be gated on every write, not just on the last one: bash ignores
			// errexit inside a compound command on the left of `&&`, so the obvious
			// `( set -e; ... ) && : >done` creates the marker anyway.
			{"SaveCannotWriteOneOfItsFiles", `mkdir "$__map_snap/env"`},
			{"SaveCannotWriteTheOptions", `mkdir "$__map_snap/opts"`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sh, _ := newShell(t, sb)
				sh("export KEEP=yes; cd /var", 0)
				got := sh("cd /tmp; export EVIL=1; "+tc.ending, 0)
				if got.TimedOut {
					t.Fatalf("%q reported a timeout — not the path under test", tc.ending)
				}
				after := sh(`echo "[${KEEP:-LOST}][${EVIL:-unset}][$(pwd)]"`, 0)
				if strings.TrimSpace(after.Stdout) != "[yes][unset][/var]" {
					t.Errorf("stdout = %q, want [yes][unset][/var] — a call that never saved must drop its own "+
						"mutations and leave the session's earlier state standing", after.Stdout)
				}
			})
		}
	})

	// The save writes the snapshot with builtins alone — no `mv`, no external
	// anything — so a command that breaks PATH is still snapshotted. This is the
	// hardening the restore already had, held to on the way out as well.
	t.Run("BrokenPATHDoesNotCostTheSnapshot", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("export KEEP=yes; cd /var", 0)
		sh("export PATH=/nonexistent", 0)
		got := sh(`echo "[${KEEP:-LOST}][$(pwd)][$PATH]"`, 0)
		if strings.TrimSpace(got.Stdout) != "[yes][/var][/nonexistent]" {
			t.Errorf("stdout = %q, want [yes][/var][/nonexistent] — a broken PATH cost the snapshot", got.Stdout)
		}
	})

	t.Run("FastCommandUnderTimeoutIsNotATimeout", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		got := sh("echo quick", time.Second)
		if got.TimedOut {
			t.Error("a fast command read as a timeout — the snapshot bracket must fit inside the deadline")
		}
		if strings.TrimSpace(got.Stdout) != "quick" {
			t.Errorf("stdout=%q", got.Stdout)
		}
	})

	// A backgrounded PROCESS survives across calls (reachable by pid), but the
	// shell's jobs table does not carry.
	t.Run("BackgroundProcessSurvivesButJobsTableDoesNot", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		sh("sleep 987654 >/dev/null 2>&1 &", 0)
		got := sh(`jobs; echo __END__`, 0)
		if before, _, _ := strings.Cut(got.Stdout, "__END__"); strings.TrimSpace(before) != "" {
			t.Errorf("jobs table carried across calls: %q — divergence not holding", before)
		}
		if n := countProc(t, sb, "sleep 987654"); n != 1 {
			t.Errorf("backgrounded process count = %d, want 1 (it must survive across calls)", n)
		}
	})

	t.Run("RestartResetsTheShellButKeepsFiles", func(t *testing.T) {
		sh, session := newShell(t, sb)
		sh("export GONE=1; cd /tmp; echo keep > /workspace/restart_probe", 0)
		res, err := shell.Run(context.Background(), sb, session, domain.NewID("sevt"),
			shell.Request{Restart: true})
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		if !res.Restarted {
			t.Error("Restarted not reported")
		}
		after := sh(`echo "[${GONE:-unset}][$(pwd)][$(cat /workspace/restart_probe)]"`, 0)
		if strings.TrimSpace(after.Stdout) != "[unset][/workspace][keep]" {
			t.Errorf("stdout = %q, want [unset][/workspace][keep] (shell reset, file kept)", after.Stdout)
		}
	})

	t.Run("OutputIsSeparatedAndCapped", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		// NUL bytes and the literal MAPDONE must survive — there is no sentinel.
		got := sh(`printf 'a\0b MAPDONE'; printf 'to-err' >&2`, 0)
		if got.Stdout != "a\x00b MAPDONE" {
			t.Errorf("stdout = %q, want binary-safe a\\0b MAPDONE", got.Stdout)
		}
		if got.Stderr != "to-err" {
			t.Errorf("stderr = %q, streams must not cross", got.Stderr)
		}
		big := sh(`yes a | head -c 1400000; echo err >&2`, 0)
		if len(big.Stdout) != sandbox.MaxOutputBytes {
			t.Errorf("stdout kept %d bytes, want the %d cap", len(big.Stdout), sandbox.MaxOutputBytes)
		}
		if !big.Truncated {
			t.Error("Truncated not reported past the cap")
		}
		if strings.TrimSpace(big.Stderr) != "err" {
			t.Errorf("stderr = %q — capping one stream must not lose the other", big.Stderr)
		}
	})

	// The whole point of re-exec over a resident shell: the sandbox's
	// outside-the-container deadline is inherited verbatim through the bracket.
	t.Run("TimeoutGuaranteeInheritedThroughTheBracket", func(t *testing.T) {
		sh, _ := newShell(t, sb)
		got := sh(killWatchdog+"sleep 5", time.Second)
		if !got.TimedOut {
			t.Errorf("a command that killed its watchdog and overran was not a timeout: %+v", got)
		}
	})
}

// killWatchdog tears down the in-container watchdog a command can see, so only
// the outside-the-sandbox probe can still catch the overrun (mirrors the sandbox
// contract suite).
const killWatchdog = `
  for parent in $$ $PPID; do
    for p in $(cat /proc/$parent/task/$parent/children 2>/dev/null); do
      [ "$p" != "$$" ] && kill -9 "$p" 2>/dev/null
    done
  done
`

func countProc(t *testing.T, sb sandbox.Sandbox, prefix string) int {
	t.Helper()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: `
		n=0
		for p in /proc/[0-9]*; do
		  [ -r "$p/cmdline" ] || continue
		  case "$(tr '\0' ' ' < "$p/cmdline")" in
		    "` + prefix + `"*) n=$((n+1)) ;;
		  esac
		done
		echo "$n"`})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatalf("count %q: %v", res.Stdout, err)
	}
	return n
}
