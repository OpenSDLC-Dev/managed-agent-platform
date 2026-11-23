package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		var calls int
		_, pd := startFakeKubeletSPDY(t, budget, 5*time.Second, func(w http.ResponseWriter, r *http.Request) {
			calls++
			call := calls
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
