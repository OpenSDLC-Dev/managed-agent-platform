// The workspace header rule and the tenancy response stamp, written once
// because all five credential resolvers apply them (plan 42 §6.2). One rule
// beats five: a header that narrows on four lanes and is silently ignored on
// the fifth is the shape a reader gets wrong, and getting it wrong on the work
// lane means a BYOC worker quietly polling the wrong tenant's queue.

package api

import (
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// workspaceHeader is the reference's own selector, request and response both.
const workspaceHeader = "anthropic-workspace-id"

// orgHeader is stamped beside it. No absence rule for it is documented
// anywhere, so we run it on the workspace header's schedule.
const orgHeader = "anthropic-organization-id"

// selectWorkspace applies the one header rule: anthropic-workspace-id may only
// NARROW to a workspace the credential already covers.
//
// covered is the set of LIVE workspaces the credential resolves to — callers
// pass live rows only, so an archived workspace is never a member and can never
// be selected. Absent header with a single covered workspace is the ordinary
// case and takes it; with more than one there is nothing to guess at, and the
// caller must choose. A well-formed id outside the set answers 404 whether or
// not that workspace exists, from this one code path, so existence never leaks.
func selectWorkspace(r *http.Request, covered []domain.Scope) (domain.Scope, error) {
	v := r.Header.Get(workspaceHeader)
	if v == "" {
		if len(covered) == 1 {
			return covered[0], nil
		}
		// len(covered) == 0 lands here too. A credential covering nothing is
		// normally refused before it reaches this function; answering the same
		// 400 rather than a special case keeps that from becoming a third arm
		// nobody exercises.
		return domain.Scope{}, errInvalid("anthropic-workspace-id header is required: this identity spans multiple workspaces and none was selected.")
	}
	// "default" is this deployment's own frozen workspace id, which the
	// reference has no equivalent of — accepting it is a lenient parse of ours,
	// registered as such.
	if !domain.ValidWithPrefix(v, domain.PrefixWorkspace) && v != "default" {
		return domain.Scope{}, errInvalid("anthropic-workspace-id header must be a valid workspace ID.")
	}
	for _, s := range covered {
		if s.WorkspaceID == v {
			return s, nil
		}
	}
	// The id is echoed AS GIVEN. The reference prints a decoded UUID here; we
	// owe no decoder and print back what we were sent.
	return domain.Scope{}, errNotFound("Workspace `%s` not found.", v)
}

// stampScope emits the two tenancy response headers. Each resolver calls it as
// soon as the scope resolves and before anything writes, so both headers are
// present on a 200 and on an authenticated 4xx, and absent on a 401 that failed
// before authentication.
func stampScope(w http.ResponseWriter, s domain.Scope) {
	w.Header().Set(orgHeader, s.OrgID)
	w.Header().Set(workspaceHeader, s.WorkspaceID)
}
