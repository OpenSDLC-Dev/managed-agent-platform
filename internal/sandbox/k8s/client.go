// Package k8s is the Kubernetes sandbox backend: one disposable Pod per session,
// driven over the Kubernetes API. The image must carry /bin/bash at that exact
// path (the plan's image contract) plus a userland with `setsid` (the deadline
// wrapper backgrounds the command in its own session), `tee` and `wc` (the
// write path counts the bytes the exec stream delivered), `mv` and `rm` (it lands
// the bytes under a temporary name and renames them into place), `tar` (a bulk
// write's archive is extracted inside the pod here, where the docker backend hands
// its own to the daemon and needs nothing — so an image without one loses skill
// materialization outright, #206), and a `stat` that
// accepts `-c`, GNU or BusyBox — required here rather than merely wanted as it is
// on docker, because a read's size gate exits on it. (`chmod` and that same `stat`
// also carry the target's mode onto the temporary file on a write, but that step
// degrades to a 0644 file rather than failing.) A shade beyond bare POSIX, but met
// by any mainstream base image. It is the self-hosted twin of the docker backend
// and passes the same sandboxtest contract suite.
package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// Config selects the cluster and namespace. Kubeconfig empty with Context empty
// tries in-cluster config first (the executor running as a Deployment), then the
// default loading rules (KUBECONFIG, ~/.kube/config) — the latter is what the
// contract test and local development use.
//
// NetSetupImage is the tiny utility image whose init container flushes a limited
// sandbox's routing table (it needs an `ip` command, which the sandbox image is
// not required to carry); empty defaults to busybox.
//
// RuntimeClass names the RuntimeClass sandbox pods run under — a hardened
// runtime such as gVisor's `runsc` or Kata. It lives here rather than on
// sandbox.Spec because which runtimes a cluster has is a property of the
// cluster, not of a session. Empty uses the cluster's default runtime; a name
// no RuntimeClass object defines makes the kubelet refuse the pod, which is the
// fail-closed direction.
//
// NodeSelector and Tolerations are where a sandbox pod may run, and are here for
// the same reason RuntimeClass is: which nodes a cluster has is a property of
// the cluster. They are carried as the raw strings the deployment configured, so
// the one place that knows their encoding is the one place that parses them
// (placement.go) — New rejects a malformed value rather than letting it become a
// pod that never schedules.
//
// ImagePullSecrets names the Secrets sandbox pods pull images with —
// comma-separated, raw here and parsed the same way (pullsecrets.go), for the
// same reason: which registry credential a cluster holds is a property of the
// deployment, not of a session.
type Config struct {
	Kubeconfig       string
	Context          string
	Namespace        string
	NetSetupImage    string
	RuntimeClass     string
	NodeSelector     string
	Tolerations      string
	ImagePullSecrets string
}

// restConfig resolves the cluster connection: in-cluster when running as a pod
// and nothing overrides it, otherwise the standard kubeconfig loading rules with
// an optional explicit path and context.
func restConfig(cfg Config) (*rest.Config, error) {
	if cfg.Kubeconfig == "" && cfg.Context == "" {
		if rc, err := rest.InClusterConfig(); err == nil {
			return rc, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Kubeconfig != "" {
		rules.ExplicitPath = cfg.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

// client wraps a clientset plus the rest.Config the SPDY executor needs, scoped
// to one namespace.
type client struct {
	cs        kubernetes.Interface
	rest      *rest.Config
	namespace string
}

func newClient(cfg Config) (*client, error) {
	rc, err := restConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: load config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("k8s: build clientset: %w", err)
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = "default"
	}
	return &client{cs: cs, rest: rc, namespace: ns}, nil
}

// streamResult is what one exec produced: the exit code and whether the command
// exited non-zero via a clean protocol exit (as opposed to a transport error).
type streamResult struct {
	code int
}

// exec runs argv in the pod's container, wiring the given streams. A nil stdout
// or stderr discards that stream; a non-nil stdin is sent. The command's exit
// code comes back as a utilexec.CodeExitError, which is a clean finish, not a
// failure — only a transport or API error is returned as err.
func (c *client) exec(ctx context.Context, pod, container string, argv []string, stdin io.Reader, stdout, stderr io.Writer) (streamResult, error) {
	req := c.cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(c.namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   argv,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(c.rest, "POST", req.URL())
	if err != nil {
		return streamResult{}, fmt.Errorf("k8s: build executor: %w", err)
	}
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
	if err == nil {
		return streamResult{code: 0}, nil
	}
	var codeErr utilexec.CodeExitError
	if errors.As(err, &codeErr) {
		return streamResult{code: codeErr.Code}, nil
	}
	return streamResult{}, err
}

// execOutput runs argv and returns its stdout, for the provider's own probes
// (liveness, stat) where the command is trusted and its output is small.
func (c *client) execOutput(ctx context.Context, pod, container string, argv []string) (string, int, error) {
	var out bytes.Buffer
	res, err := c.exec(ctx, pod, container, argv, nil, &out, io.Discard)
	if err != nil {
		return "", 0, err
	}
	return out.String(), res.code, nil
}
