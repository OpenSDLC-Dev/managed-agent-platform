package brain

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
)

// repoMount is the minimal shape of one session resources[] github_repository
// entry the brain injects — the token-free wire object the API stores
// (sessionresources.go's repoResourceJSON). The brain never reads the sealed
// credential row, so the block cannot leak what it never sees.
type repoMount struct {
	MountPath string `json:"mount_path"`
	Type      string `json:"type"`
	URL       string `json:"url"`
	Checkout  *struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
		Sha  string `json:"sha,omitempty"`
	} `json:"checkout"`
}

// resolveReposBlock builds the "Mounted repositories" system-prompt block from
// the session's resources[], so the agent can find checkouts that live outside
// its workdir. It returns the block and the number of repositories injected.
//
// Unlike the files block this needs no database join — every rendered fact
// (path, url, checkout) already lives in the stored resource — so it has no
// miss counter of its own: an entry can only be dropped by being malformed,
// which is a decode failure for the whole array.
//
// The block is emitted only for cloud environments (plan 25 decision 8):
// nothing materializes repositories on self_hosted — neither this platform nor
// the reference — and telling the model a checkout is present when none is
// would be a false statement, not a harmless omission.
func (b *Brain) resolveReposBlock(ctx context.Context, resourcesJSON []byte, envKind string) (string, int) {
	if len(resourcesJSON) == 0 || envKind != "cloud" {
		return "", 0
	}
	var mounts []repoMount
	if err := json.Unmarshal(resourcesJSON, &mounts); err != nil {
		slog.WarnContext(ctx, "session repositories not injected", "err", err)
		return "", 0
	}
	kept := make([]repoMount, 0, len(mounts))
	for _, m := range mounts {
		if m.Type != "github_repository" || m.URL == "" || m.MountPath == "" {
			continue
		}
		kept = append(kept, m)
	}
	return renderReposBlock(kept), len(kept)
}

// renderReposBlock formats the repository mounts as a system-prompt block. The
// wording and placement are inferences (docs/DIVERGENCES.md), mirroring the
// files block; the checkout descriptor names the branch, the commit, or the
// repository's default branch, which is what the clone resolved.
func renderReposBlock(mounts []repoMount) string {
	if len(mounts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Mounted repositories. Each repository below is checked out at the given path in your sandbox; work in it with your file and bash tools.\n")
	for _, m := range mounts {
		b.WriteString("\n- ")
		b.WriteString(m.MountPath)
		b.WriteString(" (")
		b.WriteString(m.URL)
		b.WriteString(", ")
		switch {
		case m.Checkout == nil:
			b.WriteString("default branch")
		case m.Checkout.Type == "branch":
			b.WriteString("branch ")
			b.WriteString(m.Checkout.Name)
		case m.Checkout.Type == "commit":
			b.WriteString("commit ")
			b.WriteString(m.Checkout.Sha)
		default:
			b.WriteString("default branch")
		}
		b.WriteString(")")
	}
	return b.String()
}
