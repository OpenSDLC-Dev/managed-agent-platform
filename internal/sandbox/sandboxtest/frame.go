package sandboxtest

import (
	"regexp"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// BannerHook is an image's `ENV BASH_ENV` file at its most disruptive, the one
// every test of a platform script's frame (sandbox.Frame, #860) runs under: it
// prints on both streams without ending either line, leaves the directory the
// shell started in, and sets an EXIT trap that prints on both after whatever
// the shell ran. The hooked image (internal/sandbox/hookedtest) carries it, and
// the host-side script tests name it as their BASH_ENV file.
const BannerHook = `printf 'welcome to the image '; printf 'stderr banner ' >&2; cd /; ` +
	`trap "printf 'exit banner '; printf 'exit stderr ' >&2" EXIT` + "\n"

// Banners are the words BannerHook prints, which a check reading the output of
// a command the image's startup reaches — the bash tool's — takes out first.
var Banners = []string{"welcome to the image ", "stderr banner ", "exit banner ", "exit stderr "}

// Unbanner is s with BannerHook's words taken out.
func Unbanner(s string) string {
	for _, b := range Banners {
		s = strings.ReplaceAll(s, b, "")
	}
	return s
}

// wrapped matches the head of a command sandbox.Frame.Wrap made: its end
// line, whose label and nonce name the frame.
var wrapped = regexp.MustCompile(`^close_frame\(\) \{ printf '\\n%s\\n' 'map-([a-z0-9]+)-end-([0-9a-f]{16})'; `)

// Unwrap undoes sandbox.Frame.Wrap: the script a framed command carries, and
// its frame. A fake sandbox answers the script as a sandbox would and frames
// its answer (Framed) as the script's run would have printed it.
func Unwrap(command string) (script string, f sandbox.Frame, ok bool) {
	m := wrapped.FindStringSubmatch(command)
	if m == nil {
		return "", sandbox.Frame{}, false
	}
	f = sandbox.FrameOf(m[1], m[2])
	// What Wrap puts around a script, read off an empty one.
	empty := f.Wrap("")
	head := f.Open() + "(\n"
	tail := empty[len(head):]
	if !strings.HasPrefix(command, head) || !strings.HasSuffix(command, tail) || len(command) < len(empty) {
		return "", sandbox.Frame{}, false
	}
	return command[len(head) : len(command)-len(tail)], f, true
}

// Framed is res as a run of a script f frames prints it: each stream between
// the frame's begin and end lines — but for a stream res marks truncated,
// which the cap cut inside what the script printed, so it keeps its begin line
// and loses its end line with the rest.
func Framed(f sandbox.Frame, res sandbox.ExecResult) sandbox.ExecResult {
	begin, end := f.Lines()
	frame := func(s string, truncated bool) string {
		s = begin + s
		if !truncated {
			s += end
		}
		return s
	}
	res.Stdout, res.Stderr = frame(res.Stdout, res.StdoutTruncated), frame(res.Stderr, res.StderrTruncated)
	return res
}
