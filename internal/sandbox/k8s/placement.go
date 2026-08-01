package k8s

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Where a sandbox pod may run is a property of the cluster, not of a session —
// the same reasoning that put RuntimeClass on the provider's Config rather than
// on the session's Spec. A deployment that keeps sandboxes on a tainted,
// dedicated node pool needs both halves: the selector to reach the pool, and the
// tolerations to be admitted onto it.
//
// Both are parsed here rather than passed through to the pod, because the
// failure mode of a bad value is the one an operator cannot debug: a selector
// that matches no node, or a toleration that tolerates nothing, produces a pod
// that sits Pending for the life of the session with nothing in the executor's
// log to explain it. Parsing in-process makes a malformed value fail the
// executor's (or worker's) startup instead — the same rule #65 set for the
// SANDBOX_* containment values.
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
	if dec.More() {
		return nil, fmt.Errorf("%s=%q has trailing content after the array", envTolerations, s)
	}
	for i, t := range out {
		if err := checkToleration(t); err != nil {
			return nil, fmt.Errorf("%s=%q: toleration %d: %w", envTolerations, s, i, err)
		}
	}
	return out, nil
}

// checkToleration applies the API server's own rules for a toleration, so a
// value that would be rejected at pod-create time — once per session, forever —
// is rejected once at startup instead.
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
	// An empty key means "match every taint key", which only means anything
	// paired with Exists; the default operator is Equal, so the bare form is a
	// toleration that matches nothing.
	if t.Key == "" && t.Operator != corev1.TolerationOpExists {
		return fmt.Errorf("an empty key needs operator Exists, not %q", t.Operator)
	}
	// Exists is a wildcard over values, so a value beside it is a contradiction
	// the operator should be told about rather than have silently ignored.
	if t.Operator == corev1.TolerationOpExists && t.Value != "" {
		return fmt.Errorf("operator Exists takes no value, got %q", t.Value)
	}
	return nil
}
