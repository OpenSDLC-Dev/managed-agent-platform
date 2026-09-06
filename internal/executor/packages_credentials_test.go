package executor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// credsDir is the directory the pass materialized a manager's credentials into,
// read back from the sandbox rather than predicted: the name is random, so that
// the agent — which can write anywhere in the sandbox — cannot pre-create it as
// a regular file and make the write fail.
func credsDir(t *testing.T, sb *fakeSandbox) string {
	t.Helper()
	var found string
	for path := range sb.files {
		if strings.HasSuffix(path, "/.netrc") {
			if found != "" {
				t.Fatalf("two netrc files were written: %q and %q", found, path)
			}
			found = strings.TrimSuffix(path, "/.netrc")
		}
	}
	if found == "" {
		t.Fatalf("no netrc was written; files = %v", keysOf(sb.files))
	}
	return found
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestACredentialNeverReachesTheInstallCommand is #599's first surface, and it
// is asserted on the whole command rather than on a substring of it: the
// assembled string is one execve argument on the docker backend and the exec
// subresource's `command` parameters on Kubernetes, which the apiserver records
// wherever auditing covers `pods/exec` — a reader outside the session's trust
// domain entirely.
func TestACredentialNeverReachesTheInstallCommand(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{
		"pip": {"git+https://bot:s3cr3t@git.example.com/team/lib", "sqlalchemy==2.0.30"},
	})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	got := installCmds(sb)
	if len(got) != 1 {
		t.Fatalf("install commands = %d, want 1:\n%s", len(got), strings.Join(got, "\n"))
	}
	if strings.Contains(got[0], "s3cr3t") {
		t.Fatalf("the install command carries the credential:\n%s", got[0])
	}
	dir := credsDir(t, sb)
	want := wrap("trap \"rm -rf '" + dir + "'\" EXIT; " +
		"python3 -m pip --version >/dev/null 2>&1 || exit 127; " +
		"export HOME='" + dir + "'; " +
		"PIP_BREAK_SYSTEM_PACKAGES=1 PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_INPUT=1 " +
		"python3 -m pip install 'git+https://git.example.com/team/lib' 'sqlalchemy==2.0.30'; exit \"$?\"")
	if got[0] != want {
		t.Errorf("install command:\n got %s\nwant %s", got[0], want)
	}
	if netrc := sb.files[dir+"/.netrc"]; netrc != "machine git.example.com\nlogin \"bot\"\npassword \"s3cr3t\"\n" {
		t.Errorf("netrc = %q", netrc)
	}
	if _, ok := sb.files[dir+"/.npmrc"]; ok {
		t.Errorf("an npmrc was written for pip, whose fetcher reads netrc")
	}
	// The sentinel compares the list as written, credential included, so a
	// rotated credential is a changed list rather than the same one with an
	// exhausted attempt count. It is the published digest that is stripped, and
	// TestTheDigestAnEnvironmentKeyCanReadIsStripped drives that one.
	recs := sentinel(t, sb)
	wantDigest := packagesDigest([]string{"git+https://bot:s3cr3t@git.example.com/team/lib", "sqlalchemy==2.0.30"})
	if got := recs["pip"].Digest; got != wantDigest {
		t.Errorf("sentinel digest = %s, want the list as written %s", got, wantDigest)
	}
	// And the directory is asked for again after the install returns, because a
	// timed-out install is SIGKILLed and its trap never runs.
	if !slices.Contains(sb.cmds, "rm -rf '"+dir+"'") {
		t.Errorf("no removal ran after the install; commands were %v", sb.cmds)
	}
}

// TestNpmAlsoGetsAnNpmrc: npm's own fetcher reads no netrc, so a tarball URL's
// credential has to arrive as the per-host pair its config carries. Both files
// are written, because an npm list can hold a `git+https://` entry too and that
// one is git's fetch, not npm's.
func TestNpmAlsoGetsAnNpmrc(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{
		"npm": {"https://ci:tok3n@npm.example.com:8443/pkg.tgz"},
	})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	dir := credsDir(t, sb)
	npmrc := sb.files[dir+"/.npmrc"]
	for _, want := range []string{
		"//npm.example.com:8443/:username=ci\n",
		"//npm.example.com:8443/:_password=dG9rM24=\n",
	} {
		if !strings.Contains(npmrc, want) {
			t.Errorf("npmrc is missing %q; got:\n%s", want, npmrc)
		}
	}
	// The netrc matches on the hostname alone — the port is not part of a
	// `machine` line, which is measured rather than assumed (plan 46).
	if netrc := sb.files[dir+"/.netrc"]; !strings.HasPrefix(netrc, "machine npm.example.com\n") {
		t.Errorf("netrc = %q, want a machine line carrying the hostname without its port", netrc)
	}
	cmds := installCmds(sb)
	if len(cmds) != 1 || strings.Contains(cmds[0], "tok3n") {
		t.Fatalf("install command carries the credential or is missing:\n%v", cmds)
	}
}

// TestAnUncredentialedListIsUntouched: nothing about the pass changes for the
// overwhelming majority of lists, which carry no credential at all — no files,
// no scratch HOME, no trap.
func TestAnUncredentialedListIsUntouched(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{"pip": {"sqlalchemy==2.0.30"}})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	got := installCmds(sb)
	want := wrap("python3 -m pip --version >/dev/null 2>&1 || exit 127; " +
		"PIP_BREAK_SYSTEM_PACKAGES=1 PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_INPUT=1 " +
		"python3 -m pip install 'sqlalchemy==2.0.30'")
	if len(got) != 1 || got[0] != want {
		t.Errorf("install command:\n got %v\nwant %s", got, want)
	}
	for path := range sb.files {
		if strings.Contains(path, ".netrc") || strings.Contains(path, ".npmrc") {
			t.Errorf("a credential file was written for a list that carries none: %q", path)
		}
	}
}

// TestACredentialIsLiftedOutOfTheEntryItRidesIn walks the shapes an entry
// takes. The entry the manager receives keeps every byte the credential did not
// occupy — a URL rebuilt from its parsed parts could re-encode a path this
// platform never examined.
func TestACredentialIsLiftedOutOfTheEntryItRidesIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   string
		want    string
		creds   []packageCredential
		inlined []string
	}{
		{
			name:  "a git URL, which is how pip and npm reach a private repo",
			entry: "git+https://bot:s3cr3t@git.example.com/team/lib.git@v1",
			want:  "git+https://git.example.com/team/lib.git@v1",
			creds: []packageCredential{{authority: "git.example.com", machine: "git.example.com", user: "bot", secret: "s3cr3t"}},
		},
		{
			name:  "a tarball URL with a port, which the npmrc key carries and the netrc line does not",
			entry: "https://ci:tok3n@npm.example.com:8443/pkg.tgz",
			want:  "https://npm.example.com:8443/pkg.tgz",
			creds: []packageCredential{{authority: "npm.example.com:8443", machine: "npm.example.com", user: "ci", secret: "tok3n"}},
		},
		{
			name:  "percent-encoded userinfo reaches the file decoded, because that is what the origin is sent",
			entry: "https://user%40corp.example:p%2Fss%20word@host.example.com/x.tgz",
			want:  "https://host.example.com/x.tgz",
			creds: []packageCredential{{authority: "host.example.com", machine: "host.example.com", user: "user@corp.example", secret: "p/ss word"}},
		},
		{
			name:  "a bare user names a user, and there is no secret to hide",
			entry: "https://someone@host.example.com/x.tgz",
			want:  "https://someone@host.example.com/x.tgz",
		},
		{
			name:  "an ordinary pin is not a URL at all",
			entry: "sqlalchemy==2.0.30",
			want:  "sqlalchemy==2.0.30",
		},
		{
			name:  "an @ in the path is not userinfo",
			entry: "https://host.example.com/a@b/c.tgz",
			want:  "https://host.example.com/a@b/c.tgz",
		},
		{
			name:    "a credential no netrc can carry keeps its entry, and is named",
			entry:   "https://u:with%0Anewline@host.example.com/x.tgz",
			want:    "https://u:with%0Anewline@host.example.com/x.tgz",
			inlined: []string{"host.example.com"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stripPackageCredentials([]string{tc.entry})
			if len(got.entries) != 1 || got.entries[0] != tc.want {
				t.Errorf("entry = %q, want %q", got.entries, tc.want)
			}
			if len(got.creds) != len(tc.creds) {
				t.Fatalf("credentials = %+v, want %+v", got.creds, tc.creds)
			}
			for i := range tc.creds {
				if got.creds[i] != tc.creds[i] {
					t.Errorf("credential %d = %+v, want %+v", i, got.creds[i], tc.creds[i])
				}
			}
			if strings.Join(got.inlined, ",") != strings.Join(tc.inlined, ",") {
				t.Errorf("inlined = %v, want %v", got.inlined, tc.inlined)
			}
		})
	}
}

// TestTheNetrcCarriesACredentialWhitespaceWouldSplit pins the quoting the
// format needs and the escaping inside it. Both were measured against the
// parser git rides on before being written here (plan 46): unquoted, a password
// with a space is two tokens and the origin is sent the first half.
func TestTheNetrcCarriesACredentialWhitespaceWouldSplit(t *testing.T) {
	got := string(netrcFile([]packageCredential{
		{machine: "a.example.com", user: "bot", secret: `pa"ss\wo rd`},
		{machine: "b.example.com", user: "two words", secret: "plain"},
	}))
	want := "machine a.example.com\nlogin \"bot\"\npassword \"pa\\\"ss\\\\wo rd\"\n" +
		"machine b.example.com\nlogin \"two words\"\npassword \"plain\"\n"
	if got != want {
		t.Errorf("netrc:\n got %q\nwant %q", got, want)
	}
}

// TestTheNpmrcCarriesTheSecretAsBase64: npm's own config takes the password
// base64-encoded, which is what lets it carry bytes a netrc value could not.
func TestTheNpmrcCarriesTheSecretAsBase64(t *testing.T) {
	got := string(npmrcFile([]packageCredential{
		{authority: "npm.example.com:8443", user: "ci", secret: "tok3n"},
	}))
	want := "//npm.example.com:8443/:username=ci\n" +
		"//npm.example.com:8443/:_password=dG9rM24=\n"
	if got != want {
		t.Errorf("npmrc:\n got %q\nwant %q", got, want)
	}
}

// TestTheDigestAnEnvironmentKeyCanReadIsStripped is #599's second surface: with
// the credential out of the pre-image, a digest an environment key can read is
// no longer an offline oracle for the credential a management key holds.
func TestTheDigestAnEnvironmentKeyCanReadIsStripped(t *testing.T) {
	with := stripPackageCredentials([]string{"git+https://bot:s3cr3t@git.example.com/team/lib"})
	without := stripPackageCredentials([]string{"git+https://git.example.com/team/lib"})
	if got, want := packagesDigest(with.entries), packagesDigest(without.entries); got != want {
		t.Errorf("digest with a credential = %s, want the credential-free list's %s", got, want)
	}
	// And a list that differs in something other than its credential still
	// digests differently, so the sentinel keeps telling one list from another.
	other := stripPackageCredentials([]string{"git+https://bot:s3cr3t@git.example.com/team/other"})
	if packagesDigest(with.entries) == packagesDigest(other.entries) {
		t.Error("two different lists share a digest")
	}
}

// TestACredentialNestedInAnEntryIsLiftedOutToo: the entry is not always the
// URL. pip's PEP 508 direct reference and npm's alias each nest one, both are
// syntax their managers accept, and parsing the whole entry as a URL sees
// neither — which left the credential in argv with no warning at all.
func TestACredentialNestedInAnEntryIsLiftedOutToo(t *testing.T) {
	for _, tc := range []struct{ name, entry, want string }{
		{
			name:  "pip's PEP 508 direct reference",
			entry: "private-lib @ git+https://bot:s3cr3t@git.example.com/team/lib.git",
			want:  "private-lib @ git+https://git.example.com/team/lib.git",
		},
		{
			name:  "npm's alias to a tarball URL",
			entry: "private-lib@https://ci:tok3n@npm.example.com/pkg.tgz",
			want:  "private-lib@https://npm.example.com/pkg.tgz",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stripPackageCredentials([]string{tc.entry})
			if got.entries[0] != tc.want {
				t.Errorf("entry = %q, want %q", got.entries[0], tc.want)
			}
			if len(got.creds) != 1 {
				t.Fatalf("credentials = %+v, want one", got.creds)
			}
		})
	}
}

// TestATransportThatReadsNothingWeWriteKeepsItsCredential: moving a credential
// into a netrc only helps a fetcher that reads one. ssh takes no password from
// a URL at all, and hg, svn and bzr authenticate from their own stores — so
// lifting theirs out would break an install that works today.
func TestATransportThatReadsNothingWeWriteKeepsItsCredential(t *testing.T) {
	for _, entry := range []string{
		"git+ssh://bot:s3cr3t@git.example.com/team/lib",
		"hg+https://bot:s3cr3t@hg.example.com/repo",
		"svn+https://bot:s3cr3t@svn.example.com/repo",
	} {
		got := stripPackageCredentials([]string{entry})
		if got.entries[0] != entry {
			t.Errorf("entry = %q, want it untouched", got.entries[0])
		}
		if len(got.creds) != 0 {
			t.Errorf("credentials = %+v, want none", got.creds)
		}
		if len(got.inlined) != 1 {
			t.Errorf("inlined = %v, want the host named once", got.inlined)
		}
	}
}

// TestOneHostnameCannotHoldTwoCredentials: a netrc line matches on the hostname
// alone, so two entries naming one host with different credentials have no
// representation that keeps them apart. Writing either would send one service
// the other's secret — a worse outcome than the argv exposure it replaces — so
// both stay where they are.
func TestOneHostnameCannotHoldTwoCredentials(t *testing.T) {
	entries := []string{
		"https://alice:secretA@registry.example:8443/a.whl",
		"https://bob:secretB@registry.example:9443/b.whl",
	}
	got := stripPackageCredentials(entries)
	if !slices.Equal(got.entries, entries) {
		t.Errorf("entries = %q, want them untouched", got.entries)
	}
	if len(got.creds) != 0 {
		t.Errorf("credentials = %+v, want none written", got.creds)
	}
	if len(got.inlined) != 2 {
		t.Errorf("inlined = %v, want both named", got.inlined)
	}
	// One host named twice with the SAME credential is not a conflict, and is
	// written once.
	same := stripPackageCredentials([]string{
		"https://alice:secretA@registry.example/a.whl",
		"https://alice:secretA@registry.example/b.whl",
	})
	if len(same.creds) != 1 {
		t.Errorf("credentials = %+v, want the repeat collapsed into one", same.creds)
	}
}

// TestARotatedCredentialIsAChangedList: the sentinel's "until the list changes"
// contract. Comparing the stripped form would make a corrected credential
// indistinguishable from the broken one it replaces, so a list that had spent
// its three attempts would never install again.
func TestARotatedCredentialIsAChangedList(t *testing.T) {
	bad := packagesDigest([]string{"git+https://bot:bad@host.example.com/repo"})
	good := packagesDigest([]string{"git+https://bot:good@host.example.com/repo"})
	if bad == good {
		t.Error("a rotated credential digests the same, so the sandbox would skip the install")
	}
}

// TestTheTrapRemovesTheCredentialsWhenTheInstallEnds runs the assembled command
// through a real shell rather than asserting that it contains a trap. Both
// arms: an install that fails, and a manager that is missing, whose preflight
// exits 127 before anything else in the group runs.
func TestTheTrapRemovesTheCredentialsWhenTheInstallEnds(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("no bash: %v", err)
	}
	for _, tc := range []struct {
		name              string
		preflight, script string
		wantStatus        int
	}{
		{name: "the install fails", preflight: "true", script: "false", wantStatus: 1},
		{name: "the manager is missing", preflight: packagePreflight("definitely-not-a-real-binary"), script: "echo unreachable", wantStatus: 127},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "creds")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".netrc"), []byte("machine h\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			m := packageManager{
				name:      "probe",
				preflight: tc.preflight,
				install:   func([]string) string { return tc.script },
			}
			// The command is expected to fail. Two things are asserted: the
			// cleanup ran, and the status the classification reads survived the
			// `exit "$?"` the cleanup needed.
			err := exec.Command("bash", "-c", m.command([]string{"x"}, dir)).Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != tc.wantStatus {
				t.Errorf("status = %v, want %d", err, tc.wantStatus)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the credential directory survived the install: %v", err)
			}
		})
	}
}

// TestTheInstallReadsItsCredentialsFromTheScratchHome pins the other half of
// the same command: HOME is exported before the install runs, so the netrc and
// npmrc the pass wrote are the ones the fetcher finds.
func TestTheInstallReadsItsCredentialsFromTheScratchHome(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("no bash: %v", err)
	}
	dir := t.TempDir()
	m := packageManager{
		name:      "probe",
		preflight: "true",
		install:   func([]string) string { return `test "$HOME" = ` + shellQuote(dir) },
	}
	if err := exec.Command("bash", "-c", m.command([]string{"x"}, dir)).Run(); err != nil {
		t.Errorf("the install did not see the scratch HOME: %v", err)
	}
}

// TestThePublishedDigestCarriesNoCredential is #599's second surface where it
// actually ships: on the event. Its subtree is readable with an environment key
// while the environment config it digests needs a management key, so a digest
// taken over the entries as written is an offline oracle for a weak credential.
// The pure function having the right answer is not the claim — this asserts the
// value that leaves the process.
func TestThePublishedDigestCarriesNoCredential(t *testing.T) {
	sb := &fakeSandbox{execHook: failInstall(sandbox.ExecResult{
		ExitCode: 100,
		Stdout:   "could not authenticate\n",
	})}
	h := newHarness(t, sb)
	h.setPackages(t, map[string][]string{
		"pip": {"git+https://bot:s3cr3t@git.example.com/team/lib"},
	})
	h.suspend(t, writeUse("out.txt", "hello"))
	h.stepOnce(t)

	errs := h.packageErrors(t)
	if len(errs) != 1 {
		t.Fatalf("package errors = %d, want 1: %+v", len(errs), errs)
	}
	stripped := packagesDigest([]string{"git+https://git.example.com/team/lib"})
	written := packagesDigest([]string{"git+https://bot:s3cr3t@git.example.com/team/lib"})
	switch got := errs[0]["packages_digest"]; got {
	case stripped:
	case written:
		t.Error("the published digest is taken over the entries as written, so it is a pre-image an environment-key holder can search")
	default:
		t.Errorf("packages_digest = %v, want the stripped list's %s", got, stripped)
	}
	// The sentinel inside the sandbox is the other half, and it keeps the list
	// as written so that a rotated credential still reads as a changed list.
	if rec := sentinel(t, sb)["pip"]; rec.Digest != written {
		t.Errorf("sentinel digest = %s, want the list as written %s", rec.Digest, written)
	}
}
