package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnthropicWireFormatMapping(t *testing.T) {
	var seen map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Fatalf("missing api key header, got %q", got)
		}
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatalf("decode req: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [
				{"type":"tool_use","id":"toolu_1","name":"sum","input":{"a":1,"b":2}}
			],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":11,"output_tokens":7}
		}`))
	}))
	defer ts.Close()

	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "")

	p, err := NewAnthropicProvider(ts.Client())
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	p.url = ts.URL

	resp, err := p.Chat(context.Background(), Request{
		System: "you are helpful",
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}},
			{Role: "assistant", Content: []ContentBlock{{Type: "tool_use", ToolCall: &ToolCall{ID: "toolu_1", Name: "sum", Arguments: json.RawMessage(`{"a":1,"b":2}`)}}}},
			{Role: "user", Content: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{ToolCallID: "toolu_1", Content: "3", IsError: false}}}},
		},
		Tools: []ToolDef{{
			Name:        "sum",
			Description: "sum numbers",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
		Options: Options{Model: "claude-test", MaxTokens: 128},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if seen["model"] != "claude-test" {
		t.Fatalf("unexpected model: %v", seen["model"])
	}
	messages := seen["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
	third := messages[2].(map[string]any)
	if third["role"] != "user" {
		t.Fatalf("tool_result message role should be user, got %v", third["role"])
	}

	tools := seen["tools"].([]any)
	tool := tools[0].(map[string]any)
	if _, ok := tool["input_schema"]; !ok {
		t.Fatalf("expected input_schema in tool payload")
	}
	if _, ok := tool["parameters"]; ok {
		t.Fatalf("did not expect parameters in tool payload")
	}

	if resp.Type != "tool_use" || len(resp.ToolCalls) != 1 {
		t.Fatalf("unexpected response: %#v", resp)
	}
}

func TestAnthropicRetry429ThenSuccess(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type":"text","text":"ok"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer ts.Close()

	t.Setenv("LLM_API_KEY", "test-key")
	p, err := NewAnthropicProvider(ts.Client())
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	p.url = ts.URL
	p.retry = RetryConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Jitter: 0}

	resp, err := p.Chat(context.Background(), Request{Options: Options{Model: "m", MaxTokens: 8}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Text != "ok" || resp.Type != "text" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestValidateToolArgs(t *testing.T) {
	tool := ToolDef{
		Name: "sum",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{"a":{"type":"number"},"b":{"type":"number"}},
			"required":["a","b"],
			"additionalProperties":false
		}`),
	}

	if err := ValidateToolArgs(tool, json.RawMessage(`{"a":1,"b":2}`)); err != nil {
		t.Fatalf("expected valid args, got %v", err)
	}
	if err := ValidateToolArgs(tool, json.RawMessage(`{"a":1}`)); err == nil {
		t.Fatal("expected error for invalid args")
	}
}

func TestNewAnthropicProviderEnvFallback(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "fallback-key")
	p, err := NewAnthropicProvider(http.DefaultClient)
	if err != nil {
		t.Fatalf("expected provider, got err: %v", err)
	}
	if p.apiKey != "fallback-key" {
		t.Fatalf("unexpected key: %q", p.apiKey)
	}
}
