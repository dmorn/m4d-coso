# sdk/agent

Agent loop, tool registry, and turn orchestration. The brain that connects LLM, tools, and messaging.

## Origin

Port of [pi-agent](https://github.com/badlogic/pi-mono/tree/main/packages/agent) (`@mariozechner/pi-agent-core`) patterns to Go.

**Reference files in pi-mono:**
- `packages/agent/src/agent-loop.ts` — the turn cycle (core loop)
- `packages/agent/src/agent.ts` — Agent class, tool management, context
- `packages/agent/src/types.ts` — AgentTool, events, config
- `packages/coding-agent/src/core/agent-session.ts` — real-world retry/compaction
- `packages/coding-agent/src/core/system-prompt.ts` — prompt construction
- `packages/coding-agent/src/core/tools/*.ts` — concrete tool implementations

## Architecture

```
sdk/agent/
├── agent.go        # Agent struct, Run() main loop
├── registry.go     # Tool registration and execution
├── context.go      # Conversation history management + hooks
├── logger.go       # Structured event logging
└── config.go       # LoadConfig() from env vars
```

### The turn cycle

This is the heart. Ported from `agent-loop.ts`:

```
┌─ telegram.Poll() ──────────────────────────────────┐
│                                                      │
│  for each message:                                   │
│    1. Identify user + role (DB lookup)               │
│    2. Append user message to history                 │
│    3. Transform context (apply hooks)                │
│    4. llm.Chat(system, messages, tools)              │
│         │                                            │
│         ├─ Response.Type == "text"                   │
│         │    → send to Telegram, done                │
│         │                                            │
│         └─ Response.Type == "tool_use"               │
│              for each tool_call:                     │
│                validate args (JSON Schema)           │
│                execute handler                       │
│                  ├─ success → ToolResult             │
│                  └─ error → ToolResult(isError=true) │
│              append all ToolResults to history        │
│              → loop back to step 4                   │
│                                                      │
│    5. Persist telegram offset                        │
│    6. Log everything                                 │
└──────────────────────────────────────────────────────┘
```

**Key insight from pi-agent:** tool errors don't crash the agent. They become structured `ToolResult{IsError: true}` messages that the LLM receives and can reason about ("the schedule query failed because no date was provided, let me ask the user").

### Tool registry

```go
type ToolHandler func(ctx ToolContext, args json.RawMessage) (string, error)

type ToolContext struct {
    UserID    int64
    Role      string  // "manager" or "cleaner"
    HotelID   string
    Timestamp int64
    DB        *store.DB
}

type ToolRegistry struct { ... }

func (r *ToolRegistry) Register(name, description string, schema json.RawMessage, handler ToolHandler)
func (r *ToolRegistry) Execute(name string, args json.RawMessage, ctx ToolContext) (*llm.ToolResult, error)
func (r *ToolRegistry) Definitions() []llm.ToolDef  // for passing to LLM
```

**pi-agent pattern:** tools are registered at startup with `setTools([...])`. Each tool has a name, description, JSON Schema, and an execute function. The registry converts them to `llm.ToolDef` for the LLM and routes execution by name.

**Access control:** the `ToolContext` carries the user's role. Tool handlers check it:
```go
func handleUpdateSchedule(ctx ToolContext, args json.RawMessage) (string, error) {
    if ctx.Role != "manager" {
        return "", fmt.Errorf("only managers can update the schedule")
    }
    // ...
}
```

### Context management

Conversation history with hooks for transformation:

```go
type ContextManager struct {
    Messages    []llm.Message
    MaxMessages int  // simple truncation (keep last N)

    // Hooks (future extensibility)
    TransformContext func([]llm.Message) []llm.Message
    ConvertToLLM     func([]llm.Message) []llm.Message
}
```

**From pi-agent:** the core agent doesn't own context-window policy. It exposes hooks:
- `TransformContext` — called before each LLM call, can prune/compact/summarize
- `ConvertToLLM` — filters out app-internal messages before sending to provider

**MVP:** simple truncation (keep last 20 messages). Future: summarization-based compaction like `coding-agent` does.

### Logger

Structured logging for every significant action:

```go
type Logger struct { ... }

func (l *Logger) Inbound(userID int64, text string)
func (l *Logger) LLMCall(model string, tokensIn, tokensOut int, durationMs int64)
func (l *Logger) ToolExec(tool string, durationMs int64, success bool)
func (l *Logger) Outbound(chatID int64, text string)
func (l *Logger) Error(context string, err error)
```

Output: stdout (for orchestrator) + `events` table in SQLite (for analysis during demo phase).

### Config

```go
type Config struct {
    TelegramToken string  // env: TELEGRAM_BOT_TOKEN
    LLMKey        string  // env: LLM_API_KEY
    LLMModel      string  // env: LLM_MODEL (default: claude-sonnet-4-5-20250514)
    DBPath        string  // env: DB_PATH (default: /data/state.db)
    HotelName     string  // env: HOTEL_NAME
    Timezone      string  // env: TIMEZONE (default: Europe/Rome)
    LogLevel      string  // env: LOG_LEVEL (default: info)
    MaxTokens     int     // env: LLM_MAX_TOKENS (default: 1024)
    PollTimeout   int     // env: POLL_TIMEOUT (default: 30)
}

func LoadConfig() (*Config, error)  // reads from env vars
```

## What we took from pi-agent

| pi-agent feature | sdk/agent | Notes |
|-----------------|-----------|-------|
| Turn cycle (LLM → tools → continue) | ✅ Ported | Core loop identical |
| Tool registry + execute | ✅ Ported | Same pattern, Go types |
| Error → structured ToolResult | ✅ Ported | Key resilience pattern |
| Context hooks (transform/convert) | ✅ Ported | As function fields |
| System prompt (mutable) | ✅ Ported | Set at init, changeable |
| Event logging | ✅ Ported | Simplified (no event bus) |

## What we deferred (but can add later)

| pi-agent feature | Status | When to add |
|-----------------|--------|-------------|
| Streaming turns with live events | 🔮 | Web dashboard / real-time UI |
| Session tree (branching/forking) | 🔮 | Multi-turn exploration |
| Extension system (plugins) | 🔮 | When agents need runtime extensibility |
| Steering queue (inject mid-turn) | 🔮 | When we need external interrupts |
| Compaction (summarize old context) | 🔮 | When conversations exceed context window |
| Follow-up queue | 🔮 | When we need chained prompts |
| Multi-modal tool results (images) | 🔮 | When tools return visual data |

### Adding compaction (future)

When conversations get long, implement a `TransformContext` hook:

```go
agent.Context.TransformContext = func(msgs []llm.Message) []llm.Message {
    if len(msgs) > 50 {
        // Summarize first 40 messages into one, keep last 10
        summary := summarize(msgs[:40])
        return append([]llm.Message{summary}, msgs[40:]...)
    }
    return msgs
}
// Reference: packages/coding-agent/src/core/agent-session.ts (compaction logic)
```

### Adding streaming (future)

Change `llm.Provider.Chat()` to return a channel of events:

```go
type StreamEvent struct {
    Type string  // "text_delta", "toolcall_delta", "done", "error"
    Data string
}
// Reference: packages/ai/src/stream.ts (event model)
```

## Status

🔴 **Not started** — architecture and interfaces defined. Next: implement after sdk/llm.
