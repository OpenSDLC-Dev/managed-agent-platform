// Package worker is the BYOC (bring-your-own-compute) consumer of the work
// queue: the customer-hosted twin of internal/executor. Where the executor runs
// inside the platform with direct database access, a worker runs in the
// customer's own network and reaches the control plane only over the wire —
// authenticating with its environment key, reading a session's suspended tool
// calls through the session events API, running the built-in toolset in a local
// sandbox, and posting the results back as user.tool_result events. Platform
// executor and BYOC worker are the same pull protocol at two deployment points;
// this is the self_hosted one.
//
// The whole loop lives here. lease.go polls the work queue, claims an item and
// heartbeats it (NewWorker and Run, which cmd/worker wires to configuration);
// toolexec.go is the driver that runs a session's outstanding tools once;
// files.go, skills.go and memory.go materialize a session's files, skills and
// memory stores into the sandbox before the tools that read them run, memory.go
// reconciling each store with the control plane at the run's boundary (plan 36
// slice 6, the wire twin of internal/executor/memory.go). client.go builds the
// SDK client, and only that client ever talks to the platform — a worker holds
// no database handle, which is the property that lets it run on compute the
// platform cannot reach.
//
// One item carries a second credential besides the environment key: a work item
// whose session attaches a memory store is handed out with a per-item sessions
// token (wtk_) in its secret, the only one of the worker's two credentials
// those routes admit (client.go decodes it; memory.go applies it per call over
// the environment-key client). Everything else the worker calls still rides the
// environment key.
//
// Two boundaries are worth knowing before changing anything here. The wire is
// the whole contract, and it is narrow: Poll serves this worker exactly one
// thing — a tool_exec item on its own self_hosted environment. Never a
// model_turn row, never web_exec, outputs_harvest or mcp_exec, and never
// another environment's work. Those kinds are claimed in-process by the brain
// and the platform executor, which hold the database handle this package
// deliberately does not (internal/queue's Kind constants record why each falls
// where it does). And a
// run that stops reporting progress for StallTimeout is cancelled, after which
// the worker deliberately stops beating rather than releasing the lease — the
// lease lapses server-side and the control plane re-offers the item, which is
// the one path that survives a worker wedged inside a call that ignores
// cancellation.
package worker
