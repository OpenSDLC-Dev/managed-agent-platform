package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Terraform half is exercised against fixtures, never against deploy/gcp:
// this package's test runs inside `make verify`, and that gate does not depend
// on the GCP tree (plan 20, Decision 9). `make gcp-kms-role-check` is what holds
// the real configuration to the rule, exactly as `gcp-split-check` does for
// check_split.py while `gcp-split-check-test` runs its units.
//
// The GO half is the real tree in both: readNeeds reads go.mod, cmd/ and the
// packages they import, so every case below measures its Terraform against what
// this repository's code actually calls rather than against a second fixture.

func repoRoot() string { return filepath.Join("..", "..") }

// fixtureIAM mirrors deploy/gcp/environment/iam.tf's two grants.
const fixtureIAM = `resource "google_kms_crypto_key_iam_member" "controlplane" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = "serviceAccount:${data.google_service_account.controlplane.email}"
}

resource "google_kms_crypto_key_iam_member" "executor" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
}
`

// grantFor is the fixture's block shape for one identity, so a case that needs
// a grant for some other binary does not have to restate it.
func grantFor(label, role string) string {
	return `resource "google_kms_crypto_key_iam_member" "` + label + `" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "` + role + `"
  member        = "serviceAccount:${data.google_service_account.` + label + `.email}"
}
`
}

// tfTree writes a scratch Terraform directory. A nil map means the fixture
// above, alone, in one root. A name without a directory lands in `environment/`,
// because that is where the real grants live and the guard reads only the roots
// an apply loads.
func tfTree(t *testing.T, files map[string]string) string {
	t.Helper()
	if files == nil {
		files = map[string]string{"iam.tf": fixtureIAM}
	}
	dir := t.TempDir()
	for name, body := range files {
		if !strings.Contains(name, "/") {
			name = "environment/" + name
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// rewrite mutates the fixture, failing the test when the anchor is not there at
// all: an anchor that has drifted would otherwise leave the fixture unchanged
// and the negative case passing for the wrong reason.
func rewrite(t *testing.T, old, new string) string {
	t.Helper()
	if !strings.Contains(fixtureIAM, old) {
		t.Fatalf("anchor %q is not in the fixture", old)
	}
	out := strings.ReplaceAll(fixtureIAM, old, new)
	if out == fixtureIAM {
		t.Fatal("the mutation changed nothing — a mutation that did not mutate agrees with its subject instead of checking it")
	}
	return out
}

const executorHeader = `resource "google_kms_crypto_key_iam_member" "executor" {`

// executorKey is the executor grant's first two lines, the shortest anchor
// unique to that block — both blocks assign the same key and the same role.
const executorKey = executorHeader + "\n  crypto_key_id = data.google_kms_crypto_key.cipher.id"

func mustCheck(t *testing.T, tfDir string) Report {
	t.Helper()
	r, err := Check(repoRoot(), tfDir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return r
}

func wantRefusal(t *testing.T, tfDir, contains string) {
	t.Helper()
	r, err := Check(repoRoot(), tfDir)
	if err == nil {
		t.Fatalf("expected a refusal, got %d finding(s) and no error: %v", len(r.Findings), r.Findings)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("refusal does not mention %q: %v", contains, err)
	}
}

func findingFor(r Report, binary string) *Finding {
	for i := range r.Findings {
		if r.Findings[i].Binary == binary {
			return &r.Findings[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The Go half, against the real tree.
// ---------------------------------------------------------------------------

// TestTheGuardHasTeeth pins the facts that make a passing run mean something. A
// scan that found no call sites, or credited them to the wrong binary, would
// pass just as quietly.
func TestTheGuardHasTeeth(t *testing.T) {
	got, err := readNeeds(repoRoot())
	if err != nil {
		t.Fatalf("readNeeds: %v", err)
	}
	rows := map[string]Row{}
	for _, row := range got {
		rows[row.Binary] = row
	}
	if len(rows) != len(got) {
		t.Fatalf("a binary appears twice in %d rows", len(got))
	}
	for _, b := range []string{"controlplane", "brain", "executor", "worker", "gate"} {
		if _, ok := rows[b]; !ok {
			t.Fatalf("cmd/%s has no row — a binary this guard never looked at is a binary whose grant it never checked", b)
		}
	}
	for _, b := range []string{"controlplane", "executor"} {
		if n := rows[b].Needs; !n.Encrypt || !n.Decrypt {
			t.Errorf("cmd/%s calls %s, want both — #748 was the Decrypt half going unnoticed", b, n)
		}
	}
	// brain is the negative case that proves the scan is not simply counting
	// every binary.
	if n := rows["brain"].Needs; n.any() {
		t.Errorf("cmd/brain calls %s, want neither", n)
	}
	// Both of these are reached only transitively — repos.go through
	// internal/executor, mcp.go two hops out through internal/api — so finding
	// them is what says the import walk, rather than a glance at cmd/, produced
	// the answer.
	if !hasSite(rows["executor"], "internal/executor/repos.go", "Decrypt") {
		t.Errorf("executor's Decrypt sites do not include internal/executor/repos.go: %v", rows["executor"].Sites)
	}
	if !hasSite(rows["controlplane"], "internal/vaultresolve/mcp.go", "Decrypt") {
		t.Errorf("controlplane's Decrypt sites do not include internal/vaultresolve/mcp.go: %v", rows["controlplane"].Sites)
	}
}

func hasSite(r Row, file, call string) bool {
	for _, s := range r.Sites {
		if strings.HasPrefix(s, file+":") && strings.HasSuffix(s, " "+call) {
			return true
		}
	}
	return false
}

// fakeRoot writes a minimal module: go.mod plus the named files.
func fakeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module m\n\ngo 1.26.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestAMethodValueCounts: `f := cipher.Decrypt` hands the method somewhere else
// to call. A scan that looked only at calls would report the binary as reaching
// no cipher — an under-count, the one direction this guard may not be wrong in.
func TestAMethodValueCounts(t *testing.T) {
	root := fakeRoot(t, map[string]string{"cmd/executor/main.go": `package main

type cipher struct{}

func (cipher) Decrypt(b []byte) []byte { return b }

func main() {
	c := cipher{}
	f := c.Decrypt
	_ = f
}
`})
	rows, err := readNeeds(root)
	if err != nil {
		t.Fatalf("readNeeds: %v", err)
	}
	if len(rows) != 1 || !rows[0].Needs.Decrypt {
		t.Fatalf("readNeeds = %+v, want cmd/executor needing Decrypt", rows)
	}
}

// TestTheModuleRootPackageIsWalked: an import of the module path itself carries
// no "/" after it, so the prefix test that finds every other in-module import
// steps straight over it.
func TestTheModuleRootPackageIsWalked(t *testing.T) {
	root := fakeRoot(t, map[string]string{
		"root.go": `package m

type cipher struct{}

func (cipher) Decrypt(b []byte) []byte { return b }

func Open(c cipher, b []byte) []byte { return c.Decrypt(b) }
`,
		"cmd/x/main.go": "package main\n\nimport \"m\"\n\nfunc main() { _ = m.Open }\n",
	})
	rows, err := readNeeds(root)
	if err != nil {
		t.Fatalf("readNeeds: %v", err)
	}
	if len(rows) != 1 || !rows[0].Needs.Decrypt {
		t.Fatalf("readNeeds = %+v, want cmd/x needing Decrypt through the module-root package", rows)
	}
}

// TestAnUnqualifiedCallCounts: a package-level `func Decrypt`, called without a
// receiver or through a dot import, carries no selector. A selector-only scan
// read both as reaching no cipher at all — an under-count, the one direction
// this guard may not be wrong in.
func TestAnUnqualifiedCallCounts(t *testing.T) {
	vault := map[string]string{
		"internal/vault/seal.go": "package vault\n\nfunc Decrypt(b []byte) []byte { return b }\n",
	}
	for name, main := range map[string]string{
		"same package": "package main\n\nfunc Decrypt(b []byte) []byte { return b }\n\nfunc main() { _ = Decrypt(nil) }\n",
		"dot import":   "package main\n\nimport . \"m/internal/vault\"\n\nfunc main() { _ = Decrypt(nil) }\n",
		"qualified":    "package main\n\nimport \"m/internal/vault\"\n\nfunc main() { _ = vault.Decrypt(nil) }\n",
	} {
		files := map[string]string{"cmd/executor/main.go": main}
		for k, v := range vault {
			files[k] = v
		}
		rows, err := readNeeds(fakeRoot(t, files))
		if err != nil {
			t.Errorf("%s: readNeeds: %v", name, err)
			continue
		}
		if len(rows) != 1 || !rows[0].Needs.Decrypt {
			t.Errorf("%s: readNeeds = %+v, want Decrypt", name, rows)
		}
	}
}

// TestABinaryTreeThatReachesNothingIsRefused: a tree this guard can read all the
// way through and still find no call in. Vacuously satisfying the rule is the
// shape a checker's bug takes.
func TestABinaryTreeThatReachesNothingIsRefused(t *testing.T) {
	root := fakeRoot(t, map[string]string{"cmd/x/main.go": "package main\n\nfunc main() {}\n"})
	// The grant names x, so the run reaches the floor rather than stopping at
	// the earlier refusal for a grant that matches no binary.
	dir := tfTree(t, map[string]string{"iam.tf": grantFor("x", "roles/cloudkms.cryptoKeyEncrypterDecrypter")})
	_, err := Check(root, dir)
	if err == nil || !strings.Contains(err.Error(), "reaches a cipher Encrypt or Decrypt") {
		t.Fatalf("error = %v, want the no-call-sites floor", err)
	}
}

// TestAMissingCmdTreeIsRefused reaches the cmd/ floor specifically — with a
// valid go.mod, so it cannot pass on the earlier module-path error instead.
func TestAMissingCmdTreeIsRefused(t *testing.T) {
	_, err := readNeeds(fakeRoot(t, nil))
	if err == nil || !strings.Contains(err.Error(), "cmd") {
		t.Fatalf("error = %v, want one naming the missing cmd/ tree", err)
	}
}

// TestANestedBinaryIsRefused: a binary at cmd/<name>/<sub> is walked from
// nowhere and maps to no identity, so it must not pass unread.
func TestANestedBinaryIsRefused(t *testing.T) {
	root := fakeRoot(t, map[string]string{
		"cmd/admin/doc.go":         "package main\n\nfunc main() {}\n",
		"cmd/admin/rotate/main.go": "package main\n\nfunc main() {}\n",
	})
	_, err := readNeeds(root)
	if err == nil || !strings.Contains(err.Error(), "below cmd/<name>") {
		t.Fatalf("error = %v, want the nested-binary refusal", err)
	}
}

// TestReadNeedsOrdersSitesNumerically covers the CALL of sortSites, not just the
// function: the two sites below sit at lines that sort one way as text and the
// other as numbers, so a sort.Strings where readNeeds calls sortSites cannot
// pass this. Without it the comparator is tested and its use is not.
func TestReadNeedsOrdersSitesNumerically(t *testing.T) {
	root := fakeRoot(t, map[string]string{"cmd/x/main.go": `package main

type c struct{}

func (c) Decrypt(b []byte) []byte { return b }

func main() {
	var v c
	_ = v.Decrypt
	_ = 0
	_ = 0
	_ = v.Decrypt
}
`})
	rows, err := readNeeds(root)
	if err != nil {
		t.Fatalf("readNeeds: %v", err)
	}
	// Line 5 is the method DECLARATION, which counts too — the scan matches
	// identifiers, and over-counting is the direction it may be wrong in.
	want := []string{"cmd/x/main.go:5 Decrypt", "cmd/x/main.go:9 Decrypt", "cmd/x/main.go:12 Decrypt"}
	if len(rows) != 1 || len(rows[0].Sites) != len(want) {
		t.Fatalf("sites = %v, want %v", rows[0].Sites, want)
	}
	for i := range want {
		if rows[0].Sites[i] != want[i] {
			t.Fatalf("sites = %v, want %v", rows[0].Sites, want)
		}
	}
}

func TestSitesSortNumerically(t *testing.T) {
	got := []string{"a.go:1106 Encrypt", "a.go:651 Encrypt", "a.go:651 Decrypt", "b.go:2 Encrypt"}
	sortSites(got)
	want := []string{"a.go:651 Decrypt", "a.go:651 Encrypt", "a.go:1106 Encrypt", "b.go:2 Encrypt"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortSites = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The rule, and the Terraform half.
// ---------------------------------------------------------------------------

// TestTheFixtureTreePasses is the control every case below needs: the scratch
// directory itself must not be what makes them fail.
func TestTheFixtureTreePasses(t *testing.T) {
	if r := mustCheck(t, tfTree(t, nil)); len(r.Findings) != 0 {
		t.Fatalf("the unmutated fixture fails: %v", r.Findings)
	}
}

// TestANarrowedRoleFails is #748 itself: the executor granted Encrypter while
// its code decrypts. Nothing in the deployment failed at the time — the
// cipher's startup probe only encrypts — so this is the check that was missing.
func TestANarrowedRoleFails(t *testing.T) {
	dir := tfTree(t, map[string]string{"iam.tf": rewrite(t,
		`"roles/cloudkms.cryptoKeyEncrypterDecrypter"`,
		`"roles/cloudkms.cryptoKeyEncrypter"`)})
	r := mustCheck(t, dir)
	for _, b := range []string{"controlplane", "executor"} {
		f := findingFor(r, b)
		if f == nil {
			t.Fatalf("%s decrypts under an Encrypter-only role and was not reported: %v", b, r.Findings)
		}
		if f.Rule != "under-granted" {
			t.Errorf("%s: rule %q, want under-granted", b, f.Rule)
		}
		if !strings.Contains(f.Msg, "missing Decrypt") {
			t.Errorf("%s: %q does not name the missing permission", b, f.Msg)
		}
	}
	// The message has to carry the site, or a red run says an identity is
	// under-granted without saying what demanded the permission.
	if f := findingFor(r, "executor"); !strings.Contains(f.Msg, "internal/executor/repos.go:") {
		t.Errorf("the executor finding names no call site: %q", f.Msg)
	}
}

// TestTheDecrypterHalfIsCheckedToo guards the mirror image, so the rule is not
// accidentally one permission wide.
func TestTheDecrypterHalfIsCheckedToo(t *testing.T) {
	dir := tfTree(t, map[string]string{"iam.tf": rewrite(t,
		`"roles/cloudkms.cryptoKeyEncrypterDecrypter"`,
		`"roles/cloudkms.cryptoKeyDecrypter"`)})
	f := findingFor(mustCheck(t, dir), "controlplane")
	if f == nil || !strings.Contains(f.Msg, "missing Encrypt") {
		t.Fatalf("a Decrypter-only role was not reported as missing Encrypt")
	}
}

// TestARemovedGrantFails covers the other way a grant goes missing: not
// narrowed, absent.
func TestARemovedGrantFails(t *testing.T) {
	only, _, _ := strings.Cut(fixtureIAM, executorHeader)
	r := mustCheck(t, tfTree(t, map[string]string{"iam.tf": only}))
	f := findingFor(r, "executor")
	if f == nil {
		t.Fatalf("the executor holds no grant at all and was not reported: %v", r.Findings)
	}
	if f.Rule != "ungranted" {
		t.Errorf("rule %q, want ungranted", f.Rule)
	}
	if findingFor(r, "controlplane") != nil {
		t.Errorf("removing the executor grant also reported the controlplane: %v", r.Findings)
	}
}

// TestGrantsInTwoRootsAreRefused is the sharpest silent pass this guard had:
// foundation/ and environment/ are separate states and separate applies, so a
// wide grant in one must never answer for a narrowed grant in the other.
func TestGrantsInTwoRootsAreRefused(t *testing.T) {
	narrowed := strings.ReplaceAll(fixtureIAM,
		`"roles/cloudkms.cryptoKeyEncrypterDecrypter"`,
		`"roles/cloudkms.cryptoKeyEncrypter"`)
	wide := executorHeader + `
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyDecrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
}
`
	wantRefusal(t, tfTree(t, map[string]string{
		"environment/iam.tf": narrowed,
		"foundation/iam.tf":  wide,
	}), "more than one Terraform root")
}

// TestTwoGrantsInOneRootUnion: the union is correct within a root, and it is the
// only place grant state is combined — an edit turning it into an intersection
// would silently under-report.
func TestTwoGrantsInOneRootUnion(t *testing.T) {
	split := `resource "google_kms_crypto_key_iam_member" "controlplane" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = "serviceAccount:${data.google_service_account.controlplane.email}"
}

resource "google_kms_crypto_key_iam_member" "executor_enc" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyEncrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
}

resource "google_kms_crypto_key_iam_member" "executor_dec" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyDecrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
}
`
	if r := mustCheck(t, tfTree(t, map[string]string{"iam.tf": split})); len(r.Findings) != 0 {
		t.Fatalf("two narrow grants in one root did not union: %v", r.Findings)
	}
}

// TestAGrantOnAnotherKeyDoesNotAnswerForTheCipher is a mutation whose wrong
// answer would be a silent pass rather than a false alarm.
func TestAGrantOnAnotherKeyDoesNotAnswerForTheCipher(t *testing.T) {
	dir := tfTree(t, map[string]string{"iam.tf": rewrite(t, executorKey,
		executorHeader+"\n  crypto_key_id = data.google_kms_crypto_key.other.id")})
	if f := findingFor(mustCheck(t, dir), "executor"); f == nil || f.Rule != "ungranted" {
		t.Fatalf("a grant on another key answered for the cipher")
	}
}

func TestAnUnattributableMemberIsRefused(t *testing.T) {
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": rewrite(t,
		`"serviceAccount:${data.google_service_account.executor.email}"`,
		`"serviceAccount:map-executor@example.iam.gserviceaccount.com"`)}),
		"cannot say which binary")
}

// TestAGrantToANonBinaryIsRefused: a grant this guard can read but cannot match
// to a binary is a grant whose rule it never evaluates.
func TestAGrantToANonBinaryIsRefused(t *testing.T) {
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": rewrite(t,
		"google_service_account.executor.email",
		"google_service_account.workloads.email")}),
		"is not a cmd/ binary")
}

func TestAnUnknownRoleIsRefused(t *testing.T) {
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": rewrite(t,
		`"roles/cloudkms.cryptoKeyEncrypterDecrypter"`,
		`"projects/p/roles/customCipherUser"`)}),
		"outside the set this guard understands")
}

func TestAConditionalGrantIsRefused(t *testing.T) {
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": rewrite(t, executorHeader,
		executorHeader+"\n  condition {\n    title      = \"t\"\n    expression = \"false\"\n  }")}),
		"opens a condition block")
}

// TestASingleLineConditionIsRefused: a nested block written on one line nets to
// zero braces, so brace arithmetic would miss it and credit the grant
// unconditionally.
func TestASingleLineConditionIsRefused(t *testing.T) {
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": rewrite(t, executorHeader,
		executorHeader+"\n  condition { expression = \"false\" }")}),
		"opens a condition block")
}

// TestALifecycleBlockIsFine: `lifecycle` and `timeouts` are meta-blocks that
// cannot change who is granted what, so refusing them would be a false alarm.
func TestALifecycleBlockIsFine(t *testing.T) {
	dir := tfTree(t, map[string]string{"iam.tf": rewrite(t, executorHeader,
		executorHeader+"\n  lifecycle {\n    prevent_destroy = true\n  }")})
	if r := mustCheck(t, dir); len(r.Findings) != 0 {
		t.Fatalf("a lifecycle block produced findings: %v", r.Findings)
	}
}

// TestAnAllowedBlockCannotShadowACondition: the test above legitimizes exactly
// the ordering that opens the hole. Reading only the FIRST nested block let a
// `lifecycle` written above a `condition` hide it, and a conditional grant
// credited unconditionally is what the refusal exists to stop.
func TestAnAllowedBlockCannotShadowACondition(t *testing.T) {
	for _, allowed := range []string{"lifecycle {\n    prevent_destroy = true\n  }", "timeouts {\n    create = \"5m\"\n  }"} {
		wantRefusal(t, tfTree(t, map[string]string{"iam.tf": rewrite(t, executorHeader,
			executorHeader+"\n  "+allowed+"\n  condition {\n    expression = \"false\"\n  }")}),
			"opens a condition block")
	}
}

// TestARefusalDoesNotFireOnAnotherKey: which key a block grants is read before
// any refusal, so a conditional or counted grant on some unrelated crypto key —
// legitimate Terraform — does not fail the build.
func TestARefusalDoesNotFireOnAnotherKey(t *testing.T) {
	for name, body := range map[string]string{
		"conditional": `resource "google_kms_crypto_key_iam_member" "signing" {
  crypto_key_id = data.google_kms_crypto_key.signing.id
  condition {
    expression = "false"
  }
  role   = "roles/cloudkms.cryptoKeyDecrypter"
  member = "serviceAccount:${data.google_service_account.executor.email}"
}
`,
		"for_each": `resource "google_kms_crypto_key_iam_member" "signing" {
  for_each      = toset(["a", "b"])
  crypto_key_id = data.google_kms_crypto_key.signing.id
  role          = "roles/cloudkms.cryptoKeyDecrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
}
`,
	} {
		r, err := Check(repoRoot(), tfTree(t, map[string]string{"iam.tf": fixtureIAM + "\n" + body}))
		if err != nil {
			t.Errorf("%s on another key was refused: %v", name, err)
			continue
		}
		if len(r.Findings) != 0 {
			t.Errorf("%s on another key produced findings: %v", name, r.Findings)
		}
	}
}

// TestAGrantOutsideTheAppliedRootsIsRefused: Terraform loads the .tf files of
// ONE directory, never recursively, and only two directories here are ever
// applied. A grant living anywhere else — a subdirectory below a root, or a
// sibling directory no apply target reads — would otherwise be credited as the
// whole configuration while the deployed reality grants nothing.
func TestAGrantOutsideTheAppliedRootsIsRefused(t *testing.T) {
	for name, path := range map[string]string{
		"below a root":          "environment/unused/iam.tf",
		"a root nobody applies": "attic/iam.tf",
		"loose at the top":      "iam.tf",
		// A directory that carries a root's NAME but not its place. Matching on
		// the base name alone would read this one as the real environment/.
		"a root's name one level down": "attic/environment/iam.tf",
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(fixtureIAM), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Check(repoRoot(), dir); err == nil {
			t.Errorf("%s (%s): a grant no apply reads was accepted", name, path)
		} else if !strings.Contains(err.Error(), "outside the Terraform roots") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestCountIsRefused: count = 0 makes Terraform create nothing while every
// attribute below still reads as a grant.
func TestCountIsRefused(t *testing.T) {
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": rewrite(t, executorHeader,
		executorHeader+"\n  count = 0")}), "carries count")
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": rewrite(t, executorHeader,
		executorHeader+"\n  for_each = toset([])")}), "carries for_each")
}

// TestAnOverrideFileIsRefused: Terraform merges an override into the resource of
// the same address and REPLACES its attributes, so unioning it — which is what
// this guard does with two blocks — would mask a narrowing.
func TestAnOverrideFileIsRefused(t *testing.T) {
	narrowing := executorHeader + `
  role = "roles/cloudkms.cryptoKeyEncrypter"
}
`
	wantRefusal(t, tfTree(t, map[string]string{
		"iam.tf":          fixtureIAM,
		"iam_override.tf": narrowing,
	}), "replace attributes rather than add")
	wantRefusal(t, tfTree(t, map[string]string{
		"iam.tf":      fixtureIAM,
		"override.tf": narrowing,
	}), "replace attributes rather than add")
}

// TestAModuleIsRefused: a module's configuration is somewhere this guard does
// not look, and an IAM policy inside one revokes the members read here.
func TestAModuleIsRefused(t *testing.T) {
	wantRefusal(t, tfTree(t, map[string]string{
		"iam.tf": fixtureIAM + "\nmodule \"kms_extra\" {\n  source = \"terraform-google-modules/kms/google\"\n}\n",
	}), "calls a module")
}

func TestABindingInsteadOfAMemberIsRefused(t *testing.T) {
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": rewrite(t,
		`resource "google_kms_crypto_key_iam_member" "executor"`,
		`resource "google_kms_crypto_key_iam_binding" "executor"`)}),
		"can carry the key's permissions past it unseen")
}

// TestABindingOnAnotherKeyIsFine: refusing a binding on a key this guard does
// not care about would be a false alarm.
func TestABindingOnAnotherKeyIsFine(t *testing.T) {
	other := `resource "google_kms_crypto_key_iam_binding" "elsewhere" {
  crypto_key_id = data.google_kms_crypto_key.other.id
  role          = "roles/cloudkms.cryptoKeyDecrypter"
  members       = ["serviceAccount:x@example.iam.gserviceaccount.com"]
}
`
	dir := tfTree(t, map[string]string{"iam.tf": fixtureIAM + "\n" + other})
	if r := mustCheck(t, dir); len(r.Findings) != 0 {
		t.Fatalf("a binding on an unrelated key produced findings: %v", r.Findings)
	}
}

// TestAWideCloudKMSGrantIsRefused: a cloudkms role granted at project level
// would make a narrow key-level grant harmless, so this guard's failure would
// be a false alarm. Refusing says which of the two it is.
func TestAWideCloudKMSGrantIsRefused(t *testing.T) {
	wide := `resource "google_project_iam_member" "wide" {
  project = "p"
  role    = "roles/cloudkms.cryptoKeyDecrypter"
  member  = "serviceAccount:${data.google_service_account.executor.email}"
}
`
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": fixtureIAM + "\n" + wide}), "above a single key")
}

// TestAWideRoleThisGuardCannotReadIsIgnoredNotRefused: a project-level role it
// cannot resolve — a custom role, or `each.value` under a for_each — is ignored.
// Ignoring a wide grant can only ADD permissions this guard never had, so the
// worst it produces is a finding a wider grant would have excused: a false
// alarm, never a silent pass. Refusing instead would fail CI on the ordinary way
// project roles are written.
func TestAWideRoleThisGuardCannotReadIsIgnoredNotRefused(t *testing.T) {
	for name, wide := range map[string]string{
		"custom role": `resource "google_project_iam_member" "custom" {
  project = "p"
  role    = "projects/p/roles/cipherUser"
  member  = "serviceAccount:${data.google_service_account.executor.email}"
}
`,
		"for_each over roles": `resource "google_project_iam_member" "executor_roles" {
  for_each = toset(["roles/logging.logWriter", "roles/monitoring.metricWriter"])
  project  = "p"
  role     = each.value
  member   = "serviceAccount:${data.google_service_account.executor.email}"
}
`,
	} {
		dir := tfTree(t, map[string]string{"iam.tf": fixtureIAM + "\n" + wide})
		r, err := Check(repoRoot(), dir)
		if err != nil {
			t.Errorf("%s: refused a legitimate project grant: %v", name, err)
			continue
		}
		if len(r.Findings) != 0 {
			t.Errorf("%s: %v", name, r.Findings)
		}
	}
}

// TestAWideGrantToSomeoneElseIsIgnored: nothing granted to a principal that is
// not one of these identities can change what they may do.
func TestAWideGrantToSomeoneElseIsIgnored(t *testing.T) {
	// roles/cloudkms.admin is the case that exercises the member filter and
	// nothing else: a role outside cloudkms is excused by the prefix check
	// further down whether or not the member was read, so a fixture using one
	// would agree with the guard instead of testing it.
	for name, role := range map[string]string{
		"a custom project role": "projects/p/roles/backupOperator",
		"a wide cloudkms role":  "roles/cloudkms.admin",
	} {
		other := `resource "google_project_iam_member" "backup" {
  project = "p"
  role    = "` + role + `"
  member  = "serviceAccount:${data.google_service_account.backup.email}"
}
`
		dir := tfTree(t, map[string]string{"iam.tf": fixtureIAM + "\n" + other})
		r, err := Check(repoRoot(), dir)
		if err != nil {
			t.Fatalf("%s granted to an identity that is not a cmd/ binary was refused: %v", name, err)
		}
		if len(r.Findings) != 0 {
			t.Fatalf("%s granted to an unrelated identity produced findings: %v", name, r.Findings)
		}
	}
}

// TestAProjectRoleThatIsNotCloudKMSIsFine keeps the refusals above from reading
// every project-level grant as a hazard — the tree has three.
func TestAProjectRoleThatIsNotCloudKMSIsFine(t *testing.T) {
	ok := `resource "google_project_iam_member" "sql" {
  project = "p"
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${data.google_service_account.brain.email}"
}
`
	dir := tfTree(t, map[string]string{"iam.tf": fixtureIAM + "\n" + ok})
	if r := mustCheck(t, dir); len(r.Findings) != 0 {
		t.Fatalf("a cloudsql project grant produced findings: %v", r.Findings)
	}
}

// TestAProjectIAMPolicyIsRefused: a policy resource has no role at all and is
// authoritative over the whole project, so "assigns no role" would be an
// unsatisfiable complaint about the wrong thing.
func TestAProjectIAMPolicyIsRefused(t *testing.T) {
	policy := `resource "google_project_iam_policy" "whole" {
  project     = "p"
  policy_data = data.google_iam_policy.admin.policy_data
}
`
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": fixtureIAM + "\n" + policy}),
		"whose contents this guard cannot read")
}

// TestNoGrantsAtAllIsRefused: the floor. A reader that stopped finding grants
// would otherwise report ok over nothing.
func TestNoGrantsAtAllIsRefused(t *testing.T) {
	if _, err := Check(repoRoot(), t.TempDir()); err == nil {
		t.Fatal("an empty directory passed")
	}
	wantRefusal(t, tfTree(t, map[string]string{"x.tf": "variable \"v\" {}\n"}),
		"about to report ok over nothing")
}

// TestJSONSyntaxIsRefused: a grant written as .tf.json would read as absent.
func TestJSONSyntaxIsRefused(t *testing.T) {
	wantRefusal(t, tfTree(t, map[string]string{"iam.tf": fixtureIAM, "extra.tf.json": "{}\n"}),
		"would read as absent")
}

// TestAGrantInsideADotDirectoryIsNotCredited: `.terraform/` is a download cache,
// and a grant found there must not answer for one the repository makes.
func TestAGrantInsideADotDirectoryIsNotCredited(t *testing.T) {
	only, _, _ := strings.Cut(fixtureIAM, executorHeader)
	planted := executorHeader + `
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
}
`
	r := mustCheck(t, tfTree(t, map[string]string{
		"iam.tf":                      only,
		".terraform/modules/m/iam.tf": planted,
	}))
	if f := findingFor(r, "executor"); f == nil || f.Rule != "ungranted" {
		t.Fatalf("a grant under .terraform/ answered for the repository's own: %v", r.Findings)
	}
}

func TestPermsArithmetic(t *testing.T) {
	both := Perms{Encrypt: true, Decrypt: true}
	enc := Perms{Encrypt: true}
	if got := both.missing(enc); got != (Perms{Decrypt: true}) {
		t.Errorf("both.missing(enc) = %+v", got)
	}
	if got := enc.missing(both); got.any() {
		t.Errorf("enc.missing(both) = %+v, want nothing", got)
	}
	if got := (Perms{}).String(); got != "neither" {
		t.Errorf("zero Perms prints %q", got)
	}
	if got := both.String(); got != "Encrypt and Decrypt" {
		t.Errorf("both prints %q", got)
	}
}
