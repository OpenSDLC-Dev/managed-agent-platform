package k8s

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

// spdyStreams groups the SPDY streams a fake kubelet exec endpoint received.
type spdyStreams struct {
	errStream httpstream.Stream
	stdin     httpstream.Stream
	stdout    httpstream.Stream
	stderr    httpstream.Stream
	argv      []string
}

func (s *spdyStreams) finish(t *testing.T, exitCode int) {
	t.Helper()
	if s.stdout != nil {
		_ = s.stdout.Close()
	}
	if s.stderr != nil {
		_ = s.stderr.Close()
	}
	if s.errStream != nil {
		status := metav1.Status{Status: metav1.StatusSuccess}
		if exitCode != 0 {
			status = metav1.Status{
				Status: metav1.StatusFailure,
				Reason: "NonZeroExitCode",
				Details: &metav1.StatusDetails{
					Causes: []metav1.StatusCause{{Type: "ExitCode", Message: "1"}},
				},
			}
		}
		b, _ := json.Marshal(status)
		_, _ = s.errStream.Write(b)
		_ = s.errStream.Close()
	}
}

// startFakeKubeletSPDY serves a real SPDY/3.1 upgrade endpoint over loopback
// HTTP so the real client-go remotecommand + guardedSPDY + spdystream stack is
// exercised end-to-end without needing a cluster.
func startFakeKubeletSPDY(t *testing.T, stall, pingPeriod time.Duration, handler func(w http.ResponseWriter, r *http.Request)) (*Provider, *pod) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	rc := &rest.Config{Host: srv.URL}
	spdyRC := rest.CopyConfig(rc)
	rc.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return sandbox.GuardRoundTripper(rt, stall)
	})
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatalf("NewForConfig: %v", err)
	}
	cl := &client{
		cs:           cs,
		rest:         spdyRC,
		namespace:    "default",
		stallTimeout: stall,
		pingPeriod:   pingPeriod,
	}
	p := &Provider{client: cl, netSetupImage: defaultNetSetupImage}
	return p, p.attach("pod-stall", "/workspace", false)
}

// acceptSPDYExec upgrades req to SPDY/3.1, collects the client's streams
// (error, optional stdin, optional stdout, optional stderr), and calls fn once
// all expected streams have opened.
func acceptSPDYExec(t *testing.T, w http.ResponseWriter, r *http.Request, pingPeriod time.Duration, fn func(s *spdyStreams, conn httpstream.Connection)) {
	t.Helper()
	wantStdin := r.URL.Query().Get("stdin") == "true"
	wantStdout := r.URL.Query().Get("stdout") == "true"
	wantStderr := r.URL.Query().Get("stderr") == "true"
	wantCount := 1 // error stream is always opened
	if wantStdin {
		wantCount++
	}
	if wantStdout {
		wantCount++
	}
	if wantStderr {
		wantCount++
	}

	var mu sync.Mutex
	st := &spdyStreams{argv: r.URL.Query()["command"]}
	opened := 0
	ready := make(chan struct{})

	w.Header().Set(httpstream.HeaderProtocolVersion, "v4.channel.k8s.io")
	upgrader := httpstreamspdy.NewResponseUpgraderWithPings(pingPeriod)
	conn := upgrader.UpgradeResponse(w, r, func(stream httpstream.Stream, replySent <-chan struct{}) error {
		mu.Lock()
		switch stream.Headers().Get(corev1.StreamType) {
		case corev1.StreamTypeError:
			st.errStream = stream
		case corev1.StreamTypeStdin:
			st.stdin = stream
		case corev1.StreamTypeStdout:
			st.stdout = stream
		case corev1.StreamTypeStderr:
			st.stderr = stream
		}
		opened++
		if opened == wantCount {
			close(ready)
		}
		mu.Unlock()
		return nil
	})
	if conn == nil {
		return
	}
	defer conn.Close()
	select {
	case <-ready:
		fn(st, conn)
	case <-conn.CloseChan():
	}
}

func TestK8sTransportStallContract(t *testing.T) {
	sandboxtest.RunTransportStall(t, sandboxtest.TransportStallBackend{
		WedgedHandshake: func(t *testing.T, stall time.Duration) sandbox.Sandbox {
			// Accept the TCP connection and HTTP upgrade request, then send zero
			// bytes (no HTTP 101 Switching Protocols) — the exact raw net.Conn
			// http.ReadResponse hazard in newConnectionAndStream (#395).
			unblock := make(chan struct{})
			_, pd := startFakeKubeletSPDY(t, stall, 30*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("ResponseWriter does not implement http.Hijacker")
					return
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Errorf("Hijack: %v", err)
					return
				}
				defer conn.Close()
				<-unblock
			})
			t.Cleanup(func() { close(unblock) })
			return pd
		},

		WedgedExecStream: func(t *testing.T, stall time.Duration) sandbox.Sandbox {
			// Upgrade to SPDY with 30ms keepalive pings (8× faster than the
			// 250ms stall budget), send one stdout chunk, and then park every
			// stream open forever while pings keep answering.
			unblock := make(chan struct{})
			_, pd := startFakeKubeletSPDY(t, stall, 30*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
				acceptSPDYExec(t, w, r, 30*time.Millisecond, func(s *spdyStreams, conn httpstream.Connection) {
					_, _ = s.stdout.Write([]byte("hello\n"))
					select {
					case <-unblock:
					case <-conn.CloseChan():
					}
				})
			})
			t.Cleanup(func() { close(unblock) })
			return pd
		},

		WedgedReadFile: func(t *testing.T, stall time.Duration) sandbox.Sandbox {
			unblock := make(chan struct{})
			_, pd := startFakeKubeletSPDY(t, stall, 30*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
				acceptSPDYExec(t, w, r, 30*time.Millisecond, func(s *spdyStreams, conn httpstream.Connection) {
					_, _ = s.stdout.Write([]byte("partial-read-bytes"))
					select {
					case <-unblock:
					case <-conn.CloseChan():
					}
				})
			})
			t.Cleanup(func() { close(unblock) })
			return pd
		},

		WedgedWriteFile: func(t *testing.T, stall time.Duration) sandbox.Sandbox {
			// The exact #318 failure: consume all stdin bytes to EOF, then sit
			// idle with stdout/stderr/error open while SPDY pings keep beating.
			unblock := make(chan struct{})
			_, pd := startFakeKubeletSPDY(t, stall, 30*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
				acceptSPDYExec(t, w, r, 30*time.Millisecond, func(s *spdyStreams, conn httpstream.Connection) {
					if s.stdin != nil {
						_, _ = io.Copy(io.Discard, s.stdin)
					}
					select {
					case <-unblock:
					case <-conn.CloseChan():
					}
				})
			})
			t.Cleanup(func() { close(unblock) })
			return pd
		},
	})
}

func TestK8sStallGuardExtraPaths(t *testing.T) {
	const budget = 200 * time.Millisecond

	t.Run("wedged REST Pods API call fails with ErrStalled", func(t *testing.T) {
		unblock := make(chan struct{})
		p, _ := startFakeKubeletSPDY(t, budget, 30*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
			<-unblock
		})
		t.Cleanup(func() { close(unblock) })
		_, err := p.Owned(context.Background())
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged Owned err = %v, want sandbox.ErrStalled", err)
		}
	})

	t.Run("wedged SPDY CreateStream after 101 fails within budget", func(t *testing.T) {
		// Server completes HTTP 101 Switching Protocols and then never replies
		// to SPDY SYN_STREAM frames (upstream createStreamResponseTimeout is
		// 30s; our guard must end it within the 200ms budget).
		unblock := make(chan struct{})
		_, pd := startFakeKubeletSPDY(t, budget, 30*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("ResponseWriter does not implement http.Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: SPDY/3.1\r\nX-Stream-Protocol-Version: v4.channel.k8s.io\r\n\r\n")
			<-unblock
		})
		t.Cleanup(func() { close(unblock) })
		start := time.Now()
		_, err := pd.Exec(context.Background(), sandbox.ExecRequest{Command: "echo hi", Timeout: 30 * time.Second})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged CreateStream err = %v, want sandbox.ErrStalled", err)
		}
		if elapsed := time.Since(start); elapsed < budget || elapsed > 5*time.Second {
			t.Errorf("elapsed = %s, want within [%s, 5s]", elapsed, budget)
		}
	})

	t.Run("wedged non-101 upgrade error body fails with ErrStalled", func(t *testing.T) {
		unblock := make(chan struct{})
		_, pd := startFakeKubeletSPDY(t, budget, 30*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 100\r\n\r\npartial")
			<-unblock
		})
		t.Cleanup(func() { close(unblock) })
		_, err := pd.Exec(context.Background(), sandbox.ExecRequest{Command: "echo hi"})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged non-101 body err = %v, want sandbox.ErrStalled", err)
		}
	})

	t.Run("streaming ReadFile outlives budget while stdout moves", func(t *testing.T) {
		const chunks = 8
		payload := bytes.Repeat([]byte("k"), chunks*1024)
		_, pd := startFakeKubeletSPDY(t, budget, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
			acceptSPDYExec(t, w, r, 5*time.Second, func(s *spdyStreams, conn httpstream.Connection) {
				// Extract the frame lines from the bash -c script argument.
				_, f, ok := sandboxtest.Unwrap(s.argv[2])
				if !ok {
					t.Errorf("read script missing frame: %q", s.argv[2])
					s.finish(t, 1)
					return
				}
				begin, end := f.Lines()
				_, _ = s.stdout.Write([]byte(begin))
				for i := 0; i < chunks; i++ {
					_, _ = s.stdout.Write(payload[i*1024 : (i+1)*1024])
					timer := time.NewTimer(budget / 3)
					select {
					case <-timer.C:
					case <-conn.CloseChan():
						timer.Stop()
						return
					}
				}
				_, _ = s.stdout.Write([]byte(end))
				s.finish(t, 0)
			})
		})
		start := time.Now()
		got, err := pd.ReadFile(context.Background(), "/workspace/big.bin")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("got %d bytes, want %d", len(got), len(payload))
		}
		if elapsed := time.Since(start); elapsed < 2*budget {
			t.Errorf("completed in %s, under 2×%s budget", elapsed, budget)
		}
	})

	t.Run("streaming WriteFileStream outlives budget while stdin moves", func(t *testing.T) {
		const chunks = 8
		const chunkSize = 1024
		payload := bytes.Repeat([]byte("w"), chunks*chunkSize)
		var received bytes.Buffer
		_, pd := startFakeKubeletSPDY(t, budget, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
			acceptSPDYExec(t, w, r, 5*time.Second, func(s *spdyStreams, conn httpstream.Connection) {
				_, f, ok := sandboxtest.Unwrap(s.argv[2])
				if !ok {
					t.Errorf("write script missing frame: %q", s.argv[2])
					s.finish(t, 1)
					return
				}
				_, _ = io.Copy(&received, s.stdin)
				begin, end := f.Lines()
				_, _ = s.stdout.Write([]byte(begin + end))
				s.finish(t, 0)
			})
		})
		src := &slowChunksReader{data: payload, chunkSize: chunkSize, delay: budget / 3}
		start := time.Now()
		if err := pd.WriteFileStream(context.Background(), "/workspace/up.bin", src, int64(len(payload))); err != nil {
			t.Fatalf("WriteFileStream: %v", err)
		}
		if !bytes.Equal(received.Bytes(), payload) {
			t.Errorf("server received %d bytes, want %d", received.Len(), len(payload))
		}
		if elapsed := time.Since(start); elapsed < 2*budget {
			t.Errorf("completed in %s, under 2×%s budget", elapsed, budget)
		}
	})

	t.Run("timed Exec disarms stream silence guard after streams open", func(t *testing.T) {
		// Simulate a silent timed command running longer than 2×budget, then
		// finishing cleanly and answering readExit.
		var calls atomic.Int32
		_, pd := startFakeKubeletSPDY(t, budget, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
			call := calls.Add(1)
			acceptSPDYExec(t, w, r, 5*time.Second, func(s *spdyStreams, conn httpstream.Connection) {
				if call == 1 {
					// Stay completely silent for 2.5×budget (longer than stall
					// timeout, well inside the 30s ExecRequest.Timeout).
					timer := time.NewTimer(budget*2 + budget/2)
					select {
					case <-timer.C:
					case <-conn.CloseChan():
						timer.Stop()
						return
					}
					_, _ = s.stdout.Write([]byte("quiet-done\n"))
					s.finish(t, 0)
					return
				}
				// readExit call: answer framed exit code 0.
				_, f, ok := sandboxtest.Unwrap(s.argv[2])
				if !ok {
					t.Errorf("exit script missing frame")
					s.finish(t, 1)
					return
				}
				begin, end := f.Lines()
				_, _ = s.stdout.Write([]byte(begin + "0 1.0 1.0 1.5\n" + end))
				s.finish(t, 0)
			})
		})
		res, err := pd.Exec(context.Background(), sandbox.ExecRequest{
			Command: "sleep 1; echo quiet-done",
			Timeout: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("timed silent Exec failed: %v", err)
		}
		if res.ExitCode != 0 || res.TimedOut || res.Stdout != "quiet-done\n" {
			t.Errorf("unexpected result: %+v", res)
		}
	})

	t.Run("wedged Export mid-stream surfaces ErrStalled on Read", func(t *testing.T) {
		sid := domain.NewID("sesn")
		unblock := make(chan struct{})
		p, _ := startFakeKubeletSPDY(t, budget, 30*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pods") {
				list := corev1.PodList{
					Items: []corev1.Pod{{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "pod-export",
							Labels: map[string]string{sessionLabel: string(sid)},
						},
					}},
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(list)
				return
			}
			acceptSPDYExec(t, w, r, 30*time.Millisecond, func(s *spdyStreams, conn httpstream.Connection) {
				// First exec is exportProbe (sh -c ...); answer framed "P".
				if len(s.argv) >= 3 && s.argv[0] == "sh" {
					_, f, ok := sandboxtest.Unwrap(s.argv[2])
					if ok {
						begin, end := f.Lines()
						_, _ = s.stdout.Write([]byte(begin + "P\n" + end))
					}
					s.finish(t, 0)
					return
				}
				// Second exec is tar -cf -: send partial chunk then wedge.
				_, _ = s.stdout.Write([]byte("partial-tar"))
				select {
				case <-unblock:
				case <-conn.CloseChan():
				}
			})
		})
		t.Cleanup(func() { close(unblock) })
		rc, err := p.Export(context.Background(), sid, "/workspace")
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		defer rc.Close()
		_, err = io.ReadAll(rc)
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged Export ReadAll err = %v, want sandbox.ErrStalled", err)
		}
	})

	t.Run("stalled WriteFileStream source fails with ErrStalled", func(t *testing.T) {
		unblock := make(chan struct{})
		_, pd := startFakeKubeletSPDY(t, budget, 30*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
			acceptSPDYExec(t, w, r, 30*time.Millisecond, func(s *spdyStreams, conn httpstream.Connection) {
				if s.stdin != nil {
					_, _ = io.Copy(io.Discard, s.stdin)
				}
				select {
				case <-unblock:
				case <-conn.CloseChan():
				}
			})
		})
		t.Cleanup(func() { close(unblock) })
		src := &blockingAfterFirstReader{first: bytes.Repeat([]byte("b"), 1024), unblock: unblock}
		err := pd.WriteFileStream(context.Background(), "/workspace/stalled.bin", src, 64<<10)
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("stalled WriteFileStream source err = %v, want sandbox.ErrStalled", err)
		}
	})

	t.Run("non-101 upgrade error formatting and prefixedConn replay", func(t *testing.T) {
		for _, tc := range []struct {
			body, want string
		}{
			{`{"message":"pod not found"}`, "pod not found"},
			{`{"error":"upgrade refused"}`, "upgrade refused"},
			{"plain failure\n", "plain failure"},
			{"   ", "empty server response"},
		} {
			if got := upgradeErrorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("upgradeErrorMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		}

		c1, c2 := netPipe(t)
		go func() {
			_, _ = c2.Write([]byte("tail"))
			_ = c2.Close()
		}()
		pc := &prefixedConn{Conn: c1, buf: []byte("head-")}
		got, err := io.ReadAll(pc)
		if err != nil {
			t.Fatalf("prefixedConn ReadAll: %v", err)
		}
		if string(got) != "head-tail" {
			t.Errorf("prefixedConn = %q, want %q", got, "head-tail")
		}
	})

	t.Run("wedged HTTP proxy CONNECT fails with ErrStalled and honors cancellation", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		unblock := make(chan struct{})
		var wg sync.WaitGroup
		t.Cleanup(func() {
			close(unblock)
			_ = ln.Close()
			wg.Wait()
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				wg.Add(1)
				go func(c net.Conn) {
					defer wg.Done()
					defer c.Close()
					// Read the CONNECT request line/headers, then stay silent forever.
					_, _ = http.ReadRequest(bufio.NewReader(c))
					<-unblock
				}(conn)
			}
		}()

		proxyURL, err := url.Parse("http://" + ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		cfg := &rest.Config{
			Host:  "http://127.0.0.1:1",
			Proxy: http.ProxyURL(proxyURL),
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cl := &client{
			cs:           cs,
			rest:         cfg,
			namespace:    "default",
			stallTimeout: budget,
			pingPeriod:   30 * time.Millisecond,
		}
		pd := &pod{client: cl, name: "sb-proxy", workdir: "/workspace"}

		start := time.Now()
		_, err = pd.Exec(context.Background(), sandbox.ExecRequest{Command: "echo hi"})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged proxy CONNECT err = %v, want sandbox.ErrStalled", err)
		}
		if elapsed := time.Since(start); elapsed < budget || elapsed > 3*time.Second {
			t.Errorf("wedged proxy CONNECT elapsed = %s, want >= %s and < 3s", elapsed, budget)
		}

		// Caller cancellation must also interrupt a wedged proxy CONNECT promptly.
		cl.stallTimeout = 10 * time.Second
		cctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err = pd.ReadFile(cctx, "/workspace/a.txt")
		if errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("caller cancel on wedged proxy CONNECT misclassified as ErrStalled: %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller cancel on wedged proxy CONNECT err = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("working HTTP proxy CONNECT preserves successful SPDY exec", func(t *testing.T) {
		backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			acceptSPDYExec(t, w, r, 5*time.Second, func(s *spdyStreams, _ httpstream.Connection) {
				_, _ = s.stdout.Write([]byte("via-proxy\n"))
				s.finish(t, 0)
			})
		}))
		t.Cleanup(backendSrv.Close)

		// Minimal HTTP CONNECT tunnel proxy forwarding to backendSrv.
		proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		t.Cleanup(func() {
			_ = proxyLn.Close()
			wg.Wait()
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				clientConn, err := proxyLn.Accept()
				if err != nil {
					return
				}
				wg.Add(1)
				go func(cc net.Conn) {
					defer wg.Done()
					defer cc.Close()
					br := bufio.NewReader(cc)
					req, err := http.ReadRequest(br)
					if err != nil || req.Method != http.MethodConnect {
						return
					}
					up, err := net.Dial("tcp", req.Host)
					if err != nil {
						return
					}
					defer up.Close()
					_, _ = io.WriteString(cc, "HTTP/1.1 200 Connection Established\r\n\r\n")
					done := make(chan struct{})
					go func() {
						_, _ = io.Copy(up, br)
						close(done)
					}()
					_, _ = io.Copy(cc, up)
					<-done
				}(clientConn)
			}
		}()

		proxyURL, err := url.Parse("http://" + proxyLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		cfg := &rest.Config{
			Host:  backendSrv.URL,
			Proxy: http.ProxyURL(proxyURL),
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cl := &client{
			cs:           cs,
			rest:         cfg,
			namespace:    "default",
			stallTimeout: budget,
			pingPeriod:   5 * time.Second,
		}
		var stdout, stderr bytes.Buffer
		res, err := cl.execStream(context.Background(), "sb-proxy-ok", containerName, []string{"echo", "via-proxy"}, nil, &stdout, &stderr, true)
		if err != nil {
			t.Fatalf("execStream through HTTP proxy: %v", err)
		}
		if res.code != 0 {
			t.Errorf("exit code = %d, want 0", res.code)
		}
		if stdout.String() != "via-proxy\n" {
			t.Errorf("Stdout = %q, want %q", stdout.String(), "via-proxy\n")
		}
	})

	t.Run("trickling HTTP 101 upgrade headers refresh deadline and outlive budget", func(t *testing.T) {
		// Trickle HTTP 101 headers in small pieces spaced budget/3 apart (total
		// > 1.5*budget), then establish a real SPDY/3.1 exec connection.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		t.Cleanup(func() {
			_ = ln.Close()
			wg.Wait()
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			br := bufio.NewReader(conn)
			if _, err := http.ReadRequest(br); err != nil {
				return
			}
			chunks := []string{
				"HTTP/1.1 101 Switching Protocols\r\n",
				"Connection: Upgrade\r\n",
				"Upgrade: SPDY/3.1\r\n",
				"X-Stream-Protocol-Version: v4.channel.k8s.io\r\n",
				"X-Trickle-1: ok\r\n",
				"\r\n",
			}
			for i, ch := range chunks {
				if _, err := io.WriteString(conn, ch); err != nil {
					return
				}
				if i < len(chunks)-1 {
					<-time.After(budget / 3)
				}
			}
			// Hand the live upgraded connection to a real SPDY server connection.
			streamCh := make(chan httpstream.Stream, 8)
			sc, err := httpstreamspdy.NewServerConnectionWithPings(conn, func(s httpstream.Stream, reply <-chan struct{}) error {
				go func() {
					<-reply
					streamCh <- s
				}()
				return nil
			}, 5*time.Second)
			if err != nil {
				return
			}
			defer sc.Close()
			var errStream, stdout httpstream.Stream
			for errStream == nil || stdout == nil {
				select {
				case s := <-streamCh:
					switch s.Headers().Get(corev1.StreamType) {
					case corev1.StreamTypeError:
						errStream = s
					case corev1.StreamTypeStdout:
						stdout = s
					}
				case <-sc.CloseChan():
					return
				}
			}
			_, _ = stdout.Write([]byte("trickled-ok\n"))
			_ = stdout.Close()
			_, _ = errStream.Write([]byte(`{"status":"Success"}`))
			_ = errStream.Close()
			<-sc.CloseChan()
		}()

		cfg := &rest.Config{Host: "http://" + ln.Addr().String()}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cl := &client{
			cs:           cs,
			rest:         cfg,
			namespace:    "default",
			stallTimeout: budget,
			pingPeriod:   5 * time.Second,
		}
		start := time.Now()
		var stdout bytes.Buffer
		res, err := cl.execStream(context.Background(), "sb-trickle", containerName, []string{"echo", "trickled-ok"}, nil, &stdout, nil, true)
		if err != nil {
			t.Fatalf("trickling 101 execStream: %v", err)
		}
		if res.code != 0 {
			t.Errorf("exit code = %d, want 0", res.code)
		}
		if stdout.String() != "trickled-ok\n" {
			t.Errorf("Stdout = %q, want %q", stdout.String(), "trickled-ok\n")
		}
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("trickling 101 finished in %s, want >= %s to prove upgrade outlived budget", elapsed, budget)
		}
	})

	t.Run("handshakeConn refresh after ctx cancel does not overwrite deadline", func(t *testing.T) {
		c1, c2 := netPipe(t)
		ctx, cancel := context.WithCancel(context.Background())
		gctx, guard := sandbox.NewStallGuard(ctx, 10*time.Second)
		defer guard.Stop()

		hc, err := newHandshakeConn(gctx, c1, guard)
		if err != nil {
			t.Fatalf("newHandshakeConn: %v", err)
		}
		defer hc.disarm()

		// Deliver 1 byte right before cancelling so the first Read returns n>0
		// while ctx is already cancelled, exercising refresh() after AfterFunc.
		go func() {
			_, _ = c2.Write([]byte("x"))
		}()
		var buf [1]byte
		// Wait briefly for peer write, cancel ctx, then read the buffered byte
		// and verify a subsequent blocking Read returns promptly instead of
		// waiting 10s.
		if _, err := hc.Read(buf[:]); err != nil {
			t.Fatalf("first Read: %v", err)
		}
		cancel()
		// Trigger refresh() directly after cancel (as happens when Read returns
		// buffered bytes concurrently with AfterFunc) and ensure the next Read
		// still hits the immediate cancellation deadline rather than +10s.
		hc.refresh()
		done := make(chan error, 1)
		go func() {
			_, err := hc.Read(buf[:])
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("second Read after cancel = nil, want timeout error")
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("hc.Read blocked >500ms after ctx cancel — refresh() overwrote cancellation deadline")
		}
	})
}

func netPipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		_ = c1.Close()
		_ = c2.Close()
	})
	return c1, c2
}

type blockingAfterFirstReader struct {
	first   []byte
	sent    bool
	unblock <-chan struct{}
}

func (r *blockingAfterFirstReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.first), nil
	}
	<-r.unblock
	return 0, io.EOF
}

type slowChunksReader struct {
	data      []byte
	offset    int
	chunkSize int
	delay     time.Duration
}

func (r *slowChunksReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	if r.offset > 0 {
		timer := time.NewTimer(r.delay)
		<-timer.C
	}
	end := min(r.offset+r.chunkSize, len(r.data))
	n := copy(p, r.data[r.offset:end])
	r.offset += n
	return n, nil
}
