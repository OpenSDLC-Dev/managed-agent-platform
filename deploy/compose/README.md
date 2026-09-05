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

## A second stack beside the first

Compose namespaces what it creates by **project**, and the `name:` at the top of
`docker-compose.yml` makes two checkouts of this repo the same project by default —
so bringing a branch build up next to a running stack means naming it:

```sh
docker compose -p mapsecond up --build
```

That one flag is the whole of the isolation. The executor's gate network and both
`:local` image tags derive from the project name rather than being pinned, so a
second stack's gates join its own network and its builds stay its own. Pinning
them is what once let a branch stack resolve `postgres` to a running stack's
container and migrate its database
([#438](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/438)).

The project name scopes the containers, the network, the volumes and the `:local`
image tags. What it does not scope is **host ports**, and it does not pick a
different `.env` — two stacks run from the same directory read the same one. The
published API port is the one you will hit, and it says so at once, so pass the
override in the shell rather than the file:

```sh
CONTROLPLANE_PORT=8081 docker compose -p mapsecond up --build
```

Under `--profile iam` the same goes for `IDP_PORT`. The `observability` profile's
Jaeger ports are fixed, so two stacks cannot both run that profile unchanged.

Give `-p` to **every** verb, not just `up` — `down`, `logs`, `ps`. Without it the command
addresses the default project, which is the other stack; see [Teardown](#teardown) for the
one that loses data.

All of this rides on Compose exposing the *resolved* project name for interpolation, which
the Compose Specification requires. One version caveat, worth stating because it is silent
when it bites: **before Compose 2.27.1**, an exported `COMPOSE_PROJECT_NAME` won the
interpolation while `-p` still named the project, so the stack came up on `mapsecond_default`
while its executor was told `managed-agent-platform_default` — #438 again, in the half you
cannot see. Either run 2.27.1 or newer, or pick one mechanism: `-p`, or the environment
variable, never a different value in each.

## Configuration

Two files, and each documents its own settings in place rather than here:

- **[`.env.example`](./.env.example)** — copy it to `.env` (compose reads that
  automatically; never commit it). It carries the handful you are expected to set:
  `CONTROLPLANE_API_KEY` is the only **required** one, and `CONTROLPLANE_BIND` stays
  on loopback until you replace the placeholder key.
- **[`docker-compose.yml`](./docker-compose.yml)** — every other variable the stack
  passes through, declared on the service that reads it and grouped under comments
  explaining the group: the executor's timeouts and reap intervals, sandbox
  containment, MinIO and OpenBao's dev credentials, and the web-tool backends. Not every
  variable has prose of its own there. Where a group needs more, the code has it: the
  binaries' package docs (`go doc ./cmd/executor`) are the authority on their own
  `EXECUTOR_*` and `DATABASE_URL` settings, and the seven `SANDBOX_*` containment
  variables are documented in `internal/sandbox` (`hardening.go`), which is what reads
  them.

Three defaults worth knowing before you change anything: **sandbox containment is
on** (512 processes, 2 CPUs, and the `NET_RAW`/`SETUID`/`SETGID` drops), so those
variables exist to lower or disable it rather than to enable it; **identity is off**,
so management auth is `x-api-key` alone; and the **web tools are unconfigured**
without their backend keys, answering the model with an error that names what is
missing rather than failing the turn.

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

An `anthropic` route may also set `flatten_search_results: true` if its endpoint
rejects `search_result` blocks inside a replayed `web_search` tool_result — MiniMax's
Anthropic-protocol endpoint does, answering `400 invalid params, invalid tool_result
content` and ending the turn with `retries_exhausted`
([#565](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/565)). It
rewrites each `search_result` block to text (the same rendering the openai adapter
always applies) before the request leaves the adapter; every other content block
passes through unchanged. Default off, and rejected at startup on a `protocol: openai`
route, since that adapter already flattens unconditionally — see docs/DIVERGENCES.md
for why the default is off.

## The sandbox and the Docker socket

The executor runs each session's tools in a per-session sandbox container. Under
compose it uses the **docker** backend and mounts the host Docker socket
(`/var/run/docker.sock`), launching sandbox containers as siblings on your host
daemon — a local-dev convenience. The production path uses the Kubernetes backend
(pod-per-session) instead; see the Helm chart.

The stack is gate-wired: a session on a `limited` environment (or with vaults
attached) gets a per-session **egress gate** — a forward proxy the sandbox's
`HTTP(S)_PROXY` rides through, admitting the environment's `allowed_hosts`
plus whatever its `allow_mcp_servers` and `allow_package_managers` flags
open, and substituting vault placeholders on plain-HTTP egress. The
`gate-image` service builds the gate image (`Dockerfile --target gate`) onto
the host daemon before the executor starts; the executor opts in via
`CONTROLPLANE_URL` + `EXECUTOR_GATE_IMAGE`, and `SANDBOX_DOCKER_GATE_NETWORK`
puts the gate on **this** stack's own network — derived from the compose project
name, never pinned; see "A second stack beside the first" — so it can fetch its policy from
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

## Single sign-on (optional)

```sh
# in .env: uncomment all seven SSO lines — the six IDENTITY_* and SSL_CERT_FILE
# (they ship commented out in .env.example)
docker compose --profile iam up --build
```

Two services join the stack (docs/plan/31_console-sso-rbac.md,
[#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56)): **casdoor**, a
local-account OIDC provider, so "the platform's default IAM works out of the box" is true
on a laptop; and **idp**, a small Caddy proxy that is the only published way to reach it.
The sign-in page is `http://localhost:8000`. Nothing else about the stack changes — with
the profile off, `IDENTITY_MODE` stays empty and management auth is `x-api-key` only.

The seven variables move as a set, because `IDENTITY_MODE` gates the rest: with it unset the
control plane reads none of the others, so uncommenting six of seven changes nothing. The
seventh, `SSL_CERT_FILE`, is the one whose absence is not silent — leave it commented and
the control plane cannot verify the proxy's certificate, so it exits at boot with
`x509: certificate signed by unknown authority` and restarts forever.
`.env.example` explains all six in one block above the commented-out assignments. Three of
them are load-bearing rather than cosmetic: the issuer must equal the IdP's `origin` byte
for byte; `IDENTITY_OIDC_JWKS_URL` is **not** optional here, because the control plane
requires an https key set and so fetches through the proxy's TLS listener while the browser
keeps plain HTTP on the issuer; and `IDENTITY_CLAIM_ROLES` must be `groups` rather than
`roles`.

Three accounts are seeded, one per role:

| Sign in as | Password | Casdoor group | Platform role |
|---|---|---|---|
| `map-admin` | `map-admin-dev` | `map/platform-admins` | `admin` |
| `map-dev` | `map-dev-dev` | `map/platform-devs` | `developer` |
| `map-viewer` | `map-viewer-dev` | `map/platform-read` | `viewer` |

They live in a seeded `map` organization rather than Casdoor's `built-in`, whose users are
all Casdoor global administrators. **These are well-known dev credentials** — committed in
`casdoor-init-data.json`, the same status as the `BAO_STATIC_KEY` default above — which is
why the proxy publishes on loopback (`IDP_BIND`/`IDP_PORT`), and why anything past a laptop
replaces them before it exposes the port.

Casdoor's own administrator is separate from all three — `built-in/admin`, password
`map-iam-admin-dev` — and it is the login for the IdP's admin UI, where you add real users
and put them in groups. The seed **resets** that password, because Casdoor's own default
for the account is the publicly documented `123`. It is not a platform principal (no group,
no role); it is simply the fourth dev credential to replace.

One restart behaviour to know, since it is what lets the seed own that password: the
profile keeps `initDataNewOnly=false`, so every entity the seed **names** is re-applied on
each start. Edits to the four accounts, the three groups, or the `map-console` application
— its 24-hour token lifetime included — therefore revert; make those in
`casdoor-init-data.json` instead. Anything the seed does not name, such as a user you add
in the admin UI, is untouched and survives.

The platform never runs the browser flow itself. A client does authorization-code + PKCE
against `http://localhost:8000` as client `map-console-dev` (the seeded redirect URI is
`http://localhost:5173/callback`) and sends the resulting ID token to the control plane as
`Authorization: Bearer <jwt>`; the control plane verifies it and resolves the role.

**The proxy is the only way in, and that is an enforced control rather than advice.**
Casdoor serves its API and its UI from one port, so the browser has to reach it and
"keep it on the internal network" is not available as a control — which is why `casdoor`
publishes nothing and `idp` does. The proxy has two jobs. It answers 404 to Casdoor's SAML
and CAS surfaces (`/api/get-saml-login`, `/api/acs`, `/api/saml/metadata`,
`/api/saml/redirect/*`, `/cas/*`) — routes this stack uses none of, and where five of the
nine CERT/CC VU#780781 CVEs live; see
[docs/self-hosted-security.md](../../docs/self-hosted-security.md) §9. And it terminates
TLS on a second, unpublished listener (`https://idp:8443`) for the control plane, because
`internal/identity` requires an `https` key-set URL and its dial guard then refuses
loopback addresses outright — so a plain-HTTP IdP cannot be wired to this platform at all.
Caddy issues that certificate from a local CA generated on first boot into the `idpca`
volume, which the control plane mounts read-only and trusts through `SSL_CERT_FILE` — the
seventh variable you uncommented, and empty in the default stack on purpose, because that
variable REPLACES Go's default certificate-file list rather than adding to it; nothing
private is committed. The control plane waits
for the proxy's healthcheck, which fetches the key set through the whole chain — a
misconfigured IdP is a boot failure by design (the verifier makes one warming key fetch at
startup), so the wait is what keeps that from firing on a stack that is merely still
starting.

Casdoor keeps its users in a **second database inside the bundled Postgres** — `casdoor`,
created on first boot by the server's own `--createDatabase=true`, beside
`managed_agent_platform`. One container, two databases, one `pgdata` volume. So
`docker compose down -v` takes the IdP's user store with everything else: the three
accounts, any you added, group membership, and the built-in signing certificate Casdoor
mints when it initializes an empty database. The next `up` re-seeds the accounts and mints
a **different** signing key, so every token issued before the wipe stops verifying — worth
knowing before you drop volumes mid-debugging with a token open in a terminal.

## Teardown

```sh
docker compose down          # stop and remove containers
docker compose down -v       # also drop the volumes (wipes all data — Postgres,
                             # MinIO, and OpenBao together; ciphertext and its
                             # transit key live and die as a pair). With the `iam`
                             # profile that includes the IdP: its user store shares
                             # the Postgres volume, and its signing certificate is
                             # minted afresh on the next boot (see above)
```

**Every verb takes `-p`, not just `up`.** A bare `docker compose down -v` in a checkout
you last brought up as `-p mapsecond` addresses the *default* project, so it drops the
volumes of the stack you were running beside — the one you meant to keep. Tear a second
stack down the way you raised it: `docker compose -p mapsecond down -v`.

Two of a session's containers are not compose services at all: the sandbox and its egress
gate are host-daemon siblings the executor creates. `down` does not remove them, and a
live gate holds an endpoint on the network, so removing that network fails with "has
active endpoints". Nothing else will collect them either — the reaper deliberately skips
containers whose session id its database has never seen, which is what stops one stack
reaping another's. Look before you sweep, because the ownership label carries no project
component and so matches every stack's on this daemon:

```sh
docker ps --filter label=dev.opensdlc.managed-agent-platform.session-id \
          --format '{{.Names}}'   # map-<session> / map-gate-<session>
```
