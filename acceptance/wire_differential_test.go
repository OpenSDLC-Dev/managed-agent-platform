package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/respjson"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api/apitest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
)

// approvedWireExtras records the narrow set of extra wire keys intentionally
// emitted beyond a typed anthropic-sdk-go struct, each naming its
// docs/DIVERGENCES.md entry. Any extra key absent here fails the wire
// differential suite so accidental wire drift cannot land unrecorded.
var approvedWireExtras = map[string]map[string]string{
	// Deliberately empty by default: every typed SDK surface exercised below
	// must decode without undocumented extra keys unless explicitly justified
	// in docs/DIVERGENCES.md.
}

// approvedOmittedRequired records the narrow set of SDK `api:"required"` keys
// intentionally omitted on the wire, each naming its docs/DIVERGENCES.md entry.
var approvedOmittedRequired = map[string]map[string]string{
	// Deliberately empty: every `api:"required"` field on the exercised surfaces
	// is emitted on the wire (or explicit JSON null when tagged `nullable`).
}

func surfaceFamily(surface string) string {
	if i := strings.IndexByte(surface, '.'); i >= 0 {
		return surface[:i]
	}
	return surface
}

func jsonFieldName(tag string) string {
	name, _, _ := strings.Cut(tag, ",")
	return name
}

// assertWireShape verifies both halves of SDK wire compatibility on a decoded
// anthropic-sdk-go response struct:
//  1. No unknown extra wire fields outside approvedWireExtras[surface].
//  2. Every field tagged `api:"required"` whose companion `JSON.<Field>`
//     respjson.Field exists is present on the wire (Valid() == true, or
//     explicit JSON null for nullable required fields), unless explicitly
//     justified in approvedOmittedRequired with a docs/DIVERGENCES.md citation.
//     This closes the inverse check noted on assertNoExtras in dcf_test.go.
func assertWireShape(t *testing.T, surface string, val any) {
	t.Helper()
	rv := reflect.ValueOf(val)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			t.Fatalf("%s: nil response value", surface)
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		t.Fatalf("%s: expected struct, got %s", surface, rv.Kind())
	}

	jsonField := rv.FieldByName("JSON")
	if !jsonField.IsValid() || jsonField.Kind() != reflect.Struct {
		t.Fatalf("%s: struct %s has no respjson metadata struct JSON", surface, rv.Type())
	}

	allowed := approvedWireExtras[surface]
	if extrasVal := jsonField.FieldByName("ExtraFields"); extrasVal.IsValid() {
		if extras, ok := extrasVal.Interface().(map[string]respjson.Field); ok {
			for k := range extras {
				if _, ok := allowed[k]; !ok {
					t.Errorf("%s: undocumented extra wire field %q (record in docs/DIVERGENCES.md and approvedWireExtras if intentional)", surface, k)
				}
			}
		}
	}

	omittedOK := approvedOmittedRequired[surfaceFamily(surface)]
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.Name == "JSON" || !sf.IsExported() {
			continue
		}
		apiTag := sf.Tag.Get("api")
		if !hasTagToken(apiTag, "required") {
			continue
		}
		meta := jsonField.FieldByName(sf.Name)
		if !meta.IsValid() {
			continue
		}
		rf, ok := meta.Interface().(respjson.Field)
		if !ok {
			continue
		}
		if rf.Valid() || rf.Raw() == "null" {
			continue
		}
		wireKey := jsonFieldName(sf.Tag.Get("json"))
		if rf.Raw() == "" {
			if _, ok := omittedOK[wireKey]; ok {
				continue
			}
		}
		t.Errorf("%s: required SDK field %s (%s) was missing or invalid on the wire (raw=%q)", surface, sf.Name, wireKey, rf.Raw())
	}
}

func hasTagToken(tag, want string) bool {
	for _, part := range strings.Split(tag, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

type wireErrorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func assertErrorEnvelope(t *testing.T, baseURL, key, method, path, body string, wantStatus int, wantErrType string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, r)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read error body %s %s: %v", method, path, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status = %d, want %d; body: %s", method, path, resp.StatusCode, wantStatus, raw)
	}
	var env wireErrorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("%s %s: invalid JSON error envelope: %v; body: %s", method, path, err, raw)
	}
	if env.Type != "error" || env.Error.Type != wantErrType || env.Error.Message == "" {
		t.Errorf("%s %s: envelope = %+v, want type=error error.type=%q non-empty message", method, path, env, wantErrType)
	}
	if env.RequestID == "" {
		t.Errorf("%s %s: error envelope missing request_id", method, path)
	}
}

// TestWireDifferentialManagedAgentsSurfaces drives every major Managed Agents
// resource family through the pinned anthropic-sdk-go typed client and checks
// bidirectional wire conformance (required fields present + zero undocumented
// extra fields), ID prefixes, multi-page cursor pagination, and structured
// Anthropic error envelopes.
func TestWireDifferentialManagedAgentsSurfaces(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	blobs := blobtest.Mem()
	cipher, err := local.New(local.Config{KeyID: "wire-k1", Key: bytes.Repeat([]byte{0x11}, 32)})
	if err != nil {
		t.Fatalf("build local cipher: %v", err)
	}
	srv := httptest.NewServer(api.NewHandler(pool, blobs, cipher, nil, apitest.PasswordLogin(pool)))
	t.Cleanup(srv.Close)

	key := apitest.SetupKey(t, pool, srv.URL)
	client := anthropic.NewClient(option.WithAPIKey(key), option.WithBaseURL(srv.URL))

	t.Run("Agents", func(t *testing.T) {
		a1, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
			Name:        "wire-agent-1",
			Description: anthropic.String("first agent"),
			Model:       anthropic.BetaManagedAgentsModelConfigParams{ID: "claude-sonnet-4-6"},
			Metadata:    map[string]string{"suite": "wire-diff"},
		})
		if err != nil {
			t.Fatalf("create agent 1: %v", err)
		}
		if !strings.HasPrefix(a1.ID, "agent_") || a1.Version != 1 {
			t.Fatalf("agent 1 id=%q version=%d, want agent_* v1", a1.ID, a1.Version)
		}
		assertWireShape(t, "agent.create", a1)

		a2, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
			Name:  "wire-agent-2",
			Model: anthropic.BetaManagedAgentsModelConfigParams{ID: "claude-sonnet-4-6"},
		})
		if err != nil {
			t.Fatalf("create agent 2: %v", err)
		}
		assertWireShape(t, "agent.create.second", a2)

		got, err := client.Beta.Agents.Get(ctx, a1.ID, anthropic.BetaAgentGetParams{})
		if err != nil {
			t.Fatalf("get agent: %v", err)
		}
		assertWireShape(t, "agent.get", got)

		updated, err := client.Beta.Agents.Update(ctx, a1.ID, anthropic.BetaAgentUpdateParams{
			Version: anthropic.Int(1),
			Name:    anthropic.String("wire-agent-1-renamed"),
		})
		if err != nil {
			t.Fatalf("update agent: %v", err)
		}
		if updated.Version != 2 || updated.Name != "wire-agent-1-renamed" {
			t.Errorf("updated agent = v%d %q, want v2 renamed", updated.Version, updated.Name)
		}
		assertWireShape(t, "agent.update", updated)

		// Paginate with limit=1 and verify all created agents are visited.
		seen := map[string]bool{}
		pager := client.Beta.Agents.ListAutoPaging(ctx, anthropic.BetaAgentListParams{Limit: anthropic.Int(1)})
		for pager.Next() {
			cur := pager.Current()
			assertWireShape(t, "agent.list.item", cur)
			seen[cur.ID] = true
		}
		if err := pager.Err(); err != nil {
			t.Fatalf("list agents autopaging: %v", err)
		}
		if !seen[a1.ID] || !seen[a2.ID] {
			t.Errorf("agents pagination missed created ids: seen=%v", seen)
		}

		archived, err := client.Beta.Agents.Archive(ctx, a2.ID, anthropic.BetaAgentArchiveParams{})
		if err != nil {
			t.Fatalf("archive agent: %v", err)
		}
		if !archived.JSON.ArchivedAt.Valid() {
			t.Errorf("archived agent missing archived_at")
		}
		assertWireShape(t, "agent.archive", archived)

		// Stale version conflict must return 409 invalid_request_error.
		assertErrorEnvelope(t, srv.URL, key, http.MethodPost, "/v1/agents/"+a1.ID,
			`{"version":1,"name":"stale"}`, http.StatusConflict, "invalid_request_error")
	})

	t.Run("Environments", func(t *testing.T) {
		cloudEnv, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
			Name:        "wire-env-cloud",
			Description: anthropic.String("limited cloud env"),
			Config: anthropic.BetaEnvironmentNewParamsConfigUnion{
				OfCloud: &anthropic.BetaCloudConfigParams{
					Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{
						OfLimited: &anthropic.BetaLimitedNetworkParams{
							AllowedHosts: []string{"api.example.com"},
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("create cloud env: %v", err)
		}
		if !strings.HasPrefix(cloudEnv.ID, "env_") {
			t.Fatalf("env id = %q, want env_*", cloudEnv.ID)
		}
		assertWireShape(t, "environment.create.cloud", cloudEnv)

		byocEnv, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
			Name: "wire-env-byoc",
			Config: anthropic.BetaEnvironmentNewParamsConfigUnion{
				OfSelfHosted: &anthropic.BetaSelfHostedConfigParams{},
			},
		})
		if err != nil {
			t.Fatalf("create byoc env: %v", err)
		}
		assertWireShape(t, "environment.create.byoc", byocEnv)

		got, err := client.Beta.Environments.Get(ctx, cloudEnv.ID, anthropic.BetaEnvironmentGetParams{})
		if err != nil {
			t.Fatalf("get env: %v", err)
		}
		assertWireShape(t, "environment.get", got)

		updated, err := client.Beta.Environments.Update(ctx, cloudEnv.ID, anthropic.BetaEnvironmentUpdateParams{
			Description: anthropic.String("updated description"),
		})
		if err != nil {
			t.Fatalf("update env: %v", err)
		}
		assertWireShape(t, "environment.update", updated)

		seen := map[string]bool{}
		pager := client.Beta.Environments.ListAutoPaging(ctx, anthropic.BetaEnvironmentListParams{Limit: anthropic.Int(1)})
		for pager.Next() {
			cur := pager.Current()
			assertWireShape(t, "environment.list.item", cur)
			seen[cur.ID] = true
		}
		if err := pager.Err(); err != nil {
			t.Fatalf("list envs autopaging: %v", err)
		}
		if !seen[cloudEnv.ID] || !seen[byocEnv.ID] {
			t.Errorf("environments pagination missed ids: seen=%v", seen)
		}

		archived, err := client.Beta.Environments.Archive(ctx, byocEnv.ID, anthropic.BetaEnvironmentArchiveParams{})
		if err != nil {
			t.Fatalf("archive env: %v", err)
		}
		assertWireShape(t, "environment.archive", archived)
	})

	t.Run("SessionsAndEvents", func(t *testing.T) {
		env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{Name: "wire-sess-env"})
		if err != nil {
			t.Fatalf("create env: %v", err)
		}
		agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
			Name:  "wire-sess-agent",
			Model: anthropic.BetaManagedAgentsModelConfigParams{ID: "claude-sonnet-4-6"},
		})
		if err != nil {
			t.Fatalf("create agent: %v", err)
		}

		s1, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
			Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
			EnvironmentID: env.ID,
			Title:         anthropic.String("wire-session-1"),
			Metadata:      map[string]string{"k": "v1"},
		})
		if err != nil {
			t.Fatalf("create session 1: %v", err)
		}
		if !strings.HasPrefix(s1.ID, "sesn_") {
			t.Fatalf("session id = %q, want sesn_*", s1.ID)
		}
		assertWireShape(t, "session.create", s1)

		s2, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
			Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
			EnvironmentID: env.ID,
			Title:         anthropic.String("wire-session-2"),
		})
		if err != nil {
			t.Fatalf("create session 2: %v", err)
		}
		assertWireShape(t, "session.create.second", s2)

		updated, err := client.Beta.Sessions.Update(ctx, s1.ID, anthropic.BetaSessionUpdateParams{
			Title: anthropic.String("wire-session-1-updated"),
		})
		if err != nil {
			t.Fatalf("update session: %v", err)
		}
		assertWireShape(t, "session.update", updated)

		sent, err := client.Beta.Sessions.Events.Send(ctx, s1.ID, anthropic.BetaSessionEventSendParams{
			Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
				OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
					Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
					Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
						OfText: &anthropic.BetaManagedAgentsTextBlockParam{
							Type: anthropic.BetaManagedAgentsTextBlockTypeText,
							Text: "hello wire diff",
						},
					}},
				},
			}},
		})
		if err != nil {
			t.Fatalf("send event: %v", err)
		}
		if len(sent.Data) != 1 || !strings.HasPrefix(sent.Data[0].ID, "sevt_") {
			t.Fatalf("sent events = %+v, want 1 sevt_*", sent.Data)
		}

		evPage, err := client.Beta.Sessions.Events.List(ctx, s1.ID, anthropic.BetaSessionEventListParams{})
		if err != nil {
			t.Fatalf("list session events: %v", err)
		}
		if len(evPage.Data) == 0 {
			t.Fatalf("session events list empty")
		}

		seen := map[string]bool{}
		pager := client.Beta.Sessions.ListAutoPaging(ctx, anthropic.BetaSessionListParams{Limit: anthropic.Int(1)})
		for pager.Next() {
			cur := pager.Current()
			assertWireShape(t, "session.list.item", cur)
			seen[cur.ID] = true
		}
		if err := pager.Err(); err != nil {
			t.Fatalf("list sessions autopaging: %v", err)
		}
		if !seen[s1.ID] || !seen[s2.ID] {
			t.Errorf("sessions pagination missed ids: seen=%v", seen)
		}

		archived, err := client.Beta.Sessions.Archive(ctx, s2.ID, anthropic.BetaSessionArchiveParams{})
		if err != nil {
			t.Fatalf("archive session: %v", err)
		}
		assertWireShape(t, "session.archive", archived)
	})

	t.Run("VaultsAndCredentials", func(t *testing.T) {
		v, err := client.Beta.Vaults.New(ctx, anthropic.BetaVaultNewParams{
			DisplayName: "wire-vault",
			Metadata:    map[string]string{"env": "test"},
		})
		if err != nil {
			t.Fatalf("create vault: %v", err)
		}
		if !strings.HasPrefix(v.ID, "vlt_") {
			t.Fatalf("vault id = %q, want vlt_*", v.ID)
		}
		assertWireShape(t, "vault.create", v)

		gotVault, err := client.Beta.Vaults.Get(ctx, v.ID, anthropic.BetaVaultGetParams{})
		if err != nil {
			t.Fatalf("get vault: %v", err)
		}
		assertWireShape(t, "vault.get", gotVault)

		const secretToken = "super-secret-wire-bearer-token-999"
		cred, err := client.Beta.Vaults.Credentials.New(ctx, v.ID, anthropic.BetaVaultCredentialNewParams{
			DisplayName: anthropic.String("wire-mcp-token"),
			Auth: anthropic.BetaVaultCredentialNewParamsAuthUnion{
				OfStaticBearer: &anthropic.BetaManagedAgentsStaticBearerCreateParams{
					Type:         anthropic.BetaManagedAgentsStaticBearerCreateParamsTypeStaticBearer,
					Token:        secretToken,
					MCPServerURL: "https://mcp.example.com",
				},
			},
		})
		if err != nil {
			t.Fatalf("create credential: %v", err)
		}
		if !strings.HasPrefix(cred.ID, "vcrd_") {
			t.Fatalf("credential id = %q, want vcrd_*", cred.ID)
		}
		assertWireShape(t, "credential.create", cred)
		if strings.Contains(cred.RawJSON(), secretToken) {
			t.Fatalf("credential create response leaked plaintext secret token on wire")
		}

		gotCred, err := client.Beta.Vaults.Credentials.Get(ctx, cred.ID, anthropic.BetaVaultCredentialGetParams{VaultID: v.ID})
		if err != nil {
			t.Fatalf("get credential: %v", err)
		}
		assertWireShape(t, "credential.get", gotCred)
		if strings.Contains(gotCred.RawJSON(), secretToken) {
			t.Fatalf("credential get response leaked plaintext secret token on wire")
		}

		delCred, err := client.Beta.Vaults.Credentials.Delete(ctx, cred.ID, anthropic.BetaVaultCredentialDeleteParams{VaultID: v.ID})
		if err != nil {
			t.Fatalf("delete credential: %v", err)
		}
		assertWireShape(t, "credential.delete", delCred)

		archVault, err := client.Beta.Vaults.Archive(ctx, v.ID, anthropic.BetaVaultArchiveParams{})
		if err != nil {
			t.Fatalf("archive vault: %v", err)
		}
		assertWireShape(t, "vault.archive", archVault)
	})

	t.Run("Files", func(t *testing.T) {
		payload := []byte("wire differential file body\n")
		uploaded, err := client.Beta.Files.Upload(ctx, anthropic.BetaFileUploadParams{
			File: anthropic.File(bytes.NewReader(payload), "wire-diff.txt", "text/plain"),
		})
		if err != nil {
			t.Fatalf("upload file: %v", err)
		}
		if !strings.HasPrefix(uploaded.ID, "file_") {
			t.Fatalf("file id = %q, want file_*", uploaded.ID)
		}
		assertWireShape(t, "file.upload", uploaded)

		meta, err := client.Beta.Files.GetMetadata(ctx, uploaded.ID, anthropic.BetaFileGetMetadataParams{})
		if err != nil {
			t.Fatalf("get file metadata: %v", err)
		}
		assertWireShape(t, "file.metadata", meta)
		if meta.Downloadable {
			t.Errorf("user-uploaded file reported downloadable=true, want false")
		}
		// User-uploaded files cannot be downloaded (only tool-generated outputs can).
		assertErrorEnvelope(t, srv.URL, key, http.MethodGet, "/v1/files/"+uploaded.ID+"/content",
			"", http.StatusBadRequest, "invalid_request_error")

		del, err := client.Beta.Files.Delete(ctx, uploaded.ID, anthropic.BetaFileDeleteParams{})
		if err != nil {
			t.Fatalf("delete file: %v", err)
		}
		assertWireShape(t, "file.delete", del)
	})

	t.Run("Skills", func(t *testing.T) {
		skillMD := "---\nname: wire-diff-skill\ndescription: Wire differential smoke skill\n---\n# Wire Diff\n"
		created, err := client.Beta.Skills.New(ctx, anthropic.BetaSkillNewParams{
			DisplayName: anthropic.String("Wire Diff Skill"),
			Files: []io.Reader{
				anthropic.File(strings.NewReader(skillMD), "wire-diff-skill/SKILL.md", "text/markdown"),
			},
		})
		if err != nil {
			t.Fatalf("create skill: %v", err)
		}
		if !strings.HasPrefix(created.ID, "skill_") {
			t.Fatalf("skill id = %q, want skill_*", created.ID)
		}
		assertWireShape(t, "skill.create", created)

		got, err := client.Beta.Skills.Get(ctx, created.ID, anthropic.BetaSkillGetParams{})
		if err != nil {
			t.Fatalf("get skill: %v", err)
		}
		assertWireShape(t, "skill.get", got)

		vers, err := client.Beta.Skills.Versions.List(ctx, created.ID, anthropic.BetaSkillVersionListParams{})
		if err != nil {
			t.Fatalf("list skill versions: %v", err)
		}
		if len(vers.Data) != 1 {
			t.Fatalf("skill versions count = %d, want 1", len(vers.Data))
		}
		assertWireShape(t, "skill.version.list.item", vers.Data[0])

		// Deleting a Skill's only version is refused with 400 invalid_request_error.
		assertErrorEnvelope(t, srv.URL, key, http.MethodDelete,
			"/v1/skills/"+created.ID+"/versions/"+vers.Data[0].ID,
			"", http.StatusBadRequest, "invalid_request_error")

		// Creating a second version allows deleting the first version cleanly.
		skillMDv2 := "---\nname: wire-diff-skill\ndescription: Wire differential smoke skill v2\n---\n# Wire Diff v2\n"
		v2, err := client.Beta.Skills.Versions.New(ctx, created.ID, anthropic.BetaSkillVersionNewParams{
			Files: []io.Reader{
				anthropic.File(strings.NewReader(skillMDv2), "wire-diff-skill/SKILL.md", "text/markdown"),
			},
		})
		if err != nil {
			t.Fatalf("create skill version 2: %v", err)
		}
		assertWireShape(t, "skill.version.create", v2)

		delVer, err := client.Beta.Skills.Versions.Delete(ctx, vers.Data[0].ID, anthropic.BetaSkillVersionDeleteParams{SkillID: created.ID})
		if err != nil {
			t.Fatalf("delete skill version: %v", err)
		}
		assertWireShape(t, "skill.version.delete", delVer)

		del, err := client.Beta.Skills.Delete(ctx, created.ID, anthropic.BetaSkillDeleteParams{})
		if err != nil {
			t.Fatalf("delete skill: %v", err)
		}
		assertWireShape(t, "skill.delete", del)
	})

	t.Run("MemoryStoresAndMemories", func(t *testing.T) {
		ms, err := client.Beta.MemoryStores.New(ctx, anthropic.BetaMemoryStoreNewParams{
			Name:        "wire-memory-store",
			Description: anthropic.String("wire diff store"),
		})
		if err != nil {
			t.Fatalf("create memory store: %v", err)
		}
		if !strings.HasPrefix(ms.ID, "memstore_") {
			t.Fatalf("memory store id = %q, want memstore_*", ms.ID)
		}
		assertWireShape(t, "memory_store.create", ms)

		mem, err := client.Beta.MemoryStores.Memories.New(ctx, ms.ID, anthropic.BetaMemoryStoreMemoryNewParams{
			Path:    "/notes/wire.md",
			Content: anthropic.String("initial content"),
		})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		if !strings.HasPrefix(mem.ID, "mem_") {
			t.Fatalf("memory id = %q, want mem_*", mem.ID)
		}
		assertWireShape(t, "memory.create", mem)

		// Precondition mismatch on memory delete must answer 409 memory_precondition_failed_error.
		badDigest := strings.Repeat("0", 64)
		assertErrorEnvelope(t, srv.URL, key, http.MethodDelete,
			"/v1/memory_stores/"+ms.ID+"/memories/"+mem.ID+"?expected_content_sha256="+badDigest,
			"", http.StatusConflict, "memory_precondition_failed_error")

		delMem, err := client.Beta.MemoryStores.Memories.Delete(ctx, mem.ID, anthropic.BetaMemoryStoreMemoryDeleteParams{
			MemoryStoreID: ms.ID,
		})
		if err != nil {
			t.Fatalf("delete memory: %v", err)
		}
		assertWireShape(t, "memory.delete", delMem)

		archMS, err := client.Beta.MemoryStores.Archive(ctx, ms.ID, anthropic.BetaMemoryStoreArchiveParams{})
		if err != nil {
			t.Fatalf("archive memory store: %v", err)
		}
		assertWireShape(t, "memory_store.archive", archMS)
	})

	t.Run("ErrorEnvelopes", func(t *testing.T) {
		assertErrorEnvelope(t, srv.URL, "", http.MethodGet, "/v1/agents", "", http.StatusUnauthorized, "authentication_error")
		assertErrorEnvelope(t, srv.URL, key, http.MethodGet, "/v1/agents/agent_doesnotexist", "", http.StatusNotFound, "not_found_error")
		assertErrorEnvelope(t, srv.URL, key, http.MethodPost, "/v1/agents", `{bad-json`, http.StatusBadRequest, "invalid_request_error")
		assertErrorEnvelope(t, srv.URL, key, http.MethodPost, "/v1/environments", `{"name":"x","unknown_key":true}`, http.StatusBadRequest, "invalid_request_error")
	})
}
