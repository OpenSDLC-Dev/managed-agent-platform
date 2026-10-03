# Multi-stage build for the platform's server binaries. One image carries all of
# cmd/{controlplane,brain,executor,worker}; each compose service (and a Helm
# deployment) selects the binary it runs via the container command. Kept minimal
# and static (CGO off) so the runtime image is small and needs no toolchain. The
# binaries live at the filesystem root (/controlplane …) — that is the path the
# Helm chart's Deployments invoke, so this one image serves compose and Helm both.
#
# syntax=docker/dockerfile:1
# The build stages (modules, and the two built from it) are pinned to the
# build host's own platform and Go cross-compiles to the target — a multi-arch
# `buildx --platform` run must not execute the whole Go toolchain under QEMU
# emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS modules
WORKDIR /src
# Download modules first so the layer caches across source-only changes.
COPY go.mod go.sum ./
RUN go mod download

# The egress gate's binary is built apart from the server binaries: it embeds
# no ripgrep, so `--target gate` neither needs the archives nor fetches them.
# Its stages come first, so that holds for a builder that builds every stage
# before its target, in file order — the classic one does, though this file's
# `--platform=$BUILDPLATFORM` already shuts it out — as well as for BuildKit,
# which builds only the stages a target needs.
FROM modules AS gate-build
COPY . .
# VERSION is stamped into internal/version.Version by the release pipeline
# (docs/RELEASING.md); an unarged build reports "dev".
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-X github.com/OpenSDLC-Dev/managed-agent-platform/internal/version.Version=${VERSION}" \
    -o /out/ ./cmd/gate

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
COPY --from=gate-build /out/gate /gate
HEALTHCHECK --interval=2s --timeout=3s --start-period=2s --retries=15 \
    CMD ["/gate", "-healthcheck"]
ENTRYPOINT ["/gate"]

FROM modules AS build
# The pinned static ripgrep the executor and worker embed for the grep tool
# (internal/ripgrep; `make ripgrep` is the same command), fetched twice. First
# from the manifest and the fetcher's own source alone, so the download is a
# layer the build cache keeps until one of those changes — the pin moving,
# chiefly — rather than one every source change re-runs. Then again after
# `COPY . .`, over whatever the build context carried into the assets
# directory: each archive is checked against the manifest's sha256 and fetched
# again if it does not match, and everything else there but the manifest is
# removed. So a checkout that ran `make ripgrep` builds without reaching
# GitHub, a clean one downloads once per pin, and nothing the context carried
# is embedded unchecked.
COPY internal/ripgrep/ripgrep.go internal/ripgrep/
COPY internal/ripgrep/assets/manifest.json internal/ripgrep/assets/
COPY tools/ripgrepfetch/main.go tools/ripgrepfetch/
RUN go run ./tools/ripgrepfetch
COPY . .
RUN go run ./tools/ripgrepfetch
# Build the four server binaries into /out (named controlplane, brain,
# executor, worker).
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-X github.com/OpenSDLC-Dev/managed-agent-platform/internal/version.Version=${VERSION}" \
    -o /out/ ./cmd/controlplane ./cmd/brain ./cmd/executor ./cmd/worker

# The default (last) stage is the server image carrying the four server binaries.
FROM debian:stable-slim AS server
# ca-certificates lets the binaries reach TLS model endpoints and OTLP collectors.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# The four server binaries — the gate binary has its own image (above) and
# does not belong in the server image. NOTICE says what third-party
# software the executor and worker embed (the ripgrep their grep tool runs),
# THIRD_PARTY_LICENSES carries its license texts, and LICENSE is the
# project's own, which NOTICE refers to.
COPY --from=build /out/controlplane /out/brain /out/executor /out/worker /
COPY LICENSE NOTICE THIRD_PARTY_LICENSES /
# No default command: each service sets one of /controlplane|/brain|/executor|/worker.
