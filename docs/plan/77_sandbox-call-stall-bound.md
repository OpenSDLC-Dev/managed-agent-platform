---
status: archived
issue: "#395"
---

# Plan 77 — bound a wedged sandbox call by silence inside the Docker and Kubernetes transports

The plan for [#395](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/395), deferred from plan 33 (D5a).

## What is broken

Plan 33 (#383) bounded a wedged *work item* at the lease/heartbeat layer (default 30m), which must clear the longest healthy step (`toolset.MaxTimeout`, cold pulls, large mounts) and only notices silence at step boundaries. Below that layer, individual sandbox transport calls remain unbounded:

1. `internal/sandbox/docker/api.go` sets no HTTP timeout on either `unix://` or `tcp://` clients (`newAPIClient`), so a wedged daemon on control RPCs, `execCreate`/`execStart`/`execInspect`, archive transfers (`getArchive`/`putArchive`), or `pullImage` blocks until the outer 30m item budget expires.
2. `internal/sandbox/k8s/client.go` runs `remotecommand.NewSPDYExecutor` + `StreamWithContext` with no silence bound:
   - Once SPDY streams are established, #318's goroutine dumps sat parked for 8–9 minutes with every stream blocked while SPDY's 5-second Ping/Pong frames kept the underlying TCP connection alive.
   - Worse, during the HTTP 101 upgrade (`newConnectionAndStream`), `SpdyRoundTripper.RoundTrip` calls `http.ReadResponse(bufio.NewReader(conn), nil)` on a raw `net.Conn` with no deadline and no context cancellation, so a peer that accepts the connection and never sends 101 is not even cancellable by context.
   - Kubernetes REST calls (`Pods.Get`/`Create`/`Delete`/`List`) via `kubernetes.NewForConfig` also carry no silence bound.

## Decisions

1. **One shared sentinel and guard in `internal/sandbox/stall.go`.**
   - `sandbox.ErrStalled` ("sandbox: call stalled") is the distinguishable error sentinel, replacing raw `context.Canceled` / `i/o timeout` via `StallGuard.Cause(err)` so callers (including `worker.reclaimFault` and `executor`) classify a transport stall as a platform fault rather than routine cancellation.
   - `sandbox.DefaultStallTimeout = 5 * time.Minute` — 6× tighter than the 30m item budget, yet generous enough to clear silent platform operations such as a 2 GiB checkpoint `tar -xf` or slow image layer unpack without false-tripping.
   - `sandbox.StallGuard` mirrors `provider.StallGuard` (`NewStallGuard`, `Progress`, `Stop`, `Cause`), adding `Disarm() bool` so a timed `Exec` can stop the silence watcher without cancelling `http.Request`'s context once the exec stream is established.

2. **Bidirectional progress measurement, never fooled by SPDY keepalive pings.**
   - **Docker (`internal/sandbox/docker`):** Every `apiClient` call runs under a `StallGuard`. Request body reads (`ProgressReadCloser`, preserving `ContentLength` for `*bytes.Reader`) report progress so 500 MB `WriteFileStream` / `WriteFiles` uploads stay alive while bytes move; response header arrival and every `resp.Body.Read` (`GuardReadCloser`) report progress, including on returned `ReadFileStream` and `Export` streams (stopping the guard on `Close`). A stalled call short-circuits path-classification probes (`pathFault`, `mkdirAll` retry, `unreplaceable`, `notWritable`, `refusedBulk`) so a wedged daemon does not multiply the budget.
   - **Kubernetes (`internal/sandbox/k8s`):**
     - `client.exec` builds a `guardedSPDY` round-tripper/upgrader passed to `remotecommand.NewSPDYExecutorForTransports`, reusing `httpstreamspdy.SpdyRoundTripper.Dial` (preserving full TLS, proxy, and `rest.HTTPWrappersForConfig` auth support).
     - Between `Dial` and 101 validation, `conn.SetDeadline` plus a mutex-guarded `context.AfterFunc` bounds `http.ReadResponse` (and any non-101 error body read) at the raw `net.Conn` level, making a wedged HTTP 101 handshake both silence-bounded and caller-cancellable. Before `NewClientConnectionWithPings`, the raw deadline is cleared and any buffered bytes after 101 are preserved.
     - Post-101, progress is tracked by wrapping `httpstream.Connection` (`CreateStream`) and each `httpstream.Stream` (`Read` and `Write`). Because `spdystream` consumes 5s SPDY Ping/Pong frames below the stream layer, keepalive pings never reset the silence guard — only real stdin/stdout/stderr/error bytes do.
     - `pod.execErr` returns `sandbox.ErrStalled` immediately without spending 10s on a diagnostic `Pods.Get`.
     - The REST `clientset` wraps its `rest.Config` transport with `sandbox.GuardRoundTripper` (on a copy separate from the SPDY `rest.Config` to avoid double-guarding).

3. **Timed `Exec` (`req.Timeout > 0`) vs untimed `Exec` and file primitives.**
   - A command invoked with `ExecRequest.Timeout > 0` (e.g., `bash` up to `toolset.MaxTimeout = 10m`, package installs) may legitimately produce zero stdout/stderr for its full timeout, and `Exec` already bounds it with `runCtx = context.WithTimeout(ctx, deadline+killGrace)`.
   - Therefore, for `Exec` with `req.Timeout > 0`, the silence guard bounds **all transport handshakes and control RPCs** (`execCreate`, `execStart` HTTP headers, K8s SPDY HTTP 101 upgrade + stream creation, `execInspect`, `processAlive`, `pathExists`, `readExit`, `probeAlive`), and calls `guard.Disarm()` once the exec stream is open so `runCtx` remains the sole bound during command execution.
   - For untimed `Exec` (`req.Timeout == 0`) and all file/export primitives (`ReadFile`, `ReadFileStream`, `WriteFile`, `WriteFileStream`, `WriteFiles`, `Export`), the silence guard stays armed end-to-end.

4. **Configuration surface.**
   - Add `StallTimeout time.Duration` to `docker.Config`, `k8s.Config`, and `backend.Config` (zero takes `sandbox.DefaultStallTimeout`).
   - Expose optional `SANDBOX_STALL_TIMEOUT` in `cmd/executor` and `cmd/worker`, parsed via tested `sandbox.ParseStallTimeout` (empty = default; malformed or non-positive fails startup).

## Acceptance

- Shared contract suite (`internal/sandbox/sandboxtest`):
  - Live-sandbox stall rows in `sandboxtest.Run` (via `Harness.WithStallTimeout`) run against real Docker and K8s backends: silent untimed `Exec` fails with `sandbox.ErrStalled`, active untimed `Exec` outlives the budget, silent timed `Exec` outlives the budget, streaming `WriteFileStream` outlives the budget while bytes move, and a stalled `WriteFileStream` source fails with `sandbox.ErrStalled`.
  - Transport-level contract suite `sandboxtest.RunTransportStall` runs against both `docker` and `k8s` transports with wedged peers: wedged handshake (HTTP 200 / HTTP 101 raw `net.Conn` with 0 bytes), wedged mid-stream `Exec`, wedged `ReadFile`, wedged `ReadFileStream`, wedged `WriteFile` (including post-stdin-EOF silence with active SPDY pings on K8s), and caller cancellation during a wedged handshake (prompt return, not `ErrStalled`).
- Mutation verification: removing the Docker guard, K8s SPDY stream guard, or K8s raw `net.Conn` handshake deadline causes the corresponding contract tests to fail (hang until `failWithin` expires).
- Full `make verify` (including `cover-gate >= 90%`) and `make test-race` pass cleanly.
