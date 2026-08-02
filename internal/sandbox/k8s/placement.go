package k8s

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/validate/content"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Where a sandbox pod may run is a property of the cluster, not of a session —
// the same reasoning that put RuntimeClass on the provider's Config rather than
// on the session's Spec. A deployment that keeps sandboxes on a tainted,
// dedicated node pool needs both halves: the selector to reach the pool, and the
// tolerations to be admitted onto it.
//
// Both are parsed here rather than passed through to the pod so that a value the
// cluster would refuse fails the executor's (or worker's) startup — the same
// rule #65 set for the SANDBOX_* containment values. What that does and does not
// cover is worth stating exactly, because the boundary is easy to overclaim:
//
//   - Caught here: anything the API server itself would reject — an ill-formed
//     selector entry, a label key or value outside the syntax, a toleration the
//     pod-create validator refuses. Left to the pod, each of those fails every
//     Provision for the life of the deployment instead of once at startup.
//   - NOT caught here: a well-formed selector that happens to match no node in
//     this cluster. Nothing is refused, the pods simply stay Pending, and only
//     the cluster can answer whether a label exists — which the parse
//     deliberately runs too early to ask.
//
// One rule the parsers apply is *cluster-dependent* rather than universal, and
// is called out where it appears: the Lt and Gt toleration operators need the
// alpha TaintTolerationComparisonOperators feature gate, which is off by default
// (measured on v1.36). They are accepted because they are real fields of the
// pinned type, so refusing them would break a cluster that enables the gate.
const (
	envNodeSelector = "SANDBOX_K8S_NODE_SELECTOR"
	envTolerations  = "SANDBOX_K8S_TOLERATIONS"
)

// parseNodeSelector reads comma-separated key=value pairs — the form kubectl
// already uses for label selectors, which is why it was chosen over inventing
// one. Empty applies nothing.
//
// Whitespace around an entry and around either side of its `=` is trimmed: an
// operator writing "role=sandbox, pool=isolated" means the obvious thing, and a
// chart that joins a YAML map is one formatting choice away from producing it.
func parseNodeSelector(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, entry := range strings.Split(s, ",") {
		k, v, found := strings.Cut(entry, "=")
		if !found {
			return nil, fmt.Errorf("%s=%q: %q is not key=value", envNodeSelector, s, entry)
		}
		// A second `=` makes the split ambiguous rather than merely wrong, so it
		// is refused instead of being read as part of the value.
		if strings.Contains(v, "=") {
			return nil, fmt.Errorf("%s=%q: %q has more than one '='", envNodeSelector, s, entry)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		// The label rules are the API server's, and applying them here is what
		// turns a typo into a startup failure rather than a node the selector
		// can never match. A label *value* may be empty; a key may not.
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return nil, fmt.Errorf("%s=%q: %q is not a label key: %s",
				envNodeSelector, s, k, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return nil, fmt.Errorf("%s=%q: %q is not a label value: %s",
				envNodeSelector, s, v, strings.Join(errs, "; "))
		}
		// Last-wins would be silent, and the string does not say which one was
		// meant.
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%s=%q: %q appears twice", envNodeSelector, s, k)
		}
		out[k] = v
	}
	return out, nil
}

// parseTolerations reads a JSON array of the Kubernetes Toleration shape.
// JSON rather than a flat encoding because a toleration carries
// key/operator/value/effect *and* tolerationSeconds, and tolerationSeconds is a
// pointer — "absent" and "0" are different tolerations, which no flat dialect
// expresses without becoming a worse JSON. Empty applies nothing.
func parseTolerations(s string) ([]corev1.Toleration, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []corev1.Toleration
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	// Strict, because the field-name typo is the expensive one: a permissive
	// decoder drops `"efect"` and leaves a toleration whose effect is "match
	// everything", which admits the pod nowhere the operator intended and says
	// nothing.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%s=%q is not a JSON array of tolerations: %w", envTolerations, s, err)
	}
	// A JSON `null` decodes into a slice without error and leaves it nil, which
	// would read as "no tolerations" — a shape that is not an array quietly
	// meaning the same as an unset variable. Refused, so the only way to apply
	// nothing is to say nothing.
	if out == nil {
		return nil, fmt.Errorf("%s=%q is not a JSON array of tolerations", envTolerations, s)
	}
	// Not dec.More(): it answers false on a stray `]` or `}`, so "[]]" passed as
	// clean input. Decoding again must hit EOF and nothing else.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s=%q has trailing content after the array", envTolerations, s)
	}
	for i, t := range out {
		if err := checkToleration(t); err != nil {
			return nil, fmt.Errorf("%s=%q: toleration %d: %w", envTolerations, s, i, err)
		}
	}
	return out, nil
}

// checkToleration applies the pod-create validator's rules to one toleration, so
// a value the API server would refuse — once per session, forever — is refused
// once at startup instead. The rules are Kubernetes', not a convenient subset:
// they were derived by probing a live v1.36 API server, which rejects several
// shapes the vendored type's own comments describe as merely "ignored".
func checkToleration(t corev1.Toleration) error {
	switch t.Operator {
	case "", corev1.TolerationOpEqual, corev1.TolerationOpExists,
		corev1.TolerationOpLt, corev1.TolerationOpGt:
	default:
		return fmt.Errorf("operator %q is not Exists, Equal, Lt or Gt", t.Operator)
	}
	switch t.Effect {
	case "", corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule,
		corev1.TaintEffectNoExecute:
	default:
		return fmt.Errorf("effect %q is not NoSchedule, PreferNoSchedule or NoExecute", t.Effect)
	}
	// A key is a label name, and the server checks it. Not checking it here was
	// the gap that let `key: "my pool"` start an executor whose every pod create
	// then failed.
	if t.Key != "" {
		if errs := validation.IsQualifiedName(t.Key); len(errs) > 0 {
			return fmt.Errorf("key %q is not a label key: %s", t.Key, strings.Join(errs, "; "))
		}
	}
	// An empty key means "match every taint key", which only means anything
	// paired with Exists; the default operator is Equal, so the bare form is a
	// toleration that matches nothing.
	if t.Key == "" && t.Operator != corev1.TolerationOpExists {
		return fmt.Errorf("an empty key needs operator Exists, not %q", t.Operator)
	}
	switch t.Operator {
	case corev1.TolerationOpExists:
		// Exists is a wildcard over values, so a value beside it is a
		// contradiction the operator should be told about rather than have
		// silently ignored.
		if t.Value != "" {
			return fmt.Errorf("operator Exists takes no value, got %q", t.Value)
		}
	case corev1.TolerationOpLt, corev1.TolerationOpGt:
		// A numeric comparison against a non-number is refused even on a cluster
		// that enables the gate — and the server applies TWO rules here, not
		// one. ParseInt alone was the first implementation and accepted four
		// shapes a gate-enabled v1.36 server rejects ("0100", "+5", "-0",
		// "-01"): canonical form comes first, so a leading zero cannot be
		// mistaken for octal, and only then the int64 range check that refuses
		// an overflowing run of digits. Both are applied, in that order, for
		// the same reason and with the same error text as the server's.
		if errs := content.IsDecimalInteger(t.Value); len(errs) > 0 {
			return fmt.Errorf("operator %s compares numerically, so value %q %s",
				t.Operator, t.Value, strings.Join(errs, "; "))
		}
		if _, err := strconv.ParseInt(t.Value, 10, 64); err != nil {
			return fmt.Errorf("operator %s compares numerically, so value %q must fit in a 64-bit integer",
				t.Operator, t.Value)
		}
	default: // "" and Equal both compare the value as a label value.
		if errs := validation.IsValidLabelValue(t.Value); len(errs) > 0 {
			return fmt.Errorf("value %q is not a label value: %s", t.Value, strings.Join(errs, "; "))
		}
	}
	// The one the vendored type gets wrong: its comment says tolerationSeconds
	// "is ignored" outside NoExecute, and the API server rejects it instead
	// ("effect must be 'NoExecute' when tolerationSeconds is set"). Measured, not
	// read.
	if t.TolerationSeconds != nil && t.Effect != corev1.TaintEffectNoExecute {
		return fmt.Errorf("tolerationSeconds needs effect NoExecute, not %q", t.Effect)
	}
	return nil
}
