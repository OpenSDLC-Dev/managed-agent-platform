package k8s

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Which registry credential a sandbox pod pulls with is a property of the
// deployment, not of a session — the same reasoning that put RuntimeClass and
// placement on the provider's Config rather than on the session's Spec (#199).
// The references land on the pod, not per-container, so the one knob covers
// whatever containers the pod's shape includes: the sandbox container, an
// ungated limited pod's net-setup init container, a gated pod's gate sidecar.
//
// Parsed here rather than passed through so a name no Secret can ever carry
// fails the executor's (or worker's) startup — #65's rule. The same boundary as
// placement applies: a well-formed name of a Secret that does not exist is NOT
// caught (only the cluster can answer that; every pull just fails with
// ImagePullBackOff), and neither is a Secret of the wrong type.
const envImagePullSecrets = "SANDBOX_K8S_IMAGE_PULL_SECRETS"

// parseImagePullSecrets reads comma-separated Secret names. Empty applies
// nothing. Whitespace around an entry is trimmed, for the same reason the
// selector parser trims it: a chart that joins a YAML list is one formatting
// choice away from producing it.
func parseImagePullSecrets(s string) ([]corev1.LocalObjectReference, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []corev1.LocalObjectReference
	seen := map[string]bool{}
	for _, entry := range strings.Split(s, ",") {
		name := strings.TrimSpace(entry)
		if name == "" {
			return nil, fmt.Errorf("%s=%q has an empty entry", envImagePullSecrets, s)
		}
		// A Secret name is a DNS-1123 subdomain — enforced when the Secret is
		// created, not when a pod references it, so a name outside the syntax
		// is one no Secret can ever carry: the reference could never resolve,
		// only fail every pull. The two habits this catches are uppercase and
		// underscores — both legal in the Docker config the credential came
		// from, neither in a Secret name.
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return nil, fmt.Errorf("%s=%q: %q is not a Secret name: %s",
				envImagePullSecrets, s, name, strings.Join(errs, "; "))
		}
		// A duplicate is harmless to the server but is almost certainly an
		// editing accident, and the string does not say which copy was meant.
		if seen[name] {
			return nil, fmt.Errorf("%s=%q: %q appears twice", envImagePullSecrets, s, name)
		}
		seen[name] = true
		out = append(out, corev1.LocalObjectReference{Name: name})
	}
	return out, nil
}
