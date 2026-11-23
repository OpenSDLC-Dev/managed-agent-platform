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
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

	executor, release, err := c.spdyExecutor(ctx, req.URL(), guard, boundStream)
	if err != nil {
		return streamResult{}, fmt.Errorf("k8s: build executor: %w", err)
	}
	defer release()
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
	err = guard.Cause(err)
	// client-go's spdy.Negotiate formats RoundTrip errors with %v rather than
	// %w, stripping context.Canceled / context.DeadlineExceeded from the chain
	// when the caller cancels during the HTTP 101 handshake. Restore the
	// caller's context sentinel when the stall guard did not trip.
	if !errors.Is(err, sandbox.ErrStalled) {
		if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
			return streamResult{}, fmt.Errorf("%w: %w", cerr, err)
		}
	}
	return streamResult{}, err
}

func (c *client) spdyExecutor(parentCtx context.Context, u *url.URL, guard *sandbox.StallGuard, boundStream bool) (remotecommand.Executor, func(), error) {
	tlsConfig, err := rest.TLSConfigFor(c.rest)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	rt := &guardedSPDY{
		inner:       inner,
		tlsConfig:   tlsConfig,
		proxier:     proxy,
		parentCtx:   parentCtx,
		guard:       guard,
		pingPeriod:  pingPeriod,
		boundStream: boundStream,
	}
	wrapper, err := rest.HTTPWrappersForConfig(c.rest, rt)
	if err != nil {
		return nil, nil, err
	}
	exec, err := remotecommand.NewSPDYExecutorForTransports(wrapper, rt, http.MethodPost, u)
	if err != nil {
		return nil, nil, err
	}
	return exec, rt.releasePending, nil
}

// guardedSPDY wraps Kubernetes SPDY upgrades to close both halves of #395:
//  1. Across the entire pre-101 handshake (HTTP proxy CONNECT if configured,
//     TLS handshake, upgrade request write, HTTP 101 response read, and any
//     non-101 error body), a handshakeConn wraps the underlying net.Conn with a
//     sliding byte-level silence deadline and a context.AfterFunc cancellation
//     trigger, refreshing both the socket deadline and StallGuard.Progress as
//     bytes arrive.
//  2. After 101 succeeds, NewConnection disarms handshakeConn (clearing the raw
//     deadline and detaching progress tracking from raw TCP reads) and wraps
//     the SPDY httpstream.Connection and each httpstream.Stream so real stdin/
//     stdout/stderr/error bytes reset the StallGuard while SPDY's 5s keepalive
//     Ping/Pong frames (handled below the stream layer) do not.
type guardedSPDY struct {
	inner       *httpstreamspdy.SpdyRoundTripper
	tlsConfig   *tls.Config
	proxier     func(*http.Request) (*url.URL, error)
	parentCtx   context.Context
	guard       *sandbox.StallGuard
	pingPeriod  time.Duration
	boundStream bool

	ctx     context.Context
	conn    net.Conn
	rawConn net.Conn
	hc      *handshakeConn
}

func (g *guardedSPDY) releasePending() {
	if g.hc != nil {
		_ = g.hc.disarm()
		g.hc = nil
	}
	if g.rawConn != nil {
		_ = g.rawConn.Close()
		g.rawConn = nil
		g.conn = nil
	}
}

func (g *guardedSPDY) RoundTrip(req *http.Request) (*http.Response, error) {
	// Release any un-upgraded connection if http.Client retried or followed a
	// redirect before NewConnection consumed it.
	g.releasePending()
	req = req.Clone(req.Context())
	req.Header = req.Header.Clone()
	req.Header.Add(httpstream.HeaderConnection, httpstream.HeaderUpgrade)
	req.Header.Add(httpstream.HeaderUpgrade, httpstreamspdy.HeaderSpdy31)

	conn, hc, err := g.dialUpgrade(req)
	if err != nil {
		return nil, g.classifyHandshakeErr(err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = hc.disarm()
		_ = conn.Close()
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
	g.hc = hc
	return resp, nil
}

func (g *guardedSPDY) dialUpgrade(req *http.Request) (net.Conn, *handshakeConn, error) {
	if err := req.Context().Err(); err != nil {
		return nil, nil, err
	}
	proxyURL, err := g.proxier(req)
	if err != nil {
		return nil, nil, err
	}
	if proxyURL != nil && proxyURL.Scheme == "socks5" {
		conn, err := g.inner.Dial(req)
		if err != nil {
			return nil, nil, err
		}
		hc, err := newHandshakeConn(req.Context(), conn, g.guard)
		if err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		return hc, hc, nil
	}

	var (
		conn net.Conn
		hc   *handshakeConn
	)
	if proxyURL != nil {
		switch proxyURL.Scheme {
		case "http", "https", "":
			conn, hc, err = g.dialHTTPProxy(req, proxyURL)
		default:
			return nil, nil, fmt.Errorf("proxy URL scheme not supported: %s", proxyURL.Scheme)
		}
	} else {
		conn, hc, err = g.dialTarget(req.Context(), req.URL)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := req.Write(conn); err != nil {
		_ = hc.disarm()
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, hc, nil
}

func (g *guardedSPDY) dialTarget(ctx context.Context, u *url.URL) (net.Conn, *handshakeConn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", canonicalAddr(u))
	if err != nil {
		return nil, nil, err
	}
	hc, err := newHandshakeConn(ctx, raw, g.guard)
	if err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	if u.Scheme == "https" {
		tc := tls.Client(hc, tlsConfigForHost(g.tlsConfig, u.Hostname()))
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = hc.disarm()
			_ = tc.Close()
			return nil, nil, err
		}
		return tc, hc, nil
	}
	return hc, hc, nil
}

// dialHTTPProxy opens a CONNECT tunnel through proxyURL with handshakeConn
// already armed on the proxy socket so a silent or half-open proxy cannot wedge
// past StallGuard or caller cancellation.
func (g *guardedSPDY) dialHTTPProxy(req *http.Request, proxyURL *url.URL) (net.Conn, *handshakeConn, error) {
	ctx := req.Context()
	proxyConn, hc, err := g.dialTarget(ctx, proxyURL)
	if err != nil {
		return nil, nil, err
	}
	targetHost := canonicalAddr(req.URL)
	connectReq := http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: targetHost},
		Host:   targetHost,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		connectReq.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
	if err := connectReq.Write(proxyConn); err != nil {
		_ = hc.disarm()
		_ = proxyConn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(proxyConn)
	resp, err := http.ReadResponse(br, &connectReq)
	if err != nil {
		_ = hc.disarm()
		_ = proxyConn.Close()
		return nil, nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = hc.disarm()
		_ = proxyConn.Close()
		return nil, nil, fmt.Errorf("CONNECT request to %s returned response: %s", proxyURL.Redacted(), resp.Status)
	}
	tunnelConn := proxyConn
	if n := br.Buffered(); n > 0 {
		peek, _ := br.Peek(n)
		tunnelConn = &prefixedConn{Conn: proxyConn, buf: append([]byte(nil), peek...)}
	}
	if req.URL.Scheme == "https" {
		tc := tls.Client(tunnelConn, tlsConfigForHost(g.tlsConfig, req.URL.Hostname()))
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = hc.disarm()
			_ = tc.Close()
			return nil, nil, err
		}
		return tc, hc, nil
	}
	return tunnelConn, hc, nil
}

func canonicalAddr(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := "80"
	if u.Scheme == "https" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func tlsConfigForHost(base *tls.Config, host string) *tls.Config {
	switch {
	case base == nil:
		return &tls.Config{ServerName: host}
	case len(base.ServerName) == 0:
		c := base.Clone()
		c.ServerName = host
		return c
	default:
		return base
	}
}

func (g *guardedSPDY) NewConnection(resp *http.Response) (httpstream.Connection, error) {
	connectionHeader := strings.ToLower(resp.Header.Get(httpstream.HeaderConnection))
	upgradeHeader := strings.ToLower(resp.Header.Get(httpstream.HeaderUpgrade))
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		!strings.Contains(connectionHeader, strings.ToLower(httpstream.HeaderUpgrade)) ||
		!strings.Contains(upgradeHeader, strings.ToLower(httpstreamspdy.HeaderSpdy31)) {
		defer func() {
			resp.Body.Close()
			g.releasePending()
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

	err := g.hc.disarm()
	g.hc = nil
	if err != nil {
		g.releasePending()
		return nil, err
	}
	if cerr := g.ctx.Err(); cerr != nil {
		g.releasePending()
		return nil, g.guard.Cause(cerr)
	}
	g.guard.Progress()

	conn := g.conn
	rawConn := g.rawConn
	g.conn = nil
	g.rawConn = nil
	sc, err := httpstreamspdy.NewClientConnectionWithPings(conn, g.pingPeriod)
	if err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	return &progressConnection{
		Connection:  sc,
		guard:       g.guard,
		boundStream: g.boundStream,
	}, nil
}

// handshakeConn bounds a raw net.Conn during the pre-101 upgrade phase with a
// sliding silence deadline and context.AfterFunc cancellation. Calling disarm
// clears the socket deadline and switches Read/Write to a lock-free pass-through
// so post-101 SPDY keepalive pings never count as progress.
type handshakeConn struct {
	net.Conn
	guard      *sandbox.StallGuard
	mu         sync.Mutex
	disarmed   atomic.Bool
	stopCancel func() bool
}

func newHandshakeConn(ctx context.Context, conn net.Conn, guard *sandbox.StallGuard) (*handshakeConn, error) {
	if err := conn.SetDeadline(time.Now().Add(guard.Budget())); err != nil {
		return nil, err
	}
	h := &handshakeConn{Conn: conn, guard: guard}
	stop := context.AfterFunc(ctx, func() {
		h.mu.Lock()
		if !h.disarmed.Load() {
			_ = h.Conn.SetDeadline(time.Now())
		}
		h.mu.Unlock()
	})
	h.mu.Lock()
	if h.disarmed.Load() {
		h.mu.Unlock()
		stop()
	} else {
		h.stopCancel = stop
		h.mu.Unlock()
	}
	return h, nil
}

func (h *handshakeConn) refresh() {
	if h.disarmed.Load() {
		return
	}
	h.mu.Lock()
	if !h.disarmed.Load() {
		h.guard.Progress()
		_ = h.Conn.SetDeadline(time.Now().Add(h.guard.Budget()))
	}
	h.mu.Unlock()
}

func (h *handshakeConn) Read(p []byte) (int, error) {
	n, err := h.Conn.Read(p)
	if n > 0 {
		h.refresh()
	}
	return n, err
}

func (h *handshakeConn) Write(p []byte) (int, error) {
	n, err := h.Conn.Write(p)
	if n > 0 {
		h.refresh()
	}
	return n, err
}

func (h *handshakeConn) disarm() error {
	h.mu.Lock()
	h.disarmed.Store(true)
	err := h.Conn.SetDeadline(time.Time{})
	stop := h.stopCancel
	h.stopCancel = nil
	h.mu.Unlock()
	if stop != nil {
		stop()
	}
	return err
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
