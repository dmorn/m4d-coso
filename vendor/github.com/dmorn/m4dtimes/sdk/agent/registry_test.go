package agent

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestToolRegistryExecute(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(r *ToolRegistry)
		toolName  string
		args      json.RawMessage
		wantError bool
		wantText  string
	}{
		{
			name: "success",
			setup: func(r *ToolRegistry) {
				r.Register("echo", "echoes", json.RawMessage(`{"type":"object"}`), func(ctx ToolContext, args json.RawMessage) (string, error) {
					return string(args), nil
				})
			},
			toolName:  "echo",
			args:      json.RawMessage(`{"ok":true}`),
			wantError: false,
			wantText:  `{"ok":true}`,
		},
		{
			name:      "unknown tool",
			setup:     func(r *ToolRegistry) {},
			toolName:  "missing",
			args:      json.RawMessage(`{}`),
			wantError: true,
			wantText:  "unknown tool: missing",
		},
		{
			name: "handler error",
			setup: func(r *ToolRegistry) {
				r.Register("fail", "fails", json.RawMessage(`{"type":"object"}`), func(ctx ToolContext, args json.RawMessage) (string, error) {
					return "", errors.New("boom")
				})
			},
			toolName:  "fail",
			args:      json.RawMessage(`{}`),
			wantError: true,
			wantText:  "boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewToolRegistry()
			tt.setup(r)
			res := r.Execute(tt.toolName, tt.args, ToolContext{})
			if res == nil {
				t.Fatalf("Execute returned nil")
			}
			if res.IsError != tt.wantError {
				t.Fatalf("IsError = %v, want %v", res.IsError, tt.wantError)
			}
			if res.Content != tt.wantText {
				t.Fatalf("Content = %q, want %q", res.Content, tt.wantText)
			}
		})
	}
}
