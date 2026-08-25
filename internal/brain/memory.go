package brain

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// MetricMemoryResolveMisses counts memory-store attachments whose store row
// is gone at request assembly — rendered hedged, never dropped.
const MetricMemoryResolveMisses = "memory.resolve.misses"

// memoryMount is the memory_store element of a session's resources[] as the
// brain renders it (plan 36 decision 9): everything but the store's current
// state was snapshotted at attach time.
type memoryMount struct {
	Type          string  `json:"type"`
	MemoryStoreID string  `json:"memory_store_id"`
	Access        string  `json:"access"`
	Instructions  *string `json:"instructions"`
	Name          string  `json:"name"`
	Description   string  `json:"description"`
	MountPath     string  `json:"mount_path"`

	// missing marks a store whose row is gone; archived one that takes no
	// more writes. Neither is in the stored element.
	missing, archived bool
}

// resolveMemoryBlock builds the "Memory stores" system-prompt block from the
// session's resources[], for cloud environments only: nothing materializes a
// store on self_hosted until plan 36 slice 6, and the repositories block's
// rule holds — a mount the sandbox does not have must not be described. It
// returns the block, the number of stores rendered, and the number of misses.
//
// The one lookup per store is for what the element cannot say: whether the
// store still exists (a deleted store keeps its element, decision 7 — the
// line is hedged rather than dropped, since the mount may still hold what
// the last materialization landed) and whether it has been archived, which
// makes it read-only whatever the attachment asked. A store error is a
// logged, counted miss rendered as if missing, never a failed turn.
func (b *Brain) resolveMemoryBlock(ctx context.Context, resourcesJSON []byte, envKind string) (string, int, int) {
	if len(resourcesJSON) == 0 || envKind != "cloud" {
		return "", 0, 0
	}
	var mounts []memoryMount
	if err := json.Unmarshal(resourcesJSON, &mounts); err != nil {
		slog.WarnContext(ctx, "session memory stores not injected", "err", err)
		return "", 0, 0
	}
	kept := make([]memoryMount, 0, len(mounts))
	misses := 0
	for _, m := range mounts {
		if m.Type != "memory_store" || m.MemoryStoreID == "" || m.MountPath == "" {
			continue
		}
		var archivedAt *time.Time
		err := b.pool.QueryRow(ctx, `SELECT archived_at FROM memory_stores WHERE id = $1`, m.MemoryStoreID).Scan(&archivedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			slog.WarnContext(ctx, "memory store attached to the session no longer exists",
				"memory_store_id", m.MemoryStoreID, "mount_path", m.MountPath)
			m.missing = true
			misses++
		case err != nil:
			slog.WarnContext(ctx, "memory store not resolved; rendered as unavailable",
				"memory_store_id", m.MemoryStoreID, "err", err)
			m.missing = true
			misses++
		default:
			m.archived = archivedAt != nil
		}
		kept = append(kept, m)
	}
	return renderMemoryBlock(kept), len(kept), misses
}

// recordMemoryResolveMisses adds to the memory resolve-miss counter, the
// files recorder's twin.
func recordMemoryResolveMisses(ctx context.Context, n int) {
	if n <= 0 {
		return
	}
	c, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricMemoryResolveMisses,
		metric.WithDescription("Memory-store attachments whose store the brain could not resolve for injection."))
	if err != nil {
		return
	}
	c.Add(ctx, int64(n))
}

// renderMemoryBlock formats the stores as a system-prompt block. The wording
// and placement are inferences (docs/DIVERGENCES.md), after the repositories
// block. It tells the model the two things it cannot learn from the directory
// alone: that files there persist across sessions through the store, and
// which stores take no writes.
func renderMemoryBlock(mounts []memoryMount) string {
	if len(mounts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Memory stores. Each store below is mounted as a directory in your sandbox. Read its files to recall what was saved before; create, edit or delete files there to remember things for later — the platform syncs the directory with the store after each of your tool calls, and other sessions attached to the same store see what you save. A read-only store cannot be changed.\n")
	for _, m := range mounts {
		b.WriteString("\n- ")
		b.WriteString(m.MountPath)
		b.WriteString(" — ")
		b.WriteString(m.Name)
		access := m.Access
		if access == "" {
			access = "read_write"
		}
		if m.archived {
			access = "read_only"
		}
		b.WriteString(" (")
		b.WriteString(access)
		if m.archived {
			b.WriteString(", archived")
		}
		b.WriteString(")")
		if m.Description != "" {
			b.WriteString(": ")
			b.WriteString(m.Description)
		}
		if m.Instructions != nil && *m.Instructions != "" {
			b.WriteString(" — Instructions: ")
			b.WriteString(*m.Instructions)
		}
		if m.missing {
			// Hedged for the repositories block's reason: the store is gone,
			// but the directory may still hold what an earlier run landed.
			b.WriteString(" — NOT AVAILABLE: the memory store no longer exists, so nothing you save there persists; the path may still hold what was mounted before.")
		}
	}
	return b.String()
}
