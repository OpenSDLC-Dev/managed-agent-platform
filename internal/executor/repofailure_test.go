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
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// cipherThatFails stands in for a cipher backend that cannot answer: a key
// service the executor cannot reach, one that drops the connection, or one
// that stalls past its client's timeout.
type cipherThatFails struct {
	secrets.Cipher
	err error
}

func (c cipherThatFails) Decrypt(context.Context, []byte, string) ([]byte, error) {
	return nil, c.err
}

// TestCloneReasonBlamesTheRemoteOnlyForTheFetch 🔍: auth, not_found, checkout
// and network name the git host, and only go-git's clone speaks to it. What
// surrounds the clone is this platform's own: the credential row in Postgres,
// the token's decrypt (a key service, over the network), the spool on the
// executor's disk. Their errors read like the host's — a refused dial, an
// unexpected EOF, a *fs.PathError, which has a Timeout method — and were
// reported as `network`, sending the operator to the git host over a fault in
// their own database or key service. A key service that stalled read as
// `timeout`, the clone's own deadline, which was not yet set.
func TestCloneReasonBlamesTheRemoteOnlyForTheFetch(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	// What an HTTP client's own timeout returns, as a key service's client
	// has: it matches context.DeadlineExceeded.
	stalled := func() error {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer srv.Close()
		_, err := (&http.Client{Timeout: 10 * time.Millisecond}).Get(srv.URL)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a client timeout = %v, which does not match context.DeadlineExceeded", err)
		}
		return err
	}()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"key service refuses the dial", refused},
		{"key service drops the connection", io.ErrUnexpectedEOF},
		{"key service stalls", stalled},
	} {
		t.Run("decrypt fails: "+tc.name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			h := newHarness(t, &fakeSandbox{})
			h.seedRepoResource(t, "sesrsc_kms", fx.url(), repoMount, "ghp_fixture", nil)
			h.exec.cipher = cipherThatFails{Cipher: h.cipher, err: fmt.Errorf("decrypt: %w", tc.err)}

			_, err := h.exec.materializeRepo(context.Background(), &fakeSandbox{}, h.sid, repoRef{
				ID: "sesrsc_kms", Type: "github_repository", URL: fx.url(), MountPath: repoMount,
			}, "", func() {})
			if got := cloneReason(err); err == nil || got != repoOutcomeInternal {
				t.Errorf("cloneReason = %q, want %q — the key service is ours: %v", got, repoOutcomeInternal, err)
			}
			if n := fx.requests.Load(); n != 0 {
				t.Errorf("the remote was asked %d time(s)", n)
			}
		})
	}

	t.Run("credential row gone", func(t *testing.T) {
		fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
		h := newHarness(t, &fakeSandbox{})
		h.seedRepoResource(t, "sesrsc_gone", fx.url(), repoMount, "ghp_fixture", nil)
		if _, err := h.pool.Exec(context.Background(),
			`DELETE FROM session_resource_credentials WHERE resource_id = $1`, "sesrsc_gone"); err != nil {
			t.Fatalf("delete the credential row: %v", err)
		}

		_, err := h.exec.materializeRepo(context.Background(), &fakeSandbox{}, h.sid, repoRef{
			ID: "sesrsc_gone", Type: "github_repository", URL: fx.url(), MountPath: repoMount,
		}, "", func() {})
		if got := cloneReason(err); !errors.Is(err, errRepoNoCredential) || got != repoOutcomeInternal {
			t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeInternal, err)
		}
	})

	// go-git's clone checks the tree out onto the spool, so the phase alone
	// does not decide it: a file error must not read as the network's, by
	// its type or by its text, which quotes a name the repository chose.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a write to the pack", &fs.PathError{
			Op: "write", Path: "/tmp/map-repo-clone-1/tree/.git/objects/pack/tmp_pack", Err: syscall.ENOSPC}},
		{"a checkout of a file named EOF", &fs.PathError{
			Op: "open", Path: "/tmp/map-repo-clone-1/tree/EOF", Err: syscall.ENOSPC}},
		{"a checkout of a symlink to `connection reset`", &os.LinkError{
			Op: "symlink", Old: "connection reset", New: "/tmp/map-repo-clone-1/tree/link", Err: syscall.ENOSPC}},
	} {
		t.Run("the spool's disk fails during "+tc.name, func(t *testing.T) {
			err := fmt.Errorf("%w: %w", errClone, tc.err)
			if got := cloneReason(err); got != repoOutcomeInternal {
				t.Errorf("cloneReason = %q, want %q — the disk is ours: %v", got, repoOutcomeInternal, err)
			}
		})
	}

	// A read of the response body fails outside the client's *url.Error, and
	// a timed-out one matches none of the network texts.
	t.Run("a read from the remote times out", func(t *testing.T) {
		err := fmt.Errorf("%w: %w", errClone, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ETIMEDOUT})
		if got := cloneReason(err); got != repoOutcomeNetwork {
			t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeNetwork, err)
		}
	})

	// Over HTTP/2 a stream the remote resets partway is neither type, and
	// only its text says so. The error is net/http's own.
	t.Run("the remote resets the stream", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("0", 4096)))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}))
		srv.EnableHTTP2 = true
		srv.StartTLS()
		defer srv.Close()
		res, err := srv.Client().Get(srv.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
		if res.ProtoMajor != 2 || err == nil {
			t.Fatalf("proto %s, body read err %v: want an HTTP/2 read that fails", res.Proto, err)
		}
		if got := cloneReason(fmt.Errorf("%w: %w", errClone, err)); got != repoOutcomeNetwork {
			t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeNetwork, err)
		}
	})

	// An untrusted certificate is an x509 error inside the client's
	// *url.Error, and matches none of the network texts either.
	t.Run("the remote's certificate is not trusted", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.NotFoundHandler())
		defer srv.Close()
		_, _, cleanup, err := cloneToTar(context.Background(), repoRef{URL: srv.URL + "/o/r.git"}, "ghp_fixture", 1<<30)
		defer cleanup()
		if got := cloneReason(err); err == nil || got != repoOutcomeNetwork {
			t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeNetwork, err)
		}
	})

	// The same refused dial, made by the fetch, is the host's.
	t.Run("the remote refuses the dial", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL + "/o/r.git"
		srv.Close()
		_, _, cleanup, err := cloneToTar(context.Background(), repoRef{URL: url}, "ghp_fixture", 1<<30)
		defer cleanup()
		if got := cloneReason(err); err == nil || got != repoOutcomeNetwork {
			t.Errorf("cloneReason = %q, want %q: %v", got, repoOutcomeNetwork, err)
		}
	})
}

// TestRepoFailedLandingSweepsTheSandbox 🔍: the extraction's cleanup tail runs
// only if the extraction script does. An upload that breaks off, or an
// extraction whose exec never starts, leaves the shipped tar, up to the whole
// byte budget, in the session's /tmp with its context still live. So the
// sweep is owed on every failure from the upload on, not only on cancellation
// (TestRepoCancelledCloneSweepsTheSandbox). It removes the tar alone, without
// -r: a directory at that path is not ours, since the upload refuses to write
// onto one, and the staging directory may be an extraction's still running.
func TestRepoFailedLandingSweepsTheSandbox(t *testing.T) {
	sweepFails := func(req sandbox.ExecRequest) *sandbox.ExecResult {
		if strings.HasPrefix(req.Command, "rm -f ") {
			return &sandbox.ExecResult{ExitCode: 3}
		}
		return nil
	}
	for _, tc := range []struct {
		name      string
		sb        *fakeSandbox
		failed    bool
		sweepFail bool
	}{
		{"upload breaks off", &fakeSandbox{writeErr: errors.New("archive upload: connection reset by peer")}, true, false},
		{"extraction never starts", &fakeSandbox{
			execErr: errors.New("exec create: container is restarting"), execErrOn: "tar -xf"}, true, false},
		{"the sweep exits nonzero", &fakeSandbox{
			writeErr: errors.New("archive upload: connection reset by peer"), execHook: sweepFails}, true, true},
		// The extraction's own tail removed the tar, so a sweep would be one
		// more exec on every clone that landed.
		{"the clone lands", &fakeSandbox{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			h := newHarness(t, &fakeSandbox{})
			h.seedRepoResource(t, "sesrsc_sweep", fx.url(), repoMount, "ghp_fixture", nil)
			logged := captureLogs(t)

			_, err := h.exec.materializeRepo(context.Background(), tc.sb, h.sid, repoRef{
				ID: "sesrsc_sweep", Type: "github_repository", URL: fx.url(), MountPath: repoMount,
			}, "", func() {})
			if tc.failed != (err != nil) {
				t.Fatalf("materializeRepo = %v, want failure %v", err, tc.failed)
			}

			const tar = repoStagingRoot + "/repo-sesrsc_sweep.tar"
			var swept bool
			for _, c := range tc.sb.cmds {
				if c == "rm -f "+shellQuote(tar) {
					swept = true
				}
			}
			if swept != tc.failed {
				t.Errorf("swept = %v, want %v; commands issued: %v", swept, tc.failed, tc.sb.cmds)
			}
			var warned bool
			for _, r := range logged() {
				if r.message == "the failed clone's sandbox tar was not swept" {
					warned = true
				}
			}
			if warned != tc.sweepFail {
				t.Errorf("warned = %v, want %v: a sweep that exits nonzero left the tar", warned, tc.sweepFail)
			}
		})
	}
}
