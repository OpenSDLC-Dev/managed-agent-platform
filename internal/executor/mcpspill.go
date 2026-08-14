package executor

import (
	"context"
	"log/slog"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// mcpSpiller writes an over-budget MCP answer into the session's sandbox, so
// that a tool result too large for the model's context is truncated *and*
// readable rather than only truncated. It is the same bargain a built-in tool's
// oversized output already gets, and deliberately the same convention —
// toolset.Spill, the same budget, the same directory, one file per call id — so
// a model that has learned where its truncated output goes is right whichever
// tool produced it.
//
// The sandbox is provisioned lazily and at most once a pass. An MCP session need
// never have one: this driver is server-side on every environment kind, and most
// answers fit. Provisioning per pass regardless would put a container behind
// every MCP session on the platform, and provisioning per call would put several
// behind one turn.
//
// Nothing spills on a self_hosted session. Its tools run in the customer's own
// sandbox, which this process reaches only through the work API — and that API
// has no MCP surface and no way to hand a file to a container it does not own.
// The answer truncates exactly as it did before the spill existed, which is a
// deliberate divergence (docs/DIVERGENCES.md): the reference's behaviour on a
// BYOC session is unobserved.
type mcpSpiller struct {
	exec *Executor
	sid  domain.ID
	sess sessionRun

	sb sandbox.Sandbox
	// tried records that provisioning has already failed this pass. A sandbox
	// that cannot be had is not a reason to stop answering calls — the spill is
	// an enhancement, never a new failure mode — but it is a reason not to pay
	// for the attempt again on every remaining call of the same pass.
	tried bool
}

// write spills full when it is over budget and returns the notice naming the
// file, or "" when it fits, when this session has no platform sandbox, or when
// the write could not happen. Every "" leaves the caller with exactly the answer
// it would have produced before.
func (s *mcpSpiller) write(ctx context.Context, id domain.ID, full string) string {
	if len(full) <= toolset.MaxOutputBytes || s.sess.envConfig.Type != domain.EnvCloud {
		return ""
	}
	if s.sb == nil {
		if s.tried {
			return ""
		}
		s.tried = true
		sb, err := s.exec.provisionSandbox(ctx, s.sid, s.sess)
		if err != nil {
			// Logged rather than returned: the call has an answer, and losing
			// the whole pass over the file it could have spilled into would cost
			// more than the truncation does.
			slog.WarnContext(ctx, "mcp answer not spilled: no sandbox",
				"session_id", s.sid.String(), "error", err)
			return ""
		}
		s.sb = sb
	}
	return toolset.Spill(ctx, s.sb, id, full)
}

// mcpAnswerText is the text of one answer, whole and uncapped — what the spill
// file holds.
//
// Text only, and that is the promise the notice makes: a model reading the file
// back gets what it would have read inline. An image or a blob is bytes no
// `read` returns anything useful for, and it is bounded by the transport rather
// than truncated, so those blocks keep the whole-or-nothing budget rule they
// already had. A text resource's body counts, since it is text the model would
// otherwise have read in the answer.
//
// Blocks are joined in the order the server sent them, so the file reads as the
// answer did.
func mcpAnswerText(content []mcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		switch c.Type {
		case "text", "resource":
			if c.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(c.Text)
		}
	}
	return toolset.SanitizeText(b.String())
}
