package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/dmorn/m4dtimes/sdk/llm"
)

type mockMessenger struct {
	mu      sync.Mutex
	updates []Update
	sent    []sentMessage
	cancel  context.CancelFunc
}

type sentMessage struct {
	chatID int64
	text   string
}

func (m *mockMessenger) Poll(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.updates) == 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	up := m.updates[0]
	m.updates = m.updates[1:]
	return []Update{up}, nil
}

func (m *mockMessenger) Send(ctx context.Context, chatID int64, text string) error {
	m.mu.Lock()
	m.sent = append(m.sent, sentMessage{chatID: chatID, text: text})
	m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	return nil
}

type mockProvider struct {
	mu        sync.Mutex
	responses []*llm.Response
	requests  []llm.Request
}

func (p *mockProvider) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return &llm.Response{Type: "text", Text: ""}, nil
	}
	resp := p.responses[0]
	p.responses = p.responses[1:]
	return resp, nil
}

func TestAgentRun_TextResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messenger := &mockMessenger{
		updates: []Update{{UpdateID: 1, ChatID: 10, UserID: 20, Text: "hello"}},
		cancel:  cancel,
	}
	provider := &mockProvider{responses: []*llm.Response{{Type: "text", Text: "hi there"}}}
	client := llm.New(provider, llm.Options{Model: "test", MaxTokens: 128})

	a := New(Options{LLM: client, Messenger: messenger, Registry: NewToolRegistry(), Prompt: "system"})
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(messenger.sent) != 1 {
		t.Fatalf("sent messages = %d, want 1", len(messenger.sent))
	}
	if messenger.sent[0].text != "hi there" {
		t.Fatalf("sent text = %q, want %q", messenger.sent[0].text, "hi there")
	}
}

func TestAgentRun_ToolUseThenText(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messenger := &mockMessenger{
		updates: []Update{{UpdateID: 1, ChatID: 11, UserID: 22, Text: "calc"}},
		cancel:  cancel,
	}
	provider := &mockProvider{responses: []*llm.Response{
		{
			Type: "tool_use",
			ToolCalls: []llm.ToolCall{{
				ID:        "call_1",
				Name:      "echo",
				Arguments: json.RawMessage(`{"value":"ok"}`),
			}},
		},
		{Type: "text", Text: "done"},
	}}
	client := llm.New(provider, llm.Options{Model: "test", MaxTokens: 128})

	registry := NewToolRegistry()
	var executed bool
	registry.Register("echo", "echo", json.RawMessage(`{"type":"object"}`), func(ctx ToolContext, args json.RawMessage) (string, error) {
		executed = true
		return "tool-result", nil
	})

	a := New(Options{LLM: client, Messenger: messenger, Registry: registry, Prompt: "system"})
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !executed {
		t.Fatalf("expected tool to execute")
	}
	if len(messenger.sent) != 1 || messenger.sent[0].text != "done" {
		t.Fatalf("unexpected sent messages: %+v", messenger.sent)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("expected at least 2 llm requests, got %d", len(provider.requests))
	}
	second := provider.requests[1]
	if len(second.Messages) == 0 {
		t.Fatalf("second request has no messages")
	}
	last := second.Messages[len(second.Messages)-1]
	if last.Role != "user" || len(last.Content) == 0 || last.Content[0].Type != "tool_result" {
		t.Fatalf("last message not tool_result user message: %+v", last)
	}
}
