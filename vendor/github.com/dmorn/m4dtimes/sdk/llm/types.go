package llm

import "encoding/json"

type Message struct {
	Role    string         `json:"role"` // "user", "assistant", "tool_result"
	Content []ContentBlock `json:"content"`
	Usage   *Usage         `json:"usage,omitempty"` // set on assistant messages; mirrors Pi/OpenClaw format
}

type ContentBlock struct {
	Type       string      `json:"type"` // "text", "tool_use", "tool_result"
	Text       string      `json:"text,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
}

type Response struct {
	Type       string     `json:"type"` // "text" or "tool_use"
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Usage      Usage      `json:"usage"`
	StopReason string     `json:"stop_reason"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
