---
name: verify
description: Local end-to-end verification skill for this repo. Use before committing or declaring behavior complete to run fast checks, targeted contract tests, and a repeatable live runtime smoke against real binaries, Postgres, and Docker/Compose, then verify zero leaked processes or containers.
---

# Local verification skill (`verify`)

Run the change against the real system rather than relying on unit tests alone. Record every command and its actual output as evidence.

## 0. Record pre-existing machine state

Before starting any service or container, snapshot what already exists so teardown never touches a parallel session's resources:

```bash
docker ps -a --format '{{.ID}} {{.Names}} {{.Image}} {{.Status}}'
```

## 1. Fast inner-loop gate (container-free)

Catch compilation, cross-platform, vet, formatting, and unit regressions before spending container or CI time:

```bash
make build crossbuild vet fmt-check test-fast
```

When touching workflows, docs, or `.claude/` config, also run the script guards:

```bash
make identifiers-test pins-test pipes-test
```

## 2. Targeted contract suites

Run the real-infrastructure suites for the packages touched by the diff (`-count=1` disables cached passes):

```bash
go test -count=1 ./internal/<changed-pkg>...
```

If a change touches `changelog.d/`, `docs/DIVERGENCES.md`, or SDK citations, also run:

```bash
go test -count=1 ./tools/changelog ./tools/registrycheck ./tools/sdkref
```

## 3. Live end-to-end runtime smoke

Pick the runtime path matching the changed surface (run at least one whenever runtime behavior changes):

### Path A — Deterministic whole-platform SDK rehearsal (API + brain + queue + executor + Docker sandbox)

Runs the full platform in-process over real Postgres and a real Docker sandbox driven through the official `anthropic-sdk-go` client (no model spend):

```bash
go test -count=1 -v -run TestRehearsalDefineOutcomes ./acceptance/...
```

### Path B — Multi-container Compose stack (real binaries + Postgres + MinIO + modelgateway)

Use when changing `cmd/`, `deploy/compose/`, auth/login, skills/files, or gateway wiring. Set `COMPOSE_PROJECT_NAME=verify-smoke` so both `./compose` (which injects `PLATFORM_BUILD_VERSION` via `tools/build-version.sh`) and `smoke_login.py`'s internal `docker compose` calls stay isolated from a developer's default stack, and pass an unused path inside a temp directory to `--key-file` (`smoke_login.py` opens it with `O_CREAT|O_EXCL`):

```bash
(
  cd deploy/compose
  [ -f .env ] || cp .env.example .env
  export COMPOSE_PROJECT_NAME=verify-smoke
  TMP_DIR="$(mktemp -d)"
  trap 'rm -rf "$TMP_DIR"; ./compose down -v' EXIT
  ./compose up -d --build postgres minio controlplane brain modelgateway
  python3 -I smoke_login.py --key-file "$TMP_DIR/api-key"
  KEY="$(cat "$TMP_DIR/api-key")"
  curl -sf -H "x-api-key: $KEY" http://localhost:8080/v1/agents | grep -q '"data"'
  curl -sf -H "x-api-key: $KEY" http://localhost:8080/v1/environments | grep -q '"data"'
)
```

### Path C — Wire compatibility check with real `ant` CLI

Use when changing public `/v1/*` wire shapes, headers, SSE streams, or worker endpoints. Build `ant` from the read-only local checkout (see `docs/REFERENCE_PROJECTS.md`):

```bash
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT
(cd ../../anthropic-cli && go build -o "$SCRATCH/ant" ./cmd/ant)
ANTHROPIC_API_KEY="$KEY" "$SCRATCH/ant" --base-url http://localhost:8080 beta:agents list
ANTHROPIC_API_KEY="$KEY" "$SCRATCH/ant" --base-url http://localhost:8080 beta:environments list
```

## 4. Full merge gate before PR

Before opening or updating a PR, run the full gate (`make verify`) or verify the exact CI run covers every required job (`ci`, `coverage`, `helm`, `terraform`, `compose`, `native-console`).

## 5. Escalation cap (three rounds)

If the same failing check still fails after **three consecutive diagnose-and-fix rounds**, stop. Do not retry blindly, add `time.Sleep`, skip tests, or weaken assertions. Report the failing command, exact error output, and the three hypotheses tested so a human engineer can step in.

## 6. Confirm clean teardown

Compare `docker ps -a` against the step 0 snapshot and remove only containers spawned by this verify run. Confirm no stray processes or fixture containers remain.
