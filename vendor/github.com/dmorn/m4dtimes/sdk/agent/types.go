package agent

import (
	"context"
	"encoding/json"
	"github.com/dmorn/m4dtimes/sdk/llm"
)

// ContextInjector allows tools to append messages into any user's conversation history.
// The Agent implements this interface and passes itself via ToolContext.
// Use it to inject outbound messages (e.g. DMs to other users) into their context
// so the recipient's next LLM turn has awareness of what was sent.
type ContextInjector interface {
	Inject(userID int64, msg llm.Message)
}

// ToolContext is passed to every tool handler.
// Extra carries domain-specific data (e.g. DB handle, user role) — set by the concrete agent.
// ContextInjector is always set by the agent; use it to inject messages into other users' contexts.
// EventBus, when set, allows tools to publish events directly into the agent loop.
type ToolContext struct {
	UserID          int64
	ChatID          int64
	Timestamp       int64
	Extra           any             // domain-specific: set via BuildExtra
	ContextInjector ContextInjector // injects messages into any user's conversation history
	EventBus        EventBus        // optional: publish events from within a tool
}

// ToolHandler is the signature for all tool implementations.
type ToolHandler func(ctx ToolContext, args json.RawMessage) (string, error)

// Tool is an optional interface for implementing tools as objects rather than bare functions.
// Use it when a group of related tool handlers share state (DB handle, config, etc.).
// Register with ToolRegistry.RegisterTool — one call per struct, any number of tools inside.
//
// Example:
//
//	type ScheduleTools struct{ db *store.DB }
//	func (s *ScheduleTools) Def() llm.ToolDef { ... }
//	func (s *ScheduleTools) Execute(ctx ToolContext, args json.RawMessage) (string, error) { ... }
//
// For multiple tools in one struct, implement ToolSet instead.
type Tool interface {
	Def() llm.ToolDef
	Execute(ctx ToolContext, args json.RawMessage) (string, error)
}

// ToolSet is a collection of tools that share state.
// Use when a domain (e.g. scheduling, hours, reports) has multiple tool handlers
// that all operate on the same underlying data.
//
// Example:
//
//	type HotelTools struct{ db *store.DB }
//	func (h *HotelTools) Tools() []Tool { return []Tool{&QuerySchedule{h.db}, &UpdateSchedule{h.db}} }
type ToolSet interface {
	Tools() []Tool
}

// Update is a generic inbound message from any messaging platform.
type Update struct {
	UpdateID int64
	ChatID   int64
	UserID   int64
	Text     string
}

// Messenger is the messaging platform abstraction.
// sdk/telegram will implement this; tests can mock it.
type Messenger interface {
	Poll(ctx context.Context, offset int64, timeoutSec int) ([]Update, error)
	Send(ctx context.Context, chatID int64, text string) error
}

// TypingNotifier is an optional extension of Messenger.
// If the Messenger also implements this interface, the agent will call SendTyping
// before every LLM invocation so the user sees a "typing…" indicator.
// Telegram shows the indicator for ~5 seconds; the agent refreshes it periodically
// during long LLM calls via a background goroutine.
type TypingNotifier interface {
	SendTyping(ctx context.Context, chatID int64) error
}

// BuildExtra is called once per inbound message to produce the ToolContext.Extra value.
// Agents register this at startup to inject domain context (DB connection, role lookup, etc.)
type BuildExtra func(userID int64, chatID int64) (any, error)

// BuildTools is an optional callback called once per inbound message to produce the tool
// definitions sent to the LLM. Use it to filter tools based on user role or other per-request
// context. If nil, the agent uses Registry.Definitions() (all registered tools).
type BuildTools func(userID int64, chatID int64) []llm.ToolDef

// BuildPrompt is an optional callback called once per inbound message to produce the system
// prompt sent to the LLM. Use it to inject per-user context (e.g. role-specific tool summaries)
// into the prompt. If nil, the agent uses the static Options.Prompt string.
type BuildPrompt func(userID int64, chatID int64) string
