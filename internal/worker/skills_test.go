package worker

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
)

// skillArchive builds an archive shaped like the one the registry stores: every
// file under the skill's single top-level directory.
func skillArchive(t *testing.T, name string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for p, content := range files {
		fw, err := w.Create(name + "/" + p)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// seedSkill plants a registry skill server-side: rows — including the archive
// digest recorded at upload — plus the archive in the control plane's object
// store. The worker only ever sees it over the wire.
func (h *harness) seedSkill(t *testing.T, id, version, name string, files map[string]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO skills (id, source, display_title, latest_version) VALUES ($1, 'custom', $1, $2)
		 ON CONFLICT (id) DO UPDATE SET latest_version = $2`, id, version); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	archive := skillArchive(t, name, files)
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory, sha256)
		 VALUES ('skillver_'||md5($1||$2), $1, $2, $3, 'test skill', $3, $4)`,
		id, version, name, skills.Digest(archive)); err != nil {
		t.Fatalf("seed skill version: %v", err)
	}
	if err := h.blobs.Put(ctx, skills.BlobKey(id, version),
		bytes.NewReader(archive), int64(len(archive)), "application/zip"); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
}

// swapArchive replaces a version's stored object while leaving its row — and so
// the digest the download advertises — untouched: the storage-layer
// substitution the digest exists to catch.
func (h *harness) swapArchive(t *testing.T, id, version, name string, files map[string]string) {
	t.Helper()
	archive := skillArchive(t, name, files)
	if err := h.blobs.Put(context.Background(), skills.BlobKey(id, version),
		bytes.NewReader(archive), int64(len(archive)), "application/zip"); err != nil {
		t.Fatalf("swap archive: %v", err)
	}
}

// refSkills points the session snapshot's skills[] at the given
// {skill_id, version} references.
func (h *harness) refSkills(t *testing.T, refs ...[2]string) {
	t.Helper()
	entries := make([]map[string]string, len(refs))
	for i, r := range refs {
		entries[i] = map[string]string{"type": "custom", "skill_id": r[0], "version": r[1]}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{skills}', $2::jsonb) WHERE id = $1`,
		h.sid.String(), raw); err != nil {
		t.Fatalf("set session skills: %v", err)
	}
}

// rosterMember is one member of the coordinator roster a fixture snapshots, in
// the shape session create stores: a full agent definition, here reduced to the
// name and skills[] the union is about.
type rosterMember struct {
	name   string
	skills [][2]string
}

// refRoster makes the session a coordinator's, with the given roster members.
// The pgtest fixture seeds "multiagent":null (a single-agent session), so every
// coordinator fixture has to patch it.
func (h *harness) refRoster(t *testing.T, members ...rosterMember) {
	t.Helper()
	agents := make([]map[string]any, len(members))
	for i, m := range members {
		refs := make([]map[string]string, len(m.skills))
		for j, s := range m.skills {
			refs[j] = map[string]string{"type": "custom", "skill_id": s[0], "version": s[1]}
		}
		agents[i] = map[string]any{
			"type": "agent", "id": "agent_" + m.name, "version": 1, "name": m.name,
			"model": map[string]string{"id": "fixture-model"}, "system": "", "description": "",
			"tools": []any{}, "mcp_servers": []any{}, "skills": refs,
		}
	}
	raw, err := json.Marshal(map[string]any{"type": "coordinator", "agents": agents})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{multiagent}', $2::jsonb) WHERE id = $1`,
		h.sid.String(), raw); err != nil {
		t.Fatalf("set session roster: %v", err)
	}
}

// TestSkillsMaterializeTheRosterUnion: the threads of a coordinator session
// share one sandbox, so what lands in it is the union of the coordinator's own
// skills and every roster member's (plan 35 decision 11) — a member-only skill
// is materialized even though the coordinator never references it, and one both
// reference is materialized once. Each thread's system prompt still carries
// only its own agent's metadata; that is the brain's side, untouched.
func TestSkillsMaterializeTheRosterUnion(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "union-shared", "100", "shared-notes", map[string]string{"SKILL.md": "# shared"})
	h.seedSkill(t, "union-member", "100", "member-notes", map[string]string{"SKILL.md": "# member"})
	h.refSkills(t, [2]string{"union-shared", "100"})
	h.refRoster(t, rosterMember{name: "researcher", skills: [][2]string{
		{"union-shared", "100"}, {"union-member", "100"},
	}})
	h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("RunSessionTools: %v", err)
	}
	if got := sb.files["/workspace/skills/shared-notes/SKILL.md"]; got != "# shared" {
		t.Errorf("the coordinator's own skill = %q, want it materialized", got)
	}
	if got := sb.files["/workspace/skills/member-notes/SKILL.md"]; got != "# member" {
		t.Errorf("the member-only skill = %q, want it materialized", got)
	}
	if len(sb.bulkSizes) != 2 {
		t.Errorf("skill trees written = %v, want 2 (the shared reference is one skill, not two)", sb.bulkSizes)
	}
}

// gaVersion is one version in the stub registry below: the id that addresses it
// on the GA wire, the pre-GA numeric this platform still accepts as a pin, and
// the archive both forms serve.
type gaVersion struct {
	id      string
	numeric string
	body    string
	archive []byte
}

// gaRegistry is a stand-in control plane speaking the GA skills wire: it
// resolves the {version} slot the way plan 39's recording pinned it — an id,
// the alias "latest", and (this platform's registered legacy form, decision 4)
// the numeric — and records the token each route was addressed by. The worker's
// half of that contract is what the test below is about, and driving it against
// a server whose slot is the plan's rather than this repo's pins the worker's
// rule independently of the API half.
type gaRegistry struct {
	skillID   string
	skillName string
	versions  []gaVersion
	pin       string   // the version the stub session's skills[] pins
	echo      string   // the id the retrieve answers with, whatever was asked for
	retrieves []string // the {version} tokens the retrieve route saw, in order
	downloads []string // ... and the /content route
}

// gaSkill builds a three-version registry, newest last. The oldest version
// carries a skillver_ id — a row minted before the GA rename, which decision 9
// keeps addressable forever — and the two newer ones the skver_ minted since.
func gaSkill(t *testing.T) *gaRegistry {
	t.Helper()
	g := &gaRegistry{skillID: "ga-skill", skillName: "ga-notes"}
	for i, id := range []string{"skillver_stub01", "skver_stub02", "skver_stub03"} {
		body := fmt.Sprintf("# v%d", i+1)
		g.versions = append(g.versions, gaVersion{
			id:      id,
			numeric: fmt.Sprintf("17591780106411%02d", i+1),
			body:    body,
			archive: skillArchive(t, g.skillName, map[string]string{"SKILL.md": body}),
		})
	}
	return g
}

// find resolves one {version} slot value the way the GA routes do.
func (g *gaRegistry) find(token string) (gaVersion, bool) {
	if token == "latest" {
		return g.versions[len(g.versions)-1], true
	}
	for _, v := range g.versions {
		if token == v.id || token == v.numeric {
			return v, true
		}
	}
	return gaVersion{}, false
}

func gaNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"not found"}}`)
}

// materialize runs the worker's whole skills pass against the stub with pin in
// the session's skills[], and returns the sandbox it wrote into.
func (g *gaRegistry) materialize(t *testing.T, pin string) *fakeSandbox {
	t.Helper()
	g.pin = pin
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"session","id":%q,"agent":{"skills":[{"type":"custom","skill_id":%q,"version":%q}]}}`,
			r.PathValue("id"), g.skillID, g.pin)
	})
	mux.HandleFunc("GET /v1/skills/{skill}/versions/{version}", func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("version")
		g.retrieves = append(g.retrieves, token)
		v, ok := g.find(token)
		if !ok {
			gaNotFound(w)
			return
		}
		id := v.id
		if g.echo != "" {
			id = g.echo
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"skill_version","id":%q,"skill_id":%q,"name":%q,"description":"stub","created_at":"2026-09-04T00:00:00Z"}`,
			id, r.PathValue("skill"), g.skillName)
	})
	mux.HandleFunc("GET /v1/skills/{skill}/versions/{version}/content", func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("version")
		g.downloads = append(g.downloads, token)
		v, ok := g.find(token)
		if !ok {
			gaNotFound(w)
			return
		}
		w.Header().Set(skills.ArchiveDigestHeader, skills.Digest(v.archive))
		w.Header().Set("Content-Type", "application/zip")
		w.Write(v.archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sb := &fakeSandbox{}
	if err := SetupSkills(context.Background(), NewClient(srv.URL, "env-key"),
		"sesn_stub", sb, "/workspace", func() {}); err != nil {
		t.Fatalf("SetupSkills: %v", err)
	}
	return sb
}

// TestSkillPinResolvesByForm pins the worker's three-way resolution of a stored
// skills[] pin (plan 39 decision 5). The alias goes to the retrieve, which is
// the only thing that resolves it, and the download rides the concrete id it
// answers with — the reference worker's own rule (anthropic-sdk-go
// tools/agenttoolset/skills.go). An id and the pre-GA numeric are already the
// addressing token and are carried through verbatim.
//
// The two id rows are the regression the plan is built on: the two-way "digits
// or else" test this replaces read a pinned version id as the alias "latest"
// and materialized the NEWEST version instead of the pinned one — a wrong
// answer rather than a refusal, and invisible to the agent that pinned it. So
// every row asserts the landed *content*, not just the tokens: the three
// versions of the one skill share a name, hence a landing directory, exactly as
// GA's immutable per-skill slug forces.
func TestSkillPinResolvesByForm(t *testing.T) {
	cases := []struct {
		name string
		pin  func(*gaRegistry) string
		want int // index of the version whose archive must land
		// downloadsBy is the token /content must be addressed by, which is the
		// pin itself for every already-concrete form and the resolved id for
		// the alias.
		downloadsBy func(*gaRegistry) string
	}{
		{"the alias latest", func(*gaRegistry) string { return "latest" }, 2,
			func(g *gaRegistry) string { return g.versions[2].id }},
		{"a skver_ id", func(g *gaRegistry) string { return g.versions[1].id }, 1,
			func(g *gaRegistry) string { return g.versions[1].id }},
		{"a legacy skillver_ id", func(g *gaRegistry) string { return g.versions[0].id }, 0,
			func(g *gaRegistry) string { return g.versions[0].id }},
		{"a legacy numeric pin", func(g *gaRegistry) string { return g.versions[1].numeric }, 1,
			func(g *gaRegistry) string { return g.versions[1].numeric }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := gaSkill(t)
			pin := c.pin(g)
			sb := g.materialize(t, pin)

			if got, want := sb.files["/workspace/skills/ga-notes/SKILL.md"], g.versions[c.want].body; got != want {
				t.Errorf("materialized SKILL.md = %q, want %q: the pin %q must land the version it names, never the newest",
					got, want, pin)
			}
			if want := []string{pin}; !slices.Equal(g.retrieves, want) {
				t.Errorf("retrieve addressed %v, want %v: the pin reaches the retrieve verbatim", g.retrieves, want)
			}
			if want := []string{c.downloadsBy(g)}; !slices.Equal(g.downloads, want) {
				t.Errorf("download addressed %v, want %v", g.downloads, want)
			}
		})
	}
}

// TestSkillPinnedIDOutranksTheRetrievesEcho: an id pin addresses the download
// itself, rather than being replaced by whatever id the retrieve answered with.
// The worker already refuses an archive whose bytes disagree with the digest
// its download advertises; a pin is the same kind of promise one layer up, and
// this worker runs in the customer's own infrastructure at the far end of a
// network. So a retrieve answering a *different* version's id may not redirect
// the download the way an alias legitimately does.
func TestSkillPinnedIDOutranksTheRetrievesEcho(t *testing.T) {
	g := gaSkill(t)
	g.echo = g.versions[2].id // whatever is asked for, answer with the newest
	sb := g.materialize(t, g.versions[0].id)

	if got, want := sb.files["/workspace/skills/ga-notes/SKILL.md"], g.versions[0].body; got != want {
		t.Errorf("materialized SKILL.md = %q, want the pinned version's %q", got, want)
	}
	if want := []string{g.versions[0].id}; !slices.Equal(g.downloads, want) {
		t.Errorf("download addressed %v, want the pinned %v", g.downloads, want)
	}
}

func TestSetupSkillsOverTheWire(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "wire-one", "100", "wire-notes", map[string]string{
		"SKILL.md":  "# wire",
		"ref/a.txt": "aaa",
	})
	// Pinned by the numeric form this platform's {version} slot accepts; the
	// alias and the two id forms are pinned against the GA wire in
	// TestSkillPinResolvesByForm.
	h.refSkills(t, [2]string{"wire-one", "100"})
	h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The skill landed through the environment-key wire path — session GET,
	// version get, /content download.
	if got := sb.files["/workspace/skills/wire-notes/SKILL.md"]; got != "# wire" {
		t.Errorf("SKILL.md = %q", got)
	}
	if got := sb.files["/workspace/skills/wire-notes/ref/a.txt"]; got != "aaa" {
		t.Errorf("nested = %q", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Errorf("tool write = %q", sb.files["/workspace/out.txt"])
	}
	// The whole two-file tree went in ONE batched call, as the executor's twin
	// asserts for the same reason (#206): a file at a time is one sandbox exec
	// per member, and a skill may hold ten thousand.
	if len(sb.bulkSizes) != 1 || sb.bulkSizes[0] != 2 {
		t.Errorf("materializing a two-file skill made WriteFiles calls of sizes %v, want exactly one of size 2",
			sb.bulkSizes)
	}
	// The sentinel records the token the download was addressed by.
	sentinel := sb.files["/workspace/skills/"+skills.SentinelName]
	if !strings.Contains(sentinel, `"100"`) {
		t.Errorf("sentinel = %q", sentinel)
	}

	// An unchanged resolved set skips rewrites on a reclaiming pass.
	sb.files["/workspace/skills/wire-notes/SKILL.md"] = "mutated"
	h.suspend(t, writeUse("out2.txt", "again"))
	if err := h.run(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := sb.files["/workspace/skills/wire-notes/SKILL.md"]; got != "mutated" {
		t.Errorf("unchanged set was rewritten: %q", got)
	}

	// The workdir is agent-writable: a tool call deleting a skill tree while
	// the marker survives must not be trusted — the next pass restores it.
	delete(sb.files, "/workspace/skills/wire-notes/SKILL.md")
	h.suspend(t, writeUse("out3.txt", "thrice"))
	if err := h.run(); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := sb.files["/workspace/skills/wire-notes/SKILL.md"]; got != "# wire" {
		t.Errorf("deleted skill not restored: %q", got)
	}
}

func TestSetupSkillsRefusesSubstitutedArchive(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "wire-swap", "100", "swap-wire", map[string]string{"SKILL.md": "genuine"})
	h.seedSkill(t, "wire-kept", "100", "kept-wire", map[string]string{"SKILL.md": "ok"})
	// A different but perfectly valid archive replaces the stored object; the
	// version row — and so the digest the /content download advertises — is
	// untouched. The worker reads that header, never the database.
	h.swapArchive(t, "wire-swap", "100", "swap-wire", map[string]string{"SKILL.md": "tampered"})
	h.refSkills(t, [2]string{"wire-swap", "100"}, [2]string{"wire-kept", "100"})
	h.suspend(t, writeUse("out.txt", "hello"))

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, ok := sb.files["/workspace/skills/swap-wire/SKILL.md"]; ok {
		t.Errorf("substituted archive reached the sandbox: %q", got)
	}
	if got := sb.files["/workspace/skills/kept-wire/SKILL.md"]; got != "ok" {
		t.Errorf("healthy skill = %q", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Error("a corrupt archive blocked the tool run")
	}
	if sentinel := sb.files["/workspace/skills/"+skills.SentinelName]; strings.Contains(sentinel, "wire-swap") {
		t.Errorf("sentinel recorded a refused skill: %q", sentinel)
	}
}

func TestSetupSkillsToleratesVersionWithoutDigest(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "wire-legacy", "100", "legacy-wire", map[string]string{"SKILL.md": "v1"})
	// No recorded digest: the download advertises no header, so there is
	// nothing to verify against and the skill must still materialize.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE skill_versions SET sha256 = NULL WHERE skill_id = $1`, "wire-legacy"); err != nil {
		t.Fatal(err)
	}
	h.refSkills(t, [2]string{"wire-legacy", "100"})
	h.suspend(t, writeUse("out.txt", "x"))

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sb.files["/workspace/skills/legacy-wire/SKILL.md"]; got != "v1" {
		t.Errorf("SKILL.md = %q, want the archive extracted unverified", got)
	}
}

func TestSetupSkillsTolerance(t *testing.T) {
	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "wire-good", "100", "good-wire", map[string]string{"SKILL.md": "ok"})
	h.refSkills(t,
		[2]string{"wire-gone", "100"},
		[2]string{"wire-good", "100"},
	)
	h.suspend(t, writeUse("out.txt", "hello"))

	// A dangling reference (wire 404s) skips; the healthy skill and the tool
	// run both proceed.
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sb.files["/workspace/skills/good-wire/SKILL.md"]; got != "ok" {
		t.Errorf("good skill = %q", got)
	}
	if sb.files["/workspace/out.txt"] != "hello" {
		t.Error("per-skill failure blocked the tool run")
	}
	sentinel := sb.files["/workspace/skills/"+skills.SentinelName]
	if strings.Contains(sentinel, "wire-gone") {
		t.Errorf("sentinel recorded a failed skill: %q", sentinel)
	}
}
