package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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
)

func gateName(sessionID domain.ID) string { return "map-gate-" + string(sessionID) }

// ensureGate creates or adopts the session's gate container and returns its id,
// running and healthy. created reports whether this call created it, so the
// caller can remove the orphan if the sandbox half then fails. The gate owns the
// network namespace the sandbox joins and enforces its egress, so it must exist
// and be healthy before the sandbox is created — the admission gate.
func (p *Provider) ensureGate(ctx context.Context, spec sandbox.Spec) (id string, created bool, err error) {
	name := gateName(spec.SessionID)
	info, ierr := p.api.inspectContainer(ctx, name)
	switch {
	case ierr == nil: // an existing gate (a re-lease, or a later tool call)
		if err := ours(info, spec.SessionID); err != nil {
			return "", false, err
		}
		if info.State.Running {
			// A running gate has already installed its firewall — one that could
			// not aborts with the process still root, so a running gate never
			// serves fail-open. Still require healthy, so a gate whose proxy is
			// not yet (or no longer) serving is not handed a sandbox; an
			// already-healthy gate returns on the first poll.
			if info.State.Health.Status != healthStatusHealthy {
				if err := p.waitGateHealthy(ctx, info.ID); err != nil {
					return "", false, err
				}
			}
			return info.ID, false, nil
		}
		// A stopped gate is not safe to restart: it may have been created but
		// never had its token persisted (a crash between the create and Persist
		// below), or it terminated on a revoked token. Remove it and recreate with
		// a fresh token rather than start a gate the control plane may reject.
		// Docker force-removes the gate even while a stopped sandbox still shares
		// its netns (verified), so a dependent sandbox does not block this; that
		// sandbox is then rebuilt by the NetworkMode-aware adoption in Provision.
		if err := p.api.removeContainer(ctx, info.ID); err != nil && !statusIs(err, 404) {
			return "", false, err
		}
	case !statusIs(ierr, 404):
		return "", false, ierr
	}

	// Create. Mint an in-memory token, create the container with it, and persist
	// it only after winning the create — so a lost create race (another executor
	// created the gate first, adopted below) never revokes the winner's token.
	token := spec.Gate.TokenMinter.Generate()
	cfg := gateConfig(spec, token, p.gateNetwork)
	id, err = p.api.createContainer(ctx, name, cfg)
	if statusIs(err, 404) { // the gate image is not on this host yet
		if perr := p.api.pullImage(ctx, spec.Gate.Image); perr != nil {
			return "", false, perr
		}
		id, err = p.api.createContainer(ctx, name, cfg)
	}
	if statusIs(err, 409) { // another executor won the create; adopt its gate
		won, werr := p.api.inspectContainer(ctx, name)
		if werr != nil {
			return "", false, werr
		}
		if werr := ours(won, spec.SessionID); werr != nil {
			return "", false, werr
		}
		if werr := p.waitGateHealthy(ctx, won.ID); werr != nil {
			return "", false, werr
		}
		return won.ID, false, nil
	}
	if err != nil {
		return "", false, err
	}
	// Won the create. Persist the token before starting, so the gate authenticates
	// its first config fetch; on any failure remove the container we just created.
	if err := spec.Gate.TokenMinter.Persist(ctx, spec.SessionID, token); err != nil {
		p.removeDetached(id)
		return "", false, err
	}
	if err := p.api.startContainer(ctx, id); err != nil {
		p.removeDetached(id)
		return "", false, err
	}
	if err := p.waitGateHealthy(ctx, id); err != nil {
		p.removeDetached(id)
		return "", false, err
	}
	return id, true, nil
}

// waitGateHealthy blocks until the gate reports healthy, failing if it reports
// unhealthy or does not become healthy within gateHealthTimeout.
func (p *Provider) waitGateHealthy(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, gateHealthTimeout)
	defer cancel()
	ticker := time.NewTicker(gateHealthPoll)
	defer ticker.Stop()
	for {
		info, err := p.api.inspectContainer(ctx, id)
		if err != nil {
			return err
		}
		switch info.State.Health.Status {
		case healthStatusHealthy:
			return nil
		case healthStatusUnhealthy:
			return fmt.Errorf("docker: gate %s reported unhealthy", id)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("docker: gate %s not healthy within %s: %w", id, gateHealthTimeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// removeDetached best-effort removes a container on a cleanup path (an orphaned
// gate or a sandbox that failed to start), under a fresh short-lived context so a
// cancelled provision still tears down what it created.
func (p *Provider) removeDetached(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.api.removeContainer(ctx, id)
}

// gateConfig builds the gate container's config: the gate image on the deploy
// network, holding CAP_NET_ADMIN so it can install its owner-match iptables
// before dropping to its own uid. Entrypoint/Cmd/WorkingDir are left unset so
// the image's own /gate entrypoint stands. The per-session token, the
// control-plane URL, and the OTLP collector config (so the gate's egress_request
// spans reach the same collector as the rest of the platform) travel as env.
func gateConfig(spec sandbox.Spec, token, network string) containerConfig {
	env := map[string]string{
		"CONTROLPLANE_URL": spec.Gate.ControlplaneURL,
		"GATE_TOKEN":       token,
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
		},
	}
}
