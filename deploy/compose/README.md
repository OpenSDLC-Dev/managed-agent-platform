# Local development stack (docker compose)

Brings up the platform's three server processes — **controlplane**, **brain**,
**executor** — against a bundled Postgres, a bundled MinIO (S3-compatible
object storage where the controlplane stores skill archives — the `/v1/skills`
registry, docs/plan/06_skills.md), and a bundled OpenBao (the transit cipher
that encrypts vault credential material, docs/plan/12_vaults-credentials.md),
so you can drive the API with the real `ant` CLI or the Anthropic SDKs on your
laptop. It's the compose companion to the [Helm chart](../helm); same binaries
(built from the repo-root `Dockerfile`), wired for local use.

The **BYOC worker** is intentionally not here — it runs on your own compute,
outside the platform. Run it separately with `go run ./cmd/worker` (or the built
`worker` binary) pointed at this controlplane.

## Quick start

From this directory:

```sh
cp .env.example .env         # set CONTROLPLANE_API_KEY
docker compose up --build
```

- Control plane API: `http://localhost:8080` (bound to loopback by default; see below).
- Drive it with the real CLI: `ANTHROPIC_API_KEY=<CONTROLPLANE_API_KEY> ant --base-url http://localhost:8080 beta:agents list` (management commands ignore `ANTHROPIC_BASE_URL`; only the worker/auth subcommands honor it).

The stack comes up out of the box — the brain loads the committed
`model-providers.example.json` and idles (its placeholder endpoint isn't real, so
model *turns* won't run until you point it at your own; see below). To use your
endpoint:

```sh
cp model-providers.example.json model-providers.json   # then edit base_url + api_key
# in .env: MODEL_PROVIDERS_FILE=model-providers.json
```

The first `--build` compiles the Go binaries; later `up`s reuse the image. Every
binary applies database migrations itself on connect (advisory-locked), so there
is no separate migrate step.

The API is published on **loopback (`127.0.0.1`) by default**, because the
committed `CONTROLPLANE_API_KEY` is a well-known placeholder — anyone who can
reach the port can drive the API with it. To expose it on the LAN, set a real key
and `CONTROLPLANE_BIND=0.0.0.0` in `.env`.

## Configuration

`.env` (read automatically by compose; never commit it):

| Variable | Purpose |
|---|---|
| `CONTROLPLANE_API_KEY` | **Required.** Bootstrap management key the CLI/SDKs send as `x-api-key`. |
| `POSTGRES_PASSWORD` | Password for the bundled Postgres (default `map`). Embedded in the DSN, so keep it URL-safe (no `@ : / ? # %` or spaces). |
| `CONTROLPLANE_PORT` | Host port for the control plane (default `8080`). |
| `CONTROLPLANE_BIND` | Interface the port binds to (default `127.0.0.1`). Set `0.0.0.0` — with a real key — to expose on the LAN. |
| `MODEL_PROVIDERS_FILE` | Brain routing file to mount (default `model-providers.example.json`). Set to your copy to use a real endpoint. |
| `EXECUTOR_IMAGE` | Base image for per-session sandbox containers (default `debian:stable-slim`). |
| `EXECUTOR_REAP_INTERVAL` | Sandbox reap pass interval, a Go duration (default `1m`). Each pass destroys the sandboxes of deleted (tombstone-evidenced), archived, and terminated **cloud** sessions; live sessions, self_hosted sessions, and sandboxes this deployment's database never saw are never touched. |
| `EXECUTOR_CHECKPOINT_MAX_BYTES` | Workspace-checkpoint size budget in bytes (default 2 GiB). Over budget, an idle-TTL reap proceeds without a checkpoint rather than letting a full disk pin the sandbox immortal. |
| `EXECUTOR_SANDBOX_IDLE_TTL` | Idle sandbox lifetime, a Go duration (default `24h`; `0` disables the idle tier). An idle cloud session older than this is checkpointed to MinIO and its sandbox reaped; the next `user.message` provisions fresh and restores the workspace. A session still owing work (a queued/starting/active work item) or an unanswered tool confirmation is left alone. |
| `SANDBOX_PIDS_LIMIT` / `SANDBOX_CPU_MILLIS` / `SANDBOX_CAP_DROP` | Sandbox containment, **on by default** — 512 processes, 2000 millicores (2 CPUs), and the `NET_RAW,SETUID,SETGID` drops (#65). Set them to lower the caps; `0` (or `none` for the capabilities) turns one off. A malformed value fails executor startup rather than falling back to the default. |
| `SANDBOX_MEMORY_BYTES` / `SANDBOX_READONLY_ROOTFS` / `SANDBOX_RUN_AS_USER` | The opt-in half: a memory cap (off by default — an OOM kill mid-task is worse than throttling), a read-only root filesystem (the provider mounts writable volumes over the workdir, `/tmp`, the persistent shell's state root and the file-resource mount root), and a numeric uid overriding the image's own `USER`. The last needs an image whose workdir that uid can reach — see [docs/self-hosted-security.md](../../docs/self-hosted-security.md). There is deliberately no disk-cap variable here: `SANDBOX_EPHEMERAL_STORAGE_BYTES` is Kubernetes-only, because whether a Docker daemon enforces a writable-layer quota depends on its storage driver — some do, some refuse the option outright, and some accept it and enforce nothing — so the platform declines to report a cap it cannot vouch for. Bound this stack's disk at the host instead. |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | Credentials for the bundled MinIO (defaults `map` / `map-blob-dev`; the password needs ≥ 8 characters). The controlplane's and executor's `BLOB_*` wiring follows them automatically. |
| `BAO_STATIC_KEY` / `BAO_PLATFORM_TOKEN` / `BAO_TRANSIT_KEY` | Static-unseal key (base64, 32 bytes), platform token, and transit key name (default `map-secrets`) for the bundled OpenBao (committed dev defaults; rotate via `.env` beyond a laptop). The `openbao-init` one-shot initializes the instance on first boot — root token and recovery keys land on the `baoinit` volume, a dev-grade convenience — and mints/renews the platform token, scoped to exactly that transit key, which the controlplane's and executor's `SECRETS_*`/`BAO_*` wiring follows automatically. |
| `TAVILY_API_KEY` / `JINA_API_KEY` / `WEBSEARCH_BASE_URL` / `WEBFETCH_BASE_URL` | The executor's web-tool backends (docs/plan/15_web-tools.md). No `TAVILY_API_KEY` leaves `web_search` unconfigured; neither `JINA_API_KEY` nor `WEBFETCH_BASE_URL` leaves `web_fetch` unconfigured (each answers the model with an error naming what is missing). Empty base URLs mean the public endpoints. |
| `WEBTOOL_ALLOWED_DOMAINS` | Comma-separated operator allowlist for both web tools (#225); empty = unrestricted. An entry is a bare hostname, an IPv4 literal, or a `*.`-wildcard, validated at startup: a bare entry admits **only that exact host**, `*.example.com` admits **subdomains but not the apex** — list both to get both. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector for traces, metrics **and logs**; empty disables telemetry export entirely. Set to `jaeger:4317` with the observability profile — but note Jaeger ingests **traces only**, so each failed log-export batch prints one `Unimplemented … LogsService` line to stderr. Point at a collector that takes all three (an OTel Collector, Grafana Alloy) to silence it. |

The **model routing** file (mounted into the brain at
`/etc/map/model-providers.json`) is a **JSON array** of routes, each with `model`
(`"*"` is the default route), `protocol` (`anthropic` or `openai`), `base_url`,
and `api_key`. `base_url` is the **API root** — the adapter appends the protocol
path (`/v1/messages` or `/v1/chat/completions`), so give e.g.
`https://api.openai.com`, **not** `.../v1`. See `model-providers.example.json` and
`internal/provider` (`LoadRoutes`). The mount defaults to the committed example
(so the stack starts and idles); point `MODEL_PROVIDERS_FILE` at your gitignored
copy for real turns.

A route may also set `upstream_model`, the model id the endpoint actually
receives. Leaving it unset — as the committed example does — **passes the
agent's own model string through** to the endpoint, which is the point of a
`"*"` route in front of a gateway that already understands your model names.
Note what that also means for metrics: the passed-through string becomes the
`gen_ai.request.model` attribute on `gen_ai.client.operation.duration` and
`gen_ai.client.token.usage`, and metric attributes are aggregation keys, so
under a `"*"` route with no `upstream_model` **whoever can supply a model
string controls how many series your metrics backend stores**. That is more
than agent creation: a session may carry an `agent_with_overrides` block whose
`model` overrides the agent's, on both `POST /v1/sessions` and a later `PATCH`,
so per-request session creation is an injection point too. That the label
follows the string is deliberate — it is genuinely the interesting dimension in
exactly this deployment — but if any of those paths is exposed to untrusted
callers, set `upstream_model`, or replace the `"*"` route with per-model routes
([#88](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/88)).

A route may also set `stall_timeout`, a Go duration (`"90s"`, `"2m"`) bounding how
long that endpoint may send **nothing at all** before the turn is abandoned with a
`session.error`. Every byte the endpoint sends — a keepalive included — buys the
budget back, so it never ends a healthy turn however long the model streams; what
it ends is a wedged proxy that accepts the connection and then goes silent, which
would otherwise hold a brain replica forever
([#121](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/121)).
Omitted, it is 10 minutes — sized for an endpoint that queues a request before it
sends its first byte. Tighten it per route if you know yours answers faster; it
must be positive, and a zero or negative value is rejected at startup rather than
read as "no bound". Tighten it a long way and the budget starts to cover the
brain's own work between chunks, so a stalled database can end a turn as if the
model had gone quiet.

A route may also set `max_tokens`, the default output cap for turns that set none
themselves. Omitted, the anthropic adapter sends its required-field fallback of
8192 and the openai adapter leaves the field off the wire (the endpoint's default
applies). Size it for the workload behind the route: an agent that writes whole
files through tool calls is cut off mid-call by a cap sized for chat, and the
turn fails with `model_request_failed_error` after retries. It must be positive —
an explicit zero is rejected at startup rather than read as "the default".

## The sandbox and the Docker socket

The executor runs each session's tools in a per-session sandbox container. Under
compose it uses the **docker** backend and mounts the host Docker socket
(`/var/run/docker.sock`), launching sandbox containers as siblings on your host
daemon — a local-dev convenience. The production path uses the Kubernetes backend
(pod-per-session) instead; see the Helm chart.

The stack is gate-wired: a session on a `limited` environment (or with vaults
attached) gets a per-session **egress gate** — a forward proxy the sandbox's
`HTTP(S)_PROXY` rides through, admitting only the environment's
`allowed_hosts` and substituting vault placeholders on plain-HTTP egress. The
`gate-image` service builds the gate image (`Dockerfile --target gate`) onto
the host daemon before the executor starts; the executor opts in via
`CONTROLPLANE_URL` + `EXECUTOR_GATE_IMAGE`, and `SANDBOX_DOCKER_GATE_NETWORK`
puts the gate on the stack's network so it can fetch its policy from
`http://controlplane:8080`. Gate containers are siblings on the host daemon,
same as the sandboxes that join their network namespace.

## Traces (optional)

```sh
# in .env: OTEL_EXPORTER_OTLP_ENDPOINT=jaeger:4317
docker compose --profile observability up --build
```

Jaeger UI: `http://localhost:16686`.

The endpoint is one address for all three signals, and this Jaeger takes only
traces: the metric and log exporters will keep reporting `Unimplemented` to
stderr, one line per failed batch. Harmless — traces still arrive, and the
platform's own logs still reach the console — but if you want the logs stored
and the noise gone, put an OTel Collector at `4317` and let it fan out.

## Teardown

```sh
docker compose down          # stop and remove containers
docker compose down -v       # also drop the volumes (wipes all data — Postgres,
                             # MinIO, and OpenBao together; ciphertext and its
                             # transit key live and die as a pair)
```
