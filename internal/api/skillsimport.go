package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/skills"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ImportSummary reports one operator-import run.
type ImportSummary struct {
	Imported []string         // "name version" pairs landed this run
	Skipped  []string         // already present at this version
	Failed   map[string]error // directory → why it did not import
}

// ImportAnthropicSkills is the controlplane's run-once operator import
// (docs/plan/06_skills.md slice 3): each dir is a skill directory from a
// local checkout of github.com/anthropics/skills, validated exactly like an
// upload and landed as a source='anthropic' skill whose id is the SKILL.md
// name (the reference catalog's short-name ids) at the given date-based
// version. Idempotent per (skill, version): an existing version is skipped
// without touching storage. A directory that fails to validate is logged and
// skipped; the returned error reports that some directories failed.
//
// The checkout's content is read at the operator's machine and never enters
// this repository — the reference document skills are source-available, not
// open source (the plan's license red lines).
func ImportAnthropicSkills(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store, dirs []string, version string) (*ImportSummary, error) {
	sum := &ImportSummary{Failed: map[string]error{}}
	if blobs == nil {
		return sum, errors.New("object storage is not configured; the import needs somewhere to put archives")
	}
	if !skillVersionRe.MatchString(version) {
		return sum, fmt.Errorf("version %q must be a digit string (the checkout's commit date, YYYYMMDD)", version)
	}
	for _, dir := range dirs {
		name, err := importSkillDir(ctx, pool, blobs, dir, version)
		switch {
		case err == nil:
			sum.Imported = append(sum.Imported, name+" "+version)
			slog.InfoContext(ctx, "anthropic skill imported", "skill_id", name, "version", version, "dir", dir)
		case errors.Is(err, errVersionExists):
			sum.Skipped = append(sum.Skipped, name+" "+version)
			slog.InfoContext(ctx, "anthropic skill already imported", "skill_id", name, "version", version)
		default:
			sum.Failed[dir] = err
			slog.WarnContext(ctx, "anthropic skill import failed", "dir", dir, "err", err)
		}
	}
	slog.InfoContext(ctx, "anthropic skills import complete",
		"imported", len(sum.Imported), "skipped", len(sum.Skipped), "failed", len(sum.Failed))
	if len(sum.Failed) > 0 {
		return sum, fmt.Errorf("%d of %d skill directories failed to import", len(sum.Failed), len(dirs))
	}
	return sum, nil
}

// errVersionExists marks the idempotent skip: this skill already carries this
// version.
var errVersionExists = errors.New("version already imported")

// importSkillDir validates one on-disk skill directory and lands it with the
// registry's transaction ordering: rows claimed, archive put, commit last.
func importSkillDir(ctx context.Context, pool *pgxpool.Pool, blobs blob.Store, dir, version string) (string, error) {
	files, err := readSkillDir(dir)
	if err != nil {
		return "", err
	}
	bundle, err := skills.FromFiles(files)
	if err != nil {
		return "", err
	}
	name := bundle.Name

	tx, err := pool.Begin(ctx)
	if err != nil {
		return name, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Ensure-then-lock the skill row. The reference catalog's ids are short
	// names, so an id collision with a custom (skill_-prefixed) skill is
	// impossible by shape; the source check guards a re-run against a row
	// something else created.
	if _, err := tx.Exec(ctx,
		`INSERT INTO skills (id, source, display_title) VALUES ($1, 'anthropic', $2)
		 ON CONFLICT (id) DO NOTHING`, name, name); err != nil {
		return name, err
	}
	var source string
	if err := tx.QueryRow(ctx, `SELECT source FROM skills WHERE id = $1 FOR UPDATE`, name).Scan(&source); err != nil {
		return name, err
	}
	if source != "anthropic" {
		return name, fmt.Errorf("skill id %q already exists with source %q", name, source)
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM skill_versions WHERE skill_id = $1 AND version = $2)`,
		name, version).Scan(&exists); err != nil {
		return name, err
	}
	if exists {
		return name, errVersionExists
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory, sha256)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		domain.NewID(domain.PrefixSkillVersion).String(), name, version,
		bundle.Name, bundle.Description, bundle.Directory, bundle.SHA256); err != nil {
		return name, err
	}
	// latest_version follows the numerically newest version (length-then-
	// lexical over digit strings — the reference worker's own "latest" rule),
	// so backfilling an older date never regresses it and both execution
	// halves resolve the alias identically.
	if _, err := tx.Exec(ctx,
		`UPDATE skills SET latest_version = $2, updated_at = now()
		 WHERE id = $1 AND (latest_version IS NULL
		   OR length($2::text) > length(latest_version)
		   OR (length($2::text) = length(latest_version) AND $2 > latest_version))`,
		name, version); err != nil {
		return name, err
	}
	key := skillBlobKey(name, version)
	if err := blobs.Put(ctx, key, bytes.NewReader(bundle.Zip), int64(len(bundle.Zip)), "application/zip"); err != nil {
		return name, fmt.Errorf("store skill archive: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		if derr := blobs.Delete(ctx, key); derr != nil {
			slog.WarnContext(ctx, "skill archive orphaned in object storage", "key", key, "err", derr)
		}
		return name, err
	}
	return name, nil
}

// readSkillDir reads a skill directory into the upload shape: regular files
// only, paths slash-separated under the directory's base name, with the
// upload caps enforced during the walk so a runaway directory cannot be read
// wholesale into memory first.
func readSkillDir(dir string) ([]skills.File, error) {
	base := filepath.Base(filepath.Clean(dir))
	var files []skills.File
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and specials never enter an archive
		}
		// Size-check from metadata before reading, so one runaway file is
		// rejected without first being pulled into memory.
		info, err := d.Info()
		if err != nil {
			return err
		}
		if total += info.Size(); total > skills.MaxTotalBytes {
			return fmt.Errorf("directory exceeds the %d-byte skill cap", skills.MaxTotalBytes)
		}
		if len(files) >= skills.MaxMembers {
			return fmt.Errorf("directory exceeds the %d-file skill cap", skills.MaxMembers)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, skills.File{Path: base + "/" + filepath.ToSlash(rel), Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
