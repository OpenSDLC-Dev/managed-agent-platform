package executor

import (
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

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// TestCloneReasonReadsTheHostsFailures 🔍: the host's failures that go-git
// keeps too little of read `network` (#940, plan 66), each through a real
// clone of a fixture whose host answers so. go-git takes an empty or cut pack
// response for "already up to date", and its checkout then blamed the caller's
// ref; the clone's transport record holds what happened. The host's in-band
// messages and the responses go-git's decoder refuses read `network` by
// go-git's own types and words. says is what the log's error then carries.
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

// A clone's record holds its own transport's failures and no other clone's:
// the transport is installed once per process, and the record travels in the
// request's context. A request with no record passes through as before. A
// failure while the context is done is the deadline's or the cancellation's,
// not the host's, and the first failure is the one kept.
func TestCloneRecordNotesItsOwnTransportsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/r.git/git-upload-pack", "/o/r.git/info/refs":
			// An empty 200.
		case "/cut":
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer conn.Close()
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nabc")
			_ = buf.Flush()
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
	client := &http.Client{Transport: recordingTransport{base: http.DefaultTransport}}
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

	t.Run("an empty pack response is the clone's alone", func(t *testing.T) {
		ctx, rec := withCloneRecord(context.Background())
		other, quiet := withCloneRecord(context.Background())
		_ = get(ctx, http.MethodPost, "/o/r.git/git-upload-pack")
		_ = get(other, http.MethodGet, "/whole")
		if !errors.Is(rec.failure(), errEmptyPack) {
			t.Errorf("record = %v, want the empty pack response", rec.failure())
		}
		if quiet.failure() != nil {
			t.Errorf("another clone's record = %v, want nothing", quiet.failure())
		}
	})
	t.Run("an empty ref advertisement is not an empty pack", func(t *testing.T) {
		ctx, rec := withCloneRecord(context.Background())
		_ = get(ctx, http.MethodGet, "/o/r.git/info/refs")
		if rec.failure() != nil {
			t.Errorf("record = %v, want nothing: go-git's decoder reads that one", rec.failure())
		}
	})
	t.Run("a body cut short", func(t *testing.T) {
		ctx, rec := withCloneRecord(context.Background())
		if err := get(ctx, http.MethodGet, "/cut"); err == nil {
			t.Fatal("a cut body read whole")
		}
		if !errors.Is(rec.failure(), io.ErrUnexpectedEOF) {
			t.Errorf("record = %v, want the cut body's read", rec.failure())
		}
		_ = get(ctx, http.MethodPost, "/o/r.git/git-upload-pack")
		if !errors.Is(rec.failure(), io.ErrUnexpectedEOF) {
			t.Errorf("record = %v, want the first failure kept", rec.failure())
		}
	})
	t.Run("a refused request", func(t *testing.T) {
		ctx, rec := withCloneRecord(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/", nil)
		if _, err := client.Do(req); err == nil {
			t.Fatal("a dial to a closed port succeeded")
		}
		var opErr *net.OpError
		if !errors.As(rec.failure(), &opErr) {
			t.Errorf("record = %v, want the refused dial", rec.failure())
		}
	})
	t.Run("a failure once the context is done", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		ctx, rec := withCloneRecord(parent)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stall", nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		cancel()
		if _, err := io.ReadAll(res.Body); err == nil {
			t.Fatal("a cancelled read finished")
		}
		if rec.failure() != nil {
			t.Errorf("record = %v, want nothing: the cancellation is ours", rec.failure())
		}
	})
	t.Run("no record", func(t *testing.T) {
		if err := get(context.Background(), http.MethodPost, "/o/r.git/git-upload-pack"); err != nil {
			t.Errorf("an empty pack response with no record = %v, want it passed through", err)
		}
		if rec := cloneRecordFrom(context.Background()); rec.failure() != nil {
			t.Errorf("no record holds %v", rec.failure())
		}
	})
}

// A recorded failure decides `network` only where go-git's error would read
// `internal` or `checkout`, the reasons go-git concludes from what was missing.
// The rest came from what arrived, or from this machine.
func TestCloneFailedReadsTheRecordOverWhatWasMissing(t *testing.T) {
	cutRead := fmt.Errorf("read: %w", io.ErrUnexpectedEOF)
	local := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ENOBUFS)}
	missing := fmt.Errorf("invalid reset option: %w", plumbing.ErrObjectNotFound)
	for _, tc := range []struct {
		name     string
		recorded error
		err      error
		want     string
	}{
		{"the checkout misses what an empty pack left out", errEmptyPack, missing, repoOutcomeNetwork},
		{"go-git reads only text", cutRead, errors.New("invalid byte in chunk length"), repoOutcomeNetwork},
		{"nothing recorded", nil, missing, repoOutcomeCheckout},
		{"the host refused the token", cutRead, fmt.Errorf("%w: bad credentials", transport.ErrAuthenticationRequired), repoOutcomeAuth},
		{"the repository is not there", cutRead, transport.ErrRepositoryNotFound, repoOutcomeNotFound},
		{"the clone is too large", cutRead, errCloneTooLarge, repoOutcomeTooLarge},
		{"the spool's disk fills", cutRead, &fs.PathError{Op: "write", Path: "/tmp/x", Err: syscall.ENOSPC}, repoOutcomeInternal},
		{"this machine's buffers ran out", local, missing, repoOutcomeCheckout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, rec := withCloneRecord(context.Background())
			if tc.recorded != nil {
				rec.note(ctx, tc.recorded)
			}
			err := cloneFailed(ctx, tc.err)
			if got := cloneReason(err); got != tc.want {
				t.Errorf("cloneReason = %q, want %q: %v", got, tc.want, err)
			}
			if tc.want == repoOutcomeNetwork && !strings.Contains(err.Error(), tc.recorded.Error()) {
				t.Errorf("error = %q, want it to carry the recorded failure", err)
			}
		})
	}
}
