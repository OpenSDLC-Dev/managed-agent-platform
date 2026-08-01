package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// Docker's writable-layer quota is only as good as the daemon's storage driver
// — some enforce it, some refuse the option, and at least one accepts it and
// enforces nothing — so this backend ignores an EphemeralStorageBytes rather
// than shipping a cap it cannot vouch for: the mirror image of Kubernetes
// ignoring PidsLimit. This row is what keeps it deliberate: the create payload
// for a disk cap must be the payload for no disk cap.
func TestSandboxConfigIgnoresEphemeralStorage(t *testing.T) {
	plain := sandboxConfig(hardenedSpec(sandbox.Hardening{}), "/workspace", "", 0)
	disk := sandboxConfig(hardenedSpec(sandbox.Hardening{EphemeralStorageBytes: 2 << 30}),
		"/workspace", "", 0)
	// The whole payload, not a chosen field: the failure this guards against is
	// the cap leaking into *some* other limit, and naming the fields in advance
	// would be guessing which one.
	want, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal the unhardened payload: %v", err)
	}
	got, err := json.Marshal(disk)
	if err != nil {
		t.Fatalf("marshal the disk-capped payload: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("a disk cap changed the create payload:\n want %s\n  got %s", want, got)
	}
}

// The two rows above exercise sandboxConfig and the warning helper directly,
// which between them leave the one line that matters in production untested:
// Provision's call into the warning. This drives the real Provision against a
// scripted daemon and asserts both halves of the backend's answer to a disk
// cap — the create payload the daemon actually received carries nothing about
// storage, and the operator is told once. Delete the call in Provision and this
// row goes red; the two above stay green.
func TestProvisionIgnoresTheDiskCapAndSaysSoOnce(t *testing.T) {
	logged := captureWarnings(t)

	s := spec()
	s.Hardening = sandbox.Hardening{EphemeralStorageBytes: 2 << 30}
	var created []byte
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/create":
			created, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"Id":"abc"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if _, err := p.Provision(context.Background(), s); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Not a named field: the failure guarded against is the cap reaching the
	// daemon under *some* option, and naming one in advance would be guessing
	// which. StorageOpt is the option Docker would take it under.
	for _, banned := range []string{"StorageOpt", "2147483648"} {
		if bytes.Contains(created, []byte(banned)) {
			t.Errorf("the create payload carries %q: %s", banned, created)
		}
	}
	if n := strings.Count(logged(), "ephemeral storage"); n != 1 {
		t.Errorf("Provision warned %d times about the ignored disk cap, want exactly 1:\n%s", n, logged())
	}
}

// captureWarnings routes WARN-and-above logging into a buffer for one test.
//
// The stdlib-log save/restore is not optional, for the reason internal/worker's
// captureWarnings documents: slog.SetDefault reroutes the standard log package
// into whatever handler it installs, and restoring only slog.Default() does not
// undo that — on the way back the previous handler IS a *defaultHandler, so
// SetDefault's type check skips the log.SetOutput call and log keeps pointing at
// this finished test's handler. Every later log.Print in this package's test
// binary would vanish into it, taking the fake daemon's httptest diagnostics
// (superfluous WriteHeader, handler panics) with them.
func captureWarnings(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf.String
}

// Ignoring it silently is the failure the Kubernetes side already refuses for
// PidsLimit: an operator who capped the sandbox's disk and got nothing should
// be told, once per provider rather than once per sandbox.
func TestWarnsOnceForAnUnenforceableDiskCap(t *testing.T) {
	logged := captureWarnings(t)

	p := &Provider{}
	ctx := context.Background()
	h := sandbox.Hardening{EphemeralStorageBytes: 2 << 30}
	p.warnUnenforceableEphemeralStorage(ctx, h)
	p.warnUnenforceableEphemeralStorage(ctx, h)

	if n := strings.Count(logged(), "ephemeral storage"); n != 1 {
		t.Errorf("logged the unenforceable-disk-cap warning %d times, want exactly 1:\n%s", n, logged())
	}
}

// And says nothing when nothing was asked for: a warning on every unconfigured
// deployment is noise that trains operators to ignore the one that matters.
func TestNoWarningWithoutADiskCap(t *testing.T) {
	logged := captureWarnings(t)

	(&Provider{}).warnUnenforceableEphemeralStorage(context.Background(), sandbox.Hardening{})
	if got := logged(); got != "" {
		t.Errorf("warned without a configured disk cap: %s", got)
	}
}
