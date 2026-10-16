package executor

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
)

// TestCloneReasonReadsTheHostsFailures 🔍: the host's failures that go-git
// keeps too little of read `network` (#940, plan 66), each through a real
// clone of a fixture whose host answers so. go-git took an empty or cut pack
// response for "already up to date", and its checkout then blamed the
// caller's ref; the clone's transport now marks what happened. The host's
// in-band messages and the responses go-git's decoder refuses read `network`
// by go-git's own types and words. A row's `says` is what the log's error then
// carries.
func TestCloneReasonReadsTheHostsFailures(t *testing.T) {
	const result, advert = "git-upload-pack-result", "git-upload-pack-advertisement"
	service := pkt("# service=git-upload-pack\n") + "0000"
	cut := "HTTP/1.1 200 OK\r\nContent-Type: application/x-git-upload-pack-result\r\nContent-Length: 1000\r\n\r\n"
	for _, tc := range []struct {
		name   string
		refs   func(t *testing.T) hostAnswer
		pack   func(t *testing.T) hostAnswer
		commit bool
		says   string
	}{
		{name: "an empty pack response", pack: func(*testing.T) hostAnswer { return gitBody(result, "") },
			says: errEmptyPack.Error()},
		{name: "a pack response cut after its headers", pack: func(t *testing.T) hostAnswer { return brokenAnswer(t, cut) },
			says: io.ErrUnexpectedEOF.Error()},
		{name: "a pack response whose chunked framing breaks", pack: func(t *testing.T) hostAnswer {
			return brokenAnswer(t, "HTTP/1.1 200 OK\r\nContent-Type: application/x-git-upload-pack-result\r\n"+
				"Transfer-Encoding: chunked\r\n\r\nzz\r\nabc")
		}, says: "invalid byte in chunk length"},
		{name: "NAK and no pack", pack: func(*testing.T) hostAnswer { return gitBody(result, "0000"+pkt("NAK\n")) }},
		{name: "a message on the sideband's error band", pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("NAK\n")+pkt("\x03fatal: the host failed to build the pack\n"))
		}},
		{name: "a sideband packet on a band git has no use for", pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("NAK\n")+pkt("\x05junk\n"))
		}},
		{name: "a pack band that carries no pack", pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("NAK\n")+pkt("\x01JUNKJUNKJUNKJUNK"))
		}},
		{name: "an ERR line in the pack response", pack: func(*testing.T) hostAnswer {
			return gitBody(result, pkt("ERR the host failed\n"))
		}},
		{name: "a pack response the decoder refuses", pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("unexpected\n"))
		}},
		{name: "an ERR line in the ref advertisement", refs: func(*testing.T) hostAnswer {
			return gitBody(advert, service+pkt("ERR access denied\n"))
		}},
		{name: "a ref advertisement that is not git's", refs: func(*testing.T) hostAnswer {
			return gitBody(advert, service+pkt("hello, this is not git\n")+"0000")
		}},
		{name: "a commit's empty pack response", commit: true, pack: func(*testing.T) hostAnswer { return gitBody(result, "") },
			says: errEmptyPack.Error()},
		{name: "a commit's pack response cut after its headers", commit: true,
			pack: func(t *testing.T) hostAnswer { return brokenAnswer(t, cut) }, says: io.ErrUnexpectedEOF.Error()},
		{name: "a commit's NAK and no pack", commit: true, pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("NAK\n"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			r := repoRef{URL: fx.url()}
			if tc.commit {
				head, err := fx.repo.Head()
				if err != nil {
					t.Fatal(err)
				}
				r.Checkout = &repoCheckout{Type: "commit", Sha: head.Hash().String()}
			}
			if tc.refs != nil {
				answer(&fx.refsAnswer, tc.refs(t))
			}
			if tc.pack != nil {
				answer(&fx.packAnswer, tc.pack(t))
			}
			_, _, cleanup, err := cloneToTar(context.Background(), r, "ghp_fixture", 1<<30)
			defer cleanup()
			if err == nil {
				t.Fatal("the clone succeeded")
			}
			if got := cloneReason(err); got != repoOutcomeNetwork {
				t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeNetwork, err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error = %q, want it to say %q", err, tc.says)
			}
		})
	}
}

// A clone's response bodies mark a read that failed with whose it was, and
// leave every other read, request and response as they were.
func TestCloneTransportMarksTheHostsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/r.git/git-upload-pack", "/o/r.git/info/refs":
			// An empty 200.
		case "/refused/r.git/git-upload-pack":
			w.WriteHeader(http.StatusUnauthorized)
		case "/whole/r.git/git-upload-pack":
			_, _ = w.Write([]byte("0008NAK\n"))
		case "/cut":
			brokenAnswer(t, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nabc")(w, r)
		case "/stall":
			// Until the client gives up.
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte("whole"))
		}
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: hostTransport{base: http.DefaultTransport}}
	get := func(ctx context.Context, method, path string) error {
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		_, err = io.ReadAll(res.Body)
		return err
	}
	clone := withCloneMarks(context.Background())

	t.Run("an empty pack response", func(t *testing.T) {
		if err := get(clone, http.MethodPost, "/o/r.git/git-upload-pack"); !errors.Is(err, errEmptyPack) {
			t.Errorf("read = %v, want the empty pack response marked", err)
		}
	})
	t.Run("a pack response that ends after its bytes", func(t *testing.T) {
		if err := get(clone, http.MethodPost, "/whole/r.git/git-upload-pack"); err != nil {
			t.Errorf("read = %v, want a clean end", err)
		}
	})
	t.Run("an empty ref advertisement is not an empty pack", func(t *testing.T) {
		if err := get(clone, http.MethodGet, "/o/r.git/info/refs"); err != nil {
			t.Errorf("read = %v, want a clean end: go-git's decoder reads that one", err)
		}
	})
	t.Run("a refusal's empty body is not an empty pack", func(t *testing.T) {
		if err := get(clone, http.MethodPost, "/refused/r.git/git-upload-pack"); err != nil {
			t.Errorf("read = %v, want a clean end: go-git reads a refusal by its status", err)
		}
	})
	t.Run("a body cut short", func(t *testing.T) {
		err := get(clone, http.MethodGet, "/cut")
		if !errors.Is(err, errHostTransport) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("read = %v, want the cut marked the host's", err)
		}
		// go-git takes an io.ErrUnexpectedEOF it compares by == for an empty
		// response.
		if err == io.ErrUnexpectedEOF {
			t.Error("read = io.ErrUnexpectedEOF itself, which go-git takes for \"already up to date\"")
		}
	})
	t.Run("a read once the context is done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(clone)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stall", nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		cancel()
		if _, err := io.ReadAll(res.Body); err == nil || errors.Is(err, errHostTransport) {
			t.Errorf("read = %v, want the cancellation's own error", err)
		}
	})
	t.Run("another request passes through", func(t *testing.T) {
		if err := get(context.Background(), http.MethodPost, "/o/r.git/git-upload-pack"); err != nil {
			t.Errorf("read = %v, want an empty pack response passed through", err)
		}
	})
	t.Run("a read this machine failed", func(t *testing.T) {
		local := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ENOBUFS)}
		_, err := (&markedBody{ReadCloser: io.NopCloser(failingReader{0, local}), ctx: clone}).Read(make([]byte, 8))
		if !errors.Is(err, errOwnRead) || errors.Is(err, errHostTransport) || !errors.Is(err, local) {
			t.Errorf("read = %v, want it marked this executor's", err)
		}
	})
	t.Run("the bytes that came with a failure", func(t *testing.T) {
		n, err := (&markedBody{ReadCloser: io.NopCloser(failingReader{3, gzip.ErrChecksum}), ctx: clone}).Read(make([]byte, 8))
		if n != 3 || !errors.Is(err, errHostTransport) || !errors.Is(err, gzip.ErrChecksum) {
			t.Errorf("read = %d, %v; want 3 bytes and the failure marked the host's", n, err)
		}
	})
}

// failingReader reads n bytes and err at once.
type failingReader struct {
	n   int
	err error
}

func (r failingReader) Read(p []byte) (int, error) { return copy(p, make([]byte, r.n)), r.err }

// The transport's marks decide the reason by their type, and by their words
// where go-git kept only text; a file named in those words does not.
func TestCloneReasonReadsTheTransportsMarks(t *testing.T) {
	local := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ENOBUFS)}
	host := fmt.Errorf("%w: %w", errHostTransport, errors.New("invalid byte in chunk length"))
	own := fmt.Errorf("%w: %w", errOwnRead, local)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an empty pack response", errEmptyPack, repoOutcomeNetwork},
		{"a cut go-git returns as it is", fmt.Errorf("%w: %w", errHostTransport, io.ErrUnexpectedEOF), repoOutcomeNetwork},
		{"a cut go-git kept only the words of", fmt.Errorf("reading the pack: %s", host), repoOutcomeNetwork},
		{"this machine's read, typed", own, repoOutcomeInternal},
		{"this machine's read, in go-git's decode words", fmt.Errorf("error decoding upload-pack response: %s", own), repoOutcomeInternal},
		{"a spool file named in the host's words", &fs.PathError{Op: "open",
			Path: "/tmp/map-repo-clone-1/tree/" + errHostTransport.Error(), Err: syscall.ENOSPC}, repoOutcomeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cloneReason(cloneFailed(context.Background(), tc.err)); got != tc.want {
				t.Errorf("cloneReason = %q, want %q: %v", got, tc.want, tc.err)
			}
		})
	}
}

// A packet too large for the plain side-band's size reads `network` by
// go-git's words for it. The fixture's http-backend offers side-band-64k,
// whose size no pkt-line passes, so no clone of it reaches this.
func TestCloneReasonReadsAnOversizeSidebandPacket(t *testing.T) {
	err := cloneFailed(context.Background(), fmt.Errorf("%s", sideband.ErrMaxPackedExceeded))
	if got := cloneReason(err); got != repoOutcomeNetwork {
		t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeNetwork, err)
	}
}
