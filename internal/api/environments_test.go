package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// environmentRequiredFields is the BetaEnvironment wire surface (all
// api:"required"). Note: no "state" field — lifecycle is archived_at only.
var environmentRequiredFields = []string{
	"id", "type", "name", "description", "config", "metadata",
	"created_at", "updated_at", "archived_at",
}

func createEnvironment(t *testing.T, s *tserver, body map[string]any) map[string]any {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/environments", body)
	if status != http.StatusOK {
		t.Fatalf("create environment: status %d, body %v", status, res)
	}
	return res
}

var emptyPackages = map[string]any{
	"type": "packages",
	"apt":  []any{}, "cargo": []any{}, "gem": []any{},
	"go": []any{}, "npm": []any{}, "pip": []any{},
}

func TestEnvironmentCreateMinimalDefaultsToCloud(t *testing.T) {
	s := newTestServer(t)
	res := createEnvironment(t, s, map[string]any{"name": "dev"})

	wantFields(t, res, environmentRequiredFields...)
	id, _ := res["id"].(string)
	if len(id) < 4 || id[:4] != "env_" {
		t.Errorf("id = %q, want env_ prefix", id)
	}
	if res["type"] != "environment" {
		t.Errorf(`type = %v, want "environment"`, res["type"])
	}
	if _, hasState := res["state"]; hasState {
		t.Errorf(`response leaks non-wire "state" field: %v`, res)
	}
	if res["scope"] != "organization" {
		t.Errorf(`scope = %v, want "organization" (single-tenant v1)`, res["scope"])
	}
	cfg, _ := res["config"].(map[string]any)
	if cfg["type"] != "cloud" {
		t.Fatalf("default config = %v, want cloud", res["config"])
	}
	if nw, _ := cfg["networking"].(map[string]any); nw["type"] != "unrestricted" {
		t.Errorf("default networking = %v, want unrestricted", cfg["networking"])
	}
	if !reflect.DeepEqual(cfg["packages"], emptyPackages) {
		t.Errorf("default packages = %v, want all six empty lists", cfg["packages"])
	}
	if res["archived_at"] != nil {
		t.Errorf("archived_at = %v, want null", res["archived_at"])
	}
	for _, k := range []string{"created_at", "updated_at"} {
		ts, _ := res[k].(string)
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("%s = %q not RFC3339: %v", k, ts, err)
		}
		if !strings.HasSuffix(ts, "Z") {
			t.Errorf("%s = %q must be UTC (Z suffix), not a local offset", k, ts)
		}
	}
}

func TestEnvironmentCreateSelfHostedAndLimitedCloud(t *testing.T) {
	s := newTestServer(t)

	sh := createEnvironment(t, s, map[string]any{
		"name":   "byoc",
		"config": map[string]any{"type": "self_hosted"},
	})
	if cfg, _ := sh["config"].(map[string]any); !reflect.DeepEqual(cfg, map[string]any{"type": "self_hosted"}) {
		t.Errorf("self_hosted config = %v", sh["config"])
	}

	lim := createEnvironment(t, s, map[string]any{
		"name":        "locked",
		"description": "restricted egress",
		"config": map[string]any{
			"type": "cloud",
			"networking": map[string]any{
				"type":          "limited",
				"allowed_hosts": []any{"api.example.com", "*.internal.example.com"},
			},
			"packages": map[string]any{"pip": []any{"requests==2.32.0"}},
		},
		"metadata": map[string]any{"env": "prod"},
	})
	cfg, _ := lim["config"].(map[string]any)
	nw, _ := cfg["networking"].(map[string]any)
	if nw["type"] != "limited" {
		t.Fatalf("networking = %v", cfg["networking"])
	}
	// Required wire fields of a limited network are always present.
	wantFields(t, nw, "allowed_hosts", "allow_mcp_servers", "allow_package_managers")
	if nw["allow_mcp_servers"] != false {
		t.Errorf("allow_mcp_servers default = %v, want false", nw["allow_mcp_servers"])
	}
	pkgs, _ := cfg["packages"].(map[string]any)
	wantFields(t, pkgs, "type", "apt", "cargo", "gem", "go", "npm", "pip")
	if pkgs["type"] != "packages" {
		t.Errorf("packages type = %v, want %q", pkgs["type"], "packages")
	}
	if pip, _ := pkgs["pip"].([]any); len(pip) != 1 || pip[0] != "requests==2.32.0" {
		t.Errorf("pip packages = %v", pkgs["pip"])
	}
	if lim["description"] != "restricted egress" {
		t.Errorf("description = %v", lim["description"])
	}
}

// TestEnvironmentPackagesTypeKeyAccepted covers #382: the reference SDK types
// packages.type as a discriminator sibling to the six manager lists ("packages"
// is the only value — BetaPackagesParams.Type / BetaPackages.Type in
// anthropic-sdk-go betaenvironment.go), not a package manager, so a cookbook
// body carrying it must not 400, and every response packages object echoes it
// back regardless of whether the request supplied it.
func TestEnvironmentPackagesTypeKeyAccepted(t *testing.T) {
	s := newTestServer(t)

	// The issue's exact repro body (research-desk / production-ready-agent
	// cwc-workshops both send this shape).
	withType := createEnvironment(t, s, map[string]any{
		"name": "with-type",
		"config": map[string]any{
			"type": "cloud",
			"packages": map[string]any{
				"type": "packages",
				"pip":  []any{"edgartools", "pandas"},
			},
		},
	})
	cfg, _ := withType["config"].(map[string]any)
	pkgs, _ := cfg["packages"].(map[string]any)
	if pkgs["type"] != "packages" {
		t.Errorf("packages type = %v, want %q", pkgs["type"], "packages")
	}
	if pip, _ := pkgs["pip"].([]any); len(pip) != 2 || pip[0] != "edgartools" || pip[1] != "pandas" {
		t.Errorf("pip = %v, want [edgartools pandas]", pkgs["pip"])
	}

	// A packages object without "type" still works, and the response still
	// echoes "type":"packages" — there is no other valid discriminator value.
	noType := createEnvironment(t, s, map[string]any{
		"name": "no-type",
		"config": map[string]any{
			"type":     "cloud",
			"packages": map[string]any{"pip": []any{"requests"}},
		},
	})
	cfg2, _ := noType["config"].(map[string]any)
	pkgs2, _ := cfg2["packages"].(map[string]any)
	if pkgs2["type"] != "packages" {
		t.Errorf("packages type (no type key in request) = %v, want %q", pkgs2["type"], "packages")
	}
}

// TestEnvironmentPackagesTypeEchoedForPreEchoStoredConfig pins the invariant
// packagesTypeEcho exists for: "type" is never persisted (added only at
// render time, in renderEnvironment), so the echo cannot depend on how a
// row's config got written. This bypasses normalizeEnvConfig entirely — an
// UPDATE straight through the pool — to simulate a row stored before this
// echo existed (or by any future writer that doesn't stamp it), then asserts
// GET and list still carry "type":"packages".
func TestEnvironmentPackagesTypeEchoedForPreEchoStoredConfig(t *testing.T) {
	s := newTestServer(t)
	env := createEnvironment(t, s, map[string]any{
		"name": "legacy-shaped",
		"config": map[string]any{
			"type":     "cloud",
			"packages": map[string]any{"pip": []any{"requests"}},
		},
	})
	id := env["id"].(string)

	preEchoConfig := `{"type":"cloud","networking":{"type":"unrestricted"},` +
		`"packages":{"apt":[],"cargo":[],"gem":[],"go":[],"npm":[],"pip":["requests"]}}`
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE environments SET config = $2 WHERE id = $1`, id, preEchoConfig); err != nil {
		t.Fatal(err)
	}

	status, got := s.do(http.MethodGet, "/v1/environments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, got)
	}
	gotCfg, _ := got["config"].(map[string]any)
	if pkgs, _ := gotCfg["packages"].(map[string]any); pkgs["type"] != "packages" {
		t.Errorf("get on a pre-echo stored config: packages type = %v, want %q", pkgs["type"], "packages")
	}

	status, list := s.do(http.MethodGet, "/v1/environments", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, list)
	}
	var found bool
	for _, e := range listData(t, list) {
		if e["id"] != id {
			continue
		}
		found = true
		cfg, _ := e["config"].(map[string]any)
		if pkgs, _ := cfg["packages"].(map[string]any); pkgs["type"] != "packages" {
			t.Errorf("list on a pre-echo stored config: packages type = %v, want %q", pkgs["type"], "packages")
		}
	}
	if !found {
		t.Fatalf("environment %s not found in list", id)
	}
}

// TestEnvironmentPackagesTypeNeverPersisted pins the storage-side half of the
// echo design: reading environments.config directly through the pool,
// bypassing every render path, so a regression that started persisting the
// key again would be caught here even if every response still looked right.
// Neither a create that supplies "type" nor a later update touching packages
// may write it into the stored config.
func TestEnvironmentPackagesTypeNeverPersisted(t *testing.T) {
	s := newTestServer(t)
	env := createEnvironment(t, s, map[string]any{
		"name": "stored-shape",
		"config": map[string]any{
			"type":     "cloud",
			"packages": map[string]any{"type": "packages", "pip": []any{"requests"}},
		},
	})
	id := env["id"].(string)

	assertNoStoredType := func(step string) {
		t.Helper()
		var raw []byte
		if err := s.pool.QueryRow(context.Background(),
			"SELECT config FROM environments WHERE id = $1", id).Scan(&raw); err != nil {
			t.Fatalf("%s: select config: %v", step, err)
		}
		var stored map[string]any
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatalf("%s: stored config is not valid JSON: %v (%s)", step, err, raw)
		}
		pkgs, _ := stored["packages"].(map[string]any)
		if _, hasType := pkgs["type"]; hasType {
			t.Errorf("%s: stored config carries a packages.type key: %s", step, raw)
		}
	}
	assertNoStoredType("after create")

	status, _ := s.do(http.MethodPost, "/v1/environments/"+id, map[string]any{
		"config": map[string]any{
			"type":     "cloud",
			"packages": map[string]any{"type": "packages", "pip": []any{"requests", "pandas"}},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d", status)
	}
	assertNoStoredType("after update")
}

// TestEnvironmentPackagesTypeEchoSkipsACorruptRow guards packagesTypeEcho's
// no-op fallback. environments_kind_config_agree keeps config->>'type' equal
// to kind even through a direct SQL write, so a genuinely malformed "type"
// can't reach a "cloud" row that way — but "packages" carries no such
// constraint, so a row can still fail to decode into cloudConfigJSON. GET
// must not panic or 500, and must render the stored value unchanged rather
// than guessing at it. This test passes unchanged on main (packagesTypeEcho
// didn't exist there, so a GET always rendered row.config verbatim) — it
// pins the fallback packagesTypeEcho now has to reproduce, not the fix.
func TestEnvironmentPackagesTypeEchoSkipsACorruptRow(t *testing.T) {
	s := newTestServer(t)
	env := createEnvironment(t, s, map[string]any{"name": "corrupt-me"})
	id := env["id"].(string)

	corrupt := []byte(`{"type":"cloud","networking":{"type":"unrestricted"},"packages":"not-an-object"}`)
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE environments SET config = $2 WHERE id = $1", id, corrupt); err != nil {
		t.Fatal(err)
	}

	status, got := s.do(http.MethodGet, "/v1/environments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get on a corrupt row: %d %v", status, got)
	}
	cfg, _ := got["config"].(map[string]any)
	if cfg["packages"] != "not-an-object" {
		t.Errorf("config.packages = %v, want the corrupt value unchanged (packagesTypeEcho's no-op fallback)", cfg["packages"])
	}
	// The networking sibling must render too — proving the fallback returned
	// row.config whole, not just left "packages" alone while reconstructing
	// the rest.
	networking, _ := cfg["networking"].(map[string]any)
	if networking["type"] != "unrestricted" {
		t.Errorf("networking = %v, want the sibling field to survive the fallback too", cfg["networking"])
	}
}

// TestEnvironmentPackagesTypeEchoPreservesUnknownFields guards a second
// regression in packagesTypeEcho: decoding the stored config through the
// typed cloudConfigJSON/packagesJSON structs and re-marshaling silently drops
// any field the current normalizer doesn't know about (a future top-level
// key, a future package manager) — before this test's fix, reads returned the
// stored config verbatim, so this is a real loss of fields no client sent
// this platform to lose. A direct-SQL row carrying both must keep both
// through a GET and a list render, alongside the echoed type.
func TestEnvironmentPackagesTypeEchoPreservesUnknownFields(t *testing.T) {
	s := newTestServer(t)
	env := createEnvironment(t, s, map[string]any{"name": "unknown-fields"})
	id := env["id"].(string)

	withUnknown := []byte(`{"type":"cloud","future_field":"kept",` +
		`"networking":{"type":"unrestricted"},` +
		`"packages":{"apt":[],"cargo":[],"gem":[],"go":[],"npm":[],"pip":[],"future_manager":["x"]}}`)
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE environments SET config = $2 WHERE id = $1", id, withUnknown); err != nil {
		t.Fatal(err)
	}

	checkPreserved := func(t *testing.T, cfg map[string]any) {
		t.Helper()
		if cfg["future_field"] != "kept" {
			t.Errorf("dropped an unknown top-level field: %v", cfg)
		}
		pkgs, _ := cfg["packages"].(map[string]any)
		if pkgs["type"] != "packages" {
			t.Errorf("packages type = %v, want %q", pkgs["type"], "packages")
		}
		if fm, _ := pkgs["future_manager"].([]any); len(fm) != 1 || fm[0] != "x" {
			t.Errorf("dropped an unknown packages field: %v", pkgs)
		}
	}

	status, got := s.do(http.MethodGet, "/v1/environments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, got)
	}
	cfg, _ := got["config"].(map[string]any)
	checkPreserved(t, cfg)

	status, list := s.do(http.MethodGet, "/v1/environments", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, list)
	}
	var found bool
	for _, e := range listData(t, list) {
		if e["id"] != id {
			continue
		}
		found = true
		cfg, _ := e["config"].(map[string]any)
		checkPreserved(t, cfg)
	}
	if !found {
		t.Fatalf("environment %s not found in list", id)
	}
}

// TestEnvironmentPackagesTypeEchoLeavesNullPackagesAlone is the other half of
// the same regression: unmarshaling JSON null into a Go struct field is a
// silent no-op (no error, zero value), so decoding through the typed structs
// could not tell "packages": null apart from an absent-but-populated object
// — it rewrote null into {"apt":null,...,"type":"packages"}. This test
// passes unchanged on main (packagesTypeEcho didn't exist there, so a GET or
// list always rendered row.config verbatim) — it pins the fallback
// packagesTypeEcho now has to reproduce, not the fix: "packages": null must
// render unchanged through both a GET and a list.
func TestEnvironmentPackagesTypeEchoLeavesNullPackagesAlone(t *testing.T) {
	s := newTestServer(t)
	env := createEnvironment(t, s, map[string]any{"name": "null-packages"})
	id := env["id"].(string)

	nullPackages := []byte(`{"type":"cloud","networking":{"type":"unrestricted"},"packages":null}`)
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE environments SET config = $2 WHERE id = $1", id, nullPackages); err != nil {
		t.Fatal(err)
	}

	checkNull := func(t *testing.T, cfg map[string]any) {
		t.Helper()
		if _, present := cfg["packages"]; !present {
			t.Fatalf("packages key missing entirely: %v", cfg)
		}
		if cfg["packages"] != nil {
			t.Errorf("packages = %v, want null unchanged", cfg["packages"])
		}
	}

	status, got := s.do(http.MethodGet, "/v1/environments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, got)
	}
	cfg, _ := got["config"].(map[string]any)
	checkNull(t, cfg)

	status, list := s.do(http.MethodGet, "/v1/environments", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, list)
	}
	var found bool
	for _, e := range listData(t, list) {
		if e["id"] != id {
			continue
		}
		found = true
		cfg, _ := e["config"].(map[string]any)
		checkNull(t, cfg)
	}
	if !found {
		t.Fatalf("environment %s not found in list", id)
	}
}

// TestEnvironmentPackagesTypeRejectsBadValues makes the 400 discriminating.
// Against parsePackages before #382, ANY non-list "type" value already 400s
// (it tries to decode the value into []string), so a bare status/error-type
// check cannot tell "the new type validation fired" from "the old generic
// list-decode failure fired". This asserts the exact message the new "type"
// branch produces. "" and null are present-but-wrong values, not absence —
// only a "type" key missing from the packages object entirely is OK; present
// with any value other than the JSON string "packages" 400s, "" and null
// included.
func TestEnvironmentPackagesTypeRejectsBadValues(t *testing.T) {
	s := newTestServer(t)
	for _, badType := range []any{"something-else", 5, "", nil} {
		status, body := s.do(http.MethodPost, "/v1/environments", map[string]any{
			"name":   "x",
			"config": map[string]any{"type": "cloud", "packages": map[string]any{"type": badType}},
		})
		if status != http.StatusBadRequest {
			t.Fatalf("type=%v: status %d, want 400 (%v)", badType, status, body)
		}
		errObj, _ := body["error"].(map[string]any)
		msg, _ := errObj["message"].(string)
		if want := `packages.type must be "packages"`; msg != want {
			t.Errorf("type=%v: message = %q, want %q", badType, msg, want)
		}
	}
}

func TestEnvironmentCreateValidation(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		body any
	}{
		{"missing name", map[string]any{}},
		{"bad config type", map[string]any{"name": "x", "config": map[string]any{"type": "orbital"}}},
		{"bad networking type", map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "mesh"}}}},
		{"unknown package manager", map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "packages": map[string]any{"brew": []any{"jq"}}}}},
		{"account scope unsupported", map[string]any{"name": "x", "scope": "account"}},
		{"malformed json", `{`},
	}
	for _, tc := range cases {
		status, body := s.do(http.MethodPost, "/v1/environments", tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", tc.name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
}

func TestEnvironmentGetUpdate(t *testing.T) {
	s := newTestServer(t)
	created := createEnvironment(t, s, map[string]any{
		"name": "u", "description": "before",
		"metadata": map[string]any{"keep": "1", "drop": "2"},
	})
	id := created["id"].(string)

	status, got := s.do(http.MethodGet, "/v1/environments/"+id, nil)
	if status != http.StatusOK || got["id"] != id {
		t.Fatalf("get: %d %v", status, got)
	}
	gotCfg, _ := got["config"].(map[string]any)
	if pkgs, _ := gotCfg["packages"].(map[string]any); pkgs["type"] != "packages" {
		t.Errorf("get packages type = %v, want %q", pkgs["type"], "packages")
	}
	status, body := s.do(http.MethodGet, "/v1/environments/env_missing", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")

	// Update: no optimistic version; patch name/description/config/metadata.
	// The config update stays within the environment's kind (cloud) — kind is
	// immutable after creation (see TestEnvironmentKindIsImmutable).
	status, updated := s.do(http.MethodPost, "/v1/environments/"+id, map[string]any{
		"name":        "renamed",
		"description": "after",
		"config":      map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": []any{"internal.corp"}}},
		"metadata":    map[string]any{"drop": nil, "new": "3"},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, updated)
	}
	if updated["name"] != "renamed" || updated["description"] != "after" {
		t.Errorf("name/description = %v/%v", updated["name"], updated["description"])
	}
	cfg, _ := updated["config"].(map[string]any)
	if cfg["type"] != "cloud" {
		t.Errorf("config = %v", updated["config"])
	}
	if net, _ := cfg["networking"].(map[string]any); net["type"] != "limited" {
		t.Errorf("config networking not updated: %v", updated["config"])
	}
	// The update's config patch didn't touch "packages" at all; the echoed
	// type must still survive rather than reverting to empty.
	if pkgs, _ := cfg["packages"].(map[string]any); pkgs["type"] != "packages" {
		t.Errorf("update packages type = %v, want %q", pkgs["type"], "packages")
	}
	if md, _ := updated["metadata"].(map[string]any); !reflect.DeepEqual(md, map[string]any{"keep": "1", "new": "3"}) {
		t.Errorf("metadata = %v", updated["metadata"])
	}

	// Environments alone also delete on empty string (the SDK's
	// map[string]string metadata cannot express null).
	status, updated = s.do(http.MethodPost, "/v1/environments/"+id, map[string]any{
		"metadata": map[string]any{"keep": ""},
	})
	if status != http.StatusOK {
		t.Fatalf("empty-string delete: %d", status)
	}
	if md, _ := updated["metadata"].(map[string]any); !reflect.DeepEqual(md, map[string]any{"new": "3"}) {
		t.Errorf(`metadata after empty-string delete = %v, want {"new":"3"}`, updated["metadata"])
	}

	status, body = s.do(http.MethodPost, "/v1/environments/env_missing", map[string]any{"name": "x"})
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestEnvironmentListPaginationAndArchive(t *testing.T) {
	s := newTestServer(t)
	e1 := createEnvironment(t, s, map[string]any{"name": "e1"})
	e2 := createEnvironment(t, s, map[string]any{"name": "e2"})
	e3 := createEnvironment(t, s, map[string]any{"name": "e3"})

	status, page1 := s.do(http.MethodGet, "/v1/environments?limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d", status)
	}
	d1 := listData(t, page1)
	if len(d1) != 2 || d1[0]["id"] != e3["id"] || d1[1]["id"] != e2["id"] {
		t.Errorf("page 1 = %v, want e3,e2", d1)
	}
	cfg1, _ := d1[0]["config"].(map[string]any)
	if pkgs, _ := cfg1["packages"].(map[string]any); pkgs["type"] != "packages" {
		t.Errorf("list packages type = %v, want %q", pkgs["type"], "packages")
	}
	status, page2 := s.do(http.MethodGet, "/v1/environments?limit=2&page="+nextPage(t, page1), nil)
	if status != http.StatusOK {
		t.Fatalf("page 2: %d", status)
	}
	if d2 := listData(t, page2); len(d2) != 1 || d2[0]["id"] != e1["id"] {
		t.Errorf("page 2 = %v, want e1", d2)
	}

	id := e2["id"].(string)
	status, archived := s.do(http.MethodPost, "/v1/environments/"+id+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive: %d", status)
	}
	if ts, _ := archived["archived_at"].(string); ts == "" {
		t.Fatalf("archived_at = %v", archived["archived_at"])
	}
	archivedCfg, _ := archived["config"].(map[string]any)
	if pkgs, _ := archivedCfg["packages"].(map[string]any); pkgs["type"] != "packages" {
		t.Errorf("archive packages type = %v, want %q", pkgs["type"], "packages")
	}
	_, list := s.do(http.MethodGet, "/v1/environments", nil)
	if entries := listData(t, list); len(entries) != 2 {
		t.Errorf("default list = %d entries, want 2 (archived hidden)", len(entries))
	}
	_, list = s.do(http.MethodGet, "/v1/environments?include_archived=true", nil)
	if entries := listData(t, list); len(entries) != 3 {
		t.Errorf("include_archived = %d entries, want 3", len(entries))
	}
	status, body := s.do(http.MethodPost, "/v1/environments/env_missing/archive", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestEnvironmentDelete(t *testing.T) {
	s := newTestServer(t)
	env := createEnvironment(t, s, map[string]any{"name": "gone"})
	id := env["id"].(string)

	status, res := s.do(http.MethodDelete, "/v1/environments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, res)
	}
	if res["id"] != id || res["type"] != "environment_deleted" {
		t.Errorf("delete response = %v", res)
	}
	status, body := s.do(http.MethodGet, "/v1/environments/"+id, nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	status, body = s.do(http.MethodDelete, "/v1/environments/env_missing", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestEnvironmentDeleteBlockedBySessions(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "a", "model": "m"})
	env := createEnvironment(t, s, map[string]any{"name": "busy"})
	status, sess := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agent["id"], "environment_id": env["id"],
	})
	if status != http.StatusOK {
		t.Fatalf("create session: %d %v", status, sess)
	}

	status, body := s.do(http.MethodDelete, "/v1/environments/"+env["id"].(string), nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Still there.
	status, _ = s.do(http.MethodGet, "/v1/environments/"+env["id"].(string), nil)
	if status != http.StatusOK {
		t.Fatalf("environment vanished after failed delete: %d", status)
	}

	// The message names the referent it actually found. It had blamed sessions
	// for every foreign key on the table, which was harmless while sessions
	// were the only one that could block. The whole sentence, not the word:
	// "sessions" appears in messages that say the opposite too.
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "still has sessions; delete them first") {
		t.Errorf("a session blocking the delete said %q", msg)
	}
}

// A live deployment blocks the delete, and the message has to name the remedy
// that works. It is not the one the sibling refusal trains an operator to
// reach for: archiving the deployment is what makes the environment
// permanently undeletable, while pointing it at another environment clears the
// reference outright. The test does not take the message's word for either —
// it follows the advice and asserts the delete then succeeds.
func TestEnvironmentDeleteBlockedByALiveDeploymentCanBeCleared(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deploymentID := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)

	status, body := s.do(http.MethodDelete, "/v1/environments/"+envID, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, deploymentID) {
		t.Errorf("message %q does not name the blocking deployment %s", msg, deploymentID)
	}
	if strings.Contains(msg, "session") {
		t.Errorf("message %q blames sessions for a deployment's foreign key", msg)
	}
	if !strings.Contains(msg, "point each at another environment") {
		t.Errorf("message %q does not name the remedy that works on a live deployment", msg)
	}
	// Archiving the environment is the irreversible answer and must not be
	// offered while the reversible one is available.
	if strings.Contains(msg, "archive it instead") {
		t.Errorf("message %q sends the operator to archive the environment when a repoint would do", msg)
	}

	// Follow the advice, and check it did what it said: a deployment deleted or
	// moved somewhere unexpected would clear the reference just as well, and
	// leave this asserting less than it reads as.
	other := createEnvironment(t, s, map[string]any{"name": "somewhere-else"})["id"].(string)
	status, moved := s.do(http.MethodPost, "/v1/deployments/"+deploymentID,
		map[string]any{"environment_id": other})
	if status != http.StatusOK {
		t.Fatalf("point the deployment at another environment: %d %v", status, moved)
	}
	if moved["environment_id"] != other {
		t.Errorf("the deployment reports environment_id %v, want %s", moved["environment_id"], other)
	}
	if status, res := s.do(http.MethodDelete, "/v1/environments/"+envID, nil); status != http.StatusOK {
		t.Fatalf("delete after the repoint the message advised: %d %v", status, res)
	}
}

// An archived deployment is the case where the reference really is permanent:
// every update is refused, so it can never be moved off the environment, and
// nothing deletes the row. Here the message must say so and offer the only
// thing left — and stop offering it once it has been done.
func TestEnvironmentDeleteBlockedByAnArchivedDeploymentIsPermanent(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deploymentID := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)

	status, archived := s.do(http.MethodPost, "/v1/deployments/"+deploymentID+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive deployment: %d %v", status, archived)
	}
	// Not just a 200: archive is idempotent, so one that stopped stamping
	// would still answer 200 and leave this testing the live case again.
	if archived["archived_at"] == nil {
		t.Fatal("the deployment archived with a null archived_at")
	}
	// The repoint the live case relies on is genuinely gone.
	other := createEnvironment(t, s, map[string]any{"name": "somewhere-else"})["id"].(string)
	if status, _ := s.do(http.MethodPost, "/v1/deployments/"+deploymentID,
		map[string]any{"environment_id": other}); status == http.StatusOK {
		t.Fatal("an archived deployment accepted a repoint, which would make the message's premise false")
	}

	status, body := s.do(http.MethodDelete, "/v1/environments/"+envID, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, deploymentID) || !strings.Contains(msg, "can no longer be deleted") {
		t.Errorf("message %q, want it to name the deployment and say the environment is stuck", msg)
	}
	if strings.Contains(msg, "point each at another environment") {
		t.Errorf("message %q advises a repoint an archived deployment refuses", msg)
	}
	// The advice has to be there before the check below can mean anything: a
	// message that never offered it would pass that one for free.
	if !strings.Contains(msg, "archive it instead") {
		t.Errorf("message %q offers no remedy at all", msg)
	}

	// Follow that advice too, and check it is not then repeated at someone who
	// has already taken it.
	status, archivedEnv := s.do(http.MethodPost, "/v1/environments/"+envID+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive the environment, which is what the message advised: %d %v", status, archivedEnv)
	}
	if archivedEnv["archived_at"] == nil {
		t.Fatal("the environment archived with a null archived_at")
	}
	status, body = s.do(http.MethodDelete, "/v1/environments/"+envID, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := body["error"].(map[string]any)["message"].(string); strings.Contains(msg, "archive it instead") {
		t.Errorf("message %q tells an already-archived environment to archive itself", msg)
	}
}

// The counted and truncated arms, which the two cases above never reach.
func TestEnvironmentDeleteNamesTheDeploymentsUpToFive(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	var created []string
	for range 7 {
		created = append(created, createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string))
	}

	_, body := s.do(http.MethodDelete, "/v1/environments/"+envID, nil)
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "referenced by 7 deployments") || !strings.Contains(msg, "and 2 more") {
		t.Errorf("seven blocking deployments: %q", msg)
	}
	n := 0
	for _, id := range created {
		if strings.Contains(msg, id) {
			n++
		}
	}
	if n != 5 {
		t.Errorf("message %q names %d of the seven deployments, want the five the cap allows", msg, n)
	}
}

// Postgres reports one violated constraint, and with a session and a deployment
// both in the way it reports the older foreign key — sessions, from migration
// 0001. Reading the constraint's name would therefore have told the operator to
// delete the sessions, which they would do, only to find the delete still
// refused. The message is built from what is actually there instead — and has
// to name *both*, since clearing either alone leaves the delete refused. A
// message that named the deployment and promised the delete would then go
// through would be the first defect over again with the parties swapped.
func TestEnvironmentDeleteBlockedByBothNamesEachOfThem(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	deploymentID := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	if status, res := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID,
	}); status != http.StatusOK {
		t.Fatalf("create session: %d %v", status, res)
	}

	status, body := s.do(http.MethodDelete, "/v1/environments/"+envID, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, deploymentID) {
		t.Errorf("message %q names no deployment — it followed the constraint the error reported", msg)
	}
	if !strings.Contains(msg, "1 session") {
		t.Errorf("message %q counts no sessions, so the repoint it advises would not be enough", msg)
	}
	if !strings.Contains(msg, "delete the sessions") {
		t.Errorf("message %q advises a repoint alone, which leaves the delete refused", msg)
	}

	// Both remedies together clear it. Either alone does not, which is what the
	// message now says.
	other := createEnvironment(t, s, map[string]any{"name": "somewhere-else"})["id"].(string)
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+deploymentID,
		map[string]any{"environment_id": other}); status != http.StatusOK {
		t.Fatalf("repoint: %d %v", status, res)
	}
	if status, _ := s.do(http.MethodDelete, "/v1/environments/"+envID, nil); status != http.StatusBadRequest {
		t.Errorf("delete after the repoint alone: %d, want it still refused by the session", status)
	}
}

// TestEnvironmentKindIsImmutable pins that an environment's cloud/self_hosted
// kind is fixed at creation: a config update that flips the kind is rejected.
// The queue routes work by kind (the executor claims cloud tool_exec, a BYOC
// worker polls self_hosted), so a mid-life switch could hand one item to both.
func TestEnvironmentKindIsImmutable(t *testing.T) {
	s := newTestServer(t)

	cloud := createEnvironment(t, s, map[string]any{"name": "c", "config": map[string]any{"type": "cloud"}})
	status, body := s.do(http.MethodPost, "/v1/environments/"+cloud["id"].(string),
		map[string]any{"config": map[string]any{"type": "self_hosted"}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	self := createEnvironment(t, s, map[string]any{"name": "s", "config": map[string]any{"type": "self_hosted"}})
	status, body = s.do(http.MethodPost, "/v1/environments/"+self["id"].(string),
		map[string]any{"config": map[string]any{"type": "cloud"}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// A same-kind config update still works (kind unchanged).
	status, _ = s.do(http.MethodPost, "/v1/environments/"+self["id"].(string),
		map[string]any{"config": map[string]any{"type": "self_hosted"}})
	if status != http.StatusOK {
		t.Errorf("same-kind config update rejected: status %d", status)
	}
}

// The reference refuses to delete a self_hosted environment whose work queue
// still holds items, in a sentence this platform now repeats (recorded
// 2026-09-02, #546). It is a refusal we already made in the wrong words: the
// items' sessions hold the environment through a foreign key, so the delete
// was refused for having sessions, and clearing those is what cascades the
// queue away — the loss the reference refuses in order to prevent. force=true
// lifts the queue refusal and only that, so a delete forced past it lands on
// the sessions' refusal instead of succeeding as it does on the reference. The
// test follows force there rather than stopping at the 409, and checks the
// queue survived both.
func TestEnvironmentDeleteRefusesASelfHostedQueueUnlessForced(t *testing.T) {
	s := newTestServer(t)
	envID, sessionID, _ := selfHostedWorker(t, s, "ek-delete")
	enqueueOn(t, s, envID, sessionID)

	status, body := s.do(http.MethodDelete, "/v1/environments/"+envID, nil)
	wantErr(t, status, body, http.StatusConflict, "invalid_request_error")
	const want = "Cannot delete self-hosted environment with work in the queue. " +
		"Either archive the environment first to allow the queue to drain, or use force=true to delete immediately."
	if msg, _ := body["error"].(map[string]any)["message"].(string); msg != want {
		t.Errorf("refusal = %q, want the reference's sentence", msg)
	}

	status, body = s.do(http.MethodDelete, "/v1/environments/"+envID+"?force=true", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "still has sessions; delete them first") {
		t.Errorf("the forced delete was refused with %q, want the sessions' refusal", msg)
	}

	var queued int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE environment_id = $1 AND kind = 'tool_exec' AND state = 'queued'`,
		envID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("queued tool_exec items after two refused deletes = %d, want 1", queued)
	}
}

// The refusal reaches exactly what the reference's sentence names, and each of
// the three predicates that decide it gets its own arm — a narrowed one would
// otherwise pass on the case above, which enqueues a queued tool_exec on a
// self_hosted environment and so satisfies all three at once.
//
//   - kind self_hosted: a cloud environment's tool_exec is the platform
//     executor's queue, not a worker's, and the sentence does not speak for it.
//   - kind tool_exec: the four platform-executed kinds never reach the wire
//     (queue's workAPIScope), so a self_hosted environment holding only a
//     model_turn holds nothing a worker could drain.
//   - state: an item is drained once stopped, so a stopped one does not refuse
//     and an active one — mid-flight, and the costliest to lose — does.
//
// The three that must not refuse fall through to the delete, which the
// sessions then refuse in their own words.
func TestEnvironmentDeleteQueueRefusalIsSelfHostedAndUndrainedOnly(t *testing.T) {
	s := newTestServer(t)
	const queueSentence = "work in the queue"

	// A cloud environment's queued tool_exec.
	agentID, cloudEnv := fixture(t, s)
	enqueueToolExec(t, s, agentID, cloudEnv)
	status, body := s.do(http.MethodDelete, "/v1/environments/"+cloudEnv, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := body["error"].(map[string]any)["message"].(string); strings.Contains(msg, queueSentence) {
		t.Errorf("a cloud environment's queued tool_exec drew the self-hosted refusal: %q", msg)
	}

	// A self_hosted environment whose only item is the brain's own model_turn.
	turnEnv, turnSession, _ := selfHostedWorker(t, s, "ek-modelturn")
	if _, err := queue.New(s.pool).Enqueue(context.Background(), s.pool,
		domain.ID(turnEnv), domain.ID(turnSession), queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	status, body = s.do(http.MethodDelete, "/v1/environments/"+turnEnv, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := body["error"].(map[string]any)["message"].(string); strings.Contains(msg, queueSentence) {
		t.Errorf("a model_turn, which no worker polls, drew the refusal: %q", msg)
	}

	// A self_hosted tool_exec that has drained.
	envID, sessionID, _ := selfHostedWorker(t, s, "ek-drained")
	enqueueOn(t, s, envID, sessionID)
	setWorkState(t, s, envID, "stopped")
	status, body = s.do(http.MethodDelete, "/v1/environments/"+envID, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := body["error"].(map[string]any)["message"].(string); strings.Contains(msg, queueSentence) {
		t.Errorf("a drained queue drew the refusal: %q", msg)
	}

	// A self_hosted tool_exec a worker is running right now. Nothing is queued
	// here, so a refusal that only counted 'queued' rows would let this delete
	// through — and it is the item whose loss costs most.
	liveEnv, liveSession, _ := selfHostedWorker(t, s, "ek-active")
	enqueueOn(t, s, liveEnv, liveSession)
	setWorkState(t, s, liveEnv, "active")
	status, body = s.do(http.MethodDelete, "/v1/environments/"+liveEnv, nil)
	wantErr(t, status, body, http.StatusConflict, "invalid_request_error")
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, queueSentence) {
		t.Errorf("an active item did not draw the refusal: %q", msg)
	}
}

// setWorkState drives the environment's work items to one state directly. The
// lifecycle that would reach it needs a worker's ack and heartbeat, which is a
// different subject than the delete's own predicate (the same seam
// worktoken_test.go uses to archive a session).
func setWorkState(t *testing.T, s *tserver, envID, state string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE work_items SET state = $2,
		   stopped_at = CASE WHEN $2 = 'stopped' THEN now() ELSE stopped_at END
		 WHERE environment_id = $1`, envID, state); err != nil {
		t.Fatal(err)
	}
}
