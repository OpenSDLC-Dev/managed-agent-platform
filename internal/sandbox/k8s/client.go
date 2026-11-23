// Package k8s is the Kubernetes sandbox backend: one disposable Pod per session,
// driven over the Kubernetes API. The image must carry /bin/bash at that exact
// path (the plan's image contract) plus a userland with `setsid` (the deadline
// wrapper backgrounds the command in its own session), `tee` and `wc` (the
// write path counts the bytes the exec stream delivered), `cat` (every read
// streams through one, and a write that refuses or fails with the body still
// unread drains it with one, #303/#304), `mv` and `rm` (it lands
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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun/uidguard"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
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
	// GateTokenRevoker, when non-nil, is called by Reap before it deletes a
	// session's pod, so a gate token never outlives its gate (#197). On the
	// provider — not a Spec — because Reap has no Spec: the reaper works from
	// a session id alone. The executor supplies its pool-backed
	// implementation; the BYOC worker has no database and leaves it nil.
	GateTokenRevoker sandbox.GateTokenRevoker
	// StallTimeout bounds how long a single Kubernetes API or SPDY exec call
	// may move zero bytes before failing with sandbox.ErrStalled (#395). Zero
	// or less takes sandbox.DefaultStallTimeout.
	StallTimeout time.Duration
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

// defaultSPDYPingPeriod matches k8s.io/client-go/transport/spdy.RoundTripperFor.
const defaultSPDYPingPeriod = 5 * time.Second

// client wraps a clientset plus the rest.Config the SPDY executor needs, scoped
// to one namespace.
type client struct {
	guarded      bool // only on clients bound to an admitted gated sandbox
	cs           kubernetes.Interface
	rest         *rest.Config
	namespace    string
	stallTimeout time.Duration
	// pingPeriod is SPDY's keepalive interval; zero takes defaultSPDYPingPeriod.
	// Overridable in tests so SPDY pings can fire faster than a short stall
	// budget and prove they never reset the guard.
	pingPeriod time.Duration
}

// forSandbox shares the connection and clientset. Its exec path also covers
// liveness probes, file operations and checkpoint exports.
func (c *client) forSandbox(gated bool) *client {
	if !gated {
		return c
	}
	bound := *c
	bound.guarded = true
	return &bound
}

func newClient(cfg Config) (*client, error) {
	rc, err := restConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: load config: %w", err)
	}
	// Keep the SPDY rest.Config free of the REST stall wrapper: SPDY builds its
	// own connection-level and stream-level stall guard per exec call below, and
	// rest.HTTPWrappersForConfig would otherwise double-wrap it.
	spdyRC := rest.CopyConfig(rc)
	rc.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return sandbox.GuardRoundTripper(rt, cfg.StallTimeout)
	})
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("k8s: build clientset: %w", err)
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = "default"
	}
	return &client{
		cs:           cs,
		rest:         spdyRC,
		namespace:    ns,
		stallTimeout: cfg.StallTimeout,
		pingPeriod:   defaultSPDYPingPeriod,
	}, nil
}

// streamResult is what one exec produced: the exit code and whether the command
// exited non-zero via a clean protocol exit (as opposed to a transport error).
type streamResult struct {
	code int
}

// exec runs argv in the pod's container, wiring the given streams, with the
// silence guard active end-to-end (handshake + streams). A nil stdout or stderr
// discards that stream; a non-nil stdin is sent. The command's exit code comes
// back as a utilexec.CodeExitError, which is a clean finish, not a failure —
// only a transport or API error is returned as err.
func (c *client) exec(ctx context.Context, pod, container string, argv []string, stdin io.Reader, stdout, stderr io.Writer) (streamResult, error) {
	return c.execStream(ctx, pod, container, argv, stdin, stdout, stderr, true)
}

// execStream runs argv in the pod's container. The HTTP 101 SPDY upgrade and
// stream setup are always bounded by the client's silence guard (at the raw
// net.Conn level during 101 so a silent peer is cancellable and bounded, #395).
// When boundStream is true (untimed Exec, file primitives, probes, Export), the
// guard stays active across all SPDY data streams; when false (timed Exec), the
// guard disarms once streams open because the command may legitimately sit
// silent up to ExecRequest.Timeout.
func (c *client) execStream(ctx context.Context, pod, container string, argv []string, stdin io.Reader, stdout, stderr io.Writer, boundStream bool) (streamResult, error) {
	if c.guarded {
		argv = uidguard.Command(argv)
	}
	req := c.cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(c.namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   argv,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
		}, scheme.ParameterCodec)

	gctx, guard := sandbox.NewStallGuard(ctx, c.stallTimeout)
	defer guard.Stop()

	executor, err := c.spdyExecutor(ctx, req.URL(), guard, boundStream)
	if err != nil {
		return streamResult{}, fmt.Errorf("k8s: build executor: %w", err)
	}
	err = executor.StreamWithContext(gctx, remotecommand.StreamOptions{
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
	if err == nil {
		return streamResult{code: 0}, nil
	}
	var codeErr utilexec.CodeExitError
	if errors.As(err, &codeErr) {
		return streamResult{code: codeErr.Code}, nil
	}
	return streamResult{}, guard.Cause(err)
}

func (c *client) spdyExecutor(parentCtx context.Context, u *url.URL, guard *sandbox.StallGuard, boundStream bool) (remotecommand.Executor, error) {
	tlsConfig, err := rest.TLSConfigFor(c.rest)
	if err != nil {
		return nil, err
	}
	proxy := http.ProxyFromEnvironment
	if c.rest.Proxy != nil {
		proxy = c.rest.Proxy
	}
	pingPeriod := c.pingPeriod
	if pingPeriod == 0 {
		pingPeriod = defaultSPDYPingPeriod
	}
	inner, err := httpstreamspdy.NewRoundTripperWithConfig(httpstreamspdy.RoundTripperConfig{
		TLS:        tlsConfig,
		Proxier:    proxy,
		PingPeriod: pingPeriod,
	})
	if err != nil {
		return nil, err
	}
	rt := &guardedSPDY{
		inner:       inner,
		parentCtx:   parentCtx,
		guard:       guard,
		pingPeriod:  pingPeriod,
		boundStream: boundStream,
	}
	wrapper, err := rest.HTTPWrappersForConfig(c.rest, rt)
	if err != nil {
		return nil, err
	}
	return remotecommand.NewSPDYExecutorForTransports(wrapper, rt, http.MethodPost, u)
}

// guardedSPDY wraps client-go's SPDY upgrade round-tripper to close both halves
// of #395:
//  1. During the HTTP 101 handshake, upstream SpdyRoundTripper.RoundTrip reads
//     the response off a raw net.Conn with no deadline and no context wiring.
//     RoundTrip here sets a real net.Conn deadline and wires context.AfterFunc
//     so both the stall budget and caller cancellation unblock http.ReadResponse.
//  2. After 101 succeeds, NewConnection clears the raw deadline and wraps the
//     SPDY httpstream.Connection and each httpstream.Stream so real stdin/
//     stdout/stderr/error bytes reset the StallGuard while SPDY's 5s keepalive
//     Ping/Pong frames (handled below the stream layer) do not.
type guardedSPDY struct {
	inner       *httpstreamspdy.SpdyRoundTripper
	parentCtx   context.Context
	guard       *sandbox.StallGuard
	pingPeriod  time.Duration
	boundStream bool

	ctx        context.Context
	conn       net.Conn
	rawConn    net.Conn
	deadlineMu *sync.Mutex
	upgraded   *bool
	stopCancel func() bool
}

func (g *guardedSPDY) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header = req.Header.Clone()
	req.Header.Add(httpstream.HeaderConnection, httpstream.HeaderUpgrade)
	req.Header.Add(httpstream.HeaderUpgrade, httpstreamspdy.HeaderSpdy31)

	conn, err := g.inner.Dial(req)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(g.guard.Budget())); err != nil {
		conn.Close()
		return nil, err
	}
	var mu sync.Mutex
	upgraded := false
	stopCancel := context.AfterFunc(req.Context(), func() {
		mu.Lock()
		if !upgraded {
			_ = conn.SetDeadline(time.Now())
		}
		mu.Unlock()
	})

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		stopCancel()
		conn.Close()
		return nil, g.classifyHandshakeErr(err)
	}
	var upgradeConn net.Conn = conn
	if n := br.Buffered(); n > 0 {
		peek, _ := br.Peek(n)
		upgradeConn = &prefixedConn{Conn: conn, buf: append([]byte(nil), peek...)}
	}
	g.ctx = req.Context()
	g.conn = upgradeConn
	g.rawConn = conn
	g.deadlineMu = &mu
	g.upgraded = &upgraded
	g.stopCancel = stopCancel
	return resp, nil
}

func (g *guardedSPDY) NewConnection(resp *http.Response) (httpstream.Connection, error) {
	connectionHeader := strings.ToLower(resp.Header.Get(httpstream.HeaderConnection))
	upgradeHeader := strings.ToLower(resp.Header.Get(httpstream.HeaderUpgrade))
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		!strings.Contains(connectionHeader, strings.ToLower(httpstream.HeaderUpgrade)) ||
		!strings.Contains(upgradeHeader, strings.ToLower(httpstreamspdy.HeaderSpdy31)) {
		defer func() {
			resp.Body.Close()
			g.stopCancel()
			g.rawConn.Close()
		}()
		responseErrorBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			err = g.classifyHandshakeErr(err)
			if errors.Is(err, sandbox.ErrStalled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, fmt.Errorf("unable to upgrade connection: unable to read error from server response: %w", err)
		}
		return nil, fmt.Errorf("unable to upgrade connection: %s", upgradeErrorMessage(responseErrorBytes))
	}

	g.deadlineMu.Lock()
	*g.upgraded = true
	err := g.rawConn.SetDeadline(time.Time{})
	g.deadlineMu.Unlock()
	g.stopCancel()
	if err != nil {
		g.rawConn.Close()
		return nil, err
	}
	if cerr := g.ctx.Err(); cerr != nil {
		g.rawConn.Close()
		return nil, g.guard.Cause(cerr)
	}
	g.guard.Progress()

	sc, err := httpstreamspdy.NewClientConnectionWithPings(g.conn, g.pingPeriod)
	if err != nil {
		return nil, err
	}
	return &progressConnection{
		Connection:  sc,
		guard:       g.guard,
		boundStream: g.boundStream,
	}, nil
}

// classifyHandshakeErr separates a raw net.Conn deadline firing from caller
// cancellation. If the socket deadline expired while the caller's context is
// still live (even a scheduling hop before StallGuard's own timer), trip the
// guard so execStream reports sandbox.ErrStalled; if the caller cancelled first,
// report the caller's cancellation rather than a misleading i/o timeout.
func (g *guardedSPDY) classifyHandshakeErr(err error) error {
	if cerr := g.parentCtx.Err(); cerr != nil {
		return cerr
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		g.guard.Trip()
		return g.guard.Cause(err)
	}
	return g.guard.Cause(err)
}

func upgradeErrorMessage(body []byte) string {
	var status struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &status) == nil {
		if msg := strings.TrimSpace(status.Message); msg != "" {
			return msg
		}
		if msg := strings.TrimSpace(status.Error); msg != "" {
			return msg
		}
	}
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return msg
	}
	return "empty server response"
}

// prefixedConn replays bytes bufio.NewReader already read past the HTTP 101
// headers before delegating to the underlying net.Conn.
type prefixedConn struct {
	net.Conn
	buf []byte
}

func (c *prefixedConn) Read(p []byte) (int, error) {
	if len(c.buf) > 0 {
		n := copy(p, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

type progressConnection struct {
	httpstream.Connection
	guard       *sandbox.StallGuard
	boundStream bool
}

func (c *progressConnection) CreateStream(headers http.Header) (httpstream.Stream, error) {
	s, err := c.Connection.CreateStream(headers)
	if err != nil {
		return nil, err
	}
	c.guard.Progress()
	if !c.boundStream {
		// remotecommand opens streams in fixed order: error, (stdin), stdout,
		// stderr. Timed pod.Exec always passes non-nil stdout and stderr, so
		// stderr is the final stream before command execution begins.
		if headers.Get(corev1.StreamType) == corev1.StreamTypeStderr {
			if !c.guard.Disarm() {
				return nil, c.guard.Cause(nil)
			}
		}
		return s, nil
	}
	return &progressStream{Stream: s, guard: c.guard}, nil
}

type progressStream struct {
	httpstream.Stream
	guard *sandbox.StallGuard
}

func (s *progressStream) Read(p []byte) (int, error) {
	n, err := s.Stream.Read(p)
	if n > 0 {
		s.guard.Progress()
	}
	return n, err
}

func (s *progressStream) Write(p []byte) (int, error) {
	n, err := s.Stream.Write(p)
	if n > 0 {
		s.guard.Progress()
	}
	return n, err
}

// execOutput runs argv and returns its stdout, for the provider's own probes
// (liveness, the export probe), whose own output is small:
// kept up to the cap Exec keeps of a stream, since an image's startup prints
// to it too, with truncated saying whether the cap cut it. Each probe frames
// its script (sandbox.Frame) and reads its answer from between the lines.
func (c *client) execOutput(ctx context.Context, pod, container string, argv []string) (out string, truncated bool, code int, err error) {
	buf := cappedBuffer{limit: sandbox.MaxOutputBytes}
	res, err := c.exec(ctx, pod, container, argv, nil, &buf, io.Discard)
	if err != nil {
		return "", false, 0, err
	}
	return buf.String(), buf.truncated, res.code, nil
}
