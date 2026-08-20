package toolset

import (
	"encoding/json"
	"slices"
)

// The six delegation tools, by name. They are the platform's own: no agent
// declares them, the brain injects them by thread role, and the settlement
// transaction answers them itself rather than any driver (plan 35 decision 6).
//
// The reference documents what the six do — spawn a subagent, address one,
// list them, wait, report a result, message the coordinator — and neither their
// descriptions nor their input schemas. Both are ours (INFERRED,
// docs/DIVERGENCES.md), which is why they are pinned here and tested: the model
// is told exactly this, and a description that drifts is a behaviour change.
const (
	ToolCreateAgent   = "create_agent"
	ToolSendToAgent   = "send_to_agent"
	ToolListAgents    = "list_agents"
	ToolWaitForAgents = "wait_for_agents"
	ToolSubmitResult  = "submit_result"
	ToolSendToParent  = "send_to_parent"
)

// coordinatorDefinitions are the four a coordinator's primary thread is
// offered. Each description states the consequence the model cannot see from
// the schema — that a spawned agent starts at once, that reports arrive as
// messages rather than as this call's return value, and that a wait is not a
// licence to conclude.
var coordinatorDefinitions = []toolDef{
	{
		name: ToolCreateAgent,
		description: "Spawn one of the agents on your roster as a new thread. It begins work immediately " +
			"and reports back to you as a message; this call returns only the new thread's session_thread_id.",
		props: map[string]any{
			"agent_name": prop("string", "Name of a callable agent from your roster."),
			"message": prop("string", "The task for that agent. State it in full: the agent sees this "+
				"message and nothing else of your conversation."),
		},
		required: []string{"agent_name", "message"},
	},
	{
		name: ToolSendToAgent,
		description: "Send a message to one of this session's agent threads. Address it by " +
			"session_thread_id, or by agent_name when exactly one live thread runs that agent.",
		props: map[string]any{
			"session_thread_id": prop("string", "Public sthr_ ID of the thread to message."),
			"agent_name":        prop("string", "Name of the agent to message, when its thread is unambiguous."),
			"message":           prop("string", "The message to send."),
		},
		required: []string{"message"},
	},
	{
		name: ToolListAgents,
		description: "List this session's agent threads: each thread's session_thread_id, the agent it " +
			"runs, and its current status.",
		props: map[string]any{},
	},
	{
		name: ToolWaitForAgents,
		description: "Wait for your agents to report. It returns immediately — their reports reach you as " +
			"messages, not as this call's result — so do not conclude the task before they have reported.",
		props: map[string]any{},
	},
}

// workerDefinitions are the two any child thread is offered — "worker" in the
// sense of the agent doing the work, not the BYOC worker process. A child gets
// no coordinator tool, which is what keeps the topology one level deep.
var workerDefinitions = []toolDef{
	{
		name: ToolSubmitResult,
		description: "Report your result to the coordinator and end this turn. Call it once your other " +
			"tool calls have returned — a turn that reports and calls tools at the same time reports nothing.",
		props: map[string]any{
			"result": prop("string", "What you accomplished, stated for a coordinator that cannot see your work."),
		},
		required: []string{"result"},
	},
	{
		name:        ToolSendToParent,
		description: "Send a message to your coordinator without ending this turn — progress, a question, a partial finding.",
		props: map[string]any{
			"message": prop("string", "The message to send."),
		},
		required: []string{"message"},
	},
}

// The rendered definitions, composed once at load. Two turns of one thread must
// assemble the same request prefix, and a definition rebuilt per call is a
// definition that can move: these are static, so the marshal is done where its
// failure would be a defect in this file rather than a session's bad luck.
var (
	coordinatorTools = renderDefs(coordinatorDefinitions)
	workerTools      = renderDefs(workerDefinitions)
)

func renderDefs(defs []toolDef) []json.RawMessage {
	out := make([]json.RawMessage, len(defs))
	for i, d := range defs {
		raw, err := d.marshal()
		if err != nil {
			panic(err)
		}
		out[i] = raw
	}
	return out
}

// AllDelegationTools names the six, for a caller that must reason about the
// half it was not offered as well as the half it was.
func AllDelegationTools() []string {
	return []string{
		ToolCreateAgent, ToolSendToAgent, ToolListAgents, ToolWaitForAgents,
		ToolSubmitResult, ToolSendToParent,
	}
}

// CoordinatorTools returns the four delegation tools the primary thread of a
// session with a roster is offered, and WorkerTools the two any child is. Both
// hand back a copy: the bytes are shared and stable, the slice is the caller's
// to build a request out of.
func CoordinatorTools() []json.RawMessage { return slices.Clone(coordinatorTools) }

// WorkerTools returns the two delegation tools of a child thread — see
// CoordinatorTools.
func WorkerTools() []json.RawMessage { return slices.Clone(workerTools) }

// IsCoordinatorTool reports whether name is one of the four a coordinator's
// primary thread is offered, and IsWorkerTool one of the two a child is. They
// exist so the settlement can tell a call made by the wrong role apart from one
// made by the right one — the brain offers each thread only its own half, so a
// name from the other half is the model reaching for a tool it was never given.
func IsCoordinatorTool(name string) bool {
	switch name {
	case ToolCreateAgent, ToolSendToAgent, ToolListAgents, ToolWaitForAgents:
		return true
	}
	return false
}

// IsWorkerTool reports whether name is one of the two — see IsCoordinatorTool.
func IsWorkerTool(name string) bool {
	return name == ToolSubmitResult || name == ToolSendToParent
}

// IsDelegationTool reports whether name is one of the six. It is the predicate
// the API's tool-result validation and both workers' scans consult, so that no
// client can forge a child's report and no driver ever tries to run a call only
// the settlement can answer — the same one-predicate discipline IsWebTool keeps
// for the split it names.
func IsDelegationTool(name string) bool {
	switch name {
	case ToolCreateAgent, ToolSendToAgent, ToolListAgents, ToolWaitForAgents,
		ToolSubmitResult, ToolSendToParent:
		return true
	}
	return false
}
