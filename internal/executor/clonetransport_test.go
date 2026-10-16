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
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// TestCloneReasonReadsTheHostsFailures 🔍: the host's failures that go-git
// keeps too little of read `network` (#940, plan 66), each through a real
// clone of a fixture whose host answers so. go-git took an empty or cut pack
// response for "already up to date", and its checkout then blamed the
// caller's ref; the clone's transport now marks what happened. The host's
// in-band messages and the responses go-git's decoder refuses read `network`
// by go-git's own types and words. A row's `says` is what the log's error then
// carries, which shows the fixture served the row's answer.
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
		{name: "NAK and no pack", pack: func(*testing.T) hostAnswer { return gitBody(result, "0000"+pkt("NAK\n")) },
			says: git.ErrFetching.Error()},
		{name: "a message on the sideband's error band", pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("NAK\n")+pkt("\x03fatal: the host failed to build the pack\n"))
		}, says: "unexpected error: fatal: the host failed to build the pack"},
		{name: "a sideband packet on a band git has no use for", pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("NAK\n")+pkt("\x05junk\n"))
		}, says: "unknown channel \x05junk"},
		{name: "a pack band that carries no pack", pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("NAK\n")+pkt("\x01JUNKJUNKJUNKJUNK"))
		}, says: packfile.ErrBadSignature.Error()},
		{name: "an ERR line in the pack response", pack: func(*testing.T) hostAnswer {
			return gitBody(result, pkt("ERR the host failed\n"))
		}, says: "error decoding upload-pack response: the host failed"},
		{name: "a pack response the decoder refuses", pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("unexpected\n"))
		}, says: "error decoding upload-pack response: unexpected content"},
		{name: "an ERR line in the ref advertisement", refs: func(*testing.T) hostAnswer {
			return gitBody(advert, service+pkt("ERR access denied\n"))
		}, says: "access denied"},
		{name: "a ref advertisement that is not git's", refs: func(*testing.T) hostAnswer {
			return gitBody(advert, service+pkt("hello, this is not git\n")+"0000")
		}, says: "hello, this is not git"},
		{name: "a commit's empty pack response", commit: true, pack: func(*testing.T) hostAnswer { return gitBody(result, "") },
			says: errEmptyPack.Error()},
		{name: "a commit's pack response cut after its headers", commit: true,
			pack: func(t *testing.T) hostAnswer { return brokenAnswer(t, cut) }, says: io.ErrUnexpectedEOF.Error()},
		{name: "a commit's NAK and no pack", commit: true, pack: func(*testing.T) hostAnswer {
			return gitBody(result, "0000"+pkt("NAK\n"))
		}, says: packfile.ErrEmptyPackfile.Error()},
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
	t.Run("an empty pack response once the context is done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(clone)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/o/r.git/git-upload-pack", nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		cancel()
		if _, err := io.ReadAll(res.Body); !errors.Is(err, errEmptyPack) {
			t.Errorf("read = %v, want the empty pack response marked: go-git takes a bare end for \"already up to date\"", err)
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
		if _, err := io.ReadAll(res.Body); !errors.Is(err, context.Canceled) || transportMark(err) != errContextRead {
			t.Errorf("read = %v, want the cancellation's own error, marked the context's", err)
		}
	})
	t.Run("a read the deadline ends", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(clone, 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stall", nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("the request = %v, want its headers before the deadline, so the body's read meets it", err)
		}
		defer res.Body.Close()
		if _, err := io.ReadAll(res.Body); !errors.Is(err, context.DeadlineExceeded) || transportMark(err) != errContextRead {
			t.Errorf("read = %v, want the deadline's own error, marked the context's", err)
		}
	})
	t.Run("a failure that is not the context's, once the context is done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(clone)
		cancel()
		_, err := (&markedBody{ReadCloser: io.NopCloser(errReader{io.ErrUnexpectedEOF}), ctx: ctx}).Read(make([]byte, 8))
		if !errors.Is(err, errHostTransport) {
			t.Errorf("read = %v, want it marked the host's", err)
		}
		local := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ENOBUFS)}
		_, err = (&markedBody{ReadCloser: io.NopCloser(errReader{local}), ctx: ctx}).Read(make([]byte, 8))
		if !errors.Is(err, errOwnRead) {
			t.Errorf("read = %v, want it marked this executor's", err)
		}
	})
	t.Run("another request passes through", func(t *testing.T) {
		if err := get(context.Background(), http.MethodPost, "/o/r.git/git-upload-pack"); err != nil {
			t.Errorf("read = %v, want an empty pack response passed through", err)
		}
	})
	t.Run("a read this machine failed", func(t *testing.T) {
		local := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ENOBUFS)}
		_, err := (&markedBody{ReadCloser: io.NopCloser(errReader{local}), ctx: clone}).Read(make([]byte, 8))
		if !errors.Is(err, errOwnRead) || errors.Is(err, errHostTransport) || !errors.Is(err, local) {
			t.Errorf("read = %v, want it marked this executor's", err)
		}
	})
	t.Run("a host's words that quote this machine's mark", func(t *testing.T) {
		goaway := errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug="` +
			errOwnRead.Error() + `: no buffer space available"`)
		_, err := (&markedBody{ReadCloser: io.NopCloser(errReader{goaway}), ctx: clone}).Read(make([]byte, 8))
		if !errors.Is(err, errHostTransport) || errors.Is(err, errOwnRead) {
			t.Errorf("read = %v, want it marked the host's", err)
		}
		if got := cloneReason(cloneFailed(context.Background(), err)); got != repoOutcomeNetwork {
			t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeNetwork, err)
		}
	})
	t.Run("the bytes that came with a failure", func(t *testing.T) {
		n, err := (&markedBody{ReadCloser: io.NopCloser(failingReader{3, gzip.ErrChecksum}), ctx: clone}).Read(make([]byte, 8))
		if n != 3 || !errors.Is(err, errHostTransport) || !errors.Is(err, gzip.ErrChecksum) {
			t.Errorf("read = %d, %v; want 3 bytes and the failure marked the host's", n, err)
		}
	})
}

// Every go-git clone over http or https goes through the transport: go-git
// holds its own default client for both until the package installs its own.
// The fixture's clones prove the http one marks the host's failures, so https
// must hold the same client.
func TestCloneTransportIsGoGitsForHTTPAndHTTPS(t *testing.T) {
	if gitclient.Protocols["http"] == githttp.DefaultClient {
		t.Error("go-git's http client is its own default, not the one that marks the host's failures")
	}
	if gitclient.Protocols["https"] != gitclient.Protocols["http"] {
		t.Error("go-git's https client is not the one its http clones use, which marks the host's failures")
	}
}

// failingReader reads n bytes and err at once, which errReader, with no bytes,
// does not.
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
		{"an ERR line in this machine's words", &pktline.ErrorLine{Text: errOwnRead.Error()}, repoOutcomeNetwork},
		{"a request the host's GOAWAY failed, in this machine's words", &url.Error{Op: "Get",
			URL: "https://example.com/o/r.git/info/refs?service=git-upload-pack", Err: errors.New(
				`http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug="` + errOwnRead.Error() + `"`)},
			repoOutcomeNetwork},
		{"the pack request the host's GOAWAY failed, in this machine's words", plumbing.NewUnexpectedError(&url.Error{Op: "Post",
			URL: "https://example.com/o/r.git/git-upload-pack", Err: errors.New(
				`http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug="` + errOwnRead.Error() + `"`)}),
			repoOutcomeNetwork},
		{"the host's mark, in go-git's decode words, quoting this machine's", fmt.Errorf("error decoding upload-pack response: %s",
			fmt.Errorf("%w: %w", errHostTransport, errors.New(`debug="`+errOwnRead.Error()+`"`))), repoOutcomeNetwork},
		{"this machine's mark, in go-git's decode words, quoting the host's", fmt.Errorf("error decoding upload-pack response: %s",
			fmt.Errorf("%w: %w", errOwnRead, errors.New(errHostTransport.Error()))), repoOutcomeInternal},
		{"a ref advertisement in this machine's words, not git's", packp.NewErrUnexpectedData("pkt-line 1: invalid length",
			[]byte(errOwnRead.Error())), repoOutcomeNetwork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cloneReason(cloneFailed(context.Background(), tc.err)); got != tc.want {
				t.Errorf("cloneReason = %q, want %q: %v", got, tc.want, tc.err)
			}
		})
	}
}

// A packet too large for the plain side-band's size reads `network` by
// go-git's sentinel for it. The fixture's http-backend offers side-band-64k,
// whose size no pkt-line passes, so no clone of it reaches this.
func TestCloneReasonReadsAnOversizeSidebandPacket(t *testing.T) {
	err := cloneFailed(context.Background(), fmt.Errorf("reading the pack: %w", sideband.ErrMaxPackedExceeded))
	if got := cloneReason(err); got != repoOutcomeNetwork {
		t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeNetwork, err)
	}
}
