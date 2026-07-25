# Multi-stage build for the platform's server binaries. One image carries all of
# cmd/{controlplane,brain,executor,worker}; each compose service (and a Helm
# deployment) selects the binary it runs via the container command. Kept minimal
# and static (CGO off) so the runtime image is small and needs no toolchain. The
# binaries live at the filesystem root (/controlplane …) — that is the path the
# Helm chart's Deployments invoke, so this one image serves compose and Helm both.
#
# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
# Download modules first so the layer caches across source-only changes.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build every binary into /out (named controlplane, brain, executor, worker).
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/...

# The per-session egress gate is a separate image (built with --target gate): it
# needs iptables (to install owner-match rules) and a dedicated UID it drops to,
# neither of which belongs in the minimal server image. It starts as root to
# apply the firewall, then drops to uid 65532 itself — so no USER directive. The
# HEALTHCHECK invokes the binary's own probe: a listening proxy port means the
# firewall was applied and verified first, so it doubles as the admission signal.
FROM debian:stable-slim AS gate
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates iptables \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 65532 gate \
    && useradd -u 65532 -g 65532 -M -s /usr/sbin/nologin gate
COPY --from=build /out/gate /gate
HEALTHCHECK --interval=2s --timeout=3s --start-period=2s --retries=15 \
    CMD ["/gate", "-healthcheck"]
ENTRYPOINT ["/gate"]

# The default (last) stage is the server image carrying the four server binaries.
FROM debian:stable-slim AS server
# ca-certificates lets the binaries reach TLS model endpoints and OTLP collectors.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# Copy only the four server binaries — the gate binary has its own image (above)
# and does not belong in the server image.
COPY --from=build /out/controlplane /out/brain /out/executor /out/worker /
# No default command: each service sets one of /controlplane|/brain|/executor|/worker.
