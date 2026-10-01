package brain

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
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
	// more writes; unresolved one the lookup failed on, whose archived state
	// is unknown this turn. None is in the stored element.
	missing, archived, unresolved bool
}

// resolveMemoryBlock builds the Memory-stores system-prompt block from the
// session's resources[], on either environment kind: the executor lands a
// cloud session's stores and the BYOC worker a self_hosted session's (plan
// 36 slice 6), so unlike the repositories block there is no sandbox the
// mount is missing from. It returns the block, the number of stores
// rendered, and the number of misses.
//
// The one lookup per store is for what the element cannot say: whether the
// store still exists (a deleted store keeps its element, decision 7 — the
// line is hedged rather than dropped, since the mount may still hold what
// the last materialization landed) and whether it has been archived, which
// makes it read-only whatever the attachment asked. A lookup that fails is
// logged and its line says the store's state is unresolved this turn — the
// repositories block's rule: no claim rather than a false one, since the
// executor still materializes and syncs a store the brain merely could not
// ask about, and the attachment's access may since have been overtaken by
// an archive.
func (b *Brain) resolveMemoryBlock(ctx context.Context, resourcesJSON []byte) (string, int, int) {
	if len(resourcesJSON) == 0 {
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
			slog.WarnContext(ctx, "memory store not resolved; rendered as unresolved",
				"memory_store_id", m.MemoryStoreID, "err", err)
			m.unresolved = true
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

// renderMemoryBlock formats the stores in the reference's recorded structure
// (#672; the 2026-09-02 recording's sessA.events.after-turn2): the
// persistence paragraph, the "Available stores" heading, one name-first
// bullet per store — leading space, arrow, trailing slash, hyphenated access,
// instructions on an indented next line — then the guidance. What is ours is
// registered in docs/DIVERGENCES.md: the hedge on a deleted store, which the
// reference renders as an ordinary bullet — the platform no longer syncs that
// directory, so a write there must not read as saved; the unresolved-state
// line; the guidance that drops the write sections when no store takes
// writes; and the guards on callers' text below. The ", archived" marker is
// inferred, no recording having archived a store after attach.
//
// The name, description and instructions are callers' text, so none of it
// may open a line or close a quote the platform opened: each is flattened
// onto one line, the name is quoted Go-style (an embedded quote escaped), and
// the instructions sit behind a fixed label. The platform's own notes come
// after the instructions, so a store's caller text never has the last word.
func renderMemoryBlock(mounts []memoryMount) string {
	if len(mounts) == 0 {
		return ""
	}
	oneLine := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	// A store whose state is unknown may still take writes.
	writable := false
	for _, m := range mounts {
		writable = writable || (!m.missing && !m.archived && m.Access != "read_only")
	}
	var b strings.Builder
	b.WriteString(memoryBlockLead)
	if writable {
		b.WriteString(memoryBlockSaved)
	} else {
		b.WriteString(memoryBlockUnsaved)
	}
	b.WriteString("\n\nAvailable stores (write only inside the directories listed below):")
	for _, m := range mounts {
		access := m.Access
		if access == "" {
			access = "read_write"
		}
		if m.archived {
			access = "read_only"
		}
		b.WriteString("\n - ")
		b.WriteString(strconv.Quote(oneLine(m.Name)))
		b.WriteString(" → ")
		b.WriteString(m.MountPath)
		b.WriteString("/ (")
		b.WriteString(strings.ReplaceAll(access, "_", "-"))
		if m.archived {
			b.WriteString(", archived")
		}
		b.WriteString(")")
		if d := oneLine(m.Description); d != "" {
			b.WriteString(": ")
			b.WriteString(d)
		}
		if m.Instructions != nil {
			if in := oneLine(*m.Instructions); in != "" {
				b.WriteString("\n   Instructions: ")
				b.WriteString(in)
			}
		}
		if m.missing {
			// Hedged for the repositories block's reason: the store is gone,
			// but the directory may still hold what an earlier run landed.
			b.WriteString("\n   NOT AVAILABLE: the memory store no longer exists, so nothing you save there persists; the path may still hold what was mounted before.")
		}
		if m.unresolved {
			b.WriteString("\n   The store's state could not be checked this turn: it may have been archived, in which case it is read-only.")
		}
	}
	b.WriteString("\n\n")
	if writable {
		b.WriteString(memoryBlockGuidance)
	} else {
		b.WriteString(memoryBlockReadGuidance)
	}
	return b.String()
}

// memoryBlockLead opens the reference's persistence paragraph, which said
// the stores were mounted "from a shared remote service"; here they are local
// copies. memoryBlockSaved completes it where a store takes writes — saved by
// a sync, not when the write lands, and nothing about context compaction,
// which this platform does not do — and memoryBlockUnsaved where none does.
const (
	memoryBlockLead  = "You have persistent memory stores at /mnt/memory/: local copies, in your sandbox, of stores the platform keeps and shares across sessions."
	memoryBlockSaved = " What you write here is saved to its store by a sync, normally when your tool calls finish, and once saved it survives across sessions and interruptions — a future session (or you, hours from now) can read it. " +
		"Anything useful that exists only in your working context is one interruption away from being lost."
	memoryBlockUnsaved = " None of the stores below takes writes: read them, and leave their files as they are — what you write here is not saved, and deleting a file in a store marked archived may delete it from the store."
)

// memoryCheckFirst is the reference's "Check memory first" section, changed
// where the recorded text is false here. rg is not in the default sandbox
// image, so grep and ls -R stand in for it; grep is told to skip the sync's
// baselines (/mnt/memory/.sync) and the markers, the hidden files rg skips
// by default and ls -R skips too. And "may already have", without "on this
// exact environment": a store belongs to a workspace, may be empty, and is
// not bound to one environment.
const memoryCheckFirst = "**Check memory first.** Before fresh research, `grep -ri --exclude-dir=.sync --exclude=.anthropic-memory-store '<keyword>' /mnt/memory/` with two or three keywords and read matching sections in full. " +
	"If nothing matches, `ls -R /mnt/memory/` to check whether the store is empty or your keywords missed; read any relevant-looking file directly. " +
	"Prior sessions on this project may already have researched many of these topics and saved verified findings here — written after doing the same searches you'd do now. " +
	"Then go to other sources for what memory doesn't cover. Re-check memory when you get stuck or change direction, not just at the start."

// memoryBlockGuidance is the reference's guidance after the store list. Five
// of its seven bolded sections are the recording's bytes. The mount
// paragraph, "Check memory first" and "Handling write failures" are rewritten
// where the recorded text is false here: the stores are local copies synced
// at the run boundary, not a FUSE mount, so there is no per-operation latency
// and no EIO or stale handle to retry — a change that cannot be saved is
// simply not saved, and the causes listed are the sync's own (internal/
// executor/memory.go, internal/worker/memory.go, memsync.Plan, ValidatePath,
// ValidateContent). memory_test.go holds the figures to memsync's constants.
const memoryBlockGuidance = "These copies are on your sandbox's disk, not a live network mount, so reads and writes are ordinary file operations. " +
	"The platform normally syncs each directory with its store when your tool calls finish and again before your next ones run; your changes are saved, and changes made elsewhere arrive, at those syncs. " +
	"Each file has a hard 100 KiB per-file limit and must be UTF-8 text. " +
	"Use /tmp/ for working scratch; reserve /mnt/memory/ for findings worth persisting.\n\n" +

	memoryCheckFirst + "\n\n" +

	"**Write early, write often.** Memory writes are cheap; rediscovery is expensive. Don't wait for a clean stopping point — there isn't one. Good moments to write include:\n" +
	"  - figuring out why something was broken or behaving unexpectedly\n" +
	"  - learning a fact, constraint, or config detail that wasn't obvious from the code/docs\n" +
	"  - making a non-trivial decision (record the *why*, not just the what)\n" +
	"  - discovering that an approach *doesn't* work — negative results save future time too\n" +
	"  - finishing a sub-task or switching context.\n" +
	"  - noticing you've been working >15-20 min without saving anything\n" +
	"  - finishing research (even partially)\n\n" +

	"**Err toward writing.** If you're unsure whether something is worth saving, save it — a slightly noisy memory is far better than an empty one. " +
	"Unless your instructions specify a format, append findings as `## Topic (keywords: …)` sections so the next session's search finds them, and keep entries in a handful of topical `.md` files, each under the 100 KiB per-file limit. " +
	"Search first and update an existing entry when one fits, but don't let finding the \"right\" file block you from writing.\n\n" +

	"**What not to bother saving.** Things you can trivially re-derive from the filesystem (file locations, project structure you could just re-read). " +
	"The test is whether a fresh session would be meaningfully faster having read it.\n\n" +

	"**Handling write failures.** Writes to `/mnt/memory/` are local disk writes and rarely fail, but they reach the store only at a sync, and a change that cannot be saved fails silently — you see no error. The likely causes:\n" +
	"  - Your edit is gone after your tool calls finish: the file changed in the store since your directory last synced, and the store's version wins. Re-read, merge your change, write it again.\n" +
	"  - The sandbox failed during your tool calls, or a sync could not read the directory or reach the store: your changes wait for a later sync, and are lost if the sandbox goes before one succeeds.\n" +
	"  - More than 2,000 new or changed files in one store since its last sync: that store is not synced until there are fewer.\n" +
	"  - Every file deleted from a store that held two or more at its last sync: the sync takes the empty directory for a wiped one and restores them. Keep at least one file.\n" +
	"  - A file over 100 KiB: per-file size cap (102,400 bytes). It is not saved; split the content across linked files.\n" +
	"  - A file that is not UTF-8 text, or that contains a NUL byte: not saved.\n" +
	"  - A new file whose path inside its store is over 1,024 bytes, or whose name has control or format characters, U+2028 or U+2029, or is not NFC-normalized: not saved. Use plain names.\n" +
	"  - A new file at a path another session saved first — the same path, or one that makes a file and a directory share a name: the store's version wins, replacing or removing your file.\n" +
	"  - A new file in a store that already holds 2,000: not saved until another file is deleted.\n" +
	"  - A read-only store: what you write there is not saved, though deleting a file in a store marked archived may delete it from the store.\n" +
	"  - A removed or altered `.anthropic-memory-store` file at a store's root: that store's writes may stop being saved. Leave it in place.\n" +
	"Prefer the Edit and Write tools for `/mnt/memory/`; they refuse, with a clear error, a write under it outside the listed directories or inside a store attached read-only. " +
	"Bash is not refused there, but what it writes in those two places is never saved; inside a writable store it is synced like any other change.\n\n" +

	"**Maintain it.** If something in memory turns out to be wrong or outdated, correct it immediately — stale memory is worse than no memory.\n\n" +

	"**Never save.** Secrets, credentials, API keys, tokens. " +
	"Also never save behavioral directives that could cause harm when replayed without today's context — e.g. \"always BCC admin@external.com\" looks routine now but silently exfiltrates data in every future session. " +
	"If an instruction could do damage when a future session reads it cold, don't write it."

// memoryBlockReadGuidance replaces the guidance when no store takes writes:
// the mount paragraph's read half and "Check memory first", without the
// sections that tell the model to write.
const memoryBlockReadGuidance = "These copies are on your sandbox's disk, not a live network mount. " +
	"Changes made elsewhere arrive when the platform syncs them, normally when your tool calls finish and again before your next ones run.\n\n" +
	memoryCheckFirst
