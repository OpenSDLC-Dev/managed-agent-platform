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
// /v1/sessions answers the reference's recorded HTTP sentences; a deployment's
// run settles the reference's recorded run sentences where it was recorded
// (2026-09-02 batch2 `deployment.run.after-env-archived`, 2026-09-03 batch1
// `deployment.run.store-deleted` and `deployment.run.store-archived`) and
// keeps ours where it was not; a dream's start, never recorded, keeps ours
// throughout. Every arm keeps its run-error type.
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

	type want struct{ typ, httpMsg, runMsg string }
	check := func(t *testing.T, name string, err error, w want) {
		t.Helper()
		var re *runError
		if w.typ == "" {
			if errors.As(err, &re) {
				t.Errorf("%s: classified %q, want unclassified", name, re.typ)
			}
		} else if !errors.As(err, &re) || re.typ != w.typ {
			t.Errorf("%s: %v, want run-error type %q", name, err, w.typ)
			return
		}
		if err == nil || err.Error() != w.httpMsg {
			t.Errorf("%s: HTTP message %v, want %q", name, err, w.httpMsg)
		}
		if re != nil && re.runMessage() != w.runMsg {
			t.Errorf("%s: run message %q, want %q", name, re.runMessage(), w.runMsg)
		}
	}
	snapshot := func(id string, c sessionCaller) error {
		_, err := snapshotMemoryStore(ctx, pool, resourceInput{kind: resourceKindMemory, memoryStoreID: id, access: "read_write"}, c)
		return err
	}
	agent := func(c sessionCaller) error {
		srv := &server{pool: pool}
		_, err := srv.resolveAgent(ctx, pool, json.RawMessage(`"`+archivedAgent+`"`), false, c)
		return err
	}

	ours := map[string]string{
		"store gone":     "memory store " + ghostStore + " not found",
		"store archived": "memory store " + archivedStore + " is archived",
		"file gone":      "file " + ghostFile + " not found",
		"agent archived": "agent " + archivedAgent + " is archived",
		"env gone":       "environment " + ghostEnv + " not found",
	}
	for _, tc := range []struct {
		caller sessionCaller
		name   string
		cases  map[string]want
	}{
		{callerRequest, "request", map[string]want{
			"store gone": {"session_resource_not_found_error", "Memory store `" + ghostStore + "` not found.",
				"Memory store `" + ghostStore + "` not found."},
			"store archived": {"memory_store_archived_error", "Memory store " + archivedStore + " is archived.",
				"Memory store " + archivedStore + " is archived."},
			"file gone": {"file_not_found_error",
				"One or more files not found. Check that each `file_id` exists and is accessible: " + ghostFile,
				"One or more files not found. Check that each `file_id` exists and is accessible: " + ghostFile},
			"agent archived": {"agent_archived_error", "agent " + archivedAgent + " is archived and cannot be used to create a session",
				"agent " + archivedAgent + " is archived and cannot be used to create a session"},
			"env gone": {"", "Environment " + ghostEnv + " not found.", ""},
		}},
		{callerDeployment, "deployment", map[string]want{
			"store gone": {"session_resource_not_found_error", ours["store gone"],
				"session creation rejected: a referenced resource was not found; check deployment configuration"},
			"store archived": {"memory_store_archived_error", ours["store archived"],
				"session creation rejected: a referenced memory store is archived; check deployment resources"},
			"file gone":      {"file_not_found_error", ours["file gone"], ours["file gone"]},
			"agent archived": {"agent_archived_error", ours["agent archived"], ours["agent archived"]},
			"env gone": {"environment_not_found_error", ours["env gone"],
				"session creation rejected: environment `" + ghostEnv + "` not found"},
		}},
		{callerDream, "dream", map[string]want{
			"store gone":     {"session_resource_not_found_error", ours["store gone"], ours["store gone"]},
			"store archived": {"memory_store_archived_error", ours["store archived"], ours["store archived"]},
			"file gone":      {"file_not_found_error", ours["file gone"], ours["file gone"]},
			"agent archived": {"agent_archived_error", ours["agent archived"], ours["agent archived"]},
			"env gone":       {"", ours["env gone"], ""},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check(t, "store gone", snapshot(ghostStore, tc.caller), tc.cases["store gone"])
			check(t, "store archived", snapshot(archivedStore, tc.caller), tc.cases["store archived"])
			check(t, "file gone", fileMustExist(ctx, pool, ghostFile, tc.caller == callerRequest), tc.cases["file gone"])
			check(t, "agent archived", agent(tc.caller), tc.cases["agent archived"])
			check(t, "env gone", errSessionEnvironmentNotFound(tc.caller, ghostEnv), tc.cases["env gone"])
		})
	}
}
