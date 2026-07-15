package k8s

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	gopath "path"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// sessionLabel tags every pod we own, so an operator (and Provision's adoption
// check) can tell this platform's sandboxes from anything else wearing the same
// derived name.
const sessionLabel = "dev.opensdlc.managed-agent-platform.session-id"

// containerName is the single container in every sandbox pod.
const containerName = "sandbox"

// readyTimeout bounds how long Provision waits for a freshly-created pod to
// schedule and start. A pull of a cold image is the slow case.
const readyTimeout = 2 * time.Minute

// defaultNetSetupImage carries an `ip` command for the limited-networking init
// container; the sandbox image is only guaranteed /bin/bash.
const defaultNetSetupImage = "busybox"

// Provider provisions per-session pods.
type Provider struct {
	client        *client
	netSetupImage string
}

func New(cfg Config) (*Provider, error) {
	cl, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	img := cfg.NetSetupImage
	if img == "" {
		img = defaultNetSetupImage
	}
	return &Provider{client: cl, netSetupImage: img}, nil
}

// podName derives a DNS-1123 pod name from the session id. Session ids carry one
// '_' (the "sesn_" prefix separator) which pod names disallow, so it becomes '-';
// the id is otherwise lowercase alphanumeric. The real id lives in a label for
// the adoption check, so this only has to be stable and collision-free.
func podName(sessionID domain.ID) string {
	return "map-" + strings.ReplaceAll(strings.ToLower(string(sessionID)), "_", "-")
}

// Provision returns the session's pod, creating and starting it only if none
// exists. Two executors racing on the same session converge: the loser of the
// create race adopts the winner's pod.
func (p *Provider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	if spec.SessionID.IsZero() {
		return nil, errors.New("k8s: provision needs a session id")
	}
	if spec.Image == "" {
		return nil, errors.New("k8s: provision needs an image")
	}
	workdir := spec.Workdir
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	name := podName(spec.SessionID)
	pods := p.client.cs.CoreV1().Pods(p.client.namespace)

	existing, err := pods.Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		if aerr := ours(existing, spec.SessionID); aerr != nil {
			return nil, aerr
		}
		if err := p.waitReady(ctx, name); err != nil {
			return nil, err
		}
		return p.attach(name, workdir), nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("k8s: get pod %s: %w", name, err)
	}

	_, err = pods.Create(ctx, p.podSpec(name, workdir, spec), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) { // another executor created it first
		existing, gerr := pods.Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return nil, fmt.Errorf("k8s: get pod %s: %w", name, gerr)
		}
		if aerr := ours(existing, spec.SessionID); aerr != nil {
			return nil, aerr
		}
	} else if err != nil {
		return nil, fmt.Errorf("k8s: create pod %s: %w", name, err)
	}
	if err := p.waitReady(ctx, name); err != nil {
		return nil, err
	}
	return p.attach(name, workdir), nil
}

// ours refuses a pod that merely wears this session's derived name — one left by
// an earlier deployment, or a collision. Adopting it would hand the agent's
// commands to a filesystem and an egress policy that are not this session's. As
// with the docker backend the label is not a trust boundary (anyone with API
// access can forge it), only a guard against the realistic single-tenant
// accident.
func ours(pod *corev1.Pod, sessionID domain.ID) error {
	if pod.Labels[sessionLabel] != string(sessionID) {
		return fmt.Errorf("k8s: pod %s is not this platform's sandbox for session %s", pod.Name, sessionID)
	}
	return nil
}

func (p *Provider) podSpec(name, workdir string, spec sandbox.Spec) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{sessionLabel: string(spec.SessionID)},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            containerName,
				Image:           spec.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				// Hold the pod open and guarantee the workdir exists. Nothing else
				// runs here: every tool call is its own exec.
				Command: []string{"/bin/bash", "-c",
					"mkdir -p " + shellQuote(workdir) + " && while :; do sleep 3600; done"},
				WorkingDir: workdir,
			}},
		},
	}
	if spec.Networking.Type == domain.NetLimited {
		pod.Spec.InitContainers = []corev1.Container{p.netSetup()}
	}
	return pod
}

// netSetup fails closed like the docker backend's `NetworkMode: none`: `limited`
// means "only AllowedHosts", which needs the egress proxy the plan reserves;
// until it exists, a limited sandbox gets no egress at all. An init container
// flushes the pod netns's routing table (the containers share it), so the
// sandbox is left with no route out — enforced before the sandbox container
// starts, and independent of a NetworkPolicy-capable CNI.
func (p *Provider) netSetup() corev1.Container {
	return corev1.Container{
		Name:            "netsetup",
		Image:           p.netSetupImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{"sh", "-c",
			"ip route flush table main; ip -6 route flush 2>/dev/null; true"},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
		},
	}
}

// waitReady blocks until the pod's container is running and ready, or the pod
// fails, or the deadline passes. A newly-created pod schedules and pulls
// asynchronously, so unlike the docker backend the sandbox is not usable the
// moment Provision's create call returns.
func (p *Provider) waitReady(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	pods := p.client.cs.CoreV1().Pods(p.client.namespace)
	for {
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("k8s: wait ready %s: %w", name, err)
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("k8s: pod %s is %s before it became ready", name, pod.Status.Phase)
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == containerName && cs.Ready {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("k8s: pod %s not ready: %w", name, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (p *Provider) attach(name, workdir string) *pod {
	return &pod{
		client: p.client, name: name, workdir: workdir,
		killGrace: defaultKillGrace, overrunSlop: defaultOverrunSlop, probeLead: defaultProbeLead,
	}
}

type pod struct {
	client      *client
	name        string
	workdir     string
	killGrace   time.Duration
	overrunSlop time.Duration
	probeLead   time.Duration
}

func (pd *pod) ID() string { return pd.name }

// Destroy deletes the pod. A NotFound is success: removal has one way to miss,
// and the pod's absence is the outcome asked for. It deletes with a zero grace
// period so the pod object is gone at once rather than lingering in Terminating
// for the default grace window — the contract is that the sandbox is final and
// unusable the moment Destroy returns.
func (pd *pod) Destroy(ctx context.Context) error {
	zero := int64(0)
	err := pd.client.cs.CoreV1().Pods(pd.client.namespace).Delete(ctx, pd.name,
		metav1.DeleteOptions{GracePeriodSeconds: &zero})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("k8s: delete pod %s: %w", pd.name, err)
	}
	return nil
}

// gone maps a vanished pod to the contract's sentinel.
func (pd *pod) gone() error { return fmt.Errorf("%s: %w", pd.name, sandbox.ErrNotFound) }

// execErr turns "the pod is gone" into ErrNotFound; every other failure keeps
// its own message. remotecommand wraps a missing-pod 404 in a
// connection-upgrade error that IsNotFound cannot see, so when the error is not
// already a structured NotFound this confirms the pod's absence with a cheap
// existence check before deciding.
func (pd *pod) execErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		return pd.gone()
	}
	if _, gerr := pd.client.cs.CoreV1().Pods(pd.client.namespace).Get(ctx, pd.name, metav1.GetOptions{}); apierrors.IsNotFound(gerr) {
		return pd.gone()
	}
	return err
}

// Exec runs the command through the in-pod wrapper and enforces its deadline
// twice, only the second of which is a guarantee — mirroring the docker backend.
// The watchdog inside the pod does the killing, but it is a process the command
// can find and kill; Exec therefore watches the command's pid from outside and
// treats any command that outlived its deadline as timed out no matter what exit
// code it chose. What a command buys by killing its watchdog is overrunSlop of
// unnoticed overrun.
//
// The command runs in a background goroutine because it may block: a straggler
// the command backgrounds inherits the exec's stdout and holds the stream open
// long after the command itself exited, so nothing may wait on the stream to
// learn the command finished. The wrapper records the exit code to a file
// instead — the Kubernetes analogue of docker's exec-inspect — which the stream
// close cannot delay.
func (pd *pod) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	seconds := 0
	if req.Timeout > 0 {
		seconds = int(math.Ceil(req.Timeout.Seconds()))
	}
	// The watchdog can only sleep whole seconds, so its deadline — not the
	// caller's unrounded request — is the one a kill has to have arrived after.
	deadline := time.Duration(seconds) * time.Second

	runCtx := ctx
	if seconds > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, deadline+pd.killGrace)
		defer cancel()
	}

	state := "/tmp/.map-exec-" + nonce()
	argv := []string{"/bin/bash", "-c", execWrapper, "map-exec", req.Command, strconv.Itoa(seconds), state}

	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = sandbox.MaxOutputBytes, sandbox.MaxOutputBytes

	// Start the clock before the request that starts the command, so the round
	// trip is charged to the sandbox and never shortens the command's measured
	// life. The command runs in the background; done carries its stream's fate.
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := pd.client.exec(runCtx, pd.name, containerName, argv, nil, &stdout, &stderr)
		done <- err
	}()

	probeCtx, stopProbing := context.WithCancel(runCtx)
	defer stopProbing()
	var probed <-chan verdict
	if seconds == 0 {
		none := make(chan verdict, 1)
		none <- verdict{}
		probed = none
	} else {
		// probeCtx times the sleeps and dies when the stream closes; runCtx
		// carries the overrun confirmation, which the stream close must not
		// cancel, and which runCtx's own Timeout+killGrace still bounds.
		probed = pd.probeDeadline(probeCtx, runCtx, state, deadline, start)
	}

	var streamErr error
	var abandoned bool
	select {
	case streamErr = <-done:
	case <-runCtx.Done():
		if ctx.Err() != nil {
			return sandbox.ExecResult{}, ctx.Err()
		}
		// Our own deadline, not the caller's. Stop waiting and take what came.
		abandoned = true
		streamErr = <-done
	}
	stopProbing()
	v := <-probed

	if abandoned && v.overran {
		// The command outlived the watchdog that should have killed it — it can
		// kill that watchdog — so the timeout is called here. Its exit code is
		// not ours to collect: it has not exited. Whatever is still running dies
		// with the pod.
		return sandbox.ExecResult{
			Stdout: stdout.String(), Stderr: stderr.String(),
			ExitCode: sigkillExit, TimedOut: true, Truncated: stdout.truncated || stderr.truncated,
		}, nil
	}
	// The stream closed on its own. A non-nil error that is not our own deadline
	// is a real failure (a vanished pod), not a finished command.
	if streamErr != nil && !abandoned {
		return sandbox.ExecResult{}, pd.execErr(ctx, streamErr)
	}

	code, err := pd.readExit(ctx, state)
	if err != nil {
		return sandbox.ExecResult{}, pd.execErr(ctx, err)
	}

	// Two ways a finished command can have hit its deadline. The watchdog killed
	// it (SIGKILL) and it was alive to receive it — a command cannot survive
	// SIGKILL to fake that, and one that killed itself before the pre-deadline
	// probe was already gone when we looked. Or it was still running after the
	// deadline and the slop and exited anyway, which on the honest path the
	// watchdog would have prevented.
	timedOut := (code == sigkillExit && v.aliveAtDeadline) || v.overran
	return sandbox.ExecResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ExitCode:  code,
		TimedOut:  timedOut,
		Truncated: stdout.truncated || stderr.truncated,
	}, nil
}

// readExit reads the exit code the wrapper recorded once the command finished.
// An absent or empty file means the wrapper was killed before it could record
// one (its own $PPID sabotage) — the command left no honest code, so it reads as
// the kill's.
func (pd *pod) readExit(ctx context.Context, state string) (int, error) {
	out, _, err := pd.client.execOutput(ctx, pd.name, containerName,
		[]string{"/bin/bash", "-c", exitScript, "map-exit", state})
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(out)
	if s == "" {
		return sigkillExit, nil
	}
	code, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("k8s: unparseable exit code %q: %w", s, err)
	}
	return code, nil
}

// nonce is a per-exec suffix for the wrapper's state files, so concurrent execs
// in one pod cannot collide.
func nonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ReadFile returns a file's bytes, distinguishing the reasons a read is not a
// plain success so the toolset can hand the model a recoverable error. It runs
// one probe-and-cat script: the exit code classifies the path, and on success
// stdout carries the raw bytes (binary included).
func (pd *pod) ReadFile(ctx context.Context, path string) ([]byte, error) {
	var out cappedBuffer
	out.limit = sandbox.MaxFileBytes + 1 // one past the cap so oversize is detectable
	argv := []string{"/bin/bash", "-c", readScript, "map-read", path, strconv.FormatInt(sandbox.MaxFileBytes, 10)}
	res, err := pd.client.exec(ctx, pd.name, containerName, argv, nil, &out, io.Discard)
	if err != nil {
		return nil, pd.execErr(ctx, err)
	}
	switch res.code {
	case 0:
		return out.Bytes(), nil
	case readNotExist:
		return nil, fmt.Errorf("%s: %w", path, sandbox.ErrFileNotExist)
	case readIsDir:
		return nil, fmt.Errorf("%s: %w", path, sandbox.ErrIsDirectory)
	case readNotRegular:
		return nil, fmt.Errorf("%s is not a regular file: %w", path, sandbox.ErrNotRegularFile)
	case readTooLarge:
		return nil, fmt.Errorf("%s: %w", path, sandbox.ErrFileTooLarge)
	default:
		return nil, fmt.Errorf("k8s: read %s: exit %d", path, res.code)
	}
}

// WriteFile writes data, creating parents and overwriting. The bytes go in over
// stdin so no shell round-trip touches them.
func (pd *pod) WriteFile(ctx context.Context, path string, data []byte) error {
	dir := gopath.Dir(path)
	argv := []string{"/bin/bash", "-c", writeScript, "map-write", path, dir}
	_, err := pd.client.exec(ctx, pd.name, containerName, argv, bytes.NewReader(data), io.Discard, io.Discard)
	if err != nil {
		return pd.execErr(ctx, err)
	}
	return nil
}

// shellQuote makes a path a single, literal shell word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

const (
	readNotExist   = 10
	readIsDir      = 11
	readNotRegular = 12
	readTooLarge   = 13
)

// readScript classifies $1 and cats it on success. $2 is the byte cap. ($0 is
// the "map-read" label, so the real args start at $1 — bash -c's convention.)
const readScript = `
f="$1"
if [ ! -e "$f" ]; then exit 10; fi
if [ -d "$f" ]; then exit 11; fi
if [ ! -f "$f" ]; then exit 12; fi
sz=$(stat -c %s "$f") || exit 1
if [ "$sz" -gt "$2" ]; then exit 13; fi
exec cat "$f"
`

// writeScript makes $2 (the parent dir) and writes stdin to $1.
const writeScript = `
mkdir -p "$2" || exit 1
exec cat > "$1"
`

// cappedBuffer keeps at most limit bytes and records whether more arrived. A
// command that writes past the cap still runs to completion — the tail is
// discarded, never blocked on a full pipe.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) <= room {
			return c.buf.Write(p)
		}
		_, _ = c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
func (c *cappedBuffer) Bytes() []byte  { return c.buf.Bytes() }
