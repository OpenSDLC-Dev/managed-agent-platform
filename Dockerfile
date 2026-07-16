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

FROM debian:stable-slim
# ca-certificates lets the binaries reach TLS model endpoints and OTLP collectors.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /
# No default command: each service sets one of /controlplane|/brain|/executor|/worker.
