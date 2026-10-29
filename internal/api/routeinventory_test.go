package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Dropping a method-less registration or inventing a single method must fail
// before HTTP checks. The same source inventory drives both exhaustive suites.
func TestMethodlessProxyRouteInventory(t *testing.T) {
	var got, want []string
	for _, pattern := range registeredRoutePatterns(t) {
		_, path, _ := strings.Cut(pattern, " ")
		if path == "/api/organizations/default/proxy/v1" || strings.HasPrefix(path, "/api/organizations/default/proxy/v1/") {
			got = append(got, pattern)
		}
	}
	for _, path := range []string{"/api/organizations/default/proxy/v1", "/api/organizations/default/proxy/v1/{resource...}"} {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "PROPFIND"} {
			want = append(want, method+" "+path)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("method-less proxy registrations were omitted or only partly enumerated: got=%v want=%v", got, want)
	}

	s := newPrivateServer(t)
	caller := seedMember(t, s, "inventory-reader", "developer", "console")
	cookie, session := s.issue(caller)
	keys := map[string]string{}
	for _, kind := range []string{"cloud", "self_hosted"} {
		body := map[string]any{"name": "inventory-" + kind}
		if kind == "self_hosted" {
			body["config"] = map[string]any{"type": "self_hosted"}
		}
		envID := createEnvironment(t, s.tserver, body)["id"].(string)
		keys[kind+"-live"] = issueKey(t, s.pool, envID, "live")
		keys[kind+"-revoked"] = issueKey(t, s.pool, envID, "revoked")
		revokeViaConsole(t, s.tserver, envID, "revoked")
	}
	exercised := map[string]int{}
	for _, pattern := range got {
		t.Run(pattern, func(t *testing.T) {
			method, template, _ := strings.Cut(pattern, " ")
			path := fillRoute(template, "env_fixture", "sesn_fixture", "file_fixture", "work_fixture", "skill_fixture", "skver_fixture")
			if path != "/api/organizations/default/proxy/v1" && path != "/api/organizations/default/proxy/v1/agents" {
				t.Fatalf("proxy inventory did not address a real resource or its root: %s", path)
			}
			for kind, key := range keys {
				for _, withCookie := range []bool{false, true} {
					name := kind + "/without-cookie"
					h := nativeHeaders("", "")
					if withCookie {
						name = kind + "/with-cookie"
						h = nativeHeaders(cookie, session.CSRFToken)
					}
					h.Set("Authorization", "Bearer "+key)
					t.Run(name, func(t *testing.T) {
						res, raw := s.request(method, path, "{}", h)
						exercised[pattern]++
						if res.Header.Get("x-platform-signed-out") != "" || res.Header.Get("Content-Type") != "application/json" {
							t.Fatalf("environment credential changed cookie state or error headers: %v", res.Header)
						}
						if method == http.MethodHead {
							if res.StatusCode != 401 || len(raw) != 0 {
								t.Fatalf("HEAD environment-key refusal=%d %q", res.StatusCode, raw)
							}
							return
						}
						var obj map[string]any
						if err := json.Unmarshal(raw, &obj); err != nil {
							t.Fatal(err)
						}
						wantErrMsg(t, res.StatusCode, obj, 401, "authentication_error", "a browser session cookie is required")
					})
				}
			}
			// A real developer cookie distinguishes resource operations and method
			// fallbacks from an unserved path. The root itself has no resource.
			wantStatus := http.StatusMethodNotAllowed
			if path == "/api/organizations/default/proxy/v1" {
				wantStatus = http.StatusNotFound
			} else {
				switch method {
				case "GET", "HEAD":
					wantStatus = http.StatusOK
				case "POST":
					wantStatus = http.StatusBadRequest // {} is not an agent create request
				}
			}
			res, raw := s.request(method, path, "{}", nativeHeaders(cookie, session.CSRFToken))
			if res.StatusCode != wantStatus || res.Header.Get("x-platform-signed-out") != "" {
				t.Fatalf("cookie control=%d %s; want %d", res.StatusCode, raw, wantStatus)
			}
			if method == http.MethodHead && len(raw) != 0 {
				t.Fatalf("HEAD cookie control has a body: %q", raw)
			}
		})
	}
	for _, pattern := range want {
		if exercised[pattern] != 8 {
			t.Errorf("%s exercised %d environment-credential cases; want eight", pattern, exercised[pattern])
		}
		t.Logf("actually exercised %s: %d environment-credential requests", pattern, exercised[pattern])
	}
}

// Unknown forms must remain errors, never an empty inventory or an invented GET.
func TestRouteInventoryRejectsUnknownForms(t *testing.T) {
	for _, rendered := range []string{
		"consoleUnregisteredPath", "consoleProxyPathSuffix", "/api/unregistered",
		"GET +consoleUnregisteredPath", "GET +consoleProxyPath+unknown",
		"GET ?", "GET ", " /v1/agents", "GET /v1/agents extra",
	} {
		t.Run(rendered, func(t *testing.T) {
			patterns, err := resolveRoutePatterns(rendered)
			if err == nil || len(patterns) != 0 {
				t.Fatalf("unknown registration was silently admitted or omitted: %q -> %v, %v", rendered, patterns, err)
			}
		})
	}
	for rendered, want := range map[string]string{
		"GET /v1/agents":          "GET /v1/agents",
		"POST +consoleTokensPath": "POST /api/oauth/organizations/default/environments/{id}/tokens",
		"GET +gateconfig.Path":    "GET /internal/v1/gate/config",
	} {
		patterns, err := resolveRoutePatterns(rendered)
		if err != nil || !slices.Equal(patterns, []string{want}) {
			t.Fatalf("existing registration changed: %q -> %v, %v", rendered, patterns, err)
		}
	}
}
