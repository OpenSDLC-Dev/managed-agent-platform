package executor

import (
	"strings"
	"testing"
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
		"python3 -m pip install 'git+https://git.example.com/team/lib' 'sqlalchemy==2.0.30'")
	if got[0] != want {
		t.Errorf("install command:\n got %s\nwant %s", got[0], want)
	}
	if netrc := sb.files[dir+"/.netrc"]; netrc != "machine git.example.com\nlogin \"bot\"\npassword \"s3cr3t\"\n" {
		t.Errorf("netrc = %q", netrc)
	}
	if _, ok := sb.files[dir+"/.npmrc"]; ok {
		t.Errorf("an npmrc was written for pip, whose fetcher reads netrc")
	}
	// The sentinel the agent can read carries the digest of the stripped list,
	// which is what the pass actually compares against on its next turn — the
	// second surface is closed in the wiring, not only in the function.
	recs := sentinel(t, sb)
	wantDigest := packagesDigest([]string{"git+https://git.example.com/team/lib", "sqlalchemy==2.0.30"})
	if got := recs["pip"].Digest; got != wantDigest {
		t.Errorf("sentinel digest = %s, want the stripped list's %s", got, wantDigest)
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
		"//npm.example.com:8443/:always-auth=true\n",
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
		"//npm.example.com:8443/:_password=dG9rM24=\n" +
		"//npm.example.com:8443/:always-auth=true\n"
	if got != want {
		t.Errorf("npmrc:\n got %q\nwant %q", got, want)
	}
}

// TestTheDigestIsTakenOverTheStrippedList is #599's second surface: with the
// credential out of the pre-image, a digest an environment key can read is no
// longer an offline oracle for the credential a management key holds.
func TestTheDigestIsTakenOverTheStrippedList(t *testing.T) {
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
