package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// maxResourceListLimit caps GET /v1/sessions/{id}/resources: the SDK documents
// limit "1 to 1000" and, uniquely, "if omitted, returns all"
// (anthropic-sdk-go betasessionresource.go). parseResourceLimit maps an omitted
// limit to -1 (all), not the managed-agents default of 20.
const maxResourceListLimit = 1000

// defaultMountRoot is the session's uploads directory — the container location
// every file resource is mounted under, whether the caller supplies a mount_path
// or not (an omitted one is /mnt/session/uploads/<file_id>; betasession.go:787
// documents that default, and resolveMountPath the rooting of a supplied one).
const defaultMountRoot = "/mnt/session/uploads/"

// maxMountPathBytes bounds a resolved mount_path so a pathological value never
// reaches the sandbox layer or the jsonb column.
const maxMountPathBytes = 1024

// defaultRepoMountRoot prefixes the default mount for a github_repository
// resource: /workspace/<repo-name> (betasession.go:816 documents the default;
// the <repo-name> derivation — last URL segment, ".git" stripped — is INFERRED,
// plan 25 decision 3). Unlike file mounts, repo mounts are used literally.
const defaultRepoMountRoot = "/workspace/"

// maxAuthorizationTokenBytes caps a github_repository authorization_token (400
// above it — INFERRED, plan 25 decision 3): real PATs are under a hundred
// bytes, and the cap keeps every secrets.Cipher backend inside its plaintext
// ceiling (gcpkms wraps ErrPlaintextTooLarge at 64 KiB, 8 KiB on HSM keys), so
// an oversized token can never degrade into a backend-dependent 500.
const maxAuthorizationTokenBytes = 8192

// maxReposPerSession caps github_repository resources per session (INFERRED,
// plan 25 decision 1 — ours: no repo cap is documented; files cap at 500). With
// the serial executor loop and per-clone spool cleanup this bounds a session's
// aggregate clone time and disk at count × the per-repo budget.
const maxReposPerSession = 8

// reservedRepoMounts are mount targets a repository may never take (plan 25
// decision 3): "/" (extraction over the rootfs breaks the sandbox image
// contract, and a re-clone's cleared target would be the sandbox itself),
// "/tmp" (the ancestor of the executor's in-sandbox staging path) and, from
// plan 36 (decision 8), memoryMountParent — validateRepoMountPath also refuses
// anything below it, which the equal-path map cannot express.
var reservedRepoMounts = map[string]bool{"/": true, "/tmp": true, memoryMountParent: true}

// memoryMountParent is the directory every attached memory store mounts under
// (the memory guide: "a directory under /mnt/memory/"); a store's own mount is
// memoryMountParent + "/" + its slug. A file mount cannot reach it
// (resolveMountPath roots files under /mnt/session/uploads) and a repository
// mount is refused at or below it, so the parent is the stores' alone.
const memoryMountParent = "/mnt/memory"

// maxMemoryStoresPerSession is the memory guide's documented cap ("8 stores
// per session"); the status of the ninth is INFERRED (plan 36 decision 7).
const maxMemoryStoresPerSession = 8

// maxMemoryInstructionsChars caps a memory attachment's instructions
// (betasession.go:912 "Max 4096 chars"; the spec's maxLength, so characters,
// not bytes).
const maxMemoryInstructionsChars = 4096

// fileResourceJSON is the materialized session file resource
// (BetaManagedAgentsSessionResource file variant, betasessionresource.go:176-209):
// every field is api:"required", so the server resolves the default mount_path
// and both timestamps at create/add and renders them. Stored verbatim as one
// element of the sessions.resources jsonb array; session GET echoes the array.
type fileResourceJSON struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	FileID    string    `json:"file_id"`
	MountPath string    `json:"mount_path"`
	Type      string    `json:"type"`
	UpdatedAt time.Time `json:"updated_at"`
}

// repoResourceJSON is the materialized github_repository session resource
// (BetaManagedAgentsGitHubRepositoryResource, betasessionresource.go:211-221):
// {id, created_at, mount_path, type, updated_at, url, checkout} — every field
// api:"required" except checkout (api:"nullable"). The authorization_token is
// write-only on the wire and deliberately absent from this type: what lands in
// the verbatim-echoed sessions.resources jsonb is token-free by construction
// (plan 25 decision 2); the secret seals into session_resource_credentials.
type repoResourceJSON struct {
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"created_at"`
	MountPath string        `json:"mount_path"`
	Type      string        `json:"type"`
	UpdatedAt time.Time     `json:"updated_at"`
	URL       string        `json:"url"`
	Checkout  *checkoutJSON `json:"checkout"` // nullable: renders null when omitted (as-given, plan 25 decision 3)
}

// checkoutJSON is the branch|commit checkout union (betasession.go:889-892
// registers exactly these two variants), stored and rendered as given.
type checkoutJSON struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"` // branch variant
	Sha  string `json:"sha,omitempty"`  // commit variant
}

// memoryResourceJSON is the stored memory_store session resource
// (BetaManagedAgentsMemoryStoreResource, betasessionresource.go:317-352):
// exactly {memory_store_id, type, access, description, instructions,
// mount_path, name} — no id and no timestamps, unlike the file and repository
// variants (plan 36 decision 7). name, description and mount_path are
// snapshotted from the store row inside the create transaction ("Later edits
// to the store's name do not propagate"); access renders the documented
// default "read_write" when the request omitted it (whether the reference
// echoes the string or null is INFERRED); instructions renders null when
// omitted. Stored verbatim as one element of sessions.resources, so the
// brain, executor and worker decoders — which pick elements by type — pass
// it over until slice 4 teaches them the mount.
type memoryResourceJSON struct {
	Access        string  `json:"access"`
	Description   string  `json:"description"`
	Instructions  *string `json:"instructions"`
	MemoryStoreID string  `json:"memory_store_id"`
	MountPath     string  `json:"mount_path"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
}

// resourceKind discriminates the validated resource union.
type resourceKind int

const (
	resourceKindFile resourceKind = iota
	resourceKindRepo
	resourceKindMemory
)

// resourceInput is a validated-but-not-yet-materialized resource: a file has
// not been proven to exist, a repo's token is not yet sealed, and neither has
// a sesrsc_ id or timestamps. The token lives only here in memory on its way
// to the cipher — it is never marshaled.
type resourceInput struct {
	kind      resourceKind
	mountPath string
	// file variant
	fileID string
	// github_repository variant
	url      string
	token    string
	checkout *checkoutJSON
	// memory_store variant; mountPath stays empty until the create
	// transaction snapshots the store (materializeResourceInputs)
	memoryStoreID string
	access        string
	instructions  *string
}

// parseResourceInputs validates the create-time resources[] union without
// touching the database: each element must be a supported resource (file,
// github_repository or memory_store) with a valid file_id, canonical GitHub
// URL + token, or memory_store_id. A file's mount_path resolves
// (resolveMountPath) under the uploads root — so "data.csv" and "/data.csv"
// collide the way they will in the sandbox — while a repository's is the
// literal container path (validateRepoMountPath). Existence of a referenced
// file, and the store a memory element names, are checked later, inside the
// create transaction (materializeResourceInputs). Cross-resource rules (plan
// 25 decision 3): uniqueness is judged on the resolved/path.Clean forms so
// aliases of one directory cannot coexist; no resource may sit at a proper
// ancestor of a repository's mount (a file there is a non-directory where the
// clone needs a directory, and nested repos would let a fresh re-clone of the
// outer clear the inner — a resource inside a repo mount, the supported
// overlay, stays legal); and a session mounts at most maxReposPerSession
// repositories. A memory element has no mount path yet — it is derived from
// the store's name in the transaction, under memoryMountParent, which no file
// or repository mount can reach — so it takes only its own two rules here:
// the same store at most once and at most maxMemoryStoresPerSession of them
// (plan 36 decision 7).
func parseResourceInputs(obj map[string]json.RawMessage) ([]resourceInput, error) {
	raw, ok := obj["resources"]
	if !ok || isNull(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errInvalid("resources must be an array")
	}
	out := make([]resourceInput, 0, len(items))
	seen := make(map[string]bool, len(items))
	stores := make(map[string]bool, len(items))
	repos := 0
	for _, item := range items {
		in, err := parseResourceItem(item)
		if err != nil {
			return nil, err
		}
		if in.kind == resourceKindMemory {
			if stores[in.memoryStoreID] {
				return nil, errInvalid("memory store %s is attached more than once", in.memoryStoreID)
			}
			stores[in.memoryStoreID] = true
			if len(stores) > maxMemoryStoresPerSession {
				return nil, errInvalid("a session can attach at most %d memory stores", maxMemoryStoresPerSession)
			}
			out = append(out, in)
			continue
		}
		clean := path.Clean(in.mountPath)
		if seen[clean] {
			return nil, errInvalid("mount_path %q is used by more than one resource", in.mountPath)
		}
		seen[clean] = true
		if in.kind == resourceKindRepo {
			repos++
		}
		out = append(out, in)
	}
	if repos > maxReposPerSession {
		return nil, errInvalid("a session can mount at most %d github_repository resources", maxReposPerSession)
	}
	for _, r := range out {
		if r.kind != resourceKindRepo {
			continue
		}
		for _, p := range out {
			if p.kind == resourceKindMemory {
				continue
			}
			if properPathAncestor(path.Clean(p.mountPath), path.Clean(r.mountPath)) {
				return nil, errInvalid("mount_path %q is an ancestor of repository mount_path %q", p.mountPath, r.mountPath)
			}
		}
	}
	return out, nil
}

// properPathAncestor reports whether clean path a is a proper ancestor
// directory of clean path b.
func properPathAncestor(a, b string) bool {
	if a == b {
		return false
	}
	if a == "/" {
		return true
	}
	return strings.HasPrefix(b, a+"/")
}

func parseResourceItem(raw json.RawMessage) (resourceInput, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return resourceInput{}, errInvalid("each resource must be an object")
	}
	return parseResourceObject(obj)
}

// parseResourceObject dispatches on the resource union's type discriminator. It
// backs both the create-time array elements and the add endpoint's single body.
func parseResourceObject(obj map[string]json.RawMessage) (resourceInput, error) {
	typ, err := requiredString(obj, "type")
	if err != nil {
		return resourceInput{}, err
	}
	switch typ {
	case "file":
		return parseFileResource(obj)
	case "github_repository":
		return parseRepoResource(obj)
	case "memory_store":
		return parseMemoryResource(obj)
	default:
		return resourceInput{}, errInvalid("resource type %q is not supported", typ)
	}
}

func parseFileResource(obj map[string]json.RawMessage) (resourceInput, error) {
	if err := rejectUnknownKeys(obj, "type", "file_id", "mount_path"); err != nil {
		return resourceInput{}, err
	}
	fileID, err := requiredString(obj, "file_id")
	if err != nil {
		return resourceInput{}, err
	}
	if !domain.ID(fileID).HasPrefix(domain.PrefixFile) || !domain.ID(fileID).Valid() {
		return resourceInput{}, errInvalid("file_id must be a valid file id")
	}
	mountPath, set, null, err := stringField(obj, "mount_path")
	if err != nil {
		return resourceInput{}, err
	}
	if !set || null || mountPath == "" {
		mountPath = defaultMountRoot + fileID
	} else {
		resolved, err := resolveMountPath(mountPath)
		if err != nil {
			return resourceInput{}, err
		}
		mountPath = resolved
	}
	return resourceInput{fileID: fileID, mountPath: mountPath}, nil
}

// parseMemoryResource validates the memory_store create variant
// (BetaManagedAgentsMemoryStoreResourceParam, betasession.go:896-935:
// memory_store_id and type required; access and instructions optional, both
// nullable). The id is checked on shape here and against the store row in the
// create transaction; an explicit null for access is the omitted case — the
// documented default, "read_write" — and for instructions the stored null.
// The SDK never transmits an empty access (omitzero), so "" is refused with
// every other value outside the enum rather than read as the default.
func parseMemoryResource(obj map[string]json.RawMessage) (resourceInput, error) {
	if err := rejectUnknownKeys(obj, "type", "memory_store_id", "access", "instructions"); err != nil {
		return resourceInput{}, err
	}
	id, err := requiredString(obj, "memory_store_id")
	if err != nil {
		return resourceInput{}, err
	}
	if !domain.ID(id).HasPrefix(domain.PrefixMemoryStore) || !domain.ID(id).Valid() {
		return resourceInput{}, errInvalid("memory_store_id must be a valid memory store id")
	}
	access, set, null, err := stringField(obj, "access")
	if err != nil {
		return resourceInput{}, err
	}
	if !set || null {
		access = "read_write"
	}
	if access != "read_write" && access != "read_only" {
		return resourceInput{}, errInvalid(`access must be "read_write" or "read_only"`)
	}
	in := resourceInput{kind: resourceKindMemory, memoryStoreID: id, access: access}
	instructions, set, null, err := stringField(obj, "instructions")
	if err != nil {
		return resourceInput{}, err
	}
	if set && !null {
		if utf8.RuneCountInString(instructions) > maxMemoryInstructionsChars {
			return resourceInput{}, errInvalid("instructions must be at most %d characters", maxMemoryInstructionsChars)
		}
		in.instructions = &instructions
	}
	return in, nil
}

// parseRepoResource validates the github_repository create variant
// (betasession.go:806-821: authorization_token, type, url required;
// mount_path and checkout optional). Validation is create-time-local — no
// network call proves the repo or the token; the first materialization is the
// probe (plan 25 decision 3).
func parseRepoResource(obj map[string]json.RawMessage) (resourceInput, error) {
	if err := rejectUnknownKeys(obj, "type", "url", "authorization_token", "mount_path", "checkout"); err != nil {
		return resourceInput{}, err
	}
	rawURL, err := requiredString(obj, "url")
	if err != nil {
		return resourceInput{}, err
	}
	repoName, err := parseGitHubRepoURL(rawURL)
	if err != nil {
		return resourceInput{}, err
	}
	token, err := requiredString(obj, "authorization_token")
	if err != nil {
		return resourceInput{}, err
	}
	if len(token) > maxAuthorizationTokenBytes {
		return resourceInput{}, errInvalid("authorization_token must be at most %d bytes", maxAuthorizationTokenBytes)
	}
	checkout, err := parseCheckout(obj)
	if err != nil {
		return resourceInput{}, err
	}
	mountPath, set, null, err := stringField(obj, "mount_path")
	if err != nil {
		return resourceInput{}, err
	}
	if !set || null || mountPath == "" {
		mountPath = defaultRepoMountRoot + repoName
	}
	// The derived default is validated too, not just a supplied path: the
	// grammar above keeps the repo name clean and storable, and this keeps
	// that true by construction (and bounds a pathologically long name).
	if err := validateRepoMountPath(mountPath); err != nil {
		return resourceInput{}, err
	}
	return resourceInput{
		kind: resourceKindRepo, mountPath: mountPath,
		url: rawURL, token: token, checkout: checkout,
	}, nil
}

// parseGitHubRepoURL enforces the exact canonical repository URL
// (plan 25 decision 3, strictness INFERRED): https://github.com/{owner}/{repo}
// — scheme https, host github.com with no userinfo and no port, exactly two
// non-empty path segments (optional ".git" on the second), no query, no
// fragment, no trailing slash. The url is a rendered, logged field: a
// credential smuggled into it (https://TOKEN@github.com/o/r, ?token=…) would
// break the write-only-token guarantee, so the grammar rejects every carrier.
// Returns the derived <repo-name> for the default mount path.
func parseGitHubRepoURL(raw string) (string, error) {
	bad := func() error { return errInvalid("url must be a https://github.com/{owner}/{repo} repository URL") }
	u, err := url.Parse(raw)
	if err != nil {
		return "", bad()
	}
	// ForceQuery catches a bare trailing "?" (RawQuery empty); a bare
	// trailing "#" leaves every fragment field empty too, so the raw string
	// is the only place the delimiter itself is visible.
	if u.Scheme != "https" || u.Host != "github.com" || u.User != nil ||
		u.Opaque != "" || u.ForceQuery || u.RawQuery != "" ||
		u.Fragment != "" || u.RawFragment != "" || strings.Contains(raw, "#") {
		return "", bad()
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(segs) != 2 || !validRepoURLSegment(segs[0]) || !validRepoURLSegment(segs[1]) {
		return "", bad()
	}
	// The repo name becomes the default mount path's last element, so the
	// names path.Clean would rewrite are refused here — "acme/.." would
	// otherwise derive /workspace/.., the reserved "/" in disguise.
	name := strings.TrimSuffix(segs[1], ".git")
	if name == "" || name == "." || name == ".." {
		return "", bad()
	}
	return name, nil
}

// validRepoURLSegment bounds a URL path segment to the GitHub owner/repo
// character set. url.Parse percent-decodes the path, so this is also what
// keeps an encoded NUL or space out of the derived default mount path (the
// #135 failure class the storableText checks exist to prevent).
func validRepoURLSegment(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// parseCheckout parses the optional checkout union: {type:"branch", name} |
// {type:"commit", sha} (betasession.go:889-892 registers exactly these two).
// Omitted or null means the repository's default branch, resolved at clone
// time — stored and rendered as null (as-given, INFERRED, plan 25 decision 3).
func parseCheckout(obj map[string]json.RawMessage) (*checkoutJSON, error) {
	raw, ok := obj["checkout"]
	if !ok || isNull(raw) {
		return nil, nil
	}
	var co map[string]json.RawMessage
	if err := json.Unmarshal(raw, &co); err != nil {
		return nil, errInvalid("checkout must be an object")
	}
	typ, err := requiredString(co, "type")
	if err != nil {
		return nil, errInvalid("checkout.type is required")
	}
	switch typ {
	case "branch":
		if err := rejectUnknownKeys(co, "type", "name"); err != nil {
			return nil, err
		}
		name, err := requiredString(co, "name")
		if err != nil {
			return nil, errInvalid("checkout.name is required for a branch checkout")
		}
		return &checkoutJSON{Type: "branch", Name: name}, nil
	case "commit":
		if err := rejectUnknownKeys(co, "type", "sha"); err != nil {
			return nil, err
		}
		sha, err := requiredString(co, "sha")
		if err != nil {
			return nil, errInvalid("checkout.sha is required for a commit checkout")
		}
		if !isFullCommitSHA(sha) {
			return nil, errInvalid("checkout.sha must be a full 40-character commit SHA")
		}
		return &checkoutJSON{Type: "commit", Sha: sha}, nil
	default:
		return nil, errInvalid("checkout.type must be \"branch\" or \"commit\"")
	}
}

// isFullCommitSHA reports whether s is exactly 40 hex characters ("Full commit
// SHA to check out", betasession.go:624 and :661; the 40-hex strictness is INFERRED).
func isFullCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// validateRepoMountPath enforces the repository arm's mount-path rules
// (plan 25 decision 3, INFERRED). Unlike a file's mount_path — which
// resolveMountPath roots under the session's uploads directory (#323) — a
// repository's is the literal container path (the documented default is
// /workspace/<repo-name>), so it must be absolute, bounded, and storable (an
// unstorable byte would otherwise fail as a 500 when the resources array binds
// into the jsonb column, see #135); it must equal its path.Clean form —
// aliases like /workspace/./r or a trailing slash would evade the cleaned-form
// uniqueness check — and may not take a reserved target.
func validateRepoMountPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return errInvalid("mount_path must be an absolute path")
	}
	if len(p) > maxMountPathBytes {
		return errInvalid("mount_path must be at most %d bytes", maxMountPathBytes)
	}
	if !storableText(p) {
		return errInvalid("mount_path must be storable text")
	}
	if path.Clean(p) != p {
		return errInvalid("mount_path must be a clean absolute path (no \".\", \"..\", doubled separators, or trailing slash)")
	}
	if reservedRepoMounts[p] {
		return errInvalid("mount_path %q is reserved", p)
	}
	if strings.HasPrefix(p, memoryMountParent+"/") {
		return errInvalid("mount_path %q is reserved for memory stores", p)
	}
	return nil
}

// resolveMountPath maps a caller-supplied mount_path to the container path the
// file is mounted at. Every supplied path is rooted under the session's uploads
// directory — "a mount_path of /data.csv places the file at
// /mnt/session/uploads/data.csv in the sandbox" (docs, managed-agents/files
// § "File paths") — so a leading "/" is that page's documented style ("paths
// should be absolute"), not a filesystem root: "/data.csv" and "data.csv"
// resolve alike. An **absolute** path already under the root passes through
// cleaned but not re-rooted, which the reference's own data-analyst cookbook
// requires: it mounts at the full /mnt/session/uploads/<name> and then prompts
// the agent with that same path, which a second rooting would break. That is the
// only form the cookbook evidence covers, so a relative "mnt/session/uploads/x"
// is rooted like any other relative path.
//
// The caller's path is cleaned **before** it is rooted, so two spellings of one
// path resolve alike: "/mnt/session/uploads/a/../../../../etc/passwd" and
// "/../../etc/passwd" both clean to "/etc/passwd" and both land at
// "/mnt/session/uploads/etc/passwd". Rooting the raw spelling instead would let
// a ".." eat the duplicated root and land the file at a nested path no client
// would look at — the very failure this rooting exists to prevent. Cleaning an
// absolute path resolves its ".." entirely (POSIX makes "/.." the root), so only
// a relative path whose cleaned form still leads with ".." can climb out, and
// that is rejected. That leaves one asymmetry worth naming rather than hiding:
// "/../etc/passwd" is accepted (it *is* "/etc/passwd") while "../etc/passwd" is
// a 400, so under the "a leading slash is style, not a root" reading above these
// two spell one intent and get opposite answers. Both are contained; the
// alternative — rejecting a leading ".." on the absolute form too — would mean
// declining to clean a path POSIX already defines, so the asymmetry is accepted.
//
// The resolved path is what gets stored and mounted, so it — not the caller's
// spelling — carries the bounds: it must stay under the root, be at most
// maxMountPathBytes, and be storable text, since an unstorable byte (U+0000,
// invalid UTF-8) would otherwise fail as a 500 when the resources array binds
// into the jsonb column (see #135). Containment is lexical, over the stored
// string: the uploads directory is agent-writable, so an intermediate symlink
// the agent plants there can still point a mount's bytes elsewhere — the same
// accepted single-tenant tampering residual the mount sentinel carries
// (docs/DIVERGENCES.md), not something this resolver can answer.
func resolveMountPath(p string) (string, error) {
	root := strings.TrimSuffix(defaultMountRoot, "/")
	resolved := path.Clean(p)
	if resolved != root && !strings.HasPrefix(resolved, root+"/") {
		resolved = path.Join(root, resolved)
	}
	// Also catches the root itself, which names a directory, not a mount target.
	if !strings.HasPrefix(resolved, root+"/") {
		return "", errInvalid("mount_path must resolve to a path under %s", root)
	}
	if len(resolved) > maxMountPathBytes {
		return "", errInvalid("mount_path must be at most %d bytes", maxMountPathBytes)
	}
	if !storableText(resolved) {
		return "", errInvalid("mount_path must be storable text")
	}
	return resolved, nil
}

// sealedToken is a repository's write-only token after the cipher: the
// ciphertext and key id sealRepoTokens produced, held in memory until the
// create transaction binds them to their credential rows.
type sealedToken struct {
	ciphertext []byte
	keyID      string
}

// sealRepoTokens runs each repository token through the cipher, in input
// order, before the create transaction opens (the vault-credential precedent,
// vaultcredentials.go): the seal depends only on the parsed inputs, so the
// cipher's network round trip (OpenBao) never runs under the session and
// environment row locks the create transaction holds.
func sealRepoTokens(ctx context.Context, cipher secrets.Cipher, inputs []resourceInput) ([]sealedToken, error) {
	var sealed []sealedToken
	for _, in := range inputs {
		if in.kind != resourceKindRepo {
			continue
		}
		ct, keyID, err := cipher.Encrypt(ctx, []byte(in.token))
		if errors.Is(err, secrets.ErrPlaintextTooLarge) {
			// Unreachable behind maxAuthorizationTokenBytes; classified as the
			// caller's error anyway per the secrets contract.
			return nil, errInvalid("authorization_token is too large for this deployment's cipher")
		}
		if err != nil {
			return nil, err
		}
		sealed = append(sealed, sealedToken{ciphertext: ct, keyID: keyID})
	}
	return sealed, nil
}

// materializeResourceInputs verifies each referenced file exists in the same
// transaction as the create (cheaper failure locality than an unvalidated
// reference — an INFERRED divergence, docs/DIVERGENCES.md) and stamps each input
// with a fresh sesrsc_ id and the create timestamp. A file deleted between this
// check and a later materialization is tolerated by design (plan decision 2).
// Repositories render token-free (plan 25 decision 2); their fresh sesrsc_
// ids come back in input order for the caller to bind to the pre-sealed
// ciphertexts (sealRepoTokens walks the same inputs in the same order) once
// the session row exists — the credential rows FK the session.
func materializeResourceInputs(ctx context.Context, db querier, inputs []resourceInput, now time.Time) ([]json.RawMessage, []string, error) {
	out := make([]json.RawMessage, 0, len(inputs))
	var repoIDs []string
	memoryMounts := map[string]string{}
	for _, in := range inputs {
		switch in.kind {
		case resourceKindMemory:
			el, err := snapshotMemoryStore(ctx, db, in)
			if err != nil {
				return nil, nil, err
			}
			// Two stores whose names slug alike would mount over each other.
			if prev, taken := memoryMounts[el.MountPath]; taken {
				return nil, nil, errInvalid("memory stores %s and %s both mount at %s", prev, in.memoryStoreID, el.MountPath)
			}
			memoryMounts[el.MountPath] = in.memoryStoreID
			out = append(out, mustJSON(el))
		case resourceKindRepo:
			id := domain.NewID(domain.PrefixResource).String()
			out = append(out, mustJSON(repoResourceJSON{
				ID: id, CreatedAt: now, MountPath: in.mountPath,
				Type: "github_repository", UpdatedAt: now,
				URL: in.url, Checkout: in.checkout,
			}))
			repoIDs = append(repoIDs, id)
		default:
			if err := fileMustExist(ctx, db, in.fileID); err != nil {
				return nil, nil, err
			}
			id := domain.NewID(domain.PrefixResource).String()
			out = append(out, mustJSON(fileResourceJSON{
				ID: id, CreatedAt: now, FileID: in.fileID,
				MountPath: in.mountPath, Type: "file", UpdatedAt: now,
			}))
		}
	}
	return out, repoIDs, nil
}

// snapshotMemoryStore turns a validated memory element into its stored form
// from the store row, read FOR SHARE so a concurrent archive or delete cannot
// slip in between this check and the session INSERT (the environment row's
// precedent in createSession). An unknown or archived store fails the create
// with a 400, the vault_ids precedent (validateAttachedVaults) rather than the
// file's 404 — statuses INFERRED, plan 36 decision 7. The mount path is
// memoryMountParent + "/" + the slug of the snapshotted name (decision 8),
// falling back to the store id's token for a name with no alphanumerics.
func snapshotMemoryStore(ctx context.Context, db querier, in resourceInput) (memoryResourceJSON, error) {
	var name, description string
	var archivedAt *time.Time
	err := db.QueryRow(ctx,
		`SELECT name, description, archived_at FROM memory_stores WHERE id = $1 FOR SHARE`,
		in.memoryStoreID).Scan(&name, &description, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return memoryResourceJSON{}, errInvalid("memory store %s not found", in.memoryStoreID)
	}
	if err != nil {
		return memoryResourceJSON{}, err
	}
	if archivedAt != nil {
		return memoryResourceJSON{}, errInvalid("memory store %s is archived", in.memoryStoreID)
	}
	slug := memsync.Slug(name, strings.TrimPrefix(in.memoryStoreID, domain.PrefixMemoryStore+"_"))
	return memoryResourceJSON{
		Access: in.access, Description: description, Instructions: in.instructions,
		MemoryStoreID: in.memoryStoreID, MountPath: memoryMountParent + "/" + slug,
		Name: name, Type: "memory_store",
	}, nil
}

// errRepoSecretsUnavailable answers a repo-bearing create or a token rotation
// on a deployment with no secrets cipher: refused, never stored unencrypted
// (the errSecretsUnavailable twin, plan 25 decision 2).
var errRepoSecretsUnavailable = &apiError{http.StatusInternalServerError, errTypeAPI,
	"a secrets cipher is not configured on this deployment; github_repository resources are unavailable"}

// resourceInputsHaveRepo reports whether any validated input is a repository —
// the create path fails fast on a cipher-less deployment before touching the
// database.
func resourceInputsHaveRepo(inputs []resourceInput) bool {
	for _, in := range inputs {
		if in.kind == resourceKindRepo {
			return true
		}
	}
	return false
}

// insertSessionResourceCredentials stores each repository's pre-sealed token
// ciphertext beside the session (plan 25 decision 2). Runs after the session
// INSERT in the same transaction — the rows FK the session with ON DELETE
// CASCADE, so a credential can never outlive its session — but the sealing
// itself happened before the transaction opened (sealRepoTokens), keeping the
// cipher's round trip out of the row locks. repoIDs and sealed walk the same
// inputs in the same order.
func insertSessionResourceCredentials(ctx context.Context, tx pgx.Tx, sessionID string, repoIDs []string, sealed []sealedToken) error {
	for i, rid := range repoIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO session_resource_credentials (resource_id, session_id, token_ciphertext, token_key_id)
			 VALUES ($1, $2, $3, $4)`, rid, sessionID, sealed[i].ciphertext, sealed[i].keyID); err != nil {
			return err
		}
	}
	return nil
}

// fileMustExist reports whether a file row exists, mapping absence to the wire
// 404. The referencing session's transaction holds no lock on the file row, so a
// concurrent delete may still leave a dangling reference — accepted (decision 2).
func fileMustExist(ctx context.Context, db querier, fileID string) error {
	var exists bool
	err := db.QueryRow(ctx, `SELECT true FROM files WHERE id = $1`, fileID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("file %s not found", fileID)
	}
	return err
}

// sessionResourceRows loads a session's stored resources array and archived
// state. forUpdate takes the session row lock so a concurrent add/delete/archive
// serializes; reads pass false and may use the pool directly. A missing session
// surfaces as pgx.ErrNoRows for the caller to map to a 404.
func sessionResourceRows(ctx context.Context, db querier, id string, forUpdate bool) (resources []json.RawMessage, archivedAt *time.Time, err error) {
	q := `SELECT resources, archived_at FROM sessions WHERE id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var raw []byte
	if err = db.QueryRow(ctx, q, id).Scan(&raw, &archivedAt); err != nil {
		return nil, nil, err
	}
	if err = json.Unmarshal(raw, &resources); err != nil {
		return nil, nil, err
	}
	return resources, archivedAt, nil
}

// updateSessionResources rewrites the resources array and bumps updated_at. No
// session.updated event is emitted: the taxonomy has no session_resource.* event
// and the documented session.updated payload carries only title/metadata/agent.
func updateSessionResources(ctx context.Context, tx pgx.Tx, id string, resources []json.RawMessage) error {
	raw, err := json.Marshal(resources)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE sessions SET resources = $2, updated_at = now() WHERE id = $1`, id, raw)
	return err
}

func (s *server) getSessionResource(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	rid := r.PathValue("rid")
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	if err := checkResourceID(rid); err != nil {
		return nil, err
	}
	resources, _, err := sessionResourceRows(ctx, s.pool, id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if raw := findResource(resources, rid); raw != nil {
		return raw, nil
	}
	return nil, errNotFound("session resource %s not found", rid)
}

func (s *server) listSessionResources(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	q := r.URL.Query()
	limit, err := parseResourceLimit(q)
	if err != nil {
		return nil, err
	}
	resources, _, err := sessionResourceRows(ctx, s.pool, id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}

	start := 0
	if cur := q.Get("page"); cur != "" {
		last, err := decodeResourceCursor(cur)
		if err != nil {
			return nil, err
		}
		// An unknown cursor yields an empty page — the files-list convention.
		idx := indexOfResource(resources, last)
		if idx < 0 {
			return pageJSON{Data: []any{}}, nil
		}
		start = idx + 1
	}
	end := len(resources)
	if limit >= 0 && start+limit < end {
		end = start + limit
	}
	page := resources[start:end]
	out := pageJSON{Data: make([]any, 0, len(page))}
	for _, raw := range page {
		out.Data = append(out.Data, raw)
	}
	if end < len(resources) && len(page) > 0 {
		c := encodeResourceCursor(resourceKey(page[len(page)-1]))
		out.NextPage = &c
	}
	return out, nil
}

func (s *server) addSessionResource(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	res, err := s.addSessionResourceTx(ctx, id, r)
	if err != nil {
		recordResourceMutation(ctx, resourceOutcomeFor(err), 1)
		return nil, err
	}
	recordResourceMutation(ctx, resourceOutcomeOK, 1)
	slog.InfoContext(ctx, "session resource added", "session_id", id,
		"resource_id", res.ID, "file_id", res.FileID, "mount_path", res.MountPath)
	return res, nil
}

func (s *server) addSessionResourceTx(ctx context.Context, id string, r *http.Request) (fileResourceJSON, error) {
	obj, err := decodeObject(r)
	if err != nil {
		return fileResourceJSON{}, err
	}
	in, err := parseResourceObject(obj)
	if err != nil {
		return fileResourceJSON{}, err
	}
	if in.kind != resourceKindFile {
		// The add endpoint is typed file-only in the SDK (Add returns the file
		// resource, betasessionresource.go:135) and the docs pin repos to the
		// session's lifetime — wire-faithful, message INFERRED (plan 25
		// decision 3).
		return fileResourceJSON{}, errInvalid("only file resources can be added to an existing session")
	}
	if err := checkID(id, "session"); err != nil {
		return fileResourceJSON{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fileResourceJSON{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	resources, archivedAt, err := sessionResourceRows(ctx, tx, id, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return fileResourceJSON{}, errNotFound("session %s not found", id)
	}
	if err != nil {
		return fileResourceJSON{}, err
	}
	if archivedAt != nil {
		return fileResourceJSON{}, errInvalid("session %s is archived", id)
	}
	if mountPathTaken(resources, in.mountPath) {
		return fileResourceJSON{}, errInvalid("mount_path %q is already in use by this session", in.mountPath)
	}
	if rm := repoMountBelow(resources, in.mountPath); rm != "" {
		// The same ancestor rule create enforces (parseResourceInputs): a
		// resource above a repository's mount is a non-directory where the
		// clone needs a directory. A file inside the repo's tree — the
		// supported overlay — stays legal.
		return fileResourceJSON{}, errInvalid("mount_path %q is an ancestor of repository mount_path %q", in.mountPath, rm)
	}
	if err := fileMustExist(ctx, tx, in.fileID); err != nil {
		return fileResourceJSON{}, err
	}
	now := time.Now().UTC()
	res := fileResourceJSON{
		ID: domain.NewID(domain.PrefixResource).String(), CreatedAt: now,
		FileID: in.fileID, MountPath: in.mountPath, Type: "file", UpdatedAt: now,
	}
	resources = append(resources, mustJSON(res))
	if err := updateSessionResources(ctx, tx, id, resources); err != nil {
		return fileResourceJSON{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return fileResourceJSON{}, err
	}
	return res, nil
}

func (s *server) deleteSessionResource(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	rid := r.PathValue("rid")
	if err := s.deleteSessionResourceTx(ctx, id, rid); err != nil {
		recordResourceMutation(ctx, resourceOutcomeFor(err), 1)
		return nil, err
	}
	recordResourceMutation(ctx, resourceOutcomeOK, 1)
	// Deletion removes the reference only; it never reaches into a live sandbox
	// to unmount (a non-goal, INFERRED divergence).
	slog.InfoContext(ctx, "session resource deleted", "session_id", id, "resource_id", rid)
	return map[string]string{"id": rid, "type": "session_resource_deleted"}, nil
}

func (s *server) deleteSessionResourceTx(ctx context.Context, id, rid string) error {
	if err := checkID(id, "session"); err != nil {
		return err
	}
	if err := checkResourceID(rid); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	resources, archivedAt, err := sessionResourceRows(ctx, tx, id, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("session %s not found", id)
	}
	if err != nil {
		return err
	}
	if archivedAt != nil {
		return errInvalid("session %s is archived", id)
	}
	idx := indexOfResource(resources, rid)
	if idx < 0 {
		return errNotFound("session resource %s not found", rid)
	}
	if resourceType(resources[idx]) == "github_repository" {
		// "Repositories are attached for the lifetime of the session" (the
		// public GitHub guide); the rejection shape is INFERRED (plan 25
		// decision 3). The credential row dies with the session cascade.
		return errInvalid("github_repository resources cannot be removed; repositories are attached for the lifetime of the session")
	}
	resources = append(resources[:idx], resources[idx+1:]...)
	if err := updateSessionResources(ctx, tx, id, resources); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// updateSessionResource handles POST …/resources/{rid} — token rotation, the
// one mutation a github_repository resource supports ("Currently only
// `github_repository` resources support token rotation",
// betasessionresource.go:690-698). The new token seals over the old ciphertext
// and the resource's updated_at bumps inside the echoed jsonb; an already
// materialized clone is unaffected (no retroactive effect — INFERRED, plan 25
// decision 5). File resources keep the established rejection.
func (s *server) updateSessionResource(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	rid := r.PathValue("rid")
	res, err := s.rotateResourceTokenTx(ctx, id, rid, r)
	if err != nil {
		recordResourceMutation(ctx, resourceOutcomeFor(err), 1)
		return nil, err
	}
	recordResourceMutation(ctx, resourceOutcomeOK, 1)
	slog.InfoContext(ctx, "session resource token rotated", "session_id", id, "resource_id", rid)
	return res, nil
}

func (s *server) rotateResourceTokenTx(ctx context.Context, id, rid string, r *http.Request) (json.RawMessage, error) {
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "authorization_token"); err != nil {
		return nil, err
	}
	token, err := requiredString(obj, "authorization_token")
	if err != nil {
		return nil, err
	}
	if len(token) > maxAuthorizationTokenBytes {
		return nil, errInvalid("authorization_token must be at most %d bytes", maxAuthorizationTokenBytes)
	}
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	if err := checkResourceID(rid); err != nil {
		return nil, err
	}

	// Seal before opening the transaction (the vault-credential precedent,
	// vaultcredentials.go): the seal depends only on the parsed token, and the
	// cipher's network round trip (OpenBao) must not run under the session row
	// lock, where it would stall every concurrent event append. A cipher-less
	// deployment defers its refusal into the transaction so a file resource
	// still gets its type rejection first.
	var ct []byte
	var keyID string
	if s.cipher != nil {
		ct, keyID, err = s.cipher.Encrypt(ctx, []byte(token))
		if errors.Is(err, secrets.ErrPlaintextTooLarge) {
			return nil, errInvalid("authorization_token is too large for this deployment's cipher")
		}
		if err != nil {
			return nil, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	resources, archivedAt, err := sessionResourceRows(ctx, tx, id, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if archivedAt != nil {
		return nil, errInvalid("session %s is archived", id)
	}
	idx := indexOfResource(resources, rid)
	if idx < 0 {
		return nil, errNotFound("session resource %s not found", rid)
	}
	if resourceType(resources[idx]) != "github_repository" {
		return nil, errInvalid("only github_repository resources support token rotation")
	}
	if s.cipher == nil {
		return nil, errRepoSecretsUnavailable
	}
	tag, err := tx.Exec(ctx,
		`UPDATE session_resource_credentials
		    SET token_ciphertext = $1, token_key_id = $2, updated_at = now()
		  WHERE resource_id = $3 AND session_id = $4`, ct, keyID, rid, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		// A stored repo resource always has its credential row (created in the
		// same transaction); its absence is an invariant breach, not a 404.
		return nil, errors.New("session resource credential row missing")
	}
	updated, err := resourceWithUpdatedAt(resources[idx], time.Now().UTC())
	if err != nil {
		return nil, err
	}
	resources[idx] = updated
	if err := updateSessionResources(ctx, tx, id, resources); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return updated, nil
}

// resourceWithUpdatedAt re-marshals a stored resource object with its
// updated_at replaced. Key order may change (Go map marshaling); JSON key
// order is not wire-significant.
func resourceWithUpdatedAt(raw json.RawMessage, now time.Time) (json.RawMessage, error) {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	ts, err := json.Marshal(now)
	if err != nil {
		return nil, err
	}
	o["updated_at"] = ts
	return json.Marshal(o)
}

// checkResourceID rejects a path id that is not a well-formed sesrsc_ id with the
// 404 an unknown resource already gets (checkID's shape-reject rationale).
func checkResourceID(id string) error {
	if !domain.ID(id).HasPrefix(domain.PrefixResource) || !domain.ID(id).Valid() {
		return errNotFound("session resource %s not found", id)
	}
	return nil
}

// findResource returns the stored resource object whose id equals rid, or nil.
func findResource(resources []json.RawMessage, rid string) json.RawMessage {
	if i := indexOfResource(resources, rid); i >= 0 {
		return resources[i]
	}
	return nil
}

func indexOfResource(resources []json.RawMessage, rid string) int {
	for i, raw := range resources {
		if resourceKey(raw) == rid {
			return i
		}
	}
	return -1
}

// resourceKey names a stored element by something every element has: the id
// of a file or repository element, "memstore:<memory_store_id>" for a memory
// element, which carries none (unique within one session's resources by the
// same-store-twice rule). The list cursor is built from it, so a page ending
// on a memory element resumes after that element rather than on an empty
// token; a get or delete by {rid} never reaches a memory element, since
// checkResourceID admits only sesrsc_ ids (plan 36 decision 7).
func resourceKey(raw json.RawMessage) string {
	var o struct {
		ID            string `json:"id"`
		Type          string `json:"type"`
		MemoryStoreID string `json:"memory_store_id"`
	}
	_ = json.Unmarshal(raw, &o)
	if o.ID == "" && o.Type == "memory_store" {
		return "memstore:" + o.MemoryStoreID
	}
	return o.ID
}

// resourceType returns the stored resource object's type discriminator.
func resourceType(raw json.RawMessage) string {
	var o struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Type
}

// repoMountBelow returns the mount_path of a stored github_repository
// resource that p is a proper ancestor of, or "" — the add-endpoint half of
// the no-resource-above-a-repo-mount rule create enforces. The stored side is
// cleaned before comparing, like mountPathTaken; p is already canonical.
func repoMountBelow(resources []json.RawMessage, p string) string {
	for _, raw := range resources {
		var o struct {
			Type      string `json:"type"`
			MountPath string `json:"mount_path"`
		}
		if json.Unmarshal(raw, &o) != nil || o.Type != "github_repository" {
			continue
		}
		if properPathAncestor(p, path.Clean(o.MountPath)) {
			return o.MountPath
		}
	}
	return ""
}

// mountPathTaken reports whether a session already mounts something at p. The
// stored side is cleaned before comparing: a session created before mount paths
// were resolved (#323) can hold a non-canonical literal — "/mnt/session/uploads//x"
// — that names the same file as a freshly resolved "/mnt/session/uploads/x", and
// a raw string compare would admit the second and let it overwrite the first's
// bytes at materialization. p is already canonical (resolveMountPath cleans).
func mountPathTaken(resources []json.RawMessage, p string) bool {
	for _, raw := range resources {
		var o struct {
			MountPath string `json:"mount_path"`
		}
		if json.Unmarshal(raw, &o) == nil && path.Clean(o.MountPath) == p {
			return true
		}
	}
	return false
}

// parseResourceLimit parses the resources-list limit. Unlike every other list,
// an omitted limit means "return all" (SDK: "if omitted, returns all"), reported
// as -1; a present value must be 1..1000.
func parseResourceLimit(q url.Values) (int, error) {
	s := q.Get("limit")
	if s == "" {
		return -1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxResourceListLimit {
		return 0, errInvalid("limit must be an integer between 1 and %d", maxResourceListLimit)
	}
	return n, nil
}

// encodeResourceCursor/decodeResourceCursor are the resources-list page cursor:
// the resources live in one jsonb array, so a page is a slice and the cursor is
// simply the last id returned. Self-contained (not the base64 keyset cursor the
// keyset lists use) because there is no (created_at, id) table walk here.
func encodeResourceCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("r1|" + id))
}

func decodeResourceCursor(s string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", errInvalid("invalid page cursor")
	}
	rest, ok := strings.CutPrefix(string(raw), "r1|")
	if !ok || rest == "" {
		return "", errInvalid("invalid page cursor")
	}
	return rest, nil
}

// MetricSessionResources counts session resource mutations (create-attach,
// add, delete, token rotation) by outcome. Outcome-only labels:
// session/resource/file ids ride the structured logs, never the metric (plan
// decision 9). Exported so the integration test can assert the name and labels.
const MetricSessionResources = "session.resources"

const (
	resourceOutcomeOK       = "ok"
	resourceOutcomeInvalid  = "invalid"
	resourceOutcomeNotFound = "not_found"
	resourceOutcomeError    = "error"
)

// resourceOutcomeFor maps a handler error to its mutation outcome label.
func resourceOutcomeFor(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.status {
		case http.StatusNotFound:
			return resourceOutcomeNotFound
		case http.StatusBadRequest:
			return resourceOutcomeInvalid
		}
	}
	return resourceOutcomeError
}

// recordResourceMutation counts resource mutations by outcome: 1 for a single
// add/delete (or a failed attempt), the resource count for a create that
// attaches several. The meter is resolved per call so it never pins a
// MeterProvider installed after startup; telemetry failure never fails a request.
func recordResourceMutation(ctx context.Context, outcome string, count int) {
	meter := otel.GetMeterProvider().Meter(apiMeterName)
	c, err := meter.Int64Counter(MetricSessionResources,
		metric.WithDescription("Session resource mutations by outcome."))
	if err != nil {
		return
	}
	c.Add(ctx, int64(count), metric.WithAttributes(attribute.String("outcome", outcome)))
}
