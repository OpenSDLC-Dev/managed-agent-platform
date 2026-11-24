package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

func stallFakeDaemon(t *testing.T, stall time.Duration, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, err := New(Config{
		Host:         "tcp://" + strings.TrimPrefix(srv.URL, "http://"),
		StallTimeout: stall,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// partialTarHeader writes a valid 512-byte tar header claiming size bytes and
// prefix bytes of payload, then stops short of size.
func partialTarHeader(t *testing.T, name string, size int64, prefix []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     size,
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(prefix); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

func TestDockerTransportStallContract(t *testing.T) {
	sandboxtest.RunTransportStall(t, sandboxtest.TransportStallBackend{
		WedgedHandshake: func(t *testing.T, stall time.Duration) sandbox.Sandbox {
			unblock := make(chan struct{})
			p := stallFakeDaemon(t, stall, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/exec"):
					_, _ = io.WriteString(w, `{"Id":"e1"}`)
				case r.URL.Path == "/exec/e1/start",
					r.Method == http.MethodGet && r.URL.Path == "/containers/abc/archive":
					// Accept request, send zero response headers until cleanup.
					<-unblock
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
			})
			t.Cleanup(func() { close(unblock) })
			return p.attach("abc", "/workspace", "", false)
		},

		WedgedExecStream: func(t *testing.T, stall time.Duration) sandbox.Sandbox {
			unblock := make(chan struct{})
			p := stallFakeDaemon(t, stall, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/exec"):
					_, _ = io.WriteString(w, `{"Id":"e1"}`)
				case r.URL.Path == "/exec/e1/start":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(frame(streamStdout, "hello\n"))
					w.(http.Flusher).Flush()
					<-unblock
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
			})
			t.Cleanup(func() { close(unblock) })
			return p.attach("abc", "/workspace", "", false)
		},

		WedgedReadFile: func(t *testing.T, stall time.Duration) sandbox.Sandbox {
			unblock := make(chan struct{})
			partial := partialTarHeader(t, "a.txt", 4096, []byte("part"))
			p := stallFakeDaemon(t, stall, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/containers/abc/archive" {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(partial)
					w.(http.Flusher).Flush()
					<-unblock
					return
				}
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			})
			t.Cleanup(func() { close(unblock) })
			return p.attach("abc", "/workspace", "", false)
		},

		WedgedWriteFile: func(t *testing.T, stall time.Duration) sandbox.Sandbox {
			unblock := make(chan struct{})
			p := stallFakeDaemon(t, stall, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut && r.URL.Path == "/containers/abc/archive" {
					_, _ = io.Copy(io.Discard, r.Body)
					<-unblock
					return
				}
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			})
			t.Cleanup(func() { close(unblock) })
			return p.attach("abc", "/workspace", "", false)
		},
	})
}

func TestDockerStallGuardCoversUnixSocketAndControlRPCs(t *testing.T) {
	const budget = 150 * time.Millisecond

	t.Run("unix socket wedged request", func(t *testing.T) {
		sock := filepath.Join(t.TempDir(), "docker.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("listen unix: %v", err)
		}
		unblock := make(chan struct{})
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-unblock
		})}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() {
			close(unblock)
			_ = srv.Close()
		})

		p, err := New(Config{Host: "unix://" + sock, StallTimeout: budget})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = p.Owned(context.Background())
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("unix socket Owned err = %v, want ErrStalled", err)
		}
	})

	t.Run("wedged execCreate", func(t *testing.T) {
		unblock := make(chan struct{})
		p := stallFakeDaemon(t, budget, func(w http.ResponseWriter, r *http.Request) {
			<-unblock
		})
		t.Cleanup(func() { close(unblock) })
		_, err := p.attach("abc", "/workspace", "", false).Exec(context.Background(), sandbox.ExecRequest{
			Command: "echo hi", Timeout: 10 * time.Second,
		})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged execCreate err = %v, want ErrStalled", err)
		}
	})

	t.Run("wedged execInspect after stream close", func(t *testing.T) {
		unblock := make(chan struct{})
		p := stallFakeDaemon(t, budget, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/exec"):
				_, _ = io.WriteString(w, `{"Id":"e1"}`)
			case r.URL.Path == "/exec/e1/start":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(frame(streamStdout, "done\n"))
			case r.URL.Path == "/exec/e1/json":
				<-unblock
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
		})
		t.Cleanup(func() { close(unblock) })
		_, err := p.attach("abc", "/workspace", "", false).Exec(context.Background(), sandbox.ExecRequest{Command: "echo done"})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged execInspect err = %v, want ErrStalled", err)
		}
	})

	t.Run("wedged pullImage mid-stream", func(t *testing.T) {
		unblock := make(chan struct{})
		p := stallFakeDaemon(t, budget, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/json"):
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"No such container"}`)
			case r.URL.Path == "/containers/create":
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"No such image"}`)
			case r.URL.Path == "/images/create":
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"status":"Pulling"}`+"\n")
				w.(http.Flusher).Flush()
				<-unblock
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
		})
		t.Cleanup(func() { close(unblock) })
		_, err := p.Provision(context.Background(), sandbox.Spec{SessionID: domain.NewID("sesn"), Image: "img:1"})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged pullImage err = %v, want ErrStalled", err)
		}
	})

	t.Run("wedged Export mid-stream", func(t *testing.T) {
		sid := domain.NewID("sesn")
		unblock := make(chan struct{})
		partial := partialTarHeader(t, "workspace/f.txt", 4096, []byte("part"))
		p := stallFakeDaemon(t, budget, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/json"):
				_, _ = io.WriteString(w, inspectJSON("abc", sandbox.Spec{SessionID: sid, Image: "img:1"}, true))
			case r.Method == http.MethodGet && r.URL.Path == "/containers/abc/archive":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(partial)
				w.(http.Flusher).Flush()
				<-unblock
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
		})
		t.Cleanup(func() { close(unblock) })
		rc, err := p.Export(context.Background(), sid, "/workspace")
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		defer rc.Close()
		_, err = io.ReadAll(rc)
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged Export ReadAll err = %v, want ErrStalled", err)
		}
	})

	t.Run("wedged HTTP 500 error body", func(t *testing.T) {
		unblock := make(chan struct{})
		p := stallFakeDaemon(t, budget, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"partial`)
			w.(http.Flusher).Flush()
			<-unblock
		})
		t.Cleanup(func() { close(unblock) })
		_, err := p.Owned(context.Background())
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged 500 body err = %v, want ErrStalled", err)
		}
	})

	t.Run("streaming ReadFileStream outlives budget while moving", func(t *testing.T) {
		const chunks = 8
		const chunkSize = 1024
		payload := bytes.Repeat([]byte("r"), chunks*chunkSize)
		tarball, err := tarFile("a.txt", payload)
		if err != nil {
			t.Fatal(err)
		}
		p := stallFakeDaemon(t, budget, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			step := len(tarball) / chunks
			for off := 0; off < len(tarball); off += step {
				end := min(off+step, len(tarball))
				_, _ = w.Write(tarball[off:end])
				flusher.Flush()
				select {
				case <-time.After(budget / 3):
				case <-r.Context().Done():
					return
				}
			}
		})
		rc, size, err := p.attach("abc", "/workspace", "", false).ReadFileStream(context.Background(), "/workspace/a.txt", sandbox.MaxFileBytes)
		if err != nil {
			t.Fatalf("ReadFileStream: %v", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if size != int64(len(payload)) || !bytes.Equal(got, payload) {
			t.Errorf("got %d bytes (declared %d), want %d", len(got), size, len(payload))
		}
	})
}
