package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/dmorn/m4dtimes/sdk/llm"
	"github.com/dmorn/m4dtimes/sdk/session"
)

// typingLoop sends a "typing" action every 4s until the stop channel is closed.
// Telegram drops the indicator after ~5s, so we refresh slightly before that.
func typingLoop(ctx context.Context, notifier TypingNotifier, chatID int64, stop <-chan struct{}) {
	_ = notifier.SendTyping(ctx, chatID) // immediate first send
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = notifier.SendTyping(ctx, chatID)
		}
	}
}

type Options struct {
	LLM         *llm.Client
	Messenger   Messenger
	Registry    *ToolRegistry
	Prompt      string      // static system prompt; ignored when BuildPrompt is set
	BuildExtra  BuildExtra
	BuildTools  BuildTools  // optional: filter/select tools per message; defaults to Registry.Definitions()
	BuildPrompt BuildPrompt // optional: build system prompt per message; overrides Prompt
	Logger      *Logger
	Session     *session.Store // optional: if set, all turns are recorded as JSONL per user
	PollTimeout int            // seconds (default: 30)

	// HandleStart is called when the bot receives a /start command (with optional deep-link payload).
	// payload is everything after "/start " (empty string for bare /start).
	// Return a non-empty reply to send without invoking the LLM (no tokens consumed).
	// Return ("", nil) to fall through to normal handling.
	// Called BEFORE Authorize, so unregistered users can complete onboarding flows.
	HandleStart func(ctx context.Context, userID, chatID int64, payload string) (string, error)

	// Authorize is called for every inbound message BEFORE any LLM call.
	// Return a non-empty message to reject the user (sent as-is, no tokens consumed).
	// Return ("", nil) to allow the message through.
	Authorize func(ctx context.Context, userID, chatID int64) (string, error)
}

type Agent struct {
	opts       Options
	contextsMu sync.Mutex
	contexts   map[int64]*ContextManager // per-user isolated conversation history
}

func New(opts Options) *Agent {
	if opts.PollTimeout <= 0 {
		opts.PollTimeout = 30
	}
	if opts.Registry == nil {
		opts.Registry = NewToolRegistry()
	}
	return &Agent{
		opts:     opts,
		contexts: make(map[int64]*ContextManager),
	}
}

// contextFor returns the ContextManager for the given userID,
// creating a fresh one on first access. If a Session store is configured,
// the context is wired to record every appended message.
func (a *Agent) contextFor(userID int64) *ContextManager {
	a.contextsMu.Lock()
	defer a.contextsMu.Unlock()
	if c, ok := a.contexts[userID]; ok {
		return c
	}
	c := NewContextManager(40)
	if a.opts.Session != nil {
		c.OnAppend = func(msg llm.Message) {
			a.opts.Session.Record(userID, msg)
		}
	}
	a.contexts[userID] = c
	return c
}

// Inject implements ContextInjector. Appends msg to the conversation history
// for userID so the next LLM turn for that user has awareness of it.
func (a *Agent) Inject(userID int64, msg llm.Message) {
	a.contextFor(userID).Append(msg)
}

func (a *Agent) logError(where string, err error) {
	if a.opts.Logger != nil {
		a.opts.Logger.Error(where, err)
	}
}

// Run is the main blocking loop. Exits only when ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if a.opts.LLM == nil || a.opts.Messenger == nil {
		return errors.New("agent requires LLM and Messenger")
	}

	var offset int64
	for {
		if ctx.Err() != nil {
			return nil
		}

		updates, err := a.opts.Messenger.Poll(ctx, offset, a.opts.PollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			a.logError("poll", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
			continue
		}

		for _, update := range updates {
			if a.opts.Logger != nil {
				a.opts.Logger.Inbound(update.UserID, update.ChatID, update.Text)
			}

			// 1. Handle /start deep links BEFORE authorization so unregistered
			//    users can complete the onboarding flow without hitting the wall.
			if strings.HasPrefix(update.Text, "/start") {
				payload := strings.TrimSpace(strings.TrimPrefix(update.Text, "/start"))
				if a.opts.HandleStart != nil {
					reply, err := a.opts.HandleStart(ctx, update.UserID, update.ChatID, payload)
					if err != nil {
						a.logError("handle_start", err)
						_ = a.opts.Messenger.Send(ctx, update.ChatID, "Sorry, something went wrong.")
						offset = update.UpdateID + 1
						continue
					}
					if reply != "" {
						_ = a.opts.Messenger.Send(ctx, update.ChatID, reply)
						offset = update.UpdateID + 1
						continue
					}
				}
			}

			// 2. Authorize — block unregistered users before touching the LLM.
			if a.opts.Authorize != nil {
				msg, err := a.opts.Authorize(ctx, update.UserID, update.ChatID)
				if err != nil {
					a.logError("authorize", err)
					_ = a.opts.Messenger.Send(ctx, update.ChatID, "Sorry, something went wrong.")
					offset = update.UpdateID + 1
					continue
				}
				if msg != "" {
					_ = a.opts.Messenger.Send(ctx, update.ChatID, msg)
					offset = update.UpdateID + 1
					continue
				}
			}

			userCtx := a.contextFor(update.UserID)
			userCtx.Append(userMessage(update))

			var extra any
			if a.opts.BuildExtra != nil {
				extra, err = a.opts.BuildExtra(update.UserID, update.ChatID)
				if err != nil {
					a.logError("build_extra", err)
					extra = nil
				}
			}

			toolCtx := ToolContext{
				UserID:          update.UserID,
				ChatID:          update.ChatID,
				Timestamp:       time.Now().Unix(),
				Extra:           extra,
				ContextInjector: a,
			}

			tools := a.opts.Registry.Definitions()
			if a.opts.BuildTools != nil {
				tools = a.opts.BuildTools(update.UserID, update.ChatID)
			}

			prompt := a.opts.Prompt
			if a.opts.BuildPrompt != nil {
				prompt = a.opts.BuildPrompt(update.UserID, update.ChatID)
			}

			// Start typing indicator if the Messenger supports it.
			var stopTyping chan struct{}
			if notifier, ok := a.opts.Messenger.(TypingNotifier); ok {
				stopTyping = make(chan struct{})
				go typingLoop(ctx, notifier, update.ChatID, stopTyping)
			}
			stopTypingOnce := func() {
				if stopTyping != nil {
					close(stopTyping)
					stopTyping = nil
				}
			}

			for {
				msgs := userCtx.Prepare()
				start := time.Now()
				resp, err := a.opts.LLM.Chat(ctx, llm.Request{
					System:   prompt,
					Messages: msgs,
					Tools:    tools,
				})
				if a.opts.Logger != nil && err == nil {
					a.opts.Logger.LLMCall("", resp.Usage.InputTokens, resp.Usage.OutputTokens, time.Since(start).Milliseconds())
				}
				if err != nil {
					stopTypingOnce()
					a.logError("llm_chat", err)
					_ = a.opts.Messenger.Send(ctx, update.ChatID, "Sorry, something went wrong.")
					break
				}

				if resp.Type == "text" {
					stopTypingOnce()
					msg := assistantMessage(resp.Text)
					msg.Usage = &resp.Usage
					userCtx.Append(msg)
					if a.opts.Logger != nil {
						a.opts.Logger.Outbound(update.ChatID, resp.Text)
					}
					_ = a.opts.Messenger.Send(ctx, update.ChatID, resp.Text)
					break
				}

				if resp.Type == "tool_use" {
					toolMsg := assistantToolUseMessage(resp.ToolCalls)
					toolMsg.Usage = &resp.Usage
					userCtx.Append(toolMsg)
					results := make([]llm.ContentBlock, 0, len(resp.ToolCalls))
					for _, toolCall := range resp.ToolCalls {
						t0 := time.Now()
						result := a.opts.Registry.Execute(toolCall.Name, toolCall.Arguments, toolCtx)
						if result.ToolCallID == "" {
							result.ToolCallID = toolCall.ID
						}
						if a.opts.Logger != nil {
							a.opts.Logger.ToolExec(toolCall.Name, time.Since(t0).Milliseconds(), !result.IsError, result.Content)
						}
						results = append(results, toolResultBlock(result))
					}
					userCtx.Append(toolResultMessage(results))
					continue
				}

				// fallback for unexpected response type
				stopTypingOnce()
				userCtx.Append(assistantMessage(resp.Text))
				_ = a.opts.Messenger.Send(ctx, update.ChatID, resp.Text)
				break
			}

			stopTypingOnce() // safety net in case inner loop exited unexpectedly

			offset = update.UpdateID + 1
		}
	}
}

func userMessage(update Update) llm.Message {
	return llm.Message{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: update.Text}}}
}

func assistantMessage(text string) llm.Message {
	return llm.Message{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func assistantToolUseMessage(toolCalls []llm.ToolCall) llm.Message {
	blocks := make([]llm.ContentBlock, 0, len(toolCalls))
	for i := range toolCalls {
		tc := toolCalls[i]
		blocks = append(blocks, llm.ContentBlock{Type: "tool_use", ToolCall: &tc})
	}
	return llm.Message{Role: "assistant", Content: blocks}
}

func toolResultBlock(result *llm.ToolResult) llm.ContentBlock {
	return llm.ContentBlock{Type: "tool_result", ToolResult: result}
}

func toolResultMessage(results []llm.ContentBlock) llm.Message {
	return llm.Message{Role: "user", Content: results}
}
