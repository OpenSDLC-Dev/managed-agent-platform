package k8s

import (
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// The empty value applies nothing. This keeps pull secrets opt-in: a deployment
// that names none must produce exactly the pod it produced before the knob
// existed (#199).
func TestEmptyImagePullSecretsApplyNothing(t *testing.T) {
	refs, err := parseImagePullSecrets("")
	if err != nil || refs != nil {
		t.Errorf("parseImagePullSecrets(\"\") = %v, %v; want nil, nil", refs, err)
	}
}

func TestParseImagePullSecrets(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"one name", "regcred", []string{"regcred"}},
		// The literal the CI helm job asserts the chart renders from
		// [{name: regcred}, {name: ghcr-pull}] — the two sides of the encoding
		// meet on this string, so they cannot drift unnoticed.
		{"two names", "regcred,ghcr-pull", []string{"regcred", "ghcr-pull"}},
		// Operators write the list with spaces after the commas, exactly as the
		// placement selector tolerates them.
		{"spaces around entries", " regcred , ghcr-pull ", []string{"regcred", "ghcr-pull"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseImagePullSecrets(tc.in)
			if err != nil {
				t.Fatalf("parseImagePullSecrets(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseImagePullSecrets(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i, name := range tc.want {
				if got[i].Name != name {
					t.Errorf("[%d].Name = %q, want %q", i, got[i].Name, name)
				}
			}
		})
	}
}

// Every one of these is a name no Secret can ever carry, or a value that
// quietly means something other than what was written. Passed through, each
// fails every session's image pull for the life of the deployment; refused
// here, it fails the process once, at boot, naming the variable (#65's rule).
func TestParseImagePullSecretsRejectsMalformed(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		// Secret names are DNS-1123 subdomains; uppercase and underscores are
		// the two mistakes a Docker-config habit produces.
		{"uppercase", "RegCred"},
		{"underscore", "reg_cred"},
		{"empty entry", "regcred,,ghcr-pull"},
		{"trailing comma", "regcred,"},
		// A duplicate is almost certainly an editing accident, and which copy
		// the operator meant to keep is not knowable from the string.
		{"duplicate", "regcred,regcred"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseImagePullSecrets(tc.in)
			if err == nil {
				t.Fatalf("parseImagePullSecrets(%q) = %v, want an error", tc.in, got)
			}
			// The message has to name the variable and quote what it got, or an
			// operator reading a crashed executor's last line cannot act on it.
			if !strings.Contains(err.Error(), envImagePullSecrets) {
				t.Errorf("error %q does not name %s", err, envImagePullSecrets)
			}
		})
	}
}

// The registry credential is a property of the deployment, not the session:
// every sandbox pod the provider creates pulls from the same private registry,
// so the parsed references land on the pod unconditionally.
func TestPodSpecCarriesImagePullSecrets(t *testing.T) {
	refs, err := parseImagePullSecrets("regcred,ghcr-pull")
	if err != nil {
		t.Fatalf("parseImagePullSecrets: %v", err)
	}
	p := fakeProvider()
	p.imagePullSecrets = refs
	pod := hardenedPod(t, p, sandbox.Hardening{})
	if len(pod.Spec.ImagePullSecrets) != 2 ||
		pod.Spec.ImagePullSecrets[0].Name != "regcred" ||
		pod.Spec.ImagePullSecrets[1].Name != "ghcr-pull" {
		t.Errorf("pod.Spec.ImagePullSecrets = %+v, want [regcred ghcr-pull]", pod.Spec.ImagePullSecrets)
	}
}

// A gated pod swaps the route-flush init container for the gate sidecar — an
// image that may live in the same private registry as the sandbox image.
// Pod-level ImagePullSecrets covers all containers in the pod, so the gated
// shape must carry the same references as the plain one; losing them here
// would strand exactly the isolated sessions the gate exists for.
func TestImagePullSecretsApplyToAGatedPodToo(t *testing.T) {
	refs, err := parseImagePullSecrets("regcred")
	if err != nil {
		t.Fatalf("parseImagePullSecrets: %v", err)
	}
	p := fakeProvider()
	p.imagePullSecrets = refs
	pod := p.podSpec("map-sesn-gated", "/workspace", sandbox.Spec{
		SessionID:  domain.ID("sesn_gated"),
		Image:      "img",
		Networking: domain.Networking{Type: domain.NetLimited},
		Gate:       &sandbox.GateSpec{Image: "gate:1"},
	}, "tok")
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "regcred" {
		t.Errorf("a gated pod's ImagePullSecrets = %+v, want [regcred]", pod.Spec.ImagePullSecrets)
	}
	if len(pod.Spec.InitContainers) == 0 {
		t.Fatal("the gated shape lost its sidecar; this test is no longer testing what it claims")
	}
}

// The additive guard: a provider with no secrets configured produces a pod with
// the field nil — not empty — so an unconfigured deployment's pods are
// byte-identical to what they were before the knob existed.
func TestPodSpecWithoutImagePullSecretsIsUnchanged(t *testing.T) {
	pod := hardenedPod(t, fakeProvider(), sandbox.Hardening{})
	if pod.Spec.ImagePullSecrets != nil {
		t.Errorf("an unconfigured pod's ImagePullSecrets = %+v, want nil", pod.Spec.ImagePullSecrets)
	}
}

// A malformed value stops the process that read it. New is where both binaries
// turn configuration into a provider, so this is the last point before a bad
// secret name becomes a pod every registry pull rejects — and it must hold
// without a cluster.
func TestNewRejectsMalformedImagePullSecrets(t *testing.T) {
	cfg := Config{ImagePullSecrets: "Bad_Name"}
	// Kubeconfig points nowhere on purpose: the parse must have refused the
	// value before New ever tried to build a client, so the error names the
	// variable rather than the connection.
	cfg.Kubeconfig = t.TempDir() + "/no-such-kubeconfig"
	p, err := New(cfg)
	if err == nil {
		t.Fatalf("New(%+v) = %v, want an error", cfg, p)
	}
	if !strings.Contains(err.Error(), envImagePullSecrets) {
		t.Errorf("error %q does not name %s", err, envImagePullSecrets)
	}
}
