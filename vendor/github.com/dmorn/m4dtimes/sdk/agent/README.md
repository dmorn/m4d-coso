# sdk/agent

Agent loop, tool registry, per-user context isolation, and session recording hooks.

Ported from [pi-agent](https://github.com/badlogic/pi-mono/tree/main/packages/agent)
(`@mariozechner/pi-agent-core`) patterns to Go.

## Files

```
sdk/agent/
├── agent.go      # Agent struct, Options, Run() main loop, Inject(), contextFor()
├── context.go    # ContextManager — conversation history, OnAppend hook
├── types.go      # ToolContext, ContextInjector, Messenger, Tool, ToolSet, Update, …
├── registry.go   # ToolRegistry — register tools/toolsets, execute by name
└── logger.go     # Structured event logger (stdout JSON)
```

## Turn cycle

```
telegram.Poll()
      │
      ▼  for each Update:
   HandleStart?  ──yes──► redeem invite / onboard, send reply, next update
      │no
      ▼
   Authorize?    ──reject──► send rejection message (0 LLM tokens), next update
      │allow
      ▼
   BuildExtra(userID)        → ToolContext.Extra (e.g. *pgxpool.Pool)
   BuildTools(userID)        → []llm.ToolDef
   BuildPrompt(userID)       → system prompt string
   contextFor(userID)        → per-user ContextManager

   userCtx.Append(userMessage)

   loop:
     llm.Chat(system, context.Prepare(), tools)
       │
       ├─ type == "text"
       │    userCtx.Append(assistantMessage + usage)
       │    Messenger.Send(chatID, text)
       │    break
       │
       └─ type == "tool_use"
            userCtx.Append(assistantToolUseMessage + usage)
            for each tool call:
              Registry.Execute(name, args, toolCtx)  → ToolResult
            userCtx.Append(toolResultMessage)
            continue
```

**Key invariant:** tool errors don't crash the agent — they become
`ToolResult{IsError: true}` messages the LLM sees and reasons about.

## Per-user conversation contexts

Each user gets an isolated `ContextManager`, created lazily on first message:

```go
func (a *Agent) contextFor(userID int64) *ContextManager
```

This prevents cross-user contamination in multi-user bots. A cleaner's "Sì"
reply is never visible in the manager's conversation history and vice versa.

## ContextInjector

Tools sometimes need to inject a message into *another* user's context (e.g.
`send_user_message` injects the sent DM into the recipient's context, so their
next LLM turn has awareness of the question they're being asked).

The `ContextInjector` interface keeps the dependency direction clean:

```go
// Defined in types.go — the SDK knows nothing about the caller
type ContextInjector interface {
    Inject(userID int64, msg llm.Message)
}
```

The `Agent` implements it. It is passed into every `ToolContext` so tools can
call `ctx.ContextInjector.Inject(recipientID, msg)` without knowing about `Agent`.

## Session recording

Pass a `*session.Store` as `Options.Session` to record every message append
to a per-user JSONL file:

```go
store, _ := session.NewStore("./sessions")
agent.New(agent.Options{
    Session: store,
    // ...
})
```

The hook is wired in `contextFor()`:

```go
c.OnAppend = func(msg llm.Message) {
    a.opts.Session.Record(userID, msg)
}
```

Every `ContextManager.Append(msg)` fires `OnAppend` — user messages, assistant
replies (with usage), tool calls, and tool results are all recorded without
modifying the loop logic.

See [`sdk/session`](../session/) for the file format.

## Options reference

```go
type Options struct {
    LLM         *llm.Client       // required
    Messenger   Messenger         // required — Poll() + Send()
    Registry    *ToolRegistry     // tool definitions + execution
    Prompt      string            // static system prompt (overridden by BuildPrompt)
    BuildExtra  BuildExtra        // func(userID, chatID) (any, error) — per-message extra context
    BuildTools  BuildTools        // func(userID, chatID) []llm.ToolDef — per-message tool list
    BuildPrompt BuildPrompt       // func(userID, chatID) string — per-message system prompt
    Logger      *Logger           // structured stdout logger (optional)
    Session     *session.Store    // JSONL session recording (optional)
    PollTimeout int               // long-poll timeout in seconds (default: 30)

    HandleStart func(ctx, userID, chatID int64, payload string) (string, error)
    // Called on /start [payload] BEFORE Authorize. Return ("", nil) to fall through.
    // Use for invite-link onboarding: unregistered users can complete registration
    // without hitting the authorization wall and consuming LLM tokens.

    Authorize func(ctx, userID, chatID int64) (string, error)
    // Called for every message BEFORE any LLM call.
    // Return a non-empty string to reject the user (0 tokens consumed).
}
```

## ToolContext

```go
type ToolContext struct {
    UserID          int64
    ChatID          int64
    Timestamp       int64
    Extra           any              // set by BuildExtra — e.g. *pgxpool.Pool
    ContextInjector ContextInjector  // always set by agent; inject into other users' contexts
}
```

## Tool and ToolSet interfaces

```go
// Single tool
type Tool interface {
    Def() llm.ToolDef
    Execute(ctx ToolContext, args json.RawMessage) (string, error)
}

// Group of tools sharing state
type ToolSet interface {
    Tools() []Tool
}
```

Register with:
```go
registry.RegisterTool(myTool)
registry.RegisterToolSet(myToolSet)
```

## ContextManager

```go
type ContextManager struct {
    Messages         []llm.Message
    MaxMessages      int                      // default 40 — truncates oldest on overflow
    TransformContext func([]llm.Message) []llm.Message  // prune/compact before LLM call
    ConvertToLLM     func([]llm.Message) []llm.Message  // filter before sending to provider
    OnAppend         func(msg llm.Message)               // called on every Append — used for session recording
}
```
