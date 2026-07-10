package events

import (
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// Inbound validation for POST /v1/sessions/{id}/events. Only the wire's seven
// inbound types exist; each is validated field-by-field against the reference
// schema and normalized so every nullable wire field is stored explicitly
// (rendering is then a plain merge of payload + envelope). Content blocks are
// kept as the client's raw bytes after validation, so they round-trip
// byte-for-byte.
//
// v1 divergences (documented in STATE.md): user.define_outcome is rejected
// (outcome surface is deferred), session_thread_id must be null/absent
// (threads are deferred), and tool_use_id existence is not cross-checked.

// NormalizeInbound validates one send batch. envKind is the session's
// environment kind ("cloud" | "self_hosted"), which gates user.tool_result.
func NormalizeInbound(envKind string, raws []json.RawMessage) ([]NewEvent, error) {
	if len(raws) == 0 {
		return nil, fmt.Errorf("events must contain at least one event")
	}
	out := make([]NewEvent, 0, len(raws))
	var prev domain.EventType
	for i, raw := range raws {
		ev, err := normalizeOne(envKind, raw)
		if err != nil {
			return nil, fmt.Errorf("events[%d]: %w", i, err)
		}
		if ev.Type == domain.EventSystemMessage {
			if i != len(raws)-1 {
				return nil, fmt.Errorf("events[%d]: system.message must be the final event in the request", i)
			}
			switch prev {
			case domain.EventUserMessage, domain.EventUserToolResult, domain.EventUserCustomToolRes:
			default:
				return nil, fmt.Errorf("events[%d]: system.message must immediately follow a user.message, user.tool_result, or user.custom_tool_result in the same request", i)
			}
		}
		prev = ev.Type
		out = append(out, ev)
	}
	return out, nil
}

func normalizeOne(envKind string, raw json.RawMessage) (NewEvent, error) {
	obj, err := asObject(raw, "event")
	if err != nil {
		return NewEvent{}, err
	}
	typRaw, ok := obj["type"]
	if !ok {
		return NewEvent{}, fmt.Errorf("type is required")
	}
	var typ string
	if err := json.Unmarshal(typRaw, &typ); err != nil {
		return NewEvent{}, fmt.Errorf("type must be a string")
	}

	et := domain.EventType(typ)
	switch et {
	case domain.EventUserMessage:
		return normalizeUserMessage(obj)
	case domain.EventUserInterrupt:
		return normalizeUserInterrupt(obj)
	case domain.EventUserToolConfirm:
		return normalizeToolConfirmation(obj)
	case domain.EventUserCustomToolRes:
		return normalizeToolResult(obj, et, "custom_tool_use_id")
	case domain.EventUserToolResult:
		if envKind != "self_hosted" {
			return NewEvent{}, fmt.Errorf("user.tool_result is only valid on self_hosted environments")
		}
		return normalizeToolResult(obj, et, "tool_use_id")
	case domain.EventSystemMessage:
		return normalizeSystemMessage(obj)
	case domain.EventUserDefineOutcome:
		return NewEvent{}, fmt.Errorf("user.define_outcome is not supported in v1 (outcome evaluation is deferred)")
	case domain.EventStart, domain.EventDelta:
		return NewEvent{}, fmt.Errorf("%q is a stream-only preview frame and cannot be sent", typ)
	}
	if platformEmitted[et] {
		return NewEvent{}, fmt.Errorf("event type %q is emitted by the platform and cannot be sent by clients", typ)
	}
	return NewEvent{}, fmt.Errorf("unknown event type %q", typ)
}

// platformEmitted enumerates the outbound taxonomy, so a client posting one
// gets a clearer error than "unknown".
var platformEmitted = map[domain.EventType]bool{
	domain.EventAgentMessage: true, domain.EventAgentThinking: true,
	domain.EventAgentToolUse: true, domain.EventAgentToolResult: true,
	domain.EventAgentMCPToolUse: true, domain.EventAgentMCPToolResult: true,
	domain.EventAgentCustomToolUse:   true,
	domain.EventSessionStatusRunning: true, domain.EventSessionStatusIdle: true,
	domain.EventSessionStatusRescheduled: true, domain.EventSessionStatusTerminated: true,
	domain.EventSessionError: true, domain.EventSessionUpdated: true, domain.EventSessionDeleted: true,
	domain.EventSpanModelRequestStart: true, domain.EventSpanModelRequestEnd: true,
}

func normalizeUserMessage(obj map[string]json.RawMessage) (NewEvent, error) {
	if err := allowKeys(obj, "type", "content"); err != nil {
		return NewEvent{}, err
	}
	content, err := requireBlocks(obj, "content", blocksUserMessage)
	if err != nil {
		return NewEvent{}, err
	}
	return newEvent(domain.EventUserMessage, fields{"content": content})
}

func normalizeUserInterrupt(obj map[string]json.RawMessage) (NewEvent, error) {
	if err := allowKeys(obj, "type", "session_thread_id"); err != nil {
		return NewEvent{}, err
	}
	if err := requireNullThread(obj); err != nil {
		return NewEvent{}, err
	}
	return newEvent(domain.EventUserInterrupt, fields{"session_thread_id": nullRaw})
}

func normalizeToolConfirmation(obj map[string]json.RawMessage) (NewEvent, error) {
	if err := allowKeys(obj, "type", "result", "tool_use_id", "deny_message", "session_thread_id"); err != nil {
		return NewEvent{}, err
	}
	result, err := requireString(obj, "result")
	if err != nil {
		return NewEvent{}, err
	}
	if result != "allow" && result != "deny" {
		return NewEvent{}, fmt.Errorf(`result must be "allow" or "deny"`)
	}
	toolUseID, err := requireString(obj, "tool_use_id")
	if err != nil {
		return NewEvent{}, err
	}
	denyMessage := nullRaw
	if raw, set := obj["deny_message"]; set && !isNullRaw(raw) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return NewEvent{}, fmt.Errorf("deny_message must be a string")
		}
		if result != "deny" {
			return NewEvent{}, fmt.Errorf(`deny_message is only allowed when result is "deny"`)
		}
		denyMessage = raw
	}
	if err := requireNullThread(obj); err != nil {
		return NewEvent{}, err
	}
	return newEvent(domain.EventUserToolConfirm, fields{
		"result":            mustJSON(result),
		"tool_use_id":       mustJSON(toolUseID),
		"deny_message":      denyMessage,
		"session_thread_id": nullRaw,
	})
}

// normalizeToolResult covers user.tool_result and user.custom_tool_result,
// which share every field except the tool-use reference key.
func normalizeToolResult(obj map[string]json.RawMessage, typ domain.EventType, refKey string) (NewEvent, error) {
	if err := allowKeys(obj, "type", refKey, "content", "is_error", "session_thread_id"); err != nil {
		return NewEvent{}, err
	}
	ref, err := requireString(obj, refKey)
	if err != nil {
		return NewEvent{}, err
	}
	content := nullRaw
	if raw, set := obj["content"]; set && !isNullRaw(raw) {
		content, err = validateBlocks(raw, "content", blocksToolResult)
		if err != nil {
			return NewEvent{}, err
		}
	}
	isError := nullRaw
	if raw, set := obj["is_error"]; set && !isNullRaw(raw) {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return NewEvent{}, fmt.Errorf("is_error must be a boolean")
		}
		isError = raw
	}
	if err := requireNullThread(obj); err != nil {
		return NewEvent{}, err
	}
	return newEvent(typ, fields{
		refKey:              mustJSON(ref),
		"content":           content,
		"is_error":          isError,
		"session_thread_id": nullRaw,
	})
}

func normalizeSystemMessage(obj map[string]json.RawMessage) (NewEvent, error) {
	if err := allowKeys(obj, "type", "content"); err != nil {
		return NewEvent{}, err
	}
	content, err := requireBlocks(obj, "content", blocksTextOnly)
	if err != nil {
		return NewEvent{}, err
	}
	return newEvent(domain.EventSystemMessage, fields{"content": content})
}

// --- shared plumbing ---

type fields map[string]json.RawMessage

var nullRaw = json.RawMessage("null")

func newEvent(typ domain.EventType, f fields) (NewEvent, error) {
	payload, err := json.Marshal(f)
	if err != nil {
		return NewEvent{}, err
	}
	return NewEvent{Type: typ, Payload: payload}, nil
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // strings and bools cannot fail to marshal
	}
	return raw
}

func asObject(raw json.RawMessage, what string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("%s must be a JSON object", what)
	}
	return obj, nil
}

func allowKeys(obj map[string]json.RawMessage, allowed ...string) error {
	for key := range obj {
		found := false
		for _, a := range allowed {
			if key == a {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}

func requireString(obj map[string]json.RawMessage, key string) (string, error) {
	raw, ok := obj[key]
	if !ok || isNullRaw(raw) {
		return "", fmt.Errorf("%s is required", key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	if s == "" {
		return "", fmt.Errorf("%s must not be empty", key)
	}
	return s, nil
}

func isNullRaw(raw json.RawMessage) bool {
	return string(raw) == "null"
}

// requireNullThread rejects a non-null session_thread_id: multi-agent threads
// are a reserved seam in v1 (documented divergence).
func requireNullThread(obj map[string]json.RawMessage) error {
	if raw, set := obj["session_thread_id"]; set && !isNullRaw(raw) {
		return fmt.Errorf("session_thread_id is not supported in v1 (multi-agent threads are deferred)")
	}
	return nil
}

// Content-block vocabularies per carrier event.
var (
	blocksUserMessage = map[string]bool{"text": true, "image": true, "document": true}
	blocksToolResult  = map[string]bool{"text": true, "image": true, "document": true, "search_result": true}
	blocksTextOnly    = map[string]bool{"text": true}
)

func requireBlocks(obj map[string]json.RawMessage, key string, allowed map[string]bool) (json.RawMessage, error) {
	raw, ok := obj[key]
	if !ok || isNullRaw(raw) {
		return nil, fmt.Errorf("%s is required", key)
	}
	return validateBlocks(raw, key, allowed)
}

// validateBlocks checks each content block against the wire schema and
// reassembles the array from the original raw block bytes.
func validateBlocks(raw json.RawMessage, what string, allowed map[string]bool) (json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s must be an array of content blocks", what)
	}
	for i, item := range items {
		if err := validateBlock(item, allowed); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", what, i, err)
		}
	}
	out, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func validateBlock(raw json.RawMessage, allowed map[string]bool) error {
	obj, err := asObject(raw, "content block")
	if err != nil {
		return err
	}
	typ, err := requireString(obj, "type")
	if err != nil {
		return err
	}
	if !allowed[typ] {
		return fmt.Errorf("content block type %q is not allowed here", typ)
	}
	switch typ {
	case "text":
		if err := allowKeys(obj, "type", "text"); err != nil {
			return err
		}
		var s string
		if raw, ok := obj["text"]; !ok || json.Unmarshal(raw, &s) != nil {
			return fmt.Errorf("text block requires a string text field")
		}
		return nil
	case "image":
		if err := allowKeys(obj, "type", "source"); err != nil {
			return err
		}
		return validateSource(obj["source"], map[string]bool{"base64": true, "url": true, "file": true})
	case "document":
		if err := allowKeys(obj, "type", "source", "context", "title"); err != nil {
			return err
		}
		for _, k := range []string{"context", "title"} {
			if raw, set := obj[k]; set && !isNullRaw(raw) {
				var s string
				if json.Unmarshal(raw, &s) != nil {
					return fmt.Errorf("%s must be a string", k)
				}
			}
		}
		return validateSource(obj["source"], map[string]bool{"base64": true, "text": true, "url": true, "file": true})
	case "search_result":
		if err := allowKeys(obj, "type", "source", "title", "citations", "content"); err != nil {
			return err
		}
		// A search result's source is its URL — a plain string.
		if _, err := requireString(obj, "source"); err != nil {
			return err
		}
		if _, err := requireString(obj, "title"); err != nil {
			return err
		}
		if raw, set := obj["citations"]; set && !isNullRaw(raw) {
			cit, err := asObject(raw, "citations")
			if err != nil {
				return err
			}
			if err := allowKeys(cit, "enabled"); err != nil {
				return err
			}
		}
		content, ok := obj["content"]
		if !ok || isNullRaw(content) {
			return fmt.Errorf("search_result requires content")
		}
		_, err := validateBlocks(content, "content", blocksTextOnly)
		return err
	}
	return nil
}

// validateSource checks an image/document source union member.
func validateSource(raw json.RawMessage, kinds map[string]bool) error {
	if raw == nil || isNullRaw(raw) {
		return fmt.Errorf("source is required")
	}
	obj, err := asObject(raw, "source")
	if err != nil {
		return err
	}
	typ, err := requireString(obj, "type")
	if err != nil {
		return err
	}
	if !kinds[typ] {
		return fmt.Errorf("source type %q is not allowed here", typ)
	}
	switch typ {
	case "base64", "text":
		if err := allowKeys(obj, "type", "data", "media_type"); err != nil {
			return err
		}
		if _, err := requireString(obj, "data"); err != nil {
			return err
		}
		_, err := requireString(obj, "media_type")
		return err
	case "url":
		if err := allowKeys(obj, "type", "url"); err != nil {
			return err
		}
		_, err := requireString(obj, "url")
		return err
	case "file":
		if err := allowKeys(obj, "type", "file_id"); err != nil {
			return err
		}
		_, err := requireString(obj, "file_id")
		return err
	}
	return nil
}
