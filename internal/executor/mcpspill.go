package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// mcpSpiller writes an MCP answer the result could not carry whole into the
// session's sandbox, so that a tool result too large for the model's context is
// truncated *and* readable rather than only truncated. It is the same bargain a
// built-in tool's oversized output already gets, and deliberately the same
// convention — toolset.SpillFile, the same directory, one file per call id — so
// a model that has learned where its truncated output goes is right whichever
// tool produced it. What it does not share is the sentence (this file holds the
// answer's text, not "the full output") or the trigger (an MCP answer can lose
// blocks while its own text is small; see write).
//
// It writes into the sandbox the session already has, through Provider.Attach,
// and that is the whole of its dealings with the sandbox layer. Attach creates
// nothing, starts nothing, replaces nothing and restores nothing — which is what
// this caller needs and Provision cannot promise. Provision heals, and healing
// is destructive by design: it rebuilds a container whose gate pairing went
// stale, and it reclaims a gated pod that misses its readiness window. Both are
// right for a caller about to run the model's tools in a sandbox and wrong for
// one writing a file it could equally skip — a spill has no business costing a
// session the container it is working in, or bringing a container into being for
// a session that never had one. So a session with no running sandbox truncates
// exactly as it did before the spill existed. So does a self_hosted session: its
// tools run in the customer's own sandbox, which this process reaches only
// through the work API, and that API has no MCP surface and no way to hand a
// file to a container it does not own. That last is a deliberate divergence
// (docs/DIVERGENCES.md): the reference's behaviour on a BYOC session is
// unobserved.
//
// One bound it shares with the sandbox tools' spill and does not fix: the file
// lives under /tmp, which checkpointRoots does not preserve, so a session idle
// long enough to be reaped comes back without it while the notice naming it
// stays on the append-only log. Moving the directory would move it for both
// callers, which is a decision about the built-in spill (#226) rather than about
// this one.
type mcpSpiller struct {
	exec *Executor
	sid  domain.ID
	sess sessionRun

	sb sandbox.Sandbox
	// failed records that a write has already failed. A sandbox whose disk is
	// full or read-only fails the same way for every remaining call of the pass,
	// and each attempt is a round trip into the container to learn what the
	// first one established. A failed *attach* is not latched: it is one cheap
	// read against the endpoint, and a daemon that blinked between two calls of
	// a pass should cost the second call nothing.
	failed bool
}

// spillTimeout bounds each attach and each write. It is deliberately short and
// deliberately not a provisioning budget: Attach is one inspect, and the write
// is one file. The spill happens after the tool has run, so a lease lost between
// the call and the append hands the item back with the call unanswered and the
// reclaim runs it again — the executor's documented at-most-once caveat, which a
// tool with a side effect pays for either way. That window existed before this
// file (rendering the blocks sits in it too); what this bound does is keep the
// spill from widening it.
const spillTimeout = 30 * time.Second

// write spills the answer and returns the notice naming the file, or "" when
// there is nothing to spill, when this session has no sandbox to spill into, or
// when the write could not happen. Every "" leaves the caller with exactly the
// answer it would have produced before.
//
// lost is the trigger, and it is measured where the loss happens rather than
// inferred from the answer's size: the renderer reports whether it truncated a
// block or the budget dropped one. Inferring was wrong in both directions. A
// size over the budget does not mean anything was lost — capMCPBlocks exempts
// one already-capped block, so two sixty-kilobyte text blocks both arrive whole,
// and a size-triggered spill would hand the model a file it already has and tell
// it to go and read it. A size under the budget does not mean nothing was lost —
// blocks are charged their *marshalled* size, so five thousand short text blocks
// sit well under it in text and far over it in JSON, and an answer whose first
// block is an oversized image loses everything behind it however small that is.
//
// The other direction is the spillable check below: an answer carrying nothing a
// text file can hold spills nothing.
func (s *mcpSpiller) write(ctx context.Context, id domain.ID, content []mcp.Content, lost bool) string {
	if !lost || s.failed || s.sess.envConfig.Type != domain.EnvCloud {
		return ""
	}
	if !spillableText(content) {
		return ""
	}
	// Nothing this pass writes will be committed once its lease context is dead
	// (runMCPTools stops and answerMCPCalls appends nothing), so reaching for a
	// sandbox would spend the round trip on an answer already discarded.
	if ctx.Err() != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, spillTimeout)
	defer cancel()
	sb := s.sandbox(ctx)
	if sb == nil {
		return ""
	}
	path, ok := toolset.SpillFile(ctx, sb, id, mcpAnswerText(content))
	if !ok {
		s.failed = true
		return ""
	}
	// Not toolset's own sentence, which promises the "full output": this file
	// holds the answer's text, and an answer that also carried an image carried
	// it somewhere this file does not go. The path convention is shared; the
	// sentence says what is true of each.
	return "[the full text of this answer was written to " + path + "]"
}

// spillableText reports whether the file mcpAnswerText would write holds any of
// the answer itself, rather than only this platform's sentences about the parts
// of it a text file cannot carry. One cheap pass over the server's content,
// asking exactly what the writer will answer — pointing a model at a file of
// nothing but "which is not in this file" would be a promise about content that
// is not in it.
//
// It has to test what the writer tests, in the order the writer tests it. A
// resource carrying bytes is written as the sentence naming them whatever text
// it also carries, because that is the order resourceBlock reads it in and the
// file may not describe a block differently from the answer; asking on the text
// alone would call a blob-with-an-extraction spillable and then write a file
// holding neither the bytes nor the text. And text is measured sanitized,
// because that is what the file would hold: a block of nothing but NUL bytes is
// text the server sent and nothing this can write.
//
// The go-sdk refuses to *marshal* a resource carrying both text and a blob, so a
// server built on it cannot send one — but nothing checks it on the way in, so a
// server built on anything else can.
func spillableText(content []mcp.Content) bool {
	for _, c := range content {
		switch c.Type {
		case "text":
			if toolset.SanitizeText(c.Text) != "" {
				return true
			}
		case "resource":
			if len(c.Data) == 0 && toolset.SanitizeText(c.Text) != "" {
				return true
			}
		case "resource_link", "audio":
			// Described rather than carried inline too, so the file holds
			// exactly what the answer would have read as here.
			return true
		}
	}
	return false
}

// sandbox is the session's sandbox if it has a running one on this endpoint, and
// nil if it has not.
//
// A miss is the ordinary case and costs one inspect to learn, so it is not
// cached: a pass of several oversized answers pays that inspect each time rather
// than carrying a stale "no" past a sandbox that appeared. A hit is cached
// because the handle is reusable and a second inspect would say nothing new.
//
// An error other than ErrNotFound is logged rather than returned. The call has
// an answer, and losing the whole pass over the file it could have spilled into
// would cost more than the truncation does.
func (s *mcpSpiller) sandbox(ctx context.Context) sandbox.Sandbox {
	if s.sb != nil {
		return s.sb
	}
	sb, err := s.exec.provider.Attach(ctx, s.sid)
	switch {
	case errors.Is(err, sandbox.ErrNotFound):
		return nil
	case err != nil:
		slog.WarnContext(ctx, "mcp answer not spilled: sandbox unavailable",
			"session_id", s.sid.String(), "error", err)
		return nil
	}
	s.sb = sb
	return sb
}

// mcpAnswerText is what the spill file holds: the answer as the model would have
// read it, whole, with none of the caps the inline blocks are held to.
//
// It is written from the server's content rather than from the rendered blocks
// because the rendering is where the truncation happens — a text resource's body
// reaches a document block already through toolset.CapOutput. What it does take
// from the rendering is the words, and the order it tests them in: a block is
// described here by the same sentence mcpResultBlocks describes it with, and a
// resource is read the way resourceBlock reads it — empty first, then bytes,
// then text — so the file and the answer cannot come to say different things
// about one block.
//
// Bytes that are not text say so and are not in the file. An image or a blob is
// bounded by the transport rather than truncated and is nothing a `read` returns
// anything useful for, so it keeps the whole-or-nothing budget rule it already
// had — and a line naming what was there beats a gap where a model cannot tell
// whether the tool returned an image or nothing.
//
// Blocks are joined in the order the server sent them, so the file reads as the
// answer did.
func mcpAnswerText(content []mcp.Content) string {
	var b strings.Builder
	section := func(s string) {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	for _, c := range content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				section(c.Text)
			}
		case "resource":
			switch {
			case len(c.Data) == 0 && toolset.SanitizeText(c.Text) == "":
				section(mcpEmptyResourceText(c))
			case len(c.Data) > 0:
				section(fmt.Sprintf("The tool returned %d bytes of %s at %s, which is not in this file.",
					len(c.Data), mimeOrUnknown(c.MIMEType), capLabel(c.URI)))
			default:
				section(fmt.Sprintf("The tool returned the resource %s (%s):\n%s",
					capLabel(c.URI), mimeOrUnknown(c.MIMEType), c.Text))
			}
		case "resource_link":
			section(mcpLinkText(c))
		case "audio":
			section(mcpAudioText(c))
		case "image":
			section(fmt.Sprintf("The tool returned %d bytes of image data (%s), which is not in this file.",
				len(c.Data), mimeOrUnknown(c.MIMEType)))
		}
	}
	return toolset.SanitizeText(b.String())
}
