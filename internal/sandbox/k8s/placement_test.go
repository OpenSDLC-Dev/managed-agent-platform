package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// The empty value applies nothing, on both knobs. This is the row that keeps
// placement opt-in: a deployment that names neither must produce exactly the
// pod it produced before these existed.
func TestEmptyPlacementAppliesNothing(t *testing.T) {
	sel, err := parseNodeSelector("")
	if err != nil || sel != nil {
		t.Errorf("parseNodeSelector(\"\") = %v, %v; want nil, nil", sel, err)
	}
	tol, err := parseTolerations("")
	if err != nil || tol != nil {
		t.Errorf("parseTolerations(\"\") = %v, %v; want nil, nil", tol, err)
	}
}

func TestParseNodeSelector(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want map[string]string
	}{
		{"one pair", "role=sandbox", map[string]string{"role": "sandbox"}},
		{"two pairs", "role=sandbox,pool=isolated",
			map[string]string{"role": "sandbox", "pool": "isolated"}},
		// Operators write the list with spaces after the commas; kubectl's own
		// selector syntax tolerates them, so refusing would be a gratuitous
		// difference from the tool this encoding was borrowed from.
		{"spaces around entries", " role = sandbox , pool = isolated ",
			map[string]string{"role": "sandbox", "pool": "isolated"}},
		// A qualified key is the normal shape for anything a cloud sets.
		{"qualified key", "cloud.google.com/gke-nodepool=sandbox",
			map[string]string{"cloud.google.com/gke-nodepool": "sandbox"}},
		// An empty value is a real selector: it matches nodes carrying the label
		// with the empty value, which is what a bare `kubectl label node x k8s-app=`
		// produces.
		{"empty value", "drained=", map[string]string{"drained": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNodeSelector(tc.in)
			if err != nil {
				t.Fatalf("parseNodeSelector(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseNodeSelector(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// Every one of these is a value the API server would refuse, or one that would
// quietly mean something other than what was written. Passed through, each fails
// every session's pod create for the life of the deployment; refused here, it
// fails the process once, at boot, naming the variable (#65's rule).
func TestParseNodeSelectorRejectsMalformed(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"no separator", "role"},
		{"empty key", "=sandbox"},
		{"empty entry", "role=sandbox,,pool=isolated"},
		{"trailing comma", "role=sandbox,"},
		// Last-wins would be silent, and which one the operator meant is not
		// knowable from the string.
		{"duplicate key", "role=sandbox,role=other"},
		// A key that is not a valid label key can never match: the API server
		// would reject the node label it is looking for.
		{"invalid key", "not a key=sandbox"},
		{"invalid value", "role=not a value"},
		// `=` is the separator, so a value containing one is ambiguous rather
		// than merely invalid.
		{"two separators", "role=sand=box"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNodeSelector(tc.in)
			if err == nil {
				t.Fatalf("parseNodeSelector(%q) = %v, want an error", tc.in, got)
			}
			// The message has to name the variable and quote what it got, or an
			// operator reading a crashed executor's last line cannot act on it.
			if !strings.Contains(err.Error(), envNodeSelector) {
				t.Errorf("error %q does not name %s", err, envNodeSelector)
			}
		})
	}
}

func TestParseTolerations(t *testing.T) {
	got, err := parseTolerations(
		`[{"key":"sandbox","operator":"Equal","value":"true","effect":"NoSchedule"},
		  {"key":"other","operator":"Exists","effect":"NoExecute","tolerationSeconds":30}]`)
	if err != nil {
		t.Fatalf("parseTolerations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d tolerations, want 2: %+v", len(got), got)
	}
	if got[0].Key != "sandbox" || got[0].Operator != corev1.TolerationOpEqual ||
		got[0].Value != "true" || got[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("first toleration = %+v", got[0])
	}
	// tolerationSeconds is the field a flat encoding would have lost, and the
	// reason the plan chose JSON: it is a *pointer*, so "absent" and "0" are
	// different tolerations.
	if got[1].TolerationSeconds == nil || *got[1].TolerationSeconds != 30 {
		t.Errorf("tolerationSeconds = %v, want 30", got[1].TolerationSeconds)
	}
	if got[0].TolerationSeconds != nil {
		t.Errorf("an unset tolerationSeconds became %v, want nil", *got[0].TolerationSeconds)
	}
}

// A toleration that does not tolerate is the failure this knob exists to
// prevent: the sandbox pool is tainted, so every sandbox stays Pending. Each of
// these produces one, silently, if the string is passed through unparsed.
func TestParseTolerationsRejectsMalformed(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"not json", "sandbox=true:NoSchedule"},
		{"an object, not an array", `{"key":"sandbox"}`},
		{"truncated", `[{"key":"sandbox"`},
		// The typo that costs the most: an unknown field is dropped by a
		// permissive decoder, and the toleration silently loses its effect.
		{"misspelled field", `[{"key":"sandbox","efect":"NoSchedule"}]`},
		{"misspelled effect", `[{"key":"sandbox","effect":"NoScheduling"}]`},
		{"misspelled operator", `[{"key":"sandbox","operator":"Equals"}]`},
		// The API server's own rule: an empty key matches every taint, which is
		// only meaningful with Exists.
		{"empty key without Exists", `[{"operator":"Equal","value":"x"}]`},
		// And its converse: Exists is a wildcard over values, so a value
		// alongside it is a contradiction the operator should see.
		{"Exists with a value", `[{"key":"sandbox","operator":"Exists","value":"true"}]`},
		// The rules below were found by probing a live v1.36 API server rather
		// than by reading the type — each of these passed this parser and was
		// then rejected at every pod create, which is precisely the failure the
		// parser exists to move to startup.
		{"key is not a label key", `[{"key":"my pool","operator":"Exists"}]`},
		{"value is not a label value", `[{"key":"sandbox","value":"pool 1"}]`},
		// The vendored type's comment calls tolerationSeconds "ignored" outside
		// NoExecute; the API server rejects it. Measured behaviour wins.
		{"tolerationSeconds with NoSchedule",
			`[{"key":"a","operator":"Exists","effect":"NoSchedule","tolerationSeconds":30}]`},
		{"tolerationSeconds with no effect",
			`[{"key":"a","operator":"Exists","tolerationSeconds":30}]`},
		// Lt and Gt compare numerically, so a non-numeric value is refused even
		// on a cluster that enables their feature gate. The four canonical-form
		// rows below are the ones ParseInt alone let through: measured against a
		// v1.36.1 server with TaintTolerationComparisonOperators ON — the only
		// cluster shape that can answer, since a gate-off server refuses every
		// Lt/Gt toleration before it ever looks at the value.
		{"Lt with a non-numeric value", `[{"key":"a","operator":"Lt","value":"abc"}]`},
		{"Lt with a leading zero", `[{"key":"a","operator":"Lt","value":"0100"}]`},
		{"Gt with a plus sign", `[{"key":"a","operator":"Gt","value":"+5"}]`},
		{"Lt with negative zero", `[{"key":"a","operator":"Lt","value":"-0"}]`},
		{"Gt with a negative leading zero", `[{"key":"a","operator":"Gt","value":"-01"}]`},
		// Canonical form is not enough on its own: a run of digits can be
		// canonical and still overflow the int64 the comparison parses into.
		{"Lt overflowing int64", `[{"key":"a","operator":"Lt","value":"99999999999999999999"}]`},
		{"Gt with an empty value", `[{"key":"a","operator":"Gt","value":""}]`},
		// Not an array. `null` decodes into a slice without error and would have
		// read as "no tolerations" — an unset variable by another spelling.
		{"json null", "null"},
		// dec.More() answers false on a stray closing bracket, so these two used
		// to pass as clean input.
		{"trailing bracket", "[]]"},
		{"trailing brace", "[]}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTolerations(tc.in)
			if err == nil {
				t.Fatalf("parseTolerations(%q) = %+v, want an error", tc.in, got)
			}
			if !strings.Contains(err.Error(), envTolerations) {
				t.Errorf("error %q does not name %s", err, envTolerations)
			}
		})
	}
}

// An empty array is a legitimate way to say "no tolerations" and must not be an
// error. The chart cannot produce it — its `with` skips an empty list, so the
// variable is simply absent — but a hand-written env var or another generator
// can, and `[]` differs from `null`, which IS refused: an array that happens to
// be empty says something, a shape that is not an array says nothing.
func TestParseTolerationsAcceptsAnEmptyArray(t *testing.T) {
	got, err := parseTolerations("[]")
	if err != nil {
		t.Fatalf("parseTolerations(\"[]\"): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parsed %+v, want none", got)
	}
}

// placedPod covers the wiring below New — parser to Provider fields to PodSpec —
// on a provider carrying exactly what New would have parsed into it. New itself
// needs a cluster for its client, so the rows that pin New's own behaviour are
// the rejection ones below, which return before it reaches for one.
func placedPod(t *testing.T, selector, tolerations string) *corev1.Pod {
	t.Helper()
	sel, err := parseNodeSelector(selector)
	if err != nil {
		t.Fatalf("parseNodeSelector(%q): %v", selector, err)
	}
	tol, err := parseTolerations(tolerations)
	if err != nil {
		t.Fatalf("parseTolerations(%q): %v", tolerations, err)
	}
	p := fakeProvider()
	p.nodeSelector, p.tolerations = sel, tol
	return hardenedPod(t, p, sandbox.Hardening{})
}

// The pair a dedicated sandbox node pool needs: the selector to reach the pool,
// the toleration to be admitted onto its taint. Without both, plan 20's tainted
// pool leaves every sandbox Pending — which is why this is a prerequisite for
// the staging environment rather than an enhancement.
func TestPodSpecCarriesNodeSelectorAndTolerations(t *testing.T) {
	pod := placedPod(t, "role=sandbox,pool=isolated",
		`[{"key":"sandbox","operator":"Equal","value":"true","effect":"NoSchedule"}]`)

	if got := pod.Spec.NodeSelector["role"]; got != "sandbox" {
		t.Errorf("nodeSelector[role] = %q, want sandbox (%v)", got, pod.Spec.NodeSelector)
	}
	if got := pod.Spec.NodeSelector["pool"]; got != "isolated" {
		t.Errorf("nodeSelector[pool] = %q, want isolated (%v)", got, pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 {
		t.Fatalf("tolerations = %+v, want exactly one", pod.Spec.Tolerations)
	}
	if tol := pod.Spec.Tolerations[0]; tol.Key != "sandbox" ||
		tol.Effect != corev1.TaintEffectNoSchedule || tol.Value != "true" {
		t.Errorf("toleration = %+v", tol)
	}
}

// Unconfigured, the pod must be exactly what it was before placement existed —
// the guard that this slice is additive. nil rather than an empty map matters:
// an empty NodeSelector serializes as `nodeSelector: {}` on the wire, a
// gratuitous difference in every existing deployment's pods.
func TestPodSpecWithoutPlacementIsUnchanged(t *testing.T) {
	pod := placedPod(t, "", "")
	if pod.Spec.NodeSelector != nil {
		t.Errorf("nodeSelector = %v, want nil", pod.Spec.NodeSelector)
	}
	if pod.Spec.Tolerations != nil {
		t.Errorf("tolerations = %+v, want nil", pod.Spec.Tolerations)
	}
}

// Placement is a property of the cluster, so it is applied whatever the session
// asked for: a gated (limited-networking) session's pod carries the same
// selector and tolerations as an unrestricted one. Getting this wrong would
// strand exactly the sessions a dedicated pool exists to isolate.
func TestPlacementAppliesToAGatedPodToo(t *testing.T) {
	sel, err := parseNodeSelector("role=sandbox")
	if err != nil {
		t.Fatalf("parseNodeSelector: %v", err)
	}
	p := fakeProvider()
	p.nodeSelector = sel
	pod := p.podSpec("map-sesn-gated", "/workspace", sandbox.Spec{
		SessionID:  domain.ID("sesn_gated"),
		Image:      "img",
		Networking: domain.Networking{Type: domain.NetLimited},
		Gate:       &sandbox.GateSpec{Image: "gate:1"},
	}, "tok")
	if got := pod.Spec.NodeSelector["role"]; got != "sandbox" {
		t.Errorf("a gated pod's nodeSelector[role] = %q, want sandbox", got)
	}
	if len(pod.Spec.InitContainers) == 0 {
		t.Fatal("the gated shape lost its sidecar; this row is no longer testing what it claims")
	}
}

// The claim the whole parser exists for: a malformed value stops the process
// that read it. New is where both binaries turn configuration into a provider,
// so this is the last point before a bad selector becomes a pod nobody can
// schedule — and it must hold without a cluster, since a deployment misconfigured
// this way should not depend on the API server answering to find out.
func TestNewRejectsMalformedPlacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"selector", Config{NodeSelector: "role"}, envNodeSelector},
		{"tolerations", Config{Tolerations: `[{"key":"x","efect":"NoSchedule"}]`}, envTolerations},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Kubeconfig points nowhere on purpose: the parse must have refused
			// the value before New ever tried to build a client, so the error
			// names the variable rather than the connection.
			tc.cfg.Kubeconfig = t.TempDir() + "/no-such-kubeconfig"
			p, err := New(tc.cfg)
			if err == nil {
				t.Fatalf("New(%+v) = %v, want an error", tc.cfg, p)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

// The chart and this parser have to agree on an encoding neither of them owns
// alone, and nothing else in the suite would notice them drifting: the chart
// renders a YAML map and list into these two strings, and only the executor
// reads them back. These are the literal values `helm template` produces from
// the sandboxPlacement block in the CI helm job's heredoc
// (.github/workflows/ci.yml), which is also what asserts the chart still renders
// them — re-derive with:
//
//	helm template t deploy/helm/managed-agent-platform \
//	  -f deploy/helm/managed-agent-platform/ci/example-values.yaml -f <placement.yaml> \
//	  --show-only templates/executor-deployment.yaml
//
// The CI helm job asserts the chart still renders them; this row asserts they
// still mean what the chart intended.
func TestTheChartsEncodingRoundTrips(t *testing.T) {
	const (
		chartSelector    = "cloud.google.com/gke-nodepool=sandbox,role=isolated"
		chartTolerations = `[{"effect":"NoSchedule","key":"sandbox","operator":"Equal","value":"true"},` +
			`{"effect":"NoExecute","key":"other","operator":"Exists","tolerationSeconds":30}]`
	)
	sel, err := parseNodeSelector(chartSelector)
	if err != nil {
		t.Fatalf("the chart's rendered node selector does not parse: %v", err)
	}
	if sel["cloud.google.com/gke-nodepool"] != "sandbox" || sel["role"] != "isolated" {
		t.Errorf("selector = %v", sel)
	}
	tol, err := parseTolerations(chartTolerations)
	if err != nil {
		t.Fatalf("the chart's rendered tolerations do not parse: %v", err)
	}
	if len(tol) != 2 {
		t.Fatalf("tolerations = %+v, want 2", tol)
	}
	// The field the encoding choice was made for: JSON keeps it a number, and a
	// pointer, so "30 seconds" does not arrive as "unset".
	if tol[1].TolerationSeconds == nil || *tol[1].TolerationSeconds != 30 {
		t.Errorf("tolerationSeconds survived the chart as %v, want 30", tol[1].TolerationSeconds)
	}
	if tol[1].Operator != corev1.TolerationOpExists || tol[1].Effect != corev1.TaintEffectNoExecute {
		t.Errorf("second toleration = %+v", tol[1])
	}
}

// The converse of the rejection rows: the shapes a real deployment writes must
// still pass. Lt/Gt are here because they are real fields of the pinned type —
// refusing them would break a cluster that enables their alpha feature gate,
// even though a default cluster rejects them, and that asymmetry is documented
// rather than legislated away.
func TestParseTolerationsAcceptsTheValidShapes(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"equal with an effect", `[{"key":"sandbox","operator":"Equal","value":"true","effect":"NoSchedule"}]`},
		{"exists, no value", `[{"key":"sandbox","operator":"Exists"}]`},
		{"wildcard key", `[{"operator":"Exists"}]`},
		{"default operator", `[{"key":"sandbox","value":"true"}]`},
		{"empty value with the default operator", `[{"key":"sandbox"}]`},
		{"tolerationSeconds with NoExecute",
			`[{"key":"a","operator":"Exists","effect":"NoExecute","tolerationSeconds":30}]`},
		{"numeric Lt", `[{"key":"a","operator":"Lt","value":"5"}]`},
		// Canonical form has exactly three accepted shapes, and a rule that
		// refused any of them would be as wrong as one that accepted "0100":
		// "0" alone, a positive with no leading zero, and a negative.
		{"Gt zero", `[{"key":"a","operator":"Gt","value":"0"}]`},
		{"Lt negative", `[{"key":"a","operator":"Lt","value":"-5"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTolerations(tc.in); err != nil {
				t.Errorf("parseTolerations(%q) = %v, want it accepted", tc.in, err)
			}
		})
	}
}
