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
	"k8s.io/apimachinery/pkg/types"

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

// execErrProbeTimeout bounds the existence check execErr does to reclassify a
// vanished pod, so a diagnostic Get cannot hang a call whose context has no
// deadline of its own.
const execErrProbeTimeout = 10 * time.Second

// reclaimTimeout bounds the detached cleanup of a pod that never became ready,
// which runs off the caller's context so a cancelled caller still triggers it.
const reclaimTimeout = 15 * time.Second

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

	created, createErr := pods.Create(ctx, p.podSpec(name, workdir, spec), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(createErr) { // another executor created it first
		existing, gerr := pods.Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return nil, fmt.Errorf("k8s: get pod %s: %w", name, gerr)
		}
		if aerr := ours(existing, spec.SessionID); aerr != nil {
			return nil, aerr
		}
	} else if createErr != nil {
		return nil, fmt.Errorf("k8s: create pod %s: %w", name, createErr)
	}
	if err := p.waitReady(ctx, name); err != nil {
		if createErr == nil {
			// We created this pod and it never came up — a bad image wedges it in
			// ImagePullBackOff, and RestartPolicyNever never retries. Reclaim it so
			// a later attempt on this session starts clean instead of re-adopting
			// the wedged pod. reclaimUnready deletes only the exact pod we created
			// and only while it is still not ready, so it cannot delete a
			// same-named replacement or one another executor has since adopted.
			p.reclaimUnready(name, created.UID)
		}
		return nil, err
	}
	return p.attach(name, workdir), nil
}

// reclaimUnready deletes a pod this call created but could not bring to
// readiness, so a retry of the session is not poisoned by re-adopting a wedged
// pod. It guards three ways against deleting the wrong thing:
//   - its own short, detached context, so a cancelled caller still triggers the
//     cleanup rather than stranding the wedged pod;
//   - a re-Get that skips a name now holding a different pod (UID mismatch) or
//     one that has since become ready (another executor may be using it);
//   - a UID precondition on the delete, so an ABA between the Get and the Delete
//     still cannot remove a replacement.
func (p *Provider) reclaimUnready(name string, uid types.UID) {
	if uid == "" {
		return // no identity to guard the delete with; leave it for an operator
	}
	ctx, cancel := context.WithTimeout(context.Background(), reclaimTimeout)
	defer cancel()
	pods := p.client.cs.CoreV1().Pods(p.client.namespace)
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil || pod.UID != uid || podReady(pod) {
		return
	}
	zero := int64(0)
	_ = pods.Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
		Preconditions:      &metav1.Preconditions{UID: &uid},
	})
}

// podReady reports whether the sandbox container is running and ready.
func podReady(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName && cs.Ready {
			return true
		}
	}
	return false
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
	// The sandbox runs untrusted tool commands and never calls the Kubernetes
	// API, so it must not receive the namespace default ServiceAccount's token:
	// were that account granted any RBAC, the agent's commands would inherit it.
	// The provider drives the cluster with its own credentials, not the pod's.
	noAutomount := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{sessionLabel: string(spec.SessionID)},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &noAutomount,
			Containers: []corev1.Container{{
				Name:            containerName,
				Image:           spec.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				// Hold the pod open and guarantee the workdir exists. Nothing else
				// runs here: every tool call is its own exec. Unlike the docker
				// backend, which sets HostConfig.Init to reap orphaned tool
				// subprocesses, a pod has no runtime-level init to inject and the
				// arbitrary sandbox image cannot be assumed to bundle one; PID 1 is
				// this bash. Orphans it does not reap linger as zombies until the
				// pod is destroyed — cheap, but a divergence noted for a later fix.
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
//
// The flush is not assumed to have worked: an `ip` that silently no-ops, a CNI
// that keeps routes out of the main table, or a NET_ADMIN cap that is granted
// but ineffective would otherwise leave the default route intact while the init
// container still exited 0 (the old trailing `; true`), starting a "limited"
// sandbox with full egress. So the container re-reads the routing table it just
// flushed and exits non-zero if any IPv4 route survives — with RestartPolicy
// Never that fails the pod, and Provision surfaces the failure rather than
// handing the agent a route out. This is not equivalent to `NetworkMode: none`
// for every cluster: it does not remove the interface, so raw (AF_PACKET)
// sockets can still reach the segment, and it only inspects the main IPv4 table,
// so a policy-routing CNI or dual-stack IPv6 egress needs the reserved egress
// proxy for a complete cutoff. It closes the common, and previously silent,
// fail-open path.
func (p *Provider) netSetup() corev1.Container {
	const cutEgress = `
ip route flush table main 2>/dev/null
ip -6 route flush 2>/dev/null
routes=$(grep -c -v '^Iface' /proc/net/route)
if [ "$routes" != "0" ]; then
  echo "netsetup: $routes IPv4 route(s) survived the flush; refusing egress" >&2
  exit 1
fi
`
	return corev1.Container{
		Name:            "netsetup",
		Image:           p.netSetupImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"sh", "-c", cutEgress},
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
		if podReady(pod) {
			return nil
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

// pod is a handle to one session's sandbox, keyed by the derived pod name rather
// than an immutable UID. A stale handle whose pod was destroyed and whose name a
// later provision reused would therefore act on the new pod — a known limitation
// versus the docker backend's immutable container id. In v1 a handle is dropped
// at Destroy and never outlives its pod, so this stays theoretical; a UID
// precondition is the fix for when that no longer holds.
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
// for the default grace window. The finality is at the API-object level: the pod
// is gone from the API and its derived name is free to reuse the moment Destroy
// returns. On a healthy node the kubelet tears the container down promptly; a
// force delete does not block on the kubelet's confirmation, so a partitioned
// node could in principle keep the old processes a while longer — they are
// unreachable through this handle either way, and reaped when the node recovers.
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
// existence check before deciding. That check is bounded on its own timeout so a
// diagnostic Get cannot hang Exec/ReadFile/WriteFile when the caller's context
// carries no deadline: past the bound, the original error is what surfaces.
func (pd *pod) execErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		return pd.gone()
	}
	gctx, cancel := context.WithTimeout(ctx, execErrProbeTimeout)
	defer cancel()
	if _, gerr := pd.client.cs.CoreV1().Pods(pd.client.namespace).Get(gctx, pd.name, metav1.GetOptions{}); apierrors.IsNotFound(gerr) {
		return pd.gone()
	}
	return err
}

// Exec runs the command through the in-pod wrapper and enforces its deadline
// twice, only the second of which is a guarantee — mirroring the docker backend.
// The watchdog inside the pod does the killing, but it is a process the command
// can find and kill; Exec therefore watches the command's pid from outside and
// treats any command that outlived its deadline as timed out no matter what exit
// code it chose.
//
// Where it does not mirror docker is what says a punctual kill *was* the
// deadline's. Docker asks its daemon, out of band and cheaply; Kubernetes offers
// no such handle, so asking costs a second in-pod exec whose answer lands an
// apiserver round trip late — late enough, on a loaded cluster, to miss the kill
// it was sent to witness (#95, #110). So the watchdog marks its own firing and
// classifyTimeout weighs that mark; the probes stay for what a mark cannot cover.
//
// One axis is weaker than the docker backend. The state Exec reads — the pid it
// watches, and the watchdog's mark — lives in files inside the sandbox, since
// Kubernetes exposes no out-of-band handle on a running exec the way Docker's
// exec-inspect does. So a command that both kills its watchdog and overwrites the
// pid file to look dead can hide an overrun, and one that writes the mark and
// contrives a SIGKILL can claim a timeout it never hit. Both are deliberately
// malicious commands, the same case the derived-name adoption check (`ours`) does
// not defend, and the second only mislabels the tenant's own tool call. The
// honest runaway the deadline exists for forges nothing: its real pid stays in
// the file, and killing its watchdog alone buys only overrunSlop of unnoticed
// overrun before the probe catches it.
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
		// with the pod. This path skips readExit, so this exec's state files are
		// not cleaned — but only an overrunning command reaches it, and its pod is
		// being abandoned, so the residue dies with the pod rather than accruing.
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

	code, watchdogFired, err := pd.readExit(ctx, state)
	if err != nil {
		return sandbox.ExecResult{}, pd.execErr(ctx, err)
	}

	timedOut := classifyTimeout(code, watchdogFired, v)
	return sandbox.ExecResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ExitCode:  code,
		TimedOut:  timedOut,
		Truncated: stdout.truncated || stderr.truncated,
	}, nil
}

// classifyTimeout decides TimedOut from the three things Exec can know about a
// finished command, and nothing else — no clock, so what it decides is testable
// without one.
//
// Three ways a finished command can have hit its deadline, and a SIGKILL is the
// evidence for two of them, because a command cannot survive one to fake it.
// The watchdog reports having fired: it, and only it, knows the kill was the
// deadline's rather than the command's own or the node's. Or the command was
// still alive when Exec looked just before the deadline, which covers a SIGKILL
// the watchdog did not deliver (the tenant killed the watchdog, or the node
// did the killing) — the pre-deadline probe is a sampled guess, so it is read
// only as extra reach, never as a veto. Or it outlived the deadline and the
// slop and exited anyway, which on the honest path the watchdog would have
// prevented; that one needs no exit code, since none it chose can be believed.
//
// Every term only ever adds a timeout. That is what lets the watchdog's mark be
// in-pod state without weakening the guarantee: a tenant that forges the mark
// mislabels its own tool call, and one that erases it is back to the probes,
// which is exactly where this backend stood before.
func classifyTimeout(code int, watchdogFired bool, v verdict) bool {
	return (code == sigkillExit && (watchdogFired || v.aliveAtDeadline)) || v.overran
}

// readExit reads the line the wrapper recorded once the command finished, and
// reports the exit code and whether the watchdog marked itself the killer.
func (pd *pod) readExit(ctx context.Context, state string) (int, bool, error) {
	out, _, err := pd.client.execOutput(ctx, pd.name, containerName,
		[]string{"/bin/bash", "-c", exitScript, "map-exit", state})
	if err != nil {
		return 0, false, err
	}
	return parseExit(out)
}

// parseExit reads exitScript's output: the exit code, then killedMark if the
// watchdog fired. Nothing at all means the wrapper was killed before it could
// record anything (its own $PPID sabotage) — the command left no honest code, so
// it reads as the kill's, and with no wrapper left to vouch for the watchdog
// there is no mark to believe either.
func parseExit(out string) (int, bool, error) {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return sigkillExit, false, nil
	}
	code, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, false, fmt.Errorf("k8s: unparseable exit code %q: %w", fields[0], err)
	}
	return code, len(fields) > 1 && fields[1] == killedMark, nil
}

// nonce is a per-exec random token: the suffix for the wrapper's state files, so
// concurrent execs in one pod cannot collide, and the marker a read uses to frame
// its stdout.
func nonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ReadFile returns a file's bytes, distinguishing the reasons a read is not a
// plain success so the toolset can hand the model a recoverable error. It runs
// one probe-and-cat script: the exit code classifies the path, and on success
// stdout carries the raw bytes (binary included) followed by this call's marker.
func (pd *pod) ReadFile(ctx context.Context, path string) ([]byte, error) {
	marker := nonce()
	var out cappedBuffer
	// Room for a file at the cap and its marker, and not one byte more, so
	// out.truncated means exactly "the file was over the cap" — see readStdout.
	out.limit = sandbox.MaxFileBytes + len(marker)
	argv := []string{"/bin/bash", "-c", readScript, "map-read", path, strconv.FormatInt(sandbox.MaxFileBytes, 10), marker}
	res, err := pd.client.exec(ctx, pd.name, containerName, argv, nil, &out, io.Discard)
	if err != nil {
		return nil, pd.execErr(ctx, err)
	}
	switch res.code {
	case 0:
		return readStdout(path, marker, &out)
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

// readStdout turns what readScript sent into the file's bytes, and is the only
// place a short read can be caught. Nothing else in the path can notice one:
// client-go hands a failed stdout copy to runtime.HandleError and never to the
// caller, so a stream that ended early is byte-for-byte a shorter file (issue
// #105). The harm is why a hazard never seen firing is still worth a guard —
// `edit` reads a file and writes back what it read, so a truncation handed to the
// model is a truncation committed to disk.
//
// Size is decided first, and by out.truncated rather than by a length. The
// buffer's room is a capped file plus its marker exactly, so the flag is set
// precisely when the file held more than the cap — it grew between readScript's
// stat and its cat. Such a read loses its marker along with the excess, so this
// order is the whole of what decides which error the caller gets. A length
// comparison would not do the same job: the buffer stops at its limit, so it can
// never exceed it.
func readStdout(path, marker string, out *cappedBuffer) ([]byte, error) {
	b := out.Bytes()
	switch {
	case out.truncated:
		return nil, fmt.Errorf("%s: %w", path, sandbox.ErrFileTooLarge)
	case !bytes.HasSuffix(b, []byte(marker)):
		return nil, fmt.Errorf("k8s: read %s: short read (exec stdout ended before the pod finished sending)", path)
	default:
		n := len(b) - len(marker)
		// Clipped, so a caller that appends cannot write over the marker's bytes.
		return b[:n:n], nil
	}
}

// WriteFile writes data, creating parents and overwriting. The bytes go in over
// stdin so no shell round-trip touches them, and the script is told how many to
// expect so a stream that delivered fewer cannot pass as a success.
func (pd *pod) WriteFile(ctx context.Context, path string, data []byte) error {
	dir := gopath.Dir(path)
	argv := []string{"/bin/bash", "-c", writeScript, "map-write", path, dir, strconv.Itoa(len(data))}
	res, err := pd.client.exec(ctx, pd.name, containerName, argv, bytes.NewReader(data), io.Discard, io.Discard)
	if err != nil {
		return pd.execErr(ctx, err)
	}
	switch res.code {
	case 0:
		return nil
	case writeShort:
		// Fewer bytes reached the pod than we handed the stream, and every call in
		// the chain still finished cleanly — see writeScript. Nothing else in the
		// path can notice, so this is the only place a truncated write can be
		// turned into the error it is rather than a silent half-written file.
		return fmt.Errorf("k8s: write %s: short write (exec stdin did not deliver all %d bytes)", path, len(data))
	default:
		// The write failed in the pod — a directory where a file was meant to go, a
		// read-only path, a full disk. A clean exec that exited non-zero wrote
		// nothing; the docker backend surfaces the daemon's error here, so this
		// must not read as a successful write.
		return fmt.Errorf("k8s: write %s: exit %d", path, res.code)
	}
}

// shellQuote makes a path a single, literal shell word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

const (
	readNotExist   = 10
	readIsDir      = 11
	readNotRegular = 12
	readTooLarge   = 13
	writeShort     = 14
)

// readScript classifies $1, cats it on success, and marks the end of what it
// sent. $2 is the byte cap and $3 the marker. ($0 is the "map-read" label, so the
// real args start at $1 — bash -c's convention.)
//
// A symlink is rejected up front, as the docker backend rejects a non-regular
// tar entry: `stat -c %s` on a link reports the link's own tiny size while `cat`
// would follow it to a target of any size, so following one here would let a
// short link past the size gate and read its large target. The `-h` test is
// lstat, so it catches the link before the size and regular-file checks (which
// follow it) ever run.
//
// The marker is the read-side half of writeScript's guard (issue #105), and says
// the same thing from the other end: only the pod knows what it sent. The `sz`
// above cannot say it — that is the size of the file, not the length of the
// stream, and here the difference is not a fine point. A file rewritten between
// the `stat` and the `cat` would fail a read that returned exactly what the file
// then held, and every procfs entry reports a `stat` size of 0 while `cat`
// streams real content, so /proc/meminfo would stop being readable. The marker
// rides the same stream as the bytes, so it measures the delivery and nothing
// else.
//
// A marker rather than a byte count because every loss this transport can suffer
// is a suffix: client-go copies stdout with a single io.Copy, which stops at its
// first error, so the stream can end early but cannot arrive with a hole in it. A
// stream still ending in the marker therefore lost nothing in transit. That is
// read out of client-go's stream protocol, not instrumented — nobody induced a
// hole, and there is no way to. It says nothing about what else wrote to stdout:
// a shell profile that prints a banner ahead of the `cat` corrupts every read in
// this backend, marker or not, and always has.
//
// The marker rides in argv, so a process inside the pod can read it out of /proc
// and plant it. That is the deliberately-malicious-command case the derived-name
// adoption check (`ours`) and Exec's pid file do not defend either; against the
// transport failure this guards, a per-call crypto/rand token is what keeps a
// file's own bytes from ending in the marker by accident.
//
// `cat` is not exec'd, because the script has to outlive it to emit the marker;
// that is the whole reason, and not the one #103 had for dropping `exec` on the
// write side. `|| exit 1` collapses every `cat` failure onto a code that means
// nothing else: codes 10-14 are one flat namespace shared with writeScript, and
// the filesystem is agent-controlled, so a `cat` left to exit 13 on its own would
// be reported to the model as a file too large.
const readScript = `
f="$1"
if [ -h "$f" ]; then exit 12; fi
if [ ! -e "$f" ]; then exit 10; fi
if [ -d "$f" ]; then exit 11; fi
if [ ! -f "$f" ]; then exit 12; fi
sz=$(stat -c %s "$f") || exit 1
if [ "$sz" -gt "$2" ]; then exit 13; fi
cat "$f" || exit 1
printf %s "$3"
`

// writeScript makes $2 (the parent dir), writes stdin to $1, and checks that
// exactly $3 bytes landed.
//
// Both departures from the obvious `exec cat > "$1"` are deliberate, and they
// are the same fix seen from two sides (issue #103).
//
// `cat` is not exec'd. What is measured: with `exec`, a 1 MiB write arrived as
// one 32 KiB io.Copy buffer, every time; without it, intact. The mechanism we
// infer — `exec` points the *shell's* stdout at the file, closing the
// container's stdout pipe for the rest of the command, and the exec session
// then tears its stdin down early — was not instrumented at either end, so the
// length check below is what the guarantee actually rests on.
//
// The length check is what makes the guarantee independent of that reasoning.
// A short stdin stream is invisible everywhere else in the path: client-go
// hands a failed stdin copy to runtime.HandleError and never to the caller, the
// redirection has already truncated the file, and `cat` exits 0 — so a write
// that lost its tail is byte-for-byte indistinguishable from one that never had
// a tail. Only the pod can count what actually arrived, so it does.
//
// The count is taken from the stream, not from the file: `tee` passes the bytes
// through to `wc` on their way to disk. Re-reading the target instead would ask
// a different question — what the path holds now — and get it wrong wherever
// that is not what we just sent: a destination that keeps nothing (`/dev/null`
// and other device nodes), a file writable but not readable by the sandbox
// user, or a path another process in the same sandbox is also writing. Each of
// those is a successful write, and each would fail a re-stat. Counting the
// stream also measures exactly what went missing in the bug this guards.
//
// `set -o pipefail` so a `tee` that cannot open the target still fails the
// command substitution — otherwise the pipeline would report only `wc`'s
// status, and an unwritable path would come back as a short write instead of
// the write failure it is. `-eq` tolerates the leading padding BSD `wc` emits.
const writeScript = `
mkdir -p "$2" || exit 1
set -o pipefail
sz=$(tee "$1" | wc -c) || exit 1
[ "$sz" -eq "$3" ] || exit 14
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
