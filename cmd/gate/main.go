// Command gate is one session's egress gate. It runs as a sidecar sharing the
// sandbox's network namespace: on startup it installs owner-match iptables rules
// (egress permitted only for the gate's own UID and loopback, everything else
// dropped), verifies they took, and drops to that unprivileged UID; thereafter
// it serves internal/gate's forward proxy on a loopback port the sandbox reaches
// via HTTP_PROXY, keeping its policy and resolved credentials current by
// periodically fetching the session's config from the control plane.
//
// Environment:
//   - CONTROLPLANE_URL   (required) base URL of the internal gate-config endpoint
//   - GATE_TOKEN         (required) the per-session gtk_ bearer token
//   - GATE_ADDR          proxy listen address (default 127.0.0.1:15080; loopback)
//   - GATE_UID           dedicated UID for owner-match + privilege drop (default 65532)
//   - GATE_GID           dedicated GID for privilege drop (default GATE_UID)
//   - GATE_FETCH_INTERVAL config refresh interval, a Go duration (default 30s)
//   - OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_INSECURE  telemetry export
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gate"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

const (
	defaultAddr          = "127.0.0.1:15080"
	defaultGateUID       = 65532
	defaultFetchInterval = 30 * time.Second
)

func main() {
	// The Docker HEALTHCHECK invokes the binary itself so the image needs no
	// extra probe tooling. A listening proxy port means Setup already applied and
	// verified the firewall (the listener starts only after it), so this is also
	// the admission signal that egress is fail-closed.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if !telemetry.Run(ctx, telemetry.Config{
		ServiceName: "gate",
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Insecure:    os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
	}, run) {
		os.Exit(1)
	}
}

// healthcheck dials the proxy listen address and reports 0 if it accepts a
// connection, 1 otherwise. It reads the same GATE_ADDR the server binds.
func healthcheck() int {
	addr := os.Getenv("GATE_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return 1
	}
	_ = conn.Close()
	return 0
}

func run(ctx context.Context) error {
	controlplaneURL := os.Getenv("CONTROLPLANE_URL")
	if controlplaneURL == "" {
		return errors.New("CONTROLPLANE_URL is required")
	}
	token := os.Getenv("GATE_TOKEN")
	if token == "" {
		return errors.New("GATE_TOKEN is required")
	}
	addr := os.Getenv("GATE_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	gateUID, err := intEnv("GATE_UID", defaultGateUID)
	if err != nil {
		return err
	}
	gateGID, err := intEnv("GATE_GID", gateUID)
	if err != nil {
		return err
	}
	interval := defaultFetchInterval
	if v := os.Getenv("GATE_FETCH_INTERVAL"); v != "" {
		if interval, err = time.ParseDuration(v); err != nil {
			return errors.New("GATE_FETCH_INTERVAL must be a Go duration")
		}
	}

	// Firewall + privilege drop before anything listens, so the HEALTHCHECK that
	// gates the sandbox's admission cannot pass until egress is fail-closed.
	if err := gaterun.Setup(ctx, iptablesFirewall{}, privDropper{uid: gateUID, gid: gateGID}, gateUID); err != nil {
		return err
	}

	// Serve a deny-all gate until the first successful fetch populates the policy.
	handler := gaterun.NewSwappableHandler(gate.New(gate.Config{}))
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe() }()
	slog.Info("gate listening", "addr", addr, "uid", gateUID)

	client := gateconfig.NewClient(controlplaneURL, token, nil)
	loopDone := make(chan error, 1)
	go func() { loopDone <- gaterun.RunFetchLoop(ctx, client, handler, interval) }()

	select {
	case err := <-srvErr:
		return err
	case err := <-loopDone:
		// The fetch loop ended: ErrUnauthorized (the token was revoked or the
		// session archived) or nil (ctx cancelled). Either way the gate stops
		// serving — a revoked session's gate must not keep proxying.
		if err != nil {
			slog.Info("gate config fetch loop ended; shutting down", "err", err)
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// intEnv reads an integer environment variable, or def when unset.
func intEnv(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New(name + " must be an integer")
	}
	return n, nil
}
