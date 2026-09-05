// Package executor is the hands' consumer: it pulls tool_exec work from the
// queue, runs the built-in toolset inside the session's sandbox, and appends
// the agent.tool_result events the brain resumes on. Platform-managed cloud
// and customer BYOC are the same pull protocol at two deployment points; this
// is the platform-managed one, embedding the Docker sandbox provider.
//
// tool_exec is the loop this package is named for but not the whole of it. The
// same process claims three more kinds, each with its own driver file, and two
// of them run in the executor's own process with no sandbox at all: web_exec
// (webwork.go — web_fetch and web_search, for BOTH environment kinds, since the
// reference's worker implements only the six sandbox tools) and mcp_exec
// (mcpwork.go for the discovery that fills mcp_catalogs, mcpexec.go for the
// call itself, mcpspill.go for an answer too large to inline, mcpcred.go for
// the credential — likewise both kinds, since only this platform's driver
// answers an MCP call). The fourth, outputs_harvest (harvest.go), is a cloud
// session's alone: it snapshots the deliverables out of the sandbox — to open
// an outcome-grading cycle, or simply because the session went idle
// (docs/plan/38) — and a self_hosted sandbox has no file lane to read.
// What goes INTO a sandbox lives beside them — skills.go, files.go, repos.go
// and repoclone.go (go-git, so no git binary is a runtime dependency).
//
// This package also owns the sandbox's lifecycle, which no work item triggers.
// reaper.go is the single owner of sandbox destruction on four tiers (a session
// deleted, archived or terminated, plus an idle tier past a configured TTL),
// and checkpoint.go captures a session's durable state to object storage before
// the idle tier destroys it, restoring it into a fresh sandbox on the next
// provision. Both are here because this is the only process holding the sandbox
// provider, and both are per-endpoint rather than coordinated: an executor sees
// only its own daemon or namespace, and reaping is idempotent.
//
// The loop mirrors the brain's: Claim the oldest tool_exec item (reclaiming an
// expired lease), do the work, hand the item back. The brain, when a turn stops
// for a built-in tool, commits the agent.tool_use intents and enqueues one
// tool_exec item; this executor answers every unanswered agent.tool_use for the
// session, then — once the set is complete — enqueues the model_turn that wakes
// the brain to continue. The result append, the resume enqueue, and the item's
// completion are one transaction under the session row lock, so a concurrent
// trigger never sees a gap.
//
// At-most-once is the queue's lease, not a marker in the sandbox (which is
// agent-writable and disposable — see internal/sandbox/shell). A lease keeper
// holds the claim while tools run so two executors never run one session's
// tools at once; a crash mid-run lets the lease lapse, and the reclaiming
// executor re-runs only the still-unanswered tools — a committed result is
// never re-run, so a tool's result is exactly-once even though a non-idempotent
// command can run more than once across a crash. That residue is inherent to a
// disposable sandbox and is documented, not solved here.
package executor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/jina"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/tavily"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the executor's OTel instrumentation scope.
const tracerName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/executor"

// Config tunes the loop. Image is the sandbox base image (a deployment choice —
// the wire's environment config has no image field). LeaseTTL must comfortably
// exceed toolset.MaxTimeout: the lease keeper renews at TTL/3 while a tool runs,
// but the TTL is also the window a crashed executor's work waits before another
// reclaims it.
type Config struct {
	Image        string
	Workdir      string
	LeaseTTL     time.Duration
	PollInterval time.Duration
	// StallTimeout bounds how long a claimed item may make no progress before
	// the lease keeper gives up on it — cancelling the work and leaving the
	// lease to lapse, so another executor reclaims it (#383). Progress is a
	// step finishing, not a byte moving, so it must clear the longest single
	// step a healthy run takes: toolset.MaxTimeout for one `bash`, a 500 MB
	// mount, a wait on the session lock behind another goroutine's checkpoint
	// capture, a cold image pull, or a CheckpointMaxBytes restore — provisioning
	// reports between those, so they are separate intervals rather than one sum.
	// The check rides the keeper's renewal tick, so detection lands somewhere in
	// [stall, stall+LeaseTTL/3]. Set it under the longest step and the item does
	// not merely retry: every reclaim re-runs that step and cancels at the same
	// point, so the session waits on a loop nothing breaks — which is why the
	// binary floors the configured value above toolset.MaxTimeout (a tool at its
	// cap is killed and answers *after* it) and a deployment whose object store
	// is far away should raise it well past that. It bounds
	// *silence*, never duration — an item that keeps finishing steps runs as
	// long as it likes. Not an off switch: a wedged sandbox call is what left
	// an executor stuck with no recovery at all, so 0 takes the default
	// (EXECUTOR_STALL_TIMEOUT).
	StallTimeout time.Duration
	// ReapInterval paces the sandbox reaper (reaper.go): one sweep of this
	// endpoint's owned sessions per interval (EXECUTOR_REAP_INTERVAL; 0 takes
	// the 60s default). Teardown latency is bounded by it, and nothing else
	// destroys sandboxes, so there is no off switch — a deployment that wants
	// slower reaping sets it longer.
	ReapInterval time.Duration
	// CheckpointMaxBytes budgets a workspace checkpoint (checkpoint.go): ONE
	// measure on both sides — the framed, uncompressed tar stream, metered as
	// capture writes it and again as restore decompresses it — so a capture
	// that fits is arithmetically guaranteed to restore
	// (EXECUTOR_CHECKPOINT_MAX_BYTES; 0 takes the 2 GiB default). Over
	// budget, the TTL tier reaps without a checkpoint — an agent must not
	// pin its sandbox immortal by filling the disk (plan 24 D8).
	CheckpointMaxBytes int64
	// SandboxIdleTTL arms the reaper's idle tier (plan 24 slice 5): an idle
	// cloud session whose last activity is older than this is checkpointed and
	// its sandbox reaped — unless it still owes work (a queued/starting/active
	// work item) or an unanswered confirmation ask (HITL-idle is mid-turn).
	// Zero disables the tier — deliberately, so a hand-built test Config never
	// reaps by surprise; cmd/executor resolves the unset env to the 24h
	// default (EXECUTOR_SANDBOX_IDLE_TTL; 0 there disables too). A blob-less
	// executor disables the tier at startup regardless: reaping without the
	// means to checkpoint would silently discard workspaces.
	SandboxIdleTTL time.Duration
	// Hardening is the containment every session's sandbox is created with —
	// cgroup limits, capability drops, optionally a uid and a read-only root
	// (#65). The zero value hardens nothing, which is what a test that builds a
	// Config by hand wants; cmd/executor resolves the platform's defaults from
	// the environment (sandbox.HardeningFromEnv), so every deployment gets them.
	Hardening sandbox.Hardening
	// ControlplaneURL and GateImage opt the deployment into the per-session egress
	// gate. A session wants a gate when its networking is `limited` or it has
	// vaults attached; its gate container (GateImage) fetches that session's egress
	// config from ControlplaneURL. When both are set, a gate-wanting session gets a
	// gate; when either is empty, no gate is requested and a gate-wanting session
	// keeps the backend's own fail-closed networking (Docker `limited` → no egress,
	// K8s → its init-container isolation, vault-attached → inert placeholders) — the
	// pre-gate behavior, so an un-opted-in deployment is unchanged rather than
	// faulted. An unrestricted, vault-less session never wanted a gate and networks
	// directly regardless. See gateSpec.
	ControlplaneURL string
	GateImage       string
	// OTelEndpoint and OTelInsecure are the deployment's OTLP collector config,
	// threaded into each session's gate container so its egress_request spans
	// export to the same collector as the executor (the gate is a separate process
	// that does not inherit this executor's environment). Empty OTelEndpoint =
	// no collector; the gate runs without an exporter.
	OTelEndpoint string
	OTelInsecure bool
	// The web tools' backends (docs/plan/15_web-tools.md). An unconfigured
	// tool answers with an is_error result naming what is missing, so the
	// misconfiguration surfaces to an operator instead of masking the tool.
	// web_search needs TavilyAPIKey; web_fetch needs JinaAPIKey OR an explicit
	// WebFetchBaseURL (the Reader protocol works keyless — a keyless proxy, or
	// the public free tier named deliberately — but with neither set, a bare
	// install must not silently egress model-chosen URLs to a public third
	// party). The base URLs point at Tavily-protocol / Jina-Reader-protocol
	// endpoints; empty resolves to the adapters' public defaults.
	TavilyAPIKey     string
	JinaAPIKey       string
	WebSearchBaseURL string
	WebFetchBaseURL  string
	// WebAllowedDomains, when non-empty, is the operator-side allowlist for
	// the web tools (#225): web_fetch may reach only these hosts, and a
	// search hit whose source is outside them is dropped. Entries use the
	// same grammar as the wire's allowed_hosts (a bare hostname, an IPv4
	// literal, or a "*."-prefixed wildcard that never matches the apex —
	// egress.HostSet, the one matcher). Empty means unrestricted: the
	// reference has per-tool allowed domains, but no wire field carries
	// them, so this knob is platform-native (docs/DIVERGENCES.md).
	WebAllowedDomains []string
	// The clone budgets for github_repository mounts (plan 25 decision 1).
	// The spool a clone lands in sits on executor-local disk, outside the
	// sandbox's own storage hardening, so one unbounded repository could
	// exhaust the executor and disrupt unrelated sessions. RepoCloneMaxBytes
	// is metered as bytes land — over the tree and its tar together — and
	// RepoCloneTimeout bounds one repository's clone; both surface as
	// tolerated clone failures (too_large / timeout), never as a failed run.
	RepoCloneTimeout  time.Duration
	RepoCloneMaxBytes int64
	// PackageInstallTimeout is the Exec deadline for ONE manager's install of
	// the environment's config.packages (packages.go): six managers is six
	// budgets, not one shared between them, because each is a separate silent
	// interval the stall bound must clear. It defaults to toolset.MaxTimeout —
	// the longest step the default stall budget already clears — so raising it
	// far enough that one manager's budget becomes the longest step past the
	// stall budget (default 30m) refuses startup until EXECUTOR_STALL_TIMEOUT
	// rises with it (EXECUTOR_PACKAGE_INSTALL_TIMEOUT; plan 40 decision 5). Past
	// it the install is killed and surfaces as a session.error with reason
	// `timeout`, never as a failed run.
	PackageInstallTimeout time.Duration
	// MCPPassTimeout bounds one mcp_exec pass — the discovery half across all of
	// the session's MCP servers, and the execution half across all of a turn's
	// outstanding calls — for the reason the clone budgets exist: both walk
	// third-party endpoints serially, so an unbounded pass would hold this
	// process's single work goroutine and disrupt unrelated sessions. Neither
	// half fails the run when it runs out: a server discovery does not reach is
	// a tolerated failed row, and a call execution does not make stays
	// outstanding and keeps the item, which comes back to finish it.
	MCPPassTimeout time.Duration
}

// DefaultStallTimeout is the budget an operator who sets none gets, shared with
// the BYOC worker rather than written out twice (toolset.DefaultStallBudget).
//
// Exported because cmd/executor must check the budget an operator will actually
// get. A floor that only guards the value they typed is no floor at all: raising
// EXECUTOR_REPO_CLONE_TIMEOUT past this default rebuilds the reclaim loop while
// EXECUTOR_STALL_TIMEOUT is left unset (#383).
const DefaultStallTimeout = toolset.DefaultStallBudget

// stallFault folds the error a cut-short run came back with into the stall
// sentinel, for every lane that settles one.
//
// Both are needed and neither substitutes for the other: queue.ErrWorkStalled is
// what an operator (and a test) matches on, while the run's error is the only
// place the tool_use the stall cut short is written down. Choosing between them
// — which is what the obvious cmp.Or does — silently drops the sentinel in the
// ordinary case, since a stall usually cancels a call in flight and so usually
// has a cause (#383).
func stallFault(kerr, cause error) error {
	if cause == nil {
		return kerr
	}
	return fmt.Errorf("%w (run: %w)", kerr, cause)
}

func (c Config) withDefaults() Config {
	if c.Image == "" {
		c.Image = "debian:stable-slim"
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 15 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.StallTimeout <= 0 {
		c.StallTimeout = DefaultStallTimeout
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = time.Minute
	}
	if c.CheckpointMaxBytes <= 0 {
		c.CheckpointMaxBytes = 2 << 30
	}
	if c.RepoCloneTimeout <= 0 {
		c.RepoCloneTimeout = 5 * time.Minute
	}
	if c.RepoCloneMaxBytes <= 0 {
		c.RepoCloneMaxBytes = 1 << 30
	}
	if c.PackageInstallTimeout <= 0 {
		c.PackageInstallTimeout = toolset.MaxTimeout
	}
	if c.MCPPassTimeout <= 0 {
		c.MCPPassTimeout = 5 * time.Minute
	}
	return c
}

// Executor consumes tool_exec work over one Postgres pool and one sandbox
// provider.
type Executor struct {
	pool     *pgxpool.Pool
	log      *events.Log
	queue    *queue.Queue
	provider sandbox.Provider
	// blobs sources skill archives for materialization; nil (a storage-less
	// deploy) skips materialization with a log line, never a fault.
	blobs blob.Store
	// cipher opens the sealed authorization token of a github_repository
	// resource so the executor can clone it (plan 25 decision 2). Nil means
	// no cipher is configured — a deployment the control plane already
	// refuses repo-bearing creates on, so a repo reaching here surfaces as a
	// tolerated clone failure rather than a silent skip.
	cipher secrets.Cipher
	cfg    Config
	// searcher and fetcher are the web tools' backends (webwork.go). Nil means
	// unconfigured — the tool answers with an is_error naming what is missing.
	// The fetcher needs either a key or an explicit base URL: the Jina Reader
	// protocol works keyless, but constructing it with neither would have an
	// entirely unconfigured self-hosted install silently egress model-chosen
	// URLs to the public r.jina.ai — the wrong default for an on-prem product.
	searcher webtool.Searcher
	fetcher  webtool.Fetcher
	// webAllowed, when non-nil, restricts the web tools' hosts (webwork.go).
	// Nil means no allowlist is configured — unrestricted, today's default.
	webAllowed *egress.HostSet
	// mcpHTTP replaces the HTTP client the MCP driver's connections use
	// (mcpwork.go). Nil — production — selects mcp.DefaultClient and the
	// dial-address guard it carries; a test sets its own so a fixture on
	// loopback, which that guard exists to refuse, is reachable.
	mcpHTTP *http.Client
	// onFault, when set, receives every per-item fault. Left nil in production
	// (the queue's reclaim is the recovery); tests set it to observe faults.
	onFault func(*queue.Item, error)
	// kindOffset rotates step's claim order across the kinds — see step.
	// Touched only by Run's single goroutine.
	kindOffset int
}

func New(pool *pgxpool.Pool, log *events.Log, q *queue.Queue, provider sandbox.Provider, blobs blob.Store, cipher secrets.Cipher, cfg Config) *Executor {
	e := &Executor{pool: pool, log: log, queue: q, provider: provider, blobs: blobs, cipher: cipher, cfg: cfg.withDefaults()}
	if cfg.TavilyAPIKey != "" {
		e.searcher = tavily.New(cfg.WebSearchBaseURL, cfg.TavilyAPIKey)
	}
	if cfg.JinaAPIKey != "" || cfg.WebFetchBaseURL != "" {
		e.fetcher = jina.New(cfg.WebFetchBaseURL, cfg.JinaAPIKey)
	}
	if len(cfg.WebAllowedDomains) > 0 {
		e.webAllowed = egress.NewHostSet(cfg.WebAllowedDomains)
	}
	return e
}

// Run polls until the context is cancelled. It claims one tool_exec item at a
// time; an error processing one item is logged by returning it up to the caller
// only for a fatal claim failure — a per-item fault is swallowed so the loop
// keeps serving other sessions, and the faulted item is reclaimed after its
// lease lapses.
func (e *Executor) Run(ctx context.Context) error {
	// The reaper rides Run's lifetime: the derived cancel stops it when Run
	// returns for any reason, not only a cancelled caller, and Run waits for
	// it — a caller that closes the pool after Run must not race a reap pass
	// still in flight. The work loop keeps checking the caller's own ctx —
	// wrapping it would put a second Err between Run and the shutdown-race
	// semantics #282 pinned.
	if e.cfg.SandboxIdleTTL > 0 && e.blobs == nil {
		slog.Warn("idle-TTL sandbox reaping disabled: no object store configured to hold checkpoints")
	}
	reapCtx, cancel := context.WithCancel(ctx)
	reapDone := make(chan struct{})
	go func() { defer close(reapDone); e.reapLoop(reapCtx) }()
	defer func() { cancel(); <-reapDone }()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		worked, err := e.step(ctx)
		if err != nil {
			// Checked against ctx, not errors.Is(err, context.Canceled): a claim
			// racing shutdown can surface a transport-level error from the dying
			// connection instead of the context error (#282). Mirrors worker.Run.
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !worked {
			if err := sleep(ctx, e.cfg.PollInterval); err != nil {
				return nil
			}
		}
	}
}

// step claims and processes at most one item, reporting whether it found work.
// The kinds are tried in rotating order across steps: a fixed order would be
// strict cross-kind priority — under sustained load of one kind, another
// session's queued work of a different kind could wait behind it indefinitely
// — while rotation bounds the wait at one item of each other kind. Within a
// kind the queue is FIFO by age. A per-item fault (sandbox gone, database
// hiccup, lost lease) is not fatal to the loop: the item keeps its lease until
// it lapses, then another claim reclaims and retries it. Only a claim failure
// is returned up.
func (e *Executor) step(ctx context.Context) (bool, error) {
	// Faults are reported by the processors themselves, from inside their
	// spans — see report. kindOffset is loop-local state: Run is one goroutine.
	kinds := [4]queue.Kind{queue.WebExec, queue.ToolExec, queue.OutputsHarvest, queue.MCPExec}
	start := e.kindOffset
	e.kindOffset = (e.kindOffset + 1) % len(kinds)
	for i := range kinds {
		kind := kinds[(start+i)%len(kinds)]
		item, err := e.queue.Claim(ctx, kind, e.cfg.LeaseTTL)
		if err != nil {
			return false, err
		}
		if item == nil {
			continue
		}
		switch kind {
		case queue.WebExec:
			_ = e.processWeb(ctx, item)
		case queue.OutputsHarvest:
			_ = e.processHarvest(ctx, item)
		case queue.MCPExec:
			_ = e.processMCP(ctx, item)
		default:
			_ = e.process(ctx, item)
		}
		return true, nil
	}
	return false, nil
}

// process runs one tool_exec item to completion.
func (e *Executor) process(ctx context.Context, item *queue.Item) (err error) {
	// The span opens on a claimed item and closes when the item is done with,
	// which is what a consumer span stands for: the handling of one message,
	// end to end. Both edges matter. Everything below can fail — the session
	// lookup, the tools, the commit — and every one of those leaves the item for
	// reclaim to retry next lease period, so a span that covered only the middle
	// would omit exactly the recurring faults an operator opens the trace to
	// find. The tools' own timing is toolset's duration metric, not this span's
	// business.
	ctx, span := consumerSpan(ctx, item, "tool_exec")
	// Every failure this function reports is the platform's own — a tool the
	// model can read and recover from (a missing file, a nonzero exit) never
	// reaches here: it rides the log verbatim and the toolset metric's
	// error.type, and erroring the span for it would light up every trace view
	// on ordinary agent behaviour.
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			// Inside the span, so the record lands on the tool_exec the fault is
			// about — that red span is where an operator asks for it.
			e.report(ctx, item, err)
		}
		span.End()
	}()

	// Drain work for a session that is no longer live before doing anything
	// expensive. Archiving freezes the status and makes every append the run
	// would commit fail, so without this the item reclaim-loops forever,
	// re-running its tools each lease period; this mirrors the brain's
	// claimLiveSession. Loading the egress policy under the same lock keeps it
	// to one round trip.
	sess, live, err := e.sessionForRun(ctx, item)
	if err != nil || !live {
		return err
	}

	// Keep the lease alive from before provisioning through the tool run: an
	// image pull can be slow, and a fixed TTL would otherwise let the lease
	// lapse mid-provision and a second executor reclaim and double-run the
	// session's tools. Provisioning and every tool run happen under kctx, so
	// losing the lease cancels the work.
	kctx, keeper := e.queue.KeepLease(ctx, item, e.cfg.LeaseTTL, e.cfg.StallTimeout)

	results, ms, faultErr, runErr := e.provisionAndRun(kctx, item, sess, keeper.Progress)
	// Whatever the paths below decide, the sync's span ends once.
	defer ms.end(ctx)
	if kerr := keeper.Close(); kerr != nil {
		if !errors.Is(kerr, queue.ErrWorkStalled) {
			// The lease is gone — another executor may already own this item.
			// Nothing of ours may commit; the results we ran are re-derived on
			// the reclaiming pass (a committed result is never re-run).
			return fmt.Errorf("lease keeper: %w", kerr)
		}
		// A stall is the opposite case, and must not be treated as a lost lease:
		// this claimant gave the lease up, so it still holds it here, and the
		// tools that already answered really did run — in a sandbox, with their
		// side effects already spent. Discarding them would leave the reclaim to
		// run each of them a second time. So they commit down the fault path
		// below (which asserts the lease, leaves the item live, and enqueues no
		// turn while uses stay unanswered), and only the wedged tool and the
		// ones behind it are re-derived (#383).
		//
		// Best-effort, and deliberately so: the keeper has stopped renewing, so
		// a settlement slower than the lease's remainder finds the item
		// reclaimed and rolls back to exactly the pre-#383 outcome. And a call
		// that ignores cancellation never returns here at all — the run that
		// commits nothing is the one still wedged (#396).
		//
		// Whichever error the run came back with rides along rather than being
		// dropped. Both are the cancellation itself in the ordinary case — but
		// the tool fault is the one that names the *tool_use that wedged*, which
		// is the first thing an operator wants and the only place it is written
		// down; the setup error is the diagnosis when nothing had started yet.
		faultErr = stallFault(kerr, cmp.Or(faultErr, runErr))
	} else if runErr != nil {
		return runErr
	}

	// Commit the results, the resume, and the item's fate together under the
	// session lock. The item is completed only when every tool ran: a backend
	// fault leaves it live so a reclaim retries the tools still unanswered
	// (the ones that did run are now committed and are skipped). Every state
	// write this claimant makes must prove it still owns the item — the
	// complete path through Complete/Requeue, the fault path by asserting the
	// lease explicitly, otherwise a claim lost while blocked on the session
	// lock could still commit a result a reclaiming executor also writes,
	// duplicating it on the append-only log. What follows — the per-thread
	// wake, the chain to the web or MCP driver for a call this sandbox pass
	// cannot answer, the re-scan for a call a sibling committed under this
	// live item — is settleDrain's, shared with the other drivers.
	//
	// The memory sync's settle phase rides the same transaction (plan 36
	// decision 11): the store's rows and the run's results commit together,
	// and the apply phase writes the sandbox only from what committed.
	if err := e.commitResults(ctx, item.SessionID, results, func(ctx context.Context, tx pgx.Tx) error {
		if err := e.settleDrain(ctx, tx, item, queue.ToolExec, faultErr != nil); err != nil {
			return err
		}
		return e.settleMemory(ctx, tx, ms)
	}); err != nil {
		// The diagnosis the branch above built rides along rather than being
		// replaced. A settlement that fails on the stall path is the worst case
		// this change has — the results are lost AND the item is left to the
		// lease — and reporting only "append tool results: ..." would drop both
		// the stall sentinel an operator matches on and the wedged tool's name,
		// which is written down nowhere else (#383).
		if faultErr != nil {
			return fmt.Errorf("append tool results: %w (run: %w)", err, faultErr)
		}
		return fmt.Errorf("append tool results: %w", err)
	}
	e.applyMemory(ctx, ms)
	return faultErr
}

// consumerSpan opens the consumer span for one claimed item — name is the work
// kind — parented on the enqueuing turn's captured trace context, so a
// session's model turns and the tools they trigger are one trace, the same
// guarantee the BYOC worker gets from the poll response. See process for why
// both edges of the item's handling live inside the span.
func consumerSpan(ctx context.Context, item *queue.Item, name string) (context.Context, trace.Span) {
	return otel.GetTracerProvider().Tracer(tracerName).Start(
		telemetry.Extract(ctx, item.TraceContext), name,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("session.id", item.SessionID.String()),
			attribute.String("work.id", item.ID.String()),
		))
}

// provisionAndRun provisions the session's sandbox and runs its unanswered
// tools under ctx (the lease-kept context). It returns the result events to
// append, the first backend fault a tool hit (nil if every tool ran), and a
// setup error from provisioning or reading the log — which stops the item with
// nothing committed, distinct from a tool fault, which commits what did run.
func (e *Executor) provisionAndRun(ctx context.Context, item *queue.Item, sess sessionRun, progress func()) ([]events.NewEvent, *memorySync, error, error) {
	sb, err := e.provisionSandbox(ctx, item.SessionID, sess, progress)
	if err != nil {
		return nil, nil, nil, err
	}
	// Each step this run finishes tells the keeper the run is alive (#383).
	// Reported at the boundaries a wedged sandbox call stops the run from
	// crossing: provisioning returned, a pass ended, a tool answered — and,
	// inside the passes, per skill, per repository and per mount, because a
	// pass over many of them is legitimately long while each item is not.
	progress()
	e.materializeSkills(ctx, sb, item.SessionID, sess.skills, progress)
	progress()
	// Repositories before files, so a file mount may deliberately overlay into
	// a checkout (plan 25 decision 5).
	e.materializeRepos(ctx, sb, item.SessionID, sess.repos, progress)
	progress()
	e.materializeFiles(ctx, sb, item.SessionID, sess.files, progress)
	progress()
	// Memory stores last: their mounts are reserved against the others
	// (/mnt/memory), so nothing here can overlay them. A mount the sandbox
	// already held is reconciled before the tools read it — the store's
	// changes since the last run land now, and what a faulted run wrote
	// goes up — so a store's change reaches a session at its next run.
	if existing := e.materializeMemory(ctx, sb, item.SessionID, sess.memories, progress); existing > 0 {
		if err := e.syncMemoryNow(ctx, sb, item.SessionID, sess.memories, progress); err != nil {
			slog.WarnContext(ctx, "memory stores not refreshed before the run; the run's end retries",
				"session_id", item.SessionID, "err", err)
		}
	}
	progress()
	uses, err := e.runnableToolUses(ctx, item.SessionID)
	if err != nil {
		return nil, nil, nil, err
	}
	// The web tools are the web driver's (webwork.go), never this sandbox
	// pass's — and normally never even seen here: a tool_exec is enqueued only
	// after every web call is answered (the web-first hold-back). A delegation
	// call is no driver's at all: the settlement that emits one answers it in
	// the same commit, so this pass can only ever meet a stray. Both filters
	// keep such a stray out of the Runner's unknown-tool arm, whose answer
	// would commit as an agent.tool_result telling a coordinator its spawn
	// failed — the same one rule the BYOC worker's scan spells (worker/toolexec.go).
	uses = slices.DeleteFunc(uses, func(u toolUse) bool {
		return toolset.IsWebTool(u.name) || toolset.IsDelegationTool(u.name)
	})
	results, faultErr := e.runTools(ctx, sb, item.SessionID, uses, sess.memories, progress)
	// The memory sync's read phase (plan 36 decision 11) closes every run
	// whose tools all answered — a run that faulted or stalled has a sandbox
	// nothing here should wait on, and the next run, or the reaper, syncs it.
	var ms *memorySync
	if faultErr == nil {
		ms = e.readMemory(ctx, sb, item.SessionID, sess.memories, progress)
	}
	return results, ms, faultErr, nil
}

// provisionSandbox resolves the session's credential env and gate spec and
// provisions (or adopts) its sandbox — the front half of provisionAndRun,
// shared with the outputs harvest, which reads the sandbox but runs no tools.
func (e *Executor) provisionSandbox(ctx context.Context, sessionID domain.ID, sess sessionRun, progress func()) (sandbox.Sandbox, error) {
	// This is the run's longest stretch of not-a-tool-call, and the stall bound
	// measures silence — so its steps report one at a time rather than as one
	// interval the budget must clear whole (#383). progress is required, like
	// every other reporting function in this package: a caller that does not
	// watch the run passes a no-op rather than nil.
	env, err := e.sandboxEnv(ctx, sessionID, sess.vaultIDs)
	if err != nil {
		return nil, fmt.Errorf("resolve vault credentials: %w", err)
	}
	progress()
	// The session advisory lock serializes the one pair that must not
	// interleave: this provision against the reaper's checkpoint+destroy
	// (plan 24 D4). Blocking, unlike the reaper's try-lock — work proceeds
	// the moment a reap finishes, on a fresh sandbox. Slice 4's restore
	// slots inside this same hold.
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire session lock connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, sessionLockKey(sessionID)); err != nil {
		return nil, fmt.Errorf("acquire session lock: %w", err)
	}
	defer unlockSession(conn, sessionID)
	// The lock is held by the reaper's checkpoint capture of this same session,
	// so the wait above is bounded by a capture, not by anything this run does.
	progress()
	// Restore fires on the D6 marker, never on "the container is fresh": a
	// ready marker means a checkpointed reap happened and nothing consumed it
	// yet. Reap-before-provision is the half-restore replacement rule in one
	// move — whatever container exists alongside a ready marker is either a
	// half-restored orphan (a crash between extract and consume) or absent,
	// and reaping first makes the following Provision the clean-create path
	// either way. A crash-recreate without a marker restores nothing: the
	// event log, not reap-time state, is that path's truth.
	marker, blobKey, err := checkpointMarker(ctx, conn, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint marker: %w", err)
	}
	// A ready marker fails closed without an object store: capture can only
	// have written it through one, so blob-less here is a misconfiguration —
	// and provisioning fresh anyway would let the standing marker rewind the
	// session's NEW work the moment a blob-configured executor next sees it.
	if marker == "ready" && e.blobs == nil {
		return nil, errors.New("session has a ready checkpoint but this executor has no object store configured")
	}
	restore := marker == "ready"
	if restore {
		if err := e.provider.Reap(ctx, sessionID); err != nil {
			return nil, fmt.Errorf("replace pre-restore sandbox: %w", err)
		}
		progress()
	}
	sb, err := e.provider.Provision(ctx, sandbox.Spec{
		SessionID:  sessionID,
		Image:      e.cfg.Image,
		Workdir:    e.cfg.Workdir,
		Networking: sess.networking,
		Env:        env,
		Hardening:  e.cfg.Hardening,
		Gate:       e.gateSpec(sess),
		// Set unconditionally, unlike Gate: the revoker is for the provision
		// that finds a gate it must dismantle, and that provision's own spec
		// is the ungated one.
		GateTokenRevoker: gateTokenMinter{pool: e.pool},
	})
	if err != nil {
		return nil, fmt.Errorf("provision sandbox: %w", err)
	}
	// The image pull is behind us; the restore below is the one step left that
	// a large workspace can spend real time in, and it gets the budget alone.
	progress()
	if restore {
		// Failure surfaces rather than consuming the marker: the tool run
		// errors, the lease reclaims it, and the next provision replaces the
		// half-restored tree and retries — plan 24 D6's crash rule.
		if err := e.restoreCheckpoint(ctx, conn, sessionID, blobKey, sb, progress); err != nil {
			return nil, fmt.Errorf("restore checkpoint: %w", err)
		}
	}
	// The environment's packages, inside this same hold and after the restore
	// (plan 40 decision 2): the lock is what keeps a reclaiming executor
	// waiting on the lapsed holder's install rather than racing its apt-get for
	// the dpkg lock, with no sandbox-side lock an arbitrary image would have to
	// supply. Only a backend fault from the sandbox comes back here — a
	// manager that cannot install is a session.error the client reads, and the
	// session runs on (decision 4).
	if err := e.installPackages(ctx, sb, sessionID, sess.packages, progress); err != nil {
		return nil, fmt.Errorf("install packages: %w", err)
	}
	return sb, nil
}

// sandboxEnv resolves the session's attached vaults into the environment
// variables the sandbox is provisioned with: one secret_name=placeholder entry
// per active environment_variable credential (vaultresolve). The placeholders
// are opaque and inert on their own — the per-session gate substitutes the real
// secrets at egress time, on admitted plain-HTTP requests. No attached vaults,
// or none carrying env-var credentials, yields a nil map: an ordinary sandbox.
//
// Placeholders are derived per (session, secret_name), so a re-provision of the
// same session resolves the identical tokens — matching what the create-bound
// Spec.Env already holds (Provision adopts a running sandbox without re-applying
// a changed Env) rather than drifting to fresh values the gate could no longer
// substitute.
//
// A credential whose secret_name cannot be safely injected is skipped rather
// than delivered — the "a bad credential surfaces [later] and does not block the
// session" arm of the resolution model — for two reasons: a name that is not a
// valid environment-variable name would fail ValidateEnv and fault the whole
// provision (a reclaim-loop), and a name the platform reserves (PATH and the
// loader/shell hooks) would break the sandbox or subvert how it launches
// processes if a credential could set it. Only a resolution I/O error faults the
// item, which then retries on reclaim like any other transient failure.
func (e *Executor) sandboxEnv(ctx context.Context, sessionID domain.ID, vaultIDs []string) (map[string]string, error) {
	bindings, err := vaultresolve.Bindings(ctx, e.pool, sessionID.String(), vaultIDs)
	if err != nil {
		return nil, err
	}
	var env map[string]string
	for _, b := range bindings {
		if !sandbox.ValidEnvName(b.SecretName) || sandbox.ReservedEnvName(b.SecretName) {
			continue
		}
		if env == nil {
			env = make(map[string]string, len(bindings))
		}
		env[b.SecretName] = b.Placeholder
	}
	return env, nil
}

// gateSpec decides whether to ask the provider for a per-session egress gate. A
// session wants one when its egress is `limited` (only its allowed_hosts may
// leave) or it has vaults attached (their credentials are injected at the gate,
// never handed to the sandbox). It returns a GateSpec only when a gate is both
// wanted and configured (GateImage + ControlplaneURL). When the executor has no
// gate configured — the K8s deployments, and any Docker deployment that has not
// opted in — it returns nil and the backend applies its own fail-closed
// networking instead: the Docker provider gives a `limited` sandbox no egress at
// all (NetworkMode "none"), the K8s provider its init-container isolation, and a
// vault-attached sandbox keeps the inert placeholders that egress literally with
// no gate to substitute them. That is the pre-gate behavior, so an un-opted-in
// deployment is unchanged rather than forced into a provision that never
// completes. The backend that runs the gate wires the sandbox to it and injects
// the proxy variables (a deployment detail), so gateSpec stays backend-agnostic
// and never touches the env.
func (e *Executor) gateSpec(sess sessionRun) *sandbox.GateSpec {
	wantsGate := sess.networking.Type == domain.NetLimited || len(sess.vaultIDs) > 0
	if !wantsGate || e.cfg.GateImage == "" || e.cfg.ControlplaneURL == "" {
		return nil
	}
	return &sandbox.GateSpec{
		Image:           e.cfg.GateImage,
		ControlplaneURL: e.cfg.ControlplaneURL,
		TokenMinter:     gateTokenMinter{pool: e.pool},
		OTelEndpoint:    e.cfg.OTelEndpoint,
		OTelInsecure:    e.cfg.OTelInsecure,
	}
}

// gateTokenMinter implements sandbox.GateTokenMinter and sandbox.GateTokenRevoker
// over the executor's pool. Generate returns a fresh in-memory token; Persist
// records it as the session's live gate token, and the provider calls Persist
// only after it has won the create race for the gate container — so a
// re-provision that adopts a running gate never revokes the token that gate is
// authenticating with. Revoke is the no-successor teardown: the provider calls
// it when it dismantles a session's gate without replacing it (gated→ungated),
// the one path where Ensure's revoke-on-re-mint never runs.
type gateTokenMinter struct{ pool *pgxpool.Pool }

func (m gateTokenMinter) Generate() string { return gatetoken.Mint() }

func (m gateTokenMinter) Persist(ctx context.Context, sessionID domain.ID, token string) error {
	return gatetoken.Ensure(ctx, m.pool, sessionID.String(), token)
}

func (m gateTokenMinter) Revoke(ctx context.Context, sessionID domain.ID) error {
	return gatetoken.Revoke(ctx, m.pool, sessionID.String())
}

// GateTokenRevoker is the pool-backed sandbox.GateTokenRevoker — the same
// implementation the executor injects into every Spec — exported for the
// provider construction in cmd glue, where the sandbox backend is built with a
// revoker so Reap can revoke a session's gate token before removing its
// containers (plan 24).
func GateTokenRevoker(pool *pgxpool.Pool) sandbox.GateTokenRevoker {
	return gateTokenMinter{pool: pool}
}

// runTools runs each runnable tool use in order, returning the result events
// to append and the first backend fault encountered (nil if all ran). A tool
// that fails at the tool level (missing file, nonzero exit) still yields a
// result event — that is the model's to see; only a backend fault (sandbox
// gone, daemon unreachable) stops the set and leaves the rest unanswered.
//
// A call answered under this pass is cancelled (plan 35 decision 9): a
// thread-scoped interrupt answers its thread's calls itself and never stops
// the shared item, so each call is checked just before it starts — skipped
// if answered, a late result for it dropped — and watched by a goroutine of
// its own on the keeper's cadence while it runs (answeredWatch, which exists
// because the keeper itself cannot tell the driver), so an interrupted
// `sleep 3600` costs one beat, not toolset.MaxTimeout, and the sibling calls
// queued behind it are not held hostage.
func (e *Executor) runTools(ctx context.Context, sb sandbox.Sandbox, sid domain.ID, uses []toolUse, memories []memoryRef, progress func()) ([]events.NewEvent, error) {
	// Workdir must match the one the sandbox was provisioned with, so the file
	// tools resolve a relative path against the same directory bash runs in.
	// Empty resolves to sandbox.DefaultWorkdir on both sides.
	runner := toolset.Runner{Sandbox: sb, Session: sid, Workdir: e.cfg.Workdir}
	// The memory roots the file tools guard (plan 36 decision 12): every
	// mounted store, and read-only the ones attached read_only or archived
	// since — the store's state is read here, per run, because an archive
	// between two runs must refuse the next run's writes, not merely leave
	// them unsynced.
	mounts := memoryMounts(memories)
	archived, err := e.archivedStores(ctx, mounts)
	if err != nil {
		return nil, fmt.Errorf("memory stores: %w", err)
	}
	for _, m := range mounts {
		runner.MemoryRoots = append(runner.MemoryRoots, m.MountPath)
		if m.Access == "read_only" || archived[m.MemoryStoreID] {
			runner.ReadOnlyRoots = append(runner.ReadOnlyRoots, m.MountPath)
		}
	}
	var results []events.NewEvent
	for _, u := range uses {
		if answered, err := events.Answered(ctx, e.pool, sid, u.id); err != nil {
			return results, fmt.Errorf("tool %s (%s): answered check: %w", u.name, u.id, err)
		} else if answered {
			continue
		}
		cctx, stop := e.answeredWatch(ctx, sid, u.id, e.cfg.LeaseTTL/3)
		res, err := runner.Run(cctx, u.id, u.name, u.input)
		if stop() {
			// Answered under it: whatever it returned, or failed with, is a
			// late result for a call the log already closed.
			progress()
			continue
		}
		if err != nil {
			// Backend fault: stop here. The results gathered so far are still
			// appended so a retry does not re-run them; this tool and any after
			// it stay unanswered for the reclaim.
			return results, fmt.Errorf("tool %s (%s): %w", u.name, u.id, err)
		}
		ev, err := toolResultEvent(u.id, res)
		if err != nil {
			return results, err
		}
		ev.ThreadID, ev.CrossPosted = u.thread, u.crossPosted
		results = append(results, ev)
		// A tool answered: the run is moving, however long the next one takes.
		progress()
	}
	return results, nil
}

// toolResultEvent renders a Result as an agent.tool_result event body:
// tool_use_id + content blocks + is_error, matching the wire's
// BetaManagedAgentsAgentToolResultEvent and what replay reads back. A
// SearchResults set (a web_search answer) IS the content — search_result
// blocks (the web driver answers a no-hit search with a text block, so the
// array is never empty in practice). Otherwise empty output (a read of an
// empty file) becomes the reference runner's toolset.NoOutput text block
// (since v1.63.1), never a text block with an empty string — a Messages endpoint rejects an empty text block, and that
// request is what the brain replays every resume, wedging the session.
func toolResultEvent(useID domain.ID, res toolset.Result) (events.NewEvent, error) {
	// SanitizeText again at this boundary: the web driver's error text embeds
	// a backend's err.Error() (which can quote a server-controlled body) and
	// never passes Runner.dispatch, so this is the last stop before jsonb.
	var content any
	if res.SearchResults != nil {
		content = res.SearchResults
	} else {
		text := toolset.SanitizeText(res.Content)
		if text == "" {
			text = toolset.NoOutput
		}
		content = []map[string]any{{"type": "text", "text": text}}
	}
	payload, err := json.Marshal(map[string]any{
		"tool_use_id": useID.String(),
		"content":     content,
		"is_error":    res.IsError,
	})
	if err != nil {
		return events.NewEvent{}, err
	}
	return events.NewEvent{Type: domain.EventAgentToolResult, Payload: payload}, nil
}

// sessionRun is the per-run session state sessionForRun loads under the row
// lock: the egress policy, the snapshot's skills and file mounts, and the
// attached vault ids that drive credential resolution.
type sessionRun struct {
	// envConfig is the environment's whole config, not only its networking
	// block: a policy decision that must distinguish a cloud environment from a
	// self_hosted one needs the kind, which lives on the config (mcpwork.go).
	envConfig  domain.EnvironmentConfig
	networking domain.Networking
	// packages is the environment's config.packages, lifted out of envConfig so
	// the one lane that must NOT install can clear it (harvest.go). A
	// sessionRun built by hand — every test that does not care — installs
	// nothing.
	packages   map[string][]string
	skills     []skillRef
	files      []fileRef
	repos      []repoRef
	memories   []memoryRef
	vaultIDs   []string
	mcpServers []mcpServerRef
}

// sessionForRun loads the session's egress policy, the skill references its
// snapshot resolves to (the roster's union on a coordinator session, plan 35
// decision 11), its file and repository mount resources, and its attached vault ids under the
// session's row lock, and reports whether the session is still live for tool
// execution. A session that is not running, or has been archived, is stale: its
// tool_exec item is completed here and false is returned, so a dead session
// cannot reclaim-loop (every append the run would make is rejected). A session
// that no longer exists took its cascade-deleted work item with it, so there is
// nothing to drain. Mirrors the brain's claimLiveSession. Reading skills,
// resources, and vault ids here keeps the run to one session read — a second,
// later read would add a transient-failure point that faults the whole item.
func (e *Executor) sessionForRun(ctx context.Context, item *queue.Item) (sessionRun, bool, error) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return sessionRun{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var archivedAt *time.Time
	var configJSON, agentJSON, resourcesJSON []byte
	var vaultIDs []string
	err = tx.QueryRow(ctx,
		`SELECT s.status, s.archived_at, e.config, s.resolved_agent, s.resources, s.vault_ids
		   FROM sessions s JOIN environments e ON e.id = s.environment_id
		  WHERE s.id = $1 FOR UPDATE OF s`,
		item.SessionID.String()).Scan(&status, &archivedAt, &configJSON, &agentJSON, &resourcesJSON, &vaultIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionRun{}, false, nil
	}
	if err != nil {
		return sessionRun{}, false, err
	}

	// Running is the live state for every kind but one. An outputs_harvest is
	// the exception because a session's idle is now a reason to schedule one
	// (docs/plan/38, #263): that item is claimed precisely because the session
	// just stopped working, so requiring running would drain every one of them
	// unread. The carve-out is exactly idle — a terminated, archived or deleted
	// session still drains — and no other kind gains a state.
	live := status == string(domain.SessionRunning) ||
		(item.Kind == queue.OutputsHarvest && status == string(domain.SessionIdle))
	if !live || archivedAt != nil {
		if err := e.queue.Complete(ctx, tx, item); err != nil {
			return sessionRun{}, false, err
		}
		return sessionRun{}, false, tx.Commit(ctx)
	}

	// An idle-triggered outputs harvest attaches to a live sandbox by session id
	// alone (docs/plan/38 decision 8) and reads none of the environment config,
	// resolved agent, or resources decoded below. Decoding them would fault the
	// item on a session whose agent/resource JSON will not decode — and runTurn
	// already idled such a session for that same corruption, which is what
	// scheduled this harvest, so faulting here would reclaim-loop it forever
	// (#263 review), the same wedge the settleHarvest outcome-decode tolerance
	// closes. A grading harvest still decodes: it provisions a fresh sandbox and
	// needs the full run state.
	if item.Kind == queue.OutputsHarvest && !item.ChainGrading {
		return sessionRun{}, true, tx.Commit(ctx)
	}

	var cfg domain.EnvironmentConfig
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return sessionRun{}, false, err
	}
	var agent struct {
		Skills     []skillRef     `json:"skills"`
		MCPServers []mcpServerRef `json:"mcp_servers"`
		// The roster snapshot, absent on a single-agent session. Only its
		// members' skills are read here: MCP servers are per thread's agent
		// (plan 35 decision 14) and the brain dials them, while the sandbox is
		// shared by every thread, so what is materialized into it is the union
		// (decision 11).
		Multiagent struct {
			Agents []struct {
				Skills []skillRef `json:"skills"`
			} `json:"agents"`
		} `json:"multiagent"`
	}
	if err := json.Unmarshal(agentJSON, &agent); err != nil {
		return sessionRun{}, false, err
	}
	// Concatenated in roster order behind the coordinator's own, not
	// deduplicated here: the materializer already collapses repeats by skill
	// id with the first occurrence winning, and a skill's landing directory
	// carries no version — so two members pinning different versions of one
	// skill share a tree and the earlier reference is what lands.
	skillRefs := agent.Skills
	for _, m := range agent.Multiagent.Agents {
		skillRefs = append(skillRefs, m.Skills...)
	}
	var resources []fileRef
	if err := json.Unmarshal(resourcesJSON, &resources); err != nil {
		return sessionRun{}, false, err
	}
	// The same bytes decoded through the repository variant's shape — each
	// materializer filters the array by type, so one read serves both.
	var repos []repoRef
	if err := json.Unmarshal(resourcesJSON, &repos); err != nil {
		return sessionRun{}, false, err
	}
	var memories []memoryRef
	if err := json.Unmarshal(resourcesJSON, &memories); err != nil {
		return sessionRun{}, false, err
	}
	return sessionRun{
		envConfig:  cfg,
		networking: cfg.Networking,
		packages:   cfg.Packages,
		skills:     skillRefs,
		files:      resources,
		repos:      repos,
		memories:   memories,
		vaultIDs:   vaultIDs,
		mcpServers: agent.MCPServers,
	}, true, tx.Commit(ctx)
}

// report is where per-item faults surface. The queue's reclaim is the recovery
// — the item keeps its lease until it lapses, then another claim retries it —
// but the fault is logged so an operator debugging "the tools never run" (a
// Docker daemon down faults every item) sees it rather than a silent stall.
// onFault is nil in production; tests set it to observe faults.
//
// Called from process's deferred exit rather than from step, so ctx still
// carries the tool_exec span. Reporting from step would work and correlate to
// the right *trace*, but only to the enqueuing turn's span — leaving the red
// tool_exec span, the one an operator clicks, with no log under it.
func (e *Executor) report(ctx context.Context, item *queue.Item, err error) {
	// Neither the item's fate nor what committed is stated here, because both
	// depend on how the run failed AND on which lane it was. In the tool_exec
	// lane a backend fault or a stall commits the tools that answered and leaves
	// the lease to expire and be reclaimed; a lost lease there commits nothing,
	// the item being another executor's by then or cancelled outright by a
	// user.interrupt; the web and MCP call lanes commit what answered on a stall
	// for the same reason and nothing on a lost lease; and the harvest lane
	// discards on either, half a snapshot being no snapshot. One sentence here
	// would be wrong for most of those, and an operator reading the wrong one
	// goes looking for results that are on the log — or stops looking for
	// results that are not (#383).
	slog.ErrorContext(ctx, "executor: work item faulted",
		"kind", item.Kind, "item", item.ID, "session", item.SessionID, "error", err)
	if e.onFault != nil {
		e.onFault(item, err)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
