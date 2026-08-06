package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// inspectJSON is what the daemon says about a container this platform created
// for s: the ownership label is what Provision checks before adopting it, and
// the fixed-at-create configuration (network mode, image, workdir) is what the
// adoption's spec check compares (#29).
func inspectJSON(id string, s sandbox.Spec, running bool) string {
	workdir := s.Workdir
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	return fmt.Sprintf(`{"Id":%q,"State":{"Running":%t},"Config":{"Labels":{%q:%q},"Image":%q,"WorkingDir":%q},"HostConfig":{"NetworkMode":%q}}`,
		id, running, sessionLabel, string(s.SessionID), s.Image, workdir, networkMode(s.Networking))
}

// fakeExec describes an exec the way a real daemon runs one: the exec's process
// and its output stream have separate lifetimes. A process the command
// backgrounds inherits the stream and holds it open after the command is dead,
// so streamFor may exceed aliveFor — and everything Exec concludes about the
// deadline has to come from the process, never from the stream.
type fakeExec struct {
	aliveFor   time.Duration // how long the exec's own process lives
	streamFor  time.Duration // how long its output stream stays open
	holdStream bool          // ignore streamFor; never close it
	code       int
	stdout     string
	inspects   *int          // optional: counts /exec/{id}/json calls
	topDelay   time.Duration // how long the daemon takes to answer /top
}

const fakeExecPid = 4242

// execDaemon serves the endpoints Exec uses, with fe's timings.
func execDaemon(t *testing.T, fe fakeExec) *container {
	t.Helper()
	if fe.streamFor == 0 {
		fe.streamFor = fe.aliveFor
	}
	held := make(chan struct{})

	var mu sync.Mutex
	var startedAt time.Time
	// ran reports how long the exec has been going, and whether it started.
	ran := func() (time.Duration, bool) {
		mu.Lock()
		defer mu.Unlock()
		if startedAt.IsZero() {
			return 0, false
		}
		return time.Since(startedAt), true
	}

	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)

		case r.URL.Path == "/exec/e1/start":
			mu.Lock()
			startedAt = time.Now()
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			if fe.stdout != "" {
				w.Write(frame(streamStdout, fe.stdout))
			}
			w.(http.Flusher).Flush()
			if fe.holdStream {
				<-held
				return
			}
			time.Sleep(fe.streamFor)

		case r.URL.Path == "/exec/e1/json":
			if fe.inspects != nil {
				*fe.inspects++
			}
			// Running tracks the stream, not the process — the real daemon's
			// quirk, and the reason Exec may not ask it about the deadline.
			elapsed, started := ran()
			running := !started || fe.holdStream || elapsed < fe.streamFor
			fmt.Fprintf(w, `{"Running":%t,"ExitCode":%d,"Pid":%d}`, running, fe.code, fakeExecPid)

		case strings.HasSuffix(r.URL.Path, "/top"):
			// A loaded daemon: `top` forks `ps` on the host, so the answer can
			// arrive well after the request did — or never, if the caller gives
			// up first.
			if fe.topDelay > 0 {
				select {
				case <-time.After(fe.topDelay):
				case <-r.Context().Done():
					return
				}
			}
			elapsed, started := ran()
			alive := !started || elapsed < fe.aliveFor
			rows := `["1","/sbin/docker-init"]`
			if alive {
				rows += fmt.Sprintf(`,["%d","bash"]`, fakeExecPid)
			}
			fmt.Fprintf(w, `{"Titles":["PID","COMMAND"],"Processes":[%s]}`, rows)

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	// Registered after fakeDaemon's, so it runs before it: cleanups are LIFO,
	// and the test server will not shut down while a handler is still held.
	t.Cleanup(func() { close(held) })
	return p.attach("abc", "/workspace", "")
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
	s := spec()
	var created bool
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, inspectJSON("abc", s, true))
		case r.URL.Path == "/containers/create":
			created = true
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	sb, err := p.Provision(context.Background(), s)
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
	s := spec()
	var started string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, inspectJSON("abc", s, false))
		case strings.HasSuffix(r.URL.Path, "/start"):
			started = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if _, err := p.Provision(context.Background(), s); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if started != "/containers/abc/start" {
		t.Errorf("stopped container not started (started=%q)", started)
	}
}

// The container name is derived from the session id, so anything on the daemon
// can hold it. Only the ownership label says the platform built it — and with
// it, that the network mode baked in at create time is the one this session
// asked for. A `limited` session must not adopt a `bridge` container.
func TestProvisionRefusesAContainerItDoesNotOwn(t *testing.T) {
	for _, tc := range []struct{ name, labels string }{
		{"no labels at all", `{}`},
		{"null labels", `null`},
		{"another session's sandbox", `{"` + sessionLabel + `":"sesn_someone_else"}`},
		{"the label under a different key", `{"session-id":"whatever"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var touched []string
			p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				touched = append(touched, r.URL.Path)
				switch {
				case strings.HasSuffix(r.URL.Path, "/json"):
					fmt.Fprintf(w, `{"Id":"squatter","State":{"Running":false},"Config":{"Labels":%s}}`, tc.labels)
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			})
			_, err := p.Provision(context.Background(), spec())
			if err == nil {
				t.Fatal("adopted a container the platform does not own")
			}
			if !strings.Contains(err.Error(), "not this platform's sandbox") {
				t.Errorf("err = %v", err)
			}
			for _, path := range touched {
				if strings.HasSuffix(path, "/start") {
					t.Error("a container the platform does not own was started")
				}
			}
		})
	}
}

// The ownership label says the platform created the container for this session;
// it does not say the container was created from the spec this call asks for.
// Networking, image and workdir are fixed at create, so an owned container that
// mismatches any of them is refused — sandbox.ErrSpecMismatch, before it is
// started and with nothing run in it — rather than silently adopted with the
// wrong containment, and it is not removed: replacement is an explicit
// lifecycle the platform does not have (#29). The handler serves only the
// inspect, so any start, create, remove, or exec would fail the test.
func TestProvisionRefusesAdoptingAMismatchedContainer(t *testing.T) {
	for name, change := range map[string]func(*sandbox.Spec){
		"network mode": func(s *sandbox.Spec) { s.Networking = domain.Networking{Type: domain.NetLimited} },
		"image":        func(s *sandbox.Spec) { s.Image = "img:2" },
		"workdir":      func(s *sandbox.Spec) { s.Workdir = "/elsewhere" },
	} {
		t.Run(name, func(t *testing.T) {
			created := spec() // what the existing container was built from
			requested := created
			change(&requested)
			p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/json"):
					io.WriteString(w, inspectJSON("abc", created, false))
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
			})
			if _, err := p.Provision(context.Background(), requested); !errors.Is(err, sandbox.ErrSpecMismatch) {
				t.Fatalf("err = %v, want sandbox.ErrSpecMismatch", err)
			}
		})
	}
}

// The mismatch check must not refuse the container that does match: a limited
// session's own `none` container is adopted, exactly as a default session's
// `bridge` one is (#29).
func TestProvisionAdoptsAMatchingLimitedContainer(t *testing.T) {
	s := spec()
	s.Networking = domain.Networking{Type: domain.NetLimited}
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, inspectJSON("abc", s, true))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "abc" {
		t.Errorf("id = %q", sb.ID())
	}
}

// The create race has its own adoption path, and the winner is only presumed to
// be a peer executor. Check the label there too.
func TestProvisionRefusesToAdoptAnUnownedRaceWinner(t *testing.T) {
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
			io.WriteString(w, `{"Id":"squatter","State":{"Running":true},"Config":{"Labels":{}}}`)
		case r.URL.Path == "/containers/create":
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"message":"Conflict. The container name is already in use"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			t.Error("an unowned race winner was started")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if _, err := p.Provision(context.Background(), spec()); err == nil ||
		!strings.Contains(err.Error(), "not this platform's sandbox") {
		t.Errorf("err = %v, want a refusal to adopt an unowned race winner", err)
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
	s := spec()
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
			io.WriteString(w, inspectJSON("winner", s, true))
		case r.URL.Path == "/containers/create":
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"message":"Conflict. The container name is already in use"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if sb.ID() != "winner" {
		t.Errorf("id = %q, want the winner's container", sb.ID())
	}
}

// The create-race loser applies the same fixed-at-create validation as the
// ordinary adoption path: a winner built from a different spec is refused, not
// adopted (#29).
func TestProvisionRefusesAMismatchedRaceWinner(t *testing.T) {
	created := spec()
	requested := created
	requested.Image = "img:2"
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
			io.WriteString(w, inspectJSON("winner", created, true))
		case r.URL.Path == "/containers/create":
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"message":"Conflict. The container name is already in use"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			t.Error("a mismatched race winner was started")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if _, err := p.Provision(context.Background(), requested); !errors.Is(err, sandbox.ErrSpecMismatch) {
		t.Errorf("err = %v, want sandbox.ErrSpecMismatch", err)
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
	}).attach("gone", "/workspace", "")
	if err := c.Destroy(context.Background()); err != nil {
		t.Errorf("destroy of a missing container: %v, want nil", err)
	}

	c = fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"removal in progress"}`)
	}).attach("busy", "/workspace", "")
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
	c := p.attach("gone", "/workspace", "")
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
	c := p.attach("abc", "/workspace", "")
	res, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "echo hi"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Stdout != "hi\n" || res.ExitCode != 9 || inspects != 3 {
		t.Errorf("res=%+v inspects=%d", res, inspects)
	}
}

// TimedOut needs both the watchdog's signal and a command that was alive to
// receive it — and the deadline it has to have been alive at is the watchdog's
// own, which is the caller's request rounded up to whole seconds.
func TestTimedOutNeedsTheWatchdogsDeadlineNotTheCallers(t *testing.T) {
	// A self-inflicted SIGKILL well inside the deadline is not a timeout.
	res, err := execDaemon(t, fakeExec{aliveFor: time.Millisecond, code: sigkillExit}).
		Exec(context.Background(), sandbox.ExecRequest{Command: "kill -9 $$", Timeout: time.Hour})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.TimedOut {
		t.Error("a SIGKILL well inside the deadline read as a timeout")
	}

	// With no deadline at all, 137 is just an exit code.
	if res, err := execDaemon(t, fakeExec{aliveFor: time.Millisecond, code: sigkillExit}).
		Exec(context.Background(), sandbox.ExecRequest{Command: "kill -9 $$"}); err != nil || res.TimedOut {
		t.Errorf("res=%+v err=%v", res, err)
	}

	// The watchdog can only sleep whole seconds. A 1.1s request makes it sleep
	// 2s, so a SIGKILL at 1.2s did not come from it — probing at the caller's
	// 1.1s would call this a timeout that never happened.
	res, err = execDaemon(t, fakeExec{aliveFor: 1200 * time.Millisecond, code: sigkillExit}).
		Exec(context.Background(), sandbox.ExecRequest{Command: "kill -9 $$", Timeout: 1100 * time.Millisecond})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.TimedOut {
		t.Error("a SIGKILL before the watchdog's rounded-up deadline read as a timeout")
	}

	// Alive when the watchdog fired, and killed by it: a timeout.
	res, err = execDaemon(t, fakeExec{aliveFor: 1200 * time.Millisecond, code: sigkillExit}).
		Exec(context.Background(), sandbox.ExecRequest{Command: "sleep 300", Timeout: time.Second})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.TimedOut {
		t.Error("a SIGKILL past the deadline did not read as a timeout")
	}

	// A command that drifts a hair past the deadline and exits on its own is
	// not accused of anything: that much is the sandbox's own measurement noise.
	res, err = execDaemon(t, fakeExec{aliveFor: 1100 * time.Millisecond, code: 0}).
		Exec(context.Background(), sandbox.ExecRequest{Command: "echo hi", Timeout: time.Second})
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
		c := execDaemon(t, fakeExec{aliveFor: 1500 * time.Millisecond, code: code})
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

// The straggler case, on a daemon whose behaviour can be dictated: the command
// dies at once, and something it backgrounded holds the output stream open well
// past the deadline. Timing the stream would report a timeout and a SIGKILL for
// a command that exited 0 in a millisecond.
func TestAStragglerHoldingTheStreamIsNotTheCommand(t *testing.T) {
	c := execDaemon(t, fakeExec{
		aliveFor:  time.Millisecond,
		streamFor: 2500 * time.Millisecond,
		code:      0,
		stdout:    "started",
	})
	res, err := c.Exec(context.Background(), sandbox.ExecRequest{
		Command: "sleep 300 & echo started", Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.TimedOut || res.ExitCode != 0 {
		t.Errorf("a command that exited at once was blamed for its straggler: %+v", res)
	}
	if res.Stdout != "started" {
		t.Errorf("stdout = %q", res.Stdout)
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
	// The command must BECOME the exec (exec /bin/bash -c "$1"), not run as a
	// child of a wrapper shell. Otherwise the pid Exec watches is a wrapper the
	// command can kill to look finished while it runs on — the bypass this
	// structure closes.
	if !strings.Contains(execWrapper, `exec /bin/bash -c "$1"`) {
		t.Error("the wrapper must exec the command so the exec's pid is the command's own")
	}
	// The watchdog must poll rather than sleep the whole deadline, so it exits
	// with a command that finishes early instead of leaving a stray sleep.
	if !strings.Contains(execWrapper, "kill -0") {
		t.Error("the watchdog must poll the command so it self-cleans on an early exit")
	}
}

// The in-container watchdog is a process the command can kill. The deadline
// must therefore be enforced outside the container too: once its own bound
// passes, Exec stops waiting and calls the timeout itself.
func TestExecStopsWaitingWhenTheSandboxsWatchdogDoesNot(t *testing.T) {
	var inspects int
	c := execDaemon(t, fakeExec{
		aliveFor:   time.Hour, // the command killed its watchdog and runs on
		holdStream: true,      // so nothing ever closes its output
		stdout:     "partial output",
		inspects:   &inspects,
	})
	c.killGrace, c.overrunSlop = 200*time.Millisecond, 50*time.Millisecond

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
	// One inspect, for the pid. A command that never finished must never be
	// asked for an exit code: the daemon holds the exec "running" for as long
	// as its stream is open, so the ask would spin until the budget ran out.
	if inspects != 1 {
		t.Errorf("%d exec inspects, want only the pid lookup", inspects)
	}
}

// The mirror image of a timeout: Exec gave up on the output stream, but the
// probes say the command itself died inside its deadline and a straggler is
// holding the stream open. There is no timeout to report, and the command's own
// exit code is there for the asking.
func TestAbandoningAStragglersStreamIsNotATimeout(t *testing.T) {
	c := execDaemon(t, fakeExec{
		aliveFor:  time.Millisecond,
		streamFor: 1400 * time.Millisecond,
		code:      7,
		stdout:    "done",
	})
	c.killGrace, c.overrunSlop = 200*time.Millisecond, 50*time.Millisecond

	res, err := c.Exec(context.Background(), sandbox.ExecRequest{
		Command: "sleep 300 & echo done", Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.TimedOut {
		t.Errorf("giving up on a straggler's stream was read as the command timing out: %+v", res)
	}
	if res.ExitCode != 7 || res.Stdout != "done" {
		t.Errorf("result = %+v, want the command's own exit code and output", res)
	}
}

// The caller's own cancellation is not a timeout — it is the caller's error,
// and reporting it as a clean "the command timed out" would hide a shutdown.
// The stream must already be open when the caller gives up, so that the
// cancellation lands where a sandbox deadline would: mid-read.
func TestCallerCancellationIsNotATimeout(t *testing.T) {
	var inspects int
	c := execDaemon(t, fakeExec{
		aliveFor:   time.Hour, // still running when the caller walks away
		holdStream: true,
		stdout:     "started",
		inspects:   &inspects,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// A generous sandbox deadline, so only the caller's context can fire.
	_, err := c.Exec(ctx, sandbox.ExecRequest{Command: "sleep 300", Timeout: time.Hour})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the caller's context error", err)
	}
	if inspects != 1 {
		t.Errorf("%d exec inspects, want only the pid lookup — a cancelled call asks for no exit code", inspects)
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
	c := p.attach("abc", "/workspace", "")
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
	c := p.attach("abc", "/workspace", "")
	_, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "true"})
	if err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("exec: %v, want the daemon's own error", err)
	}
	if !strings.Contains(err.Error(), "No such exec instance") {
		t.Errorf("exec: %v", err)
	}
}

// The shape of one buffered write: the archive endpoint is tried first and only a
// refusal costs a `mkdir`, and the entry lands under a temporary name that a
// second exec renames onto the target — the two execs are the write's whole cost,
// and the rename is what makes it atomic.
func TestWriteFileCreatesParentsOnlyWhenNeeded(t *testing.T) {
	var puts int
	var commands []string
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
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			// The wrapper takes the command as an argument, not as script text.
			commands = append(commands, body.Cmd[len(body.Cmd)-2])
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "")
	if err := c.WriteFile(context.Background(), "/workspace/a/b/f.txt", []byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if puts != 2 || len(commands) != 2 {
		t.Fatalf("puts=%d execs=%d (%q) — want one failed put, a mkdir, a retry, and a rename",
			puts, len(commands), commands)
	}
	if !strings.Contains(commands[0], "mkdir -p '/workspace/a/b'") {
		t.Errorf("first exec = %q, want the mkdir the 404 asked for", commands[0])
	}
	if !strings.Contains(commands[1], "mv -f '/workspace/a/b/"+sandbox.TempPrefix) ||
		!strings.Contains(commands[1], "'/workspace/a/b/f.txt'") {
		t.Errorf("second exec = %q, want the temporary file renamed onto the target", commands[1])
	}
	// Asked before the move and again after it. The race between them cannot be
	// staged from here — this backend's script runs in a container — so what is
	// pinned is that the script the daemon is handed asks twice; the k8s script
	// test stages the outcome itself, against a shimmed `mv`.
	if n := strings.Count(commands[1], "[ -d "); n != 2 {
		t.Errorf("the rename asks whether the target is a directory %d times, want 2: %q", n, commands[1])
	}
}

// A buffered write whose put fails takes its residue with it. However much of the
// entry the daemon extracted before the failure, it is landed under a name nothing
// will ever claim — and a real daemon does not produce this failure on demand, so
// it is staged here rather than left to the live suite (which can only reach the
// streaming half of it, through a short src). The removal is followed by the
// unreplaceable probe — a refused put is the only signal a read-only rootfs
// gives, so every failed put asks (#303) — and a target the probe answers
// replaceable keeps the daemon's own error, as here.
func TestWriteFileShedsItsTempWhenThePutFails(t *testing.T) {
	var commands []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"daemon gave up mid-transfer"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			commands = append(commands, body.Cmd[len(body.Cmd)-2])
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "")
	err := c.WriteFile(context.Background(), "/workspace/f.txt", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "daemon gave up mid-transfer") {
		t.Fatalf("err = %v, want the daemon's failure", err)
	}
	if len(commands) < 3 {
		t.Fatalf("execs = %q, want the removal then the two classification probes", commands)
	}
	if removal := commands[len(commands)-3]; !strings.HasPrefix(removal, "rm -f '/workspace/"+sandbox.TempPrefix) {
		t.Errorf("third-to-last exec = %q, want the temporary file removed", removal)
	}
	if probe := commands[len(commands)-2]; !strings.Contains(probe, "__map_unreplaceable '/workspace/f.txt'") {
		t.Errorf("second-to-last exec = %q, want the refused put asked about the target", probe)
	}
	// A replaceable target gets the second question — can the parent take a
	// create at all — and a parent that can (this fake's execs all exit 0)
	// keeps the daemon's own error (plan 23, #306).
	if last := commands[len(commands)-1]; !strings.Contains(last, ": > '/workspace/"+sandbox.TempPrefix) {
		t.Errorf("last exec = %q, want the writability probe", last)
	}
}

// A PUT the daemon refuses on a replaceable target whose parent then refuses
// the probe's create is the model's error: ErrNotWritable, carrying the
// sandbox's own strerror text as the reason (plan 23, #306) — never the raw
// daemon message the executor would abandon the work item over.
func TestWriteFileClassifiesAnUnwritableParentWhenThePutFails(t *testing.T) {
	// The probe is recognized by its command rather than its position, so the
	// mkdir-and-retry execs ahead of it cannot renumber it out from under the
	// fake. Its create is refused, and the reason travels on stdout the way
	// the write script's own refusal does.
	probes := map[string]bool{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"container rootfs is marked read-only"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			probes[id] = strings.Contains(body.Cmd[len(body.Cmd)-2], ": > '/workspace/"+sandbox.TempPrefix)
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			if probes[execID()] {
				w.Write(frame(streamStdout, "Read-only file system"))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			if probes[execID()] {
				fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, sandbox.ExitPathNotWritable)
			} else {
				io.WriteString(w, `{"Running":false,"ExitCode":0}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "")
	err := c.WriteFile(context.Background(), "/workspace/f.txt", []byte("x"))
	if !errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	var pnw *sandbox.PathNotWritableError
	if !errors.As(err, &pnw) || pnw.Reason != "Read-only file system" {
		t.Fatalf("err = %v, want the probe's reason carried", err)
	}
}

// A parent mkdirAll cannot make for a reason that is not a blocking file is the
// same refusal one probe earlier: the mkdir's own stderr names why, and the
// classification keeps the model's error out of the executor's fault path
// (plan 23, #306).
func TestWriteFileStreamClassifiesAnUnmakeableParent(t *testing.T) {
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			w.WriteHeader(http.StatusOK)
			w.Write(frame(streamStderr, "mkdir: cannot create directory '/newtop': Read-only file system"))
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":1}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "")
	err := c.WriteFileStream(context.Background(), "/newtop/f.txt", strings.NewReader("x"), 1)
	if !errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	var pnw *sandbox.PathNotWritableError
	if !errors.As(err, &pnw) || pnw.Reason != "Read-only file system" {
		t.Fatalf("err = %v, want the mkdir's reason carried", err)
	}
}

// The daemon extracts the archive as root, so a root-owned parent under a
// non-root sandbox user takes the PUT — the refusal only surfaces in the rename
// exec, which runs as that user. The same writability question a refused PUT
// asks is asked there too, so the write is the model's error on this route as
// well (plan 23, #306), never the raw exit the executor would abandon the work
// item over.
func TestWriteFileClassifiesARootOwnedParentAtRename(t *testing.T) {
	// The rename and the probe are recognized by their commands rather than
	// their positions, as the PUT-refusal tests recognize theirs.
	kinds := map[string]string{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			switch cmd := body.Cmd[len(body.Cmd)-2]; {
			case strings.Contains(cmd, "mv -f"):
				kinds[id] = "rename"
			case strings.Contains(cmd, ": > '/etc/"+sandbox.TempPrefix):
				kinds[id] = "probe"
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			switch kinds[execID()] {
			case "rename":
				w.Write(frame(streamStderr, "mv: cannot move '/etc/.map-write-x' to '/etc/f.txt': Permission denied"))
			case "probe":
				w.Write(frame(streamStdout, "Permission denied"))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch kinds[execID()] {
			case "rename":
				io.WriteString(w, `{"Running":false,"ExitCode":1}`)
			case "probe":
				fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, sandbox.ExitPathNotWritable)
			default:
				io.WriteString(w, `{"Running":false,"ExitCode":0}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "")
	err := c.WriteFile(context.Background(), "/etc/f.txt", []byte("x"))
	if !errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	var pnw *sandbox.PathNotWritableError
	if !errors.As(err, &pnw) || pnw.Reason != "Permission denied" {
		t.Fatalf("err = %v, want the probe's reason carried", err)
	}
}

// The temporary a refused rename leaves behind was landed by the daemon's
// root-credentialed extraction, so a parent the sandbox user cannot write holds
// a file only the same credential can remove. The shed therefore runs as
// User "0" — a fixed `rm -f` of the platform's own TempPrefix name, and nothing
// else — while every other exec keeps the container's own user (#310).
// rootShed reports whether an exec is the daemon-credentialed shed of a
// temporary under dir: uid 0, argv rather than script — one absolute binary and
// two literal arguments, so no shell of the agent's environment runs — with the
// loader's injection points emptied for the exec (#310).
func rootShed(e execSeen, dir string) bool {
	return e.user == "0" &&
		len(e.cmd) == 3 && e.cmd[0] == "/bin/rm" && e.cmd[1] == "-f" &&
		strings.HasPrefix(e.cmd[2], dir+sandbox.TempPrefix) &&
		strings.Contains(strings.Join(e.env, " "), "LD_PRELOAD=")
}

// execSeen is one exec as the fake daemon received it. The command is the whole
// argv, because the shed's shape is the assertion: a wrapped exec carries its
// command as the second-to-last element, the shed carries no wrapper at all.
type execSeen struct {
	cmd, env  []string
	user      string
	wrappedAs string
}

// sawExec records an exec create for the shed assertions.
func sawExec(body execConfig) execSeen {
	e := execSeen{cmd: body.Cmd, env: body.Env, user: body.User}
	if len(body.Cmd) > 2 {
		e.wrappedAs = body.Cmd[len(body.Cmd)-2]
	}
	return e
}

func TestARefusedRenameShedsItsTempWithTheDaemonsCredential(t *testing.T) {
	var seen []execSeen
	kinds := map[string]string{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			e := sawExec(body)
			seen = append(seen, e)
			switch {
			case strings.Contains(e.wrappedAs, "mv -f"):
				kinds[id] = "rename"
			case strings.Contains(e.wrappedAs, ": > '/etc/"+sandbox.TempPrefix):
				kinds[id] = "probe"
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			switch kinds[execID()] {
			case "rename":
				w.Write(frame(streamStderr, "mv: cannot move '/etc/.map-write-x' to '/etc/f.txt': Permission denied"))
			case "probe":
				w.Write(frame(streamStdout, "Permission denied"))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch kinds[execID()] {
			case "rename":
				io.WriteString(w, `{"Running":false,"ExitCode":1}`)
			case "probe":
				fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, sandbox.ExitPathNotWritable)
			default:
				io.WriteString(w, `{"Running":false,"ExitCode":0}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "")
	err := c.WriteFile(context.Background(), "/etc/f.txt", []byte("x"))
	if !errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	if last := seen[len(seen)-1]; !rootShed(last, "/etc/") {
		t.Fatalf("last exec = %+v, want the daemon-credentialed shed: uid 0, argv `/bin/rm -f <temp>`, loader hooks emptied", last)
	}
	for _, e := range seen[:len(seen)-1] {
		if e.user != "" || len(e.env) != 0 {
			t.Errorf("exec %q carries user %q env %q, want the container's own", e.wrappedAs, e.user, e.env)
		}
	}
}

// A rename exec that could not run at all — not a reported failure exit, the
// exec itself dying — sheds with both credentials, each best-effort: the exec
// just failed, so neither is trusted alone. The user's rm covers a writable
// parent when the failure was transient; the root one covers the parent the
// user cannot write (#310).
func TestARenameExecFailureShedsWithBothCredentials(t *testing.T) {
	var seen []execSeen
	renames := map[string]bool{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			e := sawExec(body)
			seen = append(seen, e)
			renames[id] = strings.Contains(e.wrappedAs, "mv -f")
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			if renames[execID()] {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"message":"exec start went away"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "")
	err := c.WriteFile(context.Background(), "/etc/f.txt", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "exec start went away") {
		t.Fatalf("err = %v, want the exec's own failure", err)
	}
	if len(seen) < 3 {
		t.Fatalf("execs = %+v, want the rename then both sheds", seen)
	}
	userShed := seen[len(seen)-2]
	if userShed.user != "" || !strings.Contains(userShed.wrappedAs, "rm -f '/etc/"+sandbox.TempPrefix) {
		t.Errorf("second-to-last exec = %+v, want the sandbox user's shed", userShed)
	}
	if last := seen[len(seen)-1]; !rootShed(last, "/etc/") {
		t.Errorf("last exec = %+v, want the root-credentialed shed", last)
	}
}

// A failed move over a parent that can take a create keeps the raw error: the
// failure was the transfer's, not the target's, and calling the path unwritable
// would tell the model the path is bad when it is not.
func TestRenameFailureOnAWritableTargetKeepsTheRawError(t *testing.T) {
	var seen []execSeen
	renames := map[string]bool{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			e := sawExec(body)
			seen = append(seen, e)
			renames[id] = strings.Contains(e.wrappedAs, "mv -f")
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/json"):
			if renames[execID()] {
				io.WriteString(w, `{"Running":false,"ExitCode":1}`)
			} else {
				io.WriteString(w, `{"Running":false,"ExitCode":0}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "")
	err := c.WriteFile(context.Background(), "/workspace/f.txt", []byte("x"))
	if errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v; a writable target must keep the raw error", err)
	}
	if err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("err = %v, want the move's own failure", err)
	}
	if len(seen) < 2 {
		t.Fatalf("execs = %+v, want the probe then the shed after the failed move", seen)
	}
	if probe := seen[len(seen)-2]; !strings.Contains(probe.wrappedAs, ": > '/workspace/"+sandbox.TempPrefix) {
		t.Errorf("second-to-last exec = %+v, want the writability probe asked after the failed move", probe)
	}
	// The shed runs on the raw route too — idempotent where the script's own
	// rm already worked (#310).
	if last := seen[len(seen)-1]; !rootShed(last, "/workspace/") {
		t.Errorf("last exec = %+v, want the daemon-credentialed shed", last)
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
	c := p.attach("abc", "/workspace", "")
	err := c.WriteFile(context.Background(), "/workspace/a/f.txt", []byte("x"))
	if err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v, want the daemon's path error", err)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v", err)
	}
}

func TestWriteFileSurfacesMkdirFailure(t *testing.T) {
	var commands []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no such directory"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			commands = append(commands, body.Cmd[len(body.Cmd)-2])
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			w.Write(frame(2, "Read-only file system\n"))
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":1}`)
		}
	})
	c := p.attach("abc", "/workspace", "")
	err := c.WriteFile(context.Background(), "/workspace/a/f.txt", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "Read-only file system") {
		t.Errorf("err = %v, want the mkdir's stderr", err)
	}
	// And the put that failed before the mkdir did not get to keep whatever it
	// landed: a put refused outright leaves nothing, but one that died in transfer
	// leaves a piece of the entry, and this is the path that sheds it.
	if last := commands[len(commands)-1]; !strings.HasPrefix(last, "rm -f '/workspace/a/"+sandbox.TempPrefix) {
		t.Errorf("last exec = %q, want the temporary file removed after the failed mkdir", last)
	}
}

// The rename is one exec, and when that exec cannot run at all the bytes are
// landed under a name nothing will claim. The removal is attempted on the way out
// with both credentials (#310) — here every attempt fails too, since this daemon
// refuses every exec, which is exactly why the attempts are what get asserted
// rather than their outcomes.
func TestWriteFileShedsItsTempWhenTheRenameCannotRun(t *testing.T) {
	var execs int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
		case strings.HasSuffix(r.URL.Path, "/exec"):
			execs++
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"cannot create exec"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "")
	if err := c.WriteFile(context.Background(), "/workspace/f.txt", []byte("x")); err == nil {
		t.Fatal("write returned nil, want the failed rename")
	}
	if execs != 3 {
		t.Errorf("%d exec attempts, want 3 — the rename, then the removal of what it could not name, with each credential", execs)
	}
}

// The unix transport is the production path; tcp is only how these tests
// reach a fake. Dial a real unix socket so the dialer itself is exercised.
func TestUnixTransportDialsTheSocket(t *testing.T) {
	s := spec()
	socket := filepath.Join(t.TempDir(), "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, inspectJSON("over-unix", s, true))
		})},
	}
	srv.Start()
	defer srv.Close()

	p, err := New(Config{Host: "unix://" + socket})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	sb, err := p.Provision(context.Background(), s)
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
	s := spec()
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, inspectJSON("abc", s, false))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"cannot start"}`)
		}
	})
	if _, err := p.Provision(context.Background(), s); err == nil ||
		!strings.Contains(err.Error(), "cannot start") {
		t.Errorf("err = %v", err)
	}
}

func TestProvisionDefaultsTheWorkdir(t *testing.T) {
	s := sandbox.Spec{SessionID: domain.NewID("sesn"), Image: "img:1"} // no Workdir
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, inspectJSON("abc", s, true))
	})
	sb, err := p.Provision(context.Background(), s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := sb.(*container).workdir; got != sandbox.DefaultWorkdir {
		t.Errorf("workdir = %q, want %q", got, sandbox.DefaultWorkdir)
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
		return p.attach("abc", "/workspace", "")
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
// The pid is what every deadline probe asks about. A zero one would answer
// "gone" to each of them, disarming the deadline in silence — so Exec insists.
func TestExecFailsLoudlyWhenTheDaemonWillNotNameTheProcess(t *testing.T) {
	var pids []int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			w.(http.Flusher).Flush()
		case r.URL.Path == "/exec/e1/json":
			pid := 0
			if len(pids) > 0 {
				pid, pids = pids[0], pids[1:]
			}
			fmt.Fprintf(w, `{"Running":false,"ExitCode":0,"Pid":%d}`, pid)
		case strings.HasSuffix(r.URL.Path, "/top"):
			io.WriteString(w, `{"Titles":["PID"],"Processes":[]}`)
		}
	})
	c := p.attach("abc", "/workspace", "")
	c.exitBudget = 100 * time.Millisecond

	_, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "true", Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "never reported a pid") {
		t.Errorf("err = %v, want a refusal to run a deadline it cannot probe", err)
	}

	// A pid that only shows up on the second ask is fine.
	pids = []int{0, 4242}
	if _, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "true", Timeout: time.Second}); err != nil {
		t.Errorf("exec: %v, want the retried pid to be accepted", err)
	}
}

// If the daemon will not say whether the command is still running, Exec must
// guess in the direction that keeps the deadline's promise. A hidden overrun
// breaks the guarantee; a mislabelled command costs one tool call.
func TestAnUnreadableProcessListPrefersTheTimeout(t *testing.T) {
	for _, tc := range []struct{ name, top string }{
		{"the daemon refuses", ""},
		{"the process list has no pid column", `{"Titles":["USER","COMMAND"],"Processes":[["root","bash"]]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/exec"):
					io.WriteString(w, `{"Id":"e1"}`)
				case r.URL.Path == "/exec/e1/start":
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					time.Sleep(1200 * time.Millisecond)
				case r.URL.Path == "/exec/e1/json":
					io.WriteString(w, `{"Running":false,"ExitCode":0,"Pid":4242}`)
				case strings.HasSuffix(r.URL.Path, "/top"):
					if tc.top == "" {
						w.WriteHeader(http.StatusInternalServerError)
						io.WriteString(w, `{"message":"cannot ps"}`)
						return
					}
					io.WriteString(w, tc.top)
				}
			})
			c := p.attach("abc", "/workspace", "")
			c.overrunSlop = 100 * time.Millisecond

			res, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "exit 0", Timeout: time.Second})
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if !res.TimedOut {
				t.Errorf("an unanswerable probe hid a possible overrun: %+v", res)
			}
		})
	}
}

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
	c := p.attach("abc", "/workspace", "")
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

// The real-daemon proof that a command cannot kill the watchdog guarding it and
// outrun or hide its deadline now lives in the shared contract suite, as
// ExecCannotOutliveItsDeadlineUnreported. It binds every backend there, and this
// provider runs it in TestDockerProviderContract.

// One overrun the contract suite cannot stage against a real daemon: the
// command exits during its own overrun probe, so the probe's `top` request and
// the stream close race. Exec stops probing the instant the stream closes; if
// the overrun confirmation rode that cancellation, the daemon's answer would be
// read as "process gone" and a real overrun erased into a clean exit. Only a
// fake daemon can hold a `top` request open across the stream close on demand.
func TestOverrunSurvivesTheStreamClosingDuringItsProbe(t *testing.T) {
	closeStream := make(chan struct{})
	var closeOnce sync.Once
	releaseStream := func() { closeOnce.Do(func() { close(closeStream) }) }

	var mu sync.Mutex
	var topCalls int
	var streamClosed bool

	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)

		case r.URL.Path == "/exec/e1/start":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-closeStream // hold the command's stream open until the probe fires
			mu.Lock()
			streamClosed = true
			mu.Unlock()

		case r.URL.Path == "/exec/e1/json":
			// A pid while the stream is open (execPid), then the clean code the
			// command chose once it has closed (exitCode).
			mu.Lock()
			done := streamClosed
			mu.Unlock()
			fmt.Fprintf(w, `{"Running":%t,"ExitCode":0,"Pid":%d}`, !done, fakeExecPid)

		case strings.HasSuffix(r.URL.Path, "/top"):
			mu.Lock()
			topCalls++
			n := topCalls
			mu.Unlock()
			if n >= 2 {
				// The overrun probe. Close the command's stream so Exec stops
				// probing, then block so that stop races this very request. The
				// command was alive at this instant — it overran — so the honest
				// answer below is "alive"; a backend that lets the stream close
				// cancel this request never reaches it.
				releaseStream()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(300 * time.Millisecond):
				}
			}
			fmt.Fprintf(w, `{"Titles":["PID"],"Processes":[["1"],["%d"]]}`, fakeExecPid)

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	// Registered after fakeDaemon's cleanup so it runs first (LIFO): the server
	// will not shut down while the start handler is still holding the stream.
	t.Cleanup(releaseStream)
	c := p.attach("abc", "/workspace", "")

	res, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "x", Timeout: time.Second})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("a command that overran, then exited during its overrun probe, was reported finished: %+v", res)
	}
}

// The overrun's sibling: it is the *pre-deadline* probe that stalls. A prober
// that ran its two probes in sequence would still be waiting on that first `top`
// when a watchdog-killed command overran and exited; the stream's close would
// cancel the whole wait before the overrun instant was ever reached, and the
// overrun would go unmeasured. The probes must keep independent clocks.
func TestOverrunDetectedWhenTheFirstProbeStalls(t *testing.T) {
	closeStream := make(chan struct{})
	var closeOnce sync.Once
	releaseStream := func() { closeOnce.Do(func() { close(closeStream) }) }

	var mu sync.Mutex
	var topCalls int
	var streamClosed bool

	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			io.WriteString(w, `{"Id":"e1"}`)

		case r.URL.Path == "/exec/e1/start":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			// The command overruns: its stream stays open well past
			// deadline+overrunSlop, then closes as it exits.
			go func() {
				time.Sleep(1700 * time.Millisecond)
				releaseStream()
			}()
			<-closeStream
			mu.Lock()
			streamClosed = true
			mu.Unlock()

		case r.URL.Path == "/exec/e1/json":
			mu.Lock()
			done := streamClosed
			mu.Unlock()
			fmt.Fprintf(w, `{"Running":%t,"ExitCode":0,"Pid":%d}`, !done, fakeExecPid)

		case strings.HasSuffix(r.URL.Path, "/top"):
			mu.Lock()
			topCalls++
			n := topCalls
			gone := streamClosed
			mu.Unlock()
			if n == 1 {
				// The pre-deadline probe, stalled: it never answers on its own,
				// and is cancelled only when the stream finally closes. A
				// sequential prober is stuck here and never reaches the overrun
				// instant below.
				<-r.Context().Done()
				return
			}
			// The overrun probe. The command is listed while it is alive and gone
			// once it has exited (its stream closed). Independent scheduling
			// reaches this at deadline+overrunSlop, while the command still runs,
			// and sees it alive; a sequential prober only reaches it after the
			// stalled first probe is cancelled at stream-close, by which point
			// the command has exited and it sees gone — the sixth bug.
			if gone {
				fmt.Fprint(w, `{"Titles":["PID"],"Processes":[["1"]]}`)
				return
			}
			fmt.Fprintf(w, `{"Titles":["PID"],"Processes":[["1"],["%d"]]}`, fakeExecPid)

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	t.Cleanup(releaseStream)
	c := p.attach("abc", "/workspace", "")

	res, err := c.Exec(context.Background(), sandbox.ExecRequest{Command: "x", Timeout: time.Second})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("a command that overran while its pre-deadline probe stalled was reported finished: %+v", res)
	}
}

// The third probe race, and the one on the punctual-kill path: the watchdog's
// kill is itself what closes the command's stream and cancels the pre-deadline
// probe's `top`, so on a daemon slower to answer than the 50ms probe lead the
// probe never answers at all — and `aliveAtDeadline` is the only term that can
// fire when a command is killed on time. Reading that cancellation as "already
// finished" reported a real timeout as a plain `{137, TimedOut: false}` (#193).
//
// What tells a punctual kill from an early exit is *when* the stream closed,
// which Exec already knows host-side: a command that finished early cannot close
// its stream after the deadline. The rows below differ only in the daemon's `top`
// latency and in when the command died.
//
// Every row runs against a one-second deadline with the probe lead widened from
// the production 50ms to 800ms, so the probe fires at 200ms and each instant that
// matters — the probe, the command's death, the deadline — is hundreds of
// milliseconds from the next. The lead's own value is not what these rows pin
// (TestTimedOutNeedsTheWatchdogsDeadlineNotTheCallers covers the default), and at
// 50ms a row would turn on whether a loaded runner scheduled one goroutine inside
// a 50ms window.
func TestAPunctualKillSurvivesASlowPreDeadlineProbe(t *testing.T) {
	for _, tc := range []struct {
		name         string
		command      string
		aliveFor     time.Duration
		topDelay     time.Duration
		wantTimedOut bool
	}{
		// The control: the same kill on a daemon that answers at once, so the probe
		// carries the verdict itself.
		{
			name: "a fast top answers before the kill", command: "sleep 300",
			aliveFor: 1200 * time.Millisecond, wantTimedOut: true,
		},
		// The bug: the answer would not have come until 1.4s, so it is still in
		// flight when the kill closes the stream at 1.2s and the probe is cancelled
		// with nothing to say. That close is 200ms *past* the deadline, and seeing
		// it can only slip later — a command that finished early could not have
		// closed there at all.
		{
			name: "a slow top loses its answer to the kill", command: "sleep 300",
			aliveFor: 1200 * time.Millisecond, topDelay: 1200 * time.Millisecond, wantTimedOut: true,
		},
		// The other direction on that same slow daemon: a command that SIGKILLs
		// itself at 700ms closes its stream 300ms *before* the deadline, and no
		// answer is needed to know that is an early exit, not a timeout to invent.
		// This is the row that guards the discriminator — and if a stall did push
		// the probe past the close, or the daemon did answer, the verdict is the
		// same `false`, so the row cannot flake into a failure either way.
		{
			name: "a close before the deadline is still an early exit", command: "kill -9 $$",
			aliveFor: 700 * time.Millisecond, topDelay: 700 * time.Millisecond, wantTimedOut: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := execDaemon(t, fakeExec{aliveFor: tc.aliveFor, topDelay: tc.topDelay, code: sigkillExit})
			c.probeLead = 800 * time.Millisecond
			start := time.Now()
			res, err := c.Exec(context.Background(), sandbox.ExecRequest{
				Command: tc.command, Timeout: time.Second,
			})
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if res.TimedOut != tc.wantTimedOut {
				t.Errorf("result = %+v after %s, want TimedOut=%t",
					res, time.Since(start), tc.wantTimedOut)
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
		return p.attach("abc", "/workspace", "")
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
