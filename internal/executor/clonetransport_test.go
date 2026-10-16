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

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// TestCloneReasonReadsTheHostsFailures 🔍: the host's failures that go-git
// keeps too little of read `network` (#940, plan 66), each through a real
// clone of a fixture whose host answers so. go-git takes an empty or cut pack
// response for "already up to date", and its checkout then blamed the caller's
// ref; the clone's transport record holds what happened. The host's in-band
// messages and the responses go-git's decoder refuses read `network` by
// go-git's own types and words. A row's `says` is what the log's error then
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

// A failure the transport recorded leaves a reason go-git's error has of its
// own: the clone's deadline or its cancellation, which ended the read the
// record failed, and a refused proxy, whose error go-git keeps. The message
// then carries no mark.
func TestCloneFailedLeavesAReasonOfItsOwn(t *testing.T) {
	refused := &net.OpError{Op: "proxyconnect", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	for _, tc := range []struct {
		name string
		end  func() (context.Context, func())
		err  error
		want string
	}{
		{"the clone's deadline passes while the pack is read", func() (context.Context, func()) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			return ctx, func() { <-ctx.Done(); cancel() }
		}, fmt.Errorf("error decoding upload-pack response: %s", context.DeadlineExceeded), repoOutcomeTimeout},
		{"the clone is cancelled while the pack is read", func() (context.Context, func()) {
			return context.WithCancel(context.Background())
		}, fmt.Errorf("error decoding upload-pack response: %s", context.Canceled), repoOutcomeInternal},
		{"a proxy refuses the connection", func() (context.Context, func()) {
			return context.Background(), func() {}
		}, &url.Error{Op: "Get", URL: "https://github.com/o/r.git/info/refs", Err: refused}, repoOutcomeNetwork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, end := tc.end()
			ctx, rec := withCloneRecord(parent)
			if tc.want == repoOutcomeNetwork {
				rec.note(ctx, refused)
			} else {
				rec.note(ctx, io.ErrUnexpectedEOF)
			}
			if rec.failure() == nil {
				t.Fatal("the note came after the clone's context ended")
			}
			end()
			err := cloneFailed(ctx, tc.err)
			if got := cloneReason(err); got != tc.want {
				t.Errorf("cloneReason = %q, want %q: %v", got, tc.want, err)
			}
			if strings.Contains(err.Error(), errHostTransport.Error()) {
				t.Errorf("error = %q, want no mark: the reason is go-git's error's own", err)
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
			brokenAnswer(t, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nabc")(w, r)
		case "/moved":
			brokenAnswer(t, "HTTP/1.1 302 Found\r\nLocation: /whole\r\nContent-Length: 10\r\n\r\nabc")(w, r)
		case "/refused/r.git/git-upload-pack":
			w.WriteHeader(http.StatusUnauthorized)
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
	t.Run("a local failure after a host's that go-git read past", func(t *testing.T) {
		ctx, rec := withCloneRecord(context.Background())
		local := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ENOBUFS)}
		rec.note(ctx, gzip.ErrChecksum)
		rec.note(ctx, local)
		rec.note(ctx, io.ErrUnexpectedEOF)
		if rec.failure() != local {
			t.Errorf("record = %v, want this machine's failure", rec.failure())
		}
		err := cloneFailed(ctx, fmt.Errorf("error decoding upload-pack response: %s", local))
		if got := cloneReason(err); got != repoOutcomeInternal {
			t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeInternal, err)
		}
	})
	t.Run("a redirect's body is the client's to drain", func(t *testing.T) {
		ctx, rec := withCloneRecord(context.Background())
		if err := get(ctx, http.MethodGet, "/moved"); err != nil {
			t.Fatalf("the redirect was not followed: %v", err)
		}
		if rec.failure() != nil {
			t.Errorf("record = %v, want nothing: the client drained the redirect's body and went on", rec.failure())
		}
	})
	t.Run("a refusal's empty body is not an empty pack", func(t *testing.T) {
		ctx, rec := withCloneRecord(context.Background())
		_ = get(ctx, http.MethodPost, "/refused/r.git/git-upload-pack")
		if rec.failure() != nil {
			t.Errorf("record = %v, want nothing: go-git reads a refusal by its status", rec.failure())
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
	chunk := errors.New("invalid byte in chunk length")
	for _, tc := range []struct {
		name     string
		recorded error
		err      error
		want     string
	}{
		{"the checkout misses what an empty pack left out", errEmptyPack, missing, repoOutcomeNetwork},
		{"go-git reads only text", chunk, fmt.Errorf("%s", chunk), repoOutcomeNetwork},
		{"a failure go-git read past, before an object the checkout missed", gzip.ErrChecksum, missing, repoOutcomeCheckout},
		{"a cut go-git read past, before a failure of go-git's own", cutRead,
			errors.New("go-git cannot check out a file of this mode"), repoOutcomeInternal},
		{"nothing recorded", nil, missing, repoOutcomeCheckout},
		{"the host refused the token", cutRead, fmt.Errorf("%w: bad credentials", transport.ErrAuthenticationRequired), repoOutcomeAuth},
		{"the repository is not there", cutRead, transport.ErrRepositoryNotFound, repoOutcomeNotFound},
		{"the clone is too large", cutRead, errCloneTooLarge, repoOutcomeTooLarge},
		{"the spool's disk fills", cutRead, &fs.PathError{Op: "write", Path: "/tmp/x", Err: syscall.ENOSPC}, repoOutcomeInternal},
		{"the spool's disk fills, on a file named for the recorded failure", cutRead,
			&fs.PathError{Op: "open", Path: "/tmp/map-repo-clone-1/tree/" + cutRead.Error(), Err: syscall.ENOSPC}, repoOutcomeInternal},
		{"this machine's buffers ran out", local, missing, repoOutcomeCheckout},
		{"this machine's buffers ran out while go-git decoded the response", local,
			fmt.Errorf("error decoding upload-pack response: %s", local), repoOutcomeInternal},
		{"this machine's descriptors ran out for the pack request after a read go-git went past", gzip.ErrChecksum,
			plumbing.NewUnexpectedError(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("socket", syscall.EMFILE)}),
			repoOutcomeInternal},
		{"a ref the host's answer lacks", cutRead, fmt.Errorf("%w: refs/heads/no-such-branch", plumbing.ErrReferenceNotFound), repoOutcomeCheckout},
		{"the host does not have the commit", cutRead,
			withCause{fmt.Errorf("%w: not our ref", plumbing.ErrObjectNotFound), errCommitRefused}, repoOutcomeCheckout},
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
			if marked := errors.Is(err, errHostTransport); marked != (tc.want == repoOutcomeNetwork) {
				t.Errorf("marked = %v, want the mark only where the host's failure ended the clone: %v", marked, err)
			}
		})
	}
}
