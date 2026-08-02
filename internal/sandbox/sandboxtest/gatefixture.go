// This file is the gate-fixture support for backends (and binaries) that test
// against the real egress-gate image. The pieces are exported separately —
// image build, host address, stand-in controlplane — because cmd/gate's own
// real-firewall test composes them differently from the Docker provider's
// contract harness.

package sandboxtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// GateImageTag is the tag BuildGateImage builds the repo's gate image under.
const GateImageTag = "map-gate:sandboxtest"

var (
	gateImageOnce sync.Once
	gateImageErr  error
)

// BuildGateImage builds the repo's gate image (docker build --target gate)
// once per test process and returns its tag. A build failure is a hard test
// failure — the repo convention for a missing daemon, not a skip.
func BuildGateImage(t *testing.T) string {
	t.Helper()
	gateImageOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			gateImageErr = fmt.Errorf("cannot locate the repo root")
			return
		}
		root := filepath.Join(filepath.Dir(file), "..", "..", "..")
		out, err := exec.Command("docker", "build", "--target", "gate", "-t", GateImageTag, root).CombinedOutput()
		if err != nil {
			gateImageErr = fmt.Errorf("docker build --target gate: %v\n%s", err, out)
		}
	})
	if gateImageErr != nil {
		t.Fatal(gateImageErr)
	}
	return GateImageTag
}

// DockerHostAddr returns an address of the test host reachable from a
// container on Docker's default bridge network: host.docker.internal under
// Docker Desktop for Mac, this host's own routable address under Docker
// Desktop on Windows/WSL, and the bridge gateway IP when the daemon shares
// this process's network namespace. MAP_DOCKER_HOST_ADDR overrides all three,
// as MAP_K8S_HOST_ADDR does for the Kubernetes harness. Listeners meant to be
// reached this way must bind all interfaces, not loopback.
func DockerHostAddr(t *testing.T) string {
	t.Helper()
	if addr := os.Getenv("MAP_DOCKER_HOST_ADDR"); addr != "" {
		return addr
	}
	if runtime.GOOS == "darwin" {
		return "host.docker.internal"
	}
	if DockerDesktop(t) {
		// Desktop on Windows: the daemon is a VM of its own, so the bridge
		// gateway belongs to a namespace this process is not in. Desktop's
		// host.docker.internal is not the answer either, because it is not one
		// address: from a container it resolves to both families (an IPv4 and
		// an IPv6), and a /dev/tcp dial takes whichever getaddrinfo returns
		// first without trying the other. Taking the IPv6 one into a netns with
		// no IPv6 route fails "Network is unreachable" before the firewall is
		// consulted — and the row that must see a *drop* passes on it anyway,
		// so the fixture would measure nothing. An address resolved here has no
		// such step to vary.
		return outboundAddr(t)
	}
	out, err := exec.Command("docker", "network", "inspect", "bridge",
		"--format", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil {
		t.Fatalf("docker network inspect bridge: %v", err)
	}
	addr := strings.TrimSpace(string(out))
	if addr == "" {
		t.Fatal("docker bridge network reports no gateway")
	}
	return addr
}

// DockerDesktop reports whether the daemon under test is Docker Desktop, which
// runs the engine inside its own VM. That — not GOOS — is what decides how a
// container addresses the test process. On macOS the two agree, which is why a
// compile-time check served; under WSL they part company, because the test
// binary is GOOS=linux while the daemon is still a Desktop VM, so the bridge
// gateway names a namespace this process is not in and nothing answers there.
// A daemon that cannot be asked is treated as sharing this namespace: that is
// the pre-existing behaviour, and the caller's own error is the better one.
func DockerDesktop(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("docker", "info", "--format", "{{.OperatingSystem}}").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Docker Desktop")
}

// outboundAddr is this host's IPv4 address on the route out — the one a
// container in another namespace can address it by. The UDP "dial" sends
// nothing; it only asks the kernel which local address that route would use,
// so it needs no reachable destination.
func outboundAddr(t *testing.T) string {
	t.Helper()
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		t.Fatalf("no outbound route to address this host by: %v", err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// GateStub is a stand-in controlplane and egress origin on one host listener:
// it serves the gate's config fetch (gateconfig.Path, bearer-authenticated
// with Token) and an /echo endpoint reflecting the Authorization header and
// body that egress delivered to the origin. The config's policy admits exactly
// the stub's own host and carries one header+body env-var credential.
type GateStub struct {
	// Addr is host:port reachable from a container on the default bridge.
	Addr        string
	Token       string
	Placeholder string
	Secret      string
}

// StartGateStub starts the stub addressed for a container on the default
// bridge, cleaned up with the test.
func StartGateStub(t *testing.T) *GateStub {
	t.Helper()
	return StartGateStubAt(t, DockerHostAddr(t))
}

// StartGateStubAt starts the stub with an explicit host address — the one the
// backend under test's containers can reach the test host at (the Docker
// bridge gateway, a kind network gateway, ...). The listener always binds
// 0.0.0.0; hostAddr only decides how the served config and the fixture name it.
func StartGateStubAt(t *testing.T, hostAddr string) *GateStub {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("gate stub listen: %v", err)
	}
	s := &GateStub{
		Addr:        net.JoinHostPort(hostAddr, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)),
		Token:       "gtk_sandboxtest",
		Placeholder: "vltph_sandboxtest",
		Secret:      "sandboxtest-secret-value",
	}
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		t.Fatalf("gate stub addr: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(gateconfig.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(gateconfig.Config{
			Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
			Credentials: []gateconfig.Credential{{
				CredentialID: "vcrd_sandboxtest",
				Placeholder:  s.Placeholder,
				Secret:       s.Secret,
				Networking: gateconfig.CredentialNetworking{
					Type: domain.NetLimited, AllowedHosts: []string{host},
				},
				InjectionLocation: gateconfig.InjectionLocation{Header: true, Body: true},
			}},
		})
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "authorization=%s\nbody=%s\n", r.Header.Get("Authorization"), body)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

// Minter returns a GateTokenMinter minting the stub's fixed token; Persist is
// a no-op — the stub itself is the token's verifier.
func (s *GateStub) Minter() sandbox.GateTokenMinter { return stubMinter{token: s.Token} }

type stubMinter struct{ token string }

func (m stubMinter) Generate() string                                 { return m.token }
func (m stubMinter) Persist(context.Context, domain.ID, string) error { return nil }
