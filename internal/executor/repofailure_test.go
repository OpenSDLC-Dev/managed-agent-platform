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
	"strings"
	"syscall"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// cipherThatFails stands in for a cipher backend that cannot answer: a key
// service the executor cannot reach, or one that drops the connection.
type cipherThatFails struct {
	secrets.Cipher
	err error
}

func (c cipherThatFails) Decrypt(context.Context, []byte, string) ([]byte, error) {
	return nil, c.err
}

// TestCloneReasonBlamesTheRemoteOnlyForTheFetch 🔍: auth, not_found, checkout
// and network name the git host, and only the fetch speaks to it. What
// surrounds the fetch is this platform's own: the credential row in Postgres,
// the token's decrypt (a key service, over the network), the spool on the
// executor's disk. Their errors read like the host's — a refused dial, an
// unexpected EOF, a *fs.PathError, which has a Timeout method — and were
// reported as `network`, sending the operator to the git host over a fault in
// their own database or key service.
func TestCloneReasonBlamesTheRemoteOnlyForTheFetch(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"key service refuses the dial", refused},
		{"key service drops the connection", io.ErrUnexpectedEOF},
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

	// A write to the spool happens inside the fetch, so the phase alone does
	// not decide it: the error's type must not read as the network's either.
	t.Run("the spool's disk fails during the fetch", func(t *testing.T) {
		err := fmt.Errorf("%w: %w", errFetch, &fs.PathError{
			Op: "write", Path: "/tmp/map-repo-clone-1/tree/.git/objects/pack/tmp_pack", Err: syscall.ENOSPC})
		if got := cloneReason(err); got != repoOutcomeInternal {
			t.Errorf("cloneReason = %q, want %q — the disk is ours: %v", got, repoOutcomeInternal, err)
		}
	})

	// A read of the response body fails outside the client's *url.Error, and
	// a timed-out one matches none of the network texts.
	t.Run("a read from the remote times out", func(t *testing.T) {
		err := fmt.Errorf("%w: %w", errFetch, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ETIMEDOUT})
		if got := cloneReason(err); got != repoOutcomeNetwork {
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
// (TestRepoCancelledCloneSweepsTheSandbox).
func TestRepoFailedLandingSweepsTheSandbox(t *testing.T) {
	for _, tc := range []struct {
		name     string
		writeErr error
		execErr  func(cmd string) error
	}{
		{"upload breaks off", errors.New("archive upload: connection reset by peer"), nil},
		{"extraction never starts", nil, func(cmd string) error {
			if strings.Contains(cmd, "tar -xf") {
				return errors.New("exec create: container is restarting")
			}
			return nil
		}},
		// The extraction's own tail removed the tar, so a sweep would be one
		// more exec on every clone that landed.
		{"the clone lands", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGitFixture(t, map[string]string{"README.md": "x\n"})
			sb := &sweepSandbox{fakeSandbox: &fakeSandbox{}, writeErr: tc.writeErr, execErr: tc.execErr}
			h := newHarness(t, &fakeSandbox{})
			h.seedRepoResource(t, "sesrsc_sweep", fx.url(), repoMount, "ghp_fixture", nil)

			_, err := h.exec.materializeRepo(context.Background(), sb, h.sid, repoRef{
				ID: "sesrsc_sweep", Type: "github_repository", URL: fx.url(), MountPath: repoMount,
			}, "", func() {})
			failed := tc.writeErr != nil || tc.execErr != nil
			if failed != (err != nil) {
				t.Fatalf("materializeRepo = %v, want failure %v", err, failed)
			}

			// The sweep names the tar first; the extraction names its staging
			// directory first and the tar later.
			const tar = repoStagingRoot + "/repo-sesrsc_sweep.tar"
			var swept bool
			for _, c := range sb.issued() {
				if strings.HasPrefix(c, "rm -rf "+shellQuote(tar)) {
					swept = true
				}
			}
			if swept != failed {
				t.Errorf("swept = %v, want %v; commands issued: %v", swept, failed, sb.issued())
			}
		})
	}
}
