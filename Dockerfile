# Multi-stage build for the platform's server binaries. One image carries all of
# cmd/{controlplane,brain,executor,worker,modelgateway}; each compose service (and a Helm
# deployment) selects the binary it runs via the container command. Kept minimal
# and static (CGO off) so the runtime image is small and needs no toolchain. The
# binaries live at the filesystem root (/controlplane …) — that is the path the
# Helm chart's Deployments invoke, so this one image serves compose and Helm both.
#
# syntax=docker/dockerfile:1
# The build stages (modules, and the three built from it) are pinned to the
# build host's own platform and Go cross-compiles to the target — a multi-arch
# `buildx --platform` run must not execute the whole Go toolchain under QEMU
# emulation.
# The Go here is go.mod's `toolchain` line, the release CI's setup-go installs,
# pinned by tag and multi-arch index digest so every release compiles with the
# patch release CI tested. That fixes the Go build toolchain only: the runtime
# stages below still start from the floating debian:stable-slim and install
# unversioned apt packages. A bump moves go.mod and this line together, and
# `make pins-test` fails while they disagree; the digest is the top-level one
# `docker buildx imagetools inspect` prints for the new tag.
FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS modules
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

# The two packages the early ripgrep fetch below takes — internal/ripgrep, its
# assets directory included, and the fetcher — copied whole, every file a
# later change adds to either included, and their tests removed: the build
# stage copies what is left with COPY --from, which BuildKit keys by the
# content it copies, so an edit to a test re-runs this stage and not the fetch.
# (COPY --exclude would say this in one line, but it needs the Dockerfile 1.19
# frontend, and this file is built with each builder's own: the `# syntax=`
# line above is not the file's first, so no builder reads it as a directive.)
FROM modules AS ripgrep-sources
COPY internal/ripgrep/ internal/ripgrep/
COPY tools/ripgrepfetch/ tools/ripgrepfetch/
RUN find internal/ripgrep tools/ripgrepfetch -name '*_test.go' -delete

FROM modules AS build
# The pinned static ripgrep the executor and worker embed for the grep tool
# (internal/ripgrep; `make ripgrep` is the same command), fetched twice. First
# with the two packages it takes, less their tests, and nothing else
# (ripgrep-sources), so the fetch is a layer the build cache keeps until one
# of their other files changes rather than one every source change re-runs,
# and it sees the archives the build context carries: a checkout that ran
# `make ripgrep` checks them against the manifest's sha256 and downloads
# nothing, and a clean one downloads once per pin. Then again after `COPY .
# .`, which brings the assets directory in again from the context: each
# archive is checked again, fetched again if it does not match, and
# everything else there but the manifest removed, so nothing the context
# carried is embedded unchecked.
COPY --from=ripgrep-sources /src/internal/ripgrep/ internal/ripgrep/
COPY --from=ripgrep-sources /src/tools/ripgrepfetch/ tools/ripgrepfetch/
RUN go run ./tools/ripgrepfetch
COPY . .
RUN go run ./tools/ripgrepfetch
# Build the five server binaries into /out (named controlplane, brain,
# executor, worker, modelgateway).
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-X github.com/OpenSDLC-Dev/managed-agent-platform/internal/version.Version=${VERSION}" \
    -o /out/ ./cmd/controlplane ./cmd/brain ./cmd/executor ./cmd/worker ./cmd/modelgateway

# The default (last) stage is the server image carrying the five server binaries.
FROM debian:stable-slim AS server
# ca-certificates lets the binaries reach TLS model endpoints and OTLP collectors.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# The five server binaries — the gate binary has its own image (above) and
# does not belong in the server image. NOTICE says what third-party
# software the executor and worker embed (the ripgrep their grep tool runs),
# THIRD_PARTY_LICENSES carries its license texts, and LICENSE is the
# project's own, which NOTICE refers to.
COPY --from=build /out/controlplane /out/brain /out/executor /out/worker /out/modelgateway /
COPY LICENSE NOTICE THIRD_PARTY_LICENSES /
# No default command: each service sets one of /controlplane|/brain|/executor|/worker|/modelgateway.
