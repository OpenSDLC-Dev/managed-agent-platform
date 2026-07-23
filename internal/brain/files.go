package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
)

// fileMount is the minimal shape of one session resources[] file entry the
// brain injects — file_id + mount_path from the stored fileResourceJSON.
// Non-file types (none in v1) are skipped by Type.
type fileMount struct {
	FileID    string `json:"file_id"`
	MountPath string `json:"mount_path"`
	Type      string `json:"type"`
}

// fileMeta is one resolved mount's rendered facts.
type fileMeta struct {
	Path, Filename, MimeType string
	Size                     int64
}

// resolveFilesBlock builds the "Mounted files" system-prompt block from the
// session's resources[], joining each mount to its files-table row for the
// filename, MIME type, and size the agent needs to recognize the mount. It
// returns the block and the number of mounts injected. Best-effort, mirroring
// resolveSkillsBlock: a dangling mount (its file row gone — the delete raced the
// reference, plan decision 2) is a logged skip, never a failed turn. The block
// is metadata only; the executor is what actually writes the bytes into the
// sandbox.
func (b *Brain) resolveFilesBlock(ctx context.Context, resourcesJSON []byte) (string, int) {
	if len(resourcesJSON) == 0 {
		return "", 0
	}
	var mounts []fileMount
	if err := json.Unmarshal(resourcesJSON, &mounts); err != nil {
		slog.WarnContext(ctx, "session resources not injected", "err", err)
		return "", 0
	}
	var metas []fileMeta
	for _, m := range mounts {
		if m.Type != "file" || m.FileID == "" || m.MountPath == "" {
			continue
		}
		var filename, mimeType string
		var size int64
		err := b.pool.QueryRow(ctx,
			`SELECT filename, mime_type, size_bytes FROM files WHERE id = $1`, m.FileID).
			Scan(&filename, &mimeType, &size)
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "mounted file not injected (file gone)",
				"file_id", m.FileID, "mount_path", m.MountPath)
			continue
		}
		if err != nil {
			slog.WarnContext(ctx, "mounted file not injected", "file_id", m.FileID, "err", err)
			continue
		}
		metas = append(metas, fileMeta{Path: m.MountPath, Filename: filename, MimeType: mimeType, Size: size})
	}
	return renderFilesBlock(metas), len(metas)
}

// renderFilesBlock formats the mounts as a system-prompt block. The wording and
// placement are inferences (docs/DIVERGENCES.md), mirroring the skills block.
func renderFilesBlock(metas []fileMeta) string {
	if len(metas) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Mounted files. Each file below is available at the given path in your sandbox; read it with your file tools.\n")
	for _, m := range metas {
		b.WriteString("\n- ")
		b.WriteString(m.Path)
		b.WriteString(" (")
		b.WriteString(m.Filename)
		if m.MimeType != "" {
			b.WriteString(", ")
			b.WriteString(m.MimeType)
		}
		fmt.Fprintf(&b, ", %d bytes)", m.Size)
	}
	return b.String()
}
