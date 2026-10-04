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
	gopath "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
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
	// killed: the watchdog's mark is on the container's filesystem, as it is
	// after a kill it actually delivered. Absent by default, which is what every
	// command that was never killed by its watchdog looks like.
	killed bool
	// marks counts the reads of that mark, so a test can pin that the round trip
	// is only paid where the answer could still change the verdict.
	marks *int
}

const fakeExecPid = 4242

// wrapperCommand pulls the shell command out of an exec create body.
//
// Every exec this backend makes — a tool call, and equally the mkdir, rename and
// shed the write path runs — goes through Exec and so through the same wrapper,
// whose argv is `/bin/bash -c <wrapper> map-exec <command> <seconds> <state>`.
// The command therefore sits at a fixed index from the front. These tests used to
// read it from the *back*, which was correct only for as long as the wrapper's
// argument count never changed: #390 appended the state path and silently shifted
// every one of them onto the seconds string. Reading from the front is what makes
// the next argument a compile-or-fail question rather than a set of assertions
// that quietly begin describing the wrong thing.
//
// Every exec these fakes read the command of is the platform's own — the write
// path's scripts — so each must carry the platform's script preamble
// (sandbox.Script) or open a frame (sandboxtest.Scripted): what a fake reads
// is the script after it, and a command without it fails the test, where
// stripping it silently would let a platform exec that lost it run under an
// image's errexit unnoticed (#860).
func wrapperCommand(t testing.TB, cmd []string) string {
	const commandArg = 4
	if len(cmd) <= commandArg {
		return ""
	}
	if !sandboxtest.Scripted(cmd[commandArg]) {
		t.Errorf("a platform exec without the script preamble (sandbox.Script): %q", cmd[commandArg])
	}
	return strings.TrimPrefix(cmd[commandArg], sandbox.ScriptPreamble)
}

// framedOutput is what the daemon's attach stream carries for a framed
// platform script (sandbox.ExecFramed): its stdout and stderr, each inside the
// frame the command carries, with what an image's BASH_ENV file prints around
// them — a banner ahead of each, the stderr one ending its line, so a reason
// read from the stream's first line would be the banner's, and an EXIT trap's
// words after.
func framedOutput(t *testing.T, cmd []string, stdout, stderr string) []byte {
	t.Helper()
	_, f, ok := sandboxtest.Unwrap(wrapperCommand(t, cmd))
	if !ok {
		t.Fatalf("exec %q is not a framed script", wrapperCommand(t, cmd))
	}
	res := sandboxtest.Framed(f, sandbox.ExecResult{Stdout: stdout, Stderr: stderr})
	return append(frame(streamStdout, "welcome to the image "+res.Stdout+"exit banner "),
		frame(streamStderr, "stderr: banner\n"+res.Stderr+"exit stderr ")...)
}

// wrapperState pulls the per-exec state path out of the same argv, so a fake
// daemon can hold Exec to the path it actually handed the wrapper rather than
// answering any archive HEAD it happens to receive.
func wrapperState(cmd []string) string {
	const stateArg = 6
	if len(cmd) <= stateArg {
		return ""
	}
	return cmd[stateArg]
}

// execDaemon serves the endpoints Exec uses, with fe's timings.
func execDaemon(t *testing.T, fe fakeExec) *container {
	t.Helper()
	if fe.streamFor == 0 {
		fe.streamFor = fe.aliveFor
	}
	held := make(chan struct{})

	var mu sync.Mutex
	var startedAt time.Time
	// state is the path Exec handed this exec's wrapper, captured at create so the
	// archive HEAD can be held to it. Without that, the fake would answer *any*
	// mark path and the tests would pass an Exec that probed the wrong one.
	var state string
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
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			mu.Lock()
			state = wrapperState(body.Cmd)
			mu.Unlock()
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

		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			// The watchdog's mark, read out of band. A 404 is the honest answer
			// for every command whose watchdog never fired.
			mu.Lock()
			if fe.marks != nil {
				*fe.marks++
			}
			want := state + ".killed"
			mu.Unlock()
			// Held to the exact path this exec's wrapper was given. Answering any
			// path would let an Exec that probed the wrong one — a stale state, or
			// one missing the suffix — pass every test in this file.
			if got := r.URL.Query().Get("path"); got != want {
				t.Errorf("archive HEAD path = %q, want %q", got, want)
			}
			if !fe.killed {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)

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
	return p.attach("abc", "/workspace", "", false)
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

// The address a test must hand the `docker` CLI so it cannot follow a `docker
// context` to a different daemon (#627). TestNewResolvesDaemonAddress cannot
// pin the last step: an unset DOCKER_HOST and an explicit unix host both leave
// the client's base at "http://docker", so only the resolved address separates
// them.
func TestDaemonHostFallsBackToTheWellKnownSocket(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.1:2375")
	if got := daemonHost("unix:///explicit.sock"); got != "unix:///explicit.sock" {
		t.Errorf("explicit host: %q", got)
	}
	if got := daemonHost(""); got != "tcp://10.0.0.1:2375" {
		t.Errorf("DOCKER_HOST: %q", got)
	}
	t.Setenv("DOCKER_HOST", "")
	if got := daemonHost(""); got != "unix:///var/run/docker.sock" {
		t.Errorf("fallback: %q", got)
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

// The ownership guard holds on the read path too: Export serves the
// checkpoint engine, and a name-squatting container must not leak its
// filesystem into another session's checkpoint. The daemon answers the inspect
// with a foreign label; any archive request after that would be the leak.
func TestExportRefusesAContainerItDoesNotOwn(t *testing.T) {
	var touched []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		touched = append(touched, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Id":"squatter","State":{"Running":true},"Config":{"Labels":{"`+sessionLabel+`":"sesn_someone_else"}}}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	_, err := p.Export(context.Background(), spec().SessionID, "/workspace")
	if err == nil || !strings.Contains(err.Error(), "not this platform's sandbox") {
		t.Fatalf("err = %v, want the ownership refusal", err)
	}
	for _, path := range touched {
		if strings.Contains(path, "/archive") {
			t.Error("exported from a container the platform does not own")
		}
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

// A handle knows whether its root is read-only from the container itself, as it
// knows its workdir: the create config on a fresh container, an inspect on one
// it adopts or attaches to — the adopted container's own flag, whatever the spec
// asked for, since hardening is adopted as created — and never by probing. It
// decides how a batch is cut (#859), so a handle that guessed wrong on a
// read-only root would send its archives to `/` and have every one refused.
func TestAHandleTakesItsReadOnlyRootFromTheContainer(t *testing.T) {
	readOnly := func(inspect string) string {
		return strings.Replace(inspect, `"HostConfig":{`, `"HostConfig":{"ReadonlyRootfs":true,`, 1)
	}
	s := spec()
	hardened := s
	hardened.Hardening = sandbox.Hardening{ReadOnlyRootfs: true}
	for _, tc := range []struct {
		name string
		spec sandbox.Spec
		// inspects answers the n-th container inspect (from 1); "" is a 404.
		inspects func(n int) string
		create   int // the create's status, 0 when none is expected
		attach   bool
		want     bool
	}{
		{"created read-only", hardened, func(int) string { return "" }, http.StatusCreated, false, true},
		{"created writable", s, func(int) string { return "" }, http.StatusCreated, false, false},
		{"adopted read-only for a writable spec", s,
			func(int) string { return readOnly(inspectJSON("abc", s, true)) }, 0, false, true},
		{"adopted writable for a read-only spec", hardened,
			func(int) string { return inspectJSON("abc", s, true) }, 0, false, false},
		{"race winner read-only", s, func(n int) string {
			if n == 1 {
				return ""
			}
			return readOnly(inspectJSON("abc", s, true))
		}, http.StatusConflict, false, true},
		{"attached read-only", s, func(int) string { return readOnly(inspectJSON("abc", s, true)) }, 0, true, true},
		{"attached writable", s, func(int) string { return inspectJSON("abc", s, true) }, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var inspects int
			p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/json"):
					inspects++
					body := tc.inspects(inspects)
					if body == "" {
						w.WriteHeader(http.StatusNotFound)
						io.WriteString(w, `{"message":"No such container"}`)
						return
					}
					io.WriteString(w, body)
				case r.URL.Path == "/containers/create" && tc.create != 0:
					if tc.create == http.StatusConflict {
						w.WriteHeader(http.StatusConflict)
						io.WriteString(w, `{"message":"Conflict. The container name is already in use"}`)
						return
					}
					w.WriteHeader(tc.create)
					io.WriteString(w, `{"Id":"abc"}`)
				case strings.HasSuffix(r.URL.Path, "/start"):
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected %s %s — the flag is never probed", r.Method, r.URL.Path)
				}
			})
			var sb sandbox.Sandbox
			var err error
			if tc.attach {
				sb, err = p.Attach(context.Background(), tc.spec.SessionID)
			} else {
				sb, err = p.Provision(context.Background(), tc.spec)
			}
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			if got := sb.(*container).readOnlyRoot; got != tc.want {
				t.Errorf("readOnlyRoot = %v, want %v", got, tc.want)
			}
		})
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
	}).attach("gone", "/workspace", "", false)
	if err := c.Destroy(context.Background()); err != nil {
		t.Errorf("destroy of a missing container: %v, want nil", err)
	}

	c = fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"removal in progress"}`)
	}).attach("busy", "/workspace", "", false)
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
	c := p.attach("gone", "/workspace", "", false)
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
	c := p.attach("abc", "/workspace", "", false)
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

// The wrapper writes exactly one thing inside the container — the watchdog's
// mark — and this test is the boundary of that permission.
//
// It used to assert the wrapper wrote *nothing*, and that was a deliberate
// property with a stated reason: the first design's marker "let a command forge
// a timeout it never hit or erase one it did" (docs/history/2026-07.md, whose
// review list names it the /tmp marker). #390 reversed the first half of it, and the reversal
// only holds because the mark is used differently than that first design used it.
// There, the mark was the evidence. Here it is one OR-term beside two
// host-measured ones, and `overran` — the term carrying the deadline's actual
// guarantee — never reads it, so no tampering lets a command outlive its deadline.
// Forging one still requires exiting 137 and buys a tenant a timeout label on its
// own tool call; erasing one costs that command its label and puts it back where
// it stood before the mark existed, which classifyTimeout states as the residual
// limitation rather than as harmlessness.
//
// So the invariant is narrowed rather than dropped, and these assertions are what
// keeps it narrow: no writable path is baked into the script (the state path
// arrives as argv, per-exec and random, so a tenant cannot pre-create a mark for
// an exec that has not started), and `mkdir` is the only write in it.
func TestExecWrapperWritesOnlyTheWatchdogsMark(t *testing.T) {
	for _, writable := range []string{"/tmp", "/var/tmp", "/dev/shm", "/run", "/workspace"} {
		if strings.Contains(execWrapper, writable) {
			t.Errorf("the exec wrapper bakes in %s, which the sandboxed command can write "+
				"and could then pre-create a mark under", writable)
		}
	}
	// One write, and it is the mark. Anything else appearing here is a new grant
	// of container-side state that has not been argued.
	for _, write := range []string{">>", "touch ", "cat >", "tee "} {
		if strings.Contains(execWrapper, write) {
			t.Errorf("the wrapper writes with %q; the mark is the only write it may make:\n%s",
				write, execWrapper)
		}
	}
	if n := strings.Count(execWrapper, "mkdir"); n != 1 {
		t.Errorf("mkdir appears %d times; the mark is the wrapper's one write", n)
	}
	if !strings.Contains(execWrapper, "set -m") {
		t.Error("the wrapper must enable job control so its watchdog runs in a process group of its own")
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
	c := p.attach("abc", "/workspace", "", false)
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
	c := p.attach("abc", "/workspace", "", false)
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
			commands = append(commands, wrapperCommand(t, body.Cmd))
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
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
// replaceable keeps the daemon's own error, as here. A put that died mid-transfer
// is the case where the residue is a *partial payload* rather than an empty name,
// and the daemon extracted it as root — so the sandbox user's `rm` is followed by
// the daemon emptying whatever it could not take back (#310).
func TestWriteFileShedsItsTempWhenThePutFails(t *testing.T) {
	var commands []string
	var reclaimed []byte
	headed := false
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			// The partial entry is still there: this parent refused the user's rm.
			headed = true
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			// Only the put that follows the HEAD is the emptying one; the write's
			// own put — and its retry after the mkdir — is what fails here.
			if headed {
				reclaimed, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"daemon gave up mid-transfer"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			commands = append(commands, wrapperCommand(t, body.Cmd))
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
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
	if reclaimed == nil {
		t.Fatal("the daemon was never handed the emptying archive for what its own put landed")
	}
	if name, size := tarEntry(t, reclaimed); !strings.HasPrefix(name, sandbox.TempPrefix) || size != 0 {
		t.Errorf("the emptying archive carries %q of %d bytes, want the temporary's own name at 0", name, size)
	}
}

// A write whose caller has already given up still sheds what it landed. Cleanup
// is the work that most needs to outlive the context it cleans up after: a tool
// call that timed out mid-transfer is a failed write like any other, and letting
// it inherit the cancellation would skip the shedding in exactly the case that
// produced the residue. Both halves are asserted, so neither can be quietly
// re-attached to the caller's context (#310, review round 5).
func TestCleanupOutlivesTheWriteThatWasCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var commands []string
	var reclaimed []byte
	headed := false
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			headed = true
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			if headed {
				reclaimed, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
				return
			}
			// The caller goes away while its own bytes are being extracted, so
			// every step after this one has a dead context to work from.
			cancel()
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"daemon gave up mid-transfer"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			commands = append(commands, wrapperCommand(t, body.Cmd))
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
	if err := c.WriteFile(ctx, "/workspace/f.txt", []byte("x")); err == nil {
		t.Fatal("write returned nil, want the failure the cancellation caused")
	}
	var removed bool
	for _, cmd := range commands {
		removed = removed || strings.HasPrefix(cmd, "rm -f '/workspace/"+sandbox.TempPrefix)
	}
	if !removed {
		t.Errorf("execs = %q, want the temporary removed on a context the caller already canceled", commands)
	}
	if reclaimed == nil {
		t.Fatal("no emptying archive: the daemon-side shed inherited the caller's cancellation")
	}
	if name, size := tarEntry(t, reclaimed); !strings.HasPrefix(name, sandbox.TempPrefix) || size != 0 {
		t.Errorf("the emptying archive carries %q of %d bytes, want the temporary's own name at 0", name, size)
	}
}

// The batch sheds on the same terms. Its residue is the daemon's extraction too
// — up to 10,000 members of it — so a batch abandoned mid-transfer must not be
// the one write that keeps what it landed (#310, review round 5).
func TestTheBatchesCleanupOutlivesTheWriteThatWasCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var commands []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			cancel()
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"daemon gave up mid-transfer"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			commands = append(commands, wrapperCommand(t, body.Cmd))
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
	err := c.WriteFiles(ctx, []sandbox.FileWrite{{Path: "/workspace/a.txt", Data: []byte("x")}})
	if err == nil {
		t.Fatal("write returned nil, want the failure the cancellation caused")
	}
	var shed bool
	for _, cmd := range commands {
		shed = shed || strings.Contains(cmd, "__map_bulk_discard")
	}
	if !shed {
		t.Errorf("execs = %q, want the batch shed on a context the caller already canceled", commands)
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
	probes := map[string][]string{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			// A read-only root refuses every put outright, so nothing landed
			// and there is nothing for the daemon to empty.
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Could not find the file"}`)
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
			if strings.Contains(wrapperCommand(t, body.Cmd), ": > '/workspace/"+sandbox.TempPrefix) {
				probes[id] = body.Cmd
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			if cmd := probes[execID()]; cmd != nil {
				w.Write(framedOutput(t, cmd, "Read-only file system", ""))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			if probes[execID()] != nil {
				fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, sandbox.ExitPathNotWritable)
			} else {
				io.WriteString(w, `{"Running":false,"ExitCode":0}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
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
// (plan 23, #306). The reason is read from inside the script's frame, so a
// banner an image prints on stderr ahead of it is not taken for it (#860).
func TestWriteFileStreamClassifiesAnUnmakeableParent(t *testing.T) {
	var cmd []string
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			cmd = body.Cmd
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			w.WriteHeader(http.StatusOK)
			w.Write(framedOutput(t, cmd, "", "mkdir: cannot create directory '/newtop': Read-only file system"))
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":1}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
	err := c.WriteFileStream(context.Background(), "/newtop/f.txt", strings.NewReader("x"), 1)
	if !errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	var pnw *sandbox.PathNotWritableError
	if !errors.As(err, &pnw) || pnw.Reason != "Read-only file system" {
		t.Fatalf("err = %v, want the mkdir's reason carried", err)
	}
}

// A probe whose output never carried its frame — a shell that died first, a
// startup that filled the output cap — said nothing the platform reads, and
// never a reason taken from a banner. The mkdir's exit 1 is no refusal then:
// a shell that died before the script left it too, so the raw error stands.
// The writability probe's exit is its own code, which still classifies the
// write as one the path refuses, with no reason, as the k8s write script's
// exit does.
func TestWriteProbesReadNoReasonOutsideTheirFrame(t *testing.T) {
	var execN int
	kinds := map[string]string{}
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Could not find the file"}`)
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
			switch cmd := wrapperCommand(t, body.Cmd); {
			case strings.Contains(cmd, "mkdir -p '/newtop'"):
				kinds[id] = "mkdir"
			case strings.Contains(cmd, ": > '/workspace/"+sandbox.TempPrefix):
				kinds[id] = "probe"
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			switch kinds[execID()] {
			case "mkdir":
				w.Write(frame(streamStderr, "banner: Read-only file system"))
			case "probe":
				w.Write(frame(streamStdout, "banner: Read-only file system"))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			switch kinds[execID()] {
			case "mkdir":
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
	c := p.attach("abc", "/workspace", "", false)
	if err := c.WriteFileStream(context.Background(), "/newtop/f.txt", strings.NewReader("x"), 1); errors.Is(err, sandbox.ErrNotWritable) ||
		err == nil || !strings.Contains(err.Error(), "mkdir -p /newtop: exit 1") {
		t.Errorf("mkdir's unframed refusal = %v; want the raw error, unclassified", err)
	}
	var pnw *sandbox.PathNotWritableError
	if err := c.WriteFile(context.Background(), "/workspace/f.txt", []byte("x")); !errors.As(err, &pnw) || pnw.Reason != "" {
		t.Errorf("the probe's unframed refusal = %v; want ErrNotWritable with no reason", err)
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
	var probeCmd []string
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			// The refused rename's temporary is still there, and the emptying
			// PUT that follows is this route's cleanup (#310).
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			switch cmd := wrapperCommand(t, body.Cmd); {
			case strings.Contains(cmd, "mv -f"):
				kinds[id] = "rename"
			case strings.Contains(cmd, ": > '/etc/"+sandbox.TempPrefix):
				kinds[id] = "probe"
				probeCmd = body.Cmd
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			switch kinds[execID()] {
			case "rename":
				w.Write(frame(streamStderr, "mv: cannot move '/etc/.map-write-x' to '/etc/f.txt': Permission denied"))
			case "probe":
				w.Write(framedOutput(t, probeCmd, "Permission denied", ""))
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
	c := p.attach("abc", "/workspace", "", false)
	err := c.WriteFile(context.Background(), "/etc/f.txt", []byte("x"))
	if !errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	var pnw *sandbox.PathNotWritableError
	if !errors.As(err, &pnw) || pnw.Reason != "Permission denied" {
		t.Fatalf("err = %v, want the probe's reason carried", err)
	}
}

// tarEntry reads the single member of a tar body: its name and its size, which
// is what the reclaim's assertion is about (#310).
func tarEntry(t *testing.T, body []byte) (string, int64) {
	t.Helper()
	h, err := tar.NewReader(bytes.NewReader(body)).Next()
	if err != nil {
		t.Fatalf("read the tar the daemon was handed: %v", err)
	}
	return h.Name, h.Size
}

// A refused rename's temporary was landed by the daemon's own extraction, in a
// parent the sandbox user cannot write — so its `rm` cannot take it back and the
// refused payload would sit there for the container's life. The daemon empties
// it, by extracting a zero-byte entry of the same name over it, after a HEAD
// that says there is something to empty. Nothing is executed to do it: the
// credential that landed the file is the one that takes it back, and no binary
// of the image's runs with it (#310).
func TestARefusedRenameReclaimsItsTempThroughTheDaemon(t *testing.T) {
	var commands []string
	var reclaimed []byte
	var headed string
	kinds := map[string]string{}
	var probeCmd []string
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			headed = r.URL.Query().Get("path")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			if headed != "" {
				reclaimed, _ = io.ReadAll(r.Body)
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			cmd := wrapperCommand(t, body.Cmd)
			commands = append(commands, cmd)
			switch {
			case strings.Contains(cmd, "mv -f"):
				kinds[id] = "rename"
			case strings.Contains(cmd, ": > '/etc/"+sandbox.TempPrefix):
				kinds[id] = "probe"
				probeCmd = body.Cmd
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			switch kinds[execID()] {
			case "rename":
				w.Write(frame(streamStderr, "mv: cannot move '/etc/.map-write-x' to '/etc/f.txt': Permission denied"))
			case "probe":
				w.Write(framedOutput(t, probeCmd, "Permission denied", ""))
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
	c := p.attach("abc", "/workspace", "", false)
	err := c.WriteFile(context.Background(), "/etc/f.txt", []byte("x"))
	if !errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	if !strings.HasPrefix(headed, "/etc/"+sandbox.TempPrefix) {
		t.Errorf("HEAD asked about %q, want the temporary the daemon landed", headed)
	}
	if reclaimed == nil {
		t.Fatal("the daemon was never handed the emptying archive")
	}
	name, size := tarEntry(t, reclaimed)
	if !strings.HasPrefix(name, sandbox.TempPrefix) || size != 0 {
		t.Errorf("the emptying archive carries %q of %d bytes, want the temporary's own name at 0", name, size)
	}
	// And the cleanup ran nothing: every exec is the write path's own, none of
	// them the removal of a file the sandbox user could not remove anyway.
	for _, cmd := range commands {
		if strings.Contains(cmd, "/bin/rm") {
			t.Errorf("exec %q: the cleanup must execute nothing — the daemon empties what it landed", cmd)
		}
	}
}

// A rename exec that could not run at all — not a reported failure exit, the
// exec itself dying — sheds with the sandbox user's `rm` and nothing else, even
// though the daemon could empty a temporary that `rm` cannot touch.
//
// The daemon can fail this call after having started the script, and then a `mv`
// is still in flight inside the container. Unlinking the temporary makes that
// `mv` fail and leaves the target holding what it held. Emptying it in place
// would let the `mv` succeed onto the target — replacing the caller's data with
// zero bytes while the caller is told the write failed. So this branch keeps the
// residue #310 is about rather than risk that, and the emptying is confined to
// the sites where no script can be running (#310, review round 5).
func TestARenameExecFailureShedsOnlyWithTheSandboxUsersRm(t *testing.T) {
	var commands []string
	var reclaimPuts, heads int
	renames := map[string]bool{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			// Were the emptying to run, this parent would report the temporary
			// still there — the case that most tempts it.
			heads++
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			if len(commands) > 0 {
				reclaimPuts++
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			cmd := wrapperCommand(t, body.Cmd)
			commands = append(commands, cmd)
			renames[id] = strings.Contains(cmd, "mv -f")
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
	c := p.attach("abc", "/workspace", "", false)
	err := c.WriteFile(context.Background(), "/etc/f.txt", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "exec start went away") {
		t.Fatalf("err = %v, want the exec's own failure", err)
	}
	if last := commands[len(commands)-1]; !strings.Contains(last, "rm -f '/etc/"+sandbox.TempPrefix) {
		t.Errorf("last exec = %q, want the sandbox user's own rm", last)
	}
	if heads != 0 || reclaimPuts != 0 {
		t.Errorf("the emptying ran (%d HEADs, %d archives) where a mv may still be in flight; "+
			"it would land zero bytes on the target the caller was told kept its own", heads, reclaimPuts)
	}
}

// tarEntries reads every member of a tar body: names and sizes, which is what
// the batch's emptying archive is asserted on (#316).
func tarEntries(t *testing.T, body []byte) []struct {
	name string
	size int64
} {
	t.Helper()
	var out []struct {
		name string
		size int64
	}
	tr := tar.NewReader(bytes.NewReader(body))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read the tar the daemon was handed: %v", err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatalf("read entry %s: %v", h.Name, err)
		}
		out = append(out, struct {
			name string
			size int64
		}{h.Name, h.Size})
	}
}

// The batch's half of TestARefusedRenameReclaimsItsTempThroughTheDaemon (#316).
// A batch refused under a parent the sandbox user cannot write is refused at
// every member's `mv`, and the script's own `rm` — the sandbox user's — cannot
// take back what the daemon's root extraction landed. So it says what is still
// there and the daemon empties all of it in ONE archive: ten thousand members
// are ten thousand round trips otherwise, and re-running the discard exec here
// would only repeat the `rm` that already failed.
func TestABulkFaultReclaimsItsMembersThroughTheDaemon(t *testing.T) {
	var commands []string
	var puts [][]byte
	var at []string
	kinds := map[string]string{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			puts = append(puts, body)
			at = append(at, r.URL.Query().Get("path"))
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			cmd := wrapperCommand(t, body.Cmd)
			commands = append(commands, cmd)
			if strings.Contains(cmd, "__map_bulk_rename") {
				kinds[id] = "rename"
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			if kinds[execID()] == "rename" {
				// Every member's move was refused, and every `rm` after it: the
				// whole payload is still there and the script says so.
				w.Write(frame(streamStdout, "\nmap-bulk-left-begin\nmap-bulk-left 0\nmap-bulk-left 1\n"))
				w.Write(frame(streamStderr, "mv: cannot move: Permission denied\nmap-bulk-fail 0\n"))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			if kinds[execID()] == "rename" {
				io.WriteString(w, `{"Running":false,"ExitCode":1}`)
			} else {
				io.WriteString(w, `{"Running":false,"ExitCode":0}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
	err := c.WriteFiles(context.Background(), []sandbox.FileWrite{
		{Path: "/etc/a.txt", Data: []byte("AAAA")},
		{Path: "/etc/b.txt", Data: []byte("BBBB")},
	})
	if err == nil {
		t.Fatal("the batch reported success where every move was refused")
	}

	// Three deliveries: the bookkeeping, the members, and the emptying — one
	// archive for the whole batch, not one per member, and on this writable
	// root every one of them at `/`, as before #859.
	if len(puts) != 3 {
		t.Fatalf("%d archives delivered, want 3 (bookkeeping, members, emptying)", len(puts))
	}
	if strings.Join(at, " ") != "/ / /" {
		t.Errorf("archives delivered at %v, want all three at /", at)
	}
	members, emptying := tarEntries(t, puts[1]), tarEntries(t, puts[2])
	if len(emptying) != len(members) {
		t.Fatalf("the emptying archive carries %d entries, want the %d members that were left",
			len(emptying), len(members))
	}
	for i, e := range emptying {
		if e.name != members[i].name {
			t.Errorf("emptying entry %d is %q, want the member's own temporary %q", i, e.name, members[i].name)
		}
		if e.size != 0 {
			t.Errorf("emptying entry %s is %d bytes, want 0: the payload is what has to go", e.name, e.size)
		}
		if members[i].size == 0 {
			t.Errorf("member %s was delivered empty, so this row proves nothing", members[i].name)
		}
	}

	// It executed nothing to do it — the invariant #310 landed — and it did not
	// re-run the discard, whose `rm` had already been refused.
	if last := commands[len(commands)-1]; !strings.Contains(last, "__map_bulk_rename") {
		t.Errorf("last exec = %q, want the rename: the emptying runs no command at all", last)
	}
	for _, cmd := range commands {
		if strings.Contains(cmd, "__map_bulk_discard") {
			t.Errorf("exec %q ran on the fault branch, where the script's own rm has already failed", cmd)
		}
	}
}

// bulkPut is one archive delivery a fake daemon took: the directory the request
// named, and the entries the archive carried.
type bulkPut struct {
	dir     string
	entries []struct {
		name string
		size int64
	}
}

// bulkAnswer is what a fake daemon's exec says for one of the batch's shell
// functions: its exit code and its two streams.
type bulkAnswer struct {
	code           int
	stdout, stderr string
}

// bulkDaemon is a fake daemon for one batch on a container whose root is
// read-only or not. refuse answers each archive PUT by the directory it names —
// a status and message, or 0 to take it — and answers maps a bulk function's
// name to what its exec says; any other exec exits 0 and says nothing. It
// returns the handle, and the PUTs and the functions the execs called, in order.
func bulkDaemon(t *testing.T, readOnlyRoot bool, refuse func(dir string) (int, string),
	answers map[string]bulkAnswer) (*container, *[]bulkPut, *[]string) {
	t.Helper()
	var puts []bulkPut
	var calls []string
	kinds := map[string]string{}
	var execN int
	// Handlers may overlap — an emptying the client gave up on is still being
	// answered when the next arrives — so what they record is locked.
	var mu sync.Mutex
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			dir := r.URL.Query().Get("path")
			mu.Lock()
			puts = append(puts, bulkPut{dir: dir, entries: tarEntries(t, body)})
			mu.Unlock()
			if status, msg := refuse(dir); status != 0 {
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"message":%q}`, msg)
				return
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			mu.Lock()
			execN++
			id := fmt.Sprintf("e%d", execN)
			// The function a bulk exec calls is its command's last line.
			lines := strings.Split(strings.TrimSpace(wrapperCommand(t, body.Cmd)), "\n")
			kinds[id], _, _ = strings.Cut(lines[len(lines)-1], " ")
			calls = append(calls, kinds[id])
			mu.Unlock()
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			mu.Lock()
			a := answers[kinds[execID]]
			mu.Unlock()
			if a.stdout != "" {
				w.Write(frame(streamStdout, a.stdout))
			}
			if a.stderr != "" {
				w.Write(frame(streamStderr, a.stderr))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			mu.Lock()
			code := answers[kinds[execID]].code
			mu.Unlock()
			fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, code)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	return p.attach("abc", "/workspace", "", readOnlyRoot), &puts, &calls
}

// readOnlyDaemon is the daemon's own rule on a read-only root (#859, measured
// with `docker cp` into a `--read-only` container): an extraction at a directory
// that does not resolve into a writable mount is refused, `/` included, whatever
// the entries below it name. The fake cannot resolve a symlink; the live
// contract rows cover that half.
func readOnlyDaemon(dir string) (int, string) {
	for _, m := range sandbox.WritablePaths("/workspace") {
		if dir == m || strings.HasPrefix(dir, m+"/") {
			return 0, ""
		}
	}
	return http.StatusBadRequest, "container rootfs is marked read-only"
}

func acceptAll(string) (int, string) { return 0, "" }

// platformBatch is the platform's own three callers in one batch: a skill under
// the workdir, a memory store under /mnt, package credentials under /tmp — with
// new nested directories, and members sitting directly in a mount point.
func platformBatch() []sandbox.FileWrite {
	return []sandbox.FileWrite{
		{Path: "/workspace/skills/pack/SKILL.md", Data: []byte("skill")},
		{Path: "/workspace/skills/pack/scripts/deep/run.sh", Data: []byte("#!/bin/sh\n")},
		{Path: "/mnt/memory/notes/todo.md", Data: []byte("rw"), Mode: 0o666},
		{Path: "/workspace/skills/pack/README.md", Data: []byte("readme")},
		{Path: "/mnt/memory/.sync/memstore_1", Data: []byte("baseline")},
		{Path: "/tmp/.map-pkgcreds-1/.netrc", Data: []byte("machine x"), Mode: 0o600},
		{Path: "/tmp/b.txt", Data: []byte("b")},
	}
}

// On a read-only root a batch is extracted a directory at a time, each entry
// named by its base name, so the daemon checks every directory a member lands in
// — and every one of the platform's own shapes lands under its rule. The
// bookkeeping comes first, at the workdir; then one extraction per directory,
// in the order each first appears in the batch.
func TestABulkWriteOnAReadOnlyRootExtractsEachDirectory(t *testing.T) {
	c, puts, _ := bulkDaemon(t, true, readOnlyDaemon, nil)
	files := platformBatch()
	if err := c.WriteFiles(context.Background(), files); err != nil {
		t.Fatalf("bulk write under a read-only root: %v (extractions: %+v)", err, *puts)
	}
	var dirs []string
	for _, put := range *puts {
		dirs = append(dirs, put.dir)
		for _, e := range put.entries {
			if strings.Contains(e.name, "/") {
				t.Errorf("entry %q at %s names a directory; the daemon checks only the one the request names", e.name, put.dir)
			}
		}
	}
	want := []string{"/workspace", "/workspace/skills/pack", "/workspace/skills/pack/scripts/deep",
		"/mnt/memory/notes", "/mnt/memory/.sync", "/tmp/.map-pkgcreds-1", "/tmp"}
	if strings.Join(dirs, " ") != strings.Join(want, " ") {
		t.Errorf("extractions at %v, want %v", dirs, want)
	}
	// Each member's temporary lands in its own target's directory, which is what
	// keeps its rename atomic, and the pack directory's two arrive together.
	for _, f := range files {
		found := false
		for _, put := range *puts {
			for _, e := range put.entries {
				found = found || put.dir == gopath.Dir(f.Path) && strings.HasPrefix(e.name, sandbox.TempPrefix)
			}
		}
		if !found {
			t.Errorf("no temporary landed in %s", gopath.Dir(f.Path))
		}
	}
	if n := len((*puts)[1].entries); n != 2 {
		t.Errorf("the pack directory's extraction carries %d entries, want SKILL.md's and README.md's", n)
	}
}

// On a writable root nothing changed from before #859: the bookkeeping is one
// extraction at `/` and the members another, each named by its path made
// relative — two round trips for the batch, whatever its shape.
func TestABulkWriteOnAWritableRootIsOneExtractionAtTheRoot(t *testing.T) {
	c, puts, _ := bulkDaemon(t, false, acceptAll, nil)
	files := platformBatch()
	if err := c.WriteFiles(context.Background(), files); err != nil {
		t.Fatalf("bulk write: %v", err)
	}
	if len(*puts) != 2 || (*puts)[0].dir != "/" || (*puts)[1].dir != "/" {
		t.Fatalf("extractions %+v, want two, both at /", *puts)
	}
	if bk := (*puts)[0].entries; len(bk) != 2 || !strings.HasPrefix(bk[0].name, "workspace/"+sandbox.TempPrefix) {
		t.Errorf("bookkeeping entries %+v, want the manifest and the directory list, relative to /", bk)
	}
	members := (*puts)[1].entries
	if len(members) != len(files) {
		t.Fatalf("the members' extraction carries %d entries, want %d", len(members), len(files))
	}
	for i, f := range files {
		if dir := gopath.Dir("/" + members[i].name); dir != gopath.Dir(f.Path) {
			t.Errorf("member %d is %q, want its temporary in %s, named relative to /", i, members[i].name, gopath.Dir(f.Path))
		}
	}
}

// A members delivery stops at the first extraction the daemon refuses: what
// follows it is never sent, and the batch is shed. The refusal is then asked
// about in the single write's terms, BEFORE the shed takes the manifest the
// question reads — and what it answers is what the caller gets.
func TestABulkDeliveryStopsAtTheFirstRefusedExtraction(t *testing.T) {
	refused := "/workspace/skills/pack/scripts"
	c, puts, calls := bulkDaemon(t, true, func(dir string) (int, string) {
		if dir == refused {
			return http.StatusBadRequest, "container rootfs is marked read-only"
		}
		return 0, ""
	}, map[string]bulkAnswer{
		"__map_bulk_refused": {code: sandbox.ExitPathNotWritable, stderr: "\nmap-bulk-unwritable 0 Read-only file system\n"},
		"__map_bulk_discard": {stdout: "\nmap-bulk-left-begin\n"},
	})
	err := c.WriteFiles(context.Background(), []sandbox.FileWrite{
		{Path: refused + "/run.sh", Data: []byte("x")},
		{Path: "/workspace/skills/pack/SKILL.md", Data: []byte("y")},
		{Path: "/mnt/memory/notes/todo.md", Data: []byte("z")},
	})
	var pnw *sandbox.PathNotWritableError
	if !errors.As(err, &pnw) || pnw.Path != refused+"/run.sh" || pnw.Reason != "Read-only file system" {
		t.Errorf("err = %v, want ErrNotWritable for %s/run.sh: Read-only file system", err, refused)
	}
	var dirs []string
	for _, put := range *puts {
		dirs = append(dirs, put.dir)
	}
	// The bookkeeping, then the first members extraction — refused — and
	// nothing after it; the shed's `rm` took everything, so nothing is emptied.
	if want := []string{"/workspace", refused}; strings.Join(dirs, " ") != strings.Join(want, " ") {
		t.Errorf("extractions at %v, want %v", dirs, want)
	}
	if want := "__map_bulk_prepare __map_bulk_refused __map_bulk_discard"; strings.Join(*calls, " ") != want {
		t.Errorf("execs %v, want %s: the refusal asked about before the shed", *calls, want)
	}
}

// What the refusal is asked about is answered as WriteFile answers it: a target
// that is itself a directory — a mount point such as /tmp, whose temporary is
// bound for `/` and so refused on a read-only root before the rename pass could
// say why — is ErrIsDirectory; and a pass that finds no member at fault leaves
// the daemon's own error standing.
func TestABulkRefusalIsAnsweredInTheSingleWritesTerms(t *testing.T) {
	c, _, _ := bulkDaemon(t, true, readOnlyDaemon, map[string]bulkAnswer{
		"__map_bulk_refused": {code: sandbox.ExitPathIsDirectory, stderr: "\nmap-bulk-fail 1\n"},
	})
	err := c.WriteFiles(context.Background(), []sandbox.FileWrite{
		{Path: "/tmp/fine.txt", Data: []byte("x")},
		{Path: "/tmp", Data: []byte("onto a mount point")},
	})
	if !errors.Is(err, sandbox.ErrIsDirectory) || !strings.Contains(err.Error(), "/tmp:") {
		t.Errorf("err = %v, want ErrIsDirectory naming /tmp", err)
	}

	// The daemon's refusal still says which extraction it was for.
	c, _, _ = bulkDaemon(t, true, readOnlyDaemon, nil)
	err = c.WriteFiles(context.Background(), []sandbox.FileWrite{{Path: "/etc/x.conf", Data: []byte("x")}})
	if err == nil || !strings.Contains(err.Error(), "extract at /etc: ") ||
		!strings.Contains(err.Error(), "container rootfs is marked read-only") || !statusIs(err, http.StatusBadRequest) {
		t.Errorf("err = %v, want the daemon's own refusal, naming /etc, where no member answered", err)
	}
}

// A members delivery refused at its SECOND directory has already landed the
// first: the shed is asked for both ways, and what its `rm` could not take is
// emptied as a read-only root delivers — at the landed member's own directory
// and at the workdir for the bookkeeping, never at `/`, which that root refuses.
func TestABulkRefusedAtALaterDirectoryEmptiesWhatEarlierOnesLanded(t *testing.T) {
	c, puts, calls := bulkDaemon(t, true, readOnlyDaemon, map[string]bulkAnswer{
		"__map_bulk_refused": {code: sandbox.ExitPathNotWritable, stderr: "\nmap-bulk-unwritable 1 Read-only file system\n"},
		// The sandbox user's `rm` could take neither the member that landed nor
		// the manifest.
		"__map_bulk_discard": {stdout: "\nmap-bulk-left-begin\nmap-bulk-left 0\nmap-bulk-left m\n"},
	})
	err := c.WriteFiles(context.Background(), []sandbox.FileWrite{
		{Path: "/workspace/skills/pack/a", Data: []byte("AAAA")},
		{Path: "/etc/b", Data: []byte("BBBB")},
	})
	var pnw *sandbox.PathNotWritableError
	if !errors.As(err, &pnw) || pnw.Path != "/etc/b" {
		t.Errorf("err = %v, want ErrNotWritable for /etc/b", err)
	}
	if want := "__map_bulk_prepare __map_bulk_refused __map_bulk_discard"; strings.Join(*calls, " ") != want {
		t.Errorf("execs %v, want %s", *calls, want)
	}
	var dirs []string
	for _, put := range *puts {
		dirs = append(dirs, put.dir)
	}
	// The bookkeeping, the pack directory (landed), /etc (refused), then the
	// emptying — concurrent, so in either order: the pack member at its own
	// directory, the manifest at the workdir.
	if len(dirs) != 5 {
		t.Fatalf("archives at %v, want five", dirs)
	}
	if want := []string{"/workspace", "/workspace/skills/pack", "/etc"}; !slices.Equal(dirs[:3], want) {
		t.Errorf("deliveries at %v, want %v", dirs[:3], want)
	}
	emptied := slices.Sorted(slices.Values(dirs[3:]))
	if want := []string{"/workspace", "/workspace/skills/pack"}; !slices.Equal(emptied, want) {
		t.Fatalf("emptying at %v, want %v", dirs[3:], want)
	}
	landed := (*puts)[1].entries
	for _, put := range (*puts)[3:] {
		switch e := put.entries; put.dir {
		case "/workspace/skills/pack":
			if len(e) != 1 || e[0].name != landed[0].name || e[0].size != 0 || landed[0].size == 0 {
				t.Errorf("the pack directory's emptying is %+v, want the landed member %+v at 0 bytes", e, landed)
			}
		case "/workspace":
			if len(e) != 1 || e[0].size != 0 || strings.HasSuffix(e[0].name, ".dirs") || strings.Contains(e[0].name, "/") {
				t.Errorf("the workdir's emptying is %+v, want the manifest alone, by its base name, at 0 bytes", e)
			}
		}
	}
}

// Each of a read-only root's emptyings gets a cleanup budget of its own. With
// every worker held by an emptying the daemon answers too slowly, the directory
// queued behind them still gets a full budget once one gives up, and reaches the
// daemon — where one shared budget would have run out with the first round and
// left every later directory's payload in place.
func TestEachEmptyingHasABudgetOfItsOwn(t *testing.T) {
	budget := cleanupBudget
	cleanupBudget = 200 * time.Millisecond
	t.Cleanup(func() { cleanupBudget = budget })

	dirs := emptyingWorkers + 1
	var n int
	var mu sync.Mutex
	c, puts, _ := bulkDaemon(t, true, func(string) (int, string) {
		mu.Lock()
		n++
		// The bookkeeping and one delivery per directory come first; the first
		// round of emptyings after them stalls past its budget.
		stall := n > 1+dirs && n <= 1+dirs+emptyingWorkers
		mu.Unlock()
		if stall {
			time.Sleep(cleanupBudget + 300*time.Millisecond)
		}
		return 0, ""
	}, map[string]bulkAnswer{
		"__map_bulk_rename": {code: 1, stdout: leftReport(dirs),
			stderr: "mv: cannot move: Permission denied\nmap-bulk-fail 0\n"},
	})
	if err := c.WriteFiles(context.Background(), batchAcross(dirs)); err == nil {
		t.Fatal("the batch reported success where every move was refused")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := len(*puts) - (1 + dirs); got != dirs {
		t.Errorf("%d emptyings reached the daemon, want all %d though the first round outlived its budget", got, dirs)
	}
}

// A daemon that answers no emptying at all holds a failed batch for the
// emptying's deadline, not a budget per directory: here 200 directories, which
// at eight at a time on per-request budgets alone would take 25 budgets, return
// within the deadline's six.
func TestAStalledDaemonEmptiesWithinTheDeadline(t *testing.T) {
	budget := cleanupBudget
	cleanupBudget = 100 * time.Millisecond
	t.Cleanup(func() { cleanupBudget = budget })

	dirs := 200
	var n int
	var mu sync.Mutex
	c, puts, _ := bulkDaemon(t, true, func(string) (int, string) {
		mu.Lock()
		n++
		stall := n > 1+dirs // every emptying
		mu.Unlock()
		if stall {
			time.Sleep(3 * cleanupBudget)
		}
		return 0, ""
	}, map[string]bulkAnswer{
		"__map_bulk_rename": {code: 1, stdout: leftReport(dirs),
			stderr: "mv: cannot move: Permission denied\nmap-bulk-fail 0\n"},
	})
	start := time.Now()
	if err := c.WriteFiles(context.Background(), batchAcross(dirs)); err == nil {
		t.Fatal("the batch reported success where every move was refused")
	}
	took := time.Since(start)
	// The deadline is 600ms; uncapped, 25 rounds of 100ms would be 2.5s.
	if limit := emptyingDeadline() + 500*time.Millisecond; took > limit {
		t.Errorf("the failed batch held its caller %v, want within the emptying's deadline (%v) plus slack", took, limit)
	}
	mu.Lock()
	defer mu.Unlock()
	if tried := len(*puts) - (1 + dirs); tried == 0 || tried >= dirs {
		t.Errorf("%d of %d emptyings were tried, want some — eight at a time — and not all of them before the deadline", tried, dirs)
	}
}

// batchAcross is a batch of one member in each of dirs directories under /tmp.
func batchAcross(dirs int) []sandbox.FileWrite {
	files := make([]sandbox.FileWrite, dirs)
	for i := range files {
		files[i] = sandbox.FileWrite{Path: fmt.Sprintf("/tmp/d%d/f", i), Data: []byte("PAYLOAD")}
	}
	return files
}

// leftReport is a shed's report that every one of n members is still there.
func leftReport(n int) string {
	var r strings.Builder
	r.WriteString("\nmap-bulk-left-begin\n")
	for i := range n {
		fmt.Fprintf(&r, "map-bulk-left %d\n", i)
	}
	return r.String()
}

// On a read-only root the emptying is cut as the deliveries were — one
// extraction per directory a left-behind file sits in, zero-byte entries named
// by base name — and every one is tried: a refused emptying leaves the next
// directory's payload no reason to stay.
func TestABulkFaultOnAReadOnlyRootEmptiesEachDirectory(t *testing.T) {
	pack := "/workspace/skills/pack"
	seen := map[string]int{}
	var mu sync.Mutex
	c, puts, _ := bulkDaemon(t, true, func(dir string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		// The pack directory's second delivery is its emptying, and it is
		// refused.
		if seen[dir]++; dir == pack && seen[dir] == 2 {
			return http.StatusInternalServerError, "daemon gave up"
		}
		return 0, ""
	}, map[string]bulkAnswer{
		"__map_bulk_rename": {code: 1,
			stdout: "\nmap-bulk-left-begin\nmap-bulk-left 0\nmap-bulk-left 1\nmap-bulk-left 2\nmap-bulk-left m\n",
			stderr: "mv: cannot move: Permission denied\nmap-bulk-fail 0\n"},
	})
	files := []sandbox.FileWrite{
		{Path: pack + "/a", Data: []byte("AAAA")},
		{Path: "/mnt/memory/notes/b", Data: []byte("BBBB")},
		{Path: pack + "/c", Data: []byte("CCCC")},
	}
	if err := c.WriteFiles(context.Background(), files); err == nil {
		t.Fatal("the batch reported success where every move was refused")
	}
	// Bookkeeping, two members extractions, then all three emptyings — the
	// others tried though one was refused. They run concurrently, so in no
	// particular order.
	if len(*puts) != 6 {
		t.Fatalf("%d archives delivered (%+v), want 6", len(*puts), *puts)
	}
	var emptied []string
	for _, put := range (*puts)[3:] {
		emptied = append(emptied, put.dir)
		for _, e := range put.entries {
			if e.size != 0 || strings.Contains(e.name, "/") || !strings.HasPrefix(e.name, sandbox.TempPrefix) {
				t.Errorf("emptying entry %q at %s is %d bytes, want a temporary's base name at 0", e.name, put.dir, e.size)
			}
		}
		if put.dir == pack && (len(put.entries) != 2 || put.entries[0].name != (*puts)[1].entries[0].name ||
			put.entries[1].name != (*puts)[1].entries[1].name) {
			t.Errorf("the pack directory's emptying carries %+v, want both of its members' temporaries", put.entries)
		}
	}
	slices.Sort(emptied)
	if want := []string{"/mnt/memory/notes", "/workspace", pack}; !slices.Equal(emptied, want) {
		t.Errorf("emptying at %v, want %v", emptied, want)
	}
}

// The three branches above the rename exec — a refused bookkeeping delivery, a
// refused prepare, a refused members delivery — shed BOTH ways, because no
// rename script has run yet and so none can be mid-`mv`. That is `shedBulk`: the
// sandbox user's `rm` first, then the daemon emptying what that `rm` reported it
// could not take (#316).
//
// The row also pins the other half of the guard: a shed that removed everything
// must send no emptying archive at all, or the cleanup would create the litter it
// exists to remove — the batch's form of what #310 pinned for the single write.
func TestABulkShedsBothWaysBeforeTheRenameCanRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		report   string
		wantPuts int
	}{
		// The discard's `rm` was refused, so the daemon is asked to empty the
		// two members it named. Deliveries: bookkeeping, members, emptying.
		{"sheds what the rm could not take", "\nmap-bulk-left-begin\nmap-bulk-left 0\nmap-bulk-left 1\n", 3},
		// The discard's `rm` worked, so there is nothing to empty and no
		// archive may be sent.
		{"sends nothing when the rm worked", "\nmap-bulk-left-begin\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var commands []string
			var puts [][]byte
			kinds := map[string]string{}
			var execN int
			p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				execID := func() string {
					return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
				}
				switch {
				case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
					body, _ := io.ReadAll(r.Body)
					puts = append(puts, body)
					// The members' delivery is the one that fails, so the batch
					// never reaches its rename exec.
					if len(puts) == 2 {
						w.WriteHeader(http.StatusInternalServerError)
						io.WriteString(w, `{"message":"daemon gave up mid-transfer"}`)
						return
					}
					w.WriteHeader(http.StatusOK)
				case strings.HasSuffix(r.URL.Path, "/exec"):
					var body execConfig
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode exec create: %v", err)
					}
					execN++
					id := fmt.Sprintf("e%d", execN)
					cmd := wrapperCommand(t, body.Cmd)
					commands = append(commands, cmd)
					if strings.Contains(cmd, "__map_bulk_discard") {
						kinds[id] = "discard"
					}
					fmt.Fprintf(w, `{"Id":%q}`, id)
				case strings.HasSuffix(r.URL.Path, "/start"):
					w.WriteHeader(http.StatusOK)
					if kinds[execID()] == "discard" {
						w.Write(frame(streamStdout, tc.report))
					}
				case strings.HasSuffix(r.URL.Path, "/json"):
					io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
			})
			c := p.attach("abc", "/workspace", "", false)
			err := c.WriteFiles(context.Background(), []sandbox.FileWrite{
				{Path: "/etc/a.txt", Data: []byte("AAAA")},
				{Path: "/etc/b.txt", Data: []byte("BBBB")},
			})
			if err == nil {
				t.Fatal("the batch reported success where its members were never delivered")
			}
			var shed bool
			for _, cmd := range commands {
				shed = shed || strings.Contains(cmd, "__map_bulk_discard")
			}
			if !shed {
				t.Errorf("execs = %q, want the sandbox user's own shed to have run first", commands)
			}
			if len(puts) != tc.wantPuts {
				t.Fatalf("%d archives delivered, want %d", len(puts), tc.wantPuts)
			}
			if tc.wantPuts == 2 {
				return // nothing was left, so nothing is emptied
			}
			for _, e := range tarEntries(t, puts[2]) {
				if e.size != 0 || !strings.Contains(e.name, sandbox.TempPrefix) {
					t.Errorf("emptying entry %s is %d bytes, want one of the batch's temporaries at 0", e.name, e.size)
				}
			}
		})
	}
}

// A batch that succeeded still ran a `rm` that can be refused: its last act is
// to remove the manifest and the directory list, which the daemon's own root
// extraction landed in the workdir. An image whose sandbox user cannot write
// there keeps both, however well the members landed — so the emptying is asked
// for on the success path too (#316).
func TestASuccessfulBulkStillEmptiesBookkeepingItCouldNotRemove(t *testing.T) {
	var puts [][]byte
	kinds := map[string]string{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			puts = append(puts, body)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			if strings.Contains(wrapperCommand(t, body.Cmd), "__map_bulk_rename") {
				kinds[id] = "rename"
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			if kinds[execID()] == "rename" {
				// Every member landed; only the bookkeeping would not go.
				w.Write(frame(streamStdout, "\nmap-bulk-left-begin\nmap-bulk-left m\nmap-bulk-left d\n"))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
	if err := c.WriteFiles(context.Background(), []sandbox.FileWrite{
		{Path: "/workspace/a.txt", Data: []byte("AAAA")},
	}); err != nil {
		t.Fatalf("the batch failed where every member landed: %v", err)
	}
	if len(puts) != 3 {
		t.Fatalf("%d archives delivered, want 3 — a successful batch must still empty what its own rm could not", len(puts))
	}
	emptying := tarEntries(t, puts[2])
	if len(emptying) != 2 {
		t.Fatalf("the emptying archive carries %d entries, want the manifest and the directory list", len(emptying))
	}
	for _, e := range emptying {
		if e.size != 0 || !strings.Contains(e.name, sandbox.TempPrefix) {
			t.Errorf("emptying entry %s is %d bytes, want a bookkeeping file at 0", e.name, e.size)
		}
	}
}

// Deleting the manifest is all it would take to defeat the emptying: the shed
// reads it to know what to walk, it lives in the sandbox's own workdir, and the
// docker backend delivers it a round trip before the exec that reads it (#206
// names that window). A pass with no list removes nothing and can name nothing,
// so the platform answers with the list it has held all along — safe only here,
// downstream of a members delivery the daemon accepted, where everything is
// landed and nothing was removed (#316).
func TestABulkThatLostItsManifestEmptiesThePlatformsOwnList(t *testing.T) {
	var puts [][]byte
	kinds := map[string]string{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			puts = append(puts, body)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			if strings.Contains(wrapperCommand(t, body.Cmd), "__map_bulk_rename") {
				kinds[id] = "rename"
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			if kinds[execID()] == "rename" {
				// The manifest was deleted under it, so the pass walked
				// nothing, removed nothing, and says exactly that.
				w.Write(frame(streamStdout, "\nmap-bulk-left-begin\nmap-bulk-left-nolist\n"))
				w.Write(frame(streamStderr, "map-bulk-fail 0\n"))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			if kinds[execID()] == "rename" {
				fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, sandbox.ExitBulkIncomplete)
			} else {
				io.WriteString(w, `{"Running":false,"ExitCode":0}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
	err := c.WriteFiles(context.Background(), []sandbox.FileWrite{
		{Path: "/etc/a.txt", Data: []byte("AAAA")},
		{Path: "/etc/b.txt", Data: []byte("BBBB")},
	})
	if err == nil {
		t.Fatal("the batch reported success where its manifest had been deleted")
	}
	if len(puts) != 3 {
		t.Fatalf("%d archives delivered, want 3 — the emptying must not be skipped for want of a report", len(puts))
	}
	// Every member, and ONLY the members. The pass removed the two bookkeeping
	// files itself on this very branch and looked at them afterwards, so its own
	// report answers for those — emptying them from the platform's list would
	// recreate as zero-byte files exactly what the shed had just deleted.
	emptying := tarEntries(t, puts[2])
	members := tarEntries(t, puts[1])
	if len(emptying) != len(members) {
		t.Fatalf("the emptying archive carries %d entries, want the %d members and nothing else",
			len(emptying), len(members))
	}
	for i, e := range emptying {
		if e.name != members[i].name {
			t.Errorf("emptying entry %d is %q, want the member's own temporary %q", i, e.name, members[i].name)
		}
		if e.size != 0 {
			t.Errorf("emptying entry %s is %d bytes, want 0", e.name, e.size)
		}
	}
}

// The other half of the same branch: bookkeeping the shed *did* say it could not
// remove is emptied, because there the report is speaking accurately about files
// it looked at. A pass with no list still looks at those two.
func TestABulkThatLostItsManifestStillEmptiesBookkeepingItNamed(t *testing.T) {
	var puts [][]byte
	kinds := map[string]string{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			puts = append(puts, body)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			if strings.Contains(wrapperCommand(t, body.Cmd), "__map_bulk_rename") {
				kinds[id] = "rename"
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
			if kinds[execID()] == "rename" {
				// No list to walk, and the directory list survived its own rm.
				w.Write(frame(streamStdout, "\nmap-bulk-left-begin\nmap-bulk-left-nolist\nmap-bulk-left d\n"))
				w.Write(frame(streamStderr, "map-bulk-fail 0\n"))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			if kinds[execID()] == "rename" {
				fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, sandbox.ExitBulkIncomplete)
			} else {
				io.WriteString(w, `{"Running":false,"ExitCode":0}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
	if err := c.WriteFiles(context.Background(), []sandbox.FileWrite{
		{Path: "/etc/a.txt", Data: []byte("AAAA")},
	}); err == nil {
		t.Fatal("the batch reported success where its manifest had been deleted")
	}
	if len(puts) != 3 {
		t.Fatalf("%d archives delivered, want 3", len(puts))
	}
	// The one member from the platform's list, plus the one bookkeeping file the
	// shed named — never the manifest, which it removed and did not name.
	emptying := tarEntries(t, puts[2])
	if len(emptying) != 2 {
		t.Fatalf("the emptying archive carries %d entries, want the member and the named directory list", len(emptying))
	}
	if !strings.HasSuffix(emptying[1].name, ".dirs") {
		t.Errorf("second emptying entry is %q, want the directory list the shed named", emptying[1].name)
	}
}

// The batch's half of TestARenameExecFailureShedsOnlyWithTheSandboxUsersRm: a
// rename exec that could not run at all may have left a `mv` in flight, so this
// branch sheds with the sandbox user's `rm` and stops — even when the discard
// pass reports members it could not take. Emptying one under a live `mv` would
// put zero bytes onto the member's target (#310, review round 5).
func TestABulkRenameExecFailureShedsOnlyWithTheSandboxUsersRm(t *testing.T) {
	var commands []string
	var puts int
	kinds := map[string]string{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			puts++
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			cmd := wrapperCommand(t, body.Cmd)
			commands = append(commands, cmd)
			switch {
			case strings.Contains(cmd, "__map_bulk_rename"):
				kinds[id] = "rename"
			case strings.Contains(cmd, "__map_bulk_discard"):
				kinds[id] = "discard"
			}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/start"):
			if kinds[execID()] == "rename" {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"message":"exec start went away"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			if kinds[execID()] == "discard" {
				// The temptation: the shed says the whole payload is still
				// there, and the daemon could take it back.
				w.Write(frame(streamStdout, "\nmap-bulk-left-begin\nmap-bulk-left 0\nmap-bulk-left 1\n"))
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
	err := c.WriteFiles(context.Background(), []sandbox.FileWrite{
		{Path: "/etc/a.txt", Data: []byte("AAAA")},
		{Path: "/etc/b.txt", Data: []byte("BBBB")},
	})
	if err == nil || !strings.Contains(err.Error(), "exec start went away") {
		t.Fatalf("err = %v, want the exec's own failure", err)
	}
	if last := commands[len(commands)-1]; !strings.Contains(last, "__map_bulk_discard") {
		t.Errorf("last exec = %q, want the sandbox user's own shed", last)
	}
	// Two deliveries and no more: the bookkeeping and the members. A third would
	// be the emptying, landing zero bytes under a `mv` that may still be running.
	if puts != 2 {
		t.Errorf("%d archives delivered, want 2 — the emptying must not run where a mv may be in flight", puts)
	}
}

// A failed move over a parent that can take a create keeps the raw error: the
// failure was the transfer's, not the target's, and calling the path unwritable
// would tell the model the path is bad when it is not.
func TestRenameFailureOnAWritableTargetKeepsTheRawError(t *testing.T) {
	var commands []string
	reclaimPuts := 0
	renames := map[string]bool{}
	var execN int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		execID := func() string {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exec/"), "/start"), "/json")
		}
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			// This parent takes the user's own rm, so the script already
			// removed the temporary: nothing is left to empty.
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Could not find the file"}`)
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			if len(commands) > 0 {
				reclaimPuts++
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var body execConfig
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			execN++
			id := fmt.Sprintf("e%d", execN)
			cmd := wrapperCommand(t, body.Cmd)
			commands = append(commands, cmd)
			renames[id] = strings.Contains(cmd, "mv -f")
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
	c := p.attach("abc", "/workspace", "", false)
	err := c.WriteFile(context.Background(), "/workspace/f.txt", []byte("x"))
	if errors.Is(err, sandbox.ErrNotWritable) {
		t.Fatalf("err = %v; a writable target must keep the raw error", err)
	}
	if err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("err = %v, want the move's own failure", err)
	}
	if last := commands[len(commands)-1]; !strings.Contains(last, ": > '/workspace/"+sandbox.TempPrefix) {
		t.Errorf("last exec = %q, want the writability probe asked after the failed move", last)
	}
	// A temporary the script's own rm already removed is not recreated as an
	// empty file: the HEAD is what keeps the cleanup from making litter (#310).
	if reclaimPuts != 0 {
		t.Errorf("%d emptying archives sent, want none — nothing was left to empty", reclaimPuts)
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
	c := p.attach("abc", "/workspace", "", false)
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
	var cmd []string
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
			commands = append(commands, wrapperCommand(t, body.Cmd))
			cmd = body.Cmd
			io.WriteString(w, `{"Id":"e1"}`)
		case r.URL.Path == "/exec/e1/start":
			if _, _, framed := sandboxtest.Unwrap(wrapperCommand(t, cmd)); framed {
				w.Write(framedOutput(t, cmd, "", "mkdir: cannot create directory '/workspace/a': Read-only file system\n"))
			}
		case r.URL.Path == "/exec/e1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":1}`)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
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
// — here it fails too, since this daemon refuses every exec, which is exactly why
// the attempt is what gets asserted rather than its outcome.
//
// This daemon refuses the exec *create*, so nothing could have started and the
// emptying would in fact be safe here. It still does not run: `Exec` reports one
// error for a create that was refused and for a stream that dropped after the
// script began, so the branch cannot tell the two apart and treats both as the
// dangerous one (#310).
func TestWriteFileShedsItsTempWhenTheRenameCannotRun(t *testing.T) {
	var execs, emptied int
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/containers/abc/archive" && r.Method == http.MethodPut:
			if execs > 0 {
				emptied++
			}
		case strings.HasSuffix(r.URL.Path, "/exec"):
			execs++
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"cannot create exec"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := p.attach("abc", "/workspace", "", false)
	if err := c.WriteFile(context.Background(), "/workspace/f.txt", []byte("x")); err == nil {
		t.Fatal("write returned nil, want the failed rename")
	}
	if execs != 2 {
		t.Errorf("%d exec attempts, want 2 — the rename, then the removal of what it could not name", execs)
	}
	if emptied != 0 {
		t.Errorf("%d emptying archives, want none — an exec that failed may still be running a mv", emptied)
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
		return p.attach("abc", "/workspace", "", false)
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
	c := p.attach("abc", "/workspace", "", false)
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
			c := p.attach("abc", "/workspace", "", false)
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
	c := p.attach("abc", "/workspace", "", false)
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
	c := p.attach("abc", "/workspace", "", false)

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
	c := p.attach("abc", "/workspace", "", false)

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
		return p.attach("abc", "/workspace", "", false)
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
	stdout, stderr, cut, err := demux(bytes.NewReader(raw), 1024)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if string(stdout) != "out1out2" || string(stderr) != "err1" || cut.stdout || cut.stderr {
		t.Errorf("stdout=%q stderr=%q cut=%+v", stdout, stderr, cut)
	}
}

// Past the cap the payload is drained, not buffered — the command must be free
// to finish, and later frames on the other stream must still arrive. Each
// stream has a cap of its own, and the cut names the stream it cut.
func TestDemuxCapsEachStreamAndKeepsReading(t *testing.T) {
	raw := bytes.Join([][]byte{
		frame(1, strings.Repeat("a", 10)), frame(1, strings.Repeat("b", 10)), frame(2, "kept"),
	}, nil)
	stdout, stderr, cut, err := demux(bytes.NewReader(raw), 4)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if string(stdout) != "aaaa" || !cut.stdout {
		t.Errorf("stdout=%q cut=%+v", stdout, cut)
	}
	if string(stderr) != "kept" || cut.stderr {
		t.Errorf("stderr=%q cut=%+v — capping stdout lost or cut the other stream", stderr, cut)
	}

	raw = bytes.Join([][]byte{frame(1, "out"), frame(2, strings.Repeat("e", 10))}, nil)
	stdout, stderr, cut, err = demux(bytes.NewReader(raw), 4)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if string(stdout) != "out" || cut.stdout || string(stderr) != "eeee" || !cut.stderr {
		t.Errorf("stdout=%q stderr=%q cut=%+v; want stderr alone cut", stdout, stderr, cut)
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
