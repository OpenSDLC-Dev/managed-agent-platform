package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// ThinkingBlock is one model thinking block kept for replay (#67,
// docs/plan/60_thinking-replay.md): the content block a later request sends
// back — a thinking block with its signature, or a redacted_thinking block —
// stored beside the agent.thinking event it belongs to, which stays
// content-free on the wire. Model and PrefixDigest are the guard it is
// replayed under; the brain computes and checks both.
type ThinkingBlock struct {
	EventID      domain.ID
	Model        string
	PrefixDigest string
	// Block is the content block's JSON, kept and returned byte for byte.
	Block json.RawMessage
}

// insertThinking writes a settlement's thinking blocks in its transaction, so
// a turn that does not commit keeps none (AppendOptions.Thinking).
func insertThinking(ctx context.Context, tx pgx.Tx, sessionID domain.ID, blocks []ThinkingBlock) error {
	if len(blocks) == 0 {
		return nil
	}
	var sb strings.Builder
	args := make([]any, 0, len(blocks)*5)
	sb.WriteString(`INSERT INTO thinking_blocks (event_id, session_id, model, prefix_digest, block) VALUES `)
	for i, b := range blocks {
		if i > 0 {
			sb.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d, $%d::json)", n+1, n+2, n+3, n+4, n+5)
		args = append(args, b.EventID.String(), sessionID.String(), b.Model, b.PrefixDigest, string(b.Block))
	}
	_, err := tx.Exec(ctx, sb.String(), args...)
	return err
}

// ThinkingBlocks returns a session's kept thinking blocks by event id, for
// replay. Block is read as text: the json column keeps the bytes it was given
// and text returns them unchanged.
func (l *Log) ThinkingBlocks(ctx context.Context, sessionID domain.ID) (map[domain.ID]ThinkingBlock, error) {
	rows, err := l.pool.Query(ctx,
		`SELECT event_id, model, prefix_digest, block::text FROM thinking_blocks WHERE session_id = $1`,
		sessionID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.ID]ThinkingBlock{}
	for rows.Next() {
		var (
			b     ThinkingBlock
			id    string
			block string
		)
		if err := rows.Scan(&id, &b.Model, &b.PrefixDigest, &block); err != nil {
			return nil, err
		}
		b.EventID, b.Block = domain.ID(id), json.RawMessage(block)
		out[b.EventID] = b
	}
	return out, rows.Err()
}
