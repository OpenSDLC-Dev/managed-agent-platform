package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// fakeDaemon serves a scripted Docker API so the provider's error and race
// paths — a missing image, a lost create race, a daemon that refuses — can be
// exercised deterministically, where the real-daemon contract suite cannot.
func fakeDaemon(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, err := New(Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return p
}

func spec() sandbox.Spec {
	return sandbox.Spec{SessionID: domain.NewID("sesn"), Image: "img:1"}
}

func TestNewResolvesDaemonAddress(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	p, err := New(Config{Host: "unix:///var/run/docker.sock"})
	if err != nil || p.api.base != "http://docker" {
		t.Errorf("unix: base=%q err=%v", p.api.base, err)
	}
	if p, err := New(Config{Host: "tcp://127.0.0.1:2375"}); err != nil || p.api.base != "http://127.0.0.1:2375" {
		t.Errorf("tcp: base=%q err=%v", p.api.base, err)
	}
	if _, err := New(Config{Host: "ssh://nope"}); err == nil {
		t.Error("unsupported address accepted")
	}

	// An empty Host follows DOCKER_HOST before the well-known socket.
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.1:2375")
	if p, err := New(Config{}); err != nil || p.api.base != "http://10.0.0.1:2375" {
		t.Errorf("DOCKER_HOST ignored: base=%q err=%v", p.api.base, err)
	}
}

func TestProvisionValidatesSpec(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call to %s", r.URL.Path)
	})
	if _, err := p.Provision(context.Background(), sandbox.Spec{Image: "img:1"}); err == nil {
		t.Error("provision without a session id accepted")
	}
	if _, err := p.Provision(context.Background(), sandbox.Spec{SessionID: domain.NewID("sesn")}); err == nil {
		t.Error("provision without an image accepted")
	}
}

func TestProvisionReusesRunningContainer(t *testing.T) {
	var created bool
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Id":"abc","State":{"Running":true}}`)
		case r.URL.Path == "/containers/create":
			created = true
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	sb, err := p.Provision(context.Background(), spec())
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if created {
		t.Error("a running container was re-created")
	}
	if sb.ID() != "abc" {
		t.Errorf("id = %q", sb.ID())
	}
}

func TestProvisionStartsStoppedContainer(t *testing.T) {
	var started string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Id":"abc","State":{"Running":false}}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			started = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if _, err := p.Provision(context.Background(), spec()); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if started != "/containers/abc/start" {
		t.Errorf("stopped container not started (started=%q)", started)
	}
}

// A create that 404s means the image is not on this host: pull, then retry.
func TestProvisionPullsMissingImage(t *testing.T) {
	var creates, pulls int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such container: map-x"}`)
		case r.URL.Path == "/containers/create":
			creates++
			if creates == 1 {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"message":"No such image: img:1"}`)
				return
			}
			io.WriteString(w, `{"Id":"new"}`)
		case r.URL.Path == "/images/create":
			pulls++
			if got := r.URL.Query().Get("fromImage"); got != "img" {
				t.Errorf("fromImage = %q", got)
			}
			if got := r.URL.Query().Get("tag"); got != "1" {
				t.Errorf("tag = %q", got)
			}
			io.WriteString(w, `{"status":"Pulling"}`+"\n"+`{"status":"Done"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	sb, err := p.Provision(context.Background(), spec())
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if pulls != 1 || creates != 2 || sb.ID() != "new" {
		t.Errorf("pulls=%d creates=%d id=%q", pulls, creates, sb.ID())
	}
}

// A pull failure arrives inside a 200 stream; ignoring it would surface as a
// confusing second create failure.
func TestProvisionSurfacesPullError(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such container"}`)
		case r.URL.Path == "/containers/create":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such image"}`)
		case r.URL.Path == "/images/create":
			io.WriteString(w, `{"error":"denied: requires authentication"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	_, err := p.Provision(context.Background(), spec())
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("err = %v, want the pull's own error", err)
	}
}

// Two executors provisioning one session: the create loser adopts the winner's
// container instead of failing the tool call.
func TestProvisionAdoptsRaceWinner(t *testing.T) {
	var inspects int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			inspects++
			if inspects == 1 {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"message":"No such container"}`)
				return
			}
			io.WriteString(w, `{"Id":"winner","State":{"Running":true}}`)
		case r.URL.Path == "/containers/create":
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"message":"Conflict. The container name is already in use"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	sb, err := p.Provision(context.Background(), spec())
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "winner" {
		t.Errorf("id = %q, want the winner's container", sb.ID())
	}
}

func TestProvisionPropagatesDaemonFailure(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"daemon is unwell"}`)
	})
	_, err := p.Provision(context.Background(), spec())
	if err == nil || !strings.Contains(err.Error(), "daemon is unwell") {
		t.Errorf("err = %v", err)
	}
	// A non-JSON body still yields the daemon's text rather than an empty error.
	p = fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "proxy exploded")
	})
	_, err = p.Provision(context.Background(), spec())
	if err == nil || !strings.Contains(err.Error(), "proxy exploded") {
		t.Errorf("err = %v", err)
	}
}

// `limited` fails closed: no network at all until the egress proxy lands.
func TestNetworkModeFailsClosed(t *testing.T) {
	if got := networkMode(domain.Networking{Type: domain.NetLimited}); got != "none" {
		t.Errorf("limited → %q, want none", got)
	}
	if got := networkMode(domain.Networking{Type: domain.NetUnrestricted}); got != "bridge" {
		t.Errorf("unrestricted → %q, want bridge", got)
	}
	// An unset networking type is not a licence to open the network... but the
	// wire default IS unrestricted, so it must stay bridge and say so here.
	if got := networkMode(domain.Networking{}); got != "bridge" {
		t.Errorf("zero networking → %q, want bridge (the wire default)", got)
	}
}

func TestDestroyIsIdempotentAndSurfacesRealFailures(t *testing.T) {
	c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container: gone"}`)
	}).attach("gone", "/workspace")
	if err := c.Destroy(context.Background()); err != nil {
		t.Errorf("destroy of a missing container: %v, want nil", err)
	}

	c = fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"removal in progress"}`)
	}).attach("busy", "/workspace")
	if err := c.Destroy(context.Background()); err == nil {
		t.Error("a failed removal reported success")
	}
}

// A destroyed sandbox must report ErrNotFound, not a raw HTTP error, so the
// executor can fail one tool call instead of the session.
func TestGoneContainerMapsToErrNotFound(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container: gone"}`)
	})
	c := p.attach("gone", "/workspace")
	if _, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "true"}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("exec: %v, want ErrNotFound", err)
	}
	if _, err := c.ReadFile(context.Background(), "/workspace/x"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("read: %v, want ErrNotFound", err)
	}
	if err := c.WriteFile(context.Background(), "/workspace/x", nil); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("write: %v, want ErrNotFound", err)
	}
}

// The daemon publishes an exec's code a moment after its output closes.
func TestExecWaitsForTheExitCode(t *testing.T) {
	var inspects int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			w.Write(frame(1, "hi\n"))
		case r.URL.Path == "/exec/e1/json":
			inspects++
			if inspects < 3 {
				io.WriteString(w, `{"Running":true}`)
				return
			}
			io.WriteString(w, `{"Running":false,"ExitCode":9}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace")
	res, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "echo hi"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Stdout != "hi\n" || res.ExitCode != 9 || inspects != 3 {
		t.Errorf("res=%+v inspects=%d", res, inspects)
	}
}

// TimedOut needs both the watchdog's signal and a deadline that actually
// passed — and the deadline that passed is the watchdog's own, which is the
// caller's request rounded up to whole seconds.
func TestTimedOutNeedsTheWatchdogsDeadlineNotTheCallers(t *testing.T) {
	// The daemon answers execStart at once and holds the stream open while the
	// command runs, as a real one does; the delay is the command's runtime.
	newFake := func(delay time.Duration, code int) *container {
		p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/exec"):
				io.WriteString(w, `{"Id":"e1"}`)
			case r.URL.Path == "/exec/e1/start":
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				time.Sleep(delay)
			case r.URL.Path == "/exec/e1/json":
				fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, code)
			}
		})
		return p.attach("abc", "/workspace")
	}

	// A self-inflicted SIGKILL well inside the deadline is not a timeout.
	res, err := newFake(0, sigkillExit).Exec(context.Background(),
		sandbox.ExecRequest{Command: "kill -9 $$", Timeout: time.Hour})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.TimedOut {
		t.Error("a SIGKILL well inside the deadline read as a timeout")
	}

	// With no deadline at all, 137 is just an exit code.
	if res, err := newFake(0, sigkillExit).Exec(context.Background(),
		sandbox.ExecRequest{Command: "kill -9 $$"}); err != nil || res.TimedOut {
		t.Errorf("res=%+v err=%v", res, err)
	}

	// The watchdog can only sleep whole seconds. A 1.1s request makes it sleep
	// 2s, so a SIGKILL at 1.2s did not come from it — comparing against the
	// caller's 1.1s would call this a timeout that never happened.
	res, err = newFake(1200*time.Millisecond, sigkillExit).Exec(context.Background(),
		sandbox.ExecRequest{Command: "kill -9 $$", Timeout: 1100 * time.Millisecond})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.TimedOut {
		t.Error("a SIGKILL before the watchdog's rounded-up deadline read as a timeout")
	}

	// Past the watchdog's own deadline, it is a timeout.
	res, err = newFake(1200*time.Millisecond, sigkillExit).Exec(context.Background(),
		sandbox.ExecRequest{Command: "sleep 300", Timeout: time.Second})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.TimedOut {
		t.Error("a SIGKILL past the deadline did not read as a timeout")
	}

	// A command that drifts a hair past the deadline and exits on its own is
	// not accused of anything: that much is the sandbox's own measurement noise.
	res, err = newFake(1100*time.Millisecond, 0).Exec(context.Background(),
		sandbox.ExecRequest{Command: "echo hi", Timeout: time.Second})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.TimedOut {
		t.Error("a command finishing within the slop read as a timeout")
	}
}

// The bypass that survived the first fix: kill the watchdog, overrun the
// deadline, then exit before Exec's own bound fires and report success. On the
// honest path a command cannot outlive its deadline and still choose its exit
// code — the watchdog would have killed it — so that is a timeout whatever it
// claims, whatever code it picks.
func TestOverrunningTheDeadlineIsATimeoutWhateverTheCommandClaims(t *testing.T) {
	for _, code := range []int{0, 124, 1} {
		p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/exec"):
				io.WriteString(w, `{"Id":"e1"}`)
			case r.URL.Path == "/exec/e1/start":
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				time.Sleep(1500 * time.Millisecond) // deadline 1s, bound 1s+3s
			case r.URL.Path == "/exec/e1/json":
				fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, code)
			}
		})
		c := p.attach("abc", "/workspace")
		c.killGrace, c.overrunSlop = 3*time.Second, 200*time.Millisecond

		res, err := c.Exec(context.Background(), sandbox.ExecRequest{
			Command: "kill the watchdog; sleep 2; exit " + strconv.Itoa(code), Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if !res.TimedOut {
			t.Errorf("a command that outran its deadline and exited %d hid the timeout: %+v", code, res)
		}
		if res.ExitCode != code {
			t.Errorf("exit code = %d, want the command's own %d", res.ExitCode, code)
		}
	}
}

// The wrapper must keep no state anywhere the agent's own commands can reach.
// A marker file under /tmp — the first design — let a command forge a timeout
// it never hit, or erase one it did.
func TestExecWrapperKeepsNoStateInsideTheContainer(t *testing.T) {
	for _, writable := range []string{"/tmp", "/var/tmp", "/dev/shm", "/run", "/workspace"} {
		if strings.Contains(execWrapper, writable) {
			t.Errorf("the exec wrapper touches %s, which the sandboxed command can write", writable)
		}
	}
	if !strings.Contains(execWrapper, "set -m") {
		t.Error("the wrapper must enable job control so the deadline kills the command's process group")
	}
	// Silencing the whole wrapper would swallow its own failures — a fork that
	// hits RLIMIT_NPROC, a missing shell — and return them as an empty stderr.
	if strings.Contains(execWrapper, "exec 3>&2") || strings.Contains(execWrapper, "exec 2>/dev/null") {
		t.Error("the wrapper redirects its own stderr wholesale; only `wait` needs silencing")
	}
}

// The in-container watchdog is a process beside the command, so the command can
// kill it. The deadline must therefore be enforced outside the container too:
// once its own bound passes, Exec stops waiting and calls the timeout itself.
func TestExecStopsWaitingWhenTheSandboxsWatchdogDoesNot(t *testing.T) {
	held := make(chan struct{})
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			w.Write(frame(1, "partial output"))
			w.(http.Flusher).Flush()
			<-held // the command killed its watchdog; nothing ever closes this
		case r.URL.Path == "/exec/e1/json":
			t.Error("a command that never finished must not be asked for an exit code")
		}
	})
	defer close(held)

	c := p.attach("abc", "/workspace")
	c.killGrace = 200 * time.Millisecond

	start := time.Now()
	res, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "kill the guard; sleep 300", Timeout: time.Second})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.TimedOut || res.ExitCode != sigkillExit {
		t.Errorf("result = %+v, want a timeout", res)
	}
	if res.Stdout != "partial output" {
		t.Errorf("stdout = %q — output that did arrive must survive the timeout", res.Stdout)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Exec waited %s past a 1s deadline", elapsed)
	}
}

// The caller's own cancellation is not a timeout — it is the caller's error,
// and reporting it as a clean "the command timed out" would hide a shutdown.
// The stream must already be open when the caller gives up, so that the
// cancellation lands where a sandbox deadline would: mid-read.
func TestCallerCancellationIsNotATimeout(t *testing.T) {
	held := make(chan struct{})
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			w.Write(frame(1, "started"))
			w.(http.Flusher).Flush()
			<-held // the command is still running when the caller walks away
		case r.URL.Path == "/exec/e1/json":
			t.Error("a cancelled call must not go on to ask for an exit code")
		}
	})
	defer close(held)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c := p.attach("abc", "/workspace")
	// A generous sandbox deadline, so only the caller's context can fire.
	_, err := c.Exec(ctx, sandbox.ExecRequest{Command: "sleep 300", Timeout: time.Hour})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the caller's context error", err)
	}
}

// A 404 whose message merely mentions a container is not a missing container:
// the archive endpoints echo the requested path, and the path is the agent's.
func TestPathProseCannotFakeAMissingSandbox(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		// Verbatim from a real daemon, for a file literally named
		// "No such container".
		io.WriteString(w, `{"message":"Could not find the file /workspace/No such container/f in container abc"}`)
	})
	c := p.attach("abc", "/workspace")
	_, err := c.ReadFile(context.Background(), "/workspace/No such container/f")
	if !errors.Is(err, sandbox.ErrFileNotExist) {
		t.Errorf("read: %v, want ErrFileNotExist", err)
	}
}

// The exec endpoints are keyed by exec id, so they have a 404 of their own.
// A lost exec is not a lost sandbox, and telling the executor otherwise would
// have it tear down a live session's container.
func TestStaleExecIsNotAMissingSandbox(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			io.WriteString(w, `{"Id":"e1"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such exec instance: e1"}`)
	})
	c := p.attach("abc", "/workspace")
	_, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "true"})
	if err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("exec: %v, want the daemon's own error", err)
	}
	if !strings.Contains(err.Error(), "No such exec instance") {
		t.Errorf("exec: %v", err)
	}
}

func TestWriteFileCreatesParentsOnlyWhenNeeded(t *testing.T) {
	var puts, execs int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			puts++
			if puts == 1 {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"message":"no such directory"}`)
				return
			}
			if got := r.URL.Query().Get("path"); got != "/workspace/a/b" {
				t.Errorf("archive path = %q", got)
			}
		case strings.HasSuffix(r.URL.Path, "/exec"):
			execs++
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace")
	if err := c.WriteFile(context.Background(), "/workspace/a/b/f.txt", []byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if puts != 2 || execs != 1 {
		t.Errorf("puts=%d execs=%d — want one failed put, one mkdir, one retry", puts, execs)
	}
}

// A path that still 404s after its parents exist is a bad path, not a missing
// sandbox: reporting ErrNotFound would send the executor after the wrong fault.
func TestWriteFileKeepsPathFailuresDistinctFromAMissingSandbox(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"not a directory"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		}
	})
	c := p.attach("abc", "/workspace")
	err := c.WriteFile(context.Background(), "/workspace/a/f.txt", []byte("x"))
	if err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v, want the daemon's path error", err)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v", err)
	}
}

func TestWriteFileSurfacesMkdirFailure(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no such directory"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			w.Write(frame(2, "Read-only file system\n"))
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":1}`)
		}
	})
	c := p.attach("abc", "/workspace")
	err := c.WriteFile(context.Background(), "/workspace/a/f.txt", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "Read-only file system") {
		t.Errorf("err = %v, want the mkdir's stderr", err)
	}
}

// The unix transport is the production path; tcp is only how these tests
// reach a fake. Dial a real unix socket so the dialer itself is exercised.
func TestUnixTransportDialsTheSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"Id":"over-unix","State":{"Running":true}}`)
		})},
	}
	srv.Start()
	defer srv.Close()

	p, err := New(Config{Host: "unix://" + socket})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	sb, err := p.Provision(context.Background(), spec())
	if err != nil {
		t.Fatalf("provision over unix socket: %v", err)
	}
	if sb.ID() != "over-unix" {
		t.Errorf("id = %q", sb.ID())
	}
}

func TestUnreachableDaemonIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close() // nothing is listening now
	p, err := New(Config{Host: "tcp://" + addr})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := p.Provision(context.Background(), spec()); err == nil {
		t.Error("provision against a dead daemon reported success")
	}
}

// A reply that is not the JSON we asked for must fail loudly rather than
// leave a zero-valued container id in play.
func TestGarbledDaemonRepliesFail(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html>not docker</html>`)
	})
	if _, err := p.Provision(context.Background(), spec()); err == nil {
		t.Error("a non-JSON inspect reply was accepted")
	}

	// Same for a create reply, reached once inspect says "no such container".
	p = fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such container"}`)
			return
		}
		io.WriteString(w, `<html>not docker</html>`)
	})
	if _, err := p.Provision(context.Background(), spec()); err == nil {
		t.Error("a non-JSON create reply was accepted")
	}

	// And for a pull stream, whose failures arrive inside a 200.
	p = fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"), r.URL.Path == "/containers/create":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such thing"}`)
		default:
			io.WriteString(w, `{"status":`)
		}
	})
	if _, err := p.Provision(context.Background(), spec()); err == nil {
		t.Error("a truncated pull stream was accepted")
	}
}

func TestProvisionSurfacesStartFailure(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Id":"abc","State":{"Running":false}}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"cannot start"}`)
		}
	})
	if _, err := p.Provision(context.Background(), spec()); err == nil ||
		!strings.Contains(err.Error(), "cannot start") {
		t.Errorf("err = %v", err)
	}
}

func TestProvisionDefaultsTheWorkdir(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Id":"abc","State":{"Running":true}}`)
	})
	sb, err := p.Provision(context.Background(), sandbox.Spec{
		SessionID: domain.NewID("sesn"), Image: "img:1", // no Workdir
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := sb.(*container).workdir; got != defaultWorkdir {
		t.Errorf("workdir = %q, want %q", got, defaultWorkdir)
	}
}

func TestExecSurfacesStartAndInspectFailures(t *testing.T) {
	failing := func(path string) *container {
		p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == path {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"message":"daemon said no"}`)
				return
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/exec"):
				io.WriteString(w, `{"Id":"e1"}`)
			case r.URL.Path == "/exec/e1/json":
				io.WriteString(w, `{"Running":false,"ExitCode":0}`)
			}
		})
		return p.attach("abc", "/workspace")
	}
	for _, path := range []string{"/exec/e1/start", "/exec/e1/json"} {
		_, err := failing(path).Exec(context.Background(), sandbox.ExecRequest{Command: "true"})
		if err == nil || !strings.Contains(err.Error(), "daemon said no") {
			t.Errorf("%s: err = %v", path, err)
		}
	}
}

// An exec whose output closed but which the daemon still calls running is a
// stuck exec, not an exit code of zero.
func TestExecRefusesToInventAnExitCode(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":true}`)
		}
	})
	c := p.attach("abc", "/workspace")
	c.exitBudget = 200 * time.Millisecond
	if _, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "true"}); err == nil ||
		!strings.Contains(err.Error(), "still running") {
		t.Errorf("err = %v, want a stuck-exec error", err)
	}

	// A caller that gives up mid-poll gets its own cancellation back.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Exec(ctx, sandbox.ExecRequest{Command: "true"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's error", err)
	}
}

// The whole point, against a real daemon: a command that finds and kills the
// watchdog guarding it still cannot outrun its deadline, and still cannot
// report success. Both bypasses reviewers demonstrated are covered — running
// long past the deadline (Exec's own bound catches it) and slipping just past
// it before Exec stops waiting (the overrun does).
func TestRealSandboxDeadlineSurvivesTheCommandKillingItsWatchdog(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("this test requires Docker: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID: domain.NewID("sesn"), Image: "debian:stable-slim", Workdir: "/workspace",
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })

	// The watchdog is the command's sibling: same parent, different pid.
	killTheWatchdog := `
	  for p in $(cat /proc/$PPID/task/$PPID/children 2>/dev/null); do
	    [ "$p" != "$$" ] && kill -9 -"$p" 2>/dev/null
	  done
	`
	for _, tc := range []struct{ name, after string }{
		{"runs far past the deadline", `sleep 300`},
		{"slips past it and exits clean", `sleep 2; exit 0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
				Command: killTheWatchdog + tc.after, Timeout: time.Second,
			})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if !res.TimedOut {
				t.Errorf("a command that killed its watchdog hid the timeout: %+v", res)
			}
			if elapsed > 8*time.Second {
				t.Errorf("the command outran its 1s deadline by %s", elapsed)
			}
		})
	}
}

func tarball(t *testing.T, header *tar.Header, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatal(err)
	}
	// Close is deliberately skipped: some cases declare a size they never
	// write, and the header block is all the reader under test gets to see.
	return buf.Bytes()
}

func TestReadFileRejectsWhatItCannotReturn(t *testing.T) {
	serve := func(archive []byte) *container {
		p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
		return p.attach("abc", "/workspace")
	}

	// A symlink carries no contents; returning its (empty) body as the file
	// would silently hand the agent the wrong answer.
	link := tarball(t, &tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, "")
	if _, err := serve(link).ReadFile(context.Background(), "/workspace/l"); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("symlink: err = %v", err)
	}

	// The header's size is the allocation, so it is what must be checked.
	big := tarball(t, &tar.Header{
		Name: "big", Typeflag: tar.TypeReg, Size: sandbox.MaxFileBytes + 1,
	}, "")
	if _, err := serve(big).ReadFile(context.Background(), "/workspace/big"); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("oversize: err = %v, want ErrFileTooLarge", err)
	}

	// An archive that ends early must not read back as a short file.
	if _, err := serve(nil).ReadFile(context.Background(), "/workspace/x"); err == nil {
		t.Error("an empty archive read back as a file")
	}
	cut := tarball(t, &tar.Header{Name: "f", Typeflag: tar.TypeReg, Size: 100}, strings.Repeat("z", 100))
	if _, err := serve(cut[:512+40]).ReadFile(context.Background(), "/workspace/f"); err == nil {
		t.Error("a truncated file body read back as a whole file")
	}
}

func TestWrapLeavesNonSandboxFailuresAlone(t *testing.T) {
	c := &container{id: "abc"}
	if err := c.wrap(nil); err != nil {
		t.Errorf("wrap(nil) = %v", err)
	}
	original := &apiError{Status: 500, Message: "boom"}
	if err := c.wrap(original); !errors.Is(err, original) {
		t.Errorf("wrap rewrote a non-404: %v", err)
	}
	if err := c.wrap(&apiError{Status: 404, Message: "No such container: abc"}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("wrap(404) = %v, want ErrNotFound", err)
	}
}

func TestSplitImageRef(t *testing.T) {
	for _, tc := range []struct{ ref, name, tag string }{
		{"debian:stable-slim", "debian", "stable-slim"},
		{"debian", "debian", "latest"},
		{"registry.io:5000/team/img", "registry.io:5000/team/img", "latest"},
		{"registry.io:5000/team/img:v2", "registry.io:5000/team/img", "v2"},
		{"img@sha256:abc", "img@sha256:abc", ""},
	} {
		name, tag := splitImageRef(tc.ref)
		if name != tc.name || tag != tc.tag {
			t.Errorf("splitImageRef(%q) = %q, %q; want %q, %q", tc.ref, name, tag, tc.name, tc.tag)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/workspace":     `'/workspace'`,
		"/a b":           `'/a b'`,
		"/it's":          `'/it'\''s'`,
		"/x; rm -rf /":   `'/x; rm -rf /'`,
		"/$(whoami)/dir": `'/$(whoami)/dir'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func frame(stream byte, payload string) []byte {
	b := make([]byte, 8+len(payload))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}

func TestDemuxSplitsStreams(t *testing.T) {
	raw := bytes.Join([][]byte{
		frame(1, "out1"), frame(2, "err1"), frame(1, "out2"),
	}, nil)
	stdout, stderr, truncated, err := demux(bytes.NewReader(raw), 1024)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if string(stdout) != "out1out2" || string(stderr) != "err1" || truncated {
		t.Errorf("stdout=%q stderr=%q truncated=%v", stdout, stderr, truncated)
	}
}

// Past the cap the payload is drained, not buffered — the command must be free
// to finish, and later frames on the other stream must still arrive.
func TestDemuxCapsEachStreamAndKeepsReading(t *testing.T) {
	raw := bytes.Join([][]byte{
		frame(1, strings.Repeat("a", 10)), frame(1, strings.Repeat("b", 10)), frame(2, "kept"),
	}, nil)
	stdout, stderr, truncated, err := demux(bytes.NewReader(raw), 4)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if string(stdout) != "aaaa" || !truncated {
		t.Errorf("stdout=%q truncated=%v", stdout, truncated)
	}
	if string(stderr) != "kept" {
		t.Errorf("stderr=%q — capping stdout lost the other stream", stderr)
	}
}

func TestDemuxRejectsTruncatedFrame(t *testing.T) {
	raw := frame(1, "hello")[:9] // header promises 5 bytes, one arrives
	if _, _, _, err := demux(bytes.NewReader(raw), 1024); err == nil {
		t.Error("a truncated frame decoded cleanly")
	}
	// A header cut in half is equally not a clean end of stream.
	if _, _, _, err := demux(bytes.NewReader(frame(1, "x")[:3]), 1024); err == nil {
		t.Error("a truncated header decoded cleanly")
	}
}

// Frame id 3 is the daemon talking about the exec, not the command talking.
// Folding it into stdout would hand the model a tool result assembled out of
// an infrastructure failure.
func TestDemuxSurfacesTheDaemonsOwnErrorFrame(t *testing.T) {
	raw := bytes.Join([][]byte{frame(1, "partial"), frame(3, "OCI runtime exec failed")}, nil)
	stdout, _, _, err := demux(bytes.NewReader(raw), 1024)
	if err == nil || !strings.Contains(err.Error(), "OCI runtime exec failed") {
		t.Errorf("err = %v, want the daemon's reason", err)
	}
	if string(stdout) != "partial" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestDemuxRejectsUnknownStreamID(t *testing.T) {
	if _, _, _, err := demux(bytes.NewReader(frame(7, "?")), 1024); err == nil {
		t.Error("an unknown stream id was silently accepted as output")
	}
	// Id 0 is stdin; it never travels back and must not be read as stdout.
	if _, _, _, err := demux(bytes.NewReader(frame(0, "?")), 1024); err == nil {
		t.Error("a stdin frame was silently accepted as output")
	}
}
