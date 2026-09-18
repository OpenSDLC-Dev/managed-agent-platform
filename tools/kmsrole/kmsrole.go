// Package main implements kmsrole, the guard that holds each GCP identity's
// key-level KMS role to what its binary's code actually calls (#750).
//
// #748 was a Terraform role string kept correct only by the prose around it.
// Seven comments asserted the executor never decrypts; the code had decrypted
// for three tagged releases before anyone noticed; nothing failed until a fresh
// GCP project ran a session — the cipher's startup probe only
// encrypts, so both processes went Ready and the deploy was green while every
// clone of a private repository and every vault-credentialed MCP dial lost its
// credential. The fix corrected the role and the seven comments, which is the
// same mechanism that failed, applied again. This guard re-derives the fact
// instead: it reads the role from the Terraform and the cipher's call sites from
// the Go source, and fails when an identity is granted less than its binary
// calls.
//
// # Where it runs
//
// `make gcp-kms-role-check`, in the gcp-* group and NOT in `make verify`. It
// reads deploy/gcp's Terraform, and that tree is developer tooling for GCP
// deployment (plan 20, Decision 9) — never a dependency of the platform, its
// build, or the Go gate, which is why `gcp-split-check` guards a scarier
// invariant from outside the gate too. CI runs it beside the other gcp-* checks.
// This package's own test runs in `make verify` like any Go test, but it reads
// only fixtures and the Go tree, never deploy/gcp: the split mirrors
// check_split.py, whose unit tests and whose run against the real configuration
// are separate targets for the same reason.
//
// # The rule, and the direction it is allowed to be wrong in
//
// One-directional: an identity must be granted at least what its code calls. An
// identity granted MORE is not reported, because the failure being prevented is a
// narrowed grant and the grants here are the predefined pair either way. It is a
// floor, not a split — it can say the executor needs Decrypt, never that the
// brain should stop being able to encrypt.
//
// The Go-source half counts every selection of a member named Encrypt or
// Decrypt — `x.Decrypt`, called or merely taken as a method value — in every
// in-module package a binary transitively imports, read with go/parser so that
// neither a comment nor a string can contribute one. That over-counts on
// purpose, in three known ways: a member with those names on something other
// than secrets.Cipher counts, a struct field named Encrypt counts, and so does a
// cipher backend's own call to its cloud client (internal/secrets/gcpkms), which
// means any binary linking the backend inherits both permissions whatever its
// own code does. Over-counting demands a broader role and names the exact file
// and line that demanded it; under-counting prints ok over #748 happening again.
// When a binary appears that genuinely encrypts and never decrypts, the honest
// refinement is to ignore a call that sits inside a method which itself
// implements the seam — a backend's inner call, never a consumer's — and it is
// deliberately not built ahead of a caller that needs it.
//
// What a syntactic scan cannot see, it cannot count: a cipher reached by
// reflection (`MethodByName("Decrypt")`) is invisible here, and no amount of
// go/parser will change that. Imports are followed from every non-test .go file
// regardless of build constraints, which over-includes in the same safe
// direction.
//
// It assumes cmd/<name> runs as the service account labelled <name>. That is
// true of the three workloads and not of the other two: cmd/gate is a sidecar in
// the executor's pod and shares the executor's identity, and cmd/worker is
// customer-hosted with no GCP identity at all. Neither reaches the cipher today,
// so the assumption costs nothing; the day one does, this guard reports it
// ungranted and the mapping has to be taught here.
//
// # What it refuses
//
// A refusal is an error, not a finding: the guard could not do its job, and
// saying so is the whole point of the tool. It refuses a role outside the closed
// set below, a member it cannot attribute to a service account, a cipher grant
// whose identity is not a cmd/ binary, a grant carrying `count` or `for_each`
// (which can make it apply zero times), a grant with a nested block other than
// `lifecycle` or `timeouts`, an override file (whose semantics REPLACE rather
// than add, so unioning it is wrong), a `module` block (whose configuration lives
// where this guard does not look, and whose _iam_policy could revoke the members
// read here), a KMS grant of a kind it does not understand on this key, a
// cloudkms role granted above the key to one of these identities, and any .tf
// construct its reader cannot read (see hcl.go). It also refuses when grants
// appear in more than one Terraform root, when it found no grant at all, and when
// no binary reaches the cipher — the last two would let it print ok over nothing,
// which is how a checker's bug becomes the input it never reads.
//
// A grant on a crypto key OTHER than the cipher is not refused: it is simply not
// this key's grant, and ignoring it is the correct reading.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Perms is the pair the crypto key's predefined roles divide on.
type Perms struct {
	Encrypt bool
	Decrypt bool
}

func (p Perms) any() bool { return p.Encrypt || p.Decrypt }

// missing returns what p needs and granted does not carry.
func (p Perms) missing(granted Perms) Perms {
	return Perms{
		Encrypt: p.Encrypt && !granted.Encrypt,
		Decrypt: p.Decrypt && !granted.Decrypt,
	}
}

func (p Perms) String() string {
	switch {
	case p.Encrypt && p.Decrypt:
		return "Encrypt and Decrypt"
	case p.Encrypt:
		return "Encrypt"
	case p.Decrypt:
		return "Decrypt"
	default:
		return "neither"
	}
}

// rolePerms is the closed set of key-level predefined roles this guard
// understands. Anything else — a custom role, an admin role, a role granted
// through a delegation variant — is refused rather than mapped by guesswork,
// because a role wrongly credited with a permission is a silent pass over
// exactly the grant this guard exists to catch. roles/cloudkms.admin is
// deliberately absent: it administers keys and does not grant their use.
// Each entry was read from the live role definition with
// `gcloud iam roles describe` on 2026-09-19 rather than assumed, because the
// whole value of a closed set is that what is in it is right. cryptoOperator
// carries useToEncrypt and useToDecrypt among wider operations, so it satisfies
// this rule wherever the predefined pair does.
var rolePerms = map[string]Perms{
	"roles/cloudkms.cryptoKeyEncrypterDecrypter": {Encrypt: true, Decrypt: true},
	"roles/cloudkms.cryptoKeyEncrypter":          {Encrypt: true},
	"roles/cloudkms.cryptoKeyDecrypter":          {Decrypt: true},
	"roles/cloudkms.cryptoOperator":              {Encrypt: true, Decrypt: true},
}

const (
	// kmsMemberKind is the one KMS grant shape this guard reads. A _binding or
	// _iam_policy resource replaces rather than adds to the members on a key,
	// so reading one as if it were a member would be wrong in both directions.
	kmsMemberKind = "google_kms_crypto_key_iam_member"

	// cipherKeyLabel is the Terraform label of the key the platform's cipher
	// uses — google_kms_crypto_key.cipher in foundation/, read back as
	// data.google_kms_crypto_key.cipher in environment/. Pinning the name is
	// the one small list here, and the alternative is worse: crediting a grant
	// on whatever key happens to be named would let a grant on an unrelated key
	// answer for the cipher. A rename fails this guard loudly, which is the
	// intended way to be told to teach it.
	cipherKeyLabel = "cipher"
)

var (
	// A literal string assignment, with an optional trailing comment. Anything
	// else — a variable, a conditional, a function call — is refused: this guard
	// does not evaluate HCL, and a value it cannot read is a value it must not
	// report on.
	tfStringRe = regexp.MustCompile(`^\s*[a-z_]+\s*=\s*"([^"]*)"\s*(?:#.*|//.*)?$`)
	// An unquoted reference, which is how crypto_key_id is written.
	tfRefRe = regexp.MustCompile(`^\s*[a-z_]+\s*=\s*([A-Za-z0-9_.]+)\s*(?:#.*|//.*)?$`)
	// The member form the tree uses, and the only one attributable to a binary.
	tfSAMemberRe = regexp.MustCompile(`^serviceAccount:\$\{(?:data\.)?google_service_account\.([A-Za-z0-9_]+)\.email\}$`)
	// A crypto_key_id reference, whose label says which key is being granted.
	tfKeyRefRe = regexp.MustCompile(`^(?:data\.)?google_kms_crypto_key\.([A-Za-z0-9_]+)\.id$`)
	// The kinds that grant a role somewhere above a single key.
	tfWideIAMRe = regexp.MustCompile(`^google_(?:project|folder|organization)_iam_(member|binding|policy)$`)
	// Every KMS IAM kind, so one this guard does not read cannot pass unseen.
	tfKMSIAMRe = regexp.MustCompile(`^google_kms_(?:crypto_key|key_ring)_iam_`)
	// Terraform's override files, which MERGE into the resource of the same
	// address and REPLACE its attributes.
	tfOverrideRe = regexp.MustCompile(`(^|_)override\.tf$`)
	// A predefined role. A custom role's permissions cannot be read from here.
	tfPredefinedRoleRe = regexp.MustCompile(`^roles/[A-Za-z0-9.]+$`)
)

// Grant is one identity's key-level role on the cipher key.
type Grant struct {
	Role  string
	Perms Perms
	Where string
}

// Row is what the guard derived about one cmd/ binary: what its code calls, the
// sites that say so, and the grant its identity holds — nil when nothing grants
// it on the cipher key.
type Row struct {
	Binary string
	Needs  Perms
	Sites  []string
	Grant  *Grant
}

// Finding is one identity granted less than its binary calls.
type Finding struct {
	Binary string
	Rule   string
	Msg    string
}

func (f Finding) String() string { return fmt.Sprintf("[%s] %s: %s", f.Rule, f.Binary, f.Msg) }

// Report is one run: a row per binary, and whatever the rule found.
type Report struct {
	Rows     []Row
	Findings []Finding
}

// Check derives both halves and applies the rule. root is the repository root
// (it holds go.mod, cmd/ and the packages they import); tfDir is the directory
// whose .tf files carry the grants, read recursively. They are separate
// parameters so a test can hold the real Go tree against fixture Terraform, and
// so `make gcp-kms-role-check` can hold both against the tree.
func Check(root, tfDir string) (Report, error) {
	rows, err := readNeeds(root)
	if err != nil {
		return Report{}, err
	}
	binaries := map[string]bool{}
	for _, r := range rows {
		binaries[r.Binary] = true
	}

	grants, err := readGrants(tfDir, binaries)
	if err != nil {
		return Report{}, err
	}
	if len(grants) == 0 {
		return Report{}, fmt.Errorf("no %s naming a service account on %q was found under %s — either the grants moved or this guard stopped reading them, and both mean it is about to report ok over nothing", kmsMemberKind, cipherKeyLabel, tfDir)
	}

	reaches := false
	for i := range rows {
		if g, ok := grants[rows[i].Binary]; ok {
			rows[i].Grant = &g
		}
		if rows[i].Needs.any() {
			reaches = true
		}
	}
	if !reaches {
		return Report{}, fmt.Errorf("no binary under %s reaches a cipher Encrypt or Decrypt — the scan found nothing to hold the grants to, so its ok would mean nothing", filepath.Join(root, "cmd"))
	}

	var findings []Finding
	for _, r := range rows {
		if !r.Needs.any() {
			continue
		}
		if r.Grant == nil {
			findings = append(findings, Finding{
				Binary: r.Binary,
				Rule:   "ungranted",
				Msg: fmt.Sprintf("calls %s on the cipher, and no %s on %q names it. Call sites: %s",
					r.Needs, kmsMemberKind, cipherKeyLabel, strings.Join(r.Sites, ", ")),
			})
			continue
		}
		if m := r.Needs.missing(r.Grant.Perms); m.any() {
			findings = append(findings, Finding{
				Binary: r.Binary,
				Rule:   "under-granted",
				Msg: fmt.Sprintf("calls %s but %s at %s carries only %s — missing %s. Call sites: %s",
					r.Needs, r.Grant.Role, r.Grant.Where, r.Grant.Perms, m, strings.Join(r.Sites, ", ")),
			})
		}
	}
	return Report{Rows: rows, Findings: findings}, nil
}

// readGrants reads the key-level grants on the cipher key under dir, keyed by
// the service-account label its member names.
//
// The .tf files are grouped by the directory holding them, because that is what
// Terraform calls a root: a separate state and a separate apply. Grants are
// unioned WITHIN a root and never across them — `foundation/` and `environment/`
// are applied independently, and unioning them would let a grant in one mask a
// narrowed grant in the other, which is #748 with an extra step.
func readGrants(dir string, binaries map[string]bool) (map[string]Grant, error) {
	byRoot := map[string][]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A dot-directory is not this repository's configuration. `.terraform/`
		// is a download cache, and reading it would be wrong in both
		// directions: a construct this reader refuses would fail the guard over
		// a vendored module, and a grant to a name that happened to match a
		// cmd/ binary would be credited to it.
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(p, ".tf.json"):
			// Machine-generated HCL this reader does not parse at all. Refusing
			// keeps a grant written that way from reading as absent.
			return fmt.Errorf("%s: this guard reads .tf only, and a grant in JSON syntax would read as absent", p)
		case tfOverrideRe.MatchString(d.Name()):
			// An override file MERGES into the resource of the same address and
			// REPLACES its attributes, so a role written here is the effective
			// role. This guard reads blocks independently and unions them, which
			// would let an override that NARROWS a grant be masked by the base
			// block it replaced.
			return fmt.Errorf("%s: Terraform override files replace attributes rather than add to them, which this guard does not model — express the grant in one place", p)
		case strings.HasSuffix(p, ".tf"):
			root := filepath.Dir(p)
			byRoot[root] = append(byRoot[root], p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(byRoot) == 0 {
		return nil, fmt.Errorf("no .tf files under %s", dir)
	}

	roots := make([]string, 0, len(byRoot))
	for r := range byRoot {
		roots = append(roots, r)
	}
	sort.Strings(roots)

	var granting []string
	out := map[string]Grant{}
	for _, root := range roots {
		files := byRoot[root]
		sort.Strings(files)
		here := map[string]Grant{}
		for _, f := range files {
			if err := readFileGrants(f, binaries, here); err != nil {
				return nil, err
			}
		}
		if len(here) == 0 {
			continue
		}
		granting = append(granting, root)
		for label, g := range here {
			out[label] = g
		}
	}
	if len(granting) > 1 {
		return nil, fmt.Errorf("grants on the %q key appear in more than one Terraform root (%s) — each root is a separate state and a separate apply, so this guard cannot tell which are applied together, and unioning them across roots is how a narrowed grant in one is masked by a wider grant in another", cipherKeyLabel, strings.Join(granting, ", "))
	}
	return out, nil
}

func readFileGrants(path string, binaries map[string]bool, into map[string]Grant) error {
	blocks, err := tfBlocks(path)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if b.Type == "module" {
			// The module's own configuration is somewhere this guard does not
			// look, and check_split.py refuses a module it cannot follow for
			// the same reason. A grant a module MAKES would only read as absent
			// here — the safe direction — but a google_kms_crypto_key_iam_policy
			// inside one is authoritative for its (key, role) pair and would
			// REVOKE the members read here, which is not.
			return fmt.Errorf("%s calls a module, whose configuration this guard does not read — an IAM policy inside it can revoke the members read here", b.Addr())
		}
		if err := wideIAMOK(b, binaries); err != nil {
			return err
		}
		if !tfKMSIAMRe.MatchString(b.Kind) {
			continue
		}
		if b.Kind != kmsMemberKind {
			// Before refusing, ask which key it grants: a binding or policy on
			// some other key is none of this guard's business.
			if other, err := grantsAnotherKey(b); err != nil {
				return err
			} else if other {
				continue
			}
			return fmt.Errorf("%s: this guard reads %s only, and a %s grant can carry the key's permissions past it unseen", b.Addr(), kmsMemberKind, b.Kind)
		}
		label, g, ok, err := readGrant(b, binaries)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		// Two members for the same identity on the same key are legal and their
		// permissions union, so union them rather than refusing.
		if prev, seen := into[label]; seen {
			g.Perms = Perms{Encrypt: g.Perms.Encrypt || prev.Perms.Encrypt, Decrypt: g.Perms.Decrypt || prev.Perms.Decrypt}
			g.Role = prev.Role + " + " + g.Role
			g.Where = prev.Where + ", " + g.Where
		}
		into[label] = g
	}
	return nil
}

// grantsAnotherKey reports whether a KMS IAM block this guard does not read is
// at least aimed at a key other than the cipher. A key-ring-level grant covers
// every key on the ring, so it is never "another key".
func grantsAnotherKey(b tfBlock) (bool, error) {
	if !strings.HasPrefix(b.Kind, "google_kms_crypto_key_iam_") {
		return false, nil
	}
	line, has, err := b.attr("crypto_key_id")
	if err != nil {
		return false, err
	}
	if !has {
		return false, nil
	}
	m := tfRefRe.FindStringSubmatch(line.Raw)
	if m == nil {
		return false, nil
	}
	kr := tfKeyRefRe.FindStringSubmatch(m[1])
	return kr != nil && kr[1] != cipherKeyLabel, nil
}

// wideIAMOK refuses a role granted above a single key that this guard cannot
// rule out as a cloudkms grant to one of these identities. Such a grant would
// make a narrow key-level grant harmless, so this guard's failure would be a
// false alarm — refusing says which of the two it is. A grant to anyone else is
// not this guard's business and is not read at all.
func wideIAMOK(b tfBlock, binaries map[string]bool) error {
	m := tfWideIAMRe.FindStringSubmatch(b.Kind)
	if m == nil {
		return nil
	}
	if m[1] == "policy" {
		// A policy resource carries opaque policy_data and has no role at all;
		// it is also authoritative, replacing every binding on the project.
		return fmt.Errorf("%s sets a whole IAM policy, whose contents this guard cannot read — it may grant or revoke cloudkms above the key", b.Addr())
	}
	if m[1] == "member" {
		// A member block names one principal. If it is not one of these
		// identities, nothing about it can change what they may do.
		member, has, err := b.attr("member")
		if err != nil {
			return err
		}
		if has {
			if sm := tfStringRe.FindStringSubmatch(member.Raw); sm != nil {
				sa := tfSAMemberRe.FindStringSubmatch(sm[1])
				if sa == nil || !binaries[sa[1]] {
					return nil
				}
			}
		}
	}
	line, has, err := b.attr("role")
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("%s assigns no role", b.Addr())
	}
	sm := tfStringRe.FindStringSubmatch(line.Raw)
	if sm == nil {
		return fmt.Errorf("%s:%d: cannot read the role as a literal string, so this guard cannot tell whether it grants cloudkms above the key", b.File, line.N)
	}
	switch {
	case strings.HasPrefix(sm[1], "roles/cloudkms."):
		return fmt.Errorf("%s grants %s above a single key, which this guard does not read — a key-level grant narrower than the code calls would then be harmless and this guard would report a failure that is not one", b.Addr(), sm[1])
	case !tfPredefinedRoleRe.MatchString(sm[1]):
		// A custom role's permissions live in the project, not here, so a
		// cloudkms permission inside one is invisible to a prefix test.
		return fmt.Errorf("%s grants the custom role %s above a single key, and this guard cannot read what a custom role permits — it may carry a cloudkms permission", b.Addr(), sm[1])
	}
	return nil
}

// readGrant reads one KMS member block. ok is false when the block grants a key
// other than the cipher, which is not this guard's business.
func readGrant(b tfBlock, binaries map[string]bool) (label string, g Grant, ok bool, err error) {
	if n := b.nested(); n != "" && n != "lifecycle" && n != "timeouts" {
		return "", Grant{}, false, fmt.Errorf("%s opens a %s block — a `condition` makes the grant conditional, and reading role and member flat would credit it unconditionally", b.Addr(), n)
	}
	// count = 0 or an empty for_each makes the grant apply zero times while
	// every attribute below still reads as a grant.
	for _, meta := range []string{"count", "for_each"} {
		if _, has, err := b.attr(meta); err != nil {
			return "", Grant{}, false, err
		} else if has {
			return "", Grant{}, false, fmt.Errorf("%s carries %s, which decides how many times the grant is made — this guard reads the attributes, not the count", b.Addr(), meta)
		}
	}

	keyLine, has, err := b.attr("crypto_key_id")
	if err != nil {
		return "", Grant{}, false, err
	}
	if !has {
		return "", Grant{}, false, fmt.Errorf("%s assigns no crypto_key_id", b.Addr())
	}
	km := tfRefRe.FindStringSubmatch(keyLine.Raw)
	if km == nil {
		return "", Grant{}, false, fmt.Errorf("%s:%d: cannot read crypto_key_id as a resource reference, so this guard cannot tell which key is granted", b.File, keyLine.N)
	}
	kr := tfKeyRefRe.FindStringSubmatch(km[1])
	if kr == nil {
		return "", Grant{}, false, fmt.Errorf("%s:%d: crypto_key_id %q is not a google_kms_crypto_key reference this guard can attribute to a key", b.File, keyLine.N, km[1])
	}
	if kr[1] != cipherKeyLabel {
		return "", Grant{}, false, nil
	}

	memberLine, has, err := b.attr("member")
	if err != nil {
		return "", Grant{}, false, err
	}
	if !has {
		return "", Grant{}, false, fmt.Errorf("%s assigns no member", b.Addr())
	}
	mm := tfStringRe.FindStringSubmatch(memberLine.Raw)
	if mm == nil {
		return "", Grant{}, false, fmt.Errorf("%s:%d: cannot read member as a literal string", b.File, memberLine.N)
	}
	// An unattributable member fails here rather than falling through as "no
	// grant", which would read as a binary that needs nothing.
	sm := tfSAMemberRe.FindStringSubmatch(mm[1])
	if sm == nil {
		return "", Grant{}, false, fmt.Errorf("%s:%d: member %q is not a service_account.<name>.email reference, so this guard cannot say which binary holds it", b.File, memberLine.N, mm[1])
	}
	if !binaries[sm[1]] {
		// A grant this guard can read but cannot match to a binary is a grant
		// whose rule it never evaluates. Dropping it silently is how a renamed
		// identity stops being checked without anything going red.
		return "", Grant{}, false, fmt.Errorf("%s:%d: the cipher is granted to %q, which is not a cmd/ binary — this guard maps cmd/<name> to the service account labelled <name> and has nothing to hold this grant to", b.File, memberLine.N, sm[1])
	}

	roleLine, has, err := b.attr("role")
	if err != nil {
		return "", Grant{}, false, err
	}
	if !has {
		return "", Grant{}, false, fmt.Errorf("%s assigns no role", b.Addr())
	}
	rm := tfStringRe.FindStringSubmatch(roleLine.Raw)
	if rm == nil {
		return "", Grant{}, false, fmt.Errorf("%s:%d: cannot read role as a literal string", b.File, roleLine.N)
	}
	perms, known := rolePerms[rm[1]]
	if !known {
		return "", Grant{}, false, fmt.Errorf("%s:%d: role %q is outside the set this guard understands, and guessing its permissions is how a narrow grant passes", b.File, roleLine.N, rm[1])
	}
	return sm[1], Grant{
		Role:  rm[1],
		Perms: perms,
		Where: fmt.Sprintf("%s:%d", b.File, roleLine.N),
	}, true, nil
}

// readNeeds derives what each cmd/ binary calls, one row per binary.
func readNeeds(root string) ([]Row, error) {
	mod, err := modulePath(root)
	if err != nil {
		return nil, err
	}
	cmdDir := filepath.Join(root, "cmd")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		return nil, err
	}
	s := &scanner{root: root, mod: mod, fset: token.NewFileSet(), pkgs: map[string]*pkgInfo{}}
	var rows []Row
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// A binary nested deeper than cmd/<name> would never be walked from
		// here, and its identity is not cmd/<name> either. Refusing beats
		// leaving a whole binary unread.
		if err := noNestedMain(filepath.Join(cmdDir, e.Name())); err != nil {
			return nil, err
		}
		row := Row{Binary: e.Name()}
		seen := map[string]bool{}
		if err := s.reach(filepath.ToSlash(filepath.Join("cmd", e.Name())), seen, &row); err != nil {
			return nil, err
		}
		sortSites(row.Sites)
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no binaries under %s", cmdDir)
	}
	return rows, nil
}

// noNestedMain refuses a `package main` below cmd/<name>.
func noNestedMain(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Dir(p) == dir {
			return err
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, parser.PackageClauseOnly)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", p, perr)
		}
		if f.Name.Name == "main" {
			return fmt.Errorf("%s is a binary below cmd/<name>, which this guard neither walks nor maps to an identity", filepath.Dir(p))
		}
		return nil
	})
}

// sortSites orders by file then by line NUMERICALLY — sort.Strings would put
// line 1106 before line 651 in the one artifact an operator reads while deciding
// which role string to widen.
func sortSites(sites []string) {
	key := func(s string) (string, int, string) {
		file, rest, _ := strings.Cut(s, ":")
		num, call, _ := strings.Cut(rest, " ")
		n := 0
		fmt.Sscanf(num, "%d", &n)
		return file, n, call
	}
	sort.Slice(sites, func(i, j int) bool {
		fi, ni, ci := key(sites[i])
		fj, nj, cj := key(sites[j])
		if fi != fj {
			return fi < fj
		}
		if ni != nj {
			return ni < nj
		}
		return ci < cj
	})
}

func modulePath(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("%s declares no module path", filepath.Join(root, "go.mod"))
}

// pkgInfo is one package's contribution, parsed once however many binaries
// reach it.
type pkgInfo struct {
	Imports []string
	Perms   Perms
	Sites   []string
}

type scanner struct {
	root string
	mod  string
	fset *token.FileSet
	pkgs map[string]*pkgInfo
}

// reach unions pkg and everything it transitively imports into row.
func (s *scanner) reach(pkg string, seen map[string]bool, row *Row) error {
	if seen[pkg] {
		return nil
	}
	seen[pkg] = true
	info, err := s.pkg(pkg)
	if err != nil {
		return err
	}
	row.Needs.Encrypt = row.Needs.Encrypt || info.Perms.Encrypt
	row.Needs.Decrypt = row.Needs.Decrypt || info.Perms.Decrypt
	row.Sites = append(row.Sites, info.Sites...)
	for _, imp := range info.Imports {
		if err := s.reach(imp, seen, row); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) pkg(pkg string) (*pkgInfo, error) {
	if info, ok := s.pkgs[pkg]; ok {
		return info, nil
	}
	dir := filepath.Join(s.root, filepath.FromSlash(pkg))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("package %s is imported but unreadable: %w", pkg, err)
	}
	info := &pkgInfo{}
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(s.fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", filepath.Join(pkg, name), err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			switch {
			case path == s.mod:
				// The module-root package, if one is ever added.
				info.Imports = append(info.Imports, ".")
			case strings.HasPrefix(path, s.mod+"/"):
				info.Imports = append(info.Imports, strings.TrimPrefix(path, s.mod+"/"))
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// Every SELECTION of a member with those names, not only the ones
			// in call position: `f := cipher.Decrypt` hands the method to
			// something else to call, and a call-only scan would report that
			// binary as reaching no cipher at all.
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Encrypt":
				info.Perms.Encrypt = true
			case "Decrypt":
				info.Perms.Decrypt = true
			default:
				return true
			}
			info.Sites = append(info.Sites, fmt.Sprintf("%s:%d %s",
				filepath.ToSlash(filepath.Join(pkg, name)), s.fset.Position(sel.Sel.Pos()).Line, sel.Sel.Name))
			return true
		})
	}
	if files == 0 {
		return nil, fmt.Errorf("package %s is imported but holds no non-test .go files", pkg)
	}
	sort.Strings(info.Imports)
	s.pkgs[pkg] = info
	return info, nil
}
