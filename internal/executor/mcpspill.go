package executor

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
// It spills into the sandbox the session already has and never creates one. The
// MCP driver is server-side on every environment kind and needs no sandbox of
// its own, so provisioning one here would put a container behind every cloud
// session that declares an MCP server — and hand its model a path it has no tool
// to open, since a session with no sandbox is a session whose agent never ran a
// built-in tool. A session with no sandbox truncates exactly as it did before
// the spill existed, and so does a self_hosted session: its tools run in the
// customer's own sandbox, which this process reaches only through the work API,
// and that API has no MCP surface and no way to hand a file to a container it
// does not own. That last is a deliberate divergence (docs/DIVERGENCES.md): the
// reference's behaviour on a BYOC session is unobserved.
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
	// looked records that the sandbox has been looked for this pass, sb nil or
	// not: a session with none is the common case, and asking the endpoint once
	// per call would put a container listing behind every oversized answer.
	looked bool
	// failed records that a write into it has already failed. A sandbox whose
	// disk is full or read-only fails the same way for every remaining call of
	// the pass, and each attempt is a round trip into the container to learn
	// what the first one established.
	failed bool
}

// spillTimeout bounds the whole spill — the sandbox lookup included. The spill
// happens after the tool has run, so a lease lost between the call and the
// append hands the item back with the call unanswered and the reclaim runs it
// again: the executor's documented at-most-once caveat, which a tool with a side
// effect pays for either way. That window existed before this file — rendering
// the blocks sits in it too — and what this bound does is keep the spill from
// widening it by however long a session lock is held.
const spillTimeout = 30 * time.Second

// write spills the answer and returns the notice naming the file, or "" when
// there is nothing to spill, when this session has no sandbox to spill into, or
// when the write could not happen. Every "" leaves the caller with exactly the
// answer it would have produced before.
//
// dropped says the budget threw blocks away, and it is a second trigger rather
// than a refinement of the first: capMCPBlocks charges each block its
// *marshalled* size, so an answer of five thousand short text blocks holds well
// under the budget in text and lands far over it in JSON, and an answer whose
// first block is an oversized image loses everything behind it however small
// that is. Spilling on the answer's text length alone would let exactly those
// answers lose their tail with nothing written — the case this exists to
// prevent.
//
// The other direction is the spillable check below: an answer carrying nothing
// a text file can hold spills nothing.
func (s *mcpSpiller) write(ctx context.Context, id domain.ID, content []mcp.Content, dropped bool) string {
	if s.sess.envConfig.Type != domain.EnvCloud || s.failed {
		return ""
	}
	// One cheap pass, because the common answer is inside the budget and
	// building its text to measure it would copy every answer this driver ever
	// returns. size is a lower bound on what the file would hold; what it misses
	// — the rendering's own words, a block charged its base64 — costs more than
	// it in marshalled JSON, so an answer over the budget on the strength of
	// what size cannot see is an answer capMCPBlocks dropped from, which is what
	// dropped reports. spillable is whether the file would hold any of the
	// answer at all: an image or a blob is bytes no text file can carry, and
	// pointing a model at a file of nothing but sentences saying so would be a
	// promise about content that is not there.
	size, spillable := 0, false
	for _, c := range content {
		switch c.Type {
		case "text", "resource":
			size += len(c.Text)
			spillable = spillable || c.Text != ""
		case "resource_link", "audio":
			// Described rather than carried inline too, so the file holds
			// exactly what the answer would have read as here.
			spillable = true
		}
	}
	if !spillable || (!dropped && size <= toolset.MaxOutputBytes) {
		return ""
	}
	// Nothing this pass writes will be committed once its lease context is dead
	// (runMCPTools stops and answerMCPCalls appends nothing), so taking a
	// session lock for it would spend the wait on an answer already discarded.
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
	// Not toolset.Spill's own sentence, which promises the "full output": this
	// file holds the answer's text, and an answer that also carried an image
	// carried it somewhere this file does not go. The path convention is shared;
	// the sentence says what is true of each.
	return "[the full text of this answer was written to " + path + "]"
}

// sandbox is the session's sandbox if it already has one on this endpoint, and
// nil if it has not — looked up once per pass either way.
//
// Owned is what makes this an adoption rather than a provision: it lists the
// sessions this endpoint currently holds sandbox assets for, so the following
// provisionSandbox call finds the container rather than creating one. Taking the
// session lock on the way is the point of going through it: the lock is what
// keeps this write from landing in a container the reaper is checkpointing and
// about to destroy.
//
// A failure at either step is logged rather than returned. The call has an
// answer, and losing the whole pass over the file it could have spilled into
// would cost more than the truncation does.
func (s *mcpSpiller) sandbox(ctx context.Context) sandbox.Sandbox {
	if s.looked {
		return s.sb
	}
	s.looked = true
	owned, err := s.exec.provider.Owned(ctx)
	if err != nil {
		slog.WarnContext(ctx, "mcp answer not spilled: sandbox lookup failed",
			"session_id", s.sid.String(), "error", err)
		return nil
	}
	if !slices.Contains(owned, s.sid) {
		return nil
	}
	sb, err := s.exec.provisionSandbox(ctx, s.sid, s.sess)
	if err != nil {
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
// from the rendering is the words: a resource is named by the same URI its
// document block is titled with, and a link and an audio block are described in
// the same sentence they are described with inline (mcpexec.go), so the file and
// the answer say the same thing about the same block.
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
			if c.Text != "" {
				section(fmt.Sprintf("The tool returned the resource %s (%s):\n%s",
					capLabel(c.URI), mimeOrUnknown(c.MIMEType), c.Text))
				continue
			}
			section(fmt.Sprintf("The tool returned %d bytes of %s at %s, which is not in this file.",
				len(c.Data), mimeOrUnknown(c.MIMEType), capLabel(c.URI)))
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
