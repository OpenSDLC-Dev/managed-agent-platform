package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun/uidguard"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

const (
	// gateHealthTimeout bounds the wait for a freshly-created gate to report
	// healthy. Its HEALTHCHECK turns healthy on the first successful proxy-port
	// probe (~seconds — the listener starts only after the firewall is applied
	// and verified) and unhealthy only after ~30s of failed probes. A gate that
	// never becomes healthy never verified its firewall; the pair fails closed.
	gateHealthTimeout = 30 * time.Second
	gateHealthPoll    = 250 * time.Millisecond

	// Docker's container health states (State.Health.Status).
	healthStatusHealthy   = "healthy"
	healthStatusUnhealthy = "unhealthy"
	guardSessionLabel     = "dev.opensdlc.managed-agent-platform.guard-session"
)

func gateName(sessionID domain.ID) string { return "map-gate-" + string(sessionID) }

// ensureGate returns the running, healthy owner of the sandbox's network
// namespace, and the minted token only when this call created the gate.
func (p *Provider) ensureGate(ctx context.Context, spec sandbox.Spec) (containerInfo, string, error) {
	name := gateName(spec.SessionID)
	info, ierr := p.api.inspectContainer(ctx, name)
	switch {
	case ierr == nil: // an existing gate (a re-lease, or a later tool call)
		if err := ours(info, spec.SessionID); err != nil {
			return containerInfo{}, "", err
		}
		if info.State.Running {
			// A running gate has already installed its firewall — one that could
			// not aborts with the process still root, so a running gate never
			// serves fail-open. Still require healthy, so a gate whose proxy is
			// not yet (or no longer) serving is not handed a sandbox; an
			// already-healthy gate returns on the first poll.
			if info.State.Health.Status == healthStatusHealthy {
				return info, "", nil
			}
			info, err := p.waitHealthy(ctx, info.ID, "gate")
			return info, "", err
		}
		// Its token may have been revoked or never persisted. Retire the old
		// gate before minting a replacement for the session.
		if err := p.retireStoppedGate(ctx, info, spec); err != nil {
			return containerInfo{}, "", err
		}
	case !statusIs(ierr, 404):
		return containerInfo{}, "", ierr
	}

	// Create. Mint an in-memory token, create the container with it, and persist
	// it only after winning the create — so a lost create race (another executor
	// created the gate first, adopted below) never revokes the winner's token.
	token := spec.Gate.TokenMinter.Generate()
	res := spec.Gate.Resources.Effective()
	cpuMillis := res.CPUMillis
	if cpuMillis > 1000 {
		cpuMillis = p.clampCPU(ctx, cpuMillis)
	}
	cfg := gateConfigWithCPU(spec, token, p.gateNetwork, cpuMillis)
	id, err := p.api.createContainer(ctx, name, cfg)
	if statusIs(err, 404) { // the gate image is not on this host yet
		if perr := p.api.pullImage(ctx, spec.Gate.Image); perr != nil {
			return containerInfo{}, "", perr
		}
		id, err = p.api.createContainer(ctx, name, cfg)
	}
	if statusIs(err, 409) { // another executor won the create; adopt its gate
		won, werr := p.api.inspectContainer(ctx, name)
		if werr != nil {
			return containerInfo{}, "", werr
		}
		if werr := ours(won, spec.SessionID); werr != nil {
			return containerInfo{}, "", werr
		}
		won, werr = p.waitHealthy(ctx, won.ID, "gate")
		return won, "", werr
	}
	if err != nil {
		return containerInfo{}, "", err
	}
	// Persist the token before the gate authenticates its first config fetch.
	if err := spec.Gate.TokenMinter.Persist(ctx, spec.SessionID, token); err != nil {
		err = errors.Join(err, p.releaseFailedPair(ctx, spec, id, token, ""))
		return containerInfo{}, "", err
	}
	if err := p.api.startContainer(ctx, id); err != nil {
		err = errors.Join(err, p.releaseFailedPair(ctx, spec, id, token, ""))
		return containerInfo{}, "", err
	}
	info, err = p.waitHealthy(ctx, id, "gate")
	if err != nil {
		if info.ID == id && !info.State.Running {
			err = errors.Join(err, p.releaseFailedPair(ctx, spec, id, token, ""))
		}
		return containerInfo{}, "", err
	}
	return info, token, nil
}

// An existing creator may still be starting this gate. Docker's unforced
// deletion must succeed before its token is revoked. Keep the labeled volume
// until revocation succeeds so the reaper can still find the session.
func (p *Provider) retireStoppedGate(ctx context.Context, gate containerInfo, spec sandbox.Spec) error {
	volume, err := guardVolume(gate)
	if err != nil {
		return err
	}
	owned, err := p.api.ownsGuard(ctx, spec.SessionID, volume)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("docker: guard volume of gate %s does not belong to its session: %w", gate.ID, sandbox.ErrSpecMismatch)
	}
	var token string
	for _, entry := range gate.Config.Env {
		if value, found := strings.CutPrefix(entry, "GATE_TOKEN="); found {
			token = value
			break
		}
	}
	if token == "" {
		return fmt.Errorf("docker: gate %s has no token to retire: %w", gate.ID, sandbox.ErrSpecMismatch)
	}
	if err := p.api.deleteContainer(ctx, gate.ID, nil); err != nil && !statusIs(err, http.StatusNotFound) {
		return err
	}
	if err := spec.Gate.TokenMinter.RevokeIfCurrent(ctx, spec.SessionID, token); err != nil {
		return fmt.Errorf("docker: revoke stopped gate token for session %s: %w", spec.SessionID, err)
	}
	return p.api.releaseGuard(ctx, spec.SessionID, volume)
}

// A published, running gate may already serve another provision. A stopped
// gate is never restarted by this provider. Revoke its exact token before
// deleting its last ownership label, so a failed revoke stays discoverable.
func (p *Provider) releaseFailedPair(ctx context.Context, spec sandbox.Spec, id, token, sandboxID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	gate, err := p.api.inspectContainer(ctx, id)
	if err != nil && !statusIs(err, http.StatusNotFound) {
		return err
	}
	if err == nil {
		if gate.ID != id || ours(gate, spec.SessionID) != nil {
			return fmt.Errorf("docker: cannot verify ownership of gate %s: %w", id, sandbox.ErrSpecMismatch)
		}
	}
	if !gate.State.Running {
		if rerr := spec.Gate.TokenMinter.RevokeIfCurrent(ctx, spec.SessionID, token); rerr != nil {
			return fmt.Errorf("docker: revoke stopped gate token for session %s: %w", spec.SessionID, rerr)
		}
	}
	// Release the borrower first so Docker can reclaim the gate's anonymous
	// volume. A failed unforced delete leaves a concurrent adopter alone.
	if sandboxID != "" {
		if rerr := p.api.removeStoppedContainer(ctx, sandboxID); rerr != nil && !statusIs(rerr, http.StatusNotFound) {
			return rerr
		}
	}
	if gate.State.Running || statusIs(err, http.StatusNotFound) {
		return nil
	}
	err = p.api.removeStoppedContainer(ctx, id)
	if statusIs(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// waitHealthy reads the daemon's result of either the gate's proxy healthcheck
// or the sandbox's passive UID check. A sandbox's launcher can refuse before
// Docker gets to its first healthcheck. A gate create-race winner, on the
// other hand, may still be waiting for its creator to start it.
func (p *Provider) waitHealthy(ctx context.Context, id, kind string) (containerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, gateHealthTimeout)
	defer cancel()
	ticker := time.NewTicker(gateHealthPoll)
	defer ticker.Stop()
	for {
		info, err := p.api.inspectContainer(ctx, id)
		if err != nil {
			return containerInfo{}, err
		}
		if kind == "sandbox" && !info.State.Running {
			if info.State.ExitCode == uidguard.Refused {
				return info, fmt.Errorf("docker: sandbox %s exited before admission with status %d (possible reserved gate UID %d refusal)", id, uidguard.Refused, uidguard.GateUID)
			}
			return info, fmt.Errorf("docker: sandbox %s stopped before UID admission (exit %d)", id, info.State.ExitCode)
		}
		switch info.State.Health.Status {
		case healthStatusHealthy:
			if info.State.Running {
				return info, nil
			}
		case healthStatusUnhealthy:
			return info, fmt.Errorf("docker: %s %s reported unhealthy", kind, id)
		}
		select {
		case <-ctx.Done():
			return info, fmt.Errorf("docker: %s %s not healthy within %s: %w", kind, id, gateHealthTimeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// guardVolume reads the name assigned by the daemon, so a sandbox can borrow
// this volume alone. An older gate without the staged launcher cannot admit one.
func guardVolume(gate containerInfo) (string, error) {
	if slices.Contains(gate.Config.Env, uidguard.StageEnv+"=1") &&
		slices.Contains(gate.Config.Env, "GATE_UID="+strconv.Itoa(uidguard.GateUID)) {
		for _, m := range gate.Mounts {
			if m.Destination == uidguard.Dir && m.Type == "volume" && m.RW && m.Name != "" {
				return m.Name, nil
			}
		}
	}
	return "", fmt.Errorf("docker: gate %s has no verified sandbox UID guard volume: %w", gate.ID, sandbox.ErrSpecMismatch)
}

func borrowedGuard(mounts []mountedVolume) string {
	for _, m := range mounts {
		if m.Destination == uidguard.Dir && m.Type == "volume" && !m.RW {
			return m.Name
		}
	}
	return ""
}

func (api *apiClient) ownsGuard(ctx context.Context, sid domain.ID, name string) (bool, error) {
	if name == "" || sid.IsZero() {
		return false, nil
	}
	var v volumeInfo
	if err := api.getJSON(ctx, "/volumes/"+url.PathEscape(name), &v); err != nil {
		if statusIs(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	return v.Labels[guardSessionLabel] == string(sid), nil
}

// Removing the gate with v=1 normally removes its anonymous volume. When
// the gate goes first, Docker retains the volume while a borrower holds it.
// The borrower's mount references it by name, so its own v=1 will leave it.
func (api *apiClient) releaseGuard(ctx context.Context, sid domain.ID, name string) error {
	owned, err := api.ownsGuard(ctx, sid, name)
	if err != nil || !owned {
		return err
	}
	resp, err := api.request(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, "")
	if err != nil {
		// A surviving owner or borrower still needs it. Docker checks this
		// at deletion, so a racing provision never loses a mounted volume.
		if statusIs(err, http.StatusConflict) || statusIs(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return resp.Body.Close()
}

// removeDetached removes a failed creation only while stopped. Docker rejects
// an unforced delete if a concurrent provision has already started it. The
// session reaper owns running orphans.
func (p *Provider) removeDetached(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := p.api.removeStoppedContainer(ctx, id); err != nil && !statusIs(err, 404) && !statusIs(err, 409) {
		slog.WarnContext(ctx, "docker: best-effort container removal failed; leaving orphan for the reaper",
			"container", id, "error", err)
	}
}

// gateConfig builds the gate container's config: the gate image on the deploy
// network, holding CAP_NET_ADMIN so it can install its owner-match iptables
// before dropping to its own uid. Entrypoint/Cmd/WorkingDir are left unset so
// the image's own /gate entrypoint stands. The per-session token, the
// control-plane URL, and the OTLP collector config (so the gate's egress_request
// spans reach the same collector as the rest of the platform) travel as env.
func gateConfig(spec sandbox.Spec, token, network string) containerConfig {
	return gateConfigWithCPU(spec, token, network, spec.Gate.Resources.Effective().CPUMillis)
}

func gateConfigWithCPU(spec sandbox.Spec, token, network string, cpuMillis int64) containerConfig {
	res := spec.Gate.Resources.Effective()
	env := map[string]string{
		"CONTROLPLANE_URL": spec.Gate.ControlplaneURL,
		"GATE_TOKEN":       token,
		uidguard.StageEnv:  "1",
		// Stated rather than left to the image's own default, because the
		// platform now depends on knowing it: Hardening.Validate refuses a
		// sandbox running as this uid, since the gate's owner-match rule ACCEPTs
		// it. A custom gate image baking a different GATE_UID would otherwise
		// move the firewall's accepted identity out from under that check
		// silently. Container env beats image env, so this is the value.
		"GATE_UID": strconv.Itoa(gaterun.DefaultGateUID),
	}
	// Only when a collector is configured; an empty endpoint runs the gate without
	// an exporter, matching the executor's own telemetry.Run behavior.
	if spec.Gate.OTelEndpoint != "" {
		env["OTEL_EXPORTER_OTLP_ENDPOINT"] = spec.Gate.OTelEndpoint
		if spec.Gate.OTelInsecure {
			env["OTEL_EXPORTER_OTLP_INSECURE"] = "true"
		}
	}
	return containerConfig{
		Image:  spec.Gate.Image,
		Env:    envSlice(env),
		Labels: map[string]string{sessionLabel: string(spec.SessionID)},
		HostConfig: hostConfig{
			NetworkMode: network,
			Init:        true,
			CapAdd:      []string{"NET_ADMIN"},
			Mounts: []mount{{Type: "volume", Target: uidguard.Dir, VolumeOptions: &volumeOptions{
				Labels: map[string]string{guardSessionLabel: string(spec.SessionID)},
			}}},
			Memory:    res.MemoryBytes,
			NanoCpus:  cpuMillis * 1_000_000,
			PidsLimit: res.PidsLimit,
		},
	}
}
