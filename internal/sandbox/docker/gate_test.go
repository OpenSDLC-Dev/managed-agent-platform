package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// fakeMinter is a sandbox.GateTokenMinter (and GateTokenRevoker) that records
// its calls, so a test can assert the provider mints only on create, persists
// exactly the token it put in the gate container, and revokes only on the
// gated→ungated dismantle.
type fakeMinter struct {
	token      string
	generated  int
	persisted  []string
	persistErr error
	revoked    []domain.ID
}

func (m *fakeMinter) Generate() string { m.generated++; return m.token }

func (m *fakeMinter) Persist(_ context.Context, _ domain.ID, token string) error {
	if m.persistErr != nil {
		return m.persistErr
	}
	m.persisted = append(m.persisted, token)
	return nil
}

func (m *fakeMinter) Revoke(_ context.Context, sid domain.ID) error {
	m.revoked = append(m.revoked, sid)
	return nil
}

// revokerFunc adapts a func to sandbox.GateTokenRevoker, so a test can observe
// the provider's state at the moment a revoke lands (the ordering guarantee).
type revokerFunc func(context.Context, domain.ID) error

func (f revokerFunc) Revoke(ctx context.Context, sid domain.ID) error { return f(ctx, sid) }

// gatedSpec is a spec that asks for an egress gate, with a recording minter.
func gatedSpec(m *fakeMinter) sandbox.Spec {
	s := spec()
	s.Gate = &sandbox.GateSpec{Image: "gate:1", ControlplaneURL: "http://cp", TokenMinter: m}
	return s
}

// inspectRef extracts the container name-or-id from a /containers/<ref>/json path.
func inspectRef(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
}

func healthJSON(id, health string, s sandbox.Spec, running bool) string {
	return `{"Id":"` + id + `","State":{"Running":` + boolStr(running) +
		`,"Health":{"Status":"` + health + `"}},"Config":{"Labels":{"` + sessionLabel + `":"` + string(s.SessionID) + `"}}}`
}

// sandboxJSON is an owned, running sandbox container inspect body carrying a
// given NetworkMode — how adoption tells a sandbox paired with the current gate
// (container:<gateID>) from a stale or ungated one — and s's own image and
// workdir, so a container modelled as created from s passes adoption's
// fixed-at-create spec check (#29).
func sandboxJSON(id string, s sandbox.Spec, networkMode string) string {
	workdir := s.Workdir
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	return fmt.Sprintf(`{"Id":%q,"State":{"Running":true},"Config":{"Labels":{%q:%q},"Image":%q,"WorkingDir":%q},"HostConfig":{"NetworkMode":%q}}`,
		id, sessionLabel, string(s.SessionID), s.Image, workdir, networkMode)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestProvisionCreatesGatePair drives the create path: the gate is created first
// (on the gate network, with NET_ADMIN and the minted token), and once healthy
// the sandbox is created inside its network namespace and hardened. The token is
// persisted only after the gate container is created.
func TestProvisionCreatesGatePair(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	s.Gate.OTelEndpoint, s.Gate.OTelInsecure = "otel:4317", true
	s.Env = map[string]string{"API_KEY": "vltph_test"} // a vault placeholder rides along
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	var gateBody, sbBody containerConfig
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName, sbName: // neither exists yet
				http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
			case "gate1": // health poll
				io.WriteString(w, healthJSON("gate1", "healthy", s, true))
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			switch r.URL.Query().Get("name") {
			case gateName:
				json.NewDecoder(r.Body).Decode(&gateBody)
				io.WriteString(w, `{"Id":"gate1"}`)
			case sbName:
				json.NewDecoder(r.Body).Decode(&sbBody)
				io.WriteString(w, `{"Id":"sb1"}`)
			default:
				t.Errorf("unexpected create name %q", r.URL.Query().Get("name"))
			}
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	p.gateNetwork = "gate-net"

	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "sb1" {
		t.Errorf("sandbox id = %q, want sb1", sb.ID())
	}

	// The gate: on the gate network, with NET_ADMIN, carrying the control-plane
	// URL and the minted token.
	if gateBody.Image != "gate:1" {
		t.Errorf("gate image = %q", gateBody.Image)
	}
	if gateBody.HostConfig.NetworkMode != "gate-net" {
		t.Errorf("gate network = %q, want gate-net", gateBody.HostConfig.NetworkMode)
	}
	if !slices.Equal(gateBody.HostConfig.CapAdd, []string{"NET_ADMIN"}) {
		t.Errorf("gate CapAdd = %v, want [NET_ADMIN]", gateBody.HostConfig.CapAdd)
	}
	if !slices.Contains(gateBody.Env, "GATE_TOKEN=gtk_test") || !slices.Contains(gateBody.Env, "CONTROLPLANE_URL=http://cp") {
		t.Errorf("gate env = %v", gateBody.Env)
	}
	// The ownership label on the gate is what makes an orphaned gate reapable:
	// Owned/Reap enumerate by label, so a gate created without it would outlive
	// every teardown pass (plan 24). Both halves must carry it.
	if gateBody.Labels[sessionLabel] != string(s.SessionID) {
		t.Errorf("gate labels = %v, want the session ownership label", gateBody.Labels)
	}
	if sbBody.Labels[sessionLabel] != string(s.SessionID) {
		t.Errorf("sandbox labels = %v, want the session ownership label", sbBody.Labels)
	}
	// The gate exports its egress spans to the deployment's collector.
	for _, want := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT=otel:4317", "OTEL_EXPORTER_OTLP_INSECURE=true"} {
		if !slices.Contains(gateBody.Env, want) {
			t.Errorf("gate env missing %q; got %v", want, gateBody.Env)
		}
	}

	// The sandbox: inside the gate's netns, hardened so it cannot become the
	// gate's uid and slip past the owner-match rule.
	if sbBody.HostConfig.NetworkMode != "container:gate1" {
		t.Errorf("sandbox network = %q, want container:gate1", sbBody.HostConfig.NetworkMode)
	}
	if !slices.Equal(sbBody.HostConfig.CapDrop, []string{"NET_RAW", "SETUID", "SETGID"}) {
		t.Errorf("sandbox CapDrop = %v", sbBody.HostConfig.CapDrop)
	}
	if !slices.Equal(sbBody.HostConfig.SecurityOpt, []string{"no-new-privileges"}) {
		t.Errorf("sandbox SecurityOpt = %v", sbBody.HostConfig.SecurityOpt)
	}
	// The sandbox reaches the world only through the gate's loopback proxy, and
	// the vault placeholder is preserved alongside the injected proxy vars. NO_PROXY
	// is forced empty so nothing the base image baked in lets a client bypass the
	// gate into the owner-match firewall's DROP.
	for _, want := range []string{
		"HTTP_PROXY=http://127.0.0.1:15080", "https_proxy=http://127.0.0.1:15080",
		"NO_PROXY=", "no_proxy=", "API_KEY=vltph_test",
	} {
		if !slices.Contains(sbBody.Env, want) {
			t.Errorf("sandbox env missing %q; got %v", want, sbBody.Env)
		}
	}

	// Minted once, on create, and persisted after the gate container existed.
	if m.generated != 1 {
		t.Errorf("Generate called %d times, want 1", m.generated)
	}
	if !slices.Equal(m.persisted, []string{"gtk_test"}) {
		t.Errorf("persisted = %v, want [gtk_test]", m.persisted)
	}
}

// TestGateConfigOmitsOTLPWhenUnset: an empty OTelEndpoint injects no OTEL_* env,
// so the gate runs without an exporter exactly as the executor does with an empty
// endpoint — never a bogus empty OTEL_EXPORTER_OTLP_ENDPOINT= that telemetry.Init
// would treat as configured.
func TestGateConfigOmitsOTLPWhenUnset(t *testing.T) {
	cfg := gateConfig(gatedSpec(&fakeMinter{token: "gtk_x"}), "gtk_x", "bridge")
	for _, e := range cfg.Env {
		if strings.HasPrefix(e, "OTEL_") {
			t.Errorf("gate env carries %q with no OTLP endpoint configured", e)
		}
	}
}

// TestProvisionAdoptsExistingGate is the normal path: on a re-provision both
// halves already run, so the gate is adopted and no token is minted — minting
// would revoke the token the live gate is authenticating with.
func TestProvisionAdoptsExistingGate(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName:
				io.WriteString(w, healthJSON("gate1", "healthy", s, true))
			case sbName:
				io.WriteString(w, sandboxJSON("sb1", s, "container:gate1"))
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			t.Errorf("nothing should be created on the adopt path")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "sb1" {
		t.Errorf("sandbox id = %q, want sb1", sb.ID())
	}
	if m.generated != 0 {
		t.Errorf("Generate called %d times on adopt, want 0 — a re-mint would revoke the live gate", m.generated)
	}
}

// TestProvisionWaitsForAdoptedRunningGateHealth: adopting a running gate that is
// not yet healthy (Docker reports Running before the gate finishes installing its
// firewall and its proxy) waits for healthy before it admits a sandbox — the
// adopt path is not a hole in the healthy-before-admit invariant.
func TestProvisionWaitsForAdoptedRunningGateHealth(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	healthPolls := 0
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName:
				io.WriteString(w, healthJSON("gate1", "starting", s, true)) // running, not yet healthy
			case "gate1":
				healthPolls++
				status := "starting"
				if healthPolls >= 2 {
					status = "healthy"
				}
				io.WriteString(w, healthJSON("gate1", status, s, true))
			case sbName:
				http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			io.WriteString(w, `{"Id":"sb1"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "sb1" {
		t.Errorf("sandbox id = %q, want sb1", sb.ID())
	}
	if healthPolls < 2 {
		t.Errorf("adopted gate not waited healthy (polls=%d, want ≥2)", healthPolls)
	}
	if m.generated != 0 {
		t.Errorf("Generate called %d times adopting a running gate, want 0", m.generated)
	}
}

// TestProvisionRejectsAdoptedUnhealthyGate: adopting a running gate that reports
// unhealthy aborts rather than admitting a sandbox into a gate whose egress
// enforcement is not proven.
func TestProvisionRejectsAdoptedUnhealthyGate(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName, "gate1":
				io.WriteString(w, healthJSON("gate1", "unhealthy", s, true)) // running but unhealthy
			case sbName:
				t.Error("sandbox inspected despite an unhealthy adopted gate")
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if _, err := p.Provision(context.Background(), s); err == nil {
		t.Fatal("provision adopted an unhealthy running gate")
	}
}

// TestProvisionGateUnhealthyFailsClosed: a gate that never reports healthy never
// verified its firewall, so provisioning fails and the orphan gate is removed
// rather than the sandbox being created against an unproven gate.
func TestProvisionGateUnhealthyFailsClosed(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	var removed []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName:
				http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
			case "gate1":
				io.WriteString(w, healthJSON("gate1", "unhealthy", s, true))
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			if r.URL.Query().Get("name") == sbName {
				t.Error("sandbox created against an unhealthy gate")
			}
			io.WriteString(w, `{"Id":"gate1"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if _, err := p.Provision(context.Background(), s); err == nil {
		t.Fatal("provision succeeded against an unhealthy gate")
	}
	if !slices.Contains(removed, "gate1") {
		t.Errorf("orphan gate not removed (removed=%v)", removed)
	}
}

// TestProvisionRemovesGateWhenSandboxCreateFails: if the sandbox half fails after
// a fresh gate was created, the gate is not leaked.
func TestProvisionRemovesGateWhenSandboxCreateFails(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	var removed []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName, sbName:
				http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
			case "gate1":
				io.WriteString(w, healthJSON("gate1", "healthy", s, true))
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			if r.URL.Query().Get("name") == sbName {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			io.WriteString(w, `{"Id":"gate1"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if _, err := p.Provision(context.Background(), s); err == nil {
		t.Fatal("provision succeeded despite a failing sandbox create")
	}
	if !slices.Contains(removed, "gate1") {
		t.Errorf("gate leaked after sandbox create failed (removed=%v)", removed)
	}
}

// TestProvisionGateStartErrorRemovesGate: a gate that is created but fails to
// start is removed rather than left as a stopped orphan.
func TestProvisionGateStartErrorRemovesGate(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName := "map-gate-" + string(s.SessionID)

	var removed []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json") && inspectRef(r.URL.Path) == gateName:
			http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
		case r.URL.Path == "/containers/create":
			io.WriteString(w, `{"Id":"gate1"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			http.Error(w, `{"message":"cannot start"}`, http.StatusInternalServerError)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if _, err := p.Provision(context.Background(), s); err == nil {
		t.Fatal("provision succeeded despite a gate start failure")
	}
	if !slices.Contains(removed, "gate1") {
		t.Errorf("gate not removed after start failure (removed=%v)", removed)
	}
}

// TestDestroyRemovesGatePair removes the sandbox and its gate, and treats a 404
// on either as success — and removes the gate even when the sandbox removal errors.
func TestDestroyRemovesGatePair(t *testing.T) {
	var removed []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			id := strings.TrimPrefix(r.URL.Path, "/containers/")
			removed = append(removed, id)
			if id == "sb1" { // sandbox already gone
				http.Error(w, `{"message":"No such container: sb1"}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	})
	c := p.attach("sb1", "/workspace", "gate1")
	if err := c.Destroy(context.Background()); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !slices.Contains(removed, "sb1") || !slices.Contains(removed, "gate1") {
		t.Errorf("destroy did not remove the pair (removed=%v)", removed)
	}
}

// TestDestroyBothFailuresSurface: when both halves of the pair fail to remove,
// the joined error carries both messages — a stuck gate is never masked by the
// sandbox's own removal error.
func TestDestroyBothFailuresSurface(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		msg := `{"message":"sandbox removal stuck"}`
		if strings.HasPrefix(r.URL.Path, "/containers/gate1") {
			msg = `{"message":"gate removal stuck"}`
		}
		http.Error(w, msg, http.StatusInternalServerError)
	})
	err := p.attach("sb1", "/workspace", "gate1").Destroy(context.Background())
	if err == nil {
		t.Fatal("both removals failed but Destroy reported success")
	}
	for _, want := range []string{"sandbox removal stuck", "gate removal stuck"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Destroy error %q is missing %q", err, want)
		}
	}
}

// TestRemoveDetachedSurvivesCancelledContext: best-effort cleanup of a detached
// container must still run when the provisioning context is already cancelled —
// that is the context.WithoutCancel guarantee the cleanup paths rely on.
func TestRemoveDetachedSurvivesCancelledContext(t *testing.T) {
	deleted := false
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/containers/gate1") {
			deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.removeDetached(ctx, "gate1")
	if !deleted {
		t.Error("cancelled context suppressed the detached removal")
	}
}

// TestRemoveDetachedLogsAndSwallowsFailure: a failed best-effort removal is
// warn-logged for the reaper trail and never surfaces — the caller's original
// error is the one that matters.
func TestRemoveDetachedLogsAndSwallowsFailure(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"removal in progress"}`, http.StatusInternalServerError)
	})
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	p.removeDetached(context.Background(), "gate1")
	if !strings.Contains(buf.String(), "gate1") || !strings.Contains(buf.String(), "removal in progress") {
		t.Errorf("removal failure not logged with its container and cause: %q", buf.String())
	}
}

// TestDestroyUngatedRemovesOnlySandbox: without a gate, Destroy removes just the
// sandbox — the pre-gate behavior is unchanged.
func TestDestroyUngatedRemovesOnlySandbox(t *testing.T) {
	var removed []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	})
	c := p.attach("sb1", "/workspace", "")
	if err := c.Destroy(context.Background()); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !slices.Equal(removed, []string{"sb1"}) {
		t.Errorf("removed = %v, want [sb1]", removed)
	}
}

// TestProvisionRecreatesStoppedGate: a stopped gate is not restarted — its token
// may never have been persisted (a crash between create and Persist) or may have
// been revoked. It is removed and a fresh gate is minted in its place, then the
// sandbox is created in the new gate's namespace.
func TestProvisionRecreatesStoppedGate(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	var removed []string
	gateCreates := 0
	healthPolls := 0
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName:
				io.WriteString(w, healthJSON("gateOld", "", s, false)) // owned but stopped
			case "gateNew":
				// Report "starting" once, then "healthy", so waitGateHealthy polls.
				healthPolls++
				status := "starting"
				if healthPolls >= 2 {
					status = "healthy"
				}
				io.WriteString(w, healthJSON("gateNew", status, s, true))
			case sbName:
				http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			switch r.URL.Query().Get("name") {
			case gateName:
				gateCreates++
				io.WriteString(w, `{"Id":"gateNew"}`)
			case sbName:
				io.WriteString(w, `{"Id":"sb1"}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "sb1" {
		t.Errorf("sandbox id = %q, want sb1", sb.ID())
	}
	if !slices.Contains(removed, "gateOld") {
		t.Errorf("stopped gate not removed (removed=%v)", removed)
	}
	if gateCreates != 1 {
		t.Errorf("gate created %d times, want 1 (a fresh gate replacing the stopped one)", gateCreates)
	}
	// A fresh token is minted and persisted; the stopped gate's is not trusted.
	if !slices.Equal(m.persisted, []string{"gtk_test"}) {
		t.Errorf("persisted = %v, want [gtk_test]", m.persisted)
	}
}

// TestProvisionRebuildsMispairedSandbox: a running sandbox that carries this
// session's label but is attached to a different (or no) gate — e.g. a pre-gate
// bridge sandbox from before the session was gated — is removed and rebuilt in
// the current gate's namespace rather than adopted with the wrong egress path.
func TestProvisionRebuildsMispairedSandbox(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	s.GateTokenRevoker = m
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	var removed []string
	sbCreates := 0
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName:
				io.WriteString(w, healthJSON("gate1", "healthy", s, true)) // gate healthy → adopt
			case sbName:
				io.WriteString(w, sandboxJSON("sbOld", s, "bridge")) // directly networked, not this gate
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			if r.URL.Query().Get("name") == sbName {
				sbCreates++
			}
			io.WriteString(w, `{"Id":"sb1"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "sb1" {
		t.Errorf("sandbox id = %q, want the rebuilt sb1", sb.ID())
	}
	if !slices.Contains(removed, "sbOld") {
		t.Errorf("mispaired sandbox not removed (removed=%v)", removed)
	}
	if sbCreates != 1 {
		t.Errorf("sandbox created %d times, want 1 (rebuilt in the gate's netns)", sbCreates)
	}
	if m.generated != 0 {
		t.Errorf("Generate called %d times while adopting a healthy gate, want 0", m.generated)
	}
	if len(m.revoked) != 0 {
		t.Errorf("gated-direction rebuild revoked %v; the token belongs to the adopted gate", m.revoked)
	}
}

// TestProvisionUngatedReshapeDismantlesGatePair: a session whose gate need went
// away re-provisions ungated onto a still-gated pair — the sandbox is
// gate-networked, so it is not adopted; the session's persisted token is revoked
// first (no replacement gate will ever re-mint it, and revoke-before-teardown is
// what lets a failed teardown retry both), then the pair — sandbox and gate — is
// removed and the sandbox rebuilt directly networked (#197).
func TestProvisionUngatedReshapeDismantlesGatePair(t *testing.T) {
	s := spec() // ungated: no GateSpec at all
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	var removed []string
	sbCreates := 0
	var sbBody containerConfig
	var removedAtRevoke []int // daemon removals already issued when each revoke landed
	s.GateTokenRevoker = revokerFunc(func(_ context.Context, sid domain.ID) error {
		if sid != s.SessionID {
			t.Errorf("revoked %s, want %s", sid, s.SessionID)
		}
		removedAtRevoke = append(removedAtRevoke, len(removed))
		return nil
	})
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case sbName:
				io.WriteString(w, sandboxJSON("sbOld", s, "container:gateOld")) // still gate-networked
			case gateName:
				io.WriteString(w, sandboxJSON("gateOld", s, "gate-net")) // the owned gate half
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			if r.URL.Query().Get("name") == sbName {
				sbCreates++
				if err := json.NewDecoder(r.Body).Decode(&sbBody); err != nil {
					t.Errorf("decode rebuilt sandbox create payload: %v", err)
				}
			}
			io.WriteString(w, `{"Id":"sb1"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "sb1" {
		t.Errorf("sandbox id = %q, want the rebuilt sb1", sb.ID())
	}
	// Exactly one revoke, and it landed before any removal — the ordering that
	// lets a teardown failure retry both instead of losing the trigger.
	if !slices.Equal(removedAtRevoke, []int{0}) {
		t.Errorf("revokes landed after %v removals, want exactly one revoke before any", removedAtRevoke)
	}
	for _, id := range []string{"sbOld", "gateOld"} {
		if !slices.Contains(removed, id) {
			t.Errorf("%s not removed (removed=%v) — the pair must be dismantled together", id, removed)
		}
	}
	if sbCreates != 1 {
		t.Errorf("sandbox created %d times, want 1 (rebuilt directly networked)", sbCreates)
	}
	if strings.HasPrefix(sbBody.HostConfig.NetworkMode, "container:") {
		t.Errorf("rebuilt sandbox network = %q; the replacement must not be gate-networked", sbBody.HostConfig.NetworkMode)
	}
}

// TestProvisionUngatedReshapeRefusesForeignGateName: the deterministic gate name
// is only a convention — when a container the platform does not own holds it,
// the dismantle fails on the ownership check (the `ours` precedent every
// adoption path follows) instead of force-removing someone else's container.
func TestProvisionUngatedReshapeRefusesForeignGateName(t *testing.T) {
	s := spec() // ungated
	s.GateTokenRevoker = revokerFunc(func(context.Context, domain.ID) error { return nil })
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	var removed []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case sbName:
				io.WriteString(w, sandboxJSON("sbOld", s, "container:gateOld"))
			case gateName: // held by a container that is not this platform's
				io.WriteString(w, `{"Id":"intruder","State":{"Running":true},"Config":{"Labels":{}}}`)
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if _, err := p.Provision(context.Background(), s); err == nil {
		t.Fatal("provision proceeded past a foreign container holding the gate name; want an error")
	}
	if slices.Contains(removed, "intruder") {
		t.Errorf("the foreign container was removed (removed=%v)", removed)
	}
}

// TestProvisionFailsClosedOnMispairedRaceWinner: losing the sandbox create race
// (409) to a winner that is NOT networked through this session's gate must fail
// closed — the winner is never adopted with the wrong egress path. (The next
// provision's adoption removes and rebuilds the stale winner.)
func TestProvisionFailsClosedOnMispairedRaceWinner(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	sbInspects := 0
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName:
				io.WriteString(w, healthJSON("gate1", "healthy", s, true))
			case sbName:
				sbInspects++
				if sbInspects == 1 {
					http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
					return
				}
				io.WriteString(w, sandboxJSON("sbWinner", s, "bridge")) // wrong egress path
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			http.Error(w, `{"message":"Conflict"}`, http.StatusConflict)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if _, err := p.Provision(context.Background(), s); err == nil {
		t.Fatal("provision adopted a race winner that is not paired with the gate")
	}
}

// TestProvisionRemovesSandboxOnStartFailure: if the sandbox is created but fails
// to start, it is removed (not left as a stopped container whose immutable
// network mode references a gate about to be torn down, which would poison every
// later retry) along with the gate this call created.
func TestProvisionRemovesSandboxOnStartFailure(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	var removed []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/start"):
			if strings.Contains(r.URL.Path, "sb1") { // the sandbox fails to start
				http.Error(w, `{"message":"cannot start"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName, sbName:
				http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
			case "gate1":
				io.WriteString(w, healthJSON("gate1", "healthy", s, true))
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			switch r.URL.Query().Get("name") {
			case gateName:
				io.WriteString(w, `{"Id":"gate1"}`)
			case sbName:
				io.WriteString(w, `{"Id":"sb1"}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if _, err := p.Provision(context.Background(), s); err == nil {
		t.Fatal("provision succeeded despite a sandbox start failure")
	}
	// Both the leaked sandbox and the gate this call created are removed.
	if !slices.Contains(removed, "sb1") || !slices.Contains(removed, "gate1") {
		t.Errorf("start failure did not clean up the pair (removed=%v)", removed)
	}
}

// TestProvisionPullsMissingGateImage: a create that 404s because the gate image
// is absent triggers a pull and one retry.
func TestProvisionPullsMissingGateImage(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	pulled := false
	gateCreates := 0
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/images/create":
			pulled = true // empty body → pullImage reads EOF and succeeds
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName, sbName:
				http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
			case "gate1":
				io.WriteString(w, healthJSON("gate1", "healthy", s, true))
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			switch r.URL.Query().Get("name") {
			case gateName:
				gateCreates++
				if gateCreates == 1 {
					http.Error(w, `{"message":"No such image: gate:1"}`, http.StatusNotFound)
					return
				}
				io.WriteString(w, `{"Id":"gate1"}`)
			case sbName:
				io.WriteString(w, `{"Id":"sb1"}`)
			}
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !pulled {
		t.Error("missing gate image was not pulled")
	}
	if sb.ID() != "sb1" {
		t.Errorf("sandbox id = %q, want sb1", sb.ID())
	}
}

// TestProvisionAdoptsGateCreateRace: losing the gate create race (409) adopts the
// winner's gate — and the generated token is discarded unpersisted, so it never
// revokes the token the winner recorded.
func TestProvisionAdoptsGateCreateRace(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName, sbName := "map-gate-"+string(s.SessionID), "map-"+string(s.SessionID)

	gateInspects := 0
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch inspectRef(r.URL.Path) {
			case gateName:
				gateInspects++
				if gateInspects == 1 {
					http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
					return
				}
				io.WriteString(w, healthJSON("gate1", "healthy", s, true)) // the winner's gate
			case "gate1":
				io.WriteString(w, healthJSON("gate1", "healthy", s, true))
			case sbName:
				http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
			default:
				t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
			}
		case r.URL.Path == "/containers/create":
			switch r.URL.Query().Get("name") {
			case gateName:
				http.Error(w, `{"message":"Conflict"}`, http.StatusConflict)
			case sbName:
				io.WriteString(w, `{"Id":"sb1"}`)
			}
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "sb1" {
		t.Errorf("sandbox id = %q, want sb1", sb.ID())
	}
	if m.generated != 1 {
		t.Errorf("Generate called %d times, want 1 (the create attempt)", m.generated)
	}
	if len(m.persisted) != 0 {
		t.Errorf("a lost race persisted %v; it must discard its token so the winner's stays live", m.persisted)
	}
}

// TestProvisionGateInspectErrorFailsClosed: a non-404 error inspecting the gate
// is fatal — the provider does not fall through to create a second gate or a
// sandbox with no gate.
func TestProvisionGateInspectErrorFailsClosed(t *testing.T) {
	m := &fakeMinter{token: "gtk_test"}
	s := gatedSpec(m)
	gateName := "map-gate-" + string(s.SessionID)

	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") && inspectRef(r.URL.Path) == gateName {
			http.Error(w, `{"message":"daemon on fire"}`, http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected %s %s after a fatal inspect", r.Method, r.URL.Path)
	})

	if _, err := p.Provision(context.Background(), s); err == nil {
		t.Fatal("provision succeeded despite a gate inspect error")
	}
	if m.generated != 0 {
		t.Errorf("Generate called %d times after a fatal inspect, want 0", m.generated)
	}
}

// TestProvisionGatePersistErrorRemovesGate: a token-persist failure on create is
// fatal to the pair — the gate would authenticate no fetch — and the gate is
// removed rather than left running with an unrecorded token.
func TestProvisionGatePersistErrorRemovesGate(t *testing.T) {
	m := &fakeMinter{token: "gtk_test", persistErr: errors.New("db down")}
	s := gatedSpec(m)
	gateName := "map-gate-" + string(s.SessionID)

	var removed []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/containers/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			if inspectRef(r.URL.Path) == gateName {
				http.Error(w, `{"message":"No such container: x"}`, http.StatusNotFound)
				return
			}
			t.Errorf("unexpected inspect %q", inspectRef(r.URL.Path))
		case r.URL.Path == "/containers/create":
			io.WriteString(w, `{"Id":"gate1"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if _, err := p.Provision(context.Background(), s); err == nil {
		t.Fatal("provision succeeded despite a persist failure")
	}
	if !slices.Contains(removed, "gate1") {
		t.Errorf("gate not removed after persist failure (removed=%v)", removed)
	}
}
