package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// TestSessionCreateRefusalsAreWordedPerCaller pins the refusals the three
// callers of createSessionInTx share, worded per caller (#540): a POST
// /v1/sessions answers the reference's recorded HTTP sentences
// (requestWording); a deployment's run settles the reference's recorded run
// sentences where it was recorded (2026-09-03 batch1
// `deployment.run.store-deleted` and `deployment.run.store-archived`;
// runWording) and keeps ours where it was not; a dream's start, never
// recorded, keeps ours throughout. Every refusal but the environment's keeps
// its run-error type; that one is unclassified, so a deployment's fire rolls
// back instead of settling a run.
func TestSessionCreateRefusalsAreWordedPerCaller(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	ghostStore := domain.NewID(domain.PrefixMemoryStore).String()
	archivedStore := domain.NewID(domain.PrefixMemoryStore).String()
	ghostFile := domain.NewID(domain.PrefixFile).String()
	ghostEnv := domain.NewID(domain.PrefixEnvironment).String()
	archivedAgent := domain.NewID(domain.PrefixAgent).String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO memory_stores (id, name, archived_at) VALUES ($1, 'archived', now())`, archivedStore); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (id, name, version, spec, archived_at) VALUES ($1, 'a', 1, '{"model":{"id":"m"}}', now())`,
		archivedAgent); err != nil {
		t.Fatal(err)
	}

	snapshot := func(id string) error {
		_, err := snapshotMemoryStore(ctx, pool, resourceInput{kind: resourceKindMemory, memoryStoreID: id, access: "read_write"})
		return err
	}
	srv := &server{pool: pool}
	_, agentErr := srv.resolveAgent(ctx, pool, json.RawMessage(`"`+archivedAgent+`"`), false)
	refusals := map[string]error{
		"store gone":     snapshot(ghostStore),
		"store archived": snapshot(archivedStore),
		"file gone":      fileMustExist(ctx, pool, ghostFile),
		"agent archived": agentErr,
		"env gone":       errSessionEnvironmentNotFound(ghostEnv),
	}

	// What each caller answers with a shared refusal: the request its HTTP
	// error, a deployment the message a classified refusal settles its run
	// with (an unclassified one is its HTTP error), a dream the error as
	// raised.
	words := map[string]func(error) string{
		"request": func(err error) string { return requestWording(err).Error() },
		"deployment": func(err error) string {
			var re *runError
			if errors.As(err, &re) {
				return runWording(re)
			}
			return err.Error()
		},
		"dream": func(err error) string { return err.Error() },
	}
	ours := map[string]string{
		"store gone":     "memory store " + ghostStore + " not found",
		"store archived": "memory store " + archivedStore + " is archived",
		"file gone":      "file " + ghostFile + " not found",
		"agent archived": "agent " + archivedAgent + " is archived",
		"env gone":       "environment " + ghostEnv + " not found",
	}
	types := map[string]string{
		"store gone":     "session_resource_not_found_error",
		"store archived": "memory_store_archived_error",
		"file gone":      "file_not_found_error",
		"agent archived": "agent_archived_error",
		"env gone":       "",
	}
	for caller, cases := range map[string]map[string]string{
		"request": {
			"store gone":     "Memory store `" + ghostStore + "` not found.",
			"store archived": "Memory store " + archivedStore + " is archived.",
			"file gone":      "One or more files not found. Check that each `file_id` exists and is accessible: " + ghostFile,
			"agent archived": "agent " + archivedAgent + " is archived and cannot be used to create a session",
			"env gone":       "Environment " + ghostEnv + " not found.",
		},
		"deployment": {
			"store gone":     "session creation rejected: a referenced resource was not found; check deployment configuration",
			"store archived": "session creation rejected: a referenced memory store is archived; check deployment resources",
			"file gone":      ours["file gone"],
			"agent archived": ours["agent archived"],
			"env gone":       ours["env gone"],
		},
		"dream": ours,
	} {
		t.Run(caller, func(t *testing.T) {
			for name, err := range refusals {
				var re *runError
				switch typ := types[name]; {
				case typ == "" && errors.As(err, &re):
					t.Errorf("%s: classified %q, want unclassified", name, re.typ)
				case typ != "" && (!errors.As(err, &re) || re.typ != typ):
					t.Errorf("%s: %v, want run-error type %q", name, err, typ)
				}
				if err == nil {
					t.Errorf("%s: no refusal", name)
					continue
				}
				if got := words[caller](err); got != cases[name] {
					t.Errorf("%s: %q, want %q", name, got, cases[name])
				}
			}
		})
	}
}
