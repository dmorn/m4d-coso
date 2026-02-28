# sdk

Go framework for building m4dtimes agents. Targets native `linux/arm64` today;
designed for `wasip1/wasm` once the host bridge is ready (see [Persistence](#persistence)).

## Packages

```
sdk/
├── llm/        Anthropic Messages API client with tool-use loop
├── agent/      Agent turn cycle + per-user context + tool registry
└── telegram/   Telegram Bot API (polling + send)
```

---

## sdk/llm

Low-level Anthropic client. Handles the tool-use protocol, retries, and token accounting.

### Core types

```go
// ToolDef describes a tool the LLM can call.
type ToolDef struct {
    Name        string
    Description string
    Parameters  json.RawMessage  // JSON Schema for the arguments object
}

// ToolCall is what the LLM returns when it wants to call a tool.
type ToolCall struct {
    ID        string
    Name      string
    Arguments json.RawMessage
}

// ToolResult is what we return after executing a tool.
type ToolResult struct {
    ToolCallID string
    Content    string
    IsError    bool
}

// Response is either a text reply or a batch of tool calls — never both.
type Response struct {
    Type      string      // "text" or "tool_use"
    Text      string      // set when Type == "text"
    ToolCalls []ToolCall  // set when Type == "tool_use"
    Usage     Usage       // input/output token counts
}

// Request is what you send to Chat().
type Request struct {
    System   string
    Messages []Message
    Tools    []ToolDef
}
```

### Creating a client

```go
// Reads LLM_API_KEY from environment.
provider, err := llm.NewAnthropicProvider(nil)

client := llm.New(provider, llm.Options{
    Model: "claude-sonnet-4-5-20250514",
})
```

### Calling Chat

```go
resp, err := client.Chat(ctx, llm.Request{
    System:   "You are a hotel management assistant.",
    Messages: messages,
    Tools:    toolDefs,
})

switch resp.Type {
case "text":
    // Final answer — send to user
    fmt.Println(resp.Text)
case "tool_use":
    // Execute each tool, then call Chat again with results
    for _, call := range resp.ToolCalls {
        result := executeTool(call.Name, call.Arguments)
        messages = append(messages, toolResultMessage(call.ID, result))
    }
}
```

The agent loop (see `sdk/agent`) handles this cycle automatically.

---

## sdk/agent

Orchestrates the full turn cycle: poll → LLM → tools → respond. Manages per-user
conversation history and dispatches tool calls.

### Options

```go
type Options struct {
    LLM         *llm.Client   // required
    Messenger   Messenger     // required — platform abstraction (Telegram, etc.)
    Registry    *ToolRegistry // tool registry; created automatically if nil
    Prompt      string        // static system prompt; ignored when BuildPrompt is set

    // Per-message callbacks — called once for each inbound message.
    BuildExtra  BuildExtra  // inject domain context into ToolContext.Extra
    BuildTools  BuildTools  // filter/select tools based on user; defaults to all
    BuildPrompt BuildPrompt // role-specific system prompt; overrides Prompt

    Logger      *Logger
    PollTimeout int   // seconds (default: 30)
}
```

### Building an agent

```go
a := agent.New(agent.Options{
    LLM:       llm.New(provider, llm.Options{Model: "claude-sonnet-4-5-20250514"}),
    Messenger: telegram.New(botToken),
    Registry:  toolRegistry,
    Logger:    agent.NewLogger("info"),

    // Inject a per-user DB pool into every tool call.
    BuildExtra: func(userID, chatID int64) (any, error) {
        return registry.Pool(ctx, userID)
    },

    // Role-specific prompt per user.
    BuildPrompt: func(userID, chatID int64) string {
        return buildPrompt(userID)
    },
})

if err := a.Run(ctx); err != nil {
    log.Fatal(err)
}
```

### Implementing tools

**Single tool — implement `Tool` interface:**

```go
type executeSQLTool struct{}

func (t *executeSQLTool) Def() llm.ToolDef {
    return llm.ToolDef{
        Name:        "execute_sql",
        Description: "Execute a SQL query against the database.",
        Parameters: json.RawMessage(`{
            "type": "object",
            "properties": {
                "query": {"type": "string"}
            },
            "required": ["query"]
        }`),
    }
}

func (t *executeSQLTool) Execute(ctx agent.ToolContext, args json.RawMessage) (string, error) {
    db := ctx.Extra.(*pgxpool.Pool)  // injected via BuildExtra
    // ...
}
```

**Multiple tools sharing state — implement `ToolSet`:**

```go
type HotelTools struct{ db *pgxpool.Pool }

func (h *HotelTools) Tools() []agent.Tool {
    return []agent.Tool{
        &queryRoomsTool{h.db},
        &assignCleanerTool{h.db},
        &updateStatusTool{h.db},
    }
}

// Registration
registry := agent.NewToolRegistry()
registry.RegisterToolSet(newHotelTools())
```

**Function-based tools:**

```go
registry.Register("tool_name", "description", schemaJSON, func(ctx agent.ToolContext, args json.RawMessage) (string, error) {
    // ...
})
```

### ToolContext

```go
type ToolContext struct {
    UserID    int64  // Telegram user ID
    ChatID    int64  // Telegram chat ID
    Timestamp int64  // Unix timestamp of the inbound message
    Extra     any    // domain-specific — set by BuildExtra
}
```

`Extra` is the primary extension point. Common patterns:

| What to inject | Type in Extra | Set in |
|---|---|---|
| Per-user DB pool | `*pgxpool.Pool` | `BuildExtra` calls `registry.Pool(userID)` |
| User struct | `*User` | `BuildExtra` does a DB lookup |
| Multi-field context | `*MyAgentContext` | `BuildExtra` returns a custom struct |

The tool extracts it with a type assertion:

```go
pool, ok := ctx.Extra.(*pgxpool.Pool)
if !ok {
    return "", fmt.Errorf("no db pool in context")
}
```

### Conversation history

The agent keeps a rolling window of the last 40 messages per *agent instance*
(not per user). For multi-user bots where history isolation matters, run a
separate agent per user, or store history externally.

---

## sdk/telegram

Minimal Telegram Bot API client. Long polling, no webhooks.

```go
client := telegram.New(botToken)

// Implements agent.Messenger interface
updates, err := client.Poll(ctx, offset, timeoutSec)
err = client.Send(ctx, chatID, "hello!")
```

Received updates expose `UpdateID`, `ChatID`, `UserID`, and `Text`.

---

## Persistence

### Why not raw Postgres from WASM?

Go's `net` package in `wasip1` is a fake in-process stub (`net/net_fake.go`,
`//go:build js || wasip1`). There is no real TCP. WasmEdge has socket extensions,
but they require the WasmEdge runtime specifically and TLS cert verification still
fails in the sandbox. wasip2/p3 are not yet supported by the Go toolchain.

**Today:** agent implementations use native `linux/arm64` builds with `pgx` (real TCP).  
**Future:** WASM migration via the host bridge pattern.

### Host bridge pattern (TODO: `sdk/db`)

```
WASM agent
  └── go:wasmimport "host" "fetch"(url, headers, body) → response
       └── host (native, wazero embedded)
            └── net/http → PostgREST → Postgres
```

The WASM module never touches sockets. The host handles TLS, DNS, certs.
Same pattern as [Extism](https://extism.org) and [Fermyon Spin](https://developer.fermyon.com/spin).

**PostgREST** exposes Postgres as a REST API. Combined with Row Level Security,
it gives the same per-user isolation as direct role-based connections:

```
Authorization: Bearer <JWT with sub=<user_uuid>, role=authenticated>
  → PostgREST: SET LOCAL ROLE authenticated; set_config('request.jwt.claim.sub', uuid, true)
  → auth.uid() = current_setting('request.jwt.claim.sub')::uuid
  → RLS policies evaluate normally
```

The planned `sdk/db` package will expose a fluent PostgREST client:

```go
// Pure Go, no CGo — compiles to wasip1
db := db.New(hostFetch, "https://postgrest.example.com", jwtToken)

rows, err := db.From("rooms").Select("id,name,floor").Eq("floor", "2").Execute(ctx)
err = db.From("assignments").Insert(ctx, map[string]any{"room_id": 1, "cleaner_id": 42})
```

Under the hood: `go:wasmimport "host" "fetch"` → HTTP request → PostgREST → Postgres.

---

## Building an agent (end to end)

```go
package main

import (
    "context"
    "log"
    "os"

    "github.com/dmorn/m4dtimes/sdk/agent"
    "github.com/dmorn/m4dtimes/sdk/llm"
    "github.com/dmorn/m4dtimes/sdk/telegram"
)

func main() {
    ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

    provider, _ := llm.NewAnthropicProvider(nil)  // reads LLM_API_KEY

    registry := agent.NewToolRegistry()
    registry.RegisterToolSet(&MyTools{})

    a := agent.New(agent.Options{
        LLM:         llm.New(provider, llm.Options{Model: "claude-sonnet-4-5-20250514"}),
        Messenger:   telegram.New(os.Getenv("TELEGRAM_BOT_TOKEN")),
        Registry:    registry,
        Logger:      agent.NewLogger("info"),
        BuildExtra:  func(userID, _ int64) (any, error) { return domainContext(userID) },
        BuildPrompt: func(userID, _ int64) string { return rolePrompt(userID) },
    })

    if err := a.Run(ctx); err != nil {
        log.Fatal(err)
    }
}
```

### Current build target

Native `linux/arm64`:

```bash
go build -o myagent .
./myagent
```

### Future WASM build target

```bash
GOOS=wasip1 GOARCH=wasm go build -o myagent.wasm .
wasmtime run --env TELEGRAM_BOT_TOKEN=xxx --env LLM_API_KEY=xxx myagent.wasm
```

Not yet working due to networking constraints (see [Persistence](#persistence)).
