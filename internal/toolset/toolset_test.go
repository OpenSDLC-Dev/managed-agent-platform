package toolset_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

const testImage = "debian:stable-slim"

// runner gives a test one real container, from testImage unless an option
// says otherwise, and a Runner over it. Each subtest works under its own
// directory beneath the workdir, and bash subtests take a fresh session so they
// never inherit another's shell state. A missing daemon is a hard failure, as
// with the other suites — skipping would hollow out the coverage gate.
//
// The provider resolves its daemon itself, and every docker CLI call in these
// tests names that same address (docker.DaemonHost), so a fixture the CLI
// builds or starts is on the daemon the provider uses.
func runner(t *testing.T, opts ...runnerOption) toolset.Runner {
	t.Helper()
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("toolset tests require Docker: %v", err)
	}
	spec := sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	}
	r := toolset.Runner{Session: domain.NewID("sesn")}
	for _, o := range opts {
		o(&spec, &r)
	}
	sb, err := provider.Provision(context.Background(), spec)
	if err != nil {
		t.Fatalf("provision %s: %v", spec.Image, err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})
	r.Sandbox = sb
	return r
}

// runnerOption shapes the sandbox runner provisions and the Runner over it.
type runnerOption func(*sandbox.Spec, *toolset.Runner)

// fromImage provisions the sandbox from image rather than testImage.
func fromImage(image string) runnerOption {
	return func(s *sandbox.Spec, _ *toolset.Runner) { s.Image = image }
}

// hardened provisions the sandbox with h.
func hardened(h sandbox.Hardening) runnerOption {
	return func(s *sandbox.Spec, _ *toolset.Runner) { s.Hardening = h }
}

// inWorkdir makes workdir both the sandbox's — where an exec starts — and the
// Runner's, where relative paths resolve and grep runs rg.
func inWorkdir(workdir string) runnerOption {
	return func(s *sandbox.Spec, r *toolset.Runner) { s.Workdir, r.Workdir = workdir, workdir }
}

// call runs one tool and fails the test on an infrastructure error — the tests
// below are about what the model sees, which is the Result.
func call(t *testing.T, r toolset.Runner, name, input string) toolset.Result {
	t.Helper()
	res, err := r.Run(context.Background(), domain.NewID("sevt"), name, json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s(%s): %v", name, input, err)
	}
	return res
}

// ok asserts a successful tool call and returns its content.
func ok(t *testing.T, r toolset.Runner, name, input string) string {
	t.Helper()
	res := call(t, r, name, input)
	if res.IsError {
		t.Fatalf("%s(%s) is an error result: %s", name, input, res.Content)
	}
	return res.Content
}

// fails asserts an error result whose content mentions want.
func fails(t *testing.T, r toolset.Runner, name, input, want string) string {
	t.Helper()
	res := call(t, r, name, input)
	if !res.IsError {
		t.Fatalf("%s(%s) succeeded, want an error result: %s", name, input, res.Content)
	}
	if !strings.Contains(res.Content, want) {
		t.Fatalf("%s(%s) error = %q, want it to mention %q", name, input, res.Content, want)
	}
	return res.Content
}

func TestBash(t *testing.T) {
	r := runner(t)

	t.Run("runs a command and captures stdout", func(t *testing.T) {
		got := ok(t, r, "bash", `{"command":"echo hello"}`)
		if strings.TrimSpace(got) != "hello" {
			t.Fatalf("content = %q, want hello", got)
		}
	})

	t.Run("captures stderr", func(t *testing.T) {
		got := ok(t, r, "bash", `{"command":"echo oops >&2"}`)
		if !strings.Contains(got, "oops") {
			t.Fatalf("content = %q, want it to carry stderr", got)
		}
	})

	t.Run("state persists across calls", func(t *testing.T) {
		r := r
		r.Session = domain.NewID("sesn")
		ok(t, r, "bash", `{"command":"cd /tmp && export MARKER=carried"}`)
		if got := ok(t, r, "bash", `{"command":"pwd; echo $MARKER"}`); !strings.Contains(got, "/tmp") ||
			!strings.Contains(got, "carried") {
			t.Fatalf("content = %q, want the shell's cwd and export to have carried", got)
		}
	})

	t.Run("restart resets the shell", func(t *testing.T) {
		r := r
		r.Session = domain.NewID("sesn")
		ok(t, r, "bash", `{"command":"cd /tmp"}`)
		if got := ok(t, r, "bash", `{"restart":true}`); !strings.Contains(got, "restarted") {
			t.Fatalf("restart content = %q, want it to report the restart", got)
		}
		if got := ok(t, r, "bash", `{"command":"pwd"}`); !strings.Contains(got, "/workspace") {
			t.Fatalf("after restart pwd = %q, want the workdir", got)
		}
	})

	t.Run("restart with a command resets and then runs it", func(t *testing.T) {
		r := r
		r.Session = domain.NewID("sesn")
		ok(t, r, "bash", `{"command":"cd /tmp"}`)
		if got := ok(t, r, "bash", `{"restart":true,"command":"pwd"}`); !strings.Contains(got, "/workspace") {
			t.Fatalf("content = %q, want the command to run in the reset shell", got)
		}
	})

	t.Run("a nonzero exit is an error result carrying the code", func(t *testing.T) {
		got := fails(t, r, "bash", `{"command":"echo partial; exit 3"}`, "exit code: 3")
		if !strings.Contains(got, "partial") {
			t.Fatalf("content = %q, want the output the command did produce", got)
		}
	})

	t.Run("an oversized failure output spills whole to a sandbox file", func(t *testing.T) {
		// The failure arms cap before dispatch, so the spill must hook ahead
		// of capWithTrailer — the exit-code trailer and the spill notice both
		// survive, and the whole payload is byte-exact in the file. The
		// payload is deterministic so the file can be checked by size and
		// digest, not just its tail: the expected bytes are stdout, a joining
		// newline, and stderr's marker — exactly what combine assembles.
		unit := "0123456789abcdef\n"
		payload := strings.Repeat(unit, 180000/len(unit)+1)[:180000]
		want := payload + "\n" + "marker\n"
		got := fails(t, r, "bash", `{"command":"yes 0123456789abcdef | head -c 180000; echo marker >&2; exit 3"}`, "exit code: 3")
		i := strings.Index(got, "/tmp/tool_outputs/")
		if i < 0 {
			t.Fatalf("content = %q..., want the spill path named", got[:80])
		}
		j := strings.Index(got[i:], ".txt")
		path := got[i : i+j+4]
		count, err := strconv.Atoi(strings.TrimSpace(ok(t, r, "bash", fmt.Sprintf(`{"command":"wc -c < %s"}`, path))))
		if err != nil || count != len(want) {
			t.Fatalf("spill file holds %d bytes (%v), want exactly %d", count, err, len(want))
		}
		sum := sha256.Sum256([]byte(want))
		digest := ok(t, r, "bash", fmt.Sprintf(`{"command":"sha256sum %s"}`, path))
		if !strings.HasPrefix(digest, hex.EncodeToString(sum[:])) {
			t.Errorf("spill digest = %q, want %s — the file is not byte-identical to the output", digest, hex.EncodeToString(sum[:]))
		}
	})

	t.Run("a timeout is an error result and does not report an exit code", func(t *testing.T) {
		got := fails(t, r, "bash", `{"command":"sleep 30","timeout_ms":500}`, "timed out")
		if strings.Contains(got, "exit code") {
			t.Fatalf("content = %q, want no exit code on a timeout (TimedOut is the authoritative field)", got)
		}
	})

	t.Run("a command is required", func(t *testing.T) {
		fails(t, r, "bash", `{}`, "command is required")
	})

	t.Run("malformed input is an error result", func(t *testing.T) {
		fails(t, r, "bash", `{"command":42}`, "invalid bash input")
	})
}

func TestReadWriteEdit(t *testing.T) {
	r := runner(t)

	t.Run("write creates parent directories and reports the byte count", func(t *testing.T) {
		got := ok(t, r, "write", `{"file_path":"rw/deep/a.txt","content":"one\ntwo\nthree\n"}`)
		if got != "wrote 14 bytes to rw/deep/a.txt" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("read returns the file", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"rw/deep/a.txt"}`); got != "one\ntwo\nthree\n" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("an absolute path inside the workdir reads the same file", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"/workspace/rw/deep/a.txt"}`); got != "one\ntwo\nthree\n" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("view_range slices 1-indexed inclusive lines", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[2,3]}`); got != "two\nthree" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("a view_range end of 0 means to end of file", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[2,0]}`); got != "two\nthree\n" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("a start line past the end reads empty", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[99,100]}`); got != "" {
			t.Fatalf("content = %q, want empty", got)
		}
	})

	t.Run("an inverted view_range selects nothing and reads empty", func(t *testing.T) {
		// The reference toolset stopped answering [3,1] with an error ("An
		// inverted range selects nothing", since anthropic-sdk-go v1.63.0 —
		// fs.go execRead): the model gets empty content, not is_error.
		if got := ok(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[3,1]}`); got != "" {
			t.Fatalf("content = %q, want empty", got)
		}
	})

	t.Run("a malformed view_range is an error result", func(t *testing.T) {
		fails(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[2]}`, "view_range")
	})

	t.Run("a missing file is an error result", func(t *testing.T) {
		fails(t, r, "read", `{"file_path":"rw/nope.txt"}`, "no such file")
	})

	t.Run("a directory is an error result", func(t *testing.T) {
		fails(t, r, "read", `{"file_path":"rw/deep"}`, "not a regular file")
	})

	// A non-regular file (a FIFO) is the model's path mistake, not the sandbox
	// failing — it is a tool error it can recover from, never a backend fault.
	t.Run("a non-regular file is an error result", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkfifo /workspace/rw/fifo"}`)
		fails(t, r, "read", `{"file_path":"rw/fifo"}`, "not a regular file")
		fails(t, r, "edit", `{"file_path":"rw/fifo","old_string":"a","new_string":"b"}`, "not a regular file")
	})

	t.Run("file_path is required", func(t *testing.T) {
		fails(t, r, "read", `{}`, "file_path is required")
		fails(t, r, "write", `{"content":"x"}`, "file_path is required")
		fails(t, r, "edit", `{"old_string":"a","new_string":"b"}`, "file_path is required")
	})

	t.Run("edit replaces a unique occurrence", func(t *testing.T) {
		ok(t, r, "write", `{"file_path":"rw/e.txt","content":"alpha beta gamma\n"}`)
		if got := ok(t, r, "edit", `{"file_path":"rw/e.txt","old_string":"beta","new_string":"BETA"}`); got !=
			"edited rw/e.txt (1 replacement(s))" {
			t.Fatalf("content = %q", got)
		}
		if got := ok(t, r, "read", `{"file_path":"rw/e.txt"}`); got != "alpha BETA gamma\n" {
			t.Fatalf("file = %q", got)
		}
	})

	t.Run("edit requires a unique match unless replace_all", func(t *testing.T) {
		ok(t, r, "write", `{"file_path":"rw/m.txt","content":"x x x\n"}`)
		fails(t, r, "edit", `{"file_path":"rw/m.txt","old_string":"x","new_string":"y"}`, "must be unique")
		if got := ok(t, r, "edit",
			`{"file_path":"rw/m.txt","old_string":"x","new_string":"y","replace_all":true}`); got !=
			"edited rw/m.txt (3 replacement(s))" {
			t.Fatalf("content = %q", got)
		}
		if got := ok(t, r, "read", `{"file_path":"rw/m.txt"}`); got != "y y y\n" {
			t.Fatalf("file = %q", got)
		}
	})

	t.Run("edit reports an old_string it cannot find", func(t *testing.T) {
		fails(t, r, "edit", `{"file_path":"rw/e.txt","old_string":"absent","new_string":"x"}`, "not found")
	})

	t.Run("edit requires a non-empty old_string", func(t *testing.T) {
		fails(t, r, "edit", `{"file_path":"rw/e.txt","old_string":"","new_string":"x"}`, "old_string is required")
	})

	t.Run("edit of a missing file is an error result", func(t *testing.T) {
		fails(t, r, "edit", `{"file_path":"rw/nope.txt","old_string":"a","new_string":"b"}`, "no such file")
	})

	t.Run("write overwrites", func(t *testing.T) {
		ok(t, r, "write", `{"file_path":"rw/o.txt","content":"first"}`)
		ok(t, r, "write", `{"file_path":"rw/o.txt","content":"second"}`)
		if got := ok(t, r, "read", `{"file_path":"rw/o.txt"}`); got != "second" {
			t.Fatalf("file = %q", got)
		}
	})

	t.Run("bash sees what the file tools wrote", func(t *testing.T) {
		if got := ok(t, r, "bash", `{"command":"cat /workspace/rw/o.txt"}`); !strings.Contains(got, "second") {
			t.Fatalf("bash saw %q", got)
		}
	})
}

// A file_path too long for Linux — resolving past PATH_MAX's 4095 bytes, or
// holding a name past NAME_MAX's 255 — is a tool error naming the bound on
// every backend, where the k8s backend would hand it to an exec that cannot
// start and Docker's archive endpoint would answer a 500, both faults a
// reclaim would only repeat. So, for write and edit, is a path whose
// directory leaves no room for the 27-byte temporary name a write lands
// under beside the file, which both backends refused as the path's own
// "file name too long", though the path is within the bounds. One at the
// bounds is the sandbox's, written, read and edited as any other — the
// largest directory a write may land in, 4067 bytes, included.
func TestFilePathsTooLongForLinux(t *testing.T) {
	filePathBounds(t, runner(t))
}

// The same in a Kubernetes pod, under a read-only root as the chart runs one:
// the cluster MAP_K8S_CONTEXT names.
func TestFilePathsTooLongForLinuxInAKubernetesPod(t *testing.T) {
	filePathBounds(t, podRunner(t))
}

// podRunner gives a test a Runner over one pod from testImage, under a
// read-only root as the chart runs one, in the cluster MAP_K8S_CONTEXT names.
// A missing cluster is a hard failure, as with the k8s contract test.
func podRunner(t *testing.T) toolset.Runner {
	t.Helper()
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
	})
	if err != nil {
		t.Fatalf("this test requires a Kubernetes cluster: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
		Hardening:  sandbox.Hardening{ReadOnlyRootfs: true},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
	return toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
}

// filePathBounds is TestFilePathsTooLongForLinux on the sandbox r runs in.
func filePathBounds(t *testing.T, r toolset.Runner) {
	t.Helper()
	// Fifteen 255-byte directories under /workspace, then a name that brings
	// the resolved path to 4095 bytes, or one past.
	dirs := strings.Repeat(strings.Repeat("d", 255)+"/", 15)
	atBound, pastBound := dirs+strings.Repeat("f", 244), dirs+strings.Repeat("f", 245)
	inputs := map[string]func(p string) string{
		"read":  func(p string) string { return `{"file_path":"` + p + `"}` },
		"write": func(p string) string { return `{"file_path":"` + p + `","content":"x"}` },
		"edit":  func(p string) string { return `{"file_path":"` + p + `","old_string":"x","new_string":"y"}` },
	}
	for _, tool := range []string{"read", "write", "edit"} {
		in := inputs[tool]
		fails(t, r, tool, in(pastBound),
			tool+": file name too long: the file_path resolves to a 4096-byte path, over the 4095 bytes a Linux path can hold; shorten it")
		fails(t, r, tool, in("/workspace/"+pastBound), "resolves to a 4096-byte path")
		fails(t, r, tool, in("bounds/"+strings.Repeat("n", 256)),
			tool+": file name too long: the file_path holds a 256-byte name, over the 255 bytes a Linux file name can hold; shorten it")
		// Past what one exec argument carries, which the k8s backend's
		// would have been.
		fails(t, r, tool, in(strings.Repeat("x/", 100<<10)), "resolves to a 204810-byte path")
	}
	// A 4095-byte path whose directory leaves no room for the 27-byte
	// temporary name a write lands under beside it: refused by write and
	// edit, and read as any other path is.
	short := dirs + strings.Repeat("e", 242) + "/f"
	for _, tool := range []string{"write", "edit"} {
		fails(t, r, tool, inputs[tool](short), tool+": file name too long: the file lands first under a 27-byte temporary name beside it, "+
			"and in the file_path's 4093-byte directory that is a 4121-byte path, over the 4095 bytes a Linux path can hold; shorten it")
	}
	fails(t, r, "read", inputs["read"](short), "read "+short+": no such file or directory")
	// The largest a write's directory may be, 4067 bytes, with a 27-byte
	// name: the target and the temporary are both 4095-byte paths.
	tightest := dirs + strings.Repeat("e", 216) + "/" + strings.Repeat("f", 27)
	for _, p := range []string{atBound, tightest, "bounds/" + strings.Repeat("n", 255)} {
		if got, want := ok(t, r, "write", `{"file_path":"`+p+`","content":"one x"}`), "wrote 5 bytes to "+p; got != want {
			t.Fatalf("write at the bound = %q, want %q", got, want)
		}
		ok(t, r, "edit", `{"file_path":"`+p+`","old_string":"x","new_string":"two"}`)
		if got := ok(t, r, "read", `{"file_path":"`+p+`"}`); got != "one two" {
			t.Fatalf("read at the bound = %q, want the edited file", got)
		}
	}
}

// A file_path within Linux's bounds can still make a command past what one
// exec argument carries: Docker's rename quotes the path into its script
// eleven times, and quoting makes each `'` four bytes. The path the model
// chose is what made it long, so it is the model's tool error, naming the
// command's size — not a fault a reclaim would only repeat — and the write
// lands nothing: the file an edit was given keeps its bytes, and no
// temporary is left beside it.
func TestAQuoteHeavyFilePathMakesACommandTooLong(t *testing.T) {
	r := runner(t)
	p := quotePath
	const tooLong = "-byte command, over the 122880 bytes one exec argument can carry; shorten it"
	size := func(content, verb string) int {
		t.Helper()
		got, ok := strings.CutPrefix(content, verb+": the file_path makes a ")
		got, whole := strings.CutSuffix(got, tooLong)
		n, err := strconv.Atoi(got)
		if !ok || !whole || err != nil || n <= sandbox.MaxCommandBytes {
			t.Fatalf("%s of a %d-byte path of quotes = %q; want the command's size, past the bound", verb, len(p), content)
		}
		return n
	}
	in, _ := json.Marshal(map[string]string{"file_path": p, "content": "y"})
	t.Logf("a %d-byte path of quotes makes a %d-byte rename", len(p), size(fails(t, r, "write", string(in), tooLong), "write"))

	// The file is put there by bash, which hands the path to no command.
	ok(t, r, "bash", `{"command":"q=$(printf '%255s' '' | tr ' ' \"'\"); d=/workspace; `+
		`for i in $(seq 15); do d=$d/$q; done; printf x > \"$d/${q:11}\""}`)
	in, _ = json.Marshal(map[string]string{"file_path": p, "old_string": "x", "new_string": "y"})
	size(fails(t, r, "edit", string(in), tooLong), "edit")
	in, _ = json.Marshal(map[string]string{"file_path": p})
	if got := ok(t, r, "read", string(in)); got != "x" {
		t.Errorf("read after the refused edit = %q, want the file as it was", got)
	}
	if left := ok(t, r, "bash", `{"command":"find /workspace -name '.map-write-*'"}`); left != "" {
		t.Errorf("the refused writes left %q behind", left)
	}
}

// quotePath is fifteen 255-byte directories under /workspace, then a 244-byte
// name: a 4095-byte path, every byte past /workspace/ but the slashes a quote.
var quotePath = "/workspace/" + strings.Repeat(strings.Repeat("'", 255)+"/", 15) + strings.Repeat("'", 244)

// The k8s backend hands the path to its write script as an argument, quoted
// into no command, so the path Docker refuses is one a pod writes, edits and
// reads as any other (docs/DIVERGENCES.md).
func TestAQuoteHeavyFilePathInAKubernetesPod(t *testing.T) {
	r := podRunner(t)
	in, _ := json.Marshal(map[string]string{"file_path": quotePath, "content": "one x"})
	if got, want := ok(t, r, "write", string(in)), "wrote 5 bytes to "+quotePath; got != want {
		t.Fatalf("write = %q, want %q", got, want)
	}
	in, _ = json.Marshal(map[string]string{"file_path": quotePath, "old_string": "x", "new_string": "two"})
	ok(t, r, "edit", string(in))
	in, _ = json.Marshal(map[string]string{"file_path": quotePath})
	if got := ok(t, r, "read", string(in)); got != "one two" {
		t.Fatalf("read = %q, want the edited file", got)
	}
}

func TestGlob(t *testing.T) {
	r := runner(t)
	// Distinct, ascending mtimes: newest-first ordering is part of the contract,
	// and stat's nanosecond precision would otherwise tie same-second writes.
	ok(t, r, "bash", `{"command":"mkdir -p g/sub && `+
		`touch -d '2020-01-01' g/old.go && touch -d '2021-01-01' g/sub/mid.go && `+
		`touch -d '2022-01-01' g/new.go && touch -d '2023-01-01' g/note.txt && `+
		`touch -d '2024-01-01' g/.hidden.go"}`)

	t.Run("doublestar matches at any depth, newest first", func(t *testing.T) {
		got := ok(t, r, "glob", `{"pattern":"g/**/*.go"}`)
		want := "/workspace/g/.hidden.go\n/workspace/g/new.go\n/workspace/g/sub/mid.go\n/workspace/g/old.go"
		if got != want {
			t.Fatalf("content =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("a single star does not cross a directory separator", func(t *testing.T) {
		got := ok(t, r, "glob", `{"pattern":"g/*.go"}`)
		if strings.Contains(got, "sub/mid.go") {
			t.Fatalf("content = %q, want no nested match", got)
		}
		if !strings.Contains(got, "g/new.go") {
			t.Fatalf("content = %q, want the top-level match", got)
		}
	})

	t.Run("path scopes the search root", func(t *testing.T) {
		got := ok(t, r, "glob", `{"pattern":"**/*.go","path":"g/sub"}`)
		if got != "/workspace/g/sub/mid.go" {
			t.Fatalf("content = %q", got)
		}
	})

	// An absolute pattern names its own root, so a path argument — even one that
	// does not exist — is irrelevant to it (the reference sets root to "/").
	t.Run("an absolute pattern ignores the path root", func(t *testing.T) {
		got := ok(t, r, "glob", `{"pattern":"/workspace/g/*.go","path":"does-not-exist"}`)
		if !strings.Contains(got, "/workspace/g/new.go") {
			t.Fatalf("content = %q, want the absolute pattern's matches", got)
		}
	})

	// stat's %n echoes a filename raw, newlines included; the whole pipeline is
	// NUL-delimited so a name carrying a fake stat record stays one record. The
	// matched file comes back with its newline intact, and the second line that
	// looks like a stat record ("<mtime> FAKE") is never split off into its own
	// path — the mark of the bug this pins.
	t.Run("a newline in a matched name cannot fabricate a path", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkdir -p g4 && touch $'g4/real\n9999999999 FAKE'"}`)
		got := ok(t, r, "glob", `{"pattern":"g4/*"}`)
		if got != "/workspace/g4/real\n9999999999 FAKE" {
			t.Fatalf("content = %q, want the one real path with its newline intact", got)
		}
	})

	t.Run("no matches is not an error", func(t *testing.T) {
		if got := ok(t, r, "glob", `{"pattern":"g/**/*.rs"}`); got != "no matches" {
			t.Fatalf("content = %q", got)
		}
	})

	// The pattern is one word, whatever it contains: the script empties IFS, so
	// a space in it is a character in a filename and not a field separator.
	t.Run("a pattern may contain a space", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkdir -p 'g3' && touch 'g3/two words.go'"}`)
		if got := ok(t, r, "glob", `{"pattern":"g3/two w*.go"}`); got != "/workspace/g3/two words.go" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("a missing search root is an error result", func(t *testing.T) {
		fails(t, r, "glob", `{"pattern":"*","path":"g/absent"}`, "no such")
	})

	// The search is one exec argument, which Linux caps near 128 KiB: a
	// pattern that would push it past is refused before anything runs, where
	// the exec would otherwise fail before it started.
	t.Run("a pattern too long for one exec argument", func(t *testing.T) {
		exactly := func(in, want string) {
			t.Helper()
			if got := ok(t, r, "glob", in); got != want {
				t.Fatalf("glob(%.40s…) = %q, want %q", in, got, want)
			}
		}
		exactly(`{"pattern":"`+strings.Repeat("z", 100<<10)+`","path":"g"}`, "no matches")
		fails(t, r, "glob", `{"pattern":"`+strings.Repeat("z", 130<<10)+`","path":"g"}`,
			"glob: the pattern and path make a ")
		fails(t, r, "glob", `{"pattern":"*","path":"`+strings.Repeat("p", 125<<10)+`"}`,
			"over the 122880 bytes one exec argument can carry; shorten them")
	})

	t.Run("pattern is required", func(t *testing.T) {
		fails(t, r, "glob", `{}`, "pattern is required")
	})

	// The pattern reaches bash as the value of a variable, and bash does not
	// rescan an expansion's result for command substitution — it only globs it.
	// This pins that: the payload matches no file and is never run.
	t.Run("a pattern with shell metacharacters is data, not code", func(t *testing.T) {
		if got := ok(t, r, "glob", `{"pattern":"$(touch /tmp/pwned)/*.go"}`); got != "no matches" {
			t.Fatalf("content = %q, want no matches", got)
		}
		if out := ok(t, r, "bash", `{"command":"test -e /tmp/pwned && echo INJECTED || echo clean"}`); !strings.Contains(out, "clean") {
			t.Fatalf("the glob pattern was executed: %q", out)
		}
	})

	// A file whose own name carries a metacharacter is still just a file.
	t.Run("a metacharacter in a matched name is not re-expanded", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkdir -p g2 && touch 'g2/$(touch evil).go'"}`)
		if got := ok(t, r, "glob", `{"pattern":"g2/*.go"}`); !strings.Contains(got, "$(touch evil).go") {
			t.Fatalf("content = %q, want the literally-named file", got)
		}
		if out := ok(t, r, "bash", `{"command":"test -e evil && echo INJECTED || echo clean"}`); !strings.Contains(out, "clean") {
			t.Fatalf("the matched name was executed: %q", out)
		}
	})
}

func TestGrep(t *testing.T) {
	r := runner(t)
	ok(t, r, "write", `{"file_path":"gr/a.txt","content":"alpha\nneedle 42\nomega\n"}`)
	ok(t, r, "write", `{"file_path":"gr/b.txt","content":"nothing here\n"}`)

	// The recorded schema's default output mode is files_with_matches.
	t.Run("by default a search lists the files that match", func(t *testing.T) {
		if got := ok(t, r, "grep", `{"pattern":"needle","path":"gr"}`); got != "/workspace/gr/a.txt" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("content matches carry path, line number and text", func(t *testing.T) {
		got := ok(t, r, "grep", `{"pattern":"needle","path":"gr","output_mode":"content"}`)
		if got != "/workspace/gr/a.txt:2:needle 42" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("a perl character class works", func(t *testing.T) {
		got := ok(t, r, "grep", `{"pattern":"needle \\d+","path":"gr","output_mode":"content"}`)
		if !strings.Contains(got, "needle 42") {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("no matches is not an error", func(t *testing.T) {
		if got := ok(t, r, "grep", `{"pattern":"absent","path":"gr"}`); got != "no matches" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("an invalid regex is an error result", func(t *testing.T) {
		fails(t, r, "grep", `{"pattern":"[unclosed","path":"gr"}`, "rg: regex parse error")
	})

	t.Run("a missing search root is an error result", func(t *testing.T) {
		fails(t, r, "grep", `{"pattern":"x","path":"gr/absent"}`, "/workspace/gr/absent: IO error")
	})

	t.Run("pattern is required", func(t *testing.T) {
		fails(t, r, "grep", `{}`, "pattern is required")
	})

	t.Run("binary files and ignored trees are skipped", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkdir -p gr/.git gr/node_modules && printf 'needle\\0bin' > gr/bin.dat && `+
			`echo needle > gr/node_modules/dep.txt && echo node_modules/ > gr/.gitignore"}`)
		got := ok(t, r, "grep", `{"pattern":"needle","path":"gr","output_mode":"content"}`)
		if strings.Contains(got, "bin.dat") || strings.Contains(got, "node_modules") {
			t.Fatalf("content = %q, want binary and git-ignored files skipped", got)
		}
	})

	t.Run("a pattern with shell metacharacters is data, not code", func(t *testing.T) {
		ok(t, r, "grep", `{"pattern":"$(touch /tmp/pwned2)","path":"gr"}`)
		if out := ok(t, r, "bash", `{"command":"test -e /tmp/pwned2 && echo INJECTED || echo clean"}`); !strings.Contains(out, "clean") {
			t.Fatalf("the grep pattern was executed: %q", out)
		}
	})

	t.Run("output is capped", func(t *testing.T) {
		ok(t, r, "bash", fmt.Sprintf(`{"command":"mkdir -p big && for i in $(seq 1 %d); do echo needle-line-with-some-padding-$i; done > big/f.txt"}`, 14000))
		got := ok(t, r, "grep", `{"pattern":"needle","path":"big","output_mode":"content"}`)
		cut := strings.LastIndex(got, "\n[output truncated; full output written to /tmp/tool_outputs/")
		if cut < 0 {
			t.Fatalf("content does not report the truncation and the spill file: %q", got[max(0, len(got)-80):])
		}
		if cut > toolset.MaxOutputBytes {
			t.Fatalf("preview body is %d bytes, want it capped at %d", cut, toolset.MaxOutputBytes)
		}
		// The fixture sits under the sandbox's 1 MiB Exec retention (~870 KB
		// with the absolute-path prefixes) so the spill must hold the WHOLE
		// result: no upstream marker, a head byte-identical to the preview the
		// model saw (digest-compared in the sandbox), every match record
		// present, and the tail the preview lost intact.
		if strings.HasPrefix(got, "[output truncated]\n") {
			t.Fatalf("the fixture crossed the Exec retention — shrink it, or completeness below asserts nothing")
		}
		i := strings.Index(got, "/tmp/tool_outputs/")
		path := got[i : i+strings.Index(got[i:], ".txt")+4]
		preview := got[:cut]
		sum := sha256.Sum256([]byte(preview))
		digest := ok(t, r, "bash", fmt.Sprintf(`{"command":"head -c %d %s | sha256sum"}`, len(preview), path))
		if !strings.HasPrefix(digest, hex.EncodeToString(sum[:])) {
			t.Errorf("spill head digest = %q, want the preview's own %s", digest, hex.EncodeToString(sum[:]))
		}
		if n := strings.TrimSpace(ok(t, r, "bash", fmt.Sprintf(`{"command":"grep -c needle-line %s"}`, path))); n != "14000" {
			t.Errorf("spill file holds %s match records, want all 14000", n)
		}
		if tl := ok(t, r, "bash", fmt.Sprintf(`{"command":"tail -c 80 %s"}`, path)); !strings.Contains(tl, "needle-line-with-some-padding-14000") {
			t.Errorf("spill tail = %q, want the last match intact", tl)
		}
	})
}

func TestUnknownTool(t *testing.T) {
	r := runner(t)
	fails(t, r, "web_search", `{"query":"x"}`, "unknown tool")
}

// Each of the six sandbox tools refuses an input property its schema does not
// declare, naming it and what the tool does accept, rather than dropping it
// and running a different call from the one asked for (#827). The Runner has
// no sandbox at all: a refusal must come before anything runs, and a call that
// reached the sandbox would panic here instead.
func TestUnknownInputPropertiesAreRefused(t *testing.T) {
	r := toolset.Runner{Session: domain.NewID("sesn")}
	for _, tc := range []struct {
		tool, input, want string
	}{
		{"bash", `{"command":"touch /tmp/ran","cwd":"/tmp"}`,
			`bash: unknown input property "cwd"; bash accepts command, restart, timeout_ms`},
		{"read", `{"file_path":"a.txt","offset":3,"limit":10}`,
			`read: unknown input properties "limit", "offset"; read accepts file_path, view_range`},
		{"write", `{"file_path":"a.txt","content":"x","mode":"0755"}`,
			`write: unknown input property "mode"; write accepts content, file_path`},
		{"edit", `{"file_path":"a.txt","old_string":"a","new_string":"b","count":2}`,
			`edit: unknown input property "count"; edit accepts file_path, new_string, old_string, replace_all`},
		{"glob", `{"pattern":"*","exclude":"*.md"}`,
			`glob: unknown input property "exclude"; glob accepts path, pattern`},
		{"grep", `{"pattern":"todo","include":"*.go"}`,
			`grep: unknown input property "include"; grep accepts -A, -B, -C, -i, -n, context, glob, ` +
				`head_limit, multiline, offset, output_mode, path, pattern, type`},
		// A key is quoted, so one carrying a NUL or a newline cannot reach
		// the event log raw or forge the message.
		{"glob", `{"pattern":"*","a\u0000b\nc":1}`,
			`glob: unknown input property "a\x00b\nc"; glob accepts path, pattern`},
	} {
		res, err := r.Run(context.Background(), domain.NewID("sevt"), tc.tool, json.RawMessage(tc.input))
		if err != nil {
			t.Fatalf("%s(%s): %v", tc.tool, tc.input, err)
		}
		if !res.IsError || res.Content != tc.want {
			t.Errorf("%s(%s) = %+v, want the is_error refusal %q", tc.tool, tc.input, res, tc.want)
		}
	}
}

// A NUL byte in tool output must never reach the Result: Postgres's jsonb
// cannot store \u0000, so one unsanitized byte - a single byte of /dev/zero
// on stdout is enough - would fault the tool-result append, and a faulted
// work item reclaim-loops re-running the same command forever (#223).
func TestOutputNULBytesAreStripped(t *testing.T) {
	r := runner(t)
	got := ok(t, r, "bash", `{"command":"printf 'a\\0b'"}`)
	if strings.IndexByte(got, 0) >= 0 {
		t.Fatalf("bash output carries a NUL byte: %q", got)
	}
	if strings.TrimSpace(got) != "ab" {
		t.Errorf("content = %q, want ab (the NUL stripped, its neighbors kept)", got)
	}

	// A failing command flooded with NUL must keep its stderr: bash sanitizes
	// before capWithTrailer, so NUL bytes cannot spend the budget the real
	// output needs (the failure arms cap before dispatch's own sanitize runs).
	t.Run("a NUL-flooded failure keeps its stderr", func(t *testing.T) {
		got := fails(t, r, "bash", `{"command":"head -c 200000 /dev/zero; echo marker >&2; exit 3"}`, "exit code: 3")
		if strings.IndexByte(got, 0) >= 0 {
			t.Errorf("content carries a NUL byte")
		}
		if !strings.Contains(got, "marker") {
			t.Errorf("stderr marker lost — NUL bytes spent the output budget")
		}
	})
}
