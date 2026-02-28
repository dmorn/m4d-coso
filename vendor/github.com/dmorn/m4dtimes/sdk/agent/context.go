package agent

import "github.com/dmorn/m4dtimes/sdk/llm"

type ContextManager struct {
	Messages    []llm.Message
	MaxMessages int // truncation limit (default: 40)

	// Hooks — set to nil for default behavior
	TransformContext func([]llm.Message) []llm.Message // prune/compact before LLM call
	ConvertToLLM     func([]llm.Message) []llm.Message // filter internal messages
	OnAppend         func(msg llm.Message)              // called after every Append; use for session recording
}

func NewContextManager(maxMessages int) *ContextManager {
	if maxMessages <= 0 {
		maxMessages = 40
	}
	c := &ContextManager{MaxMessages: maxMessages}
	c.TransformContext = func(msgs []llm.Message) []llm.Message {
		if len(msgs) <= c.MaxMessages {
			return msgs
		}
		return msgs[len(msgs)-c.MaxMessages:]
	}
	c.ConvertToLLM = func(msgs []llm.Message) []llm.Message { return msgs }
	return c
}

func (c *ContextManager) Append(msg llm.Message) {
	c.Messages = append(c.Messages, msg)
	if c.OnAppend != nil {
		c.OnAppend(msg)
	}
}

// Prepare returns the message slice to pass to the LLM.
// Applies TransformContext then ConvertToLLM (both optional).
func (c *ContextManager) Prepare() []llm.Message {
	msgs := c.Messages
	if c.TransformContext != nil {
		msgs = c.TransformContext(msgs)
	}
	if c.ConvertToLLM != nil {
		msgs = c.ConvertToLLM(msgs)
	}
	return msgs
}

func (c *ContextManager) Reset() {
	c.Messages = nil
}

// Snapshot returns up to n most recent messages, suitable for crash recovery.
// If n <= 0 or n >= len(Messages), all messages are returned.
func (c *ContextManager) Snapshot(n int) []llm.Message {
	if n <= 0 || n >= len(c.Messages) {
		out := make([]llm.Message, len(c.Messages))
		copy(out, c.Messages)
		return out
	}
	start := len(c.Messages) - n
	out := make([]llm.Message, n)
	copy(out, c.Messages[start:])
	return out
}

// RestoreSnapshot prepends msgs to the existing context. Use after crash recovery
// to seed the conversation history before replaying events.
func (c *ContextManager) RestoreSnapshot(msgs []llm.Message) {
	if len(msgs) == 0 {
		return
	}
	restored := make([]llm.Message, len(msgs)+len(c.Messages))
	copy(restored, msgs)
	copy(restored[len(msgs):], c.Messages)
	c.Messages = restored
}
