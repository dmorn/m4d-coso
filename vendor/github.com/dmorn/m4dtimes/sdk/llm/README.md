# sdk/llm

LLM client with tool-use protocol support. Designed for extensibility — starts with Anthropic, ready for more providers.

## Origin

Port of [pi-ai](https://github.com/badlogic/pi-mono/tree/main/packages/ai) (`@mariozechner/pi-ai`) patterns to Go.

**Reference files in pi-mono:**
- `packages/ai/src/types.ts` — canonical message/tool types (our Message IR)
- `packages/ai/src/stream.ts` — streaming event model
- `packages/ai/src/api-registry.ts` — provider plugin system
- `packages/ai/src/providers/transform-messages.ts` — conversation sanitization
- `packages/ai/src/utils/validation.ts` — tool argument validation (AJV)
- `packages/ai/src/utils/overflow.ts` — context window overflow detection

## Architecture

```
sdk/llm/
├── types.go          # Message IR: User, Assistant, ToolResult, ToolCall, ToolDef
├── client.go         # Provider-agnostic Client interface
├── anthropic.go      # Anthropic Messages API adapter
├── retry.go          # Exponential backoff on 429/5xx
└── validation.go     # Tool argument validation against JSON Schema
```

### Message IR (provider-agnostic)

The core abstraction. All providers convert to/from these types:

```go
// Messages
type Message struct {
    Role    string         // "user", "assistant", "tool_result"
    Content []ContentBlock
}

type ContentBlock struct {
    Type      string          // "text", "tool_use", "tool_result"
    Text      string          // for type "text"
    ToolCall  *ToolCall       // for type "tool_use"
    ToolResult *ToolResult    // for type "tool_result"
}

// Tool definitions
type ToolDef struct {
    Name        string          // identifier the LLM uses
    Description string          // what the LLM sees
    Parameters  json.RawMessage // JSON Schema
}

// Tool calls (LLM → us)
type ToolCall struct {
    ID        string
    Name      string
    Arguments json.RawMessage
}

// Tool results (us → LLM)
type ToolResult struct {
    ToolCallID string
    Content    string
    IsError    bool
}

// Response from LLM
type Response struct {
    Type       string       // "text" or "tool_use"
    Text       string       // if Type == "text"
    ToolCalls  []ToolCall   // if Type == "tool_use"
    Usage      Usage        // tokens in/out
    StopReason string       // "end_turn", "tool_use", "error"
}

type Usage struct {
    InputTokens  int
    OutputTokens int
}
```

**Why a Message IR?** pi-ai's key insight: decouple the conversation model from any specific provider. Messages flow through the system in this canonical format. Only the provider adapter touches the wire format. This makes adding providers trivial — implement one adapter, everything else works.

### Provider interface

```go
type Provider interface {
    // Chat sends messages and returns a response.
    // The provider converts Message IR → wire format → API call → wire format → Message IR.
    Chat(ctx context.Context, req Request) (*Response, error)
}

type Request struct {
    System   string
    Messages []Message
    Tools    []ToolDef
    Options  Options
}

type Options struct {
    Model     string
    MaxTokens int
    // Future: Temperature, TopP, StopSequences, etc.
}
```

### Anthropic adapter

First (and currently only) provider. Maps Message IR to Anthropic Messages API format:

```
POST https://api.anthropic.com/v1/messages
{
  "model": "claude-sonnet-4-5-20250514",
  "max_tokens": 1024,
  "system": "You are...",
  "messages": [
    {"role": "user", "content": "..."},
    {"role": "assistant", "content": [{"type": "tool_use", ...}]},
    {"role": "user", "content": [{"type": "tool_result", ...}]}
  ],
  "tools": [{"name": "...", "description": "...", "input_schema": {...}}]
}
```

**Anthropic quirks handled:**
- `tool_result` must be sent as `role: "user"` with `type: "tool_result"` content blocks
- Tool call IDs are `toolu_` prefixed
- `stop_reason: "tool_use"` means the model wants to call tools

### Retry

Exponential backoff with jitter on:
- HTTP 429 (rate limit) — respect `Retry-After` header if present
- HTTP 5xx (server error)
- Network errors (connection reset, timeout)

Max retries configurable (default: 3). Follows pi-ai's pattern from `providers/openai-codex-responses.ts`.

### Tool argument validation

Before executing a tool, validate the LLM's arguments against the tool's JSON Schema:

```go
func ValidateToolArgs(tool ToolDef, args json.RawMessage) error
```

In pi-ai this uses AJV. In Go we use `github.com/santhosh-tekuri/jsonschema` or similar. If validation fails, return a structured error to the LLM (not a crash).

## What we took from pi-ai

| pi-ai feature | sdk/llm | Notes |
|--------------|---------|-------|
| Message IR (user/assistant/toolResult) | ✅ Ported | Core abstraction |
| Content blocks (text, tool_use, tool_result) | ✅ Ported | Same model |
| Provider interface | ✅ Ported | Simplified (no streaming) |
| Tool definitions (JSON Schema) | ✅ Ported | Raw JSON instead of TypeBox |
| Retry with backoff | ✅ Ported | Same pattern |
| Argument validation | ✅ Ported | Different library, same concept |
| Usage tracking (tokens) | ✅ Ported | For cost tracking |

## What we deferred (but can add later)

| pi-ai feature | Status | When to add |
|--------------|--------|-------------|
| Multi-provider (OpenAI, Google, etc.) | 🔮 Provider interface ready | When we need model flexibility |
| Streaming (SSE events) | 🔮 | When we need real-time UI (web dashboard) |
| Transform pipeline (sanitize history) | 🔮 | When we support provider switching mid-conversation |
| Context overflow detection | 🔮 | When conversations get long (add in sdk/agent) |
| Image content blocks | 🔮 | When agents need to process images |
| Thinking blocks | 🔮 | When using extended thinking models |

### Adding a new provider (future)

Implement the `Provider` interface:

```go
// sdk/llm/openai.go
type OpenAIProvider struct { ... }

func (p *OpenAIProvider) Chat(ctx context.Context, req Request) (*Response, error) {
    // 1. Convert Message IR → OpenAI format
    // 2. POST to api.openai.com/v1/chat/completions
    // 3. Convert OpenAI response → Message IR
    // Reference: pi-ai providers/openai-completions.ts
}
```

The Message IR means all sdk/agent code works unchanged. Only the wire format changes.

## Status

🔴 **Not started** — types and interfaces defined. Next: implement types.go + anthropic.go.
